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

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
)

// Quota gate per contracts/metering.md — enforcement is always PRE-SPEND:
// every check here runs and settles before the broker touches OpenAI, so a
// rejected mint costs nothing. Checks, in order:
//
//  0. Suspension gate (M7 hardening): USER#<uid>/PROFILE status ==
//     "suspended" -> SuspendedError (HTTP 403). Read fresh on every mint
//     so a just-suspended user is cut off immediately, even inside the
//     authorizer's 60s user-cache window.
//  1. Token bucket (mint rate): 1 token / 5s, burst 3 — conditional
//     UpdateItem on pk=USER#<uid>/sk=BUCKET#mint, never read-then-write.
//     Exhausted -> RateLimitedError (HTTP 429).
//  2. Daily realtime-minutes cap (~30 min/UTC day) from the same-day
//     USAGE#<YYYY-MM-DD> item's daySeconds -> QuotaExceededError
//     kind=daily_minutes (HTTP 402).
//  3. Monthly token ceiling (~$15/mo expressed as tokens) from the
//     USAGE#<YYYY-MM> item's monthTokens (maintained by usage-rollup) ->
//     QuotaExceededError kind=monthly_tokens (HTTP 402).
//  4. Hourly-burn anomaly (M7 hardening): tokens consumed inside the
//     current UTC hour (dayTokens delta against the BUCKET#burn
//     snapshot) above QUOTA_HOURLY_BURN_TOKENS -> the user is
//     auto-suspended (PROFILE status=suspended + tokensValidAfter bump +
//     EMF UserAutoSuspended + best-effort owner alert) and the mint is
//     denied with SuspendedError.
//  5. Concurrent-session cap (M7 hardening): active BUCKET#sess#<sid>
//     slot items (each expiring at mint-time + the 10-minute hard
//     session cap) at/above QUOTA_MAX_CONCURRENT_SESSIONS ->
//     ConcurrentLimitError (HTTP 429 with Retry-After).
//
// Check order for the spend caps follows metering.md (daily before
// monthly; the rate check runs first and independently). A soft warning
// fires at >=80% of either spend cap on successful mints.
const (
	// Single-owner instance: keep abuse protection but don't punish a person
	// tapping the mic a few times (burst 6, refill 1 token / 3s).
	bucketCapacity      = 6.0
	bucketRefillSeconds = 3

	// defaultDailySecondsCap is ~30 minutes of realtime audio per user
	// per UTC day (metering.md default).
	defaultDailySecondsCap = 1800.0

	// defaultMonthlyTokenCap expresses the ~$15/month ceiling as a token
	// count at current gpt-realtime audio pricing (~$32/1M audio input,
	// ~$64/1M audio output; blended ~$40/1M -> ~375k tokens ≈ $15).
	defaultMonthlyTokenCap = 375000.0

	softWarnPercent = 80

	// defaultHourlyBurnTokens is the per-user tokens/hour anomaly
	// threshold (M7 locked decision: default 200k). Burning past this in
	// a single UTC hour is treated as runaway/abusive usage and
	// auto-suspends the account.
	defaultHourlyBurnTokens = 200000.0

	// defaultMaxConcurrentSessions caps simultaneously-active realtime
	// sessions per user (one per surface: web, android, device).
	defaultMaxConcurrentSessions = 3

	// defaultSessionCapSeconds is the FR-V08 10-minute hard session cap;
	// each mint books a concurrency slot and a ledger expiresAt exactly
	// this far out.
	defaultSessionCapSeconds = 600

	// defaultRetentionDays is the LOG# transcript/ledger retention (M7
	// privacy decision: 30 days, overridable via RETENTION_DAYS).
	defaultRetentionDays = 30

	// statusActive / statusSuspended mirror internal/store's
	// UserStatusActive / UserStatusSuspended PROFILE values (single
	// source of truth for the strings lives in internal/store; realtime
	// deliberately does not import store).
	statusActive    = "active"
	statusSuspended = "suspended"

	// sessSlotPrefix is the sort-key prefix of the per-session
	// concurrency slot items: pk=USER#<uid>/sk=BUCKET#sess#<sessionId>,
	// attrs exp (unix expiry = mint + session cap) and ttl (DynamoDB TTL
	// cleanup an hour later; expiry is always enforced from exp in code,
	// never from lazy TTL deletion).
	sessSlotPrefix = "BUCKET#sess#"

	// quotaMetricsNamespace holds the gate's own EMF metrics
	// (UserAutoSuspended, dimension Reason).
	quotaMetricsNamespace = "LiveNinja/Quota"
)

