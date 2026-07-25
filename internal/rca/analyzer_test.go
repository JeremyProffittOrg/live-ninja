package rca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// fixedTestNow is the clock every analyzer test starts from.
var fixedTestNow = time.Date(2026, 7, 25, 14, 3, 11, 482913000, time.UTC)

// ---- fakes ----

// fakeModel is the Bedrock seam: it records every ConverseInput and replays a
// scripted queue of replies/errors (the last entry repeats forever, so a test
// that only cares about "always denied" scripts one error).
type fakeModel struct {
	mu      sync.Mutex
	calls   []*bedrockruntime.ConverseInput
	replies []modelReply
}

type modelReply struct {
	out *bedrockruntime.ConverseOutput
	err error
}

func (m *fakeModel) Converse(ctx context.Context, in *bedrockruntime.ConverseInput,
	_ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, in)
	if len(m.replies) == 0 {
		return nil, errors.New("fakeModel: no scripted reply")
	}
	r := m.replies[0]
	if len(m.replies) > 1 {
		m.replies = m.replies[1:]
	}
	return r.out, r.err
}

func (m *fakeModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *fakeModel) lastCall() *bedrockruntime.ConverseInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return nil
	}
	return m.calls[len(m.calls)-1]
}

// fakeMailer is the SES seam.
type fakeMailer struct {
	mu   sync.Mutex
	sent []*sesv2.SendEmailInput
	err  error
	n    int
}

func (f *fakeMailer) SendEmail(ctx context.Context, in *sesv2.SendEmailInput,
	_ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.sent = append(f.sent, in)
	f.n++
	return &sesv2.SendEmailOutput{MessageId: aws.String(fmt.Sprintf("ses-msg-%03d", f.n))}, nil
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeMailer) subjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sent))
	for _, in := range f.sent {
		out = append(out, aws.ToString(in.Content.Simple.Subject.Data))
	}
	return out
}

// harness wires a real store over an in-memory DynamoDB plus the two fakes, so
// the tests exercise the production key shapes without AWS credentials.
type harness struct {
	analyzer *Analyzer
	model    *fakeModel
	mail     *fakeMailer
	store    *store.Store
	ddb      *testutil.FakeDynamo
	now      time.Time
	ids      int
}

func newHarness(t *testing.T, replies ...modelReply) *harness {
	t.Helper()
	ddb := testutil.NewFakeDynamo()
	h := &harness{
		model: &fakeModel{replies: replies},
		mail:  &fakeMailer{},
		store: store.NewWithClient(ddb, "live-ninja-test"),
		ddb:   ddb,
		now:   fixedTestNow,
	}
	a, err := NewAnalyzer(Deps{
		Store: h.store,
		Model: h.model,
		Mail:  h.mail,
		Log:   observ.NewLogger(io.Discard, "error"),
		Cfg: Config{
			ModelID:          "us.anthropic.claude-opus-4-5-20251101-v1:0",
			MaxOutputTokens:  2000,
			ModelTimeout:     30 * time.Second,
			DailyCap:         10,
			Cooldown:         time.Hour,
			EmailTo:          DefaultRecipient,
			EmailFrom:        DefaultFromAddress,
			EmailReplyTo:     DefaultReplyTo,
			ConfigurationSet: "live-ninja-email",
			NoticeWindow:     24 * time.Hour,
		},
		Now: func() time.Time { return h.now },
		// Deterministic ids so records and suggestions are addressable.
		NewID: func() (string, error) {
			h.ids++
			return fmt.Sprintf("id%010d", h.ids), nil
		},
	})
	require.NoError(t, err)
	h.analyzer = a
	return h
}

