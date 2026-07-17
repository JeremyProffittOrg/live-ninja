package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// deviceItem is the raw DEVICE#<deviceId>/META row (Device + table/GSI
// keys). GSI2 (gsi2pk=DEVSEEN, gsi2sk=<lastSeen RFC3339>) is the
// recently-seen device feed; at creation gsi2sk starts at createdAt.
type deviceItem struct {
	PK     string `dynamodbav:"pk"`
	SK     string `dynamodbav:"sk"`
	Gsi2PK string `dynamodbav:"gsi2pk"`
	Gsi2SK string `dynamodbav:"gsi2sk"`
	Device
}

// CreateDevice writes a new DEVICE#<deviceId>/META row. Conditional on
// the key not existing — a deviceId collision is a bug or an attack, not
// an upsert. Fills CreatedAt/Status (active) if unset.
func (s *Store) CreateDevice(ctx context.Context, d *Device) error {
	if d.DeviceID == "" || d.UserID == "" || d.FamilyID == "" {
		return errors.New("store: deviceID, userID and familyID are required")
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = time.Now().Unix()
	}
	if d.Status == "" {
		d.Status = DeviceStatusActive
	}
	it := deviceItem{
		PK:     devicePK(d.DeviceID),
		SK:     skMeta,
		Gsi2PK: gsi2pkDevSeen,
		Gsi2SK: time.Unix(d.CreatedAt, 0).UTC().Format(time.RFC3339),
		Device: *d,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("store: marshal device: %w", err)
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: create device: %w", err)
	}
	return nil
}

// GetDevice fetches DEVICE#<deviceId>/META. Returns (nil, nil) when
// absent.
func (s *Store) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key:       keyOf(devicePK(deviceID), skMeta),
	})
	if err != nil {
		return nil, fmt.Errorf("store: get device: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var it deviceItem
	if err := attributevalue.UnmarshalMap(out.Item, &it); err != nil {
		return nil, fmt.Errorf("store: unmarshal device: %w", err)
	}
	d := it.Device
	return &d, nil
}

// ListDevices returns the user's devices via a Query on the GSI2 DEVSEEN
// feed partition with a userId filter. The fleet is bounded (owner +
// allowlist personal devices), so this reads a small partition — still a
// Query against one GSI partition key, never a table Scan.
func (s *Store) ListDevices(ctx context.Context, userID string) ([]Device, error) {
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(indexGSI2),
		KeyConditionExpression: aws.String("#g2pk = :pk"),
		FilterExpression:       aws.String("#uid = :uid AND sk = :meta"),
		ExpressionAttributeNames: map[string]string{
			"#g2pk": "gsi2pk",
			"#uid":  "userId",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":   &types.AttributeValueMemberS{Value: gsi2pkDevSeen},
			":uid":  &types.AttributeValueMemberS{Value: userID},
			":meta": &types.AttributeValueMemberS{Value: skMeta},
		},
		ScanIndexForward: aws.Bool(false), // most recently seen first
	})
	if err != nil {
		return nil, fmt.Errorf("store: list devices: %w", err)
	}
	devices := make([]Device, 0, len(raw))
	for _, r := range raw {
		var it deviceItem
		if err := attributevalue.UnmarshalMap(r, &it); err != nil {
			return nil, fmt.Errorf("store: unmarshal device: %w", err)
		}
		devices = append(devices, it.Device)
	}
	return devices, nil
}

// RevokeDevice marks a device revoked (status=revoked). The caller also
// revokes its refresh family (RevokeFamily) and, once M5 lands IoT
// provisioning, detaches its certificate. Returns ErrNotFound if the
// device row does not exist.
func (s *Store) RevokeDevice(ctx context.Context, deviceID string) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 keyOf(devicePK(deviceID), skMeta),
		UpdateExpression:    aws.String("SET #st = :revoked"),
		ConditionExpression: aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames: map[string]string{
			"#st": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":revoked": &types.AttributeValueMemberS{Value: DeviceStatusRevoked},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrNotFound
		}
		return fmt.Errorf("store: revoke device: %w", err)
	}
	return nil
}
