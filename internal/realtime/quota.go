package realtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Quota gate per contracts/metering.md — enforcement is always PRE-SPEND:
// every check here runs and settles before the broker touches OpenAI, so a
// rejected mint costs nothing. Three independent limits:
//
//  1. Token bucket (mint rate): 1 token / 5s, burst 3 — conditional
//     UpdateItem on pk=USER#<uid>/sk=BUCKET#mint, never read-then-write.
//     Exhausted -> RateLimitedError (HTTP 429).
//  2. Daily realtime-minutes cap (~30 min/UTC day) from the same-day
//     USAGE#<YYYY-MM-DD> item's daySeconds -> QuotaExceededError
//     kind=daily_minutes (HTTP 402).
//  3. Monthly token ceiling (~$15/mo expressed as tokens) from the
//     USAGE#<YYYY-MM> item's monthTokens (maintained by usage-rollup) ->
//     QuotaExceededError kind=monthly_tokens (HTTP 402).
//
// Check order: bucket, then daily, then monthly (metering.md: daily
// before monthly; the rate check runs first and independently). A soft
// warning fires at >=80% of either spend cap on successful mints.
const (
	bucketCapacity      = 3.0
	bucketRefillSeconds = 5

	// defaultDailySecondsCap is ~30 minutes of realtime audio per user
	// per UTC day (metering.md default).
	defaultDailySecondsCap = 1800.0

	// defaultMonthlyTokenCap expresses the ~$15/month ceiling as a token
	// count at current gpt-realtime audio pricing (~$32/1M audio input,
	// ~$64/1M audio output; blended ~$40/1M -> ~375k tokens ≈ $15).
	defaultMonthlyTokenCap = 375000.0

	softWarnPercent = 80
)

// Env overrides so the caps can be tuned without a code change (plain
// numbers; unset/invalid falls back to the defaults above).
const (
	envDailySecondsCap = "QUOTA_DAILY_SECONDS"
	envMonthlyTokenCap = "QUOTA_MONTH_TOKENS"
)

// QuotaExceededError maps to the 402 hard-cap contract in metering.md.
// Used/Limit are already in the contract's display units: minutes for
// kind=daily_minutes, tokens for kind=monthly_tokens.
type QuotaExceededError struct {
	Kind    string // "daily_minutes" | "monthly_tokens"
	Used    float64
	Limit   float64
	ResetAt time.Time
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("quota exceeded: %s used %.1f of %.0f, resets %s",
		e.Kind, e.Used, e.Limit, e.ResetAt.Format(time.RFC3339))
}

// RateLimitedError maps to the 429 token-bucket contract in metering.md.
type RateLimitedError struct {
	RetryAfterSeconds int
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited: retry after %ds", e.RetryAfterSeconds)
}

// gateDDB is the subset of the DynamoDB client the gate needs, injectable
// for tests.
type gateDDB interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// Gate implements the metering/quota gate against the live-ninja table.
type Gate struct {
	ddb   gateDDB
	table string
	now   func() time.Time

	dailySecondsCap float64
	monthlyTokenCap float64
}

// NewGate builds a Gate over the given DynamoDB client and table,
// applying QUOTA_DAILY_SECONDS / QUOTA_MONTH_TOKENS env overrides.
func NewGate(ddb gateDDB, table string) *Gate {
	return &Gate{
		ddb:             ddb,
		table:           table,
		now:             time.Now,
		dailySecondsCap: envFloat(envDailySecondsCap, defaultDailySecondsCap),
		monthlyTokenCap: envFloat(envMonthlyTokenCap, defaultMonthlyTokenCap),
	}
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return def
	}
	return f
}

