package webapp

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
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
	// TxnHeaderName carries the per-request transaction id back to the
	// client on every response (success or error). The web client reads it
	// alongside the error envelope's txId so a user can quote a single
	// "Ref:" value when reporting a problem.
	TxnHeaderName = "X-LN-Txn"
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
	localScope     = "scope"
	// localTxID holds the transaction id set by TxnMiddleware. Read via
	// TxID(c); errorJSON stamps it into the error envelope.
	localTxID = "txId"
	// localLogger holds the request-scoped slog.Logger (already enriched
	// with txId) that TxnMiddleware stashes so errorJSON can log an error
	// without the route having to thread a logger into the helper.
	localLogger = "logger"
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

// Scope returns the independently verified bearer capability scope, or "" for
// ordinary access tokens. Route handlers use it only for scope-specific
// identity binding; authorization itself remains in ExtractAuthContext.
func Scope(c *fiber.Ctx) string { return localString(c, localScope) }

func localString(c *fiber.Ctx, key string) string {
	if v, ok := c.Locals(key).(string); ok {
		return v
	}
	return ""
}

// TxID returns the transaction id assigned to this request by
// TxnMiddleware, or "" if the middleware did not run (it always runs on the
// real app; the empty case only happens in isolated handler tests).
func TxID(c *fiber.Ctx) string { return localString(c, localTxID) }

// txnContextKey is the private key under which TxnMiddleware also stashes
// the txId in the request-scoped context.Context (c.UserContext()), so code
// paths that only hold a context.Context (store calls, downstream AWS SDK
// calls) can still recover the id via TxnFromContext.
type txnContextKey struct{}

// ContextWithTxn returns a child context carrying txID. TxnMiddleware uses
// it to enrich the Fiber user-context; callers spawning their own contexts
// can reuse it to keep the id flowing.
func ContextWithTxn(ctx context.Context, txID string) context.Context {
	return context.WithValue(ctx, txnContextKey{}, txID)
}

// TxnFromContext recovers the txId placed by ContextWithTxn, or "" if none.
func TxnFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(txnContextKey{}).(string); ok {
		return v
	}
	return ""
}

// amznRequestID is the minimal shape needed to pull the API Gateway request
// id out of the forwarded request context. The Lambda Web Adapter forwards
// the originating event's whole requestContext as the
// `x-amzn-request-context` header; API Gateway HTTP API v2 puts a unique
// per-request id at `.requestId`. Preferring it over a freshly minted UUID
// means the txId in our logs is the same id AWS records in its own access
// logs, so a support reference cross-references both.
type amznRequestID struct {
	RequestID string `json:"requestId"`
}

// deriveTxID resolves the transaction id for a request: the API Gateway
// requestId when the forwarded request context carries one, otherwise a
// fresh UUID v4. (The client cannot inject its own txId — allowing that
// would let a caller collide or spoof references; ingress always owns it.)
func deriveTxID(c *fiber.Ctx) string {
	if raw := c.Get("x-amzn-request-context"); raw != "" {
		var rc amznRequestID
		if err := json.Unmarshal([]byte(raw), &rc); err == nil && rc.RequestID != "" {
			return rc.RequestID
		}
	}
	return observ.NewTxnID()
}

// TxnMiddleware is the first middleware in the chain (ahead of auth): it
// assigns every request a transaction id, exposes it three ways —
// Fiber Locals (TxID), the request-scoped context.Context (TxnFromContext),
// and the X-LN-Txn response header — and emits the verbose request/response
// log pair required for full observability.
//
// It logs an INFO "request" line at ingress (method, path, txId, and the
// *keys* of the query string — never the values, which may hold PII) and an
// INFO "response" line at completion (status, latency, response bytes, and
// the userId/surface now resolved by the downstream auth middleware). The
// Authorization/Cookie/CSRF headers are recorded only through observ.Redact
// so their presence is logged but their secret values never are.
func TxnMiddleware(logger *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		txID := deriveTxID(c)
		c.Locals(localTxID, txID)
		c.SetUserContext(ContextWithTxn(c.UserContext(), txID))
		c.Set(TxnHeaderName, txID)

		l := observ.WithTxn(logger, txID)
		c.Locals(localLogger, l)
		l.Info("request",
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.String("query_keys", queryKeys(c)),
			slog.Any("headers", observ.Redact(requestHeaders(c))),
		)

		err := c.Next()

		surface := Surface(c)
		if surface == "" {
			surface = surfaceForPath(c.Path())
		}
		l.Info("response",
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", c.Response().StatusCode()),
			slog.String("userId", UserID(c)),
			slog.String("surface", surface),
			slog.Int("bytes", len(c.Response().Body())),
			slog.Duration("latency", time.Since(start)),
		)
		return err
	}
}

