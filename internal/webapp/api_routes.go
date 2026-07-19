// RegisterAPIRoutes mounts the authenticated `/api/v1` resource surface
// owned by the "API routes" workstream: profile, device management, the
// owner-only allowlist admin CRUD, realtime session bootstrap, the tool
// router proxy, the transcript sink, and the fallback-cascade proxy.
// RegisterAuthRoutes (owned by the auth workstream, auth_routes.go) is the
// sibling registrar for everything under `/auth/*` + `/.well-known/*`.
//
// Every handler here re-derives its identity (UserID/SessionID/Surface/
// DeviceID/Role, middleware.go accessors) from the Fiber Locals that
// ExtractAuthContext populated ahead of this group — never from a
// client-supplied body/query field (contracts/api.md's anti-confused-
// deputy rule, NFR-02/FR-A02). ExtractAuthContext only ever *extracts*;
// RequireAuth()/RequireOwner() (also middleware.go) are what actually
// reject, and this file applies them to the whole /api/v1 group (plus
// RequireOwner on the admin allowlist routes) so every route here is
// fail-closed regardless of the API Gateway authorizer already having
// enforced the same thing upstream (defense in depth).
//
// CSRF (the `__Host-ln_csrf` double-submit cookie + `X-LN-CSRF` header on
// POSTs, web surface only) is enforced by CSRFProtect(), mounted globally
// in cmd/web/main.go ahead of both route registrars — not duplicated here.
package webapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/memory"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// RegisterAPIRoutes mounts every authenticated /api/v1 route this
// workstream owns. Called once from cmd/web/main.go after Deps is built,
// behind the app-wide ExtractAuthContext/CSRFProtect middleware.
func RegisterAPIRoutes(app *fiber.App, deps *Deps) {
	registry := buildAPIToolsRegistry(deps)

	api := app.Group("/api/v1", RequireAuth())

	api.Get("/me", handleGetMe(deps))

	api.Get("/devices", handleListDevices(deps))
	api.Delete("/devices/:id", handleRevokeDevice(deps))

	api.Get("/admin/allowlist", RequireOwner(), handleListAllowlist(deps))
	api.Post("/admin/allowlist", RequireOwner(), handleAddAllowlist(deps))
	api.Delete("/admin/allowlist", RequireOwner(), handleRemoveAllowlist(deps))

	registerPersonaRoutes(api, deps)

	api.Get("/realtime/session", handleRealtimeSession(deps))
	api.Post("/tools/invoke", handleToolsInvoke(deps, registry))
	api.Post("/transcript", handleTranscript(deps))

	api.Post("/fallback/turn", handleFallbackTurn(deps, registry))
	api.Post("/fallback/stt", handleFallbackSTT(deps))
	api.Post("/fallback/tts", handleFallbackTTS(deps))
}

// ---- small response helpers (auth extraction/enforcement lives in
// middleware.go: UserID/SessionID/Surface/DeviceID/Role, RequireAuth,
// RequireOwner) ----

func apiBadRequest(c *fiber.Ctx, msg string) error {
	return errorJSON(c, fiber.StatusBadRequest, "invalid_request", msg)
}

func apiInternalError(c *fiber.Ctx, deps *Deps, op string, err error) error {
	deps.Log.Error("api: "+op, slog.String("error", err.Error()))
	return errorJSON(c, fiber.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
}

// ---- GET /api/v1/me ----

func handleGetMe(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		u, err := deps.Store.GetUser(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "get user", err)
		}
		if u == nil {
			return errorJSON(c, fiber.StatusNotFound, "not_found", "User not found.")
		}

		return c.JSON(fiber.Map{
			"userId":    u.UserID,
			"email":     u.Email,
			"name":      u.Name,
			"role":      u.Role,
			"status":    u.Status,
			"createdAt": u.CreatedAt,
		})
	}
}

// ---- GET/DELETE /api/v1/devices ----

func handleListDevices(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		devices, err := deps.Store.ListDevices(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list devices", err)
		}

		out := make([]fiber.Map, 0, len(devices))
		for _, d := range devices {
			out = append(out, fiber.Map{
				"deviceId":  d.DeviceID,
				"name":      d.Name,
				"status":    d.Status,
				"thingName": d.ThingName,
				"createdAt": d.CreatedAt,
			})
		}
		return c.JSON(fiber.Map{"devices": out})
	}
}