// seedTranscript writes the transcript rows a real session would have left,
// including the tool router's audit row for the failing call.
func (h *harness) seedTranscript(t *testing.T, f tools.ToolFailure, turns int) {
	t.Helper()
	ctx := context.Background()
	base := fixedTestNow.Add(-5 * time.Minute)
	for i := 0; i < turns; i++ {
		role, text := "user", fmt.Sprintf("utterance %d", i)
		if i%2 == 1 {
			role, text = "assistant", fmt.Sprintf("reply %d", i)
		}
		require.NoError(t, h.store.ConditionalPut(ctx, "USER#"+f.UserID,
			fmt.Sprintf("LOG#%s#%06d", f.SessionID, i),
			map[string]any{
				"role":    role,
				"text":    text,
				"surface": "web",
				"engine":  "gpt-realtime",
				"ts":      base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			}, 0))
	}
	require.NoError(t, h.store.ConditionalPut(ctx, "USER#"+f.UserID,
		fmt.Sprintf("LOG#%s#%06d", f.SessionID, turns),
		map[string]any{
			"role": "tool",
			"text": fmt.Sprintf("tool=%s outcome=error callId=%s args=%s error=%s",
				f.Tool, f.CallID, f.ArgsJSON, f.ErrorCode),
			"surface": "web",
			"engine":  "tool-router",
			"ts":      fixedTestNow.Format(time.RFC3339),
		}, 0))
}

func (h *harness) records(t *testing.T, family string) []store.RCARecord {
	t.Helper()
	recs, err := h.store.RecentRCAs(context.Background(), family, 25)
	require.NoError(t, err)
	return recs
}

// ---- fixtures ----

const goodReportJSON = `{
  "symptom": "get_weather was called with a one-character location placeholder",
  "rootCause": "The model emitted \"x\" instead of omitting the location argument.",
  "evidence": ["the audit turn shows args={\"location\":\"x\"}"],
  "confidence": "high",
  "codeFixSuggestions": ["internal/tools/weather.go: document the omit-for-profile-default behaviour"],
  "baseKnowledgeSuggestions": [
    {"field": "profile.units", "proposedValue": "metric", "reason": "the user asked for celsius twice"}
  ],
  "reproSteps": ["invoke get_weather with {\"location\":\"x\"}"]
}`

func converseReply(text string, stop types.StopReason) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{Value: types.Message{
			Role:    types.ConversationRoleAssistant,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
		}},
		StopReason: stop,
		Usage: &types.TokenUsage{
			InputTokens:  aws.Int32(6412),
			OutputTokens: aws.Int32(744),
			TotalTokens:  aws.Int32(7156),
		},
	}
}

func okReply() modelReply {
	return modelReply{out: converseReply(goodReportJSON, types.StopReasonEndTurn)}
}

// ---- happy path ----

func TestAnalyzeHappyPath(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply())
	f := baseFailure()
	h.seedTranscript(t, f, 6)

	out, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, store.RCAStatusAnalyzed, out.Status)
	assert.Equal(t, Family(f.Tool, f.ErrorCode), out.Family)
	assert.Equal(t, Signature(f), out.Signature)
	assert.True(t, out.Emailed)
	assert.Equal(t, 1, out.BudgetUsed)
	assert.Equal(t, 1, h.model.callCount())

	recs := h.records(t, out.Family)
	require.Len(t, recs, 1)
	rec := recs[0]
	assert.Equal(t, store.RCAStatusAnalyzed, rec.Status)
	assert.Equal(t, "get_weather was called with a one-character location placeholder", rec.Symptom)
	assert.Equal(t, ConfidenceHigh, rec.Confidence)
	assert.Len(t, rec.Evidence, 1)
	assert.Len(t, rec.CodeFixes, 1)
	assert.Len(t, rec.ReproSteps, 1)
	assert.Equal(t, "gpt-realtime", rec.Engine, "the engine is recovered from the transcript, not the envelope")
	assert.Equal(t, 7, rec.TurnsInWindow)
	assert.Equal(t, f.TxID, rec.TxID)
	assert.Equal(t, 6412, rec.InputTokens)
	assert.Equal(t, 744, rec.OutputTokens)
	assert.Len(t, rec.PromptSHA256, 64)
	assert.Equal(t, fixedTestNow.Add(30*24*time.Hour).Unix(), rec.TTL)
	assert.True(t, rec.Emailed)
	assert.Equal(t, "ses-msg-001", rec.EmailMessageID)

	require.Equal(t, 1, h.mail.count())
	sent := h.mail.sent[0]
	assert.Equal(t,
		"Live Ninja RCA: get_weather — get_weather was called with a one-character location placeholder",
		aws.ToString(sent.Content.Simple.Subject.Data))
	assert.Equal(t, DefaultFromAddress, aws.ToString(sent.FromEmailAddress))
	assert.Equal(t, []string{"proffitt.jeremy@gmail.com"}, sent.ReplyToAddresses)
	assert.Equal(t, []string{"proffitt.jeremy@gmail.com"}, sent.Destination.ToAddresses)
	assert.Equal(t, "live-ninja-email", aws.ToString(sent.ConfigurationSetName))
	assert.Contains(t, aws.ToString(sent.Content.Simple.Body.Text.Data), "ROOT CAUSE")
}

