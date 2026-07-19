// Command topics-extract is the direct-invoke post-session topic tagger
// (M11, FR-TOP-01/02/03): the web function's transcript sink async-invokes
// it (InvocationType=Event) when a client flushes its final transcript
// batch with {final:true}. It:
//
//  1. Reads the session's LOG#<sessionId># transcript turns (Query,
//     single partition — never a Scan).
//  2. Invokes the realtime broker's "extract-topics" mode (the broker is
//     the sole OpenAI key holder; gpt-4o-mini, engine-agnostic — works
//     the same whatever voice engine produced the transcript).
//  3. Creates any proposed new topics (stable random ids, deterministic
//     palette colors), writes one TREF# assignment row per topic (bumping
//     each topic's convCount only when the ref is new), and finally writes
//     the canonical CONV# record.
//
// Idempotency: a conditional CONVSESS#<sessionId> claim marker pins ONE
// canonical timestamp per session up front, so even a client that flushed
// {final:true} twice with different timestamps converges on a single CONV
// row. The CONV# record itself is written last with a conditional put —
// an async-retry that finds it already present exits without re-tagging,
// and every TREF put is itself conditional, so a crash-and-retry midway
// never double-counts or duplicates rows.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	awslambda "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const metricsNamespace = "LiveNinja/TopicsExtract"

// defaultBrokerFn matches template.yaml's fixed RealtimeBrokerFunction
// FunctionName; BROKER_FUNCTION_NAME overrides when set.
const defaultBrokerFn = "live-ninja-realtime-broker"

// transcript shaping caps: keep the extraction prompt cheap and bounded
// even for marathon sessions — keep the head and tail, drop the middle.
const (
	transcriptHeadChars = 12000
	transcriptTailChars = 12000
)

// maxTitleRunes caps the conversation title (first user utterance).
const maxTitleRunes = 80

// extractionInFlightGrace: how long another attempt's session claim is
// treated as "still running" (broker extraction takes a few seconds; async
// Lambda retries arrive minutes apart, comfortably past this window).
const extractionInFlightGrace = 2 * time.Minute

// Event is the invoke payload from the transcript sink
// (internal/webapp/api_routes.go, {final:true} seam). Identity fields come
// from the web function's verified authorizer context, never a client body.
type Event struct {
	UserID    string `json:"userId"`
	SessionID string `json:"sessionId"`
	// TS is the conversation timestamp (RFC3339 UTC, session end time) —
	// it becomes part of the CONV#/TREF# sort keys, so retried deliveries
	// of the same event address the same items.
	TS       string `json:"ts"`
	DeviceID string `json:"deviceId,omitempty"`
	Surface  string `json:"surface,omitempty"`
	// Client-estimated session cost (list price), shipped by the web
	// function from the final transcript flush; zero = not reported.
	CostUSD         float64 `json:"costUsd,omitempty"`
	CostTextTokens  int     `json:"costTextTokens,omitempty"`
	CostAudioTokens int     `json:"costAudioTokens,omitempty"`
}

// brokerRequest / brokerResponse mirror cmd/realtime-broker's wire shapes
// (that package is `main` and cannot be imported).
type brokerRequest struct {
	Mode    string          `json:"mode"`
	UserID  string          `json:"userId"`
	Surface string          `json:"surface"`
	Payload json.RawMessage `json:"payload"`
}

type brokerResponse struct {
	Error     string   `json:"error,omitempty"`
	Code      int      `json:"code,omitempty"`
	Message   string   `json:"message,omitempty"`
	TopicIDs  []string `json:"topicIds,omitempty"`
	NewTopics []string `json:"newTopics,omitempty"`
}