// Env overrides so the caps can be tuned without a code change (plain
// numbers; unset/invalid falls back to the defaults above).
const (
	envDailySecondsCap  = "QUOTA_DAILY_SECONDS"
	envMonthlyTokenCap  = "QUOTA_MONTH_TOKENS"
	envHourlyBurnTokens = "QUOTA_HOURLY_BURN_TOKENS"
	envMaxConcurrent    = "QUOTA_MAX_CONCURRENT_SESSIONS"
	envSessionCap       = "QUOTA_SESSION_CAP_SECONDS"
	envRetentionDays    = "RETENTION_DAYS"
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

// SuspendedError is returned when the account is (or has just been)
// suspended — either the PROFILE already carries status=suspended, or the
// hourly-burn anomaly check tripped on this very request. The broker maps
// it to HTTP 403 account_suspended. Reason is "hourly_burn" for anomaly
// trips, or the stored suspendReason for already-suspended accounts.
type SuspendedError struct {
	Reason string
}

func (e *SuspendedError) Error() string {
	return fmt.Sprintf("account suspended (reason: %s)", e.Reason)
}

// ConcurrentLimitError is returned when the user already has the maximum
// number of unexpired realtime sessions. Maps to HTTP 429 with
// Retry-After set to when the earliest active session slot expires.
type ConcurrentLimitError struct {
	Limit             int
	RetryAfterSeconds int
}

func (e *ConcurrentLimitError) Error() string {
	return fmt.Sprintf("concurrent session limit %d reached: retry after %ds",
		e.Limit, e.RetryAfterSeconds)
}

// SessionUnknownError is returned by CheckSession when the session being
// redeemed has no live concurrency slot: either it was never minted
// (RecordMint never ran for this sessionID) or its slot has passed the
// hard session cap. The bridge maps it to HTTP 401.
type SessionUnknownError struct {
	SessionID string
}

func (e *SessionUnknownError) Error() string {
	return fmt.Sprintf("realtime: session %q unknown or expired", e.SessionID)
}

// SuspendAlert carries the details of an auto-suspension to the
// broker-provided alert hook (SetAlerter) — the hook owns delivery (SES
// via the email queue) and its own error logging.
type SuspendAlert struct {
	UserID     string
	Reason     string
	BurnTokens float64
	Threshold  float64
	At         time.Time
}

// AlertFunc is the auto-suspension notification hook. Implementations
// must be best-effort and non-blocking-ish (a slow alert delays one
// already-rejected mint, nothing else) and must handle their own errors.
type AlertFunc func(ctx context.Context, alert SuspendAlert)

// gateDDB is the subset of the DynamoDB client the gate needs, injectable
// for tests.
type gateDDB interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// Gate implements the metering/quota gate against the live-ninja table.
type Gate struct {
	ddb   gateDDB
	table string
	now   func() time.Time
	alert AlertFunc // optional; nil disables suspension notifications

	dailySecondsCap   float64
	monthlyTokenCap   float64
	hourlyBurnTokens  float64
	maxConcurrent     int
	sessionCapSeconds int
	retentionDays     int
}

// NewGate builds a Gate over the given DynamoDB client and table,
// applying the QUOTA_* / RETENTION_DAYS env overrides.
func NewGate(ddb gateDDB, table string) *Gate {
	return &Gate{
		ddb:               ddb,
		table:             table,
		now:               time.Now,
		dailySecondsCap:   envFloat(envDailySecondsCap, defaultDailySecondsCap),
		monthlyTokenCap:   envFloat(envMonthlyTokenCap, defaultMonthlyTokenCap),
		hourlyBurnTokens:  envFloat(envHourlyBurnTokens, defaultHourlyBurnTokens),
		maxConcurrent:     envInt(envMaxConcurrent, defaultMaxConcurrentSessions),
		sessionCapSeconds: envInt(envSessionCap, defaultSessionCapSeconds),
		retentionDays:     envInt(envRetentionDays, defaultRetentionDays),
	}
}

// SetAlerter installs the auto-suspension notification hook (nil-safe:
// leaving it unset simply skips notification; suspension itself and the
// EMF metric never depend on it).
func (g *Gate) SetAlerter(fn AlertFunc) { g.alert = fn }

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

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// CheckMint runs the full pre-spend gate for a session mint. On success
// it returns the soft-cap warnings ("daily_minutes=83%"-style pairs, per
// metering.md's X-LN-Quota-Warning format), possibly empty. On rejection
// the returned error is a *SuspendedError, *RateLimitedError,
// *QuotaExceededError, or *ConcurrentLimitError.
func (g *Gate) CheckMint(ctx context.Context, userID string) ([]string, error) {
	// Suspension first: a suspended account is denied before it can even
	// spend a rate-limiter token.
	if err := g.checkSuspended(ctx, userID); err != nil {
		return nil, err
	}

	if err := g.takeToken(ctx, userID); err != nil {
		return nil, err
	}

	now := g.now().UTC()

	dayUsage, err := g.readUsageNumbers(ctx, userID, "USAGE#"+now.Format("2006-01-02"),
		"daySeconds", "dayTokens")
	if err != nil {
		return nil, err
	}
	daySeconds := dayUsage["daySeconds"]
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

	// Hourly-burn anomaly: runaway token consumption inside the current
	// UTC hour auto-suspends the account (and denies this mint).
	if err := g.checkHourlyBurn(ctx, userID, dayUsage["dayTokens"], now); err != nil {
		return nil, err
	}

	// Concurrent-session cap: count unexpired session slots.
	if err := g.checkConcurrent(ctx, userID, now); err != nil {
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

// CheckSession validates that an already-minted session may be REDEEMED
// (M12 nova-bridge connect): the account must not be suspended (fresh
// read, same kill-switch as CheckMint) and the BUCKET#sess#<sessionID>
// concurrency slot RecordMint wrote at mint time must still exist and be
// unexpired (the FR-V08 hard session cap bounds token replay).
//
// It deliberately does NOT re-run the pre-spend mint gate: the broker ran
// CheckMint and RecordMint before issuing the session token, so the spend
// caps and rate bucket were already enforced for this session — and
// re-running checkConcurrent at redemption counts the session's OWN slot,
// which self-rejected every legitimate bridge connect with "concurrent
// session limit" (prod, 2026-07-18).
//
// On rejection the error is a *SuspendedError or *SessionUnknownError;
// anything else is an infrastructure failure.
func (g *Gate) CheckSession(ctx context.Context, userID, sessionID string) error {
	if sessionID == "" {
		return &SessionUnknownError{SessionID: sessionID}
	}
	if err := g.checkSuspended(ctx, userID); err != nil {
		return err
	}
	out, err := g.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(g.table),
		ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: sessSlotPrefix + sessionID},
		},
	})
	if err != nil {
		return fmt.Errorf("realtime: read session slot: %w", err)
	}
	if out.Item == nil {
		return &SessionUnknownError{SessionID: sessionID}
	}
	n, ok := out.Item["exp"].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return &SessionUnknownError{SessionID: sessionID}
	}
	exp, perr := strconv.ParseInt(n.Value, 10, 64)
	if perr != nil || exp <= g.now().UTC().Unix() {
		return &SessionUnknownError{SessionID: sessionID}
	}
	return nil
}

