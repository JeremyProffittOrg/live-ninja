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
//     IDEMP#<userId>#<key> (24h TTL), claimed BEFORE the handler runs so
//     duplicate deliveries are a no-op, and released again when the handler
//     failed before any side effect (idempotencyReleasable);
//  4. real execution (no stubs — SES/SQS email, EventBridge Scheduler,
//     IoT publish, Open-Meteo, Wikipedia, DynamoDB notes);
//  5. an audit LOG# write into the caller's transcript partition, a
//     ToolInvocations EMF metric, and — for error outcomes only — an
//     enqueue onto the M17 RCA queue (Deps.RCA; nil disables it).
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
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
	// rcaEnvelopeVersion is ToolFailure's schema version (M17).
	rcaEnvelopeVersion = 1
	// rcaSourceToolRouter marks a ToolFailure produced by this package (as
	// opposed to the phase-2 web-client breadcrumb, which rides the same
	// queue at the same version with a different Source).
	rcaSourceToolRouter = "tool-router"
	// unknownToolSentinel replaces the client-supplied tool name on the
	// CodeUnknownTool path — see enqueueRCA for why that string must never
	// reach a DynamoDB key.
	unknownToolSentinel = "unknown_tool"
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

// ToolFailure is the SQS message body the RCA pipeline consumes (M17): one
// failed invocation, flattened into a self-contained wire record. It is
// produced by finish (the single egress) and consumed by cmd/rca-analyzer
// via internal/rca — one struct, so producer and consumer can never drift.
//
// Everything here is server-derived. Args is a JSON *string* rather than a
// map so a pathological argument can be capped at the producer instead of
// bloating an SQS message (256 KB hard limit) or the RCA prompt.
type ToolFailure struct {
	// V is the envelope schema version; 1 is the tool-router shape. The
	// phase-2 client breadcrumb (plan.md WS-2 M17 last task) will ride the
	// same queue at V=1 with Source="web-client".
	V      int    `json:"v"`
	Source string `json:"source"` // "tool-router"

	Tool string `json:"tool"`
	// RequestedTool is the raw, client-supplied tool name. It is only ever
	// non-empty for ErrorCode == CodeUnknownTool, where Tool is forced to the
	// "unknown_tool" sentinel: the client controls that string, and letting it
	// key a DynamoDB partition would give unbounded fan-out plus '#'
	// injection into the key.
	RequestedTool string `json:"requestedTool,omitempty"`

	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
	ArgsJSON     string `json:"args"`

	CallID    string `json:"callId,omitempty"`
	TxID      string `json:"txId,omitempty"`
	UserID    string `json:"userId"`
	SessionID string `json:"sessionId,omitempty"`
	Surface   string `json:"surface,omitempty"`
	Role      string `json:"role,omitempty"`

	// ConvID and Engine are reserved and ALWAYS EMPTY from the tool router.
	// The conversation id is minted by cmd/topics-extract at session end (long
	// after a mid-session failure), and the session's voice engine never
	// reaches tools.Deps — the analyzer recovers the engine from the
	// transcript window instead. Both exist for the phase-2 client breadcrumb.
	ConvID string `json:"convId,omitempty"`
	Engine string `json:"engine,omitempty"`

	// OccurredAt is the invocation's completion time, RFC3339Nano UTC.
	OccurredAt string `json:"occurredAt"`
}

// maxFailureArgsJSON caps the serialized args carried on a ToolFailure. It is
// deliberately larger than maxAuditText (the audit line must stay small) and
// far below both the SQS 256 KB body limit and the RCA prompt budget.
const maxFailureArgsJSON = 2048

