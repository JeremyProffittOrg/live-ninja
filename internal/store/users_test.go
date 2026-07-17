package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBindOwnerConditional(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// Unbound deployment: no owner yet.
	owner, err := st.GetOwner(ctx)
	require.NoError(t, err)
	assert.Nil(t, owner)

	// First bind wins.
	require.NoError(t, st.BindOwner(ctx, "amzn1.account.first", "uid-1"))

	owner, err = st.GetOwner(ctx)
	require.NoError(t, err)
	require.NotNil(t, owner)
	assert.Equal(t, "amzn1.account.first", owner.AmazonUserID)
	assert.Equal(t, "uid-1", owner.UserID)

	// Any later bind — same or different identity — loses the conditional.
	err = st.BindOwner(ctx, "amzn1.account.second", "uid-2")
	require.ErrorIs(t, err, ErrAlreadyBound)
	err = st.BindOwner(ctx, "amzn1.account.first", "uid-1")
	require.ErrorIs(t, err, ErrAlreadyBound)

	// The stored binding is unchanged by the losing attempts.
	owner, err = st.GetOwner(ctx)
	require.NoError(t, err)
	assert.Equal(t, "uid-1", owner.UserID)
}

func TestCreateAndGetUserByLWA(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	u := &User{
		UserID:       "uid-1",
		AmazonUserID: "amzn1.account.a",
		Email:        "a@example.com",
		Role:         RoleOwner,
		Status:       UserStatusActive,
	}
	require.NoError(t, st.CreateUser(ctx, u))
	assert.NotZero(t, u.CreatedAt)

	got, err := st.GetUserByLWA(ctx, "amzn1.account.a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "uid-1", got.UserID)

	byID, err := st.GetUser(ctx, "uid-1")
	require.NoError(t, err)
	require.NotNil(t, byID)
	assert.Equal(t, "amzn1.account.a", byID.AmazonUserID)

	// Duplicate create is a conditional failure.
	err = st.CreateUser(ctx, u)
	require.ErrorIs(t, err, ErrAlreadyExists)

	// Unknown lookups -> (nil, nil).
	none, err := st.GetUserByLWA(ctx, "amzn1.account.nobody")
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestSetTokensValidAfter(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	require.NoError(t, st.CreateUser(ctx, &User{
		UserID: "uid-1", AmazonUserID: "amzn1.account.a", Status: UserStatusActive,
	}))

	now := time.Now().Unix()
	require.NoError(t, st.SetTokensValidAfter(ctx, "uid-1", now))

	u, err := st.GetUser(ctx, "uid-1")
	require.NoError(t, err)
	assert.Equal(t, now, u.TokensValidAfter)

	// Missing user -> ErrNotFound, not a silent create.
	err = st.SetTokensValidAfter(ctx, "uid-ghost", now)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestAllowlistLogic(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// Empty allowlist: nobody is allowed.
	ok, err := st.IsAllowed(ctx, "amzn1.account.x", "x@example.com")
	require.NoError(t, err)
	assert.False(t, ok)

	// Emails are normalized to lowercase on add AND on check.
	require.NoError(t, st.AddAllow(ctx, "  Friend@Example.COM ", "owner"))
	ok, err = st.IsAllowed(ctx, "", "friend@example.com")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = st.IsAllowed(ctx, "", "FRIEND@example.com")
	require.NoError(t, err)
	assert.True(t, ok)

	// Amazon user ids pass through case-sensitively.
	require.NoError(t, st.AddAllow(ctx, "amzn1.account.ABC", "owner"))
	ok, err = st.IsAllowed(ctx, "amzn1.account.ABC", "")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = st.IsAllowed(ctx, "amzn1.account.abc", "")
	require.NoError(t, err)
	assert.False(t, ok, "amazon user ids are case-sensitive opaque ids")

	// Either key matching is enough.
	ok, err = st.IsAllowed(ctx, "amzn1.account.unknown", "friend@example.com")
	require.NoError(t, err)
	assert.True(t, ok)

	// Empty keys are skipped safely.
	ok, err = st.IsAllowed(ctx, "", "")
	require.NoError(t, err)
	assert.False(t, ok)

	// List returns both entries with keys stripped of the ALLOW# prefix.
	entries, err := st.ListAllow(ctx)
	require.NoError(t, err)
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	assert.ElementsMatch(t, []string{"friend@example.com", "amzn1.account.ABC"}, keys)

	// Remove is normalized the same way and idempotent.
	require.NoError(t, st.RemoveAllow(ctx, "FRIEND@example.com"))
	ok, err = st.IsAllowed(ctx, "", "friend@example.com")
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, st.RemoveAllow(ctx, "friend@example.com")) // already gone
}

func TestConditionalPutIdempotency(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	err := st.ConditionalPut(ctx, "IDEMP#u1#key1", "IDEMP",
		map[string]any{"tool": "send_email"}, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)

	err = st.ConditionalPut(ctx, "IDEMP#u1#key1", "IDEMP",
		map[string]any{"tool": "send_email"}, time.Now().Add(time.Hour).Unix())
	require.ErrorIs(t, err, ErrAlreadyExists)

	// A different key is independent.
	err = st.ConditionalPut(ctx, "IDEMP#u1#key2", "IDEMP", nil, 0)
	require.NoError(t, err)
}