// CheckFallback gates the text/STT/TTS fallback modes: suspension gate,
// token bucket (the same abuse guard — fallback turns are still
// per-interaction requests), plus the monthly spend ceiling. The
// daily-minutes cap, hourly-burn trip, and concurrency cap are specific
// to realtime audio sessions and do not block the degraded fallback path
// (an already-suspended user is still rejected by the status check).
func (g *Gate) CheckFallback(ctx context.Context, userID string) error {
	if err := g.checkSuspended(ctx, userID); err != nil {
		return err
	}
	if err := g.takeToken(ctx, userID); err != nil {
		return err
	}
	_, err := g.checkMonthly(ctx, userID, g.now().UTC())
	return err
}

// checkSuspended reads USER#<uid>/PROFILE fresh (no cache — mints are
// already rate-limited to 1/5s per user) and rejects status=suspended. A
// missing profile or any other status passes: "disabled" users never get
// past the authorizer, and the gate's job here is only the suspension
// kill-switch.
func (g *Gate) checkSuspended(ctx context.Context, userID string) error {
	out, err := g.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(g.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: "PROFILE"},
		},
	})
	if err != nil {
		return fmt.Errorf("realtime: read profile: %w", err)
	}
	if out.Item == nil {
		return nil
	}
	if s, ok := out.Item["status"].(*ddbtypes.AttributeValueMemberS); ok && s.Value == statusSuspended {
		reason := "suspended"
		if r, ok := out.Item["suspendReason"].(*ddbtypes.AttributeValueMemberS); ok && r.Value != "" {
			reason = r.Value
		}
		return &SuspendedError{Reason: reason}
	}
	return nil
}