// TestSESSenderIsJeremyNinja guards the house rule on its own: sending FROM the
// gmail address makes SES return a MessageId while Gmail silently drops the mail
// on DMARC failure, so the report would look delivered and never arrive.
func TestSESSenderIsJeremyNinja(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply())
	f := baseFailure()

	_, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	require.Equal(t, 1, h.mail.count())

	from := aws.ToString(h.mail.sent[0].FromEmailAddress)
	assert.Contains(t, from, "@jeremy.ninja")
	assert.NotContains(t, from, "proffitt.jeremy@gmail.com")
	assert.Equal(t, []string{"proffitt.jeremy@gmail.com"}, h.mail.sent[0].ReplyToAddresses)

	assert.Contains(t, DefaultFromAddress, "@jeremy.ninja")
	assert.Equal(t, "proffitt.jeremy@gmail.com", DefaultReplyTo)
}

// TestConverseRequestShape guards the "send nothing else" rule against a
// well-meaning future edit that adds temperature or forced tool use.
func TestConverseRequestShape(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply())

	_, err := h.analyzer.Analyze(ctx, baseFailure())
	require.NoError(t, err)

	in := h.model.lastCall()
	require.NotNil(t, in)
	assert.Equal(t, "us.anthropic.claude-opus-4-5-20251101-v1:0", aws.ToString(in.ModelId))
	require.Len(t, in.System, 1)
	sys, ok := in.System[0].(*types.SystemContentBlockMemberText)
	require.True(t, ok)
	assert.Contains(t, sys.Value, "UNTRUSTED")
	require.Len(t, in.Messages, 1)
	assert.Equal(t, types.ConversationRoleUser, in.Messages[0].Role)
	require.Len(t, in.Messages[0].Content, 1)
	user, ok := in.Messages[0].Content[0].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Contains(t, user.Value, "# YOUR TASK")
	require.NotNil(t, in.InferenceConfig)
	assert.Equal(t, int32(2000), aws.ToInt32(in.InferenceConfig.MaxTokens))
	assert.Nil(t, in.InferenceConfig.Temperature)
	assert.Nil(t, in.InferenceConfig.TopP)
	assert.Nil(t, in.ToolConfig)
	assert.Nil(t, in.AdditionalModelRequestFields)
	assert.Nil(t, in.GuardrailConfig)
}

// ---- dedupe / cap ----

func TestAnalyzeCooldownSuppressesSecond(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply())
	f := baseFailure()

	first, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	require.Equal(t, store.RCAStatusAnalyzed, first.Status)

	h.now = fixedTestNow.Add(time.Minute)
	second, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuppressedCooldown, second.Status)

	assert.Equal(t, 1, h.model.callCount(), "the duplicate must not reach Bedrock")
	assert.Equal(t, 1, h.mail.count(), "and must not email the owner again")

	recs := h.records(t, first.Family)
	require.Len(t, recs, 1)
	assert.Equal(t, 1, recs[0].SuppressedCount, "the suppression is counted on the existing record")
}

func TestAnalyzeCooldownExpiryAllowsSecond(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply(), okReply())
	f := baseFailure()

	_, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)

	h.now = fixedTestNow.Add(61 * time.Minute)
	out, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, store.RCAStatusAnalyzed, out.Status)

	assert.Equal(t, 2, h.model.callCount())
	assert.Equal(t, 2, h.mail.count())
	assert.Len(t, h.records(t, out.Family), 2)
}

// distinctFailure produces failures with distinct SIGNATURES that still share a
// family. It varies the argument key set, not the error message: quoted runs
// collapse to `"?"` during normalization, so two messages that differ only in
// the argument they name are — correctly — the same failure shape.
func distinctFailure(i int) tools.ToolFailure {
	f := baseFailure()
	f.ArgsJSON = fmt.Sprintf(`{"location":"x","extra_%d":true}`, i)
	f.CallID = fmt.Sprintf("call_%d", i)
	return f
}

