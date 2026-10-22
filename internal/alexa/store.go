package alexa

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var ErrConflict = errors.New("Alexa state changed")

type Entry struct {
	Owner, Key       string
	Version, Expires int64
	Data             []byte
}
type Change struct {
	Before *Entry
	After  *Entry
}
type Repository interface {
	Read(context.Context, string, string) (*Entry, error)
	Commit(context.Context, []Change) error
}
type DynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}
type Dynamo struct {
	DB    DynamoAPI
	Table string
}

func NewDynamo(db DynamoAPI, table string) *Dynamo { return &Dynamo{DB: db, Table: table} }
func stateKey(owner, key string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "USER#" + owner}, "sk": &types.AttributeValueMemberS{Value: "ALEXA#" + key}}
}
func (d *Dynamo) Read(ctx context.Context, owner, key string) (*Entry, error) {
	r, err := d.DB.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: stateKey(owner, key), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, err
	}
	if len(r.Item) == 0 {
		return nil, nil
	}
	data, ok := r.Item["data"].(*types.AttributeValueMemberS)
	if !ok {
		return nil, errors.New("invalid Alexa state")
	}
	version, ok := r.Item["version"].(*types.AttributeValueMemberN)
	if !ok {
		return nil, errors.New("invalid Alexa version")
	}
	v, err := strconv.ParseInt(version.Value, 10, 64)
	if err != nil {
		return nil, err
	}
	ttl, ok := r.Item["ttl"].(*types.AttributeValueMemberN)
	if !ok {
		return nil, errors.New("invalid Alexa expiry")
	}
	expires, err := strconv.ParseInt(ttl.Value, 10, 64)
	if err != nil {
		return nil, err
	}
	return &Entry{Owner: owner, Key: key, Version: v, Expires: expires, Data: []byte(data.Value)}, nil
}
func (d *Dynamo) Commit(ctx context.Context, changes []Change) error {
	items := make([]types.TransactWriteItem, 0, len(changes))
	owner := ""
	for _, change := range changes {
		identity := change.After
		if identity == nil {
			identity = change.Before
		}
		if identity == nil || identity.Owner == "" {
			return ErrUnauthorized
		}
		if owner != "" && owner != identity.Owner {
			return ErrUnauthorized
		}
		owner = identity.Owner
		if change.Before != nil && (change.Before.Owner != identity.Owner || change.Before.Key != identity.Key) {
			return ErrUnauthorized
		}
		condition := "attribute_not_exists(pk)"
		var names map[string]string
		var values map[string]types.AttributeValue
		if change.Before != nil {
			condition = "#version = :version"
			names = map[string]string{"#version": "version"}
			values = map[string]types.AttributeValue{":version": &types.AttributeValueMemberN{Value: strconv.FormatInt(change.Before.Version, 10)}}
		}
		if change.After == nil {
			if change.Before == nil {
				return errors.New("invalid Alexa deletion")
			}
			items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: aws.String(d.Table), Key: stateKey(change.Before.Owner, change.Before.Key), ConditionExpression: aws.String(condition), ExpressionAttributeNames: names, ExpressionAttributeValues: values}})
			continue
		}
		a := change.After
		item := stateKey(a.Owner, a.Key)
		item["data"] = &types.AttributeValueMemberS{Value: string(a.Data)}
		item["version"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(a.Version, 10)}
		item["ttl"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(a.Expires, 10)}
		items = append(items, types.TransactWriteItem{Put: &types.Put{TableName: aws.String(d.Table), Item: item, ConditionExpression: aws.String(condition), ExpressionAttributeNames: names, ExpressionAttributeValues: values}})
	}
	if len(items) == 0 {
		return nil
	}
	// Fence every state mutation against account purge/revocation. A worker
	// authorized just before deletion cannot recreate data after the purge scan.
	items = append(items,
		types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(d.Table), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "CONFIG"}, "sk": &types.AttributeValueMemberS{Value: "OWNER"}}, ConditionExpression: aws.String("userId = :owner"), ExpressionAttributeValues: map[string]types.AttributeValue{":owner": &types.AttributeValueMemberS{Value: owner}}}},
		types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(d.Table), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "USER#" + owner}, "sk": &types.AttributeValueMemberS{Value: "PROFILE"}}, ConditionExpression: aws.String("#role = :owner AND #status = :active"), ExpressionAttributeNames: map[string]string{"#role": "role", "#status": "status"}, ExpressionAttributeValues: map[string]types.AttributeValue{":owner": &types.AttributeValueMemberS{Value: "owner"}, ":active": &types.AttributeValueMemberS{Value: "active"}}}},
	)
	_, err := d.DB.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	var conflict *types.TransactionCanceledException
	if errors.As(err, &conflict) {
		for index, r := range conflict.CancellationReasons {
			if aws.ToString(r.Code) == "ConditionalCheckFailed" {
				if index >= len(changes) {
					return ErrUnauthorized
				}
				return ErrConflict
			}
		}
	}
	return err
}
func changed(owner, key string, value any, expires int64, before *Entry) Change {
	data, err := json.Marshal(value)
	if err != nil {
		panic("Alexa static record serialization failed")
	}
	version := int64(1)
	if before != nil {
		version = before.Version + 1
	}
	return Change{Before: before, After: &Entry{Owner: owner, Key: key, Data: data, Version: version, Expires: expires}}
}