// checkHourlyBurn compares the user's dayTokens against the BUCKET#burn
// hourly snapshot ({hourKey, startTokens}). On a new hour — or whenever
// dayTokens regressed below the anchor (UTC-day rollover mid-hour, or a
// usage-rollup recompute downward) — the snapshot is re-anchored at the
// current reading and the burn restarts from zero. A same-hour delta
// strictly above the threshold suspends the account.
func (g *Gate) checkHourlyBurn(ctx context.Context, userID string, dayTokens float64, now time.Time) error {
	pk := "USER#" + userID
	const sk = "BUCKET#burn"
	hourKey := now.Format("2006-01-02T15")

	out, err := g.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(g.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: pk},
			"sk": &ddbtypes.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return fmt.Errorf("realtime: read burn snapshot: %w", err)
	}

	storedHour, startTokens := "", 0.0
	if out.Item != nil {
		if h, ok := out.Item["hourKey"].(*ddbtypes.AttributeValueMemberS); ok {
			storedHour = h.Value
		}
		if n, ok := out.Item["startTokens"].(*ddbtypes.AttributeValueMemberN); ok {
			if f, perr := strconv.ParseFloat(n.Value, 64); perr == nil {
				startTokens = f
			}
		}
	}

	if out.Item == nil || storedHour != hourKey || dayTokens < startTokens {
		// (Re-)anchor the snapshot; burn measurement restarts here. An
		// unconditional Put is fine — a concurrent racer writes an
		// equivalent anchor for the same hour.
		if _, err := g.ddb.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(g.table),
			Item: map[string]ddbtypes.AttributeValue{
				"pk":          &ddbtypes.AttributeValueMemberS{Value: pk},
				"sk":          &ddbtypes.AttributeValueMemberS{Value: sk},
				"hourKey":     &ddbtypes.AttributeValueMemberS{Value: hourKey},
				"startTokens": numberAV(dayTokens),
			},
		}); err != nil {
			return fmt.Errorf("realtime: write burn snapshot: %w", err)
		}
		return nil
	}

	if burn := dayTokens - startTokens; burn > g.hourlyBurnTokens {
		return g.suspend(ctx, userID, "hourly_burn", burn)
	}
	return nil
}

// suspend flips USER#<uid>/PROFILE from active to suspended (recording
// suspendReason/suspendedAt and bumping tokensValidAfter so every
// outstanding access JWT dies with it — the same kill-switch as "log out
// everywhere"). The conditional status=active check makes the transition,
// the EMF metric, and the owner alert fire exactly once; a lost race (or
// an already-suspended row) is silent. Always returns *SuspendedError —
// the triggering request is denied no matter how the write went (fail
// closed; a failed write is retried by the next mint attempt, since the
// burn condition persists). Mirrors store.SuspendUser — keep in sync.
func (g *Gate) suspend(ctx context.Context, userID, reason string, burn float64) error {
	now := g.now().UTC()
	_, err := g.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(g.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: "PROFILE"},
		},
		UpdateExpression:    aws.String("SET #st = :susp, suspendReason = :r, suspendedAt = :ts, tokensValidAfter = :now"),
		ConditionExpression: aws.String("attribute_exists(pk) AND #st = :active"),
		ExpressionAttributeNames: map[string]string{
			"#st": "status",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":susp":   &ddbtypes.AttributeValueMemberS{Value: statusSuspended},
			":active": &ddbtypes.AttributeValueMemberS{Value: statusActive},
			":r":      &ddbtypes.AttributeValueMemberS{Value: reason},
			":ts":     &ddbtypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":now":    numberAV(float64(now.Unix())),
		},
	})
	switch {
	case err == nil:
		observ.EmitMetric(quotaMetricsNamespace, "UserAutoSuspended", 1, "Count",
			map[string]string{"Reason": reason})
		if g.alert != nil {
			g.alert(ctx, SuspendAlert{
				UserID:     userID,
				Reason:     reason,
				BurnTokens: burn,
				Threshold:  g.hourlyBurnTokens,
				At:         now,
			})
		}
	case isConditionalFailure(err):
		// Already suspended (or profile absent/not active): another
		// request won the transition — no duplicate metric/alert.
	default:
		// The suspension write itself failed; still deny this mint. The
		// anomaly persists in the counters, so the next mint retries.
	}
	return &SuspendedError{Reason: reason}
}

