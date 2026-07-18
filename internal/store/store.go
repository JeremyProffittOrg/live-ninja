// Package store wraps the Live Ninja single-table DynamoDB access
// patterns. Every method here is a key lookup (GetItem/PutItem) or a
// Query against one partition/GSI — never a Scan, per the repo's
// standing "no Scan on a serving path" rule (see deploy.md and
// plan.md M0's DynamoDB table definition: pk/sk + GSI1 + GSI2, TTL on
// `ttl`, PAY_PER_REQUEST).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrAlreadyExists is returned by ConditionalPut when the conditional
// write loses to an item already occupying that key. Callers use this to
// treat a duplicate delivery/invocation as an idempotent no-op rather
// than a hard failure.
var ErrAlreadyExists = errors.New("store: item already exists")

// ddbAPI is the subset of the DynamoDB client Store depends on, so tests
// can inject a fake without a real table.
type ddbAPI interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	TransactWriteItems(ctx context.Context, params *dynamodb.TransactWriteItemsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// Store is a thin, typed wrapper around the live-ninja DynamoDB table.
type Store struct {
	client ddbAPI
	table  string
}

// New builds a Store from the ambient AWS config (the Lambda execution
// role's credentials) and the given table name (each function reads this
// from the TABLE_NAME env var via config.FromEnv().TableName).
func New(ctx context.Context, tableName string) (*Store, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: load aws config: %w", err)
	}
	return &Store{
		client: dynamodb.NewFromConfig(cfg),
		table:  tableName,
	}, nil
}

// NewWithClient builds a Store around an already-constructed client (or a
// test fake implementing ddbAPI).
func NewWithClient(client ddbAPI, tableName string) *Store {
	return &Store{client: client, table: tableName}
}

// DeviceTelemetry is the item shape iot-ingest persists for each inbound
// `liveninja/<thing>/telemetry` MQTT message: the latest-snapshot record
// at pk=DEVICE#<id>/sk=TELEM, plus a GSI2 "recently seen" feed
// (gsi2pk=DEVSEEN#, gsi2sk=<RFC3339 lastSeen>) so "devices seen recently"
// is a Query, never a table Scan.
type DeviceTelemetry struct {
	PK       string         `dynamodbav:"pk"`
	SK       string         `dynamodbav:"sk"`
	Gsi2PK   string         `dynamodbav:"gsi2pk"`
	Gsi2SK   string         `dynamodbav:"gsi2sk"`
	DeviceID string         `dynamodbav:"deviceId"`
	LastSeen string         `dynamodbav:"lastSeen"`
	Payload  map[string]any `dynamodbav:"payload"`
}

// PutDeviceTelemetry records the latest telemetry snapshot for a device.
// This is iot-ingest's M0 real behavior (see plan.md): parse
// {deviceId, ...} off the IoT rule and PutItem it under
// DEVICE#<id>/TELEM.
func (s *Store) PutDeviceTelemetry(ctx context.Context, deviceID string, payload map[string]any) error {
	if deviceID == "" {
		return errors.New("store: deviceID is required")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	item := DeviceTelemetry{
		PK:       "DEVICE#" + deviceID,
		SK:       "TELEM",
		Gsi2PK:   "DEVSEEN#",
		Gsi2SK:   now,
		DeviceID: deviceID,
		LastSeen: now,
		Payload:  payload,
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("store: marshal device telemetry: %w", err)
	}

	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("store: put device telemetry: %w", err)
	}
	return nil
}

// ReleaseSessionSlot deletes the realtime concurrency slot the broker
// wrote at mint (pk=USER#<uid>, sk=BUCKET#sess#<sessionId> — the prefix
// mirrors internal/realtime's sessSlotPrefix). Called from the transcript
// route's final flush so a deliberately-ended session frees its slot
// immediately instead of burning the full 10-minute hard cap; deleting a
// missing item is a DynamoDB no-op, so this is safely idempotent.
func (s *Store) ReleaseSessionSlot(ctx context.Context, userID, sessionID string) error {
	if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &types.AttributeValueMemberS{Value: "BUCKET#sess#" + sessionID},
		},
	}); err != nil {
		return fmt.Errorf("store: release session slot: %w", err)
	}
	return nil
}

// ConditionalPut writes an item under pk/sk only if no item currently
// occupies that key — the idempotency primitive used by email-dispatch
// (pk=IDEMP#<messageId>, sk=IDEMP) and anywhere else "process exactly
// once" is required. When ttlUnix is non-zero it is stored as the
// table's `ttl` attribute so DynamoDB expires the marker automatically.
// Returns ErrAlreadyExists (not a generic AWS error) when the conditional
// check fails, so callers can treat that as an idempotent no-op.
func (s *Store) ConditionalPut(ctx context.Context, pk, sk string, attrs map[string]any, ttlUnix int64) error {
	item := make(map[string]any, len(attrs)+3)
	for k, v := range attrs {
		item[k] = v
	}
	item["pk"] = pk
	item["sk"] = sk
	if ttlUnix > 0 {
		item["ttl"] = ttlUnix
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("store: marshal conditional put item: %w", err)
	}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: conditional put: %w", err)
	}
	return nil
}

// QueryUsageToday queries the USAGE#<day> partition (day formatted
// "2006-01-02"). This is usage-rollup's M0 access pattern: Query only,
// never Scan — the partition is simply empty until M2 starts writing
// per-session usage records into it.
func (s *Store) QueryUsageToday(ctx context.Context, day string) ([]map[string]any, error) {
	pk := "USAGE#" + day
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: pk},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: query usage: %w", err)
	}

	items := make([]map[string]any, 0, len(out.Items))
	for _, raw := range out.Items {
		var m map[string]any
		if err := attributevalue.UnmarshalMap(raw, &m); err != nil {
			return nil, fmt.Errorf("store: unmarshal usage item: %w", err)
		}
		items = append(items, m)
	}
	return items, nil
}

// queryAllPages runs a Query to exhaustion, following LastEvaluatedKey
// pagination, and returns the concatenated raw items. Every caller passes
// a single-partition KeyConditionExpression (table or GSI) — never a Scan.
func (s *Store) queryAllPages(ctx context.Context, in *dynamodb.QueryInput) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	for {
		out, err := s.client.Query(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("store: query: %w", err)
		}
		items = append(items, out.Items...)
		if out.LastEvaluatedKey == nil || len(out.LastEvaluatedKey) == 0 {
			return items, nil
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

// GetItem fetches a single item by its full key. Exposed for callers that
// need a raw key lookup beyond the typed helpers above.
func (s *Store) GetItem(ctx context.Context, pk, sk string) (map[string]any, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: pk},
			"sk": &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get item: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}

	var m map[string]any
	if err := attributevalue.UnmarshalMap(out.Item, &m); err != nil {
		return nil, fmt.Errorf("store: unmarshal item: %w", err)
	}
	return m, nil
}
