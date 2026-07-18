package store

// Tests for the device-pairing store surface: the PAIR row's RFC 8628
// user-code fields (required code, atomic wrong-attempt counting, the
// pending → failed invalidation transition) and the PAIRCONFIRM row that
// carries the LWA-verified identity between the browser callback and the
// user-code confirm POST.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newPairStore() *Store {
	return NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test")
}

func seedPair(t *testing.T, st *Store, nonce string) *Pair {
	t.Helper()
	p := &Pair{
		Nonce:         nonce,
		CodeChallenge: "challenge-challenge-challenge-challenge-chal",
		UserCode:      "GQNSVBTX",
	}
	require.NoError(t, st.CreatePair(context.Background(), p))
	return p
}

func TestCreatePairRequiresUserCode(t *testing.T) {
	st := newPairStore()
	err := st.CreatePair(context.Background(), &Pair{
		Nonce:         "n1",
		CodeChallenge: "challenge-challenge-challenge-challenge-chal",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "userCode")

	// Nothing was written.
	p, err := st.GetPair(context.Background(), "n1")
	require.NoError(t, err)
	assert.Nil(t, p)
}

func TestPairUserCodeRoundTrip(t *testing.T) {
	st := newPairStore()
	seedPair(t, st, "n1")

	p, err := st.GetPair(context.Background(), "n1")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "GQNSVBTX", p.UserCode)
	assert.Equal(t, PairStatusPending, p.Status)
	assert.Zero(t, p.CodeAttempts)
}

func TestIncrementPairAttempts(t *testing.T) {
	ctx := context.Background()
	st := newPairStore()
	seedPair(t, st, "n1")

	for want := 1; want <= 3; want++ {
		require.NoError(t, st.IncrementPairAttempts(ctx, "n1"))
		p, err := st.GetPair(ctx, "n1")
		require.NoError(t, err)
		assert.Equal(t, want, p.CodeAttempts)
	}

	// Attempts cannot accrue on a pairing that already left pending.
	require.NoError(t, st.UpdatePair(ctx, "n1", PairStatusPending, PairStatusBound, "dev-1", "u1"))
	require.ErrorIs(t, st.IncrementPairAttempts(ctx, "n1"), ErrInvalidPairState)

	// Nor on a nonce that never existed.
	require.ErrorIs(t, st.IncrementPairAttempts(ctx, "no-such"), ErrInvalidPairState)
}

func TestUpdatePairPendingToFailedIsTerminal(t *testing.T) {
	ctx := context.Background()
	st := newPairStore()
	seedPair(t, st, "n1")

	require.NoError(t, st.UpdatePair(ctx, "n1", PairStatusPending, PairStatusFailed, "", ""))
	p, err := st.GetPair(ctx, "n1")
	require.NoError(t, err)
	assert.Equal(t, PairStatusFailed, p.Status)

	// A failed pairing cannot be bound or re-failed.
	require.ErrorIs(t, st.UpdatePair(ctx, "n1", PairStatusPending, PairStatusBound, "dev-1", "u1"), ErrInvalidPairState)
	require.ErrorIs(t, st.UpdatePair(ctx, "n1", PairStatusPending, PairStatusFailed, "", ""), ErrInvalidPairState)
}

func TestPairConfirmLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newPairStore()

	pc := &PairConfirm{
		Token:        "tok-1",
		Nonce:        "n1",
		AmazonUserID: "amzn1.account.owner",
		Email:        "owner@example.com",
		Name:         "Owner",
	}
	require.NoError(t, st.PutPairConfirm(ctx, pc))
	assert.NotZero(t, pc.CreatedAt)
	assert.NotZero(t, pc.TTL)

	got, err := st.GetPairConfirm(ctx, "tok-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "tok-1", got.Token)
	assert.Equal(t, "n1", got.Nonce)
	assert.Equal(t, "amzn1.account.owner", got.AmazonUserID)
	assert.Equal(t, "owner@example.com", got.Email)

	// NOT consumed on read — wrong-code retries re-use the same token.
	again, err := st.GetPairConfirm(ctx, "tok-1")
	require.NoError(t, err)
	require.NotNil(t, again)

	// Tokens are single-use random values: a duplicate Put is a collision.
	require.ErrorIs(t, st.PutPairConfirm(ctx, &PairConfirm{
		Token: "tok-1", Nonce: "n2", AmazonUserID: "amzn1.account.other",
	}), ErrAlreadyExists)

	require.NoError(t, st.DeletePairConfirm(ctx, "tok-1"))
	gone, err := st.GetPairConfirm(ctx, "tok-1")
	require.NoError(t, err)
	assert.Nil(t, gone)

	// Idempotent delete.
	require.NoError(t, st.DeletePairConfirm(ctx, "tok-1"))
}

func TestPairConfirmValidation(t *testing.T) {
	ctx := context.Background()
	st := newPairStore()

	for _, pc := range []*PairConfirm{
		{Nonce: "n1", AmazonUserID: "a"},
		{Token: "t", AmazonUserID: "a"},
		{Token: "t", Nonce: "n1"},
	} {
		require.Error(t, st.PutPairConfirm(ctx, pc))
	}
}

func TestGetPairConfirmExpired(t *testing.T) {
	ctx := context.Background()
	st := newPairStore()

	require.NoError(t, st.PutPairConfirm(ctx, &PairConfirm{
		Token:        "tok-old",
		Nonce:        "n1",
		AmazonUserID: "amzn1.account.owner",
		TTL:          time.Now().Add(-time.Minute).Unix(),
	}))
	got, err := st.GetPairConfirm(ctx, "tok-old")
	require.NoError(t, err)
	assert.Nil(t, got, "expired confirm rows must read as gone even before the TTL sweep")
}

func TestUnknownPairConfirm(t *testing.T) {
	got, err := newPairStore().GetPairConfirm(context.Background(), "never-existed")
	require.NoError(t, err)
	assert.Nil(t, got)
}
