// Package rca is the tool-failure agentic root-cause analyzer (plan.md WS-2
// M17): when a tool call fails in production, an agent gathers the evidence,
// asks Claude Opus on Bedrock what actually went wrong, emails the owner a
// structured report, and files any base-knowledge it inferred as a *pending*
// suggestion for the owner to approve.
//
// One failure flows through one pipeline:
//
//  1. envelope validation — a tools.ToolFailure without a tool, an error code
//     or a verified userId is not analysable and is dropped, not retried;
//  2. dedupe: Signature(f) identifies "the same failure", and
//     Store.ClaimRCACooldown atomically claims that signature's analysis slot
//     for the next hour;
//  3. cost control: Store.ClaimRCADailyBudget atomically consumes one unit of
//     the day's cap (10 Opus calls/day worst case — this is the whole cost
//     story);
//  4. context gathering — the transcript window either side of the failing
//     call, the tool's advertised contract from tools.CatalogManifest(), prior
//     RCAs for the same tool+errorCode, the user's base-knowledge profile, and
//     docs.SystemMap;
//  5. one bedrockruntime.Converse call under a bounded deadline;
//  6. permissive extraction of the agreed JSON report, then an RCA# item
//     (30-day TTL), a plain-text SES report to the owner, and up to five
//     allowlisted PROFSUGG# suggestions.
//
// The seams are all interfaces or funcs — ContextStore, ModelInvoker, Mailer,
// Now, NewID — so the whole pipeline is exercised in tests against a fake
// Bedrock, a fake SES and an in-memory DynamoDB with no AWS credentials at all.
//
// Two properties are load-bearing and easy to break by accident:
//
//   - every permanent condition (unusable envelope, denied model, malformed
//     reply, genuine ValidationException) is reported through Outcome.Status
//     with a NIL error. A non-nil error from Analyze always means "transient,
//     please retry". cmd/rca-analyzer turns exactly those into SQS batch item
//     failures, so nothing in this pipeline can retry forever;
//   - the cooldown and budget claims happen BEFORE the model call. A
//     model_unavailable run therefore still consumes one unit of the day's
//     budget — deliberately, because that is what bounds parked records and
//     Dynamo writes to RCA_DAILY_CAP per day while Bedrock access is pending.
package rca

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// metricsNamespace is the EMF namespace for every RCA metric. Emitted via
// observ.EmitMetric (stdout EMF) — no PutMetricData, and no CloudWatch alarms
// (owner decision 2026-07-19).
const metricsNamespace = "LiveNinja/RCA"

// envelopeVersion is the tools.ToolFailure schema version this analyzer
// understands. A future producer bumping it must ship a consumer that knows
// the new shape; until then an unknown version is skipped rather than
// misinterpreted.
const envelopeVersion = 1

// ContextStore is the DynamoDB seam. *store.Store satisfies it; tests inject a
// store.NewWithClient(testutil.NewFakeDynamo(), ...) so the real key shapes are
// exercised rather than a hand-rolled map.
type ContextStore interface {
	ListSessionTurns(ctx context.Context, userID, sessionID string) ([]store.Turn, error)
	GetProfile(ctx context.Context, userID string) (store.Profile, error)
	RecentRCAs(ctx context.Context, family string, limit int32) ([]store.RCARecord, error)
	ClaimRCACooldown(ctx context.Context, family, signature string, now time.Time, cooldown time.Duration) (bool, error)
	ClaimRCADailyBudget(ctx context.Context, day string, cap int, now time.Time) (int, bool, error)
	ClaimRCANotice(ctx context.Context, kind string, now time.Time, window time.Duration) (bool, error)
	PutRCA(ctx context.Context, rec *store.RCARecord) error
	IncrementRCASuppressed(ctx context.Context, family, sk string) error
	PutProfileSuggestion(ctx context.Context, userID string, sg *store.ProfileSuggestion) error
}

// Deps is everything the analyzer needs.
type Deps struct {
	Store ContextStore
	Model ModelInvoker
	Mail  Mailer
	Log   *slog.Logger
	Cfg   Config

	// Now is the clock, defaulted to time.Now. NewID mints the 12-hex ids,
	// defaulted to crypto/rand. Both are overridden for deterministic tests.
	Now   func() time.Time
	NewID func() (string, error)
}