func handleRevokeDevice(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		deviceID := c.Params("id")
		if strings.TrimSpace(deviceID) == "" {
			return apiBadRequest(c, "device id is required")
		}

		d, err := deps.Store.GetDevice(c.Context(), deviceID)
		if err != nil {
			return apiInternalError(c, deps, "get device", err)
		}
		if d == nil || d.UserID != userID {
			// Same shape whether absent or owned by someone else — never
			// let a caller enumerate other users' device ids.
			return errorJSON(c, fiber.StatusNotFound, "not_found", "Device not found.")
		}

		if d.FamilyID != "" {
			if err := deps.Store.RevokeFamily(c.Context(), userID, d.FamilyID); err != nil {
				deps.Log.Error("api: revoke device session family failed",
					slog.String("error", err.Error()), slog.String("deviceId", deviceID))
				// Continue: still revoke the device row below so a
				// partial failure doesn't leave the device looking active.
			}
		}

		if err := deps.Store.RevokeDevice(c.Context(), deviceID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Device not found.")
			}
			return apiInternalError(c, deps, "revoke device", err)
		}

		// IoT cert detachment is deliberately not attempted here: the
		// WebFunction role is only granted iot:Publish (template.yaml,
		// shared spec M2 infra changes), not
		// iot:DetachThingPrincipal/UpdateCertificate. Cert lifecycle is
		// M5's IoT-provisioning scope (internal/auth/device.go's
		// ProvisionIoT hook); this revoke still makes the device
		// unusable immediately by killing its refresh family and
		// flipping status=revoked, which device_control's ownership
		// check (internal/tools) and any reconnect attempt both honor.
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// ---- GET/POST/DELETE /api/v1/admin/allowlist (owner-only, RequireOwner
// applied at route registration in RegisterAPIRoutes) ----

func handleListAllowlist(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		entries, err := deps.Store.ListAllow(c.Context())
		if err != nil {
			return apiInternalError(c, deps, "list allowlist", err)
		}
		out := make([]fiber.Map, 0, len(entries))
		for _, e := range entries {
			out = append(out, fiber.Map{"key": e.Key, "addedBy": e.AddedBy, "addedAt": e.AddedAt})
		}
		return c.JSON(fiber.Map{"entries": out})
	}
}

func handleAddAllowlist(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)

		var body struct {
			Key string `json:"key"`
		}
		if err := c.BodyParser(&body); err != nil || strings.TrimSpace(body.Key) == "" {
			return apiBadRequest(c, "key (an Amazon user id or email address) is required")
		}

		if err := deps.Store.AddAllow(c.Context(), body.Key, userID); err != nil {
			return apiInternalError(c, deps, "add allowlist entry", err)
		}
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"ok": true})
	}
}

