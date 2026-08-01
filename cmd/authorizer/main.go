// Command authorizer is the HTTP API v2 Lambda authorizer (simple
// response format) fronting the Live Ninja API.
//
// M1 real behavior (per plan.md / contracts/api.md): deny by default;
// allow OPTIONS (CORS preflight) and the explicitly public route surface
// (/healthz, "/", /static/*, /auth/*, /.well-known/*, /v1/app/android/latest,
// /v1/compat) without any session check. Every other route requires a
// valid first-party ES256 access JWT: the bearer token is verified against
// the JWKS published at JWKS_URL (fetched once per cold start, cached
// 24h), its iss/aud/exp are checked, and its subject is cross-checked
// against the user's `tokensValidAfter` kill-switch (store.GetUser,
// cached 60s per user so "log out everywhere" takes effect within that
// window without a DynamoDB read on every request). On success the
// verified userId/sessionId/surface/deviceId/role are injected into the
// simple-response context for downstream handlers (see internal/webapp's
// ExtractAuthContext, which reads them back out of the request-context
// header the HTTP API passes through).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const (
	// defaultJWKSURL mirrors the value template.yaml sets on this
	// function's JWKS_URL env var; used only as a local-dev fallback so
	// `go run` against this package doesn't need every env var set.
	defaultJWKSURL = "https://live.jeremy.ninja/.well-known/jwks.json"

	jwksCacheTTL = 24 * time.Hour
	userCacheTTL = 60 * time.Second

	httpTimeout = 5 * time.Second
)

// publicExact/publicPrefixes are the authorizer's public-route allowlist,
// reconciled against contracts/api.md's Auth column and
// contracts/README.md's CI cross-check note. Every route marked "Public"
// in api.md appears here; every non-public route falls through to full
// JWT verification below.
var (
	publicExact = map[string]bool{
		"/":                      true, // Fiber-rendered landing/login page
		"/healthz":               true,
		"/v1/app/android/latest": true,
		"/v1/compat":             true,

		// /api/v1 aliases of the pre-auth flows (shared-spec route names;
		// internal/webapp/auth_routes.go registers these alongside the
		// canonical /auth/* paths, which the prefix list below covers).
		// They validate their own credential (PKCE code, refresh token, or
		// pairing nonce) inside the handler — same as their /auth/* twins.
		// NOT public: /api/v1/auth/logout-all (JWT-gated, RequireAuth) and
		// /api/v1/auth/logout (bearer path; cookie logout uses /auth/logout).
		"/api/v1/auth/lwa/exchange":    true,
		"/api/v1/auth/refresh":         true,
		"/api/v1/auth/device/register": true,
		"/api/v1/auth/device/poll":     true,

		// SSR pages: auth is enforced server-side by the Fiber page handlers
		// (cookie check → login redirect); the API-GW layer must let the HTML
		// request through or a signed-in browser gets a bare 403 JSON.
		"/conversation": true,
		"/downloads":    true,
		"/memory":       true,
		"/history":      true,
		"/personas":     true,

		// Root-scoped PWA assets (served by Fiber outside /static/).
		"/sw.js":       true,
		"/favicon.ico": true,

		// Mid-run progress from a coding agent on one of the owner's own
		// machines. It carries a per-run `cu_` bearer token, NOT a session JWT,
		// so the full verification below would reject it outright — the route
		// authenticates its own credential in
		// internal/webapp/codeupdate_routes.go (constant-time hash compare
		// against a hashed, TTL-bounded row, uniform 401 on every failure, and
		// a conditional post cap). Same pattern as the /api/v1/auth/* entries
		// above: public at this layer, credential-checked in the handler.
		"/v1/code-update/progress": true,
	}

	publicPrefixes = []string{
		"/static/",
		"/auth/",
		"/.well-known/",
	}
)

var (
	logger   = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	verifier *auth.TokenVerifier
)