// maxFailureRequestedTool caps RequestedTool, the one ToolFailure field that is
// raw, unvalidated client input of unbounded length: POST /api/v1/tools/invoke
// accepts any non-empty `tool` string, and on the unknown_tool path that string
// is carried verbatim so the RCA can say what the model invented.
//
// Unbounded, it reaches three places that must not take it: the SQS body (256 KB
// hard limit — over it the enqueue just fails), the RCA# DynamoDB item (400 KB
// item limit — over it every PutRCA for that failure fails after the Opus call
// has already been paid for), and the Opus prompt itself. internal/rca clamps
// its own render too, because it must be safe against any producer; this is the
// producer half, so nothing downstream ever stores the oversized string at all.
// 256 is ~4x the longest plausible real tool name and keeps the signal intact.
const maxFailureRequestedTool = 256

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
	// schema (Manifest / CatalogManifest) while validateArgs
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
	// DeviceLocal marks a tool whose work happens on the user's device (stopping
	// the microphone, recycling a realtime session) and which the client is
	// expected to intercept before it reaches this router. It still appears in the
	// manifest — that is what tells the model the capability exists — but reaching
	// the server Handler means a surface that cannot perform it called it anyway.
	DeviceLocal bool
	Handler     HandlerFunc
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

// IdempotencyReleaseAPI is the one raw DynamoDB write this package needs
// beyond Store.ConditionalPut: deleting an IDEMP# marker again when the
// handler it was claimed for failed *before* it could touch anything
// external (WS-3 3.2, see idempotencyReleasable). A *dynamodb.Client
// satisfies it — which is exactly what Deps.DDB already carries in
// production — so NewRegistry defaults the field from DDB and no caller has
// to wire it; tests inject the same fake that backs Deps.Store.
type IdempotencyReleaseAPI interface {
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// FailureEnqueuer is the RCA pipeline's ingress (M17): finish hands every
// error-outcome invocation to it, fire-and-forget. Errors are logged and
// never surfaced onto the request path. A nil Deps.RCA disables the hook
// entirely, so every registry built without one — every existing test, every
// local smoke run — behaves byte-identically to pre-M17.
type FailureEnqueuer interface {
	EnqueueToolFailure(ctx context.Context, f ToolFailure) error
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

	// Profile loads the caller's Base Knowledge profile (M15) so
	// profile-aware tools can take their defaults from it: get_weather's
	// location and units, the scheduler's timezone. Defaulted by NewRegistry
	// to a projected single-item read through Store; tests inject fakes.
	//
	// It is a loader rather than a value because Deps is process-wide while a
	// profile is per-user: the invocation's verified UserID picks the row, so
	// one user's profile can never leak into another's tool call.
	Profile func(ctx context.Context, userID string) store.Profile

	// RCA enqueues failed invocations onto the RCA queue (M17,
	// internal/rca.SQSEnqueuer). nil — the default, and the case for every
	// registry built without an RCA_QUEUE_URL — means the hook is inert:
	// no send, no log line, no behaviour change.
	RCA FailureEnqueuer

	// IdempotencyRelease deletes an IDEMP# marker whose handler failed before
	// any side effect could have happened (WS-3 3.2). Defaulted by NewRegistry
	// from DDB when that client also supports DeleteItem — the production
	// *dynamodb.Client does, so this needs no wiring at the call site. When it
	// is nil the release is skipped and logged: the marker then simply lives
	// out its 24h TTL, which is the pre-fix behaviour, never a silent success.
	IdempotencyRelease IdempotencyReleaseAPI

	Reauthorize ReauthorizeFunc

	// Now is the clock; defaulted to time.Now by NewRegistry (tests
	// override for deterministic schedules/IDs).
	Now func() time.Time
}

// profileFor is the nil-safe accessor every profile-aware handler uses. A
// registry built without a Profile loader (or for an invocation with no
// user, e.g. a local smoke test) sees the zero profile and falls back to the
// pre-M15 behaviour rather than failing the call.
func (d *Deps) profileFor(ctx context.Context, userID string) store.Profile {
	if d == nil || d.Profile == nil || userID == "" {
		return store.Profile{}
	}
	return d.Profile(ctx, userID)
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
	// The idempotency releaser is the same DynamoDB client the recall_note
	// Query already runs on; DDB is declared as the narrow QueryAPI, so the
	// DeleteItem half is picked up by assertion rather than by widening
	// QueryAPI (which every existing caller and fake would then have to
	// implement). Nothing is required: a client without DeleteItem leaves the
	// field nil and releases degrade to a Warn.
	if deps.IdempotencyRelease == nil {
		if releaser, ok := deps.DDB.(IdempotencyReleaseAPI); ok {
			deps.IdempotencyRelease = releaser
		}
	}
	if deps.Profile == nil {
		st := deps.Store
		deps.Profile = func(ctx context.Context, userID string) store.Profile {
			p, err := st.GetProfile(ctx, userID)
			if err != nil {
				return store.Profile{}
			}
			return p
		}
	}

	r := &Registry{deps: deps, tools: make(map[string]*Definition)}
	for _, def := range definitions() {
		if err := r.register(def); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// definitions builds the full tool catalog in its canonical order. The
// Definition constructors take no dependencies, so this slice can be
// rendered (CatalogManifest) without a live Deps; NewRegistry registers
// exactly this set.
func definitions() []*Definition {
	return []*Definition{
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
		profileSuggestDefinition(),
		// Device-local session controls: declared here so the manifest advertises
		// them, executed by the client (see devicesession.go).
		stopListeningDefinition(),
		startNewConversationDefinition(),
	}
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

// CatalogManifest renders the full tool catalog as OpenAI Realtime
// function-tool definitions — the `tools` array the realtime broker binds
// into every session config (internal/realtime derives its manifest from
// this function), and the exact schema Invoke later enforces. It needs no
// live Deps: the Definition constructors are dependency-free.
func CatalogManifest() []map[string]any {
	return renderManifest(definitions())
}

// Manifest renders this registry's catalog as OpenAI Realtime
// function-tool definitions — the same rendering as CatalogManifest
// (both go through renderManifest), scoped to what this Registry has
// registered.
func (r *Registry) Manifest() []map[string]any {
	defs := make([]*Definition, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name])
	}
	return renderManifest(defs)
}

// renderManifest is the single manifest renderer behind both
// CatalogManifest and (*Registry).Manifest, so the advertised schema can
// never diverge between them. Unadvertised params are excluded from the
// rendered schema (they remain validated and enforced by Invoke).
func renderManifest(defs []*Definition) []map[string]any {
	out := make([]map[string]any, 0, len(defs))
	for _, def := range defs {
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
		params := map[string]any{"type": "object"}
		// Omit "properties" entirely for a parameterless tool rather than emitting
		// an empty object. The Gemini Go SDK drops an empty property map when it
		// marshals the same schema into a minted token's constraints, so emitting
		// it here makes the raw wire setup frame and the SDK-typed constraints
		// disagree about a tool the model must see identically through both paths.
		// Fixed at the source (here) rather than in the Gemini sanitizer, because
		// gemini_mint_test.go asserts the sanitized copy stays exactly equal to the
		// manifest — divergence is drift to fix upstream, not to paper over
		// downstream. Surfaced by the catalog's first zero-parameter tools
		// (stop_listening, start_new_conversation); an object schema with no
		// declared properties means the same thing either way.
		if len(props) > 0 {
			params["properties"] = props
		}
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
	defer func() { r.finish(ctx, l, inv, res, start) }()

	def, ok := r.tools[inv.Tool]
	if !ok {
		res.Error = toolErrf(CodeUnknownTool, "unknown tool %q", inv.Tool)
		return res
	}
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

	// Idempotency guard for side-effecting tools: claim the IDEMP# marker
	// before execute so a duplicate delivery can never repeat the side effect
	// (at-most-once). claimedIdempotency records that THIS invocation owns the
	// marker, which is what lets the handler-failure path below release it —
	// claim-before-execute on its own is only half of an at-most-once
	// protocol, see idempotencyReleasable.
	claimedIdempotency := false
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
		claimedIdempotency = true
	}

	output, terr := def.Handler(ctx, r.deps, inv, args)
	if terr != nil {
		res.Error = terr
		// WS-3 3.2 — the other half of at-most-once. The marker was written
		// before the handler ran, so without this a handler that failed
		// *before* doing anything leaves a marker that makes the very next
		// re-delivery of the same call answer ok:true / duplicate:true
		// ("this call was already processed"). The side effect would never
		// have happened, yet the model — and the human it is talking to —
		// would be told it did. Releasing the claim for those codes turns an
		// undetectable lost side effect back into a retryable error.
		if claimedIdempotency && idempotencyReleasable(terr.Code) {
			r.releaseIdempotency(ctx, l, inv)
		}
	} else {
		res.OK = true
		res.Output = output
	}
	return res
}

// idempotencyReleasable reports whether a handler's error code proves the
// tool's side effect had not yet been attempted, so the IDEMP# marker claimed
// for it can be deleted and an identical re-delivery allowed to run for real.
//
// Deliberately an ALLOWLIST rather than "anything but upstream_error": a new
// error code, or one whose position relative to the side effect is unclear,
// keeps the marker. That is the conservative direction — a held marker costs
// at most one lost retry of a call the caller can re-issue under a new
// callId, while a wrongly-released one can duplicate a real-world side effect
// (a second email, a second MQTT publish to the lamp).
//
// Every code below is returned by a handler *before* it reaches its external
// dependency: argument shapes it validates itself (invalid_args, e.g. an
// unparseable datetime or an ungeocodable place), the send allow-list check
// (forbidden), ownership/lookup misses (not_found), a name/key clash
// (already_exists), a nil dependency (not_configured), and a confirmation
// prompt (confirmation_required).
//
// CodeUpstreamError is the one code excluded on purpose. It means the external
// system WAS called and its outcome is unknown — SES may have accepted the
// send, IoT may have published — so releasing the key would licence a
// duplicate side effect. Holding it is what at-most-once means, it is bounded
// by the marker's 24h TTL, and it does not wedge the conversation: a model
// retrying after an error emits a fresh function_call with a fresh callId, so
// a fresh idempotency key.
func idempotencyReleasable(code string) bool {
	switch code {
	case CodeInvalidArgs, CodeForbidden, CodeNotFound,
		CodeAlreadyExists, CodeNotConfigured, CodeConfirmationRequired:
		return true
	default:
		return false
	}
}

// releaseIdempotency deletes the IDEMP# marker this invocation claimed. It is
// best-effort by design: the caller already has a real ToolError to return,
// and a failed release only costs the retryability the marker's TTL restores
// 24h later — so a failure here is logged and never allowed to replace the
// handler's error with a less useful one.
//
// Only ever called for a marker this invocation itself wrote (claimedIdempotency),
// so it can never delete another caller's claim. One narrow window remains and
// is accepted: a truly concurrent second delivery that lost the claim race was
// already answered "duplicate" while the first handler was still running, and
// releasing afterwards cannot un-answer it. Closing that would need a
// completion state on the marker and a read on the duplicate branch — an extra
// round trip on every side-effecting call to cover simultaneous double
// delivery of the same callId, which the surfaces do not do (each retry is a
// sequential re-POST).
func (r *Registry) releaseIdempotency(ctx context.Context, l *slog.Logger, inv Invocation) {
	if r.deps.IdempotencyRelease == nil {
		l.Warn("tools: idempotency release skipped (no DeleteItem-capable DynamoDB client wired)",
			slog.String("idempotencyKey", inv.IdempotencyKey))
		return
	}
	_, err := r.deps.IdempotencyRelease.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(r.deps.TableName),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "IDEMP#" + inv.UserID + "#" + inv.IdempotencyKey},
			"sk": &types.AttributeValueMemberS{Value: "IDEMP"},
		},
	})
	if err != nil {
		l.Warn("tools: idempotency release failed; marker holds until its TTL",
			slog.String("idempotencyKey", inv.IdempotencyKey),
			slog.String("error", err.Error()))
		return
	}
	l.Info("tools: idempotency claim released (handler failed before any side effect)",
		slog.String("idempotencyKey", inv.IdempotencyKey))
}