// CheckMint runs the full pre-spend gate for a session mint. On success
// it returns the soft-cap warnings ("daily_minutes=83%"-style pairs, per
// metering.md's X-LN-Quota-Warning format), possibly empty. On rejection
// the returned error is a *RateLimitedError or *QuotaExceededError.
func (g *Gate) CheckMint(ctx context.Context, userID string) ([]string, error) {
	if err := g.takeToken(ctx, userID); err != nil {
		return nil, err
	}

	now := g.now().UTC()

	daySeconds, err := g.readUsageNumber(ctx, userID, "USAGE#"+now.Format("2006-01-02"), "daySeconds")
	if err != nil {
		return nil, err
	}
	if daySeconds >= g.dailySecondsCap {
		return nil, &QuotaExceededError{
			Kind:    "daily_minutes",
			Used:    math.Round(daySeconds/60*10) / 10,
			Limit:   math.Round(g.dailySecondsCap / 60),
			ResetAt: nextUTCMidnight(now),
		}
	}

	monthTokens, err := g.checkMonthly(ctx, userID, now)
	if err != nil {
		return nil, err
	}

	var warnings []string
	if pct := int(math.Floor(daySeconds * 100 / g.dailySecondsCap)); pct >= softWarnPercent {
		warnings = append(warnings, fmt.Sprintf("daily_minutes=%d%%", pct))
	}
	if pct := int(math.Floor(monthTokens * 100 / g.monthlyTokenCap)); pct >= softWarnPercent {
		warnings = append(warnings, fmt.Sprintf("monthly_tokens=%d%%", pct))
	}
	return warnings, nil
}

// CheckFallback gates the text/STT/TTS fallback modes: token bucket (the
// same abuse guard — fallback turns are still per-interaction requests)
// plus the monthly spend ceiling. The daily-minutes cap is specific to
// realtime audio sessions and does not block the degraded fallback path.
func (g *Gate) CheckFallback(ctx context.Context, userID string) error {
	if err := g.takeToken(ctx, userID); err != nil {
		return err
	}
	_, err := g.checkMonthly(ctx, userID, g.now().UTC())
	return err
}

func (g *Gate) checkMonthly(ctx context.Context, userID string, now time.Time) (float64, error) {
	monthTokens, err := g.readUsageNumber(ctx, userID, "USAGE#"+now.Format("2006-01"), "monthTokens")
	if err != nil {
		return 0, err
	}
	if monthTokens >= g.monthlyTokenCap {
		return 0, &QuotaExceededError{
			Kind:    "monthly_tokens",
			Used:    math.Round(monthTokens),
			Limit:   g.monthlyTokenCap,
			ResetAt: nextMonthStart(now),
		}
	}
	return monthTokens, nil
}

// takeToken consumes one token from the per-user mint bucket
// (pk=USER#<uid>/sk=BUCKET#mint; tokens float, lastRefill unix). The
// refill computation happens optimistically in code, but the write is a
// conditional UpdateItem asserting the exact (tokens, lastRefill) pair it
// was computed from — a concurrent consumer fails the condition and
// retries, so two callers can never both spend the last token (no
// read-then-write race, per metering.md).
func (g *Gate) takeToken(ctx context.Context, userID string) error {
	pk := "USER#" + userID
	const sk = "BUCKET#mint"

	for attempt := 0; attempt < 4; attempt++ {
		nowUnix := g.now().UTC().Unix()

		out, err := g.ddb.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:      aws.String(g.table),
			ConsistentRead: aws.Bool(true),
			Key: map[string]ddbtypes.AttributeValue{
				"pk": &ddbtypes.AttributeValueMemberS{Value: pk},
				"sk": &ddbtypes.AttributeValueMemberS{Value: sk},
			},
		})
		if err != nil {
			return fmt.Errorf("realtime: read mint bucket: %w", err)
		}

		if out.Item == nil {
			// First mint for this user: create the bucket already down
			// one token. attribute_not_exists loses to a concurrent
			// creator, in which case we retry against the real item.
			_, err := g.ddb.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:           aws.String(g.table),
				ConditionExpression: aws.String("attribute_not_exists(pk)"),
				Item: map[string]ddbtypes.AttributeValue{
					"pk":         &ddbtypes.AttributeValueMemberS{Value: pk},
					"sk":         &ddbtypes.AttributeValueMemberS{Value: sk},
					"tokens":     numberAV(bucketCapacity - 1),
					"lastRefill": numberAV(float64(nowUnix)),
				},
			})
			if err == nil {
				return nil
			}
			if isConditionalFailure(err) {
				continue
			}
			return fmt.Errorf("realtime: create mint bucket: %w", err)
		}

		var bucket struct {
			Tokens     float64 `dynamodbav:"tokens"`
			LastRefill int64   `dynamodbav:"lastRefill"`
		}
		if err := attributevalue.UnmarshalMap(out.Item, &bucket); err != nil {
			return fmt.Errorf("realtime: unmarshal mint bucket: %w", err)
		}

		// Refill in whole 5-second steps so unconsumed fractional refill
		// time is preserved by advancing lastRefill only by the steps
		// actually credited.
		elapsed := nowUnix - bucket.LastRefill
		if elapsed < 0 {
			elapsed = 0
		}
		steps := elapsed / bucketRefillSeconds
		avail := bucket.Tokens + float64(steps)
		newLastRefill := bucket.LastRefill + steps*bucketRefillSeconds
		if avail >= bucketCapacity {
			avail = bucketCapacity
			newLastRefill = nowUnix
		}

		if avail < 1 {
			return &RateLimitedError{RetryAfterSeconds: bucketRefillSeconds}
		}

		_, err = g.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(g.table),
			Key: map[string]ddbtypes.AttributeValue{
				"pk": &ddbtypes.AttributeValueMemberS{Value: pk},
				"sk": &ddbtypes.AttributeValueMemberS{Value: sk},
			},
			UpdateExpression:    aws.String("SET tokens = :t, lastRefill = :lr"),
			ConditionExpression: aws.String("tokens = :oldT AND lastRefill = :oldLR"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":t":     numberAV(avail - 1),
				":lr":    numberAV(float64(newLastRefill)),
				":oldT":  numberAV(bucket.Tokens),
				":oldLR": numberAV(float64(bucket.LastRefill)),
			},
		})
		if err == nil {
			return nil
		}
		if isConditionalFailure(err) {
			continue // lost a CAS race; re-read and retry
		}
		return fmt.Errorf("realtime: update mint bucket: %w", err)
	}

	// Persistent CAS contention only happens under a same-user request
	// storm — exactly what the bucket exists to reject.
	return &RateLimitedError{RetryAfterSeconds: bucketRefillSeconds}
}

