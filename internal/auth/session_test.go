package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

const testKeyARN = "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555"

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	fake, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	return NewSignerWithClient(fake, testKeyARN)
}

func TestKeyIDSuffix(t *testing.T) {
	s := newTestSigner(t)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", s.KeyID())
	assert.Equal(t, "bare-key-id", keyIDSuffix("bare-key-id"))
}

func TestSignAccessTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)

	before := time.Now().Unix()
	token, err := s.SignAccessToken(ctx, Claims{
		Sub:     "user-1",
		Sid:     "sess-1",
		Surface: "web",
	})
	require.NoError(t, err)
	after := time.Now().Unix()

	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	claims, err := VerifyJWT(token, jwks)
	require.NoError(t, err)

	assert.Equal(t, Issuer, claims.Iss)
	assert.Equal(t, Audience, claims.Aud)
	assert.Equal(t, "user-1", claims.Sub)
	assert.Equal(t, "sess-1", claims.Sid)
	assert.Equal(t, "web", claims.Surface)
	assert.NotEmpty(t, claims.Jti)

	// iat within the sign window; exp exactly iat + 15 minutes.
	assert.GreaterOrEqual(t, claims.Iat, before)
	assert.LessOrEqual(t, claims.Iat, after)
	assert.Equal(t, claims.Iat+int64(AccessTokenTTL/time.Second), claims.Exp)
}

func TestSignAccessTokenRequiresSub(t *testing.T) {
	s := newTestSigner(t)
	_, err := s.SignAccessToken(context.Background(), Claims{Sid: "sess"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sub")
}

func TestVerifyJWTRejectsTamperedClaims(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)
	token, err := s.SignAccessToken(ctx, Claims{Sub: "user-1", Sid: "s", Surface: "web"})
	require.NoError(t, err)
	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	parts := strings.Split(token, ".")
	var c Claims
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(payload, &c))
	c.Sub = "attacker" // privilege swap
	forged, err := json.Marshal(c)
	require.NoError(t, err)
	parts[1] = base64.RawURLEncoding.EncodeToString(forged)

	_, err = VerifyJWT(strings.Join(parts, "."), jwks)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyJWTRejectsAlgNone(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)
	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := Claims{
		Iss: Issuer, Aud: Audience, Sub: "user-1",
		Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(), Jti: "x",
	}
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."

	_, err = VerifyJWT(token, jwks)
	require.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "alg")
}

func TestVerifyJWTRejectsExpired(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)
	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	// exp two minutes ago is past even the 60s clock-skew leeway.
	iat := time.Now().Add(-20 * time.Minute).Unix()
	token, err := s.SignAccessToken(ctx, Claims{
		Sub: "user-1", Sid: "s", Surface: "web",
		Iat: iat, Exp: time.Now().Add(-2 * time.Minute).Unix(),
	})
	require.NoError(t, err)

	_, err = VerifyJWT(token, jwks)
	require.ErrorIs(t, err, ErrTokenExpired)
}

func TestVerifyJWTAllowsExpiredWithinSkew(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)
	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	// exp 30s ago is inside the 60s leeway.
	token, err := s.SignAccessToken(ctx, Claims{
		Sub: "user-1", Sid: "s", Surface: "web",
		Iat: time.Now().Add(-16 * time.Minute).Unix(),
		Exp: time.Now().Add(-30 * time.Second).Unix(),
	})
	require.NoError(t, err)

	_, err = VerifyJWT(token, jwks)
	require.NoError(t, err)
}

func TestVerifyJWTRejectsFutureIat(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)
	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	iat := time.Now().Add(10 * time.Minute).Unix()
	token, err := s.SignAccessToken(ctx, Claims{
		Sub: "user-1", Sid: "s", Surface: "web",
		Iat: iat, Exp: iat + 900,
	})
	require.NoError(t, err)

	_, err = VerifyJWT(token, jwks)
	require.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "iat")
}

func TestVerifyJWTRejectsWrongAudAndIss(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)
	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	badAud, err := s.SignAccessToken(ctx, Claims{Sub: "u", Aud: "someone-else"})
	require.NoError(t, err)
	_, err = VerifyJWT(badAud, jwks)
	require.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "aud")

	badIss, err := s.SignAccessToken(ctx, Claims{Sub: "u", Iss: "https://evil.example"})
	require.NoError(t, err)
	_, err = VerifyJWT(badIss, jwks)
	require.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "iss")
}

func TestVerifyJWTRejectsForeignKey(t *testing.T) {
	ctx := context.Background()
	signer := newTestSigner(t)
	other := newTestSigner(t) // different key, same kid (same ARN)

	token, err := signer.SignAccessToken(ctx, Claims{Sub: "u", Sid: "s", Surface: "web"})
	require.NoError(t, err)
	otherJWKS, err := other.JWKS(ctx)
	require.NoError(t, err)

	_, err = VerifyJWT(token, otherJWKS)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyJWTRejectsUnknownKid(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)
	other := NewSignerWithClient(mustFakeKMS(t), "arn:aws:kms:us-east-1:1:key/other-kid")

	token, err := other.SignAccessToken(ctx, Claims{Sub: "u"})
	require.NoError(t, err)
	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	_, err = VerifyJWT(token, jwks)
	require.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "kid")
}

func TestVerifyJWTRejectsGarbage(t *testing.T) {
	ctx := context.Background()
	s := newTestSigner(t)
	jwks, err := s.JWKS(ctx)
	require.NoError(t, err)

	for _, tok := range []string{"", "abc", "a.b", "a.b.c.d", "!!!.@@@.###"} {
		_, err := VerifyJWT(tok, jwks)
		assert.ErrorIs(t, err, ErrInvalidToken, "token %q", tok)
	}
}

func TestJWKSDocumentShapeAndCaching(t *testing.T) {
	ctx := context.Background()
	fake := mustFakeKMS(t)
	s := NewSignerWithClient(fake, testKeyARN)

	doc1, err := s.JWKS(ctx)
	require.NoError(t, err)

	var parsed struct {
		Keys []map[string]string `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(doc1, &parsed))
	require.Len(t, parsed.Keys, 1)
	k := parsed.Keys[0]
	assert.Equal(t, "EC", k["kty"])
	assert.Equal(t, "P-256", k["crv"])
	assert.Equal(t, "ES256", k["alg"])
	assert.Equal(t, s.KeyID(), k["kid"])
	assert.NotEmpty(t, k["x"])
	assert.NotEmpty(t, k["y"])

	// Second call is served from cache (same bytes, no error).
	doc2, err := s.JWKS(ctx)
	require.NoError(t, err)
	assert.Equal(t, doc1, doc2)
}

func mustFakeKMS(t *testing.T) *testutil.FakeKMS {
	t.Helper()
	fake, err := testutil.NewFakeKMS()
	require.NoError(t, err)
	return fake
}