// finish stamps the transaction id onto any error, emits the audit LOG#
// line (best effort) and the EMF metric, and logs the verbose "invoke done"
// line with outcome + latency for every invocation, success or failure.
func (r *Registry) finish(ctx context.Context, l *slog.Logger, inv Invocation, res *Result, start time.Time) {
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
	// The Tool dimension is sentinelised on the unknown-tool path for the same
	// reason enqueueRCA keys the RCA partition with a sentinel: inv.Tool is raw
	// client input there (Invoke rejects unknown tools before any validation), and
	// a CloudWatch dimension VALUE is billed per distinct combination — one custom
	// metric per unique name, indefinitely. A caller looping over random tool
	// names would mint unbounded custom metrics off an authenticated 404. The
	// signal is not lost: outcome=error plus the "invoke done" log line (which is
	// per-request, retention-bounded, and free of cardinality cost) still carries
	// the exact name.
	metricTool := inv.Tool
	if res.Error != nil && res.Error.Code == CodeUnknownTool {
		metricTool = unknownToolSentinel
	}
	observ.EmitMetric("LiveNinja/Tools", "ToolInvocations", 1, "Count", map[string]string{
		"Tool":    metricTool,
		"Outcome": outcome,
		"Surface": surface,
	})

	latencyMs := time.Since(start).Milliseconds()
	switch {
	case res.OK:
		l.Info("tools: invoke done",
			slog.String("outcome", outcome),
			slog.Int64("latencyMs", latencyMs))
	case res.Error != nil:
		l.Warn("tools: invoke done",
			slog.String("outcome", outcome),
			slog.String("code", res.Error.Code),
			slog.String("message", res.Error.Message),
			slog.Int64("latencyMs", latencyMs))
	default:
		// Neither OK nor carrying an error: the only way to reach this is a
		// handler that panicked, unwinding through this deferred call before
		// res.Error was set. Dereferencing res.Error here would replace the
		// original panic with a nil-pointer one inside a defer and throw away the
		// stack that says which handler broke.
		l.Error("tools: invoke done with no result and no error (handler panicked)",
			slog.String("outcome", outcome),
			slog.Int64("latencyMs", latencyMs))
	}

	// M17 (last, deliberately): hand error outcomes to the RCA pipeline only
	// after the audit row, the EMF metric and the "invoke done" line are all
	// already emitted, so nothing this does can delay, reorder or lose the
	// observability record that existed before M17.
	r.enqueueRCA(ctx, l, inv, res, outcome)
}