// Analyzer runs the RCA pipeline for one failure at a time.
type Analyzer struct {
	store ContextStore
	model ModelInvoker
	mail  Mailer
	log   *slog.Logger
	cfg   Config
	now   func() time.Time
	newID func() (string, error)
}

// NewAnalyzer validates Deps and applies the defaults. Store, Model, Mail, Log
// and Cfg.ModelID are all required: there is no useful half-analyzer, and a
// silently mail-less or model-less one would look healthy while producing
// nothing.
func NewAnalyzer(d Deps) (*Analyzer, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("rca: Deps.Store is required")
	case d.Model == nil:
		return nil, errors.New("rca: Deps.Model is required")
	case d.Mail == nil:
		return nil, errors.New("rca: Deps.Mail is required")
	case d.Log == nil:
		return nil, errors.New("rca: Deps.Log is required")
	case d.Cfg.ModelID == "":
		return nil, errors.New("rca: Cfg.ModelID is required")
	}
	a := &Analyzer{
		store: d.Store, model: d.Model, mail: d.Mail, log: d.Log, cfg: d.Cfg,
		now: d.Now, newID: d.NewID,
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.newID == nil {
		a.newID = NewID
	}
	if a.cfg.MaxOutputTokens <= 0 {
		a.cfg.MaxOutputTokens = defaultMaxOutputTokens
	}
	if a.cfg.ModelTimeout <= 0 {
		a.cfg.ModelTimeout = defaultModelTimeout
	}
	if a.cfg.DailyCap <= 0 {
		a.cfg.DailyCap = defaultDailyCap
	}
	if a.cfg.Cooldown <= 0 {
		a.cfg.Cooldown = defaultCooldown
	}
	if a.cfg.NoticeWindow <= 0 {
		a.cfg.NoticeWindow = noticeWindow
	}
	if a.cfg.EmailTo == "" {
		a.cfg.EmailTo = DefaultRecipient
	}
	if a.cfg.EmailFrom == "" {
		a.cfg.EmailFrom = DefaultFromAddress
	}
	if a.cfg.EmailReplyTo == "" {
		a.cfg.EmailReplyTo = DefaultReplyTo
	}
	return a, nil
}

// NewID mints a 12-lowercase-hex-character id (rcaId / suggId), the same shape
// cmd/topics-extract uses for topic ids.
func NewID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rca: id generation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Outcome statuses that are not store.RCAStatus* values: the pipeline stopped
// before an analysis existed to record.
const (
	OutcomeSuppressedCooldown = "suppressed_cooldown"
	OutcomeSuppressedCap      = "suppressed_daily_cap"
	OutcomeSkipped            = "skipped" // envelope not analysable
)

// Outcome is what one message produced — the assertable surface for tests and
// the thing the handler logs.
type Outcome struct {
	Status        string // one of store.RCAStatus*, or OutcomeSuppressed*/OutcomeSkipped
	RCAID         string
	Signature     string
	Family        string
	Emailed       bool
	SuggestionIDs []string
	BudgetUsed    int
}

// Handle is the thin SQS-shaped wrapper cmd/rca-analyzer calls: it discards the
// Outcome and returns Analyze's error verbatim, preserving the "non-nil means
// transient" contract the batch-item-failure response depends on.
func (a *Analyzer) Handle(ctx context.Context, f tools.ToolFailure) error {
	_, err := a.Analyze(ctx, f)
	return err
}