// lambdaInvokeAPI is the subset of the Lambda client the handler needs,
// interface-typed so tests inject a fake broker.
type lambdaInvokeAPI interface {
	Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

type handler struct {
	log      *slog.Logger
	store    *store.Store
	lambda   lambdaInvokeAPI
	brokerFn string
}

var validSurfaces = map[string]bool{"web": true, "android": true, "device": true}

// Handle processes one session-end event. Returning an error triggers the
// async-invoke retry machinery; permanent conditions (empty transcript,
// broker 4xx, already-processed session) return nil so they are not
// retried pointlessly.
func (h *handler) Handle(ctx context.Context, ev Event) error {
	if ev.UserID == "" || ev.SessionID == "" {
		return errors.New("topics-extract: userId and sessionId are required")
	}
	ts := ev.TS
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	l := h.log.With(slog.String("userId", ev.UserID), slog.String("sessionId", ev.SessionID))

	// Pin the session's canonical timestamp FIRST (conditional put of a
	// CONVSESS#<sessionId> marker): the first final flush wins, and every
	// later invoke — an async-retry redelivery of the same event, or a
	// second final:true flush carrying a DIFFERENT ts (End button + pagehide
	// seconds apart, the duplicate-CONV bug) — adopts the stored ts so all
	// attempts upsert the same CONV#<ts>#<sessionId> row.
	claim, err := h.store.ClaimConversationSession(ctx, ev.UserID, ev.SessionID, ts)
	if err != nil {
		return err
	}

	// Already processed? (Retried async delivery, or a client that sent
	// {final:true} twice — the claim maps both onto one canonical CONV.)
	if conv, err := h.store.GetConversation(ctx, ev.UserID, claim.TS+"#"+ev.SessionID); err != nil {
		return err
	} else if conv != nil {
		l.Info("topics-extract: conversation already recorded; skipping")
		return nil
	}

	// A DIFFERENT event claimed this session moments ago and its CONV
	// hasn't landed yet: that attempt is almost certainly still in flight
	// (the broker call takes seconds). Error out so the async-invoke retry
	// re-checks later — by then the first attempt has either recorded the
	// CONV (skip above) or died (grace expired: resume with its ts).
	if claim.Existing && claim.TS != ts && time.Since(claim.ClaimedAt) < extractionInFlightGrace {
		return fmt.Errorf("topics-extract: session %s extraction in flight (claimed %s); retrying later",
			ev.SessionID, claim.ClaimedAt.Format(time.RFC3339))
	}
	ts = claim.TS

	turns, err := h.store.ListSessionTurns(ctx, ev.UserID, ev.SessionID)
	if err != nil {
		return err
	}
	spoken := make([]store.Turn, 0, len(turns))
	for _, t := range turns {
		if t.Role == "system" || strings.TrimSpace(t.Text) == "" {
			continue // broker's seq-0 "session-start" marker etc.
		}
		spoken = append(spoken, t)
	}
	if len(spoken) == 0 {
		l.Info("topics-extract: session has no spoken turns; nothing to tag")
		return nil
	}

	topics, err := h.store.ListTopics(ctx, ev.UserID)
	if err != nil {
		return err
	}
	existing := activeTopicOptions(topics)

	result, retryable, err := h.extractViaBroker(ctx, ev, spoken, existing)
	if err != nil {
		observ.EmitMetric(metricsNamespace, "ExtractionErrors", 1, "Count", nil)
		if retryable {
			return err
		}
		l.Error("topics-extract: permanent extraction failure; dropping", slog.String("error", err.Error()))
		return nil
	}

	// Create proposed new topics (stable ids, deterministic colors).
	finalIDs := append([]string{}, result.TopicIDs...)
	for _, name := range result.NewTopics {
		id, err := newTopicID()
		if err != nil {
			return fmt.Errorf("topics-extract: topic id generation: %w", err)
		}
		t := &store.Topic{TopicID: id, Name: name, Color: topicColor(name), CreatedAt: ts}
		if err := h.store.CreateTopic(ctx, ev.UserID, t); err != nil {
			return err
		}
		finalIDs = append(finalIDs, id)
	}
	finalIDs = dedupe(finalIDs)

	// Assignment rows first, canonical CONV record last (the completion
	// marker) — see the package comment's idempotency notes.
	for _, topicID := range finalIDs {
		ref := &store.TopicRef{
			TopicID:   topicID,
			SessionID: ev.SessionID,
			TS:        ts,
			DeviceID:  ev.DeviceID,
		}
		switch err := h.store.PutTopicRef(ctx, ev.UserID, ref); {
		case err == nil:
			if err := h.store.IncrementTopicConvCount(ctx, ev.UserID, topicID, 1); err != nil &&
				!errors.Is(err, store.ErrNotFound) {
				return err
			}
		case errors.Is(err, store.ErrAlreadyExists):
			// Retried event already wrote this ref (and its count bump).
		default:
			return err
		}
	}

	conv := &store.Conversation{
		SessionID:       ev.SessionID,
		TS:              ts,
		DeviceID:        ev.DeviceID,
		Engine:          firstEngine(spoken),
		Surface:         resolveSurface(ev, spoken),
		Title:           conversationTitle(spoken),
		TopicIDs:        finalIDs,
		TurnCount:       len(spoken),
		CostUSD:         ev.CostUSD,
		CostTextTokens:  ev.CostTextTokens,
		CostAudioTokens: ev.CostAudioTokens,
	}
	if err := h.store.CreateConversation(ctx, ev.UserID, conv); err != nil &&
		!errors.Is(err, store.ErrAlreadyExists) {
		return err
	}

	observ.EmitMetric(metricsNamespace, "ConversationsTagged", 1, "Count", nil)
	l.Info("topics-extract: conversation tagged",
		slog.Int("topics", len(finalIDs)),
		slog.Int("newTopics", len(result.NewTopics)),
		slog.Int("turns", len(spoken)))
	return nil
}

// extractViaBroker invokes the broker's extract-topics mode. retryable
// reports whether a failure is worth an async-invoke retry (transport
// errors and 5xx-class broker errors) or permanent (broker 4xx).
func (h *handler) extractViaBroker(ctx context.Context, ev Event, spoken []store.Turn, existing []realtime.TopicOption) (*realtime.ExtractResult, bool, error) {
	payload, err := json.Marshal(map[string]any{
		"transcript":     buildTranscript(spoken),
		"existingTopics": existing,
	})
	if err != nil {
		return nil, false, fmt.Errorf("topics-extract: marshal payload: %w", err)
	}
	reqBody, err := json.Marshal(brokerRequest{
		Mode:    "extract-topics",
		UserID:  ev.UserID,
		Surface: resolveSurface(ev, spoken),
		Payload: payload,
	})
	if err != nil {
		return nil, false, fmt.Errorf("topics-extract: marshal broker request: %w", err)
	}

	out, err := h.lambda.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: aws.String(h.brokerFn),
		Payload:      reqBody,
	})
	if err != nil {
		return nil, true, fmt.Errorf("topics-extract: invoke broker: %w", err)
	}
	if out.FunctionError != nil {
		return nil, true, fmt.Errorf("topics-extract: broker function error %q", aws.ToString(out.FunctionError))
	}

	var resp brokerResponse
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		return nil, false, fmt.Errorf("topics-extract: decode broker response: %w", err)
	}
	if resp.Error != "" {
		retryable := resp.Code == 0 || resp.Code >= http.StatusInternalServerError ||
			resp.Code == http.StatusBadGateway
		return nil, retryable, fmt.Errorf("topics-extract: broker %s (%d): %s", resp.Error, resp.Code, resp.Message)
	}
	return &realtime.ExtractResult{TopicIDs: resp.TopicIDs, NewTopics: resp.NewTopics}, false, nil
}

