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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/gofiber/fiber/v2"

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

	api.Get("/realtime/session", handleRealtimeSession(deps))
	api.Post("/tools/invoke", handleToolsInvoke(deps, registry))
	api.Post("/transcript", handleTranscript(deps))

	api.Post("/fallback/turn", handleFallbackTurn(deps))
	api.Post("/fallback/stt", handleFallbackSTT(deps))
	api.Post("/fallback/tts", handleFallbackTTS(deps))
}

// ---- small response helpers (auth extraction/enforcement lives in
// middleware.go: UserID/SessionID/Surface/DeviceID/Role, RequireAuth,
// RequireOwner) ----

func apiBadRequest(c *fiber.Ctx, msg string) error {
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request", "message": msg})
}

func apiInternalError(c *fiber.Ctx, deps *Deps, op string, err error) error {
	deps.Log.Error("api: "+op, slog.String("error", err.Error()))
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal_error"})
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
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_found"})
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
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_found"})
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
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_found"})
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
	Mode          string          `json:"mode,omitempty"`
	UserID        string          `json:"userId"`
	Surface       string          `json:"surface"`
	DeviceID      string          `json:"deviceId,omitempty"`
	Persona       string          `json:"persona,omitempty"`
	VoiceOverride string          `json:"voiceOverride,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// brokerResponse mirrors cmd/realtime-broker's Response.
type brokerResponse struct {
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
	ClientSecret *struct {
		Value     string `json:"value"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"clientSecret,omitempty"`
	Model         string          `json:"model,omitempty"`
	Voice         string          `json:"voice,omitempty"`
	SessionConfig json.RawMessage `json:"sessionConfig,omitempty"`
	ToolManifest  json.RawMessage `json:"toolManifest,omitempty"`
	SessionID     string          `json:"sessionId,omitempty"`
	QuotaWarning  string          `json:"quotaWarning,omitempty"`

	// Fallback success shapes: Text for turn/stt; audio for tts.
	Text        string `json:"text,omitempty"`
	AudioBase64 string `json:"audioBase64,omitempty"`
	ContentType string `json:"contentType,omitempty"`
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
func apiRespondBrokerError(c *fiber.Ctx, resp *brokerResponse) error {
	status := resp.Code
	if status == 0 {
		status = fiber.StatusBadGateway
	}

	body := fiber.Map{"error": resp.Error}
	if resp.Message != "" {
		body["message"] = resp.Message
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

		resp, err := invokeRealtimeBroker(c.Context(), deps, brokerRequest{
			UserID:        userID,
			Surface:       surface,
			DeviceID:      deviceID,
			Persona:       c.Query("persona"),
			VoiceOverride: c.Query("voice"),
		})
		if err != nil {
			deps.Log.Error("api: realtime session mint failed", slog.String("error", err.Error()), slog.String("userId", userID))
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
				"error":   "broker_unavailable",
				"message": "Could not reach the realtime broker; use the fallback cascade.",
			})
		}
		if resp.Error != "" {
			return apiRespondBrokerError(c, resp)
		}

		if resp.QuotaWarning != "" {
			c.Set("X-LN-Quota-Warning", resp.QuotaWarning)
		}
		return c.JSON(fiber.Map{
			"clientSecret":  resp.ClientSecret,
			"model":         resp.Model,
			"voice":         resp.Voice,
			"sessionConfig": resp.SessionConfig,
			"toolManifest":  resp.ToolManifest,
			"sessionId":     resp.SessionID,
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
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "not_configured", "message": "the tool router is not configured",
			})
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
			UserID:         userID,
			SessionID:      sessionID,
			Surface:        surface,
		})
		return c.Status(res.StatusCode()).JSON(res)
	}
}

// ---- POST /api/v1/transcript ----

// transcriptTTL matches the shared spec's LOG# retention (90 days).
const transcriptTTL = 90 * 24 * time.Hour

// activeUserMarkerTTL only needs to outlive usage-rollup's hourly pass
// over "today" (and comfortably cover a late/retried run into the next
// UTC day) — 48h is ample and keeps CONFIG partition churn bounded.
const activeUserMarkerTTL = 48 * time.Hour

func handleTranscript(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, surface := UserID(c), Surface(c)

		var body struct {
			SessionID string `json:"sessionId"`
			Turns     []struct {
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
		if len(body.Turns) == 0 {
			return apiBadRequest(c, "turns must be a non-empty array")
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

		return c.JSON(fiber.Map{"ok": true, "written": written})
	}
}

// ---- POST /api/v1/fallback/{turn,stt,tts} ----

func handleFallbackTurn(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, surface, deviceID := UserID(c), Surface(c), DeviceID(c)

		var body struct {
			Text    string `json:"text"`
			Persona string `json:"persona,omitempty"`
		}
		if err := c.BodyParser(&body); err != nil || strings.TrimSpace(body.Text) == "" {
			return apiBadRequest(c, "text is required")
		}

		payload, err := json.Marshal(map[string]any{"text": body.Text})
		if err != nil {
			return apiInternalError(c, deps, "marshal fallback turn payload", err)
		}
		resp, err := invokeRealtimeBroker(c.Context(), deps, brokerRequest{
			Mode: "fallback-turn", UserID: userID, Surface: surface, DeviceID: deviceID,
			Persona: body.Persona, Payload: payload,
		})
		if err != nil {
			deps.Log.Error("api: fallback turn failed", slog.String("error", err.Error()), slog.String("userId", userID))
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "broker_unavailable", "message": "The fallback turn request failed."})
		}
		if resp.Error != "" {
			return apiRespondBrokerError(c, resp)
		}
		return c.JSON(fiber.Map{"text": resp.Text})
	}
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
			Mode: "fallback-stt", UserID: userID, Surface: surface, DeviceID: deviceID, Payload: payload,
		})
		if err != nil {
			deps.Log.Error("api: fallback stt failed", slog.String("error", err.Error()), slog.String("userId", userID))
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "broker_unavailable", "message": "The fallback transcription request failed."})
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
			Mode: "fallback-tts", UserID: userID, Surface: surface, DeviceID: deviceID, Payload: payload,
		})
		if err != nil {
			deps.Log.Error("api: fallback tts failed", slog.String("error", err.Error()), slog.String("userId", userID))
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "broker_unavailable", "message": "The fallback speech request failed."})
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
