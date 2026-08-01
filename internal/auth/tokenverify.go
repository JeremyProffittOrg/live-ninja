package auth

// Shared access-token verification for the Lambda authorizers.
//
// This is the whole "is this bearer token good right now" decision, in one
// place: fetch/cache the JWKS, verify the ES256 signature and iss/aud/exp
// (VerifyJWT), look the user up, and apply the two live checks a signature
// cannot express — the account must be active, and the token must not predate
// the user's `tokensValidAfter` ("log out everywhere") mark.
//
// It exists because there are now TWO authorizers: cmd/authorizer in front of
// the HTTP API, and cmd/iot-authorizer in front of MQTT. Two copies of this
// logic would drift, and the drift would be silent and one-sided — an MQTT
// connection is authorized ONCE and then held for up to an hour, so a
// kill-switch tightened on the HTTP side and missed here would leave a revoked
// user with a live subscription. Sharing the code is what keeps "log out
// everywhere" meaning the same thing on both.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const (
	// JWKSCacheTTL matches session.go's own JWKS cache lifetime — the signing
	// key does not rotate more often than this in normal operation.
	JWKSCacheTTL = 24 * time.Hour
	// UserCacheTTL bounds how long a revoked user keeps working: the
	// tokensValidAfter kill-switch lands within this window without costing a
	// DynamoDB read on every single request.
	UserCacheTTL = 60 * time.Second

	jwksHTTPTimeout = 5 * time.Second
)

// ErrUserNotFound means the token verified but its subject has no user record.
// Callers distinguish it so a missing user logs differently from a broken
// DynamoDB — one is an authorization outcome, the other is an outage.
var ErrUserNotFound = errors.New("auth: token subject has no user record")

// ErrUserNotActive means the account is disabled.
var ErrUserNotActive = errors.New("auth: user is not active")

// ErrTokenRevoked means the token predates the user's tokensValidAfter mark:
// its signature and exp are perfectly valid and it is still refused.
var ErrTokenRevoked = errors.New("auth: token predates tokensValidAfter")

// UserSnapshot is the subset of a store.User an authorization decision needs.
type UserSnapshot struct {
	Role             string
	Status           string
	TokensValidAfter int64

	expiresAt time.Time
}

// UserGetter is the single store operation verification needs; *store.Store
// satisfies it and tests inject a fake.
type UserGetter interface {
	GetUser(ctx context.Context, userID string) (*store.User, error)
}

// TokenVerifier caches the JWKS document and per-user snapshots across warm
// Lambda invocations. The zero value is not usable — use NewTokenVerifier.
type TokenVerifier struct {
	jwksURL string
	users   UserGetter
	client  *http.Client

	mu        sync.RWMutex
	jwks      []byte
	jwksUntil time.Time

	umu   sync.Mutex
	cache map[string]UserSnapshot
}

// NewTokenVerifier builds a verifier over the JWKS at jwksURL and the user
// records in users.
func NewTokenVerifier(jwksURL string, users UserGetter) *TokenVerifier {
	return &TokenVerifier{
		jwksURL: jwksURL,
		users:   users,
		client:  &http.Client{Timeout: jwksHTTPTimeout},
		cache:   make(map[string]UserSnapshot),
	}
}

// Verify runs the full decision and returns the token's claims plus the user
// snapshot behind them. Every failure mode is an error: callers deny on any of
// them and only inspect the type to decide how loudly to log.
//
// CONTRACT: claims are non-nil for every outcome in which the JWT itself
// verified — including ErrUserNotFound, ErrUserNotActive and ErrTokenRevoked —
// so a caller may log claims.Sub on any of those without a nil check. Only a
// JWKS failure or a JWT that failed verification returns nil claims.
func (v *TokenVerifier) Verify(ctx context.Context, token string) (*Claims, UserSnapshot, error) {
	jwksJSON, err := v.jwksDocument(ctx)
	if err != nil {
		return nil, UserSnapshot{}, err
	}

	// VerifyJWT already validates structure, the ES256 signature, and
	// iss/aud/exp with clock-skew leeway. Re-checking those here with a
	// stricter comparison would only risk rejecting a token it considers good.
	claims, err := VerifyJWT(token, jwksJSON)
	if err != nil {
		return nil, UserSnapshot{}, fmt.Errorf("auth: verify jwt: %w", err)
	}

	// claims are returned even here: "which subject had no user record" is the
	// useful half of that log line, and a caller that has a verified JWT should
	// never have to nil-check it.
	snap, err := v.snapshot(ctx, claims.Sub)
	if err != nil {
		return claims, UserSnapshot{}, err
	}
	if snap.Status != store.UserStatusActive {
		return claims, snap, ErrUserNotActive
	}
	if claims.Iat < snap.TokensValidAfter {
		return claims, snap, ErrTokenRevoked
	}
	return claims, snap, nil
}

func (v *TokenVerifier) jwksDocument(ctx context.Context) ([]byte, error) {
	v.mu.RLock()
	if v.jwks != nil && time.Now().Before(v.jwksUntil) {
		data := v.jwks
		v.mu.RUnlock()
		return data, nil
	}
	v.mu.RUnlock()

	data, err := v.fetchJWKS(ctx)
	if err != nil {
		return nil, err
	}

	v.mu.Lock()
	v.jwks, v.jwksUntil = data, time.Now().Add(JWKSCacheTTL)
	v.mu.Unlock()
	return data, nil
}

func (v *TokenVerifier) fetchJWKS(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build jwks request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch jwks: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: jwks endpoint returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("auth: read jwks body: %w", err)
	}
	return body, nil
}

func (v *TokenVerifier) snapshot(ctx context.Context, userID string) (UserSnapshot, error) {
	v.umu.Lock()
	entry, ok := v.cache[userID]
	v.umu.Unlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry, nil
	}

	u, err := v.users.GetUser(ctx, userID)
	if err != nil {
		return UserSnapshot{}, fmt.Errorf("auth: get user %s: %w", userID, err)
	}
	if u == nil {
		return UserSnapshot{}, ErrUserNotFound
	}

	fresh := UserSnapshot{
		Role:             u.Role,
		Status:           u.Status,
		TokensValidAfter: u.TokensValidAfter,
		expiresAt:        time.Now().Add(UserCacheTTL),
	}
	v.umu.Lock()
	v.cache[userID] = fresh
	v.umu.Unlock()
	return fresh, nil
}

// SetJWKSForTest seeds the JWKS cache so a test never reaches the network.
func (v *TokenVerifier) SetJWKSForTest(doc []byte) {
	v.mu.Lock()
	v.jwks, v.jwksUntil = doc, time.Now().Add(JWKSCacheTTL)
	v.mu.Unlock()
}
