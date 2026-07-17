package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func TestIsPublicRoutePathTable(t *testing.T) {
	cases := []struct {
		path   string
		public bool
	}{
		// Public exact.
		{"/", true},
		{"/healthz", true},
		{"/v1/app/android/latest", true},
		{"/v1/compat", true},
		{"/api/v1/auth/lwa/exchange", true},
		{"/api/v1/auth/refresh", true},
		{"/api/v1/auth/device/register", true},
		{"/api/v1/auth/device/poll", true},
		// Public prefixes.
		{"/static/app.css", true},
		{"/auth/lwa/login", true},
		{"/auth/lwa/callback", true},
		{"/auth/device/pair/start", true},
		{"/.well-known/jwks.json", true},
		// Protected.
		{"/api/v1/tools/invoke", false},
		{"/api/v1/realtime/session", false},
		{"/api/v1/auth/logout", false},
		{"/api/v1/auth/logout-all", false},
		{"/api/v1/devices", false},
		{"/healthzX", false},
		{"/staticX", false},
		{"/api/v1/auth/lwa/exchangeX", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.public, isPublicRoute(tc.path), "path %s", tc.path)
	}
}

func TestExtractBearerToken(t *testing.T) {
	assert.Equal(t, "tok", extractBearerToken(map[string]string{"authorization": "Bearer tok"}))
	assert.Equal(t, "tok", extractBearerToken(map[string]string{"Authorization": "bearer tok"}))
	assert.Equal(t, "tok", extractBearerToken(map[string]string{"AUTHORIZATION": "BEARER tok"}))
	// Raw token without scheme is passed through trimmed.
	assert.Equal(t, "tok", extractBearerToken(map[string]string{"authorization": " tok "}))
	assert.Equal(t, "", extractBearerToken(map[string]string{}))
	assert.Equal(t, "", extractBearerToken(map[string]string{"authorization": ""}))
}

// authorizerRequest builds a minimal API GW v2 authorizer request.
func authorizerRequest(method, path, token string) events.APIGatewayV2CustomAuthorizerV2Request {
	req := events.APIGatewayV2CustomAuthorizerV2Request{
		RawPath: path,
		Headers: map[string]string{},
	}
	req.RequestContext.HTTP.Method = method
	req.RequestContext.RequestID = "req-test"
	if token != "" {
		req.Headers["authorization"] = "Bearer " + token
	}
	return req
}

// setupAuthorizer wires the package globals (jwksURL, st, users, jwks
// caches) to a local JWKS httptest server + fake store, returning the
// signer whose key that JWKS matches. Cleanup restores nothing — each
// test calls this to get a fresh world (the globals are only ever used
// through handler()).
func setupAuthorizer(t *testing.T) (*auth.Signer, *store.Store) {
	t.Helper()

	fakeKMS, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	signer := auth.NewSignerWithClient(fakeKMS, "arn:aws:kms:us-east-1:1:key/test-key")

	jwksJSON, err := signer.JWKS(context.Background())
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(jwksJSON)
	}))
	t.Cleanup(srv.Close)

	fakeDDB := testutil.NewFakeDynamo()
	st = store.NewWithClient(fakeDDB, "live-ninja-test")
	jwksURL = srv.URL
	jwks = &jwksCache{}    // reset the 24h JWKS cache between tests
	users = newUserCache() // reset the 60s user cache between tests
	return signer, st
}

func TestHandlerAllowsOptionsPreflight(t *testing.T) {
	setupAuthorizer(t)
	resp, err := handler(context.Background(), authorizerRequest(http.MethodOptions, "/api/v1/tools/invoke", ""))
	require.NoError(t, err)
	assert.True(t, resp.IsAuthorized)
}

func TestHandlerAllowsPublicRoutes(t *testing.T) {
	setupAuthorizer(t)
	for _, path := range []string{"/healthz", "/auth/lwa/login", "/.well-known/jwks.json"} {
		resp, err := handler(context.Background(), authorizerRequest(http.MethodGet, path, ""))
		require.NoError(t, err)
		assert.True(t, resp.IsAuthorized, "path %s", path)
		assert.Equal(t, "public", resp.Context["surface"])
	}
}

func TestHandlerDeniesMissingToken(t *testing.T) {
	setupAuthorizer(t)
	resp, err := handler(context.Background(), authorizerRequest(http.MethodPost, "/api/v1/tools/invoke", ""))
	require.NoError(t, err)
	assert.False(t, resp.IsAuthorized)
}