// ---- pure helpers ----

// activeTopicOptions filters the taxonomy down to what the extractor may
// tag against: not archived and not merged away (merged topics are
// aliases — their conversations already point at the destination id).
func activeTopicOptions(topics []store.Topic) []realtime.TopicOption {
	opts := make([]realtime.TopicOption, 0, len(topics))
	for _, t := range topics {
		if t.Archived || t.MergedInto != "" {
			continue
		}
		opts = append(opts, realtime.TopicOption{ID: t.TopicID, Name: t.Name})
	}
	return opts
}

// buildTranscript flattens the turns into "role: text" lines, keeping the
// head and tail when the whole thing exceeds the prompt budget.
func buildTranscript(turns []store.Turn) string {
	var b strings.Builder
	for _, t := range turns {
		b.WriteString(t.Role)
		b.WriteString(": ")
		b.WriteString(t.Text)
		b.WriteString("\n")
	}
	s := b.String()
	if len(s) <= transcriptHeadChars+transcriptTailChars {
		return s
	}
	return s[:transcriptHeadChars] + "\n[... transcript truncated ...]\n" + s[len(s)-transcriptTailChars:]
}

// resolveSurface picks the broker-valid surface: the event's (from the
// verified auth context), else the first turn's recorded surface, else web.
func resolveSurface(ev Event, spoken []store.Turn) string {
	if validSurfaces[ev.Surface] {
		return ev.Surface
	}
	for _, t := range spoken {
		if validSurfaces[t.Surface] {
			return t.Surface
		}
	}
	return "web"
}

// firstEngine returns the first non-empty engine recorded on a turn.
func firstEngine(spoken []store.Turn) string {
	for _, t := range spoken {
		if t.Engine != "" {
			return t.Engine
		}
	}
	return ""
}

// conversationTitle is the first user utterance, rune-capped — a cheap,
// deterministic list label (no extra model call).
func conversationTitle(spoken []store.Turn) string {
	for _, t := range spoken {
		if t.Role != "user" {
			continue
		}
		title := strings.TrimSpace(t.Text)
		if r := []rune(title); len(r) > maxTitleRunes {
			title = string(r[:maxTitleRunes-1]) + "…"
		}
		return title
	}
	return ""
}

func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// newTopicID returns a 12-hex-char random stable topic id.
func newTopicID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// topicPalette are the auto-assigned topic chip colors (UI shows these on
// topic chips; the Topic Manager lets the user recolor). Deterministic by
// name so the same proposal always lands the same color.
var topicPalette = []string{
	"#e6194b", "#3cb44b", "#b8860b", "#4363d8", "#f58231",
	"#911eb4", "#0e7490", "#c2417f", "#5b8a00", "#008080",
}

func topicColor(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(name)))
	return topicPalette[int(h.Sum32())%len(topicPalette)]
}

func main() {
	ctx := context.Background()
	appCfg := config.FromEnv()
	logger := observ.NewLogger(os.Stdout, appCfg.LogLevel)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("topics-extract: load aws config failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	st, err := store.New(ctx, appCfg.TableName)
	if err != nil {
		logger.Error("topics-extract: store init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	brokerFn := os.Getenv("BROKER_FUNCTION_NAME")
	if brokerFn == "" {
		brokerFn = defaultBrokerFn
	}

	h := &handler{
		log:      logger,
		store:    st,
		lambda:   lambda.NewFromConfig(awsCfg),
		brokerFn: brokerFn,
	}
	awslambda.Start(h.Handle)
}
