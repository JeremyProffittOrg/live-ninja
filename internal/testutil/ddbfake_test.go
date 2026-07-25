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