func TestAnalyzeDailyCapStopsAtTen(t *testing.T) {
	ctx := context.Background()
	replies := make([]modelReply, 0, 11)
	for i := 0; i < 11; i++ {
		replies = append(replies, okReply())
	}
	h := newHarness(t, replies...)

	for i := 0; i < 10; i++ {
		out, err := h.analyzer.Analyze(ctx, distinctFailure(i))
		require.NoError(t, err)
		require.Equal(t, store.RCAStatusAnalyzed, out.Status, "analysis %d", i)
		require.Equal(t, i+1, out.BudgetUsed)
	}

	out, err := h.analyzer.Analyze(ctx, distinctFailure(10))
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuppressedCap, out.Status)
	assert.Equal(t, 10, h.model.callCount(), "the 11th failure must not reach Bedrock")
	assert.Equal(t, 10, h.mail.count())
}

func TestAnalyzeDailyCapResetsNextDay(t *testing.T) {
	ctx := context.Background()
	replies := make([]modelReply, 0, 11)
	for i := 0; i < 11; i++ {
		replies = append(replies, okReply())
	}
	h := newHarness(t, replies...)

	for i := 0; i < 10; i++ {
		_, err := h.analyzer.Analyze(ctx, distinctFailure(i))
		require.NoError(t, err)
	}

	h.now = fixedTestNow.Add(24 * time.Hour) // next UTC day
	out, err := h.analyzer.Analyze(ctx, distinctFailure(10))
	require.NoError(t, err)
	assert.Equal(t, store.RCAStatusAnalyzed, out.Status)
	assert.Equal(t, 1, out.BudgetUsed, "the counter is per UTC day")
	assert.Equal(t, 11, h.model.callCount())
}

// TestAnalyzeCooldownClaimedBeforeBudget pins the claim ordering: reversing it
// would burn daily budget on exactly the duplicates the cooldown exists to
// absorb.
func TestAnalyzeCooldownClaimedBeforeBudget(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply())
	f := baseFailure()

	claimed, err := h.store.ClaimRCACooldown(ctx, Family(f.Tool, f.ErrorCode), Signature(f),
		fixedTestNow, time.Hour)
	require.NoError(t, err)
	require.True(t, claimed)

	out, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, OutcomeSuppressedCooldown, out.Status)
	assert.Equal(t, 0, out.BudgetUsed)
	assert.Nil(t, h.ddb.RawItem("RCA#BUDGET", "DAY#"+DayKey(fixedTestNow)),
		"the day's counter must not have been touched")
	assert.Equal(t, 0, h.model.callCount())
}

// ---- degradation ----

func TestAnalyzeModelAccessDenied(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, modelReply{err: &types.AccessDeniedException{
		Message: aws.String("You don't have access to the model with the specified model ID."),
	}})
	f := baseFailure()

	out, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err, "a denied model must NEVER be reported as a retryable failure")
	assert.Equal(t, store.RCAStatusModelUnavailable, out.Status)

	recs := h.records(t, Family(f.Tool, f.ErrorCode))
	require.Len(t, recs, 1)
	assert.Equal(t, store.RCAStatusModelUnavailable, recs[0].Status)
	assert.Equal(t, "AccessDeniedException", recs[0].DegradeReason)
	assert.NotEmpty(t, recs[0].PromptSHA256, "the gathered context is still recorded")
	assert.NotEmpty(t, recs[0].ArgsJSON)
	assert.Empty(t, recs[0].RootCause)
	assert.False(t, recs[0].Emailed)

	// Exactly one email, and it is the operational notice — not a report.
	require.Equal(t, 1, h.mail.count())
	assert.Equal(t, "Live Ninja RCA: disabled — Bedrock model access unavailable", h.mail.subjects()[0])
}

func TestAnalyzeModelAccessDeniedNotifiesOncePerDay(t *testing.T) {
	ctx := context.Background()
	replies := make([]modelReply, 0, 4)
	for i := 0; i < 4; i++ {
		replies = append(replies, modelReply{err: &types.AccessDeniedException{Message: aws.String("denied")}})
	}
	h := newHarness(t, replies...)

	for i := 0; i < 3; i++ {
		h.now = fixedTestNow.Add(time.Duration(i) * time.Hour)
		require.NoError(t, h.analyzer.Handle(ctx, distinctFailure(i)))
	}
	assert.Equal(t, 1, h.mail.count(), "three denials inside 24h send ONE notice")

	h.now = fixedTestNow.Add(25 * time.Hour)
	require.NoError(t, h.analyzer.Handle(ctx, distinctFailure(3)))
	assert.Equal(t, 2, h.mail.count(), "the notice window reopens after 24h")

	// All four failures were still recorded (they share one family).
	f := distinctFailure(0)
	assert.Len(t, h.records(t, Family(f.Tool, f.ErrorCode)), 4,
		"every denied failure is still parked for a later re-run")
}