// Analyze runs the full pipeline for one failure and returns what happened. A
// non-nil error is ALWAYS transient — the caller retries it. Every permanent
// condition is reported through Outcome.Status with a nil error.
func (a *Analyzer) Analyze(ctx context.Context, f tools.ToolFailure) (Outcome, error) {
	now := a.now().UTC()
	l := a.log.With(
		slog.String("tool", f.Tool),
		slog.String("errorCode", f.ErrorCode),
		slog.String("source", f.Source),
	)

	// 1. Is this envelope analysable at all? Without a verified userId there is
	// no transcript partition to read and no profile to load, so there is
	// nothing to reason over; without a tool or an error code there is no
	// failure identity. All three are permanent conditions — drop, never retry.
	if f.V != envelopeVersion || f.Tool == "" || f.ErrorCode == "" || f.UserID == "" {
		l.Warn("rca: unusable failure envelope; skipping",
			slog.Int("v", f.V),
			slog.Bool("hasUser", f.UserID != ""))
		return Outcome{Status: OutcomeSkipped}, nil
	}

	family := Family(f.Tool, f.ErrorCode)
	sig := Signature(f)
	out := Outcome{Family: family, Signature: sig}
	l = l.With(slog.String("family", family), slog.String("signature", sig))

	// 2/3. Cooldown BEFORE budget. Reversing the two would burn daily budget on
	// suppressed duplicates — which is exactly the storm the cooldown exists to
	// absorb. (If the budget claim then fails, the cooldown marker stays set:
	// accepted, and documented at step 4 — it suppresses one signature for an
	// hour, and no analysis was owed anyway.)
	proceed, err := a.store.ClaimRCACooldown(ctx, family, sig, now, a.cfg.Cooldown)
	if err != nil {
		return out, fmt.Errorf("rca: claim cooldown: %w", err) // transient
	}
	if !proceed {
		observ.EmitMetric(metricsNamespace, "RcaSuppressed", 1, "Count",
			map[string]string{"Reason": "cooldown"})
		a.bumpSuppressed(ctx, l, family)
		l.Info("rca: duplicate failure suppressed by cooldown")
		out.Status = OutcomeSuppressedCooldown
		return out, nil
	}

	// 4. The day's budget. Note this is claimed before the model call, so a
	// model_unavailable run still spends a unit — see the package comment.
	count, granted, err := a.store.ClaimRCADailyBudget(ctx, DayKey(now), a.cfg.DailyCap, now)
	if err != nil {
		return out, fmt.Errorf("rca: claim daily budget: %w", err) // transient
	}
	out.BudgetUsed = count
	if !granted {
		observ.EmitMetric(metricsNamespace, "RcaSuppressed", 1, "Count",
			map[string]string{"Reason": "daily_cap"})
		l.Warn("rca: daily analysis cap reached; failure not analysed",
			slog.Int("dailyCap", a.cfg.DailyCap))
		out.Status = OutcomeSuppressedCap
		return out, nil
	}

	// 5. Gather the context.
	in, turnCount, err := a.gather(ctx, l, f, family)
	if err != nil {
		return out, err // transient: a table read failed
	}

	// 6. Render the prompt and pin its digest so context-gathering drift is
	// auditable from the record alone.
	prompt := BuildPrompt(in)
	digest := sha256.Sum256([]byte(prompt))

	rcaID, err := a.newID()
	if err != nil {
		return out, err // transient: crypto/rand failure
	}
	out.RCAID = rcaID
	l = l.With(slog.String("rcaId", rcaID))

	rec := store.RCARecord{
		PK:            family,
		RCAID:         rcaID,
		Tool:          f.Tool,
		ErrorCode:     f.ErrorCode,
		Signature:     sig,
		RequestedTool: f.RequestedTool,
		ErrorMessage:  truncateRunes(f.ErrorMessage, maxErrorMessageChars),
		ArgsJSON:      truncateRunes(f.ArgsJSON, maxArgsChars),
		TxID:          f.TxID,
		CallID:        f.CallID,
		UserID:        f.UserID,
		SessionID:     f.SessionID,
		Surface:       f.Surface,
		Engine:        in.Engine,
		TurnsInWindow: turnCount,
		OccurredAt:    occurredAtOr(f.OccurredAt, now),
		CreatedAt:     now.Format(time.RFC3339Nano),
		ModelID:       a.cfg.ModelID,
		PromptSHA256:  hex.EncodeToString(digest[:]),
	}

	// 7. One Converse call under its own deadline.
	modelCtx, cancel := context.WithTimeout(ctx, a.cfg.ModelTimeout)
	reply, cerr := a.model.Converse(modelCtx, buildConverseInput(a.cfg, prompt))
	cancel()
	if cerr != nil {
		return a.handleModelError(ctx, l, out, rec, cerr)
	}

	// A nil reply with a nil error is not a shape the SDK produces, but treating
	// it as an unusable reply (rather than dereferencing it) keeps the pipeline
	// honest against a future client/middleware that does.
	if reply == nil {
		return a.handleMalformedReply(ctx, l, out, rec, "", ErrNoJSON)
	}

	raw := converseText(reply)
	rec.StopReason = string(reply.StopReason)
	if reply.Usage != nil {
		if reply.Usage.InputTokens != nil {
			rec.InputTokens = int(*reply.Usage.InputTokens)
		}
		if reply.Usage.OutputTokens != nil {
			rec.OutputTokens = int(*reply.Usage.OutputTokens)
		}
	}

	rep, err := ExtractReport(raw)
	if err != nil {
		return a.handleMalformedReply(ctx, l, out, rec, raw, err)
	}

	// 8. Persist, file suggestions, report.
	rec.Status = store.RCAStatusAnalyzed
	rec.Symptom = rep.Symptom
	rec.RootCause = rep.RootCause
	rec.Evidence = rep.Evidence
	rec.Confidence = rep.Confidence
	rec.CodeFixes = rep.CodeFixSuggestions
	rec.ReproSteps = rep.ReproSteps
	if len(in.Prior) > 0 {
		// Carry the running suppression count forward so the report can say
		// "N similar since the last report" without a second query.
		rec.SuppressedCount = in.Prior[0].SuppressedCount
	}

	var notes []string
	if reply.StopReason == types.StopReasonMaxTokens {
		notes = append(notes, "NOTE: the model response hit the output-token limit "+
			"(RCA_MAX_OUTPUT_TOKENS), so the report below may be incomplete.")
	}
	if strings.Contains(prompt, strings.TrimSpace(truncationMarker)) {
		notes = append(notes, "NOTE: some gathered context was truncated to fit the prompt budget.")
	}

	filed, suggNotes := a.fileSuggestions(ctx, l, f.UserID, rcaID, in.Profile, rep)
	notes = append(notes, suggNotes...)
	rec.SuggestionIDs = filed
	out.SuggestionIDs = filed

	return a.persistAndReport(ctx, l, out, rec, rep, in, notes)
}

