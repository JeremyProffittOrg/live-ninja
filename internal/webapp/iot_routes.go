package webapp

// GET /api/v1/iot/credentials — everything a browser needs to open its MQTT
// connection for the cross-device change fan-out (plan.md §6 WS-3 M3.5).
//
// This route exists because of a mismatch that only surfaced when the client
// was wired: the web app authenticates by HttpOnly cookie, so JavaScript has
// no readable token — but an MQTT CONNECT packet carries its credential as a
// value, and AWS IoT accepts it only in the user-name field (the sole route a
// browser has, since it cannot set WebSocket handshake headers).
//
// The obvious shortcut — return the session JWT — is the wrong answer. That
// would move a full API credential into JS memory and put the entire API in
// reach of any XSS, to save writing this file. Instead the token minted here
// carries a DIFFERENT audience (auth.AudienceIoT) that cmd/iot-authorizer
// requires and cmd/authorizer refuses. A leaked one opens a subscription to
// its own owner's event stream and can do nothing else — the IoT policy that
// authorizer returns is itself scoped to a single user, and grants publish
// only on that user's presence prefix and the single turn-taking lock topic.

import (
	"log/slog"
	"os"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	lnsync "github.com/JeremyProffittOrg/live-ninja/internal/sync"
)

// iotAuthorizerName must match the AuthorizerName in template.yaml. Clients
// name it explicitly on the connect URL, which is why the account's other
// (certificate-authenticated) IoT devices are unaffected by its existence.
const iotAuthorizerName = "live-ninja-iot"

// RegisterIoTRoutes mounts the credential route under the authenticated API.
func RegisterIoTRoutes(app *fiber.App, deps *Deps) {
	api := app.Group("/api/v1", RequireAuth())
	api.Get("/iot/credentials", handleIoTCredentials(deps))
}

func handleIoTCredentials(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		if userID == "" {
			return errorJSON(c, fiber.StatusUnauthorized, "unauthorized", "sign in first")
		}
		if deps.Signer == nil {
			return errorJSON(c, fiber.StatusServiceUnavailable, "not_configured",
				"realtime notifications are not available")
		}

		endpoint, err := iotDataEndpoint(c, deps)
		if err != nil {
			deps.Log.Warn("api: iot endpoint unavailable",
				slog.String("error", err.Error()), slog.String("userId", userID))
			return errorJSON(c, fiber.StatusServiceUnavailable, "not_configured",
				"realtime notifications are not available")
		}

		// The narrow token. Surface and device come from the verified request
		// context, never from the body, so a client cannot mint itself a
		// credential describing a different device.
		token, err := deps.Signer.SignAccessToken(c.Context(), auth.Claims{
			Sub:     userID,
			Sid:     SessionID(c),
			Did:     DeviceID(c),
			Surface: Surface(c),
			Aud:     auth.AudienceIoT,
		})
		if err != nil {
			deps.Log.Error("api: mint iot token failed", slog.String("error", err.Error()))
			return errorJSON(c, fiber.StatusInternalServerError, "internal", "could not mint a token")
		}

		return c.JSON(fiber.Map{
			"endpoint":       endpoint,
			"authorizerName": iotAuthorizerName,
			// The MQTT client id. It lands inside the IoT policy's Connect
			// resource ARN, so the authorizer character-allowlists it; keeping
			// it to the device id (or the session id for a browser with no
			// device) stays well inside that.
			"clientId": iotClientID(c, userID),
			// The value the SERVER will stamp as actorDeviceId on events this
			// client causes (tools.Invocation.DeviceID — the same
			// authorizer-derived id). The client echoes it back in the
			// comparison rather than deriving its own, because a locally
			// generated device id and the verified one are not guaranteed to
			// be the same string, and a mismatch means every device announces
			// its OWN edits back to the user.
			"actorDeviceId": DeviceID(c),
			"token":         token,
			// Seconds, so a client can schedule its reconnect without parsing
			// the JWT it is not supposed to inspect.
			"expiresInSeconds": int(auth.AccessTokenTTL.Seconds()),
			// Topics the client may subscribe to, so the filter lives in ONE
			// place rather than being rebuilt (and drifting) in each client.
			"topicFilter":   "liveninja/user/" + userID + "/#",
			"presenceTopic": lnsync.PresenceTopic(userID, iotClientID(c, userID)),
			// The turn-taking lock (plan.md §6 WS-5 M5.2). Server-supplied for
			// the same reason topicFilter is: the authorizer grants publish on
			// this exact literal, so if a client concatenated its own copy the
			// two would drift and every claim would be refused — silently, as a
			// closed connection the client reconnects into.
			"speakingTopic": lnsync.SpeakingTopic(userID),
		})
	}
}

// iotClientID derives a stable, ARN-safe MQTT client id. Two connections
// sharing a client id would evict each other, so it must differ per browser
// tab / device — the session id does that and is already unique per sign-in.
func iotClientID(c *fiber.Ctx, userID string) string {
	for _, candidate := range []string{DeviceID(c), SessionID(c), userID} {
		if s := sanitizeClientID(candidate); s != "" {
			return s
		}
	}
	return "web"
}

// sanitizeClientID mirrors cmd/iot-authorizer's allowlist. Anything it would
// reject is dropped here rather than handed out as a client id that can never
// connect.
func sanitizeClientID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 128 {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == ':', r == '.':
		default:
			return ""
		}
	}
	return s
}

// iotDataEndpoint resolves the account's IoT ATS data endpoint. IOT_DATA_ENDPOINT
// short-circuits the control-plane call (same convention internal/sync uses);
// otherwise the shared publisher resolves and caches it per warm container.
func iotDataEndpoint(c *fiber.Ctx, deps *Deps) (string, error) {
	if env := strings.TrimSpace(os.Getenv("IOT_DATA_ENDPOINT")); env != "" {
		return env, nil
	}
	pub, err := lnsync.SharedPublisher(c.Context(), deps.Log)
	if err != nil {
		return "", err
	}
	return pub.DataEndpoint(c.Context())
}