// enqueueRCA hands one failed invocation to the RCA pipeline (M17). It is the
// only new work M17 adds to the request path and it is entirely inert unless
// deps.RCA was wired: a nil enqueuer returns immediately, so every registry
// built without one — every test in this package, every local smoke run — is
// byte-identical to pre-M17 behaviour.
//
// ctx is the caller's request context, passed for signature honesty even
// though the shipped enqueuer (internal/rca.SQSEnqueuer) derives its own
// bounded context: under the Lambda Web Adapter the Fiber/fasthttp context is
// recycled the instant the handler returns, so the send cannot borrow it.
//
// Errors are logged at Warn and never raised: an RCA is diagnostics, and a
// diagnostics failure must never turn a recoverable tool error into a
// different one for the model to react to.
func (r *Registry) enqueueRCA(ctx context.Context, l *slog.Logger, inv Invocation, res *Result, outcome string) {
	if r.deps.RCA == nil || outcome != "error" || res.Error == nil {
		return
	}
	if inv.UserID == "" {
		// No verified user means no transcript partition to read and no
		// profile to load — there is nothing for the analyzer to reason over.
		// It also coincides exactly with the missing-user-context CodeForbidden
		// branch in Invoke, which the allowlist below excludes on its own
		// merits.
		return
	}
	if !rcaWorthAnalyzing(res.Error.Code) {
		return
	}

	tool, requested := inv.Tool, ""
	if res.Error.Code == CodeUnknownTool {
		// inv.Tool is raw client input on this path (Invoke rejects unknown
		// tools before any validation), so it must never key a DynamoDB
		// partition: an unbounded set of tool names would fan the RCA table
		// out arbitrarily, and a '#' in the name would inject into the key.
		// The signal — "the model invented a tool" — is preserved by the
		// sentinel plus RequestedTool, clamped (see maxFailureRequestedTool: the
		// name is unbounded client input and must not reach the SQS body, the
		// RCA# item or the Opus prompt at full length).
		tool, requested = unknownToolSentinel, clampRunes(inv.Tool, maxFailureRequestedTool)
	}

	// "{}" rather than json.Marshal's "null" for an absent args map: the
	// analyzer treats args as a JSON object (it renders its top-level keys into
	// the dedupe signature), so the empty object is the honest zero value.
	argsJSON := "{}"
	if len(inv.Args) > 0 {
		if b, err := json.Marshal(inv.Args); err == nil {
			argsJSON = string(b)
			if len(argsJSON) > maxFailureArgsJSON {
				argsJSON = argsJSON[:maxFailureArgsJSON]
			}
		}
	}

	f := ToolFailure{
		V: rcaEnvelopeVersion, Source: rcaSourceToolRouter,
		Tool: tool, RequestedTool: requested,
		ErrorCode: res.Error.Code, ErrorMessage: res.Error.Message, ArgsJSON: argsJSON,
		CallID: inv.CallID, TxID: res.TxID, UserID: inv.UserID,
		SessionID: inv.SessionID, Surface: inv.Surface, Role: inv.Role,
		OccurredAt: r.deps.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := r.deps.RCA.EnqueueToolFailure(ctx, f); err != nil {
		l.Warn("tools: rca enqueue failed", slog.String("error", err.Error()))
		return
	}
	observ.EmitMetric("LiveNinja/RCA", "RcaEnqueued", 1, "Count",
		map[string]string{"Source": rcaSourceToolRouter, "Tool": tool})
}

// clampRunes truncates to at most n runes. Rune-based, not byte-based, because
// the value it bounds is arbitrary client input that may be multi-byte and a
// byte slice would leave a torn rune in the JSON envelope.
func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// rcaWorthAnalyzing is the enqueue allowlist (M17). An ALLOWLIST, not a
// denylist, so a future error code is silent by default rather than
// accidentally emailing the owner an RCA on every occurrence.
//
// Enqueued:
//   - invalid_args   — the headline case: malformed-args failures ARE the
//     prompt/schema drift the RCA exists to catch.
//   - not_found, upstream_error, already_exists — real, low-volume failures.
//   - not_configured — the highest-value code in the set: it means a Deps
//     wiring or IAM gap in production (the 2026-07-18 incident where every
//     voice-session memory tool answered not_configured while IAM, Bedrock
//     and the REST surface were all healthy). Nothing else notices this.
//   - unknown_tool   — the model invented a tool name: genuine manifest or
//     prompt drift. Sentinel-keyed, see enqueueRCA.
//
// Excluded:
//   - forbidden — two causes, neither a defect: missing user context (already
//     filtered above), or Reauthorize denying a revoked/de-allowlisted user,
//     i.e. the security control WORKING. Enqueuing it would also hand an
//     unauthorized caller a lever to drive Bedrock spend and owner email.
//   - confirmation_required — no handler returns it today, and when one does
//     it will be an ordinary conversational step ("shall I send that?"), not
//     a failure worth an email.
func rcaWorthAnalyzing(code string) bool {
	switch code {
	case CodeInvalidArgs, CodeNotFound, CodeUpstreamError,
		CodeNotConfigured, CodeAlreadyExists, CodeUnknownTool:
		return true
	default:
		return false
	}
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
