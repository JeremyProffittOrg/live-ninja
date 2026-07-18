package webapp

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
)

// Cookie / header names fixed by the shared spec. __Host- prefixed cookies
// require Secure + Path=/ + no Domain attribute — enforced wherever they
// are set (auth_routes.go).
const (
	// RefreshCookieName holds the web surface's refresh credential:
	// "<sessionId>.<secret>" (see auth_routes.go wire format). HttpOnly.
	RefreshCookieName = "__Host-ln_rt"
	// CSRFCookieName holds the CSRF double-submit value. NOT HttpOnly —
	// the browser JS reads it and echoes it in CSRFHeaderName on POSTs.
	CSRFCookieName = "__Host-ln_csrf"
	// CSRFHeaderName is the request header that must match CSRFCookieName.
	CSRFHeaderName = "X-LN-CSRF"
	// OAuthStateCookieName binds an in-flight LWA transaction to the browser
	// that started it: login sets it to the state value, callback requires an
	// exact match. Without it, callback only checks that the state exists
	// server-side, so an attacker's captured (code,state) could be replayed
	// into a victim's browser (login-CSRF / session fixation). HttpOnly.
	OAuthStateCookieName = "__Host-ln_oauth"
)

// Locals keys populated by ExtractAuthContext and read via the accessor
// helpers below (used by both auth_routes.go and api_routes.go).
const (
	localUserID    = "userId"
	localSessionID = "sessionId"
	localSurface   = "surface"
	localDeviceID  = "deviceId"
	localRole      = "role"
)

// UserID returns the authenticated user id for this request, or "" when
// unauthenticated.
func UserID(c *fiber.Ctx) string { return localString(c, localUserID) }

// SessionID returns the authenticated session id, or "".
func SessionID(c *fiber.Ctx) string { return localString(c, localSessionID) }

// Surface returns the authenticated surface (web|android|device), or "".
func Surface(c *fiber.Ctx) string { return localString(c, localSurface) }

// DeviceID returns the authenticated device id (device surface only), or "".
func DeviceID(c *fiber.Ctx) string { return localString(c, localDeviceID) }

// Role returns the authenticated user's role (owner|member), or "".
func Role(c *fiber.Ctx) string { return localString(c, localRole) }

func localString(c *fiber.Ctx, key string) string {
	if v, ok := c.Locals(key).(string); ok {
		return v
	}
	return ""
}

// amznRequestContext is the fragment of the API Gateway HTTP API v2
// request context we care about. The Lambda Web Adapter layer forwards the
// whole requestContext of the originating event as the
// `x-amzn-request-context` request header; a Lambda authorizer's context
// map lands at .authorizer.lambda.
type amznRequestContext struct {
	Authorizer struct {
		Lambda map[string]any `json:"lambda"`
	} `json:"authorizer"`
}

// ExtractAuthContext populates the auth Locals for every request, from two
// sources in priority order:
//
//  1. The Lambda authorizer context forwarded by the Lambda Web Adapter in
//     the `x-amzn-request-context` header (the deployed path — the
//     authorizer already verified the JWT and tokensValidAfter).
//  2. A local verification of the `Authorization: Bearer` JWT against the
//     Signer's JWKS (local dev where no API Gateway fronts the app, plus
//     defense in depth), including the tokensValidAfter/status check via
//     the user record.
//
// It only ever *extracts* — it never rejects. Enforcement is RequireAuth /
// RequireOwner on the routes that need it.
func ExtractAuthContext(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if raw := c.Get("x-amzn-request-context"); raw != "" {
			var rc amznRequestContext
			if err := json.Unmarshal([]byte(raw), &rc); err == nil && len(rc.Authorizer.Lambda) > 0 {
				setLocalFromAny(c, localUserID, rc.Authorizer.Lambda["userId"])
				setLocalFromAny(c, localSessionID, rc.Authorizer.Lambda["sessionId"])
				setLocalFromAny(c, localSurface, rc.Authorizer.Lambda["surface"])
				setLocalFromAny(c, localDeviceID, rc.Authorizer.Lambda["deviceId"])
				setLocalFromAny(c, localRole, rc.Authorizer.Lambda["role"])
				if UserID(c) != "" {
					return c.Next()
				}
			}
		}

		// Fallback: verify a Bearer JWT locally against our own JWKS.
		token := bearerToken(c)
		if token == "" || deps.Signer == nil {
			return c.Next()
		}
		jwks, err := deps.Signer.JWKS(c.Context())
		if err != nil {
			deps.Log.Warn("auth context: jwks unavailable for local verification", "error", err.Error())
			return c.Next()
		}
		claims, err := auth.VerifyJWT(token, jwks)
		if err != nil {
			return c.Next() // unauthenticated; RequireAuth will 401 where it matters
		}
		now := time.Now().Unix()
		if claims.Exp < now || claims.Iss != auth.Issuer || claims.Aud != auth.Audience {
			return c.Next()
		}

		// tokensValidAfter kill-switch + account status, defense in depth
		// (the deployed authorizer does the same check).
		user, err := deps.Store.GetUser(c.Context(), claims.Sub)
		if err != nil || user == nil || user.Status != "active" {
			return c.Next()
		}
		if user.TokensValidAfter > 0 && claims.Iat < user.TokensValidAfter {
			return c.Next()
		}

		c.Locals(localUserID, claims.Sub)
		c.Locals(localSessionID, claims.Sid)
		c.Locals(localSurface, claims.Surface)
		c.Locals(localDeviceID, claims.Did)
		c.Locals(localRole, user.Role)
		return c.Next()
	}
}

func setLocalFromAny(c *fiber.Ctx, key string, v any) {
	switch t := v.(type) {
	case string:
		if t != "" {
			c.Locals(key, t)
		}
	case fmt.Stringer:
		c.Locals(key, t.String())
	}
}

func bearerToken(c *fiber.Ctx) string {
	h := c.Get(fiber.HeaderAuthorization)
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// CSRFProtect enforces the double-submit CSRF check on state-changing
// requests that arrive with the web refresh cookie: the request must carry
// the (non-HttpOnly) CSRF cookie AND echo its exact value in the X-LN-CSRF
// header. Requests without the refresh cookie (Android/device bearer
// flows, unauthenticated POSTs) pass through — they have no ambient cookie
// credential to forge, so CSRF does not apply.
func CSRFProtect() fiber.Handler {
	return func(c *fiber.Ctx) error {
		switch c.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return c.Next()
		}
		if c.Cookies(RefreshCookieName) == "" {
			return c.Next()
		}

		cookie := c.Cookies(CSRFCookieName)
		header := c.Get(CSRFHeaderName)
		if cookie == "" || header == "" ||
			subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) != 1 {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":   "csrf_failed",
				"message": "Missing or mismatched " + CSRFHeaderName + " header.",
			})
		}
		return c.Next()
	}
}

// RequireAuth rejects the request with 401 unless ExtractAuthContext
// resolved an authenticated user.
func RequireAuth() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if UserID(c) == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		return c.Next()
	}
}

// RequireOwner rejects with 401 when unauthenticated and 403 unless the
// authenticated user's role is owner (admin allowlist management, etc.).
func RequireOwner() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if UserID(c) == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		if Role(c) != "owner" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "owner_only"})
		}
		return c.Next()
	}
}