// gather assembles the PromptInput. Only ListSessionTurns is allowed to fail
// softly: the transcript is nice-to-have context, and a *fallback* turn
// (internal/realtime/fallback.go) can legitimately have produced a tool call
// with no transcript rows at all — refusing to analyse it would drop exactly
// the degraded-path failures most worth understanding. Every other read failing
// is transient.
func (a *Analyzer) gather(ctx context.Context, l *slog.Logger, f tools.ToolFailure, family string) (PromptInput, int, error) {
	var turns []store.Turn
	if f.SessionID != "" {
		got, err := a.store.ListSessionTurns(ctx, f.UserID, f.SessionID)
		if err != nil {
			l.Warn("rca: transcript read failed; analysing without a conversation window",
				slog.String("error", err.Error()))
		} else {
			turns = got
		}
	}

	profile, err := a.store.GetProfile(ctx, f.UserID)
	if err != nil {
		return PromptInput{}, 0, fmt.Errorf("rca: load profile: %w", err)
	}
	prior, err := a.store.RecentRCAs(ctx, family, priorRCALimit)
	if err != nil {
		return PromptInput{}, 0, fmt.Errorf("rca: load prior rcas: %w", err)
	}

	window := WindowTurns(turns, f)
	in := PromptInput{
		Failure:  f,
		Contract: RenderToolContract(f.Tool),
		Window:   window,
		Prior:    prior,
		Profile:  profile,
		// The session's voice engine never reaches the tool router (writeAudit
		// stamps a constant engine="tool-router" on the audit row), so it is
		// recovered from the transcript — the same trick cmd/topics-extract's
		// firstEngine uses.
		Engine: firstEngine(turns),
	}
	return in, len(window), nil
}

