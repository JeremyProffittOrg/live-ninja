package rca

// The Bedrock leg of the pipeline.
//
// Converse is used rather than InvokeModel because it normalizes the request
// across Anthropic model generations: no anthropic_version body field, no
// per-model body schema, `system` blocks and maxTokens as first-class fields.
// The single most likely failure of a hand-rolled InvokeModel body against a
// model id nobody in this repo has called yet is a ValidationException on the
// body shape, and Converse removes that whole surface.
// internal/memory/embedder.go uses InvokeModel because Titan embeddings have
// no Converse support; that is not a precedent to copy here. IAM is identical
// either way: Converse is authorized by the bedrock:InvokeModel action —
// there is no bedrock:Converse action.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// ModelInvoker is the one Bedrock operation the analyzer needs. A
// *bedrockruntime.Client satisfies it; tests inject a fake.
type ModelInvoker interface {
	Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// bedrockRegion is the locked Bedrock region for this account (same posture as
// memory.NewBedrockEmbedder). The cross-region "us." inference profiles the
// RCA model id uses are *called* here and may route the inference elsewhere;
// that routing is what makes the extra foundation-model ARNs necessary in the
// IAM policy, not an extra region in the client.
const bedrockRegion = "us-east-1"

// NewBedrockInvoker builds a Converse client pinned to us-east-1.
func NewBedrockInvoker(ctx context.Context) (ModelInvoker, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(bedrockRegion))
	if err != nil {
		return nil, fmt.Errorf("rca: load aws config: %w", err)
	}
	return bedrockruntime.NewFromConfig(cfg), nil
}

// buildConverseInput assembles the request. Nothing beyond model id, one system
// block, one user message and MaxTokens is sent.
//
// No sampling parameters and no additionalModelRequestFields: current Anthropic
// models reject temperature/top_p/top_k and any explicit thinking
// configuration with a 400, and the exact set varies by model generation.
// Sending nothing means the same code works against whatever RCA_MODEL_ID
// names. Forced tool use (ToolConfig + ToolChoice) was considered as a stronger
// structured-output guarantee and rejected: on Bedrock the forced-tool_choice x
// thinking interaction is model-version dependent, so it would convert an
// unverifiable combination into a hard ValidationException — and the
// malformed-response path below is required regardless, because truncation can
// always produce a broken reply.
func buildConverseInput(cfg Config, prompt string) *bedrockruntime.ConverseInput {
	return &bedrockruntime.ConverseInput{
		ModelId: aws.String(cfg.ModelID),
		System: []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: SystemPrompt()},
		},
		Messages: []types.Message{{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: prompt}},
		}},
		InferenceConfig: &types.InferenceConfiguration{MaxTokens: aws.Int32(cfg.MaxOutputTokens)},
	}
}

// converseText concatenates every text block of a Converse reply. A nil or
// unexpected output shape yields "" — which ExtractReport reports as ErrNoJSON,
// the same disposition as any other unusable reply.
func converseText(out *bedrockruntime.ConverseOutput) string {
	if out == nil {
		return ""
	}
	msg, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, block := range msg.Value.Content {
		if text, ok := block.(*types.ContentBlockMemberText); ok {
			b.WriteString(text.Value)
		}
	}
	return b.String()
}

// ---- structured output ----

// Report is the parsed model output. Field names are the wire contract stated
// in the # YOUR TASK section — the two must be edited together.
type Report struct {
	Symptom                  string             `json:"symptom"`
	RootCause                string             `json:"rootCause"`
	Evidence                 []string           `json:"evidence"`
	Confidence               string             `json:"confidence"`
	CodeFixSuggestions       []string           `json:"codeFixSuggestions"`
	BaseKnowledgeSuggestions []ReportSuggestion `json:"baseKnowledgeSuggestions"`
	ReproSteps               []string           `json:"reproSteps"`
}

// ReportSuggestion is one proposed base-knowledge change.
type ReportSuggestion struct {
	Field         string `json:"field"`
	ProposedValue string `json:"proposedValue"`
	Reason        string `json:"reason"`
}

// Output limits, mirrored in the prompt's schema block. Strings are truncated;
// list items past the count limit are DROPPED rather than merged, because half
// an evidence item is worse than no evidence item.
const (
	maxSymptomChars    = 160
	maxRootCauseChars  = 800
	maxEvidenceChars   = 300
	maxEvidenceItems   = 6
	maxCodeFixChars    = 400
	maxCodeFixItems    = 5
	maxReproStepChars  = 200
	maxReproStepItems  = 6
	maxSuggestions     = 5
	maxSuggestionValue = 300
	maxSuggestionChars = 300

	// maxRawResponseChars caps the verbatim reply persisted on a
	// malformed_response record — enough to see what went wrong, bounded so a
	// runaway generation cannot bloat the item.
	maxRawResponseChars = 4096
)

