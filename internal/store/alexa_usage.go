package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// AlexaUsage contains numbers and model identity only. Reported=false records
// an uncertain provider outcome; it never presents missing usage as measured zero.
type AlexaUsage struct {
	Model    string  `dynamodbav:"model"`
	Reported bool    `dynamodbav:"reported"`
	Input    int64   `dynamodbav:"inputTokens"`
	Output   int64   `dynamodbav:"outputTokens"`
	Cached   int64   `dynamodbav:"cachedTokens"`
	CostUSD  float64 `dynamodbav:"estimatedCostUSD"`
}

func (s *Store) RecordAlexaUsage(ctx context.Context, owner, receipt string, round int, now time.Time, usage AlexaUsage) error {
	if owner == "" || receipt == "" || round < 0 || round >= 5 || usage.Input < 0 || usage.Output < 0 || usage.Cached < 0 || usage.Cached > usage.Input || math.IsNaN(usage.CostUSD) || math.IsInf(usage.CostUSD, 0) || usage.CostUSD < 0 {
		return errors.New("invalid Alexa usage")
	}
	item, err := attributevalue.MarshalMap(usage)
	if err != nil {
		return err
	}
	item["pk"] = &types.AttributeValueMemberS{Value: userPK(owner)}
	item["sk"] = &types.AttributeValueMemberS{Value: fmt.Sprintf("ALEXAUSAGE#%s#%d", receipt, round)}
	item["ts"] = &types.AttributeValueMemberS{Value: now.UTC().Format(time.RFC3339Nano)}
	item["ttl"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(logTTL).Unix(), 10)}
	updates := []types.TransactWriteItem{{Put: &types.Put{TableName: aws.String(s.table), Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")}}}
	for _, period := range []struct{ key, field string }{{DayPeriod(now), "dayTokens"}, {MonthPeriod(now), "monthTokens"}} {
		updates = append(updates, types.TransactWriteItem{Update: &types.Update{TableName: aws.String(s.table), Key: keyOf(userPK(owner), usageSK(period.key)),
			UpdateExpression: aws.String("SET updatedAt = :now ADD #tokens :tokens"), ExpressionAttributeNames: map[string]string{"#tokens": period.field},
			ExpressionAttributeValues: map[string]types.AttributeValue{":now": &types.AttributeValueMemberS{Value: now.UTC().Format(time.RFC3339)}, ":tokens": &types.AttributeValueMemberN{Value: strconv.FormatInt(usage.Input+usage.Output, 10)}}}})
	}
	updates = append(updates, types.TransactWriteItem{Put: &types.Put{TableName: aws.String(s.table), Item: map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: pkConfig}, "sk": &types.AttributeValueMemberS{Value: "ACTIVEUSER#" + owner + "#" + DayPeriod(now)},
		"userId": &types.AttributeValueMemberS{Value: owner}, "ttl": &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(activeMarkerTTL).Unix(), 10)},
	}}}, s.alexaOwnerCondition(owner), s.alexaProfileCondition(owner))
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: updates})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) && len(cancelled.CancellationReasons) == len(updates) && aws.ToString(cancelled.CancellationReasons[0].Code) == "ConditionalCheckFailed" {
		for _, r := range cancelled.CancellationReasons[1:] {
			if code := aws.ToString(r.Code); code != "" && code != "None" {
				return err
			}
		}
		return nil
	}
	return err
}
