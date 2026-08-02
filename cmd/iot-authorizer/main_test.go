package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	lnsync "github.com/JeremyProffittOrg/live-ninja/internal/sync"
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
	verifier = auth.NewTokenVerifierForAudience(srv.URL, st, auth.AudienceIoT)

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

	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "sess-1", Surface: "web", Did: "dev-1", Aud: auth.AudienceIoT})
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
	// The turn-taking lock is user-scoped like everything else here, and since
	// it moved under presence/ it is covered by the presence grant rather than
	// named by a statement of its own. Pinned through the helper both clients
	// actually publish to, so a change to either side shows up here instead of
	// as claims silently refused in production.
	assert.True(t, grantCovers(arnBase()+":topic/liveninja/user/user-alice/presence/*",
		lnsync.SpeakingTopic("user-alice")),
		"the presence grant must cover the lock topic %q", lnsync.SpeakingTopic("user-alice"))
}

// grantCovers reports whether an IoT policy Resource pattern authorizes a
// publish to topic.
//
// Modelled rather than eyeballed because the rule is easy to get backwards:
// IoT POLICY wildcards are not MQTT wildcards. '*' matches any run of
// characters INCLUDING '/', and '?' matches exactly one — which is why
// presence/* reaches presence/speaking, and equally why a `topic/<uid>/*`
// resource would reach the server-authored doc and memory topics.
func grantCovers(resource, topic string) bool {
	arnPrefix := arnBase() + ":topic/"
	if !strings.HasPrefix(resource, arnPrefix) {
		return false
	}
	pattern := regexp.QuoteMeta(strings.TrimPrefix(resource, arnPrefix))
	pattern = strings.ReplaceAll(pattern, `\*`, ".*")
	pattern = strings.ReplaceAll(pattern, `\?`, ".")
	return regexp.MustCompile("^" + pattern + "$").MatchString(topic)
}

// arnBase rebuilds the ARN prefix policyFor builds, from whatever
// setupIoTAuthorizer put in the environment, so a change to either follows
// through instead of silently making the assertions below vacuous.
func arnBase() string {
	return fmt.Sprintf("arn:aws:iot:%s:%s", os.Getenv("AWS_REGION"), os.Getenv("AWS_ACCOUNT_ID"))
}

