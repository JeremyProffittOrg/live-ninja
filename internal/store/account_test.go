package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type consistentAccountQuery struct {
	*testutil.FakeDynamo
	t       *testing.T
	queries int
}

func (f *consistentAccountQuery) Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.queries++
	require.True(f.t, aws.ToBool(in.ConsistentRead), "every export page must include recently committed history")
	bounded := *in
	bounded.Limit = aws.Int32(2)
	return f.FakeDynamo.Query(ctx, &bounded, opts...)
}

func TestQueryUserPartitionIncludesAllHistoryUsingStrongPages(t *testing.T) {
	ctx := context.Background()
	f := &consistentAccountQuery{FakeDynamo: testutil.NewFakeDynamo(), t: t}
	st := NewWithClient(f, "table")
	for n := 0; n < 7; n++ {
		require.NoError(t, st.ConditionalPut(ctx, "USER#alice", fmt.Sprintf("JOBEVENT#job#%020d", n), map[string]any{"payload": "retained note"}, 0))
	}
	require.NoError(t, st.ConditionalPut(ctx, "USER#bob", "JOBEVENT#job#other", map[string]any{"payload": "private"}, 0))
	items, e := st.QueryUserPartition(ctx, "alice")
	require.NoError(t, e)
	require.Len(t, items, 7)
	require.Greater(t, f.queries, 1)
	for _, item := range items {
		require.Equal(t, "USER#alice", item["pk"])
	}
}

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