// Confidence values, normalized. Anything else becomes low: an unrecognized
// confidence is itself evidence the model was not following the contract.
const (
	ConfidenceLow    = "low"
	ConfidenceMedium = "medium"
	ConfidenceHigh   = "high"
)

// Base-knowledge fields RCA is allowed to propose.
//
// This is the M16 locked policy, not a temporary limitation: a location must
// carry geocode-verified coordinates from the settings picker (validateProfile
// rejects coordinate-less locations), so a free-text location proposal could
// never be approved anyway — and a name/email/quiet-hours proposal inferred
// from a transcript is exactly the kind of "helpful" guess an owner should
// never be nudged to accept.
//
// M16 landed the shared vocabulary these two names spell (store.SuggestField*,
// also used by the profile_suggest tool's enum and by the Settings drawer that
// renders the queue), so they now alias it rather than repeating the strings —
// a producer and a consumer disagreeing about what "the units field" is called
// would silently orphan every row RCA files.
const (
	FieldProfileNotes = store.SuggestFieldNotes
	FieldProfileUnits = store.SuggestFieldUnits
)

var allowedSuggestionFields = map[string]bool{
	FieldProfileNotes: true,
	FieldProfileUnits: true,
}

// Extraction failures. Both are permanent: retrying produces the same broken
// reply and burns Opus tokens.
var (
	ErrNoJSON      = errors.New("rca: model reply contained no JSON object")
	ErrEmptyReport = errors.New("rca: model reply had neither a symptom nor a root cause")
)

// ExtractReport parses a model reply permissively, then normalizes it:
//
//   - the JSON object is taken as the substring from the first '{' to the last
//     '}' — this tolerates a stray "Here is the analysis:" preamble and a
//     ```json fence without a second parser;
//   - a missing/unknown confidence becomes "low";
//   - every field is trimmed and capped; overflow items are dropped, not
//     truncated silently into nonsense;
//   - a suggestion whose field is not in the allowlist is dropped;
//   - ErrNoJSON when no '{'...'}' span exists, and ErrEmptyReport when the
//     object parses but symptom AND rootCause are both empty (a truncated
//     reply's typical shape).
func ExtractReport(raw string) (Report, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return Report{}, ErrNoJSON
	}

	var rep Report
	if err := json.Unmarshal([]byte(raw[start:end+1]), &rep); err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrNoJSON, err)
	}

	rep.Symptom = clampPlain(oneLine(rep.Symptom), maxSymptomChars)
	rep.RootCause = clampPlain(strings.TrimSpace(rep.RootCause), maxRootCauseChars)
	rep.Evidence = normalizeList(rep.Evidence, maxEvidenceItems, maxEvidenceChars)
	rep.CodeFixSuggestions = normalizeList(rep.CodeFixSuggestions, maxCodeFixItems, maxCodeFixChars)
	rep.ReproSteps = normalizeList(rep.ReproSteps, maxReproStepItems, maxReproStepChars)

	switch strings.ToLower(strings.TrimSpace(rep.Confidence)) {
	case ConfidenceHigh:
		rep.Confidence = ConfidenceHigh
	case ConfidenceMedium:
		rep.Confidence = ConfidenceMedium
	default:
		rep.Confidence = ConfidenceLow
	}

	if rep.Symptom == "" && rep.RootCause == "" {
		return Report{}, ErrEmptyReport
	}
	return rep, nil
}

// AllowedSuggestions filters a report's proposals down to the allowlist,
// capped at maxSuggestions, and returns the rejected fields so the caller can
// warn and emit a metric per rejection (a model repeatedly proposing a
// location is itself worth noticing).
func AllowedSuggestions(rep Report) (kept []ReportSuggestion, rejected []string) {
	for _, sg := range rep.BaseKnowledgeSuggestions {
		field := strings.TrimSpace(sg.Field)
		value := strings.TrimSpace(sg.ProposedValue)
		if !allowedSuggestionFields[field] || value == "" {
			if field == "" {
				field = "(empty)"
			}
			rejected = append(rejected, field)
			continue
		}
		if len(kept) >= maxSuggestions {
			rejected = append(rejected, field)
			continue
		}
		kept = append(kept, ReportSuggestion{
			Field:         field,
			ProposedValue: clampPlain(oneLine(value), maxSuggestionValue),
			Reason:        clampPlain(oneLine(sg.Reason), maxSuggestionChars),
		})
	}
	return kept, rejected
}