func TestAnalyzeInvalidModelIdIsModelUnavailable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, modelReply{err: &types.ValidationException{
		Message: aws.String("The provided model identifier is invalid."),
	}})
	f := baseFailure()

	require.NoError(t, h.analyzer.Handle(ctx, f))
	recs := h.records(t, Family(f.Tool, f.ErrorCode))
	require.Len(t, recs, 1)
	assert.Equal(t, store.RCAStatusModelUnavailable, recs[0].Status)
	assert.Equal(t, 1, h.mail.count())
}

func TestAnalyzeThrottlingIsTransient(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, modelReply{err: &types.ThrottlingException{Message: aws.String("slow down")}})
	f := baseFailure()

	err := h.analyzer.Handle(ctx, f)
	require.Error(t, err, "throttling must come back as a retryable failure")
	assert.Empty(t, h.records(t, Family(f.Tool, f.ErrorCode)), "no record for a retryable failure")
	assert.Equal(t, 0, h.mail.count())
}

func TestAnalyzeGenuineValidationErrorIsPermanent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, modelReply{err: &types.ValidationException{
		Message: aws.String("maxTokens must be positive"),
	}})
	f := baseFailure()

	require.NoError(t, h.analyzer.Handle(ctx, f))
	recs := h.records(t, Family(f.Tool, f.ErrorCode))
	require.Len(t, recs, 1)
	assert.Equal(t, store.RCAStatusUpstreamError, recs[0].Status)
	assert.Equal(t, "ValidationException", recs[0].DegradeReason)
	assert.Equal(t, 0, h.mail.count(), "a code bug is for the logs, not the owner's inbox")
}

