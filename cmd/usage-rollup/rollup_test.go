package main

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRollupDDB struct {
	items   []map[string]types.AttributeValue
	updates []*dynamodb.UpdateItemInput
	query   func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error)
}

func (f *fakeRollupDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if f.query != nil {
		return f.query(in)
	}
	return &dynamodb.QueryOutput{Items: f.items}, nil
}

func (f *fakeRollupDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.updates = append(f.updates, in)
	return &dynamodb.UpdateItemOutput{}, nil
}

func n(v string) types.AttributeValue   { return &types.AttributeValueMemberN{Value: v} }
func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func TestActiveUserQueryUsesLegalKeyQueryAndFiltersEveryPage(t *testing.T) {
	calls := 0
	cursor := map[string]types.AttributeValue{"pk": str("CONFIG"), "sk": str("ACTIVEUSER#yesterday#2026-10-23")}
	fake := &fakeRollupDDB{query: func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		calls++
		// Reproduce the actual DynamoDB rejection observed in CloudWatch.
		if aws.ToString(in.FilterExpression) != "" {
			return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "Filter Expression can only contain non-primary key attributes: Primary key attribute: sk"}
		}
		require.Equal(t, "pk = :pk AND begins_with(sk, :prefix)", aws.ToString(in.KeyConditionExpression))
		require.NotContains(t, in.ExpressionAttributeValues, ":suffix")
		if calls == 1 {
			return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{{"sk": str("ACTIVEUSER#yesterday#2026-10-23")}}, LastEvaluatedKey: cursor}, nil
		}
		require.Equal(t, cursor, in.ExclusiveStartKey)
		return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
			{"sk": str("ACTIVEUSER#owner#2026-10-24")}, {"sk": str("ACTIVEUSER#owner#2026-10-24")},
			{"sk": str("ACTIVEUSER#other#2026-10-24")}, {"sk": str("ACTIVEUSER##2026-10-24")},
			{"sk": str("ACTIVEUSER#future#2026-10-25")},
		}}, nil
	}}
	users, err := queryActiveUsersToday(t.Context(), fake, "test", "2026-10-24")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"owner", "other"}, users)
	require.Equal(t, 2, calls)
}

// The monthly row sums every day row, including the agentcore-memory
// counters (plan.md cost-guard), and excludes itself from the sum.
func TestRollupUserSumsDayRowsIncludingMemoryCounters(t *testing.T) {
	fake := &fakeRollupDDB{items: []map[string]types.AttributeValue{
		{"sk": str("USAGE#2026-09"), "monthTokens": n("999999"), "monthMemEvents": n("999")},
		{"sk": str("USAGE#2026-09-13"), "dayTokens": n("100"), "daySeconds": n("60"), "dayMemEvents": n("12"), "dayMemRetrievals": n("3")},
		{"sk": str("USAGE#2026-09-14"), "dayTokens": n("50"), "daySeconds": n("30"), "dayMemEvents": n("8")},
	}}

	require.NoError(t, rollupUser(context.Background(), fake, "live-ninja", "u1", "2026-09"))
	require.Len(t, fake.updates, 1)
	up := fake.updates[0]
	assert.Equal(t, "USER#u1", up.Key["pk"].(*types.AttributeValueMemberS).Value)
	assert.Equal(t, "USAGE#2026-09", up.Key["sk"].(*types.AttributeValueMemberS).Value)
	assert.Contains(t, aws.ToString(up.UpdateExpression), "monthMemEvents = :me")
	assert.Equal(t, "150", up.ExpressionAttributeValues[":mt"].(*types.AttributeValueMemberN).Value)
	assert.Equal(t, "90", up.ExpressionAttributeValues[":ms"].(*types.AttributeValueMemberN).Value)
	assert.Equal(t, "20", up.ExpressionAttributeValues[":me"].(*types.AttributeValueMemberN).Value)
	assert.Equal(t, "3", up.ExpressionAttributeValues[":mr"].(*types.AttributeValueMemberN).Value)
}
