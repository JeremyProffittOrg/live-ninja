// Package auth implements Live Ninja's identity layer: Login with Amazon
// (this file), first-party session/JWT signing (session.go), rotating
// refresh tokens (refresh.go), the 10-year device pairing lineage
// (device.go), and access control (access.go).
//
// This file owns the LWA (Login with Amazon) OAuth client: building the
// Authorization Code + PKCE authorize URL, exchanging a code for tokens,
// and the mandatory two-check validation of an access token
// (tokeninfo aud check + profile fetch) that yields the canonical
// Amazon `user_id` subject used everywhere else in the system.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
)

// LWA endpoints. Fixed by Amazon's Login with Amazon API — not
// configurable per-environment.
const (
	lwaAuthorizeURL = "https://www.amazon.com/ap/oa"
	lwaTokenURL     = "https://api.amazon.com/auth/o2/token"
	lwaTokenInfoURL = "https://api.amazon.com/auth/o2/tokeninfo"
	lwaProfileURL   = "https://api.amazon.com/user/profile"

	// lwaScope is the only scope Live Ninja requests: the minimal
	// profile scope needed to resolve user_id/email/name.
	lwaScope = "profile"

	// httpTimeout bounds every outbound call to Amazon's endpoints so a
	// slow/hanging LWA response can never wedge a Lambda invocation past
	// its own budget.
	httpTimeout = 10 * time.Second
)

// Errors returned by Validate. Deliberately distinct from a generic
// wrapped HTTP error so callers (access.go, the /auth/lwa/callback and
// /auth/lwa/exchange handlers) can distinguish "Amazon rejected/doesn't
// recognize this token" from a transient network failure.
var (
	// ErrTokenInfoFailed means the tokeninfo endpoint did not return a
	// usable 200 response for the presented access token.
	ErrTokenInfoFailed = errors.New("auth: lwa tokeninfo request failed")
	// ErrAudienceMismatch means tokeninfo succeeded but its `aud` did not
	// match this app's client_id — i.e. the token was minted for a
	// different application. This is the confused-deputy guard that
	// makes the two-check validation mandatory rather than trusting a
	// bare access token.
	ErrAudienceMismatch = errors.New("auth: lwa token audience mismatch")
	// ErrProfileFailed means the /user/profile call did not return a
	// usable 200 response (or was missing user_id) for a token that
	// passed the tokeninfo check.
	ErrProfileFailed = errors.New("auth: lwa profile request failed")
)

// LWATokens is the response body of a successful `/auth/o2/token`
// exchange (authorization_code grant).
type LWATokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// LWAProfile is the resolved identity of an LWA end user, assembled from
// the two-check Validate flow. UserID is Amazon's stable `user_id` — the
// canonical subject everywhere else in Live Ninja (USER.amazonUserId,
// OWNER.amazonUserId, ALLOW key).
type LWAProfile struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
}

// httpDoer is the subset of *http.Client LWAClient depends on, so tests
// can inject a fake transport without a real network call.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// LWAClient is a Login with Amazon OAuth client bound to this app's
// client_id/client_secret (read once from SSM at construction — see
// NewLWAClient). It never logs the client secret, an access/refresh
// token, or a raw tokeninfo/profile response body.
type LWAClient struct {
	clientID     string
	clientSecret string
	httpClient   httpDoer
}

// NewLWAClient resolves this app's LWA client_id/client_secret via the
// shared SSM-backed config.Loader (config.ParamLWAClientID /
// config.ParamLWAClientSecret, with the LWA_CLIENT_ID/LWA_CLIENT_SECRET
// local-dev env overrides baked into the loader itself) and returns a
// ready-to-use client. Resolution happens once, eagerly, at construction
// time — BuildAuthorizeURL below is a pure string function with no
// context argument, so the client_id must already be in hand before it
// can be called.
func NewLWAClient(ctx context.Context, loader *config.Loader) (*LWAClient, error) {
	if loader == nil {
		return nil, errors.New("auth: config loader is required")
	}

	clientID, err := loader.Get(ctx, config.ParamLWAClientID, config.EnvOverrideLWAClientID)
	if err != nil {
		return nil, fmt.Errorf("auth: resolve lwa client_id: %w", err)
	}
	clientSecret, err := loader.Get(ctx, config.ParamLWAClientSecret, config.EnvOverrideLWAClientSecret)
	if err != nil {
		return nil, fmt.Errorf("auth: resolve lwa client_secret: %w", err)
	}

	return &LWAClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: httpTimeout},
	}, nil
}