func TestClassifyBedrockError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantClass  FailureClass
		wantReason string
	}{
		{"access denied", &types.AccessDeniedException{}, ClassModelUnavailable, "AccessDeniedException"},
		{"resource not found", &types.ResourceNotFoundException{}, ClassModelUnavailable, "ResourceNotFoundException"},
		{"validation: model access", &types.ValidationException{Message: aws.String("You need Model access for this")}, ClassModelUnavailable, "ValidationException(model unavailable)"},
		{"validation: not authorized", &types.ValidationException{Message: aws.String("Not authorized to invoke")}, ClassModelUnavailable, "ValidationException(model unavailable)"},
		{"validation: don't have access", &types.ValidationException{Message: aws.String("You don't have access to it")}, ClassModelUnavailable, "ValidationException(model unavailable)"},
		{"validation: does not have access", &types.ValidationException{Message: aws.String("Account does not have access")}, ClassModelUnavailable, "ValidationException(model unavailable)"},
		{"validation: invalid model identifier", &types.ValidationException{Message: aws.String("Invalid model identifier")}, ClassModelUnavailable, "ValidationException(model unavailable)"},
		{"validation: is not supported", &types.ValidationException{Message: aws.String("That model is not supported here")}, ClassModelUnavailable, "ValidationException(model unavailable)"},
		{"validation: could not be found", &types.ValidationException{Message: aws.String("The model could not be found")}, ClassModelUnavailable, "ValidationException(model unavailable)"},
		{"validation: on-demand throughput", &types.ValidationException{Message: aws.String("On-demand throughput isn't supported")}, ClassModelUnavailable, "ValidationException(model unavailable)"},
		{"throttling", &types.ThrottlingException{}, ClassTransient, "ThrottlingException"},
		{"quota", &types.ServiceQuotaExceededException{}, ClassTransient, "ServiceQuotaExceededException"},
		{"unavailable", &types.ServiceUnavailableException{}, ClassTransient, "ServiceUnavailableException"},
		{"internal", &types.InternalServerException{}, ClassTransient, "InternalServerException"},
		{"model not ready", &types.ModelNotReadyException{}, ClassTransient, "ModelNotReadyException"},
		{"model timeout", &types.ModelTimeoutException{}, ClassTransient, "ModelTimeoutException"},
		{"our deadline", context.DeadlineExceeded, ClassTransient, "context.DeadlineExceeded"},
		{"wrapped deadline", fmt.Errorf("converse: %w", context.DeadlineExceeded), ClassTransient, "context.DeadlineExceeded"},
		{"genuine validation", &types.ValidationException{Message: aws.String("maxTokens must be positive")}, ClassPermanent, "ValidationException"},
		{"model error", &types.ModelErrorException{}, ClassPermanent, "ModelErrorException"},
		{"conflict", &types.ConflictException{}, ClassPermanent, "ConflictException"},
		{"unrecognized", errors.New("something else entirely"), ClassPermanent, "UnrecognizedError"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			class, reason := ClassifyBedrockError(tc.err)
			assert.Equal(t, tc.wantClass, class)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}

func TestAnalyzeMalformedResponse(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t,
		modelReply{out: converseReply("I'm afraid I can't do that.", types.StopReasonEndTurn)},
		modelReply{out: converseReply("Still no JSON here.", types.StopReasonEndTurn)},
	)
	f := baseFailure()

	require.NoError(t, h.analyzer.Handle(ctx, f), "a broken reply is permanent, not retryable")
	recs := h.records(t, Family(f.Tool, f.ErrorCode))
	require.Len(t, recs, 1)
	assert.Equal(t, store.RCAStatusMalformedResponse, recs[0].Status)
	assert.Equal(t, "I'm afraid I can't do that.", recs[0].RawResponse)
	assert.Empty(t, recs[0].Symptom)

	// One notice, no report.
	require.Equal(t, 1, h.mail.count())
	assert.Contains(t, h.mail.subjects()[0], "not parseable")

	// A second malformed reply inside the window records but does not re-notify.
	h.now = fixedTestNow.Add(2 * time.Hour)
	require.NoError(t, h.analyzer.Handle(ctx, distinctFailure(1)))
	assert.Equal(t, 1, h.mail.count())
}

func TestAnalyzeTruncatedButParseable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, modelReply{out: converseReply(goodReportJSON, types.StopReasonMaxTokens)})
	f := baseFailure()

	out, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, store.RCAStatusAnalyzed, out.Status)

	require.Equal(t, 1, h.mail.count())
	body := aws.ToString(h.mail.sent[0].Content.Simple.Body.Text.Data)
	assert.Contains(t, body, "NOTE: the model response hit the output-token limit")
	assert.Contains(t, body, "stop=max_tokens")
}

func TestExtractReportPermissive(t *testing.T) {
	bare := `{"symptom":"s","rootCause":"r","confidence":"high"}`
	tests := []struct {
		name    string
		raw     string
		wantErr error
		check   func(t *testing.T, rep Report)
	}{
		{name: "bare object", raw: bare, check: func(t *testing.T, rep Report) {
			assert.Equal(t, "s", rep.Symptom)
			assert.Equal(t, ConfidenceHigh, rep.Confidence)
		}},
		{name: "fenced", raw: "```json\n" + bare + "\n```"},
		{name: "prose preamble", raw: "Here is the analysis:\n" + bare},
		{name: "trailing prose", raw: bare + "\n\nHope that helps!"},
		{name: "unknown confidence", raw: `{"symptom":"s","confidence":"very sure"}`,
			check: func(t *testing.T, rep Report) {
				assert.Equal(t, ConfidenceLow, rep.Confidence)
			}},
		{name: "missing confidence", raw: `{"rootCause":"r"}`,
			check: func(t *testing.T, rep Report) {
				assert.Equal(t, ConfidenceLow, rep.Confidence)
			}},
		{name: "no braces", raw: "I refuse.", wantErr: ErrNoJSON},
		{name: "empty report", raw: `{"symptom":"  ","rootCause":""}`, wantErr: ErrEmptyReport},
		{name: "unparseable object", raw: `{"symptom": }`, wantErr: ErrNoJSON},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := ExtractReport(tc.raw)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.check != nil {
				tc.check(t, rep)
			}
		})
	}

	// Overflow items are dropped, not merged.
	many := make([]string, 12)
	for i := range many {
		many[i] = fmt.Sprintf("evidence %d", i)
	}
	body, err := json.Marshal(map[string]any{
		"symptom":            "s",
		"evidence":           many,
		"codeFixSuggestions": many,
		"reproSteps":         many,
	})
	require.NoError(t, err)
	rep, err := ExtractReport(string(body))
	require.NoError(t, err)
	assert.Len(t, rep.Evidence, maxEvidenceItems)
	assert.Len(t, rep.CodeFixSuggestions, maxCodeFixItems)
	assert.Len(t, rep.ReproSteps, maxReproStepItems)

	// Over-long strings are capped.
	long, err := json.Marshal(map[string]any{
		"symptom":   strings.Repeat("a", 500),
		"rootCause": strings.Repeat("b", 2000),
	})
	require.NoError(t, err)
	rep, err = ExtractReport(string(long))
	require.NoError(t, err)
	assert.Len(t, []rune(rep.Symptom), maxSymptomChars)
	assert.Len(t, []rune(rep.RootCause), maxRootCauseChars)
	assert.NotContains(t, rep.Symptom, "truncated to fit",
		"the prompt truncation marker must never leak into model output")
}

