package store

import (
	"context"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/require"
)

type approvalOwnerReadDynamo struct {
	*testutil.FakeDynamo
	consistent bool
}

func (d *approvalOwnerReadDynamo) GetItem(ctx context.Context, in *dynamodb.GetItemInput, o ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	d.consistent = aws.ToBool(in.ConsistentRead)
	return d.FakeDynamo.GetItem(ctx, in, o...)
}
func TestApprovalOwnerReadIsStronglyConsistent(t *testing.T) {
	db := &approvalOwnerReadDynamo{FakeDynamo: testutil.NewFakeDynamo()}
	st := NewWithClient(db, "test")
	ctx := context.Background()
	require.NoError(t, st.CreateUser(ctx, &User{UserID: "owner", AmazonUserID: "amazon-owner", Status: UserStatusActive, Role: RoleOwner}))
	user, e := st.GetUserConsistent(ctx, "owner")
	require.NoError(t, e)
	require.True(t, db.consistent)
	require.Equal(t, RoleOwner, user.Role)
	user, e = st.GetUserConsistent(ctx, "missing")
	require.NoError(t, e)
	require.Nil(t, user)
	require.True(t, db.consistent)
}
