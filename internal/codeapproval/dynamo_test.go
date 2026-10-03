package codeapproval

import (
	"context"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/require"
)

func seedOwner(db *testutil.FakeDynamo, uid, status, role string) {
	db.SeedItem(map[string]types.AttributeValue{"pk": avs("USER#" + uid), "sk": avs("PROFILE"), "status": avs(status), "role": avs(role)})
}

type fencedDynamo struct {
	*testutil.FakeDynamo
	beforeWrite func()
	reads       int
	writes      int
}

func (d *fencedDynamo) GetItem(ctx context.Context, in *dynamodb.GetItemInput, o ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if !aws.ToBool(in.ConsistentRead) {
		panic("approval reads must be consistent")
	}
	d.reads++
	return d.FakeDynamo.GetItem(ctx, in, o...)
}
func (d *fencedDynamo) TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, o ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	d.writes++
	if d.beforeWrite != nil {
		d.beforeWrite()
		d.beforeWrite = nil
	}
	return d.FakeDynamo.TransactWriteItems(ctx, in, o...)
}
func TestDynamoApprovalTransactionFencesCurrentOwnership(t *testing.T) {
	for _, change := range []string{"disabled", "deleting", "member", "missing"} {
		t.Run(change, func(t *testing.T) {
			db := &fencedDynamo{FakeDynamo: testutil.NewFakeDynamo()}
			seedOwner(db.FakeDynamo, "owner", "active", "owner")
			s := NewService(NewDynamoStoreWithClient(db, "test"), testCatalog{})
			ctx := context.Background()
			i, e := s.Prepare(ctx, "owner", testInput(), "prepare-fenced")
			require.NoError(t, e)
			db.beforeWrite = func() {
				if change == "missing" {
					_, e := db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String("test"), Key: map[string]types.AttributeValue{"pk": avs("USER#owner"), "sk": avs("PROFILE")}})
					require.NoError(t, e)
				} else if change == "member" {
					seedOwner(db.FakeDynamo, "owner", "active", "member")
				} else {
					seedOwner(db.FakeDynamo, "owner", change, "owner")
				}
			}
			_, e = s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-fenced")
			require.ErrorIs(t, e, ErrForbidden)
			stored, e := s.Get(ctx, "owner", i.ID)
			require.NoError(t, e)
			require.Equal(t, StatusPrepared, stored.Status)
			require.Nil(t, stored.Receipt)
		})
	}
}
func TestDynamoStoreDurableReplayAndTenantQuery(t *testing.T) {
	db := &fencedDynamo{FakeDynamo: testutil.NewFakeDynamo()}
	seedOwner(db.FakeDynamo, "owner", "active", "owner")
	seedOwner(db.FakeDynamo, "other", "active", "owner")
	s := NewService(NewDynamoStoreWithClient(db, "test"), testCatalog{})
	ctx := context.Background()
	i, e := s.Prepare(ctx, "owner", testInput(), "prepare-dynamo")
	require.NoError(t, e)
	approved, e := s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-dynamo")
	require.NoError(t, e)
	writes := db.writes
	replay, e := s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-dynamo")
	require.NoError(t, e)
	require.Equal(t, approved, replay)
	require.Equal(t, writes, db.writes)
	page, e := s.List(ctx, "owner", 10, "")
	require.NoError(t, e)
	require.Len(t, page.Intents, 1)
	page, e = s.List(ctx, "other", 10, "")
	require.NoError(t, e)
	require.Empty(t, page.Intents)
	raw := db.RawItem("USER#owner", "CODEAPPROVAL#"+i.ID)
	require.NotNil(t, raw["ttl"])
	require.NotNil(t, raw["payload"])
}

func TestExpiredStorageRecreationCannotReuseOldApprovalHash(t *testing.T) {
	for _, changeAction := range []bool{false, true} {
		db := &fencedDynamo{FakeDynamo: testutil.NewFakeDynamo()}
		seedOwner(db.FakeDynamo, "owner", "active", "owner")
		s := NewService(NewDynamoStoreWithClient(db, "test"), testCatalog{})
		ctx := context.Background()
		in := testInput()
		old, e := s.Prepare(ctx, "owner", in, "prepare-reused-id")
		require.NoError(t, e)
		// Simulate the eventual TTL removal; idempotency receipts are bounded.
		_, e = db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String("test"), Key: intentKey("owner", old.ID)})
		require.NoError(t, e)
		if changeAction {
			in.Instructions = "A different change after expiry"
		}
		current, e := s.Prepare(ctx, "owner", in, "prepare-reused-id")
		require.NoError(t, e)
		require.Equal(t, old.ID, current.ID)
		require.Equal(t, old.Version, current.Version)
		require.NotEqual(t, old.ActionHash, current.ActionHash)
		_, e = s.Approve(ctx, "owner", old.ID, old.Version, old.ActionHash, "approve-old-generation")
		require.ErrorIs(t, e, ErrConflict)
		saved, e := s.Get(ctx, "owner", current.ID)
		require.NoError(t, e)
		require.Nil(t, saved.Receipt)
		_, e = s.Approve(ctx, "owner", current.ID, current.Version, current.ActionHash, "approve-new-generation")
		require.NoError(t, e)
	}
}