// ---- suggestions ----

func TestAnalyzeFilesAllowlistedSuggestionsOnly(t *testing.T) {
	ctx := context.Background()
	reportJSON := `{
      "symptom": "s", "rootCause": "r", "confidence": "medium",
      "baseKnowledgeSuggestions": [
        {"field": "profile.units", "proposedValue": "metric", "reason": "asked for celsius twice"},
        {"field": "profile.notes[]", "proposedValue": "prefers short answers", "reason": "said so"},
        {"field": "profile.homeLocation", "proposedValue": "Austin", "reason": "mentioned Austin"}
      ]
    }`
	h := newHarness(t, modelReply{out: converseReply(reportJSON, types.StopReasonEndTurn)})
	f := baseFailure()

	out, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	require.Len(t, out.SuggestionIDs, 2, "the location proposal must be dropped")

	list, err := h.store.ListProfileSuggestions(ctx, f.UserID, 10)
	require.NoError(t, err)
	require.Len(t, list, 2)

	byField := map[string]store.ProfileSuggestion{}
	for _, sg := range list {
		byField[sg.Field] = sg
		assert.Equal(t, store.SuggestionStatusPending, sg.Status)
		assert.Equal(t, "rca", sg.Source)
		assert.Equal(t, out.RCAID, sg.SourceRef)
		assert.Contains(t, out.SuggestionIDs, sg.SuggID)
		assert.NotZero(t, sg.TTL)
	}
	require.Contains(t, byField, FieldProfileUnits)
	require.Contains(t, byField, FieldProfileNotes)
	assert.Equal(t, "metric", byField[FieldProfileUnits].ProposedValue)
	assert.Equal(t, "asked for celsius twice", byField[FieldProfileUnits].Reason)
	assert.Equal(t, "prefers short answers", byField[FieldProfileNotes].ProposedValue)
	assert.Empty(t, byField[FieldProfileNotes].CurrentValue, "a note is an addition, not a replacement")

	recs := h.records(t, out.Family)
	require.Len(t, recs, 1)
	assert.ElementsMatch(t, out.SuggestionIDs, recs[0].SuggestionIDs)

	// The report email says a proposal was dropped and why.
	body := aws.ToString(h.mail.sent[0].Content.Simple.Body.Text.Data)
	assert.Contains(t, body, "profile.homeLocation")
	assert.Contains(t, body, "outside the allowlist")
}

func TestAllowedSuggestionsCapsAtFive(t *testing.T) {
	var rep Report
	for i := 0; i < 8; i++ {
		rep.BaseKnowledgeSuggestions = append(rep.BaseKnowledgeSuggestions, ReportSuggestion{
			Field: FieldProfileNotes, ProposedValue: fmt.Sprintf("note %d", i), Reason: "because",
		})
	}
	kept, rejected := AllowedSuggestions(rep)
	assert.Len(t, kept, maxSuggestions)
	assert.Len(t, rejected, 3)

	// An empty proposed value is rejected outright.
	kept, rejected = AllowedSuggestions(Report{BaseKnowledgeSuggestions: []ReportSuggestion{
		{Field: FieldProfileUnits, ProposedValue: "  "},
		{Field: "", ProposedValue: "x"},
	}})
	assert.Empty(t, kept)
	assert.Equal(t, []string{FieldProfileUnits, "(empty)"}, rejected)
}