func TestHandlerDeniesBadJWT(t *testing.T) {
	setupAuthorizer(t)
	for _, tok := range []string{
		"garbage",
		"a.b.c",
		"eyJhbGciOiJub25lIn0.e30.", // alg:none
	} {
		resp, err := handler(context.Background(), authorizerRequest(http.MethodGet, "/api/v1/realtime/session", tok))
		require.NoError(t, err)
		assert.False(t, resp.IsAuthorized, "token %q must be denied", tok)
	}
}

func TestHandlerDeniesForgedSignature(t *testing.T) {
	setupAuthorizer(t) // JWKS for key A

	// Token signed with a DIFFERENT key but the same kid.
	otherKMS, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	forger := auth.NewSignerWithClient(otherKMS, "arn:aws:kms:us-east-1:1:key/test-key")
	token, err := forger.SignAccessToken(context.Background(), auth.Claims{Sub: "victim", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(context.Background(), authorizerRequest(http.MethodGet, "/api/v1/realtime/session", token))
	require.NoError(t, err)
	assert.False(t, resp.IsAuthorized)
}

func TestHandlerAllowsValidJWTAndInjectsContext(t *testing.T) {
	signer, testStore := setupAuthorizer(t)
	ctx := context.Background()

	require.NoError(t, testStore.CreateUser(ctx, &store.User{
		UserID:       "uid-1",
		AmazonUserID: "amzn1.account.a",
		Role:         store.RoleOwner,
		Status:       store.UserStatusActive,
	}))

	token, err := signer.SignAccessToken(ctx, auth.Claims{
		Sub: "uid-1", Sid: "sess-1", Surface: "web",
	})
	require.NoError(t, err)

	resp, err := handler(ctx, authorizerRequest(http.MethodGet, "/api/v1/realtime/session", token))
	require.NoError(t, err)
	require.True(t, resp.IsAuthorized)
	assert.Equal(t, "uid-1", resp.Context["userId"])
	assert.Equal(t, "sess-1", resp.Context["sessionId"])
	assert.Equal(t, "web", resp.Context["surface"])
	assert.Equal(t, store.RoleOwner, resp.Context["role"])
}

func TestHandlerDeniesUnknownSubject(t *testing.T) {
	signer, _ := setupAuthorizer(t)
	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "uid-ghost", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(context.Background(), authorizerRequest(http.MethodGet, "/api/v1/devices", token))
	require.NoError(t, err)
	assert.False(t, resp.IsAuthorized)
}

func TestHandlerTokensValidAfterKillSwitch(t *testing.T) {
	signer, testStore := setupAuthorizer(t)
	ctx := context.Background()

	// Token minted 5 minutes ago; user then hit "log out everywhere" NOW.
	iat := time.Now().Add(-5 * time.Minute).Unix()
	require.NoError(t, testStore.CreateUser(ctx, &store.User{
		UserID:       "uid-1",
		AmazonUserID: "amzn1.account.a",
		Role:         store.RoleOwner,
		Status:       store.UserStatusActive,
	}))
	require.NoError(t, testStore.SetTokensValidAfter(ctx, "uid-1", time.Now().Unix()))

	token, err := signer.SignAccessToken(ctx, auth.Claims{
		Sub: "uid-1", Sid: "sess-1", Surface: "web",
		Iat: iat, Exp: iat + 900,
	})
	require.NoError(t, err)

	resp, err := handler(ctx, authorizerRequest(http.MethodGet, "/api/v1/devices", token))
	require.NoError(t, err)
	assert.False(t, resp.IsAuthorized, "iat < tokensValidAfter must be denied")
}

func TestHandlerDeniesDisabledUser(t *testing.T) {
	signer, testStore := setupAuthorizer(t)
	ctx := context.Background()

	require.NoError(t, testStore.CreateUser(ctx, &store.User{
		UserID:       "uid-1",
		AmazonUserID: "amzn1.account.a",
		Role:         store.RoleMember,
		Status:       store.UserStatusDisabled,
	}))
	token, err := signer.SignAccessToken(ctx, auth.Claims{Sub: "uid-1", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(ctx, authorizerRequest(http.MethodGet, "/api/v1/devices", token))
	require.NoError(t, err)
	assert.False(t, resp.IsAuthorized)
}
