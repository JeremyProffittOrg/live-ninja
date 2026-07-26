package main

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/auth"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

const testKMSKeyARN = "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555"

// newAuthTestServer builds a server whose JWKS provider is pinned to a fake
// KMS signer's key set and whose gate runs against an in-memory DynamoDB —
// enough to exercise handleSession's pre-upgrade auth/session gates.
func newAuthTestServer(t *testing.T) (*server, *auth.Signer, *testutil.FakeDynamo) {
	t.Helper()
	fakeKMS, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	signer := auth.NewSignerWithClient(fakeKMS, testKMSKeyARN)
	jwks, err := signer.JWKS(context.Background())
	require.NoError(t, err)

	fakeDDB := testutil.NewFakeDynamo()
	fakeDDB.SeedItem(map[string]ddbtypes.AttributeValue{
		"pk":     &ddbtypes.AttributeValueMemberS{Value: "USER#u1"},
		"sk":     &ddbtypes.AttributeValueMemberS{Value: "PROFILE"},
		"status": &ddbtypes.AttributeValueMemberS{Value: "active"},
	})
	return &server{
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		gate: realtime.NewGate(fakeDDB, "live-ninja-test"),
		jwks: newJWKSProvider(nil, "", string(jwks)),
	}, signer, fakeDDB
}

// seedSessionSlot writes the BUCKET#sess#<sid> concurrency slot RecordMint
// would have recorded at mint time, expiring expIn from now.
func seedSessionSlot(fake *testutil.FakeDynamo, userID, sessionID string, expIn time.Duration) {
	fake.SeedItem(map[string]ddbtypes.AttributeValue{
		"pk":  &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
		"sk":  &ddbtypes.AttributeValueMemberS{Value: "BUCKET#sess#" + sessionID},
		"exp": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(expIn).Unix(), 10)},
	})
}

func mintBridgeToken(t *testing.T, signer *auth.Signer, scope string) string {
	t.Helper()
	tok, err := signer.SignAccessToken(context.Background(), auth.Claims{
		Sub:       "u1",
		Sid:       "sess-1",
		Surface:   "web",
		Scope:     scope,
		ConfigSHA: "signed-config-digest",
	})
	require.NoError(t, err)
	return tok
}

func doSession(srv *server, url string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	srv.handleSession(w, req)
	return w
}

func doWebSocketSession(srv *server, url string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	req.Header.Set("Sec-WebSocket-Version", "13")
	w := httptest.NewRecorder()
	srv.handleSession(w, req)
	return w
}

func TestHandleSessionRejectsMissingToken(t *testing.T) {
	srv, _, _ := newAuthTestServer(t)
	w := doSession(srv, "/nova/session")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "missing session token")
}

func TestHandleSessionRejectsGarbageToken(t *testing.T) {
	srv, _, _ := newAuthTestServer(t)
	w := doSession(srv, "/nova/session?token=not.a.jwt")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid session token")
}

func TestHandleSessionRejectsWrongScope(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", 5*time.Minute)

	// A perfectly valid first-party session JWT WITHOUT scope=nova (e.g. the
	// browser's ordinary web session token) must not open the bridge.
	tok := mintBridgeToken(t, signer, "")
	w := doSession(srv, "/nova/session?token="+tok)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "not scoped")
}

func TestHandleSessionRejectsNovaTokenWithoutConfigDigest(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", 5*time.Minute)
	tok, err := signer.SignAccessToken(context.Background(), auth.Claims{
		Sub: "u1", Sid: "sess-1", Surface: "web", Scope: realtime.NovaScope,
	})
	require.NoError(t, err)

	w := doSession(srv, "/nova/session?token="+tok)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "missing nova session config")
}

func TestHandleSessionRejectsUnredeemedSession(t *testing.T) {
	srv, signer, _ := newAuthTestServer(t)

	// scope=nova but no RecordMint slot: the broker never minted sess-1.
	tok := mintBridgeToken(t, signer, realtime.NovaScope)
	w := doSession(srv, "/nova/session?token="+tok)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unknown or expired session")
}

