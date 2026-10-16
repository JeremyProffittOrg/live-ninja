// Package mcpauth implements the resource-bound OAuth authority for Live Ninja MCP.
// Browser identity remains owned by the existing Login with Amazon session layer.
package mcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"strconv"
	"time"
)

var ErrConflict = errors.New("oauth concurrent update")

type Record struct {
	Key     string
	Version int64
	Expires int64
	Data    []byte
}
type Change struct {
	Before *Record
	After  Record
}

// Repository provides consistent reads and all-or-nothing compare-and-swap writes.
type Repository interface {
	Get(context.Context, string) (*Record, error)
	Commit(context.Context, []Change) error
}
type dynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}
type Dynamo struct {
	Client dynamoAPI
	Table  string
}

func NewDynamo(ctx context.Context, table string) (*Dynamo, error) {
	if table == "" {
		return nil, errors.New("MCP OAuth table is required")
	}
	cfg, e := awsconfig.LoadDefaultConfig(ctx)
	if e != nil {
		return nil, e
	}
	return &Dynamo{dynamodb.NewFromConfig(cfg), table}, nil
}
func key(k string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "MCP#" + k}, "sk": &types.AttributeValueMemberS{Value: "AUTH"}}
}
func (d *Dynamo) Get(ctx context.Context, k string) (*Record, error) {
	out, e := d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: key(k), ConsistentRead: aws.Bool(true)})
	if e != nil {
		return nil, e
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	data, ok := out.Item["data"].(*types.AttributeValueMemberS)
	if !ok {
		return nil, errors.New("invalid OAuth record")
	}
	v, ok := out.Item["version"].(*types.AttributeValueMemberN)
	if !ok {
		return nil, errors.New("invalid OAuth version")
	}
	version, e := strconv.ParseInt(v.Value, 10, 64)
	if e != nil {
		return nil, e
	}
	ttl, ok := out.Item["ttl"].(*types.AttributeValueMemberN)
	if !ok {
		return nil, errors.New("invalid OAuth expiry")
	}
	expires, e := strconv.ParseInt(ttl.Value, 10, 64)
	if e != nil {
		return nil, e
	}
	return &Record{k, version, expires, []byte(data.Value)}, nil
}
func (d *Dynamo) Commit(ctx context.Context, changes []Change) error {
	items := make([]types.TransactWriteItem, 0, len(changes))
	for _, c := range changes {
		av := key(c.After.Key)
		av["data"] = &types.AttributeValueMemberS{Value: string(c.After.Data)}
		av["version"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(c.After.Version, 10)}
		av["ttl"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(c.After.Expires, 10)}
		condition := "attribute_not_exists(pk)"
		var values map[string]types.AttributeValue
		if c.Before != nil {
			condition = "#version = :old"
			values = map[string]types.AttributeValue{":old": &types.AttributeValueMemberN{Value: strconv.FormatInt(c.Before.Version, 10)}}
		}
		items = append(items, types.TransactWriteItem{Put: &types.Put{TableName: aws.String(d.Table), Item: av, ConditionExpression: aws.String(condition), ExpressionAttributeValues: values, ExpressionAttributeNames: func() map[string]string {
			if c.Before != nil {
				return map[string]string{"#version": "version"}
			}
			return nil
		}()}})
	}
	_, e := d.Client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	var cancel *types.TransactionCanceledException
	if errors.As(e, &cancel) {
		for _, r := range cancel.CancellationReasons {
			if aws.ToString(r.Code) == "ConditionalCheckFailed" {
				return ErrConflict
			}
		}
	}
	return e
}
func record(k string, value any, expires int64, before *Record) Change {
	data, e := json.Marshal(value)
	if e != nil {
		panic(fmt.Sprintf("static OAuth record serialization: %v", e))
	}
	v := int64(1)
	if before != nil {
		v = before.Version + 1
	}
	return Change{before, Record{k, v, expires, data}}
}
func (s *Service) read(ctx context.Context, k string, out any) (*Record, error) {
	r, e := s.Repo.Get(ctx, k)
	if e != nil {
		return nil, e
	}
	if r == nil || r.Expires <= s.now().Unix() {
		return nil, ErrInvalidGrant
	}
	if json.Unmarshal(r.Data, out) != nil {
		return nil, ErrInvalidGrant
	}
	return r, nil
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// OwnerActive reads the existing identity records consistently; it never creates
// an account or accepts an identity asserted by an OAuth client.
func (d *Dynamo) OwnerActive(ctx context.Context, uid string, issued int64) bool {
	if uid == "" {
		return false
	}
	owner, e := d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "CONFIG"}, "sk": &types.AttributeValueMemberS{Value: "OWNER"}}, ConsistentRead: aws.Bool(true)})
	if e != nil {
		return false
	}
	var binding store.OwnerBinding
	if attributevalue.UnmarshalMap(owner.Item, &binding) != nil || binding.UserID != uid {
		return false
	}
	out, e := d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "USER#" + uid}, "sk": &types.AttributeValueMemberS{Value: "PROFILE"}}, ConsistentRead: aws.Bool(true)})
	if e != nil {
		return false
	}
	var user store.User
	if attributevalue.UnmarshalMap(out.Item, &user) != nil {
		return false
	}
	return user.UserID == uid && user.Role == store.RoleOwner && user.Status == store.UserStatusActive && user.TokensValidAfter <= issued
}
