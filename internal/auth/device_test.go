package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"strings"
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

	// 1. Device registers pairing with its S256 challenge and receives the
	// user code it must display.
	nonce, userCode, expiresAt, err := RegisterPairing(ctx, st, challenge)
	require.NoError(t, err)
	assert.NotEmpty(t, nonce)
	assert.Len(t, userCode, UserCodeLength)
	assert.False(t, expiresAt.IsZero())

	// Claim before the browser leg -> not bound yet.
	_, err = ClaimPairing(ctx, st, nonce, verifier)
	require.ErrorIs(t, err, ErrPairNotBound)

	// 2. Browser leg: bind to the (first, owner-binding) profile — entering
	// the user code exactly as a human would (lowercased, dashed form).
	profile := &LWAProfile{UserID: "amzn1.account.owner", Email: "Owner@Example.com", Name: "Owner"}
	typed := strings.ToLower(FormatUserCode(userCode))
	require.NoError(t, BindPairing(ctx, st, log, nonce, "Kitchen Tab5", typed, profile))

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

	// A second bind attempt on the same nonce also fails closed — even with
	// the correct user code.
	err = BindPairing(ctx, st, log, nonce, "Evil Rebind", userCode, profile)
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

	nonce, userCode, _, err := RegisterPairing(ctx, st, s256("some-verifier"))
	require.NoError(t, err)

	// A stranger (not owner, not allowlisted) cannot bind a device — even
	// knowing the correct user code.
	stranger := &LWAProfile{UserID: "amzn1.account.stranger", Email: "stranger@example.com"}
	err = BindPairing(ctx, st, log, nonce, "Rogue Device", userCode, stranger)
	require.ErrorIs(t, err, ErrNotAllowed)

	// Pair row must still be pending (claimable by a legitimate bind), with
	// no attempt burned — the code gate never ran for the unauthorized user.
	pair, err := PollPairing(ctx, st, nonce)
	require.NoError(t, err)
	assert.Equal(t, store.PairStatusPending, pair.Status)
	assert.Zero(t, pair.CodeAttempts)
}

// ---- RFC 8628 user-code gate ----

func TestGenerateUserCodeFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		code, err := generateUserCode()
		require.NoError(t, err)
		require.Len(t, code, UserCodeLength)
		for _, r := range code {
			assert.Contains(t, UserCodeAlphabet, string(r),
				"user code %q contains %q outside the RFC 8628 alphabet", code, string(r))
		}
		seen[code] = true
	}
	// 50 draws from 20^8 must not collide — a collision means broken randomness.
	assert.Len(t, seen, 50)
}

func TestFormatAndNormalizeUserCode(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
		match bool // matches stored "GQNSVBTX"
	}{
		{"exact stored form", "GQNSVBTX", "GQNSVBTX", true},
		{"dashed display form", "GQNS-VBTX", "GQNSVBTX", true},
		{"lowercase", "gqnsvbtx", "GQNSVBTX", true},
		{"lowercase dashed", "gqns-vbtx", "GQNSVBTX", true},
		{"internal spaces", " gqns vbtx ", "GQNSVBTX", true},
		{"wrong code", "BBBB-BBBB", "BBBBBBBB", false},
		{"truncated", "GQNS-VBT", "GQNSVBT", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NormalizeUserCode(tt.in))
			assert.Equal(t, tt.match, matchUserCode(tt.in, "GQNSVBTX"))
		})
	}

	assert.Equal(t, "GQNS-VBTX", FormatUserCode("GQNSVBTX"))
	// Unexpected lengths pass through rather than corrupting.
	assert.Equal(t, "SHORT", FormatUserCode("SHORT"))
}

func TestMatchUserCodeFailsClosedOnEmptyStoredCode(t *testing.T) {
	// A pairing without a stored code must never bind — not even to an
	// empty input (which would trivially "equal" an empty stored code).
	assert.False(t, matchUserCode("", ""))
	assert.False(t, matchUserCode("GQNSVBTX", ""))
}

func TestBindPairingWrongCodeCountsAttemptsAndInvalidates(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	log := slog.Default()
	profile := &LWAProfile{UserID: "amzn1.account.owner", Email: "owner@example.com"}

	verifier := "device-on-chip-code-verifier-0123456789abcdef"
	nonce, userCode, _, err := RegisterPairing(ctx, st, s256(verifier))
	require.NoError(t, err)

	// Wrong entries 1..4: mismatch error carrying the remaining budget.
	for i := 1; i < MaxUserCodeAttempts; i++ {
		err := BindPairing(ctx, st, log, nonce, "", "WRNG-CODE", profile)
		require.ErrorIs(t, err, ErrUserCodeMismatch, "attempt %d", i)
		var mismatch *UserCodeMismatchError
		require.ErrorAs(t, err, &mismatch)
		assert.Equal(t, MaxUserCodeAttempts-i, mismatch.AttemptsRemaining, "attempt %d", i)

		pair, perr := PollPairing(ctx, st, nonce)
		require.NoError(t, perr)
		assert.Equal(t, store.PairStatusPending, pair.Status)
		assert.Equal(t, i, pair.CodeAttempts)
	}

	// 5th wrong entry: terminal — the pairing is invalidated.
	err = BindPairing(ctx, st, log, nonce, "", "WRNG-CODE", profile)
	require.ErrorIs(t, err, ErrPairFailed)

	pair, err := PollPairing(ctx, st, nonce)
	require.NoError(t, err)
	assert.Equal(t, store.PairStatusFailed, pair.Status)

	// The correct code can no longer bind the invalidated pairing...
	err = BindPairing(ctx, st, log, nonce, "", userCode, profile)
	require.ErrorIs(t, err, ErrPairFailed)
	// ...and the device's claim leg gets the terminal failure too, even
	// with a valid verifier.
	_, err = ClaimPairing(ctx, st, nonce, verifier)
	require.ErrorIs(t, err, ErrPairFailed)
	// No device record was ever created for the failed pairing.
	pair, err = PollPairing(ctx, st, nonce)
	require.NoError(t, err)
	assert.Empty(t, pair.DeviceID)
}

func TestBindPairingWrongCodeThenCorrectCodeSucceeds(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	log := slog.Default()
	profile := &LWAProfile{UserID: "amzn1.account.owner", Email: "owner@example.com"}

	nonce, userCode, _, err := RegisterPairing(ctx, st, s256("some-verifier"))
	require.NoError(t, err)

	// One typo doesn't kill the pairing...
	err = BindPairing(ctx, st, log, nonce, "", "WRNG-CODE", profile)
	require.ErrorIs(t, err, ErrUserCodeMismatch)

	// ...the correct code still binds.
	require.NoError(t, BindPairing(ctx, st, log, nonce, "", FormatUserCode(userCode), profile))
	pair, err := PollPairing(ctx, st, nonce)
	require.NoError(t, err)
	assert.Equal(t, store.PairStatusBound, pair.Status)
}

func TestBindPairingEmptyCodeNeverBinds(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	profile := &LWAProfile{UserID: "amzn1.account.owner", Email: "owner@example.com"}

	nonce, _, _, err := RegisterPairing(ctx, st, s256("some-verifier"))
	require.NoError(t, err)

	err = BindPairing(ctx, st, slog.Default(), nonce, "", "", profile)
	require.ErrorIs(t, err, ErrUserCodeMismatch)
	pair, err := PollPairing(ctx, st, nonce)
	require.NoError(t, err)
	assert.Equal(t, store.PairStatusPending, pair.Status)
	assert.Equal(t, 1, pair.CodeAttempts)
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