// normalizeList trims, drops empties, caps each item's length, and keeps at
// most maxItems.
func normalizeList(in []string, maxItems, maxChars int) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = clampPlain(oneLine(s), maxChars)
		if s == "" {
			continue
		}
		out = append(out, s)
		if len(out) == maxItems {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clampPlain rune-truncates without appending the prompt truncation marker —
// model output goes into an email and a DynamoDB item, where the marker would
// read as part of the analysis.
func clampPlain(s string, n int) string { return truncateRunes(s, n) }

// ---- error classification ----

// FailureClass is how a Bedrock error is dispositioned.
type FailureClass int

const (
	// ClassTransient — retry via the SQS partial-batch response. Throttling,
	// 5xx, model-not-ready, model-timeout, our own deadline.
	ClassTransient FailureClass = iota
	// ClassModelUnavailable — the account cannot call this model at all:
	// access not granted, or the model id does not exist here. NOT retryable,
	// and NOT something the owner should be emailed about more than once a day.
	ClassModelUnavailable
	// ClassPermanent — a real but non-retryable upstream failure (a genuine
	// ValidationException, ModelErrorException). Persist and drop.
	ClassPermanent
)

// modelUnavailablePhrases are the ValidationException messages that mean "this
// account/region cannot use this model", as opposed to "your request was
// malformed". Bedrock reports both with the same exception type, so the
// message is the only discriminator available — and getting it wrong in the
// safe direction (classifying a real bug as model_unavailable) would email the
// owner about a pending grant that is not actually pending, so the list is
// deliberately specific rather than generous.
var modelUnavailablePhrases = []string{
	"model access",
	"not authorized",
	"don't have access",
	"does not have access",
	"invalid model identifier",
	// Bedrock's actual wording for an id that does not exist in the account is
	// "The provided model identifier is invalid." — the reversed phrasing above
	// never matches it, so both spellings are listed. (Correction to the M17
	// spec's classification table, which listed only the reversed form while its
	// own test used the real message.)
	"model identifier is invalid",
	"is not supported",
	"could not be found",
	"on-demand throughput isn't supported",
}

// ClassifyBedrockError maps a Converse error onto a FailureClass and the
// DegradeReason string persisted on the record. Pure over the error value, so
// every branch is unit-testable with a constructed *types.XException.
func ClassifyBedrockError(err error) (FailureClass, string) {
	if err == nil {
		return ClassTransient, ""
	}

	var accessDenied *types.AccessDeniedException
	if errors.As(err, &accessDenied) {
		return ClassModelUnavailable, "AccessDeniedException"
	}
	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return ClassModelUnavailable, "ResourceNotFoundException"
	}

	var validation *types.ValidationException
	if errors.As(err, &validation) {
		msg := strings.ToLower(validation.ErrorMessage())
		for _, phrase := range modelUnavailablePhrases {
			if strings.Contains(msg, phrase) {
				return ClassModelUnavailable, "ValidationException(model unavailable)"
			}
		}
		return ClassPermanent, "ValidationException"
	}

	var throttling *types.ThrottlingException
	if errors.As(err, &throttling) {
		return ClassTransient, "ThrottlingException"
	}
	var quota *types.ServiceQuotaExceededException
	if errors.As(err, &quota) {
		return ClassTransient, "ServiceQuotaExceededException"
	}
	var unavailable *types.ServiceUnavailableException
	if errors.As(err, &unavailable) {
		return ClassTransient, "ServiceUnavailableException"
	}
	var internal *types.InternalServerException
	if errors.As(err, &internal) {
		return ClassTransient, "InternalServerException"
	}
	var notReady *types.ModelNotReadyException
	if errors.As(err, &notReady) {
		return ClassTransient, "ModelNotReadyException"
	}
	var modelTimeout *types.ModelTimeoutException
	if errors.As(err, &modelTimeout) {
		return ClassTransient, "ModelTimeoutException"
	}
	// Our own per-message model deadline: the call may well succeed next time.
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTransient, "context.DeadlineExceeded"
	}

	var modelErr *types.ModelErrorException
	if errors.As(err, &modelErr) {
		return ClassPermanent, "ModelErrorException"
	}
	var conflict *types.ConflictException
	if errors.As(err, &conflict) {
		return ClassPermanent, "ConflictException"
	}
	return ClassPermanent, "UnrecognizedError"
}
