package main

import (
	"context"
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
		Sub:     "u1",
		Sid:     "sess-1",
		Surface: "web",
		Scope:   scope,
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

// TestHandleSessionValidTokenPassesGates proves the fixed contract end to
// end at the auth layer: a broker-shaped token (scope=nova, sid bound) with
// its RecordMint slot present clears every pre-upgrade gate. The request is
// deliberately not a WebSocket upgrade, so the handler stops at the upgrade
// step — anything but a 4xx/5xx means auth+session redemption passed.
func TestHandleSessionValidTokenPassesGates(t *testing.T) {
	srv, signer, fake := newAuthTestServer(t)
	seedSessionSlot(fake, "u1", "sess-1", 5*time.Minute)

	tok := mintBridgeToken(t, signer, realtime.NovaScope)
	w := doSession(srv, "/nova/session?sid=sess-1&token="+tok)
	assert.Equal(t, http.StatusOK, w.Code,
		"gates must pass; the (non-upgrade) request only fails at the websocket handshake")
}
