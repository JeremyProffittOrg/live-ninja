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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
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
		"/settings":     true,
		"/downloads":    true,

		// Root-scoped PWA assets (served by Fiber outside /static/).
		"/sw.js":       true,
		"/favicon.ico": true,
	}

	publicPrefixes = []string{
		"/static/",
		"/auth/",
		"/.well-known/",
	}
)

var errUserNotFound = errors.New("authorizer: token subject has no user record")

// jwksCache holds the most recently fetched JWKS document (raw JSON) so a
// warm Lambda container verifies tokens without hitting KMS/the JWKS
// endpoint on every invocation. 24h matches internal/auth/session.go's
// own JWKS cache lifetime (the signing key doesn't rotate more often than
// that in normal operation).
type jwksCache struct {
	mu        sync.RWMutex
	data      []byte
	expiresAt time.Time
}

func (c *jwksCache) get(ctx context.Context, url string) ([]byte, error) {
	c.mu.RLock()
	if c.data != nil && time.Now().Before(c.expiresAt) {
		data := c.data
		c.mu.RUnlock()
		return data, nil
	}
	c.mu.RUnlock()

	data, err := fetchJWKS(ctx, url)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.data = data
	c.expiresAt = time.Now().Add(jwksCacheTTL)
	c.mu.Unlock()
	return data, nil
}

var httpClient = &http.Client{Timeout: httpTimeout}

func fetchJWKS(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("authorizer: build jwks request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authorizer: fetch jwks: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authorizer: jwks endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("authorizer: read jwks body: %w", err)
	}
	return body, nil
}

// userSnapshot is the subset of a store.User the authorizer needs per
// request, cached for userCacheTTL so the tokensValidAfter kill-switch
// check ("log out everywhere") lands within that window without a
// DynamoDB read on every single request.
type userSnapshot struct {
	role             string
	status           string
	tokensValidAfter int64
	expiresAt        time.Time
}

type userCache struct {
	mu    sync.Mutex
	items map[string]userSnapshot
}

func newUserCache() *userCache {
	return &userCache{items: make(map[string]userSnapshot)}
}

func (c *userCache) get(ctx context.Context, st *store.Store, userID string) (userSnapshot, error) {
	c.mu.Lock()
	entry, ok := c.items[userID]
	c.mu.Unlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry, nil
	}

	u, err := st.GetUser(ctx, userID)
	if err != nil {
		return userSnapshot{}, fmt.Errorf("authorizer: get user %s: %w", userID, err)
	}
	if u == nil {
		return userSnapshot{}, errUserNotFound
	}

	fresh := userSnapshot{
		role:             u.Role,
		status:           u.Status,
		tokensValidAfter: u.TokensValidAfter,
		expiresAt:        time.Now().Add(userCacheTTL),
	}

	c.mu.Lock()
	c.items[userID] = fresh
	c.mu.Unlock()
	return fresh, nil
}

var (
	logger  = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	jwks    = &jwksCache{}
	users   = newUserCache()
	st      *store.Store
	jwksURL string
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

	jwksJSON, err := jwks.get(ctx, jwksURL)
	if err != nil {
		l.Error("authorizer: jwks fetch/cache failed, denying", slog.String("error", err.Error()))
		return denyResponse(), nil
	}

	// auth.VerifyJWT already validates structure, ES256 signature, and the
	// iss/aud/exp claims (with clock-skew leeway) — no need to re-check
	// those here, and doing so with a stricter/non-skewed comparison would
	// only risk rejecting a token VerifyJWT itself considers valid.
	claims, err := auth.VerifyJWT(token, jwksJSON)
	if err != nil {
		l.Info("authorizer: jwt verification failed, denying", slog.String("error", err.Error()))
		return denyResponse(), nil
	}

	snap, err := users.get(ctx, st, claims.Sub)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			l.Warn("authorizer: token subject has no user record, denying", slog.String("userId", claims.Sub))
		} else {
			l.Error("authorizer: user lookup failed, denying", slog.String("userId", claims.Sub), slog.String("error", err.Error()))
		}
		return denyResponse(), nil
	}

	if snap.status != store.UserStatusActive {
		l.Info("authorizer: user not active, denying",
			slog.String("userId", claims.Sub), slog.String("status", snap.status))
		return denyResponse(), nil
	}

	// The tokensValidAfter kill-switch: any JWT issued before the user's
	// last "log out everywhere" (or admin disable) is rejected, even
	// though its signature and exp are otherwise perfectly valid.
	if claims.Iat < snap.tokensValidAfter {
		l.Info("authorizer: token predates tokensValidAfter, denying",
			slog.String("userId", claims.Sub),
			slog.Int64("iat", claims.Iat),
			slog.Int64("tokensValidAfter", snap.tokensValidAfter))
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
			"role":      snap.role,
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

	jwksURL = os.Getenv("JWKS_URL")
	if jwksURL == "" {
		jwksURL = defaultJWKSURL
	}

	s, err := store.New(ctx, cfg.TableName)
	if err != nil {
		logger.Error("authorizer: store init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	st = s

	lambda.Start(handler)
}