func TestHandleSessionRejectsExpiredSessionSlot(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", -time.Minute) // past the hard cap

	tok := mintBridgeToken(t, signer, realtime.NovaScope)
	w := doSession(srv, "/nova/session?token="+tok)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unknown or expired session")
}

func TestHandleSessionRejectsSuspendedAccount(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", 5*time.Minute)
	fake.SeedItem(map[string]ddbtypes.AttributeValue{
		"pk":     &ddbtypes.AttributeValueMemberS{Value: "USER#u1"},
		"sk":     &ddbtypes.AttributeValueMemberS{Value: "PROFILE"},
		"status": &ddbtypes.AttributeValueMemberS{Value: "suspended"},
	})

	tok := mintBridgeToken(t, signer, realtime.NovaScope)
	w := doSession(srv, "/nova/session?token="+tok)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "suspended")
}

func TestHandleSessionRejectsDisabledAccount(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", 5*time.Minute)
	fake.SeedItem(map[string]ddbtypes.AttributeValue{
		"pk":     &ddbtypes.AttributeValueMemberS{Value: "USER#u1"},
		"sk":     &ddbtypes.AttributeValueMemberS{Value: "PROFILE"},
		"status": &ddbtypes.AttributeValueMemberS{Value: "disabled"},
	})

	tok := mintBridgeToken(t, signer, realtime.NovaScope)
	w := doSession(srv, "/nova/session?token="+tok)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "account inactive")
}

func TestHandleSessionRejectsTokenPredatingTokensValidAfter(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", 5*time.Minute)
	issuedAt := time.Now().Add(-time.Minute).Unix()
	fake.SeedItem(map[string]ddbtypes.AttributeValue{
		"pk":               &ddbtypes.AttributeValueMemberS{Value: "USER#u1"},
		"sk":               &ddbtypes.AttributeValueMemberS{Value: "PROFILE"},
		"status":           &ddbtypes.AttributeValueMemberS{Value: "active"},
		"tokensValidAfter": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(issuedAt+1, 10)},
	})
	tok, err := signer.SignAccessToken(context.Background(), auth.Claims{
		Sub:       "u1",
		Sid:       "sess-1",
		Surface:   "web",
		Scope:     realtime.NovaScope,
		ConfigSHA: "signed-config-digest",
		Iat:       issuedAt,
		Exp:       time.Now().Add(5 * time.Minute).Unix(),
	})
	require.NoError(t, err)

	w := doSession(srv, "/nova/session?token="+tok)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "session token invalidated")
}

func TestHandleSessionNonUpgradeDoesNotRedeemToken(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", 5*time.Minute)

	tok := mintBridgeToken(t, signer, realtime.NovaScope)
	w := doSession(srv, "/nova/session?sid=sess-1&token="+tok)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "websocket upgrade required")
	require.NoError(t, srv.gate.RedeemSession(context.Background(), "u1", "sess-1"),
		"a malformed request must not burn the single-use token")
}

func TestHandleSessionUpgradeFailureDoesNotRedeemToken(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", 5*time.Minute)

	tok := mintBridgeToken(t, signer, realtime.NovaScope)
	_ = doWebSocketSession(srv, "/nova/session?token="+tok)
	require.NoError(t, srv.gate.RedeemSession(context.Background(), "u1", "sess-1"),
		"a failed HTTP hijack/upgrade must not burn the single-use token")
}

func TestUpgradedReplayGetsTypedError(t *testing.T) {
	srv, _, _ := newAuthTestServer(t)
	client := newFakeClient(nil)

	srv.writeUpgradedSessionError(client, &realtime.SessionRedeemedError{}, "u1", "sess-1")

	select {
	case ev := <-client.out:
		assert.Equal(t, voiceengine.TypeError, ev.Type)
		assert.Equal(t, "session_redeemed", ev.Code)
		assert.Contains(t, ev.Message, "already redeemed")
	default:
		t.Fatal("expected an in-band replay error")
	}
}
