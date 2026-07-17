// Command usage-rollup runs hourly off an EventBridge rate(1 hour)
// schedule (see template.yaml).
//
// M2 real behavior (per plan.md / contracts/metering.md): find every user
// active "today" via their pk=CONFIG sk=ACTIVEUSER#<uid>#<YYYY-MM-DD>
// marker (written by the transcript sink on each turn it persists — a
// single Query against the CONFIG partition, never a Scan, and naturally
// bounded since those markers TTL out after a day or two), then for each
// active user recompute their pk=USER#<uid> sk=USAGE#<YYYY-MM> monthly
// rollup by summing the dayTokens/daySeconds already accrued on that same
// user's day-level USAGE#<YYYY-MM-DD> items (written in real time by the
// realtime broker / tool router as sessions complete). This function
// never writes the day-level counters itself — only the derived monthly
// aggregate that contracts/metering.md's mint-time monthly_tokens check
// reads — so a concurrent real-time atomic UpdateItem ADD on a day item
// can never race against this recompute. Both the active-user lookup and
// the per-user rollup are single-partition Query calls (CONFIG and
// USER#<uid> respectively), never a table Scan.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
)

const (
	configPK         = "CONFIG"
	activeUserPrefix = "ACTIVEUSER#"
	usagePrefix      = "USAGE#"
)

// ddbAPI is the subset of the DynamoDB client this function depends on,
// so tests can inject a fake without a real table. usage-rollup talks to
// DynamoDB directly (rather than through internal/store) because the
// access patterns it needs — a CONFIG-partition marker scan and a
// per-user USAGE#-prefix aggregation — aren't part of that package's
// dictated method set for this milestone.
type ddbAPI interface {
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// usageItem is the subset of a USAGE# item's attributes this function
// reads. Only day-level items (sk = USAGE#<YYYY-MM-DD>) carry
// dayTokens/daySeconds; the monthly item itself (sk = USAGE#<YYYY-MM>) is
// skipped when summing since it's the recompute target, not an input.
type usageItem struct {
	SK         string  `dynamodbav:"sk"`
	DayTokens  float64 `dynamodbav:"dayTokens"`
	DaySeconds float64 `dynamodbav:"daySeconds"`
}

var (
	logger    = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	ddb       ddbAPI
	tableName string
)

func handler(ctx context.Context, _ events.CloudWatchEvent) error {
	l := observ.WithRequest(logger, "", "", "system")

	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	month := now.Format("2006-01")

	uids, err := queryActiveUsersToday(ctx, ddb, tableName, today)
	if err != nil {
		l.Error("usage-rollup: query active users failed", slog.String("error", err.Error()), slog.String("day", today))
		return err
	}

	var failures []error
	rolledUp := 0
	for _, uid := range uids {
		if err := rollupUser(ctx, ddb, tableName, uid, month); err != nil {
			l.Error("usage-rollup: rollup failed for user",
				slog.String("userId", uid), slog.String("error", err.Error()))
			failures = append(failures, err)
			continue
		}
		rolledUp++
	}

	l.Info("usage-rollup: run complete",
		slog.String("day", today),
		slog.String("month", month),
		slog.Int("activeUsers", len(uids)),
		slog.Int("rolledUp", rolledUp))

	observ.EmitMetric("LiveNinja/UsageRollup", "UsageRollupRuns", 1, "Count",
		map[string]string{"Day": today})
	observ.EmitMetric("LiveNinja/UsageRollup", "UsersRolledUp", float64(rolledUp), "Count",
		map[string]string{"Day": today})

	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

// queryActiveUsersToday returns the distinct userIds with a
// pk=CONFIG sk=ACTIVEUSER#<uid>#<today> marker. A single Query against
// the CONFIG partition (never a Scan): begins_with narrows to the
// ACTIVEUSER# key range (excluding the OWNER/ALLOW# items that also live
// in CONFIG), and a FilterExpression narrows further to markers whose sk
// ends in today's date.
func queryActiveUsersToday(ctx context.Context, client ddbAPI, table, today string) ([]string, error) {
	dateSuffix := "#" + today

	out, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :prefix)"),
		FilterExpression:       aws.String("contains(sk, :suffix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: configPK},
			":prefix": &types.AttributeValueMemberS{Value: activeUserPrefix},
			":suffix": &types.AttributeValueMemberS{Value: dateSuffix},
		},
		ProjectionExpression: aws.String("sk"),
	})
	if err != nil {
		return nil, fmt.Errorf("usage-rollup: query active users: %w", err)
	}

	seen := make(map[string]bool, len(out.Items))
	uids := make([]string, 0, len(out.Items))
	for _, raw := range out.Items {
		skAttr, ok := raw["sk"].(*types.AttributeValueMemberS)
		if !ok {
			continue
		}
		uid := strings.TrimSuffix(strings.TrimPrefix(skAttr.Value, activeUserPrefix), dateSuffix)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		uids = append(uids, uid)
	}
	return uids, nil
}

// rollupUser recomputes one user's monthly USAGE#<YYYY-MM> aggregate by
// summing dayTokens/daySeconds off every USAGE#<YYYY-MM-DD> item recorded
// so far this month. A single Query against that user's own partition
// (pk=USER#<uid>, begins_with(sk, "USAGE#<month>")) picks up both the
// day items and the pre-existing monthly item in one call; the monthly
// item itself is excluded from the sum (it's the write target, not an
// input) by its shorter sk.
func rollupUser(ctx context.Context, client ddbAPI, table, uid, month string) error {
	pk := "USER#" + uid
	monthSK := usagePrefix + month

	out, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: pk},
			":prefix": &types.AttributeValueMemberS{Value: monthSK},
		},
	})
	if err != nil {
		return fmt.Errorf("usage-rollup: query usage items for %s: %w", uid, err)
	}

	var monthTokens, monthSeconds float64
	for _, raw := range out.Items {
		var item usageItem
		if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
			return fmt.Errorf("usage-rollup: unmarshal usage item for %s: %w", uid, err)
		}
		if item.SK == monthSK {
			continue // the monthly item itself - recompute target, not an input
		}
		monthTokens += item.DayTokens
		monthSeconds += item.DaySeconds
	}

	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: pk},
			"sk": &types.AttributeValueMemberS{Value: monthSK},
		},
		UpdateExpression: aws.String("SET monthTokens = :mt, monthSeconds = :ms, updatedAt = :ua"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":mt": &types.AttributeValueMemberN{Value: strconv.FormatFloat(monthTokens, 'f', -1, 64)},
			":ms": &types.AttributeValueMemberN{Value: strconv.FormatFloat(monthSeconds, 'f', -1, 64)},
			":ua": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("usage-rollup: update monthly usage for %s: %w", uid, err)
	}
	return nil
}

func main() {
	ctx := context.Background()
	cfg := config.FromEnv()
	tableName = cfg.TableName

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("usage-rollup: aws config load failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	ddb = dynamodb.NewFromConfig(awsCfg)

	lambda.Start(handler)
}