// ---- resilience ----

func TestAnalyzeEmailFailureStillPersists(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply())
	h.mail.err = errors.New("ses: throttled")
	f := baseFailure()

	out, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err, "retrying is pointless once the cooldown is claimed")
	assert.Equal(t, store.RCAStatusAnalyzed, out.Status)
	assert.False(t, out.Emailed)

	recs := h.records(t, out.Family)
	require.Len(t, recs, 1)
	assert.False(t, recs[0].Emailed)
	assert.Empty(t, recs[0].EmailMessageID)
	assert.Equal(t, "get_weather was called with a one-character location placeholder", recs[0].Symptom,
		"the analysis is still durable")
}

func TestAnalyzeSkipsUnusableEnvelope(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(f *tools.ToolFailure){
		"missing tool":      func(f *tools.ToolFailure) { f.Tool = "" },
		"missing errorCode": func(f *tools.ToolFailure) { f.ErrorCode = "" },
		"missing userId":    func(f *tools.ToolFailure) { f.UserID = "" },
		"future version":    func(f *tools.ToolFailure) { f.V = 2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, okReply())
			f := baseFailure()
			mutate(&f)

			out, err := h.analyzer.Analyze(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, OutcomeSkipped, out.Status)
			assert.Equal(t, 0, h.model.callCount())
			assert.Equal(t, 0, h.mail.count())
			assert.Equal(t, 0, h.ddb.Len(), "nothing at all is written for an unusable envelope")
		})
	}
}

// TestAnalyzeMissingTranscriptDegrades covers the fallback-turn case: a typed
// text turn can invoke a tool with no transcript rows at all, and those degraded
// paths are the failures most worth understanding.
func TestAnalyzeMissingTranscriptDegrades(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply())
	f := baseFailure() // no transcript seeded

	out, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, store.RCAStatusAnalyzed, out.Status)

	recs := h.records(t, out.Family)
	require.Len(t, recs, 1)
	assert.Equal(t, 0, recs[0].TurnsInWindow)
	assert.Empty(t, recs[0].Engine)

	in := h.model.lastCall()
	require.NotNil(t, in)
	prompt := in.Messages[0].Content[0].(*types.ContentBlockMemberText).Value
	assert.Contains(t, prompt, "# CONVERSATION WINDOW\n(none)")
}

func TestAnalyzePriorRCAsRideAlong(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, okReply(), okReply())
	f := baseFailure()

	_, err := h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)

	h.now = fixedTestNow.Add(61 * time.Minute)
	_, err = h.analyzer.Analyze(ctx, f)
	require.NoError(t, err)

	in := h.model.lastCall()
	prompt := in.Messages[0].Content[0].(*types.ContentBlockMemberText).Value
	assert.Contains(t, prompt, "# PRIOR RCAs FOR THIS TOOL + ERROR CODE")
	assert.Contains(t, prompt, "rcaId=id0000000001")
	assert.NotContains(t, prompt, "# PRIOR RCAs FOR THIS TOOL + ERROR CODE\n(none)")
}

func TestNewAnalyzerRequiresItsCollaborators(t *testing.T) {
	base := Deps{
		Store: store.NewWithClient(testutil.NewFakeDynamo(), "t"),
		Model: &fakeModel{},
		Mail:  &fakeMailer{},
		Log:   observ.NewLogger(io.Discard, "error"),
		Cfg:   Config{ModelID: "m"},
	}
	_, err := NewAnalyzer(base)
	require.NoError(t, err)

	missing := base
	missing.Store = nil
	require.Error(t, mustErr(NewAnalyzer(missing)))
	missing = base
	missing.Model = nil
	require.Error(t, mustErr(NewAnalyzer(missing)))
	missing = base
	missing.Mail = nil
	require.Error(t, mustErr(NewAnalyzer(missing)))
	missing = base
	missing.Log = nil
	require.Error(t, mustErr(NewAnalyzer(missing)))
	missing = base
	missing.Cfg = Config{}
	require.Error(t, mustErr(NewAnalyzer(missing)))
}

func mustErr(_ *Analyzer, err error) error { return err }