// newLWAClientForTest builds an LWAClient with an injected httpDoer and
// fixed credentials, bypassing SSM resolution. Unexported — for this
// package's own tests only.
func newLWAClientForTest(clientID, clientSecret string, doer httpDoer) *LWAClient {
	return &LWAClient{clientID: clientID, clientSecret: clientSecret, httpClient: doer}
}

// BuildAuthorizeURL builds the `https://www.amazon.com/ap/oa` Authorization
// Code + PKCE redirect URL. state and codeChallenge (S256 over a
// caller-generated code_verifier) are generated and persisted by the
// caller (the OAUTHSTATE item, per the shared spec) — this function only
// assembles the URL.
func (c *LWAClient) BuildAuthorizeURL(state, codeChallenge, redirectURI string) string {
	v := url.Values{}
	v.Set("client_id", c.clientID)
	v.Set("scope", lwaScope)
	v.Set("response_type", "code")
	v.Set("redirect_uri", redirectURI)
	v.Set("state", state)
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")
	return lwaAuthorizeURL + "?" + v.Encode()
}

// ExchangeCode exchanges an authorization code for LWA tokens via
// `POST https://api.amazon.com/auth/o2/token` (authorization_code grant),
// presenting both the PKCE code_verifier and this app's confidential
// client_secret (LWA's web/BFF flow uses both belt-and-suspenders per the
// shared spec).
func (c *LWAClient) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI string) (*LWATokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lwaTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: build lwa token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: lwa token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read lwa token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Never log body: on a failed exchange it may still echo back
		// fragments of the request (client_secret, code_verifier). Log
		// only the status.
		return nil, fmt.Errorf("auth: lwa token exchange failed: status %d", resp.StatusCode)
	}

	var tokens LWATokens
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("auth: decode lwa token response: %w", err)
	}
	if tokens.AccessToken == "" {
		return nil, errors.New("auth: lwa token response missing access_token")
	}
	return &tokens, nil
}

// tokenInfoResponse is the body of a successful
// `GET /auth/o2/tokeninfo?access_token=` call.
type tokenInfoResponse struct {
	Aud    string `json:"aud"`
	UserID string `json:"user_id"`
	AppID  string `json:"app_id"`
	Exp    string `json:"exp"`
}

// Validate runs the mandatory two-check validation of an LWA access
// token: (1) GET /auth/o2/tokeninfo and confirm `aud == client_id` (a
// bare access token is not proof it was minted for THIS app — omitting
// this check is a confused-deputy vulnerability: a token minted for any
// other Amazon app would otherwise be accepted); then (2) GET
// /user/profile with the token to resolve user_id/email/name. Both legs
// must succeed for Validate to return a profile.
func (c *LWAClient) Validate(ctx context.Context, accessToken string) (*LWAProfile, error) {
	if accessToken == "" {
		return nil, errors.New("auth: access token is required")
	}

	// Check 1: tokeninfo aud match.
	tiReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		lwaTokenInfoURL+"?access_token="+url.QueryEscape(accessToken), nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build tokeninfo request: %w", err)
	}
	tiResp, err := c.httpClient.Do(tiReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenInfoFailed, err)
	}
	defer tiResp.Body.Close()

	tiBody, err := io.ReadAll(io.LimitReader(tiResp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read tokeninfo response: %w", err)
	}
	if tiResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrTokenInfoFailed, tiResp.StatusCode)
	}

	var ti tokenInfoResponse
	if err := json.Unmarshal(tiBody, &ti); err != nil {
		return nil, fmt.Errorf("auth: decode tokeninfo response: %w", err)
	}
	if ti.Aud == "" || ti.Aud != c.clientID {
		return nil, ErrAudienceMismatch
	}

	// Check 2: profile fetch.
	pReq, err := http.NewRequestWithContext(ctx, http.MethodGet, lwaProfileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build profile request: %w", err)
	}
	pReq.Header.Set("Authorization", "bearer "+accessToken)
	pReq.Header.Set("Accept", "application/json")

	pResp, err := c.httpClient.Do(pReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProfileFailed, err)
	}
	defer pResp.Body.Close()

	pBody, err := io.ReadAll(io.LimitReader(pResp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read profile response: %w", err)
	}
	if pResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrProfileFailed, pResp.StatusCode)
	}

	var profile LWAProfile
	if err := json.Unmarshal(pBody, &profile); err != nil {
		return nil, fmt.Errorf("auth: decode profile response: %w", err)
	}
	if profile.UserID == "" {
		return nil, fmt.Errorf("%w: missing user_id", ErrProfileFailed)
	}
	// Defense in depth: the two calls must agree on user_id.
	if ti.UserID != "" && ti.UserID != profile.UserID {
		return nil, fmt.Errorf("%w: tokeninfo/profile user_id mismatch", ErrProfileFailed)
	}

	return &profile, nil
}
