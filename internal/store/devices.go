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

var (
	// ErrDeviceOwnership is deliberately ambiguous at the HTTP boundary:
	// callers map it to not-found so a user cannot enumerate another
	// account's stable device ids.
	ErrDeviceOwnership = errors.New("store: device belongs to another user")
	// ErrDeviceRevoked prevents a browser/app whose local storage survived a
	// revoke from silently resurrecting the same device identity.
	ErrDeviceRevoked = errors.New("store: device is revoked")
	// ErrDeviceBindingConflict means an authenticated session was already
	// bound to a different stable device id.
	ErrDeviceBindingConflict = errors.New("store: session is already bound to another device")
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
	if d.Surface == "" {
		d.Surface = SurfaceDevice
	}
	if d.LastSeenAt == 0 {
		d.LastSeenAt = d.CreatedAt
	}
	if d.UpdatedAt == 0 {
		d.UpdatedAt = d.CreatedAt
	}
	it := deviceItem{
		PK:     devicePK(d.DeviceID),
		SK:     skMeta,
		Gsi2PK: gsi2pkDevSeen,
		Gsi2SK: time.Unix(d.LastSeenAt, 0).UTC().Format(time.RFC3339),
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

// UpsertClientDevice registers or refreshes a stable web/Android host
// identity. It reuses the DEVICE#<id>/META directory so existing device
// pickers, account purge, and the no-Scan GSI listing keep one source of
// truth. Credential/provisioning fields on an existing paired device are
// preserved. An inferred name is refreshed only until the owner explicitly
// renames the device.
func (s *Store) UpsertClientDevice(ctx context.Context, proposed *Device) (*Device, error) {
	return s.upsertClientDevice(ctx, proposed, 0)
}

func (s *Store) upsertClientDevice(ctx context.Context, proposed *Device, attempt int) (*Device, error) {
	if proposed == nil || proposed.DeviceID == "" || proposed.UserID == "" {
		return nil, errors.New("store: deviceID and userID are required")
	}
	if proposed.Surface != SurfaceWeb && proposed.Surface != SurfaceAndroid {
		return nil, errors.New("store: invalid device surface")
	}

	now := time.Now().UTC()
	existing, err := s.GetDevice(ctx, proposed.DeviceID)
	if err != nil {
		return nil, err
	}

	var next Device
	condition := "attribute_not_exists(pk)"
	var names map[string]string
	var values map[string]types.AttributeValue
	if existing == nil {
		next = *proposed
		next.Status = DeviceStatusActive
		next.CreatedAt = now.Unix()
		next.UpdatedAt = next.CreatedAt
		next.LastSeenAt = next.CreatedAt
	} else {
		if existing.UserID != proposed.UserID {
			return nil, ErrDeviceOwnership
		}
		if existing.Status == DeviceStatusRevoked {
			return nil, ErrDeviceRevoked
		}
		if existing.Surface == SurfaceDevice || existing.ThingName != "" ||
			existing.CertArn != "" || existing.CertID != "" {
			return nil, ErrDeviceBindingConflict
		}
		if existing.Surface != "" && existing.Surface != proposed.Surface {
			return nil, ErrDeviceBindingConflict
		}
		next = *existing
		if !existing.NameCustomized && proposed.Name != "" {
			next.Name = proposed.Name
		}
		next.Surface = proposed.Surface
		next.Metadata = proposed.Metadata
		next.Capabilities = proposed.Capabilities
		if proposed.FamilyID != "" {
			next.FamilyID = proposed.FamilyID
		}
		next.UpdatedAt = now.Unix()
		next.LastSeenAt = now.Unix()
		condition = "#uid = :uid AND #status = :status"
		names = map[string]string{"#uid": "userId", "#status": "status"}
		values = map[string]types.AttributeValue{
			":uid":    &types.AttributeValueMemberS{Value: proposed.UserID},
			":status": &types.AttributeValueMemberS{Value: DeviceStatusActive},
		}
		// A whole-item refresh must never roll back a concurrent rename.
		// Name is always present; nameCustomized is omitted while false, so
		// the condition covers both a different-name rename and a same-name
		// rename that only flips the customization bit.
		names["#name"] = "name"
		names["#custom"] = "nameCustomized"
		values[":expectedName"] = &types.AttributeValueMemberS{Value: existing.Name}
		condition += " AND #name = :expectedName"
		if existing.NameCustomized {
			values[":custom"] = &types.AttributeValueMemberBOOL{Value: true}
			condition += " AND #custom = :custom"
		} else {
			condition += " AND attribute_not_exists(#custom)"
		}
	}
	if next.Name == "" {
		next.Name = "Live Ninja Device"
	}

	it := deviceItem{
		PK:     devicePK(next.DeviceID),
		SK:     skMeta,
		Gsi2PK: gsi2pkDevSeen,
		Gsi2SK: now.Format(time.RFC3339),
		Device: next,
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return nil, fmt.Errorf("store: marshal client device: %w", err)
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 aws.String(s.table),
		Item:                      av,
		ConditionExpression:       aws.String(condition),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			// Resolve both a create collision and a concurrent rename/revoke
			// from a fresh base-table read. A same-owner active winner retries
			// from that state, preserving any newly customized name.
			winner, getErr := s.GetDevice(ctx, proposed.DeviceID)
			if getErr != nil {
				return nil, getErr
			}
			if winner == nil || winner.UserID != proposed.UserID {
				return nil, ErrDeviceOwnership
			}
			if winner.Status != DeviceStatusActive {
				return nil, ErrDeviceRevoked
			}
			if attempt >= 4 {
				return nil, ErrDeviceBindingConflict
			}
			return s.upsertClientDevice(ctx, proposed, attempt+1)
		}
		return nil, fmt.Errorf("store: upsert client device: %w", err)
	}
	return &next, nil
}

// RenameDevice sets the owner-visible name and marks it customized so later
// host-info refreshes cannot replace it with an inferred browser/model name.
func (s *Store) RenameDevice(ctx context.Context, userID, deviceID, name string) (*Device, error) {
	if userID == "" || deviceID == "" || name == "" {
		return nil, errors.New("store: userID, deviceID and name are required")
	}
	now := time.Now().UTC().Unix()
	out, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(s.table),
		Key:              keyOf(devicePK(deviceID), skMeta),
		UpdateExpression: aws.String("SET #name = :name, #custom = :custom, #updated = :updated"),
		ConditionExpression: aws.String(
			"attribute_exists(pk) AND #uid = :uid AND #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#name":    "name",
			"#custom":  "nameCustomized",
			"#updated": "updatedAt",
			"#uid":     "userId",
			"#status":  "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":name":    &types.AttributeValueMemberS{Value: name},
			":custom":  &types.AttributeValueMemberBOOL{Value: true},
			":updated": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
			":uid":     &types.AttributeValueMemberS{Value: userID},
			":status":  &types.AttributeValueMemberS{Value: DeviceStatusActive},
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: rename device: %w", err)
	}
	var it deviceItem
	if err := attributevalue.UnmarshalMap(out.Attributes, &it); err != nil {
		return nil, fmt.Errorf("store: unmarshal renamed device: %w", err)
	}
	d := it.Device
	return &d, nil
}

// DetachDeviceFamily clears a stale device-to-refresh-family link after a
// web/Android session replaces its locally-reset installation id. The
// expected-family condition prevents a concurrent re-login on the old device
// from being detached accidentally.
func (s *Store) DetachDeviceFamily(ctx context.Context, userID, deviceID, expectedFamilyID string) error {
	if userID == "" || deviceID == "" || expectedFamilyID == "" {
		return errors.New("store: userID, deviceID and expectedFamilyID are required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(s.table),
		Key:              keyOf(devicePK(deviceID), skMeta),
		UpdateExpression: aws.String("SET #family = :empty, #updated = :updated"),
		ConditionExpression: aws.String(
			"attribute_exists(pk) AND #uid = :uid AND #family = :expected"),
		ExpressionAttributeNames: map[string]string{
			"#family":  "familyId",
			"#updated": "updatedAt",
			"#uid":     "userId",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":empty":    &types.AttributeValueMemberS{Value: ""},
			":updated":  &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", time.Now().UTC().Unix())},
			":uid":      &types.AttributeValueMemberS{Value: userID},
			":expected": &types.AttributeValueMemberS{Value: expectedFamilyID},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrDeviceBindingConflict
		}
		return fmt.Errorf("store: detach device family: %w", err)
	}
	return nil
}

// GetDevice fetches DEVICE#<deviceId>/META. Returns (nil, nil) when
// absent.
func (s *Store) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            keyOf(devicePK(deviceID), skMeta),
		ConsistentRead: aws.Bool(true),
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
