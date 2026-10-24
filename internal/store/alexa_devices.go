package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
)

const SurfaceAlexa = "alexa"

func AlexaDeviceID(owner, skill, device string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("live-ninja/alexa/"+owner+"/"+skill+"/"+device)).String()
}

func (s *Store) activeAlexaOwner(ctx context.Context, owner string) error {
	b, err := s.GetOwner(ctx)
	if err != nil {
		return err
	}
	if b == nil || b.UserID != owner {
		return ErrDeviceOwnership
	}
	u, err := s.GetUserConsistent(ctx, owner)
	if err != nil {
		return err
	}
	if u == nil || u.Role != RoleOwner || u.Status != UserStatusActive {
		return ErrDeviceOwnership
	}
	return nil
}

// EnsureAlexaDevice is called only after linked-account verification. It never
// resurrects a revoked device or replaces a customized website name.
func (s *Store) EnsureAlexaDevice(ctx context.Context, owner, skill, device string) (*Device, error) {
	if owner == "" || skill == "" || device == "" || len(device) > 1024 {
		return nil, ErrDeviceOwnership
	}
	if err := s.activeAlexaOwner(ctx, owner); err != nil {
		return nil, err
	}
	id := AlexaDeviceID(owner, skill, device)
	existing, err := s.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.UserID != owner || existing.Surface != SurfaceAlexa || existing.FamilyID != "" || existing.CertID != "" || existing.ThingName != "" {
			return nil, ErrDeviceOwnership
		}
		if existing.Status != DeviceStatusActive {
			return nil, ErrDeviceRevoked
		}
		if existing.AlexaGeneration == "" {
			return nil, ErrDeviceBindingConflict
		}
		now := time.Now().UTC()
		_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(s.table), Key: keyOf(devicePK(id), skMeta),
			UpdateExpression:          aws.String("SET lastSeenAt = :now, gsi2sk = :seen"),
			ConditionExpression:       aws.String("userId = :owner AND #status = :active AND alexaGeneration = :generation"),
			ExpressionAttributeNames:  map[string]string{"#status": "status"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":owner": &types.AttributeValueMemberS{Value: owner}, ":active": &types.AttributeValueMemberS{Value: DeviceStatusActive}, ":generation": &types.AttributeValueMemberS{Value: existing.AlexaGeneration}, ":now": &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Unix(), 10)}, ":seen": &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)}},
		})
		if err != nil {
			return nil, err
		}
		existing.LastSeenAt = now.Unix()
		return existing, nil
	}
	now := time.Now().UTC()
	d := Device{DeviceID: id, UserID: owner, Name: "Alexa", Surface: SurfaceAlexa, Status: DeviceStatusActive, Capabilities: []string{SettingsSectionAboutYou, SettingsSectionPersona, SettingsSectionPrivacy}, Metadata: map[string]string{"managedBy": "server"}, CreatedAt: now.Unix(), UpdatedAt: now.Unix(), LastSeenAt: now.Unix()}
	d.AlexaGeneration = uuid.NewString()
	av, err := attributevalue.MarshalMap(deviceItem{PK: devicePK(id), SK: skMeta, Gsi2PK: gsi2pkDevSeen, Gsi2SK: now.Format(time.RFC3339), Device: d})
	if err != nil {
		return nil, err
	}
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: aws.String(s.table), Item: av, ConditionExpression: aws.String("attribute_not_exists(pk)")}},
		s.alexaOwnerCondition(owner), s.alexaProfileCondition(owner),
	}})
	if err != nil {
		var collision *types.TransactionCanceledException
		if errors.As(err, &collision) && len(collision.CancellationReasons) == 3 && aws.ToString(collision.CancellationReasons[0].Code) == "ConditionalCheckFailed" {
			for _, r := range collision.CancellationReasons[1:] {
				if c := aws.ToString(r.Code); c != "" && c != "None" {
					return nil, err
				}
			}
			winner, e := s.GetDevice(ctx, id)
			if e != nil {
				return nil, e
			}
			if winner == nil || winner.UserID != owner || winner.Surface != SurfaceAlexa {
				return nil, ErrDeviceOwnership
			}
			if winner.Status != DeviceStatusActive {
				return nil, ErrDeviceRevoked
			}
			return winner, nil
		}
		return nil, err
	}
	return &d, nil
}

func (s *Store) alexaOwnerCondition(owner string) types.TransactWriteItem {
	return types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(s.table), Key: keyOf(pkConfig, skOwner), ConditionExpression: aws.String("userId = :owner"), ExpressionAttributeValues: map[string]types.AttributeValue{":owner": &types.AttributeValueMemberS{Value: owner}}}}
}
func (s *Store) alexaProfileCondition(owner string) types.TransactWriteItem {
	return types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(s.table), Key: keyOf(userPK(owner), skProfile), ConditionExpression: aws.String("#role = :role AND #status = :status"), ExpressionAttributeNames: map[string]string{"#role": "role", "#status": "status"}, ExpressionAttributeValues: map[string]types.AttributeValue{":role": &types.AttributeValueMemberS{Value: RoleOwner}, ":status": &types.AttributeValueMemberS{Value: UserStatusActive}}}}
}

// ReenableAlexaDevice is an explicit owner website operation, never an effect of
// a voice request. The conditional update leaves the name and overrides intact.
func (s *Store) ReenableAlexaDevice(ctx context.Context, owner, id string) error {
	if IsManagedSettingsDevice(owner, id) {
		return ErrDeviceBindingConflict
	}
	if err := s.activeAlexaOwner(ctx, owner); err != nil {
		return err
	}
	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: keyOf(devicePK(id), skMeta),
			UpdateExpression:         aws.String("SET #status = :active, updatedAt = :now, alexaGeneration = :generation"),
			ConditionExpression:      aws.String("userId = :owner AND surface = :surface AND #status = :revoked"),
			ExpressionAttributeNames: map[string]string{"#status": "status"},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":owner":      &types.AttributeValueMemberS{Value: owner},
				":surface":    &types.AttributeValueMemberS{Value: SurfaceAlexa},
				":active":     &types.AttributeValueMemberS{Value: DeviceStatusActive},
				":revoked":    &types.AttributeValueMemberS{Value: DeviceStatusRevoked},
				":now":        &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Unix(), 10)},
				":generation": &types.AttributeValueMemberS{Value: uuid.NewString()},
			},
		}},
		s.alexaOwnerCondition(owner), s.alexaProfileCondition(owner),
	}})
	var conflict *types.TransactionCanceledException
	if errors.As(err, &conflict) && len(conflict.CancellationReasons) == 3 && aws.ToString(conflict.CancellationReasons[0].Code) == "ConditionalCheckFailed" {
		for _, reason := range conflict.CancellationReasons[1:] {
			if c := aws.ToString(reason.Code); c != "" && c != "None" {
				return err
			}
		}
		current, e := s.GetDevice(ctx, id)
		if e != nil {
			return e
		}
		if current != nil && current.UserID == owner && current.Surface == SurfaceAlexa && current.Status == DeviceStatusActive && current.AlexaGeneration != "" {
			return nil
		}
	}
	return err
}