func handler(ctx context.Context, req events.APIGatewayV2CustomAuthorizerV2Request) (events.APIGatewayV2CustomAuthorizerSimpleResponse, error) {
	path := req.RawPath
	method := req.RequestContext.HTTP.Method
	requestID := req.RequestContext.RequestID
	l := observ.WithRequest(logger, requestID, "", "authorizer")

	// CORS preflight never carries credentials and must never be blocked
	// by the authorizer, or every browser client breaks on its very first
	// cross-origin request.
	if method == http.MethodOptions {
		l.Info("authorizer: OPTIONS preflight allowed", slog.String("path", path))
		return allowPublic(), nil
	}

	if isPublicRoute(path) {
		l.Info("authorizer: public route allowed", slog.String("path", path))
		return allowPublic(), nil
	}

	token := extractBearerToken(req.Headers)
	if token == "" {
		l.Info("authorizer: no bearer token presented, denying", slog.String("path", path))
		return denyResponse(), nil
	}

	// One shared decision (internal/auth.TokenVerifier): JWKS fetch/cache,
	// ES256 + iss/aud/exp, the user lookup, the active-account check and the
	// tokensValidAfter kill-switch. cmd/iot-authorizer runs the identical call,
	// which is the point — "revoked" cannot come to mean two different things.
	claims, snap, err := verifier.Verify(ctx, token)
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		l.Warn("authorizer: token subject has no user record, denying", slog.String("userId", claims.Sub))
		return denyResponse(), nil
	case errors.Is(err, auth.ErrUserNotActive):
		l.Info("authorizer: user not active, denying",
			slog.String("userId", claims.Sub), slog.String("status", snap.Status))
		return denyResponse(), nil
	case errors.Is(err, auth.ErrTokenRevoked):
		l.Info("authorizer: token predates tokensValidAfter, denying",
			slog.String("userId", claims.Sub),
			slog.Int64("iat", claims.Iat),
			slog.Int64("tokensValidAfter", snap.TokensValidAfter))
		return denyResponse(), nil
	case err != nil:
		l.Info("authorizer: verification failed, denying", slog.String("error", err.Error()))
		return denyResponse(), nil
	}

	l.Info("authorizer: authorized",
		slog.String("userId", claims.Sub),
		slog.String("surface", claims.Surface),
		slog.String("sessionId", claims.Sid))

	return events.APIGatewayV2CustomAuthorizerSimpleResponse{
		IsAuthorized: true,
		Context: map[string]interface{}{
			"userId":    claims.Sub,
			"sessionId": claims.Sid,
			"surface":   claims.Surface,
			"deviceId":  claims.Did,
			"role":      snap.Role,
		},
	}, nil
}

func isPublicRoute(path string) bool {
	if publicExact[path] {
		return true
	}
	for _, prefix := range publicPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// extractBearerToken reads the Authorization header (case-insensitive
// header name and "Bearer" scheme, since API Gateway payload casing isn't
// something to trust blindly). No client in this system currently sets
// an access-JWT cookie (the only cookie defined anywhere, __Host-ln_rt,
// carries an opaque refresh token, not a JWT — see plan.md M1), so there
// is deliberately no cookie-based fallback here to invent an
// unimplemented contract; the API Gateway identity-source list may still
// include the Cookie header (template.yaml, infra agent) purely so the
// authorizer's response cache key varies correctly if that ever changes.
func extractBearerToken(headers map[string]string) string {
	raw := headerValue(headers, "authorization")
	if raw == "" {
		return ""
	}
	const scheme = "bearer "
	if len(raw) > len(scheme) && strings.EqualFold(raw[:len(scheme)], scheme) {
		return strings.TrimSpace(raw[len(scheme):])
	}
	return strings.TrimSpace(raw)
}

func headerValue(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func denyResponse() events.APIGatewayV2CustomAuthorizerSimpleResponse {
	return events.APIGatewayV2CustomAuthorizerSimpleResponse{IsAuthorized: false}
}

func allowPublic() events.APIGatewayV2CustomAuthorizerSimpleResponse {
	return events.APIGatewayV2CustomAuthorizerSimpleResponse{
		IsAuthorized: true,
		Context: map[string]interface{}{
			"surface": "public",
		},
	}
}

func main() {
	ctx := context.Background()
	cfg := config.FromEnv()

	jwksURL := os.Getenv("JWKS_URL")
	if jwksURL == "" {
		jwksURL = defaultJWKSURL
	}

	s, err := store.New(ctx, cfg.TableName)
	if err != nil {
		logger.Error("authorizer: store init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	verifier = auth.NewTokenVerifier(jwksURL, s)

	lambda.Start(handler)
}
