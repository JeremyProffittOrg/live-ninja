package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newFakeStore() *store.Store {
	return store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test")
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestGenerateRefreshTokenAndHash(t *testing.T) {
	plain, hash, err := GenerateRefreshToken()
	require.NoError(t, err)
	assert.NotEmpty(t, plain)
	assert.Equal(t, HashRefreshToken(plain), hash)
	assert.Len(t, hash, 64) // hex sha-256
	assert.NotEqual(t, plain, hash)

	plain2, hash2, err := GenerateRefreshToken()
	require.NoError(t, err)
	assert.NotEqual(t, plain, plain2)
	assert.NotEqual(t, hash, hash2)
}

func TestVerifyPKCE(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := s256(verifier)

	assert.True(t, verifyPKCE(verifier, challenge))
	assert.False(t, verifyPKCE(verifier+"x", challenge))
	assert.False(t, verifyPKCE("", challenge))
	assert.False(t, verifyPKCE(verifier, "not-the-challenge"))
	// The verifier itself is never a valid challenge (must be S256'd).
	assert.False(t, verifyPKCE(verifier, verifier))
}

func TestDevicePairingFullLineage(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	log := slog.Default()

	verifier := "device-on-chip-code-verifier-0123456789abcdef"
	challenge := s256(verifier)

	// 1. Device registers pairing with its S256 challenge.
	nonce, expiresAt, err := RegisterPairing(ctx, st, challenge)
	require.NoError(t, err)
	assert.NotEmpty(t, nonce)
	assert.False(t, expiresAt.IsZero())

	// Claim before the browser leg -> not bound yet.
	_, err = ClaimPairing(ctx, st, nonce, verifier)
	require.ErrorIs(t, err, ErrPairNotBound)

	// 2. Browser leg: bind to the (first, owner-binding) profile.
	profile := &LWAProfile{UserID: "amzn1.account.owner", Email: "Owner@Example.com", Name: "Owner"}
	require.NoError(t, BindPairing(ctx, st, log, nonce, "Kitchen Tab5", profile))

	// 3. Device polls and sees "bound".
	pair, err := PollPairing(ctx, st, nonce)
	require.NoError(t, err)
	assert.Equal(t, store.PairStatusBound, pair.Status)
	assert.NotEmpty(t, pair.DeviceID)
	assert.NotEmpty(t, pair.UserID)

	// 4a. Claim with the WRONG verifier -> PKCE mismatch, nothing minted.
	_, err = ClaimPairing(ctx, st, nonce, "wrong-verifier")
	require.ErrorIs(t, err, ErrPKCEMismatch)

	// 4b. Claim with the right verifier -> one-shot credential.
	claim, err := ClaimPairing(ctx, st, nonce, verifier)
	require.NoError(t, err)
	assert.Equal(t, pair.DeviceID, claim.DeviceID)
	assert.NotEmpty(t, claim.RefreshToken)
	assert.NotEmpty(t, claim.SessionID)
	assert.NotEmpty(t, claim.FamilyID)

	// The stored session holds only the hash, on the device surface.
	sess, err := st.GetSessionByID(ctx, claim.SessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, HashRefreshToken(claim.RefreshToken), sess.RefreshHash)
	assert.Equal(t, store.SurfaceDevice, sess.Surface)
	assert.Equal(t, claim.FamilyID, sess.FamilyID)

	// 5. Replayed claim fails closed — no second credential.
	_, err = ClaimPairing(ctx, st, nonce, verifier)
	require.ErrorIs(t, err, ErrPairAlreadyClaimed)

	// A second bind attempt on the same nonce also fails closed.
	err = BindPairing(ctx, st, log, nonce, "Evil Rebind", profile)
	require.ErrorIs(t, err, ErrPairAlreadyClaimed)
}

func TestClaimPairingUnknownNonce(t *testing.T) {
	st := newFakeStore()
	_, err := ClaimPairing(context.Background(), st, "no-such-nonce", "verifier")
	require.ErrorIs(t, err, ErrPairNotFound)

	_, err = PollPairing(context.Background(), st, "no-such-nonce")
	require.ErrorIs(t, err, ErrPairNotFound)
}

func TestBindPairingRejectsUnauthorizedUser(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	log := slog.Default()

	// Bind the owner first via a normal sign-in.
	owner := &LWAProfile{UserID: "amzn1.account.owner", Email: "owner@example.com"}
	_, err := Authorize(ctx, st, owner)
	require.NoError(t, err)

	nonce, _, err := RegisterPairing(ctx, st, s256("some-verifier"))
	require.NoError(t, err)

	// A stranger (not owner, not allowlisted) cannot bind a device.
	stranger := &LWAProfile{UserID: "amzn1.account.stranger", Email: "stranger@example.com"}
	err = BindPairing(ctx, st, log, nonce, "Rogue Device", stranger)
	require.ErrorIs(t, err, ErrNotAllowed)

	// Pair row must still be pending (claimable by a legitimate bind).
	pair, err := PollPairing(ctx, st, nonce)
	require.NoError(t, err)
	assert.Equal(t, store.PairStatusPending, pair.Status)
}

func TestRotateDeviceSession(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	plain, hash, err := GenerateRefreshToken()
	require.NoError(t, err)
	sess := &store.Session{
		SessionID:   "sess-dev",
		UserID:      "user-1",
		FamilyID:    "fam-1",
		Surface:     store.SurfaceDevice,
		DeviceID:    "dev-1",
		RefreshHash: hash,
		ExpiresAt:   1,
		TTL:         1,
	}
	require.NoError(t, st.CreateSession(ctx, sess))

	rotated, newPlain, err := RotateDeviceSession(ctx, st, sess, plain)
	require.NoError(t, err)
	assert.NotEqual(t, plain, newPlain)
	assert.Equal(t, HashRefreshToken(newPlain), rotated.RefreshHash)
	assert.Equal(t, hash, rotated.PrevHash)
	// 10-year window slide, not 30-day.
	assert.Greater(t, rotated.ExpiresAt, int64(0))

	// Replaying the OLD device token after rotation -> reuse detected.
	_, _, err = RotateDeviceSession(ctx, st, rotated, plain)
	require.ErrorIs(t, err, store.ErrRefreshReuse)
}