func handleRemoveAllowlist(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		key := c.Query("key")
		if strings.TrimSpace(key) == "" {
			return apiBadRequest(c, "key query parameter is required")
		}
		if err := deps.Store.RemoveAllow(c.Context(), key); err != nil {
			return apiInternalError(c, deps, "remove allowlist entry", err)
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// ---- GET /api/v1/realtime/session ----

// brokerRequest mirrors cmd/realtime-broker's Request (that package is
// `main`, so it can't be imported — this is the wire-contract mirror, per
// the shared spec's M2 broker section).
type brokerRequest struct {
	Mode string `json:"mode,omitempty"`
	// TxID forwards the ingress transaction id (Locals, set by
	// TxnMiddleware) so a single user action correlates across the web
	// function and the broker in CloudWatch. The broker generates one
	// itself when this is empty (e.g. a direct system invoke).
	TxID          string          `json:"txId,omitempty"`
	UserID        string          `json:"userId"`
	Surface       string          `json:"surface"`
	DeviceID      string          `json:"deviceId,omitempty"`
	Persona       string          `json:"persona,omitempty"`
	VoiceOverride string          `json:"voiceOverride,omitempty"`
	MicEagerness  string          `json:"micEagerness,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// brokerResponse mirrors cmd/realtime-broker's Response.
type brokerResponse struct {
	// TxID echoes the transaction correlation id the broker resolved
	// (reused from Request.TxID, or minted fresh when that was empty).
	TxID string `json:"txId,omitempty"`

	// Error shape (contracts/metering.md 402/429 bodies + generic errors).
	Error             string  `json:"error,omitempty"`
	Code              int     `json:"code,omitempty"`
	Kind              string  `json:"kind,omitempty"`
	Message           string  `json:"message,omitempty"`
	Used              float64 `json:"used,omitempty"`
	Limit             float64 `json:"limit,omitempty"`
	ResetAt           string  `json:"resetAt,omitempty"`
	RetryAfterSeconds int     `json:"retryAfterSeconds,omitempty"`

	// Session-mint success shape.
	Mode         string `json:"mode,omitempty"`   // "openai-direct" | "nova-bridge"
	Engine       string `json:"engine,omitempty"` // resolved voiceEngine pin
	ClientSecret *struct {
		Value     string `json:"value"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"clientSecret,omitempty"`
	Model         string          `json:"model,omitempty"`
	Voice         string          `json:"voice,omitempty"`
	SessionConfig json.RawMessage `json:"sessionConfig,omitempty"`
	ToolManifest  json.RawMessage `json:"toolManifest,omitempty"`
	SessionID     string          `json:"sessionId,omitempty"`
	// Nova-bridge fields (Mode == "nova-bridge" only).
	WSURL                string `json:"wsUrl,omitempty"`
	BridgeToken          string `json:"bridgeToken,omitempty"`
	BridgeTokenExpiresAt string `json:"bridgeTokenExpiresAt,omitempty"`
	QuotaWarning         string `json:"quotaWarning,omitempty"`

	// Fallback success shapes: Text for turn/stt; audio for tts.
	// ToolCalls (tool-capable fallback-turn only) carries the model's
	// requested function calls verbatim — the broker never executes them;
	// handleFallbackTurn runs each through the internal/tools registry
	// here (the web function holds the tool-side IAM) and re-invokes.
	Text        string               `json:"text,omitempty"`
	ToolCalls   []brokerChatToolCall `json:"toolCalls,omitempty"`
	AudioBase64 string               `json:"audioBase64,omitempty"`
	ContentType string               `json:"contentType,omitempty"`
}

// brokerChatToolCall mirrors realtime.ChatToolCall (broker wire contract):
// one model-requested function call, arguments as the raw JSON string.
type brokerChatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// brokerChatMessage mirrors realtime.ChatMessage: one turn in the
// fallback tool loop's conversation (user text, an assistant message
// carrying tool calls, or a tool-result message paired by toolCallId).
type brokerChatMessage struct {
	Role       string               `json:"role"`
	Content    string               `json:"content"`
	ToolCalls  []brokerChatToolCall `json:"toolCalls,omitempty"`
	ToolCallID string               `json:"toolCallId,omitempty"`
}

// invokeRealtimeBroker invokes the realtime-broker Lambda (BrokerFn) via
// direct Lambda:Invoke — the web function never holds the OpenAI key
// itself (that isolation is the whole point of the broker split).
func invokeRealtimeBroker(ctx context.Context, deps *Deps, req brokerRequest) (*brokerResponse, error) {
	if deps.Lambda == nil || deps.BrokerFn == "" {
		return nil, errors.New("realtime broker is not configured (BROKER_FUNCTION_NAME / Lambda client)")
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal broker request: %w", err)
	}

	out, err := deps.Lambda.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: aws.String(deps.BrokerFn),
		Payload:      payload,
	})
	if err != nil {
		return nil, fmt.Errorf("invoke broker: %w", err)
	}
	if out.FunctionError != nil {
		return nil, fmt.Errorf("broker function error %q: %s", aws.ToString(out.FunctionError), truncateForLog(out.Payload))
	}

	var resp brokerResponse
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		return nil, fmt.Errorf("decode broker response: %w", err)
	}
	return &resp, nil
}

func truncateForLog(b []byte) string {
	const max = 500
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// apiRespondBrokerError maps a broker error-shaped response onto the HTTP
// contract in contracts/metering.md (402 quota_exceeded / 429
// rate_limited) plus a generic fallback for the broker's other error
// codes (mint_failed, fallback_failed, invalid_request, internal_error).
// The canonical {error:{code,message,txId}} envelope carries resp.Error/
// resp.Message/the caller's own ingress txId (from Locals — always
// present even if the broker's echoed TxID was lost in transit); the
// quota/rate-limit-specific fields ride alongside it at the top level so
// the existing wire contract (kind/used/limit/resetAt/retryAfterSeconds)
// is unchanged for clients already parsing them.
func apiRespondBrokerError(c *fiber.Ctx, resp *brokerResponse) error {
	status := resp.Code
	if status == 0 {
		status = fiber.StatusBadGateway
	}
	message := resp.Message
	if message == "" {
		message = "The realtime broker reported an error."
	}

	requestLogger(c).Error("request failed",
		slog.Int("status", status),
		slog.String("code", resp.Error),
		slog.String("message", message),
		slog.String("method", c.Method()),
		slog.String("path", c.Path()),
	)

	body := fiber.Map{
		"error": ErrorBody{Code: resp.Error, Message: message, TxID: TxID(c)},
	}
	switch resp.Error {
	case "quota_exceeded":
		body["kind"] = resp.Kind
		body["used"] = resp.Used
		body["limit"] = resp.Limit
		body["resetAt"] = resp.ResetAt
	case "rate_limited":
		body["retryAfterSeconds"] = resp.RetryAfterSeconds
		if resp.RetryAfterSeconds > 0 {
			c.Set("Retry-After", strconv.Itoa(resp.RetryAfterSeconds))
		}
	}
	return c.Status(status).JSON(body)
}

func handleRealtimeSession(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, surface, deviceID := UserID(c), Surface(c), DeviceID(c)

		// Resolve session preferences from the settings document (query
		// params win). Before this read, the user's saved voice was never
		// applied — every mint fell back to the default (prod, 2026-07-18:
		// user had Ballad selected, logs showed voice=cedar).
		voice, persona, eagerness := c.Query("voice"), c.Query("persona"), ""
		if doc, derr := deps.Store.GetSettings(c.Context(), userID); derr == nil {
			if voice == "" {
				if v, ok := doc["voice"].(string); ok {
					voice = v
				}
			}
			if persona == "" {
				if p, ok := doc["persona"].(map[string]any); ok {
					if id, ok := p["presetId"].(string); ok {
						persona = id
					}
				}
			}
			if e, ok := doc["micEagerness"].(string); ok {
				eagerness = e
			}
		} else {
			deps.Log.Warn("api: settings read for mint failed; using defaults",
				slog.String("error", derr.Error()), slog.String("userId", userID))
		}

		// Turn the client/settings persona ID into the server-composed ref
		// the broker resolves at mint (built-in -> own -> shared; refs are
		// composed from the verified auth context, never taken from the
		// client — personas_routes.go's qualifyPersonaRef).
		persona = qualifyPersonaRef(c.Context(), deps, userID, persona)

		resp, err := invokeRealtimeBroker(c.Context(), deps, brokerRequest{
			TxID:          TxID(c),
			UserID:        userID,
			Surface:       surface,
			DeviceID:      deviceID,
			Persona:       persona,
			VoiceOverride: voice,
			MicEagerness:  eagerness,
		})
		if err != nil {
			deps.Log.Error("api: realtime session mint failed", slog.String("error", err.Error()), slog.String("userId", userID))
			return errorJSON(c, fiber.StatusBadGateway, "broker_unavailable",
				"Could not reach the realtime broker; use the fallback cascade.")
		}
		if resp.Error != "" {
			return apiRespondBrokerError(c, resp)
		}

		if resp.QuotaWarning != "" {
			c.Set("X-LN-Quota-Warning", resp.QuotaWarning)
		}

		// Session bootstrap is engine-aware (FR-VE-03): a nova-pinned device
		// gets a backend-bridge WSS URL + short-lived token instead of an
		// OpenAI ephemeral secret. Default the mode to openai-direct for
		// backward compatibility with a broker that predates M12.
		mode := resp.Mode
		if mode == "" {
			mode = "openai-direct"
		}
		if mode == "nova-bridge" {
			return c.JSON(fiber.Map{
				"mode":                 mode,
				"engine":               resp.Engine,
				"model":                resp.Model,
				"wsUrl":                resp.WSURL,
				"token":                resp.BridgeToken,
				"bridgeTokenExpiresAt": resp.BridgeTokenExpiresAt,
				"toolManifest":         resp.ToolManifest,
				"sessionId":            resp.SessionID,
			})
		}
		return c.JSON(fiber.Map{
			"mode":          mode,
			"engine":        resp.Engine,
			"clientSecret":  resp.ClientSecret,
			"model":         resp.Model,
			"voice":         resp.Voice,
			"sessionConfig": resp.SessionConfig,
			"toolManifest":  resp.ToolManifest,
			"sessionId":     resp.SessionID,
			// Per-1M-token USD list rates for resp.Model (cost-badge estimate;
			// see internal/realtime/rates.go). openai-direct only — nova-bridge
			// usage events aren't surfaced to the client (internal/voiceengine).
			"rates": realtime.RatesFor(resp.Model),
		})
	}
}

// ---- POST /api/v1/tools/invoke ----

// buildAPIToolsRegistry constructs the server-side tool router
// (internal/tools) once at cold start. Tool-specific AWS clients
// (Scheduler, IoT data-plane) and env config not yet threaded through
// config.App are read directly from the environment here — they degrade
// gracefully (tools.NewRegistry only requires Store/Log/Reauthorize; a
// missing Scheduler/IoT/SQS dependency makes only the affected tool fail
// with CodeNotConfigured at invoke time, per internal/tools's own design).
func buildAPIToolsRegistry(deps *Deps) *tools.Registry {
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		deps.Log.Error("api: load aws config for tools registry failed", slog.String("error", err.Error()))
		return nil
	}

	toolDeps := &tools.Deps{
		Store:            deps.Store,
		DDB:              dynamodb.NewFromConfig(awsCfg),
		TableName:        deps.Cfg.TableName,
		Log:              deps.Log,
		SQS:              deps.SQS,
		EmailQueueURL:    deps.SQSEmailURL,
		OwnerEmail:       os.Getenv("OWNER_EMAIL"),
		Scheduler:        scheduler.NewFromConfig(awsCfg),
		SchedulerGroup:   os.Getenv("SCHEDULER_GROUP"),
		SchedulerRoleARN: os.Getenv("SCHEDULER_ROLE_ARN"),
		Reauthorize:      apiReauthorize(deps),
	}
	if deps.Deliv != nil { // M9 deliverable_* tools (nil interface stays nil → not_configured)
		toolDeps.Deliverables = deps.Deliv
	}
	// M10 memory_* / entity_get / plan_upsert / forget tools: the same
	// Titan-embedder core RegisterMemoryRoutes serves over REST, adapted
	// onto the tool seam. This wiring MUST be here — without it every
	// voice-session memory tool answers not_configured ("Memory failed"
	// in-session) even while IAM, Bedrock access, and the REST surface
	// are all healthy (the prod incident of 2026-07-18).
	if mem := buildAPIToolMemory(ctx, deps); mem != nil {
		toolDeps.Memory = mem
	}
	// IoT.Publish requires a per-account/region data-plane endpoint; only
	// wire the client when one is configured (leaving the field a true
	// nil interface, not a typed-nil *iotdataplane.Client, when it
	// isn't — device_control then reports CodeNotConfigured rather than
	// panicking or silently no-op'ing).
	if endpoint := os.Getenv("IOT_DATA_ENDPOINT"); strings.TrimSpace(endpoint) != "" {
		toolDeps.IoT = iotdataplane.NewFromConfig(awsCfg, func(o *iotdataplane.Options) {
			o.BaseEndpoint = aws.String("https://" + endpoint)
		})
	}

	registry, err := tools.NewRegistry(toolDeps)
	if err != nil {
		deps.Log.Error("api: tools registry init failed", slog.String("error", err.Error()))
		return nil
	}
	return registry
}

// buildAPIToolMemory wires tools.Deps.Memory: the M10 memory core
// (internal/memory, Bedrock Titan v2 embedder over the shared store)
// behind the tool seam via tools.NewMemoryService. Returns nil on a
// construction failure — the memory tools then degrade to
// not_configured, mirroring buildMemoryService in cmd/web/main.go for
// the REST surface — but never nil silently: the degradation is logged
// loudly so a missing embedder shows up in CloudWatch, not just as
// in-session "Memory failed" replies.
func buildAPIToolMemory(ctx context.Context, deps *Deps) tools.MemoryService {
	embedder, err := memory.NewBedrockEmbedder(ctx)
	if err != nil {
		deps.Log.Warn("api: memory embedder unavailable; memory tools degraded to not_configured",
			slog.String("error", err.Error()))
		return nil
	}
	svc, err := memory.NewService(deps.Store, embedder)
	if err != nil {
		deps.Log.Warn("api: memory core unavailable; memory tools degraded to not_configured",
			slog.String("error", err.Error()))
		return nil
	}
	return tools.NewMemoryService(svc)
}

var errAPIUserNotAllowed = errors.New("api: user is not active or no longer allowed")

// apiReauthorize is the tool router's per-call re-authorization callback:
// status active, and owner OR still on the allowlist — re-checked fresh
// against the store on every single tool call, never trusted from the
// (up to 15-minute-old) access JWT.
func apiReauthorize(deps *Deps) tools.ReauthorizeFunc {
	return func(ctx context.Context, userID string) error {
		u, err := deps.Store.GetUser(ctx, userID)
		if err != nil {
			return fmt.Errorf("api: reauthorize: get user: %w", err)
		}
		if u == nil || u.Status != store.UserStatusActive {
			return errAPIUserNotAllowed
		}
		if u.Role == store.RoleOwner {
			return nil
		}
		allowed, err := deps.Store.IsAllowed(ctx, u.AmazonUserID, u.Email)
		if err != nil {
			return fmt.Errorf("api: reauthorize: check allowlist: %w", err)
		}
		if !allowed {
			return errAPIUserNotAllowed
		}
		return nil
	}
}

func handleToolsInvoke(deps *Deps, registry *tools.Registry) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, sessionID, surface := UserID(c), SessionID(c), Surface(c)
		if registry == nil {
			return errorJSON(c, fiber.StatusServiceUnavailable, "not_configured", "the tool router is not configured")
		}

		var body struct {
			Tool           string         `json:"tool"`
			Args           map[string]any `json:"args"`
			IdempotencyKey string         `json:"idempotencyKey"`
			CallID         string         `json:"callId"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if strings.TrimSpace(body.Tool) == "" {
			return apiBadRequest(c, "tool is required")
		}

		res := registry.Invoke(c.Context(), tools.Invocation{
			Tool:           body.Tool,
			Args:           body.Args,
			IdempotencyKey: body.IdempotencyKey,
			CallID:         body.CallID,
			TxID:           TxID(c),
			UserID:         userID,
			SessionID:      sessionID,
			Surface:        surface,
		})
		return c.Status(res.StatusCode()).JSON(res)
	}
}

// ---- POST /api/v1/transcript ----

// transcriptTTL is the LOG# retention window. M7 privacy default is 30
// days (plan.md M7 / Crosscut §4: "transcripts 30d default"); the
// RETENTION_DAYS env var (template.yaml, WebFunction) overrides it.
var transcriptTTL = func() time.Duration {
	days := 30
	if v, err := strconv.Atoi(os.Getenv("RETENTION_DAYS")); err == nil && v > 0 {
		days = v
	}
	return time.Duration(days) * 24 * time.Hour
}()

// activeUserMarkerTTL only needs to outlive usage-rollup's hourly pass
// over "today" (and comfortably cover a late/retried run into the next
// UTC day) — 48h is ample and keeps CONFIG partition churn bounded.
const activeUserMarkerTTL = 48 * time.Hour

func handleTranscript(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, surface := UserID(c), Surface(c)

		var body struct {
			SessionID string `json:"sessionId"`
			// Final marks this as the session's last transcript flush —
			// the M11 session-end seam: it triggers an async invoke of the
			// topics-extract Lambda (FR-TOP-01). A final-only flush with
			// zero turns is valid (client already flushed everything).
			Final bool `json:"final"`
			// Cost is the client's list-price session estimate (accumulated
			// from realtime usage events); only meaningful on the final
			// flush, where it's persisted onto the CONV record.
			Cost *struct {
				USD         float64 `json:"usd"`
				TextTokens  int     `json:"textTokens"`
				AudioTokens int     `json:"audioTokens"`
			} `json:"cost"`
			Turns []struct {
				Seq    int    `json:"seq"`
				Role   string `json:"role"`
				Text   string `json:"text"`
				Engine string `json:"engine"`
			} `json:"turns"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if strings.TrimSpace(body.SessionID) == "" {
			return apiBadRequest(c, "sessionId is required")
		}
		if len(body.Turns) == 0 && !body.Final {
			return apiBadRequest(c, "turns must be a non-empty array (unless final is true)")
		}

		now := time.Now().UTC()
		ttl := now.Add(transcriptTTL).Unix()
		written := 0
		for _, t := range body.Turns {
			if strings.TrimSpace(t.Role) == "" || strings.TrimSpace(t.Text) == "" {
				continue // skip malformed turns rather than failing the whole batch
			}
			sk := fmt.Sprintf("LOG#%s#%06d", body.SessionID, t.Seq)
			err := deps.Store.ConditionalPut(c.Context(), "USER#"+userID, sk, map[string]any{
				"role":    t.Role,
				"text":    t.Text,
				"surface": surface,
				"engine":  t.Engine,
				"ts":      now.Format(time.RFC3339Nano),
			}, ttl)
			if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
				deps.Log.Error("api: transcript turn write failed",
					slog.String("error", err.Error()), slog.String("userId", userID), slog.Int("seq", t.Seq))
				continue
			}
			written++
		}

		// ACTIVEUSER marker: lets usage-rollup find today's active users
		// via a Query on CONFIG (never a table Scan) instead of scanning
		// every user's USAGE partition.
		markerSK := "ACTIVEUSER#" + userID + "#" + now.Format("2006-01-02")
		if err := deps.Store.ConditionalPut(c.Context(), "CONFIG", markerSK,
			map[string]any{"userId": userID}, now.Add(activeUserMarkerTTL).Unix()); err != nil &&
			!errors.Is(err, store.ErrAlreadyExists) {
			deps.Log.Warn("api: activeuser marker write failed", slog.String("error", err.Error()), slog.String("userId", userID))
		}

		// Session-end seam (M11, FR-TOP-01): fire-and-forget the topic
		// extractor. Best-effort — history tagging must never fail a
		// transcript flush.
		if body.Final {
			// Free the broker's concurrency slot now rather than letting it
			// burn the rest of the 10-minute hard cap — leaked slots were
			// blocking every new mint with 429 concurrent_sessions.
			if err := deps.Store.ReleaseSessionSlot(c.Context(), userID, body.SessionID); err != nil {
				deps.Log.Warn("api: session slot release failed",
					slog.String("error", err.Error()), slog.String("sessionId", body.SessionID),
					slog.String("userId", userID))
			}
			cost := sessionCost{}
			if body.Cost != nil {
				cost = sanitizeSessionCost(body.Cost.USD, body.Cost.TextTokens, body.Cost.AudioTokens)
			}
			enqueueTopicExtraction(c.Context(), deps, userID, body.SessionID, DeviceID(c), surface, now, cost)
		}

		return c.JSON(fiber.Map{"ok": true, "written": written})
	}
}

// sessionCost is the sanitized client cost estimate forwarded to the
// topic extractor (zero value = not reported / rejected).
type sessionCost struct {
	USD         float64
	TextTokens  int
	AudioTokens int
}

// sanitizeSessionCost bounds the client-reported figures: the cost is an
// unauthenticated-in-spirit client estimate, so reject anything non-finite,
// negative, or absurd rather than persisting garbage. Caps: $1,000/session
// and 1e9 tokens — orders of magnitude above any real session.
func sanitizeSessionCost(usd float64, textTokens, audioTokens int) sessionCost {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 || usd > 1000 {
		return sessionCost{}
	}
	if textTokens < 0 || textTokens > 1e9 || audioTokens < 0 || audioTokens > 1e9 {
		return sessionCost{}
	}
	return sessionCost{USD: usd, TextTokens: textTokens, AudioTokens: audioTokens}
}

// enqueueTopicExtraction async-invokes the topics-extract Lambda
// (TOPICS_EXTRACT_FUNCTION_NAME, wired in template.yaml alongside the
// web function's lambda:InvokeFunction grant on it). InvocationType=Event
// so the sink never waits on the extraction; identity fields come from the
// verified auth context, mirroring cmd/topics-extract's Event shape.
func enqueueTopicExtraction(ctx context.Context, deps *Deps, userID, sessionID, deviceID, surface string, now time.Time, cost sessionCost) {
	fn := os.Getenv("TOPICS_EXTRACT_FUNCTION_NAME")
	if fn == "" || deps.Lambda == nil {
		deps.Log.Warn("api: topics-extract not configured; skipping topic extraction",
			slog.String("sessionId", sessionID))
		return
	}

	event := map[string]any{
		"userId":    userID,
		"sessionId": sessionID,
		"ts":        now.Format(time.RFC3339),
		"deviceId":  deviceID,
		"surface":   surface,
	}
	if cost.USD > 0 || cost.TextTokens > 0 || cost.AudioTokens > 0 {
		event["costUsd"] = cost.USD
		event["costTextTokens"] = cost.TextTokens
		event["costAudioTokens"] = cost.AudioTokens
	}
	payload, err := json.Marshal(event)
	if err != nil {
		deps.Log.Error("api: marshal topics-extract event failed", slog.String("error", err.Error()))
		return
	}

	if _, err := deps.Lambda.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(fn),
		InvocationType: lambdatypes.InvocationTypeEvent,
		Payload:        payload,
	}); err != nil {
		deps.Log.Warn("api: topics-extract async invoke failed",
			slog.String("error", err.Error()), slog.String("sessionId", sessionID),
			slog.String("userId", userID))
	}
}

// ---- POST /api/v1/fallback/{turn,stt,tts} ----

// maxFallbackToolIterations caps the broker-invoke/execute-tools loop for
// one typed message; when the model still wants tools after the cap, the
// turn degrades to a plain text answer saying the tool limit was hit.
const maxFallbackToolIterations = 5

// fallbackToolLimitText is that degraded answer.
const fallbackToolLimitText = "I hit the limit of tool calls I can run for a single message, so I stopped early. " +
	"The results above are what completed — send another message if you'd like me to continue."

func handleFallbackTurn(deps *Deps, registry *tools.Registry) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, surface, deviceID := UserID(c), Surface(c), DeviceID(c)
		sessionID := SessionID(c)

		var body struct {
			Text    string `json:"text"`
			Persona string `json:"persona,omitempty"`
		}
		if err := c.BodyParser(&body); err != nil || strings.TrimSpace(body.Text) == "" {
			return apiBadRequest(c, "text is required")
		}
		// Same mint-side qualification as handleRealtimeSession: the typed
		// fallback turn accepts only a persona ID and composes the stored-
		// persona ref server-side (anti-injection).
		personaRef := qualifyPersonaRef(c.Context(), deps, userID, body.Persona)

		invokeTurn := func(payloadObj map[string]any) (*brokerResponse, error) {
			payload, err := json.Marshal(payloadObj)
			if err != nil {
				return nil, fmt.Errorf("marshal fallback turn payload: %w", err)
			}
			return invokeRealtimeBroker(c.Context(), deps, brokerRequest{
				Mode: "fallback-turn", TxID: TxID(c), UserID: userID, Surface: surface, DeviceID: deviceID,
				Persona: personaRef, Payload: payload,
			})
		}

		// No tool registry (cold-start init failure) → the legacy plain
		// completion; a broker that returned tool calls here would have no
		// executor, so don't ask for them.
		if registry == nil {
			resp, err := invokeTurn(map[string]any{"text": body.Text})
			if err != nil {
				deps.Log.Error("api: fallback turn failed", slog.String("error", err.Error()), slog.String("userId", userID))
				return errorJSON(c, fiber.StatusBadGateway, "broker_unavailable", "The fallback turn request failed.")
			}
			if resp.Error != "" {
				return apiRespondBrokerError(c, resp)
			}
			return c.JSON(fiber.Map{"text": resp.Text, "toolCalls": []*tools.Result{}})
		}

		// Tool loop: invoke the broker's tool-capable turn; execute every
		// returned tool call through the SAME registry pipeline as
		// POST /api/v1/tools/invoke (schema validation, per-call re-authz,
		// idempotency, audit — confirm-before-send for send_email included);
		// append the results as tool messages and re-invoke, capped at
		// maxFallbackToolIterations.
		messages := []brokerChatMessage{{Role: "user", Content: body.Text}}
		executed := make([]*tools.Result, 0, 4)
		for iter := 0; iter < maxFallbackToolIterations; iter++ {
			resp, err := invokeTurn(map[string]any{"messages": messages})
			if err != nil {
				deps.Log.Error("api: fallback turn failed", slog.String("error", err.Error()), slog.String("userId", userID))
				return errorJSON(c, fiber.StatusBadGateway, "broker_unavailable", "The fallback turn request failed.")
			}
			if resp.Error != "" {
				return apiRespondBrokerError(c, resp)
			}
			if len(resp.ToolCalls) == 0 {
				return c.JSON(fiber.Map{"text": resp.Text, "toolCalls": executed})
			}

			messages = append(messages, brokerChatMessage{
				Role: "assistant", Content: resp.Text, ToolCalls: resp.ToolCalls,
			})
			for i, tc := range resp.ToolCalls {
				res := executeFallbackToolCall(c, registry, tc, iter, i, userID, sessionID, surface)
				executed = append(executed, res)
				out, merr := json.Marshal(res)
				if merr != nil {
					// tools.Result is plain data — this cannot realistically
					// fail, but the model must still get *an* output per call.
					out = []byte(`{"ok":false,"error":{"code":"internal_error","message":"the tool result could not be serialized"}}`)
				}
				messages = append(messages, brokerChatMessage{
					Role: "tool", ToolCallID: tc.ID, Content: string(out),
				})
			}
		}

		deps.Log.Warn("api: fallback turn hit the tool-iteration cap",
			slog.String("userId", userID), slog.Int("executed", len(executed)),
			slog.Int("cap", maxFallbackToolIterations))
		return c.JSON(fiber.Map{
			"text":             fallbackToolLimitText,
			"toolCalls":        executed,
			"toolLimitReached": true,
		})
	}
}

// executeFallbackToolCall runs one model-requested call through the tool
// registry with the authenticated caller's identity (same entrypoint as
// /api/v1/tools/invoke — never a bypass). Arguments that are not valid
// JSON become an invalid_args Result so the model can recover
// conversationally, mirroring the live datachannel dispatcher's posture.
func executeFallbackToolCall(c *fiber.Ctx, registry *tools.Registry, tc brokerChatToolCall,
	iter, idx int, userID, sessionID, surface string) *tools.Result {
	var args map[string]any
	if strings.TrimSpace(tc.Arguments) != "" {
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return &tools.Result{
				Tool: tc.Name, CallID: tc.ID, TxID: TxID(c),
				Error: &tools.ToolError{
					Code:    tools.CodeInvalidArgs,
					Message: "the function-call arguments were not valid JSON",
					TxID:    TxID(c),
				},
			}
		}
	}
	// Idempotency key: unique per attempt — the per-request txId plus the
	// loop position plus the model's (already unique) call id — so a
	// side-effecting tool can never double-fire on a duplicate delivery,
	// while distinct calls always get distinct keys.
	return registry.Invoke(c.Context(), tools.Invocation{
		Tool:           tc.Name,
		Args:           args,
		IdempotencyKey: fmt.Sprintf("%s#fb%d-%d#%s", TxID(c), iter, idx, tc.ID),
		CallID:         tc.ID,
		TxID:           TxID(c),
		UserID:         userID,
		SessionID:      sessionID,
		Surface:        surface,
	})
}

func handleFallbackSTT(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, surface, deviceID := UserID(c), Surface(c), DeviceID(c)

		fh, err := c.FormFile("audio")
		if err != nil {
			return apiBadRequest(c, "multipart form field 'audio' is required")
		}
		f, err := fh.Open()
		if err != nil {
			return apiInternalError(c, deps, "open uploaded audio", err)
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, 25<<20))
		if err != nil {
			return apiInternalError(c, deps, "read uploaded audio", err)
		}
		if len(data) == 0 {
			return apiBadRequest(c, "uploaded audio file is empty")
		}

		payload, err := json.Marshal(map[string]any{
			"audioBase64": base64.StdEncoding.EncodeToString(data),
			"contentType": fh.Header.Get("Content-Type"),
			"filename":    fh.Filename,
		})
		if err != nil {
			return apiInternalError(c, deps, "marshal fallback stt payload", err)
		}
		resp, err := invokeRealtimeBroker(c.Context(), deps, brokerRequest{
			Mode: "fallback-stt", TxID: TxID(c), UserID: userID, Surface: surface, DeviceID: deviceID, Payload: payload,
		})
		if err != nil {
			deps.Log.Error("api: fallback stt failed", slog.String("error", err.Error()), slog.String("userId", userID))
			return errorJSON(c, fiber.StatusBadGateway, "broker_unavailable", "The fallback transcription request failed.")
		}
		if resp.Error != "" {
			return apiRespondBrokerError(c, resp)
		}
		return c.JSON(fiber.Map{"text": resp.Text})
	}
}

func handleFallbackTTS(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, surface, deviceID := UserID(c), Surface(c), DeviceID(c)

		var body struct {
			Text  string `json:"text"`
			Voice string `json:"voice,omitempty"`
		}
		if err := c.BodyParser(&body); err != nil || strings.TrimSpace(body.Text) == "" {
			return apiBadRequest(c, "text is required")
		}

		payload, err := json.Marshal(map[string]any{"text": body.Text, "voice": body.Voice})
		if err != nil {
			return apiInternalError(c, deps, "marshal fallback tts payload", err)
		}
		resp, err := invokeRealtimeBroker(c.Context(), deps, brokerRequest{
			Mode: "fallback-tts", TxID: TxID(c), UserID: userID, Surface: surface, DeviceID: deviceID, Payload: payload,
		})
		if err != nil {
			deps.Log.Error("api: fallback tts failed", slog.String("error", err.Error()), slog.String("userId", userID))
			return errorJSON(c, fiber.StatusBadGateway, "broker_unavailable", "The fallback speech request failed.")
		}
		if resp.Error != "" {
			return apiRespondBrokerError(c, resp)
		}

		audio, err := base64.StdEncoding.DecodeString(resp.AudioBase64)
		if err != nil {
			return apiInternalError(c, deps, "decode fallback tts audio", err)
		}
		contentType := resp.ContentType
		if contentType == "" {
			contentType = "audio/mpeg"
		}
		c.Set("Content-Type", contentType)
		return c.Send(audio)
	}
}
