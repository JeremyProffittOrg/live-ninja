// Package tools is the Live Ninja server-side tool router (plan.md M2,
// FR-V04): the single execution path behind POST /api/v1/tools/invoke
// through which every surface (web, Android, and the M5Stack over plain
// HTTPS) runs realtime `function_call`s.
//
// Every invocation flows through one pipeline:
//
//  1. enumerated-argument validation against the tool's declared schema
//     (JSON-schema-style: typed params, required flags, enums, bounds);
//  2. per-call re-authorization via the caller-supplied Reauthorize
//     callback (user status + allowlist re-check — never trust a JWT
//     minted before a revocation);
//  3. idempotency for side-effecting tools: a conditional PutItem at
//     IDEMP#<userId>#<key> (24h TTL) makes duplicate deliveries a no-op;
//  4. real execution (no stubs — SES/SQS email, EventBridge Scheduler,
//     IoT publish, Open-Meteo, Wikipedia, DynamoDB notes);
//  5. an audit LOG# write into the caller's transcript partition and a
//     ToolInvocations EMF metric.
//
// The registry also renders the OpenAI Realtime tool manifest
// (Manifest) that the realtime broker binds into every ephemeral
// session, so the schema advertised to the model and the schema
// enforced here are one and the same.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Error codes carried by ToolError. The HTTP layer maps these via
// Result.StatusCode; realtime clients feed Result JSON back to the model
// as the function_call_output either way.
const (
	CodeUnknownTool          = "unknown_tool"
	CodeInvalidArgs          = "invalid_args"
	CodeConfirmationRequired = "confirmation_required"
	CodeForbidden            = "forbidden"
	CodeNotFound             = "not_found"
	CodeAlreadyExists        = "already_exists"
	CodeNotConfigured        = "not_configured"
	CodeUpstreamError        = "upstream_error"
)

const (
	// idempotencyTTL bounds IDEMP# markers for tool calls (shared spec:
	// ttl now+24h).
	idempotencyTTL = 24 * time.Hour

	// auditTTL is the transcript-store retention for tool audit LOG#
	// items (shared spec: ttl now+90d).
	auditTTL = 90 * 24 * time.Hour

	// auditEngine is the `engine` attribute stamped on audit LOG# items.
	auditEngine = "tool-router"

	// maxAuditText caps the serialized args/output summary persisted in
	// an audit line so a pathological argument can't bloat the item.
	maxAuditText = 512
)

// ToolError is a structured, client-safe tool failure. TxID carries the
// transaction correlation id so the failure the model (and the human) sees
// matches the canonical error envelope {code, message, txId}; it is stamped
// by the invocation pipeline (Invoke/finish), not by individual handlers.
type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	TxID    string `json:"txId,omitempty"`
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

