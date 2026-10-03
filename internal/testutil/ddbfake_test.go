package testutil

import (
	"context"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConditionLessThanComparator covers the "<" clause the M17 RCA claim
// markers depend on (attribute_not_exists(pk) OR expiresAt < :now): the whole
// correctness argument for the cooldown is that an expired-but-unswept marker
// must still lose the condition, and that is expressed only by "<".
func TestConditionLessThanComparator(t *testing.T) {
	const expr = "attribute_not_exists(pk) OR expiresAt < :now"
	now := int64(1_800_000_000)
	values := map[string]types.AttributeValue{
		":now": &types.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
	}

	item := func(expiresAt int64) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{
			"pk":        &types.AttributeValueMemberS{Value: "RCA#t#c"},
			"sk":        &types.AttributeValueMemberS{Value: "COOLDOWN#sig"},
			"expiresAt": &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)},
		}
	}

	assert.True(t, evalCondition(expr, nil, nil, values), "absent item passes via attribute_not_exists")
	assert.False(t, evalCondition(expr, item(now+3600), nil, values), "a live window must fail the condition")
	assert.True(t, evalCondition(expr, item(now-1), nil, values), "an expired window must pass")

	// A non-numeric (or missing) attribute cannot satisfy a numeric comparison.
	bad := item(now - 1)
	bad["expiresAt"] = &types.AttributeValueMemberS{Value: "soon"}
	assert.False(t, evalCondition(expr, bad, nil, values))
}

// TestUpdateItemReturnValues covers the UPDATED_NEW read-back the RCA daily
// budget claim uses to learn its own post-increment count without a second
// (racy) GetItem.
func TestUpdateItemReturnValues(t *testing.T) {
	ctx := context.Background()
	f := NewFakeDynamo()

	in := &dynamodb.UpdateItemInput{
		TableName: aws.String("t"),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "RCA#BUDGET"},
			"sk": &types.AttributeValueMemberS{Value: "DAY#2026-07-25"},
		},
		UpdateExpression:         aws.String("ADD #c :one"),
		ExpressionAttributeNames: map[string]string{"#c": "count"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
		},
		ReturnValues: types.ReturnValueUpdatedNew,
	}

	out, err := f.UpdateItem(ctx, in)
	require.NoError(t, err)
	require.NotNil(t, out.Attributes)
	assert.Equal(t, "1", out.Attributes["count"].(*types.AttributeValueMemberN).Value)

	out, err = f.UpdateItem(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, "2", out.Attributes["count"].(*types.AttributeValueMemberN).Value)

	// Without ReturnValues nothing is handed back (the default NONE).
	in.ReturnValues = ""
	out, err = f.UpdateItem(ctx, in)
	require.NoError(t, err)
	assert.Nil(t, out.Attributes)
}

func TestTransactionPutDeleteAreAtomicWhenAnyConditionFails(t *testing.T) {
	f := NewFakeDynamo()
	ctx := context.Background()
	key := func(sk string) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "USER#alice"}, "sk": &types.AttributeValueMemberS{Value: sk},
		}
	}
	f.SeedItem(key("RULE#old"))
	changes := []types.TransactWriteItem{
		{Put: &types.Put{Item: key("AUDIT#new"), ConditionExpression: aws.String("attribute_not_exists(pk)")}},
		{Delete: &types.Delete{Key: key("RULE#old"), ConditionExpression: aws.String("attribute_exists(pk)")}},
		{ConditionCheck: &types.ConditionCheck{Key: key("PROFILE"), ConditionExpression: aws.String("attribute_exists(pk)")}},
	}
	_, err := f.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: changes})
	var cancelled *types.TransactionCanceledException
	require.ErrorAs(t, err, &cancelled)
	require.Equal(t, "ConditionalCheckFailed", aws.ToString(cancelled.CancellationReasons[2].Code))
	require.Nil(t, f.RawItem("USER#alice", "AUDIT#new"), "failed final check rolls back earlier Put")
	require.NotNil(t, f.RawItem("USER#alice", "RULE#old"), "failed final check rolls back earlier Delete")
	f.SeedItem(key("PROFILE"))
	_, err = f.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: changes})
	require.NoError(t, err)
	require.NotNil(t, f.RawItem("USER#alice", "AUDIT#new"))
	require.Nil(t, f.RawItem("USER#alice", "RULE#old"))
	_, err = f.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: changes})
	require.ErrorAs(t, err, &cancelled)
	require.Equal(t, "ConditionalCheckFailed", aws.ToString(cancelled.CancellationReasons[0].Code))
	require.Equal(t, "ConditionalCheckFailed", aws.ToString(cancelled.CancellationReasons[1].Code))
}
