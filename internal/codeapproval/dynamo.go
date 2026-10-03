package codeapproval

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoAPI deliberately excludes SQS and Ghost launch. All records live in the
// owner's partition, are retained for 30 days, and participate in account purge.
type DynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}
type DynamoStore struct {
	client DynamoAPI
	table  string
}

func NewDynamoStoreWithClient(client DynamoAPI, table string) *DynamoStore {
	return &DynamoStore{client: client, table: table}
}
func avs(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func intentKey(uid, id string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": avs("USER#" + uid), "sk": avs("CODEAPPROVAL#" + id)}
}
func decodeIntent(item map[string]types.AttributeValue) (*Intent, error) {
	p, ok := item["payload"].(*types.AttributeValueMemberS)
	if !ok {
		return nil, ErrCorrupt
	}
	var i Intent
	if e := json.Unmarshal([]byte(p.Value), &i); e != nil {
		return nil, ErrCorrupt
	}
	if e := validateIntent(&i); e != nil {
		return nil, e
	}
	return &i, nil
}
func (s *DynamoStore) Get(ctx context.Context, uid, id string) (*Intent, error) {
	out, e := s.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.table), Key: intentKey(uid, id), ConsistentRead: aws.Bool(true)})
	if e != nil {
		return nil, e
	}
	if len(out.Item) == 0 {
		return nil, ErrNotFound
	}
	i, e := decodeIntent(out.Item)
	if e != nil {
		return nil, e
	}
	if i.UserID != uid || i.ID != id {
		return nil, ErrCorrupt
	}
	return i, nil
}
func (s *DynamoStore) List(ctx context.Context, uid string, limit int, cursor string) ([]Intent, string, error) {
	if cursor != "" && !validID(cursor) {
		return nil, "", ErrValidation
	}
	in := &dynamodb.QueryInput{TableName: aws.String(s.table), ConsistentRead: aws.Bool(true), KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :prefix)"), ExpressionAttributeValues: map[string]types.AttributeValue{":pk": avs("USER#" + uid), ":prefix": avs("CODEAPPROVAL#")}, Limit: aws.Int32(int32(pageLimit(limit)))}
	if cursor != "" {
		in.ExclusiveStartKey = intentKey(uid, cursor)
	}
	out, e := s.client.Query(ctx, in)
	if e != nil {
		return nil, "", e
	}
	intents := make([]Intent, 0, len(out.Items))
	for _, item := range out.Items {
		i, e := decodeIntent(item)
		if e != nil {
			return nil, "", e
		}
		if i.UserID != uid {
			return nil, "", ErrCorrupt
		}
		intents = append(intents, *i)
	}
	next := ""
	if len(out.LastEvaluatedKey) > 0 {
		sk, ok := out.LastEvaluatedKey["sk"].(*types.AttributeValueMemberS)
		if !ok || len(sk.Value) != len("CODEAPPROVAL#")+32 {
			return nil, "", ErrCorrupt
		}
		next = sk.Value[len("CODEAPPROVAL#"):]
		if !validID(next) {
			return nil, "", ErrCorrupt
		}
	}
	return intents, next, nil
}
func (s *DynamoStore) CompareAndSwap(ctx context.Context, i *Intent, expected int64) error {
	if e := validateIntent(i); e != nil {
		return e
	}
	if i.Version != expected+1 {
		return ErrConflict
	}
	payload, e := json.Marshal(i)
	if e != nil {
		return e
	}
	item := intentKey(i.UserID, i.ID)
	item["payload"] = avs(string(payload))
	item["actionHash"] = avs(i.ActionHash)
	item["version"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(i.Version, 10)}
	item["ttl"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(i.RetainUntil, 10)}
	put := &types.Put{TableName: aws.String(s.table), Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")}
	if expected > 0 {
		put.ConditionExpression = aws.String("#version = :expected AND actionHash = :actionHash")
		put.ExpressionAttributeNames = map[string]string{"#version": "version"}
		put.ExpressionAttributeValues = map[string]types.AttributeValue{":expected": &types.AttributeValueMemberN{Value: strconv.FormatInt(expected, 10)}, ":actionHash": avs(i.ActionHash)}
	}
	// A profile may be disabled or demoted after the HTTP freshness check.
	// Enforce current ownership INSIDE the same atomic write boundary.
	owner := &types.ConditionCheck{TableName: aws.String(s.table),
		Key:                       map[string]types.AttributeValue{"pk": avs("USER#" + i.UserID), "sk": avs("PROFILE")},
		ConditionExpression:       aws.String("attribute_exists(pk) AND #status = :active AND #role = :owner"),
		ExpressionAttributeNames:  map[string]string{"#status": "status", "#role": "role"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":active": avs("active"), ":owner": avs("owner")}}
	_, e = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{{ConditionCheck: owner}, {Put: put}}})
	var cancelled *types.TransactionCanceledException
	if errors.As(e, &cancelled) {
		for index, reason := range cancelled.CancellationReasons {
			if aws.ToString(reason.Code) == "ConditionalCheckFailed" {
				if index == 0 {
					return ErrForbidden
				}
				return ErrConflict
			}
		}
	}
	return e
}
