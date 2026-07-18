package store

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newHardeningStore(t *testing.T) (*Store, *testutil.FakeDynamo) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	return NewWithClient(fake, "live-ninja-test"), fake
}

func TestSuspendUserFlipsActiveProfileOnce(t *testing.T) {
	s, fake := newHardeningStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &User{
		UserID:       "u1",
		AmazonUserID: "amzn1.account.a",
		Status:       UserStatusActive,
		Role:         RoleMember,
	}))

	require.NoError(t, s.SuspendUser(ctx, "u1", "hourly_burn"))

	u, err := s.GetUser(ctx, "u1")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, UserStatusSuspended, u.Status)
	assert.Greater(t, u.TokensValidAfter, int64(0), "JWT kill-switch must be bumped")

	raw := fake.RawItem("USER#u1", "PROFILE")
	require.NotNil(t, raw)
	reason, _ := raw["suspendReason"].(*types.AttributeValueMemberS)
	require.NotNil(t, reason)
	assert.Equal(t, "hourly_burn", reason.Value)
	_, hasAt := raw["suspendedAt"].(*types.AttributeValueMemberS)
	assert.True(t, hasAt, "suspendedAt must be recorded")

	// Second suspension is a conditional no-op (exactly-once transition).
	assert.ErrorIs(t, s.SuspendUser(ctx, "u1", "hourly_burn"), ErrNotFound)
}

func TestSuspendUserRequiresActiveProfile(t *testing.T) {
	s, _ := newHardeningStore(t)
	ctx := context.Background()

	// Absent profile.
	assert.ErrorIs(t, s.SuspendUser(ctx, "ghost", "hourly_burn"), ErrNotFound)

	// Disabled (non-active) profile is not silently converted.
	require.NoError(t, s.CreateUser(ctx, &User{
		UserID:       "u2",
		AmazonUserID: "amzn1.account.b",
		Status:       UserStatusDisabled,
	}))
	assert.ErrorIs(t, s.SuspendUser(ctx, "u2", "hourly_burn"), ErrNotFound)
	u, err := s.GetUser(ctx, "u2")
	require.NoError(t, err)
	assert.Equal(t, UserStatusDisabled, u.Status)
}

func TestReinstateUserRestoresActive(t *testing.T) {
	s, fake := newHardeningStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &User{
		UserID:       "u1",
		AmazonUserID: "amzn1.account.a",
		Status:       UserStatusActive,
	}))
	require.NoError(t, s.SuspendUser(ctx, "u1", "hourly_burn"))
	suspended, err := s.GetUser(ctx, "u1")
	require.NoError(t, err)

	require.NoError(t, s.ReinstateUser(ctx, "u1"))
	u, err := s.GetUser(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, UserStatusActive, u.Status)
	assert.Equal(t, suspended.TokensValidAfter, u.TokensValidAfter,
		"reinstate must not roll back the kill-switch")

	raw := fake.RawItem("USER#u1", "PROFILE")
	_, hasReinstated := raw["reinstatedAt"].(*types.AttributeValueMemberS)
	assert.True(t, hasReinstated)

	// Only suspended users can be reinstated.
	assert.ErrorIs(t, s.ReinstateUser(ctx, "u1"), ErrNotFound)
	assert.ErrorIs(t, s.ReinstateUser(ctx, "ghost"), ErrNotFound)
}