// queryKeys returns the sorted, comma-joined set of query-string parameter
// names present on the request — keys only, never values (a query value may
// carry an email, code, or other PII). Empty string when there is no query.
func queryKeys(c *fiber.Ctx) string {
	m := c.Queries()
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// requestHeaders collects the incoming request headers into a plain map so
// observ.Redact can strip the credential-bearing ones before they are
// logged. Multi-value headers are joined with ", " (the standard HTTP
// list separator); order within a header is preserved.
func requestHeaders(c *fiber.Ctx) map[string]string {
	out := make(map[string]string)
	c.Request().Header.VisitAll(func(key, value []byte) {
		k := string(key)
		if existing, ok := out[k]; ok {
			out[k] = existing + ", " + string(value)
		} else {
			out[k] = string(value)
		}
	})
	return out
}

// surfaceForPath is the pre-auth fallback surface label, derived from the
// URL path when the auth context has not (yet) resolved a real surface. It
// mirrors the labels the auth middleware would set.
func surfaceForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/static/"):
		return "static"
	case strings.HasPrefix(path, "/auth/"):
		return "auth"
	case strings.HasPrefix(path, "/.well-known/"):
		return "well-known"
	default:
		return "web"
	}
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
// Ordinary session/device tokens are only extracted here; enforcement is
// RequireAuth / RequireOwner on the routes that need it. The one exception is
// the broker-minted scope=nova credential: it is a capability token for the
// Nova bridge's two server-to-server sinks, not a general access token. This
// middleware therefore rejects it everywhere except the exact POST routes
// that the bridge calls.
func ExtractAuthContext(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		token := bearerToken(c)
		if raw := c.Get("x-amzn-request-context"); raw != "" {
			var rc amznRequestContext
			if err := json.Unmarshal([]byte(raw), &rc); err == nil && len(rc.Authorizer.Lambda) > 0 {
				setLocalFromAny(c, localUserID, rc.Authorizer.Lambda["userId"])
				setLocalFromAny(c, localSessionID, rc.Authorizer.Lambda["sessionId"])
				setLocalFromAny(c, localSurface, rc.Authorizer.Lambda["surface"])
				setLocalFromAny(c, localDeviceID, rc.Authorizer.Lambda["deviceId"])
				setLocalFromAny(c, localRole, rc.Authorizer.Lambda["role"])
				if UserID(c) != "" {
					// The current API Gateway authorizer context predates the
					// scope claim. Read it when present, but also detect the
					// scope on the original bearer JWT so deployed requests are
					// protected without trusting an unverified claim.
					contextScope, _ := rc.Authorizer.Lambda["scope"].(string)
					return continueAuthorizerRequest(c, deps, token, contextScope)
				}
			}
		}

		// Fallback: verify a Bearer JWT locally against our own JWKS.
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
		c.Locals(localScope, claims.Scope)
		return enforceBearerScope(c, deps, claims.Scope)
	}
}