func toolErrf(code, format string, a ...any) *ToolError {
	return &ToolError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Invocation is one tool call as extracted by the HTTP layer from
// POST /api/v1/tools/invoke plus the authorizer context. UserID,
// SessionID, and Surface come from verified claims — never from the
// request body (anti-confused-deputy, NFR-02).
type Invocation struct {
	Tool           string         `json:"tool"`
	Args           map[string]any `json:"args"`
	IdempotencyKey string         `json:"idempotencyKey"`
	CallID         string         `json:"callId"`

	// TxID is the transaction correlation id forwarded by the HTTP layer
	// (the ingress txId). Generated in Invoke when absent so every tool call
	// is traceable and every ToolError carries a Ref the user can report.
	TxID string `json:"txId,omitempty"`

	// Authorizer-derived context (set server-side by the caller).
	UserID    string `json:"-"`
	SessionID string `json:"-"`
	Surface   string `json:"-"`
	Role      string `json:"-"`
}

// Result is the outcome of one tool invocation. It is safe to serialize
// straight back to the client (and from there into the model as the
// function_call_output payload).
type Result struct {
	Tool      string         `json:"tool"`
	CallID    string         `json:"callId,omitempty"`
	TxID      string         `json:"txId,omitempty"`
	OK        bool           `json:"ok"`
	Duplicate bool           `json:"duplicate,omitempty"`
	Output    map[string]any `json:"output,omitempty"`
	Error     *ToolError     `json:"error,omitempty"`
}

// StatusCode maps the result to the HTTP status the /api/v1/tools/invoke
// handler should return.
func (r *Result) StatusCode() int {
	if r.OK {
		return http.StatusOK
	}
	if r.Error == nil {
		return http.StatusInternalServerError
	}
	switch r.Error.Code {
	case CodeUnknownTool, CodeNotFound:
		return http.StatusNotFound
	case CodeAlreadyExists:
		return http.StatusConflict
	case CodeInvalidArgs, CodeConfirmationRequired:
		return http.StatusBadRequest
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotConfigured:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// ParamSpec is one declared tool argument — the enforced schema and the
// JSON-schema fragment advertised to the model are both derived from it.
type ParamSpec struct {
	Name        string
	Type        string // "string" | "integer" | "number" | "boolean" | "string_array"
	Description string
	Required    bool
	Enum        []string // valid values for string params; empty = unconstrained
	MinLen      int      // strings: minimum length (0 = no minimum beyond non-empty required)
	MaxLen      int      // strings: maximum length (0 = no cap)
	Min         *float64 // numbers: inclusive minimum
	Max         *float64 // numbers: inclusive maximum

	// SafeName restricts a string param to a safe filename slug: it must
	// match safeFileNamePattern (ASCII letters/digits/dot/dash/underscore,
	// leading alphanumeric) and contain no ".." — so path traversal, path
	// separators, control characters, and hidden-file names are rejected
	// at the schema gate before any handler runs.
	SafeName bool

	// Unadvertised, when true, keeps this param out of the rendered JSON
	// schema (Manifest / the future CatalogManifest) while validateArgs
	// still accepts, coerces, and enforces it. This is for back-compat
	// aliases a model must never be *taught* — advertising two spellings
	// for the same argument just re-introduces the ambiguity — but that
	// real callers may still send and that must keep working regardless
	// (e.g. set_timer's legacy "seconds" spelling; see scheduler.go).
	Unadvertised bool

	// OutOfRangeHint, when non-empty, is appended to the standard
	// "must be >= / <= ..." error message the router returns when this
	// param's Min/Max bound is violated. Used to redirect the model to a
	// different tool that can serve the value it actually wants (e.g.
	// set_timer's overflow pointing at set_reminder) so it can self-correct
	// conversationally instead of dead-ending on a bare rejection.
	OutOfRangeHint string
}

// safeFileNamePattern is the advertised (and enforced) filename shape for
// SafeName params. The leading [A-Za-z0-9] bans hidden/dot files; the
// class bans path separators, spaces, quotes, and control characters.
const safeFileNamePattern = `^[A-Za-z0-9][A-Za-z0-9._-]*$`

var safeFileNameRe = regexp.MustCompile(safeFileNamePattern)

// isSafeFileName reports whether s is a safe filename slug: pattern-
// conformant and free of any ".." sequence.
func isSafeFileName(s string) bool {
	return !strings.Contains(s, "..") && safeFileNameRe.MatchString(s)
}

// HandlerFunc executes a validated, re-authorized tool call. args has
// already passed schema validation (types coerced: integer params are
// int, string_array params are []string).
type HandlerFunc func(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError)

// Definition declares one tool: its advertised schema and its handler.
type Definition struct {
	Name        string
	Description string
	Params      []ParamSpec
	// SideEffecting tools require an idempotencyKey and get an IDEMP#
	// conditional-put guard before execution.
	SideEffecting bool
	Handler       HandlerFunc
}

// ReauthorizeFunc re-checks, at call time, that the user behind a still-
// valid JWT is itself still valid: status active, and owner-or-allowlisted
// (shared spec access rule). Supplied by the web layer (internal/auth
// owns the actual check); a non-nil error denies the call.
type ReauthorizeFunc func(ctx context.Context, userID string) error

// QueryAPI is the one raw DynamoDB operation this package needs beyond
// the typed store helpers: a single-partition Query (recall_note). A
// *dynamodb.Client satisfies it; tests inject a fake.
type QueryAPI interface {
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// SQSAPI is the SendMessage subset of the SQS client (send_email).
type SQSAPI interface {
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// SchedulerAPI is the CreateSchedule subset of the EventBridge Scheduler
// client (set_timer / set_reminder).
type SchedulerAPI interface {
	CreateSchedule(ctx context.Context, params *scheduler.CreateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error)
}

// IoTDataAPI is the Publish subset of the IoT data-plane client
// (device_control).
type IoTDataAPI interface {
	Publish(ctx context.Context, params *iotdataplane.PublishInput, optFns ...func(*iotdataplane.Options)) (*iotdataplane.PublishOutput, error)
}

// Deps carries everything the tool handlers need. The web function wires
// real AWS clients; tests wire fakes against the narrow interfaces.
type Deps struct {
	Store     *store.Store // ConditionalPut (IDEMP#, LOG#, NOTE#) + GetItem (DEVICE# ownership check)
	DDB       QueryAPI     // recall_note single-partition Query
	TableName string       // env TABLE_NAME
	Log       *slog.Logger

	SQS           SQSAPI // send_email enqueue
	EmailQueueURL string // env EMAIL_QUEUE_URL
	OwnerEmail    string // env OWNER_EMAIL — default (and only unconfirmed) email recipient

	Scheduler        SchedulerAPI // set_timer / set_reminder
	SchedulerGroup   string       // env SCHEDULER_GROUP
	SchedulerRoleARN string       // env SCHEDULER_ROLE_ARN

	IoT IoTDataAPI // device_control publish

	// Deliverables backs the deliverable_create/zip/deliver tools (M9);
	// nil → those tools report not_configured (interface in deliverable.go).
	Deliverables DeliverableService

	// Memory backs the M10 memory_search/memory_write/entity_get/
	// plan_upsert/forget tools (interface in memory.go, implemented by
	// internal/memory); nil → those tools report not_configured.
	Memory MemoryService

	HTTPClient *http.Client // get_weather / web_lookup; defaulted by NewRegistry

	Reauthorize ReauthorizeFunc

	// Now is the clock; defaulted to time.Now by NewRegistry (tests
	// override for deterministic schedules/IDs).
	Now func() time.Time
}

// Registry holds the tool catalog and runs the invocation pipeline.
type Registry struct {
	deps  *Deps
	tools map[string]*Definition
	order []string // registration order, for a stable Manifest
}

// NewRegistry validates the universally-required dependencies, applies
// defaults, and registers the full M2 tool catalog. Tool-specific AWS
// clients may be nil in local dev — the affected tool then fails with
// not_configured at invoke time while the rest of the catalog works.
func NewRegistry(deps *Deps) (*Registry, error) {
	if deps == nil {
		return nil, errors.New("tools: deps are required")
	}
	if deps.Store == nil {
		return nil, errors.New("tools: deps.Store is required")
	}
	if deps.Log == nil {
		return nil, errors.New("tools: deps.Log is required")
	}
	if deps.Reauthorize == nil {
		return nil, errors.New("tools: deps.Reauthorize is required (per-call re-authorization is mandatory)")
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}

	r := &Registry{deps: deps, tools: make(map[string]*Definition)}
	for _, def := range []*Definition{
		sendEmailDefinition(),
		setTimerDefinition(),
		setReminderDefinition(),
		deviceControlDefinition(),
		getWeatherDefinition(),
		webLookupDefinition(),
		rememberNoteDefinition(),
		recallNoteDefinition(),
		deliverableCreateDefinition(),
		deliverableZipDefinition(),
		deliverableDeliverDefinition(),
		fileListDefinition(),
		fileReadDefinition(),
		fileCreateDefinition(),
		memorySearchDefinition(),
		memoryWriteDefinition(),
		entityGetDefinition(),
		planUpsertDefinition(),
		forgetDefinition(),
		webResearchDefinition(),
	} {
		if err := r.register(def); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) register(def *Definition) error {
	if def.Name == "" || def.Handler == nil {
		return errors.New("tools: definition requires a name and a handler")
	}
	if _, dup := r.tools[def.Name]; dup {
		return fmt.Errorf("tools: duplicate tool %q", def.Name)
	}
	r.tools[def.Name] = def
	r.order = append(r.order, def.Name)
	return nil
}

// Manifest renders the catalog as OpenAI Realtime function-tool
// definitions — the `tools` array the broker binds into every session
// config, and the exact schema Invoke later enforces.
func (r *Registry) Manifest() []map[string]any {
	out := make([]map[string]any, 0, len(r.order))
	for _, name := range r.order {
		def := r.tools[name]
		props := make(map[string]any, len(def.Params))
		var required []string
		for _, p := range def.Params {
			if p.Unadvertised {
				continue
			}
			props[p.Name] = p.jsonSchema()
			if p.Required {
				required = append(required, p.Name)
			}
		}
		params := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			sort.Strings(required)
			params["required"] = required
		}
		out = append(out, map[string]any{
			"type":        "function",
			"name":        def.Name,
			"description": def.Description,
			"parameters":  params,
		})
	}
	return out
}

func (p ParamSpec) jsonSchema() map[string]any {
	s := map[string]any{"description": p.Description}
	switch p.Type {
	case "string_array":
		s["type"] = "array"
		s["items"] = map[string]any{"type": "string"}
	default:
		s["type"] = p.Type
	}
	if len(p.Enum) > 0 {
		s["enum"] = p.Enum
	}
	if p.Type == "string" {
		if p.MinLen > 0 {
			s["minLength"] = p.MinLen
		}
		if p.MaxLen > 0 {
			s["maxLength"] = p.MaxLen
		}
		if p.SafeName {
			s["pattern"] = safeFileNamePattern
		}
	}
	if p.Min != nil {
		s["minimum"] = *p.Min
	}
	if p.Max != nil {
		s["maximum"] = *p.Max
	}
	return s
}

// Invoke runs the full pipeline for one tool call and always returns a
// non-nil Result whose TxID (and, on failure, Error.TxID) carries the
// transaction correlation id.
func (r *Registry) Invoke(ctx context.Context, inv Invocation) (res *Result) {
	start := r.deps.Now()

	// Resolve the transaction id: reuse the HTTP-layer forwarded txId when
	// present, else mint a fresh UUID v4 so every tool call is traceable.
	txID := inv.TxID
	if txID == "" {
		txID = observ.NewTxnID()
	}
	res = &Result{Tool: inv.Tool, CallID: inv.CallID, TxID: txID}

	l := observ.WithTxn(r.deps.Log.With(
		slog.String("tool", inv.Tool),
		slog.String("userId", inv.UserID),
		slog.String("sessionId", inv.SessionID),
		slog.String("surface", inv.Surface),
		slog.String("callId", inv.CallID),
	), txID)
	l.Info("tools: invoke start")

	// Single egress point: stamps Error.TxID, writes the audit line, emits
	// the EMF metric, and logs "invoke done" with outcome + latency — for
	// every return path below.
	var def *Definition
	defer func() { r.finish(ctx, l, def, inv, res, start) }()

	d, ok := r.tools[inv.Tool]
	if !ok {
		res.Error = toolErrf(CodeUnknownTool, "unknown tool %q", inv.Tool)
		return res
	}
	def = d
	if inv.UserID == "" {
		res.Error = toolErrf(CodeForbidden, "missing authenticated user context")
		return res
	}

	args, verr := validateArgs(def, inv.Args)
	if verr != nil {
		res.Error = verr
		return res
	}

	// Per-call re-authorization: the JWT proved who the caller was at
	// mint time; this proves they are still active and still allowed.
	if err := r.deps.Reauthorize(ctx, inv.UserID); err != nil {
		l.Warn("tools: re-authorization denied", slog.String("error", err.Error()))
		res.Error = toolErrf(CodeForbidden, "user is not authorized to invoke tools")
		return res
	}

	// Idempotency guard for side-effecting tools: mark before execute so
	// a duplicate delivery can never repeat the side effect (at-most-once).
	if def.SideEffecting {
		if inv.IdempotencyKey == "" {
			res.Error = toolErrf(CodeInvalidArgs, "idempotencyKey is required for %s", def.Name)
			return res
		}
		err := r.deps.Store.ConditionalPut(ctx,
			"IDEMP#"+inv.UserID+"#"+inv.IdempotencyKey, "IDEMP",
			map[string]any{"tool": inv.Tool, "userId": inv.UserID, "callId": inv.CallID},
			r.deps.Now().Add(idempotencyTTL).Unix())
		if errors.Is(err, store.ErrAlreadyExists) {
			res.OK = true
			res.Duplicate = true
			res.Output = map[string]any{"status": "duplicate", "message": "this call was already processed"}
			return res
		}
		if err != nil {
			l.Error("tools: idempotency put failed", slog.String("error", err.Error()))
			res.Error = toolErrf(CodeUpstreamError, "idempotency check failed")
			return res
		}
	}

	output, terr := def.Handler(ctx, r.deps, inv, args)
	if terr != nil {
		res.Error = terr
	} else {
		res.OK = true
		res.Output = output
	}
	return res
}

// finish stamps the transaction id onto any error, emits the audit LOG#
// line (best effort) and the EMF metric, and logs the verbose "invoke done"
// line with outcome + latency for every invocation, success or failure.
func (r *Registry) finish(ctx context.Context, l *slog.Logger, def *Definition, inv Invocation, res *Result, start time.Time) {
	outcome := "ok"
	switch {
	case res.Duplicate:
		outcome = "duplicate"
	case !res.OK:
		outcome = "error"
	}

	// Every error the tool router returns carries the txId so the client's
	// canonical envelope {code, message, txId} — and the model's
	// function_call_output — pin the exact invocation.
	if res.Error != nil && res.Error.TxID == "" {
		res.Error.TxID = res.TxID
	}

	if inv.UserID != "" {
		r.writeAudit(ctx, l, inv, res, outcome)
	}

	surface := inv.Surface
	if surface == "" {
		surface = "unknown"
	}
	observ.EmitMetric("LiveNinja/Tools", "ToolInvocations", 1, "Count", map[string]string{
		"Tool":    inv.Tool,
		"Outcome": outcome,
		"Surface": surface,
	})

	latencyMs := time.Since(start).Milliseconds()
	if res.OK {
		l.Info("tools: invoke done",
			slog.String("outcome", outcome),
			slog.Int64("latencyMs", latencyMs))
	} else {
		l.Warn("tools: invoke done",
			slog.String("outcome", outcome),
			slog.String("code", res.Error.Code),
			slog.String("message", res.Error.Message),
			slog.Int64("latencyMs", latencyMs))
	}
	_ = def
}

// writeAudit persists a role=tool transcript line at
// USER#<uid> / LOG#<sessionId>#<seq %06d> (shared-spec LOG shape, 90d
// TTL). The tool router has no transcript sequence counter of its own,
// so seq derives from the invocation's millisecond-of-session clock with
// conditional-put collision retry; audit failures are logged, never
// surfaced to the caller.
func (r *Registry) writeAudit(ctx context.Context, l *slog.Logger, inv Invocation, res *Result, outcome string) {
	sessionID := inv.SessionID
	if sessionID == "" {
		sessionID = "none"
	}

	argsJSON, _ := json.Marshal(inv.Args)
	text := fmt.Sprintf("tool=%s outcome=%s callId=%s args=%s", inv.Tool, outcome, inv.CallID, argsJSON)
	if !res.OK && res.Error != nil {
		text += " error=" + res.Error.Code
	}
	if len(text) > maxAuditText {
		text = text[:maxAuditText]
	}

	// Output snippet (capped like args) so History can render the same
	// tool card the live transcript shows without replaying the call.
	outputSnippet := ""
	if res.OK && len(res.Output) > 0 {
		if outJSON, err := json.Marshal(res.Output); err == nil {
			outputSnippet = string(outJSON)
			if len(outputSnippet) > maxAuditText {
				outputSnippet = outputSnippet[:maxAuditText]
			}
		}
	}

	now := r.deps.Now().UTC()
	seq := int(now.UnixMilli() % 1_000_000)
	for attempt := 0; attempt < 3; attempt++ {
		sk := fmt.Sprintf("LOG#%s#%06d", sessionID, (seq+attempt)%1_000_000)
		item := map[string]any{
			"role":    "tool",
			"text":    text,
			"surface": inv.Surface,
			"engine":  auditEngine,
			"ts":      now.Format(time.RFC3339Nano),
		}
		if outputSnippet != "" {
			item["output"] = outputSnippet
		}
		err := r.deps.Store.ConditionalPut(ctx, "USER#"+inv.UserID, sk, item, now.Add(auditTTL).Unix())
		if err == nil {
			return
		}
		if !errors.Is(err, store.ErrAlreadyExists) {
			l.Error("tools: audit write failed", slog.String("error", err.Error()))
			return
		}
	}
	l.Error("tools: audit write failed after seq-collision retries")
}

// validateArgs enforces a Definition's parameter schema over raw
// JSON-decoded arguments, returning a cleaned map with coerced types.
// Unknown arguments are rejected outright — the model only ever sees the
// advertised schema, so anything extra is malformed or adversarial.
func validateArgs(def *Definition, raw map[string]any) (map[string]any, *ToolError) {
	specs := make(map[string]ParamSpec, len(def.Params))
	for _, p := range def.Params {
		specs[p.Name] = p
	}

	for name := range raw {
		if _, ok := specs[name]; !ok {
			return nil, toolErrf(CodeInvalidArgs, "unexpected argument %q for tool %s", name, def.Name)
		}
	}

	clean := make(map[string]any, len(raw))
	for _, p := range def.Params {
		v, present := raw[p.Name]
		if !present || v == nil {
			if p.Required {
				return nil, toolErrf(CodeInvalidArgs, "missing required argument %q", p.Name)
			}
			continue
		}
		cv, err := p.coerce(v)
		if err != nil {
			return nil, err
		}
		clean[p.Name] = cv
	}
	return clean, nil
}

func (p ParamSpec) coerce(v any) (any, *ToolError) {
	switch p.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be a string", p.Name)
		}
		if p.Required && s == "" {
			return nil, toolErrf(CodeInvalidArgs, "argument %q must not be empty", p.Name)
		}
		// Length bounds are measured in runes (utf8.RuneCountInString), not
		// bytes: a byte count would cap multi-byte content earlier than a
		// user typing in, say, Japanese or with emoji would expect.
		n := utf8.RuneCountInString(s)
		if p.MinLen > 0 && n < p.MinLen {
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be at least %d characters", p.Name, p.MinLen)
		}
		if p.MaxLen > 0 && n > p.MaxLen {
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be at most %d characters", p.Name, p.MaxLen)
		}
		if p.SafeName && !isSafeFileName(s) {
			return nil, toolErrf(CodeInvalidArgs,
				"argument %q must be a plain filename (letters, digits, dot, dash, underscore; "+
					"starting with a letter or digit; no path separators, no '..')", p.Name)
		}
		if len(p.Enum) > 0 {
			for _, e := range p.Enum {
				if s == e {
					return s, nil
				}
			}
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be one of %v", p.Name, p.Enum)
		}
		return s, nil

	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be a boolean", p.Name)
		}
		return b, nil

	case "integer":
		f, ok := v.(float64)
		if !ok {
			// json.Number or already-int callers.
			switch n := v.(type) {
			case int:
				f, ok = float64(n), true
			case int64:
				f, ok = float64(n), true
			case json.Number:
				parsed, err := n.Float64()
				if err == nil {
					f, ok = parsed, true
				}
			}
		}
		if !ok {
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be an integer", p.Name)
		}
		if f != math.Trunc(f) {
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be a whole number", p.Name)
		}
		if err := p.checkRange(f); err != nil {
			return nil, err
		}
		return int(f), nil

	case "number":
		f, ok := v.(float64)
		if !ok {
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be a number", p.Name)
		}
		if err := p.checkRange(f); err != nil {
			return nil, err
		}
		return f, nil

	case "string_array":
		items, ok := v.([]any)
		if !ok {
			if ss, already := v.([]string); already {
				return ss, nil
			}
			return nil, toolErrf(CodeInvalidArgs, "argument %q must be an array of strings", p.Name)
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			s, isStr := it.(string)
			if !isStr {
				return nil, toolErrf(CodeInvalidArgs, "argument %q must contain only strings", p.Name)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, toolErrf(CodeInvalidArgs, "argument %q has unsupported schema type %q", p.Name, p.Type)
}

func (p ParamSpec) checkRange(f float64) *ToolError {
	if p.Min != nil && f < *p.Min {
		msg := fmt.Sprintf("argument %q must be >= %v", p.Name, *p.Min)
		if p.OutOfRangeHint != "" {
			msg += " " + p.OutOfRangeHint
		}
		return toolErrf(CodeInvalidArgs, "%s", msg)
	}
	if p.Max != nil && f > *p.Max {
		msg := fmt.Sprintf("argument %q must be <= %v", p.Name, *p.Max)
		if p.OutOfRangeHint != "" {
			msg += " " + p.OutOfRangeHint
		}
		return toolErrf(CodeInvalidArgs, "%s", msg)
	}
	return nil
}

func floatPtr(f float64) *float64 { return &f }