// handleModelError disposes of a Converse failure per its class. Transient
// returns the error (the message goes back on the queue); everything else
// persists what was gathered, warns, emits a metric and returns nil so the
// message is deleted.
func (a *Analyzer) handleModelError(ctx context.Context, l *slog.Logger, out Outcome,
	rec store.RCARecord, cerr error) (Outcome, error) {
	class, reason := ClassifyBedrockError(cerr)
	rec.DegradeReason = reason

	switch class {
	case ClassTransient:
		l.Warn("rca: transient bedrock failure; message will be retried",
			slog.String("reason", reason), slog.String("error", cerr.Error()))
		return out, fmt.Errorf("rca: bedrock converse: %w", cerr)

	case ClassModelUnavailable:
		// A denied model-access grant is a PENDING OWNER ACTION, not a broken
		// deploy, and must not read like one in CloudWatch — hence Warn, not
		// Error. Returning nil is the critical anti-storm rule: this failure
		// will recur identically on every retry, and with maxReceiveCount 3 a
		// flooded queue would triple the Dynamo writes and re-race the notice
		// claim for nothing.
		rec.Status = store.RCAStatusModelUnavailable
		out.Status = rec.Status
		if err := a.store.PutRCA(ctx, &rec); err != nil {
			// The record is the whole value of this path (it is what makes the
			// missed analysis re-runnable), so a failed write IS worth a retry.
			return out, fmt.Errorf("rca: persist model_unavailable record: %w", err)
		}
		l.Warn("rca: bedrock model unavailable; RCA recorded without analysis",
			slog.String("status", rec.Status),
			slog.String("modelId", rec.ModelID),
			slog.String("reason", reason),
			slog.String("rcaId", rec.RCAID))
		observ.EmitMetric(metricsNamespace, "RcaModelUnavailable", 1, "Count",
			map[string]string{"Model": rec.ModelID})
		subject, body := modelUnavailableNotice(a.cfg, reason)
		out.Emailed = a.sendNotice(ctx, l, NoticeModelUnavailable, subject, body)
		return out, nil

	default: // ClassPermanent
		// A genuine ValidationException is a code bug for a developer to find
		// in the logs, not an owner action — so it is recorded, but no email is
		// sent at all.
		rec.Status = store.RCAStatusUpstreamError
		out.Status = rec.Status
		if err := a.store.PutRCA(ctx, &rec); err != nil {
			return out, fmt.Errorf("rca: persist upstream_error record: %w", err)
		}
		l.Warn("rca: permanent bedrock failure; RCA recorded without analysis",
			slog.String("reason", reason), slog.String("error", cerr.Error()))
		observ.EmitMetric(metricsNamespace, "RcaUpstreamError", 1, "Count", nil)
		return out, nil
	}
}

// handleMalformedReply records a reply that could not be parsed into the agreed
// report. Permanent by construction: re-running produces the same broken reply
// and burns Opus tokens for it.
func (a *Analyzer) handleMalformedReply(ctx context.Context, l *slog.Logger, out Outcome,
	rec store.RCARecord, raw string, cause error) (Outcome, error) {
	rec.Status = store.RCAStatusMalformedResponse
	rec.DegradeReason = cause.Error()
	rec.RawResponse = truncateRunes(raw, maxRawResponseChars)
	out.Status = rec.Status

	if err := a.store.PutRCA(ctx, &rec); err != nil {
		return out, fmt.Errorf("rca: persist malformed_response record: %w", err)
	}
	l.Warn("rca: model reply not parseable; no report sent",
		slog.String("reason", cause.Error()),
		slog.String("stopReason", rec.StopReason),
		slog.Int("outputTokens", rec.OutputTokens))
	observ.EmitMetric(metricsNamespace, "RcaMalformedResponse", 1, "Count", nil)

	subject, body := malformedResponseNotice(a.cfg, rec.RCAID, rec.PK, cause.Error())
	out.Emailed = a.sendNotice(ctx, l, NoticeMalformedResponse, subject, body)
	return out, nil
}