// TestPublishIsScopedToThePresencePrefix: doc/memory events are
// SERVER-authored. A client able to publish them could make every other device
// of that user announce a change that never happened. Clients own exactly one
// prefix — presence/*, which carries both their own presence slot and the
// shared turn-taking lock at presence/speaking — and this test's job is to keep
// that set CLOSED, so it is an exact allowlist rather than a substring check
// that a widened resource would still satisfy.
func TestPublishIsScopedToThePresencePrefix(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	activeUser(t, st, "user-alice")
	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web", Aud: auth.AudienceIoT})
	require.NoError(t, err)

	resp, err := handler(context.Background(), mqttRequest(token, "web-1"))
	require.NoError(t, err)

	// Action is decoded as a string on purpose: it also pins that no statement
	// collapses into an ["iot:Publish","iot:RetainPublish"] array, which would
	// hide a resource from the allowlist below.
	var doc struct {
		Statement []struct {
			Action   string `json:"Action"`
			Resource string `json:"Resource"`
		} `json:"Statement"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.PolicyDocuments[0]), &doc))

	base := arnBase()
	var publish, retain []string
	for _, st := range doc.Statement {
		switch st.Action {
		case "iot:Publish":
			publish = append(publish, st.Resource)
		case "iot:RetainPublish":
			retain = append(retain, st.Resource)
		}
	}

	// One grant, not two. The lock used to have a literal statement of its own;
	// since it moved under presence/ that statement was a strict subset of this
	// one, and a redundant grant reads as a boundary that is no longer there.
	require.Equal(t, []string{base + ":topic/liveninja/user/user-alice/presence/*"}, publish)

	// ...and this is the assertion that keeps the single grant honest: the lock
	// topic both clients publish to must actually fall inside it. Without this,
	// merging the statements could quietly stop covering the lock and every
	// claim would be refused in production — which AWS signals by closing the
	// socket, not by erroring.
	assert.True(t, grantCovers(publish[0], lnsync.SpeakingTopic("user-alice")))
	assert.True(t, grantCovers(publish[0], lnsync.PresenceTopic("user-alice", "web-1")))

	// Retained publish is scoped to the same prefix. AWS IoT refuses a RETAIN=1
	// publish without this action and refuses it SILENTLY — an empty roster, not
	// an error — which is why it is asserted rather than left to be noticed.
	//
	// This does mean a client can technically retain a lock claim, which clients
	// deliberately never do (a retained claim would outlive the crashed holder
	// its 30s expiry exists to survive). Accepted: expiry is armed locally by
	// each reader, so a retained claim costs a connecting device one quiet 30s
	// window and nothing more — see the package doc.
	assert.Equal(t, []string{base + ":topic/liveninja/user/user-alice/presence/*"}, retain)

	// The forgery boundary. The exact list above already fixes the set; these
	// state the invariant it is exact FOR, so a future edit that changes the
	// expected list has to argue with them. Note a suffix test for "/*" cannot
	// express this — the legitimate presence resource ends in "/*" too; what
	// must never appear is the bare user subtree, whose wildcard would swallow
	// the server-authored topics because IoT policy '*' spans '/'.
	for _, r := range append(append([]string{}, publish...), retain...) {
		assert.NotEqual(t, base+":topic/liveninja/user/user-alice/*", r,
			"publish must never be a bare subtree wildcard")
		for _, forged := range []string{
			lnsync.UserEventTopic("user-alice", lnsync.EventDoc),
			lnsync.UserEventTopic("user-alice", lnsync.EventMemory),
		} {
			assert.False(t, grantCovers(r, forged),
				"grant %q must not reach the server-authored topic %q", r, forged)
		}
	}
}

// TestDenials: every refusal path returns the same empty-policy shape, so a
// denied connection can do nothing even if IoT kept it.
func TestDenials(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	activeUser(t, st, "user-alice")
	good, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web", Aud: auth.AudienceIoT})
	require.NoError(t, err)

	cases := []struct {
		name string
		req  Request
	}{
		{"no credential at all", mqttRequest("", "web-1")},
		{"garbage token", mqttRequest("not-a-jwt", "web-1")},
		{"unknown subject", func() Request {
			tok, e := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-ghost", Sid: "s", Surface: "web", Aud: auth.AudienceIoT})
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

	token, err := signer.SignAccessToken(ctx, auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web", Aud: auth.AudienceIoT})
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
	token, err := signer.SignAccessToken(ctx, auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web", Aud: auth.AudienceIoT})
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
	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web", Aud: auth.AudienceIoT})
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
	token, err := signer.SignAccessToken(context.Background(), auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web", Aud: auth.AudienceIoT})
	require.NoError(t, err)

	resp, err := handler(context.Background(), mqttRequest(token, "web-1"))
	require.NoError(t, err)
	assert.Equal(t, 3600, resp.DisconnectAfterInSeconds)
	assert.Equal(t, 300, resp.RefreshAfterInSeconds)
}

// TestApiAudienceIsRejected is the other half of the audience split. A full API
// access token must NOT open an MQTT connection: if it did, the narrow token
// would be pointless and any leaked session credential would reach the event
// stream too. cmd/authorizer's mirror of this test refuses the IoT audience.
func TestApiAudienceIsRejected(t *testing.T) {
	signer, st := setupIoTAuthorizer(t)
	activeUser(t, st, "user-alice")

	// Default audience = auth.Audience, i.e. an ordinary API access token.
	apiToken, err := signer.SignAccessToken(context.Background(),
		auth.Claims{Sub: "user-alice", Sid: "s", Surface: "web"})
	require.NoError(t, err)

	resp, err := handler(context.Background(), mqttRequest(apiToken, "web-1"))
	require.NoError(t, err)
	assert.False(t, resp.IsAuthenticated, "an API token must not authorize MQTT")
	assert.Empty(t, resp.PolicyDocuments)
}
