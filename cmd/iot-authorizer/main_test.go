package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// setupIoTAuthorizer wires a real signer + fake DynamoDB behind the shared
// verifier, exactly as cmd/authorizer's harness does.
func setupIoTAuthorizer(t *testing.T) (*auth.Signer, *store.Store) {
	t.Helper()

	fakeKMS, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	signer := auth.NewSignerWithClient(fakeKMS, "arn:aws:kms:us-east-1:1:key/test-key")

	jwksJSON, err := signer.JWKS(context.Background())
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	t.Cleanup(srv.Close)

	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test")
	verifier = auth.NewTokenVerifier(srv.URL, st)

	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCOUNT_ID", "759775734231")
	return signer, st
}

func mqttRequest(username, clientID string) Request {
	var r Request
	r.ProtocolData.MQTT = &struct {
		Username string `json:"username"`
		Password string `json:"password"`
		ClientID string `json:"clientId"`
	}{Username: username, ClientID: clientID}
	return r
}

func activeUser(t *testing.T, st *store.Store, userID string) {
	t.Helper()
	require.NoError(t, st.CreateUser(context.Background(), &store.User{
		UserID: userID, AmazonUserID: "amzn1.account." + userID,
		Role: store.RoleOwner, Status: store.UserStatusActive,
	}))
}

// TestAuthorizesAndScopesPolicyToOneUser is the security test for this whole
// feature: a good token yields a policy that reaches exactly one user's topic
// subtree and no one else's.
func TestAuthorizesAndScopesPolicyToOneUser(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	activeUser(t, st, "user-alice")

	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "sess-1", Surface: "web", Did: "dev-1"})
	require.NoError(t, err)

	resp, err := handler(context.Background(), mqttRequest(token, "web-abc123"))
	require.NoError(t, err)
	require.True(t, resp.IsAuthenticated)
	assert.Equal(t, "user-alice", resp.PrincipalID)
	require.Len(t, resp.PolicyDocuments, 1)

	policy := resp.PolicyDocuments[0]
	assert.True(t, json.Valid([]byte(policy)), "policy must be valid JSON")

	// Reaches alice's own subtree...
	assert.Contains(t, policy, "topicfilter/liveninja/user/user-alice/#")
	assert.Contains(t, policy, "topic/liveninja/user/user-alice/*")
	assert.Contains(t, policy, "client/web-abc123")
	// ...and cannot name anyone else's.
	assert.NotContains(t, policy, "user-bob")
	assert.NotContains(t, policy, "liveninja/user/*")
	assert.NotContains(t, policy, `"Resource":"*"`)
}

// TestPublishIsPresenceOnly: doc/memory events are SERVER-authored. A client
// able to publish them could make every other device of that user announce a
// change that never happened.
func TestPublishIsPresenceOnly(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	activeUser(t, st, "user-alice")
	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(context.Background(), mqttRequest(token, "web-1"))
	require.NoError(t, err)

	var doc struct {
		Statement []struct {
			Action   string `json:"Action"`
			Resource string `json:"Resource"`
		} `json:"Statement"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.PolicyDocuments[0]), &doc))

	var publish []string
	for _, st := range doc.Statement {
		if st.Action == "iot:Publish" {
			publish = append(publish, st.Resource)
		}
	}
	require.Len(t, publish, 1, "exactly one publish grant")
	assert.True(t, strings.HasSuffix(publish[0], "/presence/*"),
		"publish must be presence-only, got %s", publish[0])
}

// TestDenials: every refusal path returns the same empty-policy shape, so a
// denied connection can do nothing even if IoT kept it.
func TestDenials(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	activeUser(t, st, "user-alice")
	good, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	cases := []struct {
		name string
		req  Request
	}{
		{"no credential at all", mqttRequest("", "web-1")},
		{"garbage token", mqttRequest("not-a-jwt", "web-1")},
		{"unknown subject", func() Request {
			tok, e := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-ghost", Sid: "s", Surface: "web"})
			require.NoError(t, e)
			return mqttRequest(tok, "web-1")
		}()},
		// The client id is interpolated into a resource ARN, so a wildcard or a
		// quote in it must never reach the policy.
		{"client id with a wildcard", mqttRequest(good, "web-*")},
		{"client id with a quote", mqttRequest(good, `web","Resource":"*`)},
		{"empty client id", mqttRequest(good, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := handler(context.Background(), tc.req)
			require.NoError(t, err)
			assert.False(t, resp.IsAuthenticated)
			assert.Empty(t, resp.PolicyDocuments, "a denial must carry no policy")
		})
	}
}

// TestRevokedTokenIsDenied: the tokensValidAfter kill-switch has to reach MQTT
// too. This is the case the shared verifier exists for — an MQTT connection is
// authorized once and then held, so a kill-switch that stopped at the HTTP API
// would leave a revoked user with a live subscription.
func TestRevokedTokenIsDenied(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	ctx := context.Background()
	require.NoError(t, st.CreateUser(ctx, &store.User{
		UserID: "user-alice", AmazonUserID: "amzn1.account.a",
		Role: store.RoleOwner, Status: store.UserStatusActive,
		TokensValidAfter: 1 << 40, // far future: every token predates it
	}))

	token, err := signer.SignAccessToken(ctx, auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(ctx, mqttRequest(token, "web-1"))
	require.NoError(t, err)
	assert.False(t, resp.IsAuthenticated)
}

// TestDisabledUserIsDenied guards the other live check a signature cannot make.
func TestDisabledUserIsDenied(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	ctx := context.Background()
	require.NoError(t, st.CreateUser(ctx, &store.User{
		UserID: "user-alice", AmazonUserID: "amzn1.account.a",
		Role: store.RoleOwner, Status: "disabled",
	}))
	token, err := signer.SignAccessToken(ctx, auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(ctx, mqttRequest(token, "web-1"))
	require.NoError(t, err)
	assert.False(t, resp.IsAuthenticated)
}

// TestUsernameQuerySuffixIsStripped: some MQTT SDKs append
// `?x-amz-customauthorizer-name=...` to the CONNECT user name. The JWT must
// still be recovered from it.
func TestUsernameQuerySuffixIsStripped(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	activeUser(t, st, "user-alice")
	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(context.Background(),
		mqttRequest(token+"?x-amz-customauthorizer-name=live-ninja-iot", "web-1"))
	require.NoError(t, err)
	assert.True(t, resp.IsAuthenticated)
}

// TestConnectionBoundsAreSet: the access JWT lives 15 minutes but the socket
// must not, and the refresh interval is what bounds revocation on a live one.
func TestConnectionBoundsAreSet(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	activeUser(t, st, "user-alice")
	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(context.Background(), mqttRequest(token, "web-1"))
	require.NoError(t, err)
	assert.Equal(t, 3600, resp.DisconnectAfterInSeconds)
	assert.Equal(t, 300, resp.RefreshAfterInSeconds)
}