// persistAndReport writes the record, sends the report, then re-writes the
// record with the delivery result.
//
// The ordering is deliberate. Writing FIRST (with emailed:false) means a send
// failure still leaves a durable, queryable artifact; sending second means the
// email can quote the PROFSUGG ids that were actually filed. A send failure
// then returns nil rather than an error: the cooldown was already claimed, so a
// redelivery would be suppressed and the retry could never deliver the report
// anyway — the record on the table is the artifact that matters, and
// RcaEmailFailed is the signal that it did not reach a mailbox.
//
// Email idempotency needs no IDEMP# marker of its own: an SQS redelivery after a
// successful send recomputes the same Signature, finds the cooldown marker
// written moments earlier, loses the claim, and exits as suppressed_cooldown.
func (a *Analyzer) persistAndReport(ctx context.Context, l *slog.Logger, out Outcome,
	rec store.RCARecord, rep Report, in PromptInput, notes []string) (Outcome, error) {
	putErr := a.store.PutRCA(ctx, &rec)
	if putErr != nil {
		// Do not bail before sending: the Opus call is already paid for, and
		// losing the report as well would waste it entirely.
		l.Error("rca: record write failed; sending the report anyway",
			slog.String("error", putErr.Error()))
	}

	subject := Subject(rec.Tool, rep.Symptom, rec.ErrorCode)
	body := Body(rec, rep, in, notes, a.cfg)

	msgID, sendErr := a.send(ctx, subject, body)
	if sendErr != nil {
		l.Warn("rca: report email failed", slog.String("error", sendErr.Error()))
		observ.EmitMetric(metricsNamespace, "RcaEmailFailed", 1, "Count", nil)
	} else {
		rec.Emailed = true
		rec.EmailMessageID = msgID
		out.Emailed = true
		observ.EmitMetric(metricsNamespace, "RcaEmailsSent", 1, "Count", nil)
		if putErr == nil {
			// Idempotent overwrite of the same pk/sk — one extra write to make
			// "did it send?" answerable from the table.
			if err := a.store.PutRCA(ctx, &rec); err != nil {
				l.Warn("rca: record emailed-flag update failed",
					slog.String("error", err.Error()))
			}
		}
	}

	if putErr != nil {
		// A Dynamo write failure is genuinely retryable, so surface it; the
		// redelivery will be suppressed by the cooldown (no second email).
		return out, fmt.Errorf("rca: persist rca record: %w", putErr)
	}

	out.Status = store.RCAStatusAnalyzed
	observ.EmitMetric(metricsNamespace, "RcaAnalyzed", 1, "Count",
		map[string]string{"Tool": rec.Tool})
	l.Info("rca: analysis complete",
		slog.String("confidence", rep.Confidence),
		slog.Bool("emailed", rec.Emailed),
		slog.Int("suggestions", len(rec.SuggestionIDs)),
		slog.Int("inputTokens", rec.InputTokens),
		slog.Int("outputTokens", rec.OutputTokens))
	return out, nil
}

// fileSuggestions writes the allowlisted base-knowledge proposals as pending
// PROFSUGG# rows and returns their ids plus any notes for the email.
//
// A rejected proposal is not an error: the model was told the allowlist, and a
// location/name/email proposal is dropped by design (see
// allowedSuggestionFields). A *write* failure is not fatal either — the report
// email is the primary artifact, and a missing suggestion row costs the owner
// nothing but a suggestion.
func (a *Analyzer) fileSuggestions(ctx context.Context, l *slog.Logger, userID, rcaID string,
	profile store.Profile, rep Report) ([]string, []string) {
	kept, rejected := AllowedSuggestions(rep)
	var notes []string

	for _, field := range rejected {
		observ.EmitMetric(metricsNamespace, "RcaSuggestionRejected", 1, "Count",
			map[string]string{"Field": sanitizeKeyComponent(field)})
		l.Warn("rca: base-knowledge suggestion rejected (outside the allowlist)",
			slog.String("field", field))
	}
	if len(rejected) > 0 {
		notes = append(notes, fmt.Sprintf(
			"NOTE: %d base-knowledge suggestion(s) were dropped as outside the allowlist "+
				"(only profile.units and profile.notes[] may be proposed): %s",
			len(rejected), strings.Join(rejected, ", ")))
	}

	ids := make([]string, 0, len(kept))
	for _, sg := range kept {
		suggID, err := a.newID()
		if err != nil {
			l.Warn("rca: suggestion id generation failed", slog.String("error", err.Error()))
			continue
		}
		rec := &store.ProfileSuggestion{
			SuggID:        suggID,
			Status:        store.SuggestionStatusPending,
			Field:         sg.Field,
			CurrentValue:  currentProfileValue(profile, sg.Field),
			ProposedValue: sg.ProposedValue,
			Reason:        sg.Reason,
			Source:        "rca",
			SourceRef:     rcaID,
		}
		if err := a.store.PutProfileSuggestion(ctx, userID, rec); err != nil {
			l.Warn("rca: filing base-knowledge suggestion failed",
				slog.String("field", sg.Field), slog.String("error", err.Error()))
			continue
		}
		ids = append(ids, suggID)
		observ.EmitMetric(metricsNamespace, "RcaSuggestionsFiled", 1, "Count",
			map[string]string{"Field": sanitizeKeyComponent(sg.Field)})
	}
	if len(ids) == 0 {
		return nil, notes
	}
	return ids, notes
}

