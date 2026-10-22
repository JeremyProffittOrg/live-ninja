package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type AlexaExchange struct {
	Session, Device, Generation, Input, Output string
	StartedAt, At                              time.Time
	Sequence, RetentionDays                    int
	SettingsVersion                            int64
	CostUSD                                    float64
	Tokens                                     int
}

// SaveAlexaExchange writes the existing History and LOG formats atomically.
// It fences the active owner/device/settings so deletion or a privacy change
// cannot resurrect a transcript after the worker's last authority check.
func (s *Store) SaveAlexaExchange(ctx context.Context, owner string, x AlexaExchange) error {
	if x.Session == "" || x.Device == "" || x.Generation == "" || x.Sequence < 1 || x.RetentionDays <= 0 || x.RetentionDays > 365 || x.Input == "" || x.Output == "" {
		return errors.New("invalid Alexa transcript")
	}
	ts := x.StartedAt.UTC().Format(time.RFC3339)
	conv, err := s.GetConversation(ctx, owner, ts+"#"+x.Session)
	if err != nil {
		return err
	}
	if conv == nil {
		title := []rune(x.Input)
		if len(title) > 80 {
			title = title[:80]
		}
		conv = &Conversation{SessionID: x.Session, TS: ts, DeviceID: x.Device, Surface: SurfaceAlexa, Engine: "gpt-6-luna", Title: string(title), TopicIDs: []string{}}
	}
	conv.TurnCount += 2
	conv.CostUSD += x.CostUSD
	conv.CostTextTokens += x.Tokens
	ttl := x.At.Add(time.Duration(x.RetentionDays) * 24 * time.Hour).Unix()
	items := []types.TransactWriteItem{}
	for i, text := range []string{x.Input, x.Output} {
		role := "user"
		if i == 1 {
			role = "assistant"
		}
		av, e := attributevalue.MarshalMap(map[string]any{"pk": userPK(owner), "sk": fmt.Sprintf("LOG#%s#%06d", x.Session, x.Sequence+i), "role": role, "text": text, "ts": x.At.UTC().Format(time.RFC3339Nano), "surface": SurfaceAlexa, "deviceId": x.Device, "engine": "gpt-6-luna", "ttl": ttl})
		if e != nil {
			return e
		}
		items = append(items, types.TransactWriteItem{Put: &types.Put{TableName: aws.String(s.table), Item: av, ConditionExpression: aws.String("attribute_not_exists(pk)")}})
	}
	av, err := attributevalue.MarshalMap(conv)
	if err != nil {
		return err
	}
	av["pk"] = &types.AttributeValueMemberS{Value: userPK(owner)}
	av["sk"] = &types.AttributeValueMemberS{Value: conv.SK()}
	av["ttl"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(ttl, 10)}
	items = append(items, types.TransactWriteItem{Put: &types.Put{TableName: aws.String(s.table), Item: av}}, s.alexaOwnerCondition(owner), s.alexaProfileCondition(owner),
		types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(s.table), Key: keyOf(devicePK(x.Device), skMeta),
			ConditionExpression: aws.String("userId = :owner AND #status = :active AND alexaGeneration = :generation"), ExpressionAttributeNames: map[string]string{"#status": "status"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":owner": &types.AttributeValueMemberS{Value: owner}, ":active": &types.AttributeValueMemberS{Value: DeviceStatusActive}, ":generation": &types.AttributeValueMemberS{Value: x.Generation}}}})
	condition := "#version = :version"
	if x.SettingsVersion == 1 {
		condition = "attribute_not_exists(pk) OR #version = :version"
	}
	items = append(items, types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(s.table), Key: keyOf(userPK(owner), settingsSK), ConditionExpression: aws.String(condition), ExpressionAttributeNames: map[string]string{"#version": "version"}, ExpressionAttributeValues: map[string]types.AttributeValue{":version": &types.AttributeValueMemberN{Value: strconv.FormatInt(x.SettingsVersion, 10)}}}})
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	return err
}