// readUsageNumber reads one numeric attribute off a USAGE item, returning
// 0 when the item or attribute does not exist yet.
func (g *Gate) readUsageNumber(ctx context.Context, userID, sk, attr string) (float64, error) {
	out, err := g.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(g.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("realtime: read %s: %w", sk, err)
	}
	if out.Item == nil {
		return 0, nil
	}
	av, ok := out.Item[attr]
	if !ok {
		return 0, nil
	}
	n, ok := av.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, nil
	}
	f, err := strconv.ParseFloat(n.Value, 64)
	if err != nil {
		return 0, fmt.Errorf("realtime: parse %s.%s: %w", sk, attr, err)
	}
	return f, nil
}

// RecordMint performs the post-mint bookkeeping (shared spec: "writes
// session ledger LOG# seq 0 marker + bumps dayMints"): an atomic ADD of
// dayMints on today's USAGE item (created on first use) and a session
// ledger marker at LOG#<sessionId>#000000 with a 90-day TTL. The marker
// lets the transcript sink and usage-rollup anchor a session's turns even
// if the client never posts a transcript.
func (g *Gate) RecordMint(ctx context.Context, userID, sessionID, surface string) error {
	now := g.now().UTC()

	if _, err := g.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(g.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: "USAGE#" + now.Format("2006-01-02")},
		},
		UpdateExpression: aws.String("SET updatedAt = :ts ADD dayMints :one"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":ts":  &ddbtypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":one": numberAV(1),
		},
	}); err != nil {
		return fmt.Errorf("realtime: bump dayMints: %w", err)
	}

	if _, err := g.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(g.table),
		Item: map[string]ddbtypes.AttributeValue{
			"pk":      &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk":      &ddbtypes.AttributeValueMemberS{Value: fmt.Sprintf("LOG#%s#%06d", sessionID, 0)},
			"role":    &ddbtypes.AttributeValueMemberS{Value: "system"},
			"text":    &ddbtypes.AttributeValueMemberS{Value: "session-start"},
			"surface": &ddbtypes.AttributeValueMemberS{Value: surface},
			"engine":  &ddbtypes.AttributeValueMemberS{Value: "openai-realtime"},
			"ts":      &ddbtypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			"ttl":     numberAV(float64(now.Add(90 * 24 * time.Hour).Unix())),
		},
	}); err != nil {
		return fmt.Errorf("realtime: write session ledger marker: %w", err)
	}
	return nil
}

func numberAV(f float64) *ddbtypes.AttributeValueMemberN {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(f, 'f', -1, 64)}
}

func isConditionalFailure(err error) bool {
	var cf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &cf)
}

func nextUTCMidnight(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

func nextMonthStart(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
}