// checkConcurrent counts the user's unexpired BUCKET#sess#<sid> slots
// (single-partition Query, bounded by the cap plus a few not-yet-TTLed
// stragglers — never a Scan). At/above the cap the mint is rejected with
// Retry-After pointed at the earliest slot expiry.
func (g *Gate) checkConcurrent(ctx context.Context, userID string, now time.Time) error {
	out, err := g.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(g.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":pk":  &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			":pfx": &ddbtypes.AttributeValueMemberS{Value: sessSlotPrefix},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("realtime: query session slots: %w", err)
	}

	nowUnix := now.Unix()
	active := 0
	var earliest int64
	for _, item := range out.Items {
		n, ok := item["exp"].(*ddbtypes.AttributeValueMemberN)
		if !ok {
			continue
		}
		exp, perr := strconv.ParseInt(n.Value, 10, 64)
		if perr != nil || exp <= nowUnix {
			continue // expired slot awaiting lazy TTL cleanup
		}
		active++
		if earliest == 0 || exp < earliest {
			earliest = exp
		}
	}

	if active >= g.maxConcurrent {
		retry := int(earliest - nowUnix)
		if retry < 1 {
			retry = 1
		}
		return &ConcurrentLimitError{Limit: g.maxConcurrent, RetryAfterSeconds: retry}
	}
	return nil
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
	m, err := g.readUsageNumbers(ctx, userID, sk, attr)
	if err != nil {
		return 0, err
	}
	return m[attr], nil
}

// readUsageNumbers reads several numeric attributes off one USAGE item in
// a single GetItem, mapping absent items/attributes to 0.
func (g *Gate) readUsageNumbers(ctx context.Context, userID, sk string, attrs ...string) (map[string]float64, error) {
	result := make(map[string]float64, len(attrs))
	for _, a := range attrs {
		result[a] = 0
	}

	out, err := g.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(g.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: read %s: %w", sk, err)
	}
	if out.Item == nil {
		return result, nil
	}
	for _, attr := range attrs {
		n, ok := out.Item[attr].(*ddbtypes.AttributeValueMemberN)
		if !ok {
			continue
		}
		f, perr := strconv.ParseFloat(n.Value, 64)
		if perr != nil {
			return nil, fmt.Errorf("realtime: parse %s.%s: %w", sk, attr, perr)
		}
		result[attr] = f
	}
	return result, nil
}

// RecordMint performs the post-mint bookkeeping (shared spec: "writes
// session ledger LOG# seq 0 marker + bumps dayMints", extended by M7's
// session-cap/concurrency bookkeeping): an atomic ADD of dayMints on
// today's USAGE item (created on first use), a session ledger marker at
// LOG#<sessionId>#000000 (TTL = RETENTION_DAYS, default 30d, matching the
// transcript retention policy), and a BUCKET#sess#<sessionId> concurrency
// slot expiring at the 10-minute hard session cap. The marker lets the
// transcript sink and usage-rollup anchor a session's turns even if the
// client never posts a transcript; its expiresAt records the hard cut-off
// (FR-V08) so downstream metering can clamp session seconds to it.
func (g *Gate) RecordMint(ctx context.Context, userID, sessionID, surface string) error {
	now := g.now().UTC()
	sessionEnd := now.Add(time.Duration(g.sessionCapSeconds) * time.Second)

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
			"pk":        &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk":        &ddbtypes.AttributeValueMemberS{Value: fmt.Sprintf("LOG#%s#%06d", sessionID, 0)},
			"role":      &ddbtypes.AttributeValueMemberS{Value: "system"},
			"text":      &ddbtypes.AttributeValueMemberS{Value: "session-start"},
			"surface":   &ddbtypes.AttributeValueMemberS{Value: surface},
			"engine":    &ddbtypes.AttributeValueMemberS{Value: "openai-realtime"},
			"ts":        &ddbtypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			"expiresAt": &ddbtypes.AttributeValueMemberS{Value: sessionEnd.Format(time.RFC3339)},
			"ttl":       numberAV(float64(now.Add(time.Duration(g.retentionDays) * 24 * time.Hour).Unix())),
		},
	}); err != nil {
		return fmt.Errorf("realtime: write session ledger marker: %w", err)
	}

	// Concurrency slot: expires exactly at the hard session cap; the ttl
	// attribute (an hour later) only handles physical cleanup — expiry is
	// always enforced from exp in checkConcurrent, never from lazy TTL.
	if _, err := g.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(g.table),
		Item: map[string]ddbtypes.AttributeValue{
			"pk":  &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk":  &ddbtypes.AttributeValueMemberS{Value: sessSlotPrefix + sessionID},
			"exp": numberAV(float64(sessionEnd.Unix())),
			"ttl": numberAV(float64(sessionEnd.Add(time.Hour).Unix())),
		},
	}); err != nil {
		return fmt.Errorf("realtime: write session slot: %w", err)
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