// continueAuthorizerRequest applies the Nova capability boundary after API
// Gateway has already authenticated the bearer token and supplied identity
// locals. Normal access tokens take the historical fast path unchanged. A
// potential Nova token is independently verified before its scope is trusted;
// this both keeps the sink usable with today's authorizer context (which does
// not include scope) and fails closed if local JWKS verification is unavailable.
func continueAuthorizerRequest(c *fiber.Ctx, deps *Deps, token, contextScope string) error {
	candidateScope := strings.TrimSpace(contextScope)
	if candidateScope == "" {
		candidateScope = unverifiedBearerScope(token)
	}
	if candidateScope != realtime.NovaScope {
		return c.Next()
	}
	if token == "" || deps == nil || deps.Signer == nil {
		return errorJSON(c, fiber.StatusUnauthorized, "invalid_token",
			"The scoped bearer token could not be verified.")
	}

	jwks, err := deps.Signer.JWKS(c.Context())
	if err != nil {
		requestLogger(c).Warn("auth context: jwks unavailable for scoped-token verification",
			slog.String("error", err.Error()))
		return errorJSON(c, fiber.StatusUnauthorized, "invalid_token",
			"The scoped bearer token could not be verified.")
	}
	claims, err := auth.VerifyJWT(token, jwks)
	if err != nil || claims.Scope != realtime.NovaScope ||
		!claimsMatchAuthContext(c, claims) {
		return errorJSON(c, fiber.StatusUnauthorized, "invalid_token",
			"The scoped bearer token could not be verified.")
	}
	c.Locals(localScope, claims.Scope)
	return enforceBearerScope(c, deps, claims.Scope)
}

// claimsMatchAuthContext prevents a scoped token from being paired with a
// different upstream authorizer identity. API Gateway derives both values
// from the same Authorization header in production; an inconsistency is
// therefore always an authentication failure.
func claimsMatchAuthContext(c *fiber.Ctx, claims *auth.Claims) bool {
	if claims == nil {
		return false
	}
	return claims.Sub == UserID(c) &&
		claims.Sid == SessionID(c) &&
		claims.Surface == Surface(c) &&
		claims.Did == DeviceID(c)
}

// unverifiedBearerScope is only a cheap classifier deciding whether the
// independently verified slow path above is required. Its result never grants
// access: scope=nova must still pass auth.VerifyJWT and match the upstream
// authorizer identity before either sink is reached.
func unverifiedBearerScope(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Scope)
}

// enforceBearerScope confines the broker-minted Nova capability credential to
// the two bridge sink calls. Exact path and method matching is deliberate:
// aliases, descendants, reads, and all other authenticated resources remain
// unavailable to this short-lived bridge token.
func enforceBearerScope(c *fiber.Ctx, deps *Deps, scope string) error {
	if scope != realtime.NovaScope {
		return c.Next()
	}
	if c.Method() == fiber.MethodPost {
		switch c.Path() {
		case "/api/v1/transcript", "/api/v1/tools/invoke":
			if deps == nil || deps.Store == nil {
				return errorJSON(c, fiber.StatusServiceUnavailable, "session_check_unavailable",
					"The scoped session could not be verified.")
			}
			active, err := deps.Store.IsRedeemedSessionActive(c.Context(), UserID(c), SessionID(c))
			if err != nil {
				requestLogger(c).Error("auth context: scoped session check failed",
					slog.String("error", err.Error()))
				return errorJSON(c, fiber.StatusServiceUnavailable, "session_check_unavailable",
					"The scoped session could not be verified.")
			}
			if !active {
				return errorJSON(c, fiber.StatusUnauthorized, "session_inactive",
					"The scoped session is no longer active.")
			}
			return c.Next()
		}
	}
	return errorJSON(c, fiber.StatusForbidden, "insufficient_scope",
		"The scoped bearer token is not authorized for this endpoint.")
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
		// The device-pairing confirm POST carries its own, stronger CSRF
		// defense: the one-shot PAIRCONFIRM token double-submitted as a
		// hidden form field + the __Host-ln_pair HttpOnly cookie, both
		// constant-time matched in the handler (auth_routes.go). The generic
		// header check must not apply — a phone that ALSO holds a signed-in
		// web session hits this middleware with a plain HTML form POST that
		// cannot send X-LN-CSRF (live repro: Tab5 pairing 403 csrf_failed,
		// 2026-07-18).
		if c.Path() == "/auth/device/pair/confirm" {
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
