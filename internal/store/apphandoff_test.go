package store

// Tests for the Android broker APPHANDOFF row (oauth.go): single-use
// consume-on-read, the PKCE app_challenge carried forward to the claim, and
// TTL / required-field guards.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newHandoffStore() *Store {
	return NewWithClient(testutil.NewFakeDynamo(), "live-ninja-test")
}

func TestPutAppHandoffRequiresFields(t *testing.T) {
	st := newHandoffStore()
	ctx := context.Background()

	assert.Error(t, st.PutAppHandoff(ctx, &AppHandoff{UserID: "u1", AppChallenge: "c1"}))          // no code
	assert.Error(t, st.PutAppHandoff(ctx, &AppHandoff{Code: "c", AppChallenge: "c1"}))              // no userId
	assert.Error(t, st.PutAppHandoff(ctx, &AppHandoff{Code: "c", UserID: "u1"}))                    // no challenge
	assert.NoError(t, st.PutAppHandoff(ctx, &AppHandoff{Code: "c", UserID: "u1", AppChallenge: "c1"}))
}

func TestAppHandoffRoundTripAndSingleUse(t *testing.T) {
	st := newHandoffStore()
	ctx := context.Background()

	require.NoError(t, st.PutAppHandoff(ctx, &AppHandoff{
		Code:         "handoff-code",
		UserID:       "USER#42",
		AppChallenge: "N9awN5LcsuJsGXtvZ8ihHKaTAg6PgVbaXuZs9LJt3bY",
	}))

	got, err := st.GetAppHandoff(ctx, "handoff-code")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "handoff-code", got.Code)
	assert.Equal(t, "USER#42", got.UserID)
	assert.Equal(t, "N9awN5LcsuJsGXtvZ8ihHKaTAg6PgVbaXuZs9LJt3bY", got.AppChallenge)

	// Consumed on first read — a replay of the same code returns nothing, so
	// a stolen/intercepted handoff URL cannot be claimed twice.
	again, err := st.GetAppHandoff(ctx, "handoff-code")
	require.NoError(t, err)
	assert.Nil(t, again)
}

func TestAppHandoffDuplicateCodeRejected(t *testing.T) {
	st := newHandoffStore()
	ctx := context.Background()

	require.NoError(t, st.PutAppHandoff(ctx, &AppHandoff{Code: "dup", UserID: "u1", AppChallenge: "c1"}))
	err := st.PutAppHandoff(ctx, &AppHandoff{Code: "dup", UserID: "u2", AppChallenge: "c2"})
	assert.ErrorIs(t, err, ErrAlreadyExists)
}

func TestAppHandoffExpiredReturnsNil(t *testing.T) {
	st := newHandoffStore()
	ctx := context.Background()

	require.NoError(t, st.PutAppHandoff(ctx, &AppHandoff{
		Code:         "stale",
		UserID:       "u1",
		AppChallenge: "c1",
		TTL:          time.Now().Add(-time.Minute).Unix(),
	}))
	got, err := st.GetAppHandoff(ctx, "stale")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGetAppHandoffMissing(t *testing.T) {
	st := newHandoffStore()
	got, err := st.GetAppHandoff(context.Background(), "never-created")
	require.NoError(t, err)
	assert.Nil(t, got)
}