// currentProfileValue renders what the profile holds today for an allowlisted
// field, so the Settings drawer can show "imperial -> metric" rather than a
// bare proposal. profile.notes[] is an ADDITION, not a replacement, so it has
// no current value by construction — showing the existing notes there would
// imply approving the suggestion replaces them.
func currentProfileValue(p store.Profile, field string) string {
	if field == FieldProfileUnits {
		return p.Units
	}
	return ""
}

// bumpSuppressed increments suppressedCount on the family's newest record so
// the next report can say "suppressed N similar since". Entirely best-effort:
// an empty family (the cooldown marker outlived its record's 30-day TTL) or a
// Dynamo hiccup here must never turn a correctly-suppressed duplicate into a
// retried message.
func (a *Analyzer) bumpSuppressed(ctx context.Context, l *slog.Logger, family string) {
	recent, err := a.store.RecentRCAs(ctx, family, 1)
	if err != nil {
		l.Warn("rca: suppression bump lookup failed", slog.String("error", err.Error()))
		return
	}
	if len(recent) == 0 {
		return
	}
	if err := a.store.IncrementRCASuppressed(ctx, family, recent[0].SK); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		l.Warn("rca: suppression bump failed", slog.String("error", err.Error()))
	}
}

// sendNotice claims the once-per-window slot for an operational notice and, on
// a successful claim, sends it. Reports whether mail went out.
func (a *Analyzer) sendNotice(ctx context.Context, l *slog.Logger, kind, subject, body string) bool {
	claimed, err := a.store.ClaimRCANotice(ctx, kind, a.now().UTC(), a.cfg.NoticeWindow)
	if err != nil {
		l.Warn("rca: notice claim failed; notice not sent",
			slog.String("kind", kind), slog.String("error", err.Error()))
		return false
	}
	if !claimed {
		l.Info("rca: operational notice suppressed (already sent inside the window)",
			slog.String("kind", kind))
		return false
	}
	if _, err := a.send(ctx, subject, body); err != nil {
		l.Warn("rca: notice email failed",
			slog.String("kind", kind), slog.String("error", err.Error()))
		observ.EmitMetric(metricsNamespace, "RcaEmailFailed", 1, "Count", nil)
		return false
	}
	observ.EmitMetric(metricsNamespace, "RcaEmailsSent", 1, "Count", nil)
	l.Info("rca: operational notice sent", slog.String("kind", kind))
	return true
}

// firstEngine returns the first non-empty engine recorded on a transcript turn,
// skipping the tool router's own audit rows (which always carry the constant
// engine="tool-router" and would otherwise mask the real voice engine).
func firstEngine(turns []store.Turn) string {
	for _, t := range turns {
		if t.Role == "tool" || t.Engine == "" {
			continue
		}
		return t.Engine
	}
	return ""
}

// occurredAtOr validates the producer's timestamp and substitutes the analyzer
// clock when it is missing or unparseable — occurredAt is part of the record's
// sort key, so it must never be empty or non-sortable.
func occurredAtOr(ts string, now time.Time) string {
	if _, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return ts
	}
	return now.Format(time.RFC3339Nano)
}
