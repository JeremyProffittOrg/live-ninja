package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetUserStatus(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// No profile row -> ErrNotFound (never upserts a ghost profile).
	err := st.SetUserStatus(ctx, "u1", UserStatusDeleting)
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, st.CreateUser(ctx, &User{
		UserID: "u1", AmazonUserID: "amzn1.account.a",
		Email: "a@example.com", Role: RoleMember, Status: UserStatusActive,
	}))

	require.NoError(t, st.SetUserStatus(ctx, "u1", UserStatusDeleting))
	u, err := st.GetUser(ctx, "u1")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, UserStatusDeleting, u.Status)

	// Everything else on the profile is preserved.
	assert.Equal(t, "a@example.com", u.Email)
	assert.Equal(t, RoleMember, u.Role)
}

func TestQueryUserPartition(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	require.NoError(t, st.CreateUser(ctx, &User{
		UserID: "u1", AmazonUserID: "amzn1.account.a", Status: UserStatusActive,
	}))
	require.NoError(t, st.ConditionalPut(ctx, "USER#u1", "LOG#sess-1#000001",
		map[string]any{"role": "user", "text": "hello"}, 0))
	_, err := st.RecordConsent(ctx, "u1", "web", "v1", "")
	require.NoError(t, err)
	// Another user's data must not leak into the partition query.
	require.NoError(t, st.ConditionalPut(ctx, "USER#u2", "LOG#sess-9#000001",
		map[string]any{"role": "user", "text": "other"}, 0))

	items, err := st.QueryUserPartition(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, items, 3)
	sks := map[string]bool{}
	for _, it := range items {
		sk, _ := it["sk"].(string)
		sks[sk] = true
	}
	assert.True(t, sks["PROFILE"])
	assert.True(t, sks["LOG#sess-1#000001"])

	_, err = st.QueryUserPartition(ctx, "")
	assert.Error(t, err)
}
