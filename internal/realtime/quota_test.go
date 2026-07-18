package realtime

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// fakeClock is a settable clock injected into Gate.now.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestGate() (*Gate, *testutil.FakeDynamo, *fakeClock) {
	fake := testutil.NewFakeDynamo()
	g := NewGate(fake, "live-ninja-test")
	clock := &fakeClock{t: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)}
	g.now = clock.now
	return g, fake, clock
}

func seedUsage(fake *testutil.FakeDynamo, userID, sk, attr string, value float64) {
	fake.SeedItem(map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "USER#" + userID},
		"sk": &types.AttributeValueMemberS{Value: sk},
		attr: &types.AttributeValueMemberN{Value: strconv.FormatFloat(value, 'f', -1, 64)},
	})
}

func bucketState(t *testing.T, fake *testutil.FakeDynamo, userID string) (tokens float64, lastRefill int64) {
	t.Helper()
	item := fake.RawItem("USER#"+userID, "BUCKET#mint")
	require.NotNil(t, item, "bucket item must exist")
	tokAV, ok := item["tokens"].(*types.AttributeValueMemberN)
	require.True(t, ok)
	tokens, err := strconv.ParseFloat(tokAV.Value, 64)
	require.NoError(t, err)
	lrAV, ok := item["lastRefill"].(*types.AttributeValueMemberN)
	require.True(t, ok)
	lrF, err := strconv.ParseFloat(lrAV.Value, 64)
	require.NoError(t, err)
	return tokens, int64(lrF)
}

func TestCheckMintHappyPathNoWarnings(t *testing.T) {
	g, _, _ := newTestGate()
	warnings, err := g.CheckMint(context.Background(), "u1")
	require.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestDailyHardCapRejectsPreSpend(t *testing.T) {
	g, fake, clock := newTestGate()
	day := clock.t.UTC().Format("2006-01-02")
	seedUsage(fake, "u1", "USAGE#"+day, "daySeconds", 1800) // exactly at cap

	_, err := g.CheckMint(context.Background(), "u1")
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	assert.Equal(t, "daily_minutes", qe.Kind)
	assert.Equal(t, 30.0, qe.Used)  // minutes
	assert.Equal(t, 30.0, qe.Limit) // minutes
	assert.Equal(t, time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC), qe.ResetAt)
}

func TestMonthlyHardCapRejectsPreSpend(t *testing.T) {
	g, fake, clock := newTestGate()
	month := clock.t.UTC().Format("2006-01")
	seedUsage(fake, "u1", "USAGE#"+month, "monthTokens", 375000) // at cap

	_, err := g.CheckMint(context.Background(), "u1")
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	assert.Equal(t, "monthly_tokens", qe.Kind)
	assert.Equal(t, 375000.0, qe.Used)
	assert.Equal(t, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), qe.ResetAt)
}

func TestSoftCapWarningsAt80Percent(t *testing.T) {
	g, fake, clock := newTestGate()
	day := clock.t.UTC().Format("2006-01-02")
	month := clock.t.UTC().Format("2006-01")
	seedUsage(fake, "u1", "USAGE#"+day, "daySeconds", 1500)      // 83%
	seedUsage(fake, "u1", "USAGE#"+month, "monthTokens", 300000) // 80%

	warnings, err := g.CheckMint(context.Background(), "u1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"daily_minutes=83%", "monthly_tokens=80%"}, warnings)
}

func TestTokenBucketBurstThenRateLimit(t *testing.T) {
	g, _, _ := newTestGate()
	ctx := context.Background()

	// Burst of 3 mints succeeds (capacity 3).
	for i := 0; i < 3; i++ {
		_, err := g.CheckMint(ctx, "u1")
		require.NoError(t, err, "mint %d within burst must pass", i+1)
	}

	// 4th immediate mint is rate limited — rejected before any spend.
	_, err := g.CheckMint(ctx, "u1")
	var rl *RateLimitedError
	require.ErrorAs(t, err, &rl)
	assert.Equal(t, bucketRefillSeconds, rl.RetryAfterSeconds)
}

func TestTokenBucketRefillAfterWait(t *testing.T) {
	g, _, clock := newTestGate()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := g.CheckMint(ctx, "u1")
		require.NoError(t, err)
	}
	_, err := g.CheckMint(ctx, "u1")
	require.Error(t, err)

	// 4s later: still no whole refill step.
	clock.advance(4 * time.Second)
	_, err = g.CheckMint(ctx, "u1")
	var rl *RateLimitedError
	require.ErrorAs(t, err, &rl)

	// 1 more second completes the 5s step -> exactly one token.
	clock.advance(1 * time.Second)
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err)

	// And it is spent again immediately.
	_, err = g.CheckMint(ctx, "u1")
	require.ErrorAs(t, err, &rl)
}

func TestTokenBucketRefillMath(t *testing.T) {
	g, fake, clock := newTestGate()
	ctx := context.Background()

	// First mint creates the bucket with capacity-1 tokens.
	_, err := g.CheckMint(ctx, "u1")
	require.NoError(t, err)
	tokens, lastRefill := bucketState(t, fake, "u1")
	assert.Equal(t, bucketCapacity-1, tokens)
	assert.Equal(t, clock.t.Unix(), lastRefill)

	// Drain the remaining two tokens.
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err)
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err)
	tokens, _ = bucketState(t, fake, "u1")
	assert.Equal(t, 0.0, tokens)

	// Advance 12s = 2 whole steps (10s credited) + 2s fractional remainder.
	// A mint consumes one of the two refilled tokens; lastRefill advances
	// by exactly the credited 10s so the 2s remainder is preserved.
	start := clock.t.Unix()
	clock.advance(12 * time.Second)
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err)
	tokens, lastRefill = bucketState(t, fake, "u1")
	assert.Equal(t, 1.0, tokens) // 0 + 2 refilled - 1 spent
	assert.Equal(t, start+10, lastRefill, "unconsumed fractional refill time must be preserved")

	// Long idle caps at capacity: advance 10 minutes, spend one, expect
	// capacity-1 remaining and lastRefill snapped to now.
	clock.advance(10 * time.Minute)
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err)
	tokens, lastRefill = bucketState(t, fake, "u1")
	assert.Equal(t, bucketCapacity-1, tokens)
	assert.Equal(t, clock.t.Unix(), lastRefill)
}

func TestTokenBucketIsPerUser(t *testing.T) {
	g, _, _ := newTestGate()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := g.CheckMint(ctx, "u1")
		require.NoError(t, err)
	}
	_, err := g.CheckMint(ctx, "u1")
	require.Error(t, err)

	// A different user has an untouched bucket.
	_, err = g.CheckMint(ctx, "u2")
	require.NoError(t, err)
}

func TestCheckOrderBucketBeforeCaps(t *testing.T) {
	// When both the bucket is empty AND a cap is exceeded, the rate limit
	// (429) fires first per metering.md check order.
	g, fake, clock := newTestGate()
	ctx := context.Background()
	day := clock.t.UTC().Format("2006-01-02")
	seedUsage(fake, "u1", "USAGE#"+day, "daySeconds", 99999)

	// Exhaust the bucket: each attempt spends a token then hits the cap.
	var lastErr error
	for i := 0; i < 3; i++ {
		_, lastErr = g.CheckMint(ctx, "u1")
		var qe *QuotaExceededError
		require.ErrorAs(t, lastErr, &qe)
	}
	_, err := g.CheckMint(ctx, "u1")
	var rl *RateLimitedError
	require.ErrorAs(t, err, &rl, "bucket exhaustion must win over the cap check")
}

func TestCheckFallbackSkipsDailyCap(t *testing.T) {
	g, fake, clock := newTestGate()
	ctx := context.Background()
	day := clock.t.UTC().Format("2006-01-02")
	seedUsage(fake, "u1", "USAGE#"+day, "daySeconds", 99999) // way over daily

	// Fallback is allowed despite the daily-minutes cap...
	require.NoError(t, g.CheckFallback(ctx, "u1"))

	// ...but still blocked by the monthly spend ceiling.
	month := clock.t.UTC().Format("2006-01")
	seedUsage(fake, "u2", "USAGE#"+month, "monthTokens", 999999)
	err := g.CheckFallback(ctx, "u2")
	var qe *QuotaExceededError
	require.ErrorAs(t, err, &qe)
	assert.Equal(t, "monthly_tokens", qe.Kind)
}

// seedProfile seeds a USER#<uid>/PROFILE row with the given status.
func seedProfile(fake *testutil.FakeDynamo, userID, status string) {
	fake.SeedItem(map[string]types.AttributeValue{
		"pk":     &types.AttributeValueMemberS{Value: "USER#" + userID},
		"sk":     &types.AttributeValueMemberS{Value: "PROFILE"},
		"status": &types.AttributeValueMemberS{Value: status},
		"userId": &types.AttributeValueMemberS{Value: userID},
	})
}

// captureAlerts installs a recording alerter and returns the sink.
func captureAlerts(g *Gate) *[]SuspendAlert {
	var alerts []SuspendAlert
	g.SetAlerter(func(_ context.Context, a SuspendAlert) {
		alerts = append(alerts, a)
	})
	return &alerts
}

func profileAttr(t *testing.T, fake *testutil.FakeDynamo, userID, attr string) types.AttributeValue {
	t.Helper()
	item := fake.RawItem("USER#"+userID, "PROFILE")
	require.NotNil(t, item, "profile item must exist")
	return item[attr]
}

func TestSuspendedUserDeniedBeforeBucket(t *testing.T) {
	g, fake, _ := newTestGate()
	seedProfile(fake, "u1", "suspended")

	_, err := g.CheckMint(context.Background(), "u1")
	var se *SuspendedError
	require.ErrorAs(t, err, &se)

	// Denied pre-bucket: no rate-limiter token was spent (item never created).
	assert.Nil(t, fake.RawItem("USER#u1", "BUCKET#mint"))

	// Fallback modes are equally denied.
	err = g.CheckFallback(context.Background(), "u1")
	require.ErrorAs(t, err, &se)
}

func TestHourlyBurnAnomalySuspendsAndAlertsOnce(t *testing.T) {
	g, fake, clock := newTestGate()
	ctx := context.Background()
	seedProfile(fake, "u1", "active")
	alerts := captureAlerts(g)
	day := clock.t.UTC().Format("2006-01-02")

	// Mint 1 anchors the hourly snapshot at dayTokens=0.
	_, err := g.CheckMint(ctx, "u1")
	require.NoError(t, err)

	// The user then burns 200,001 tokens inside the same UTC hour
	// (default threshold 200,000 — strictly-greater trips).
	seedUsage(fake, "u1", "USAGE#"+day, "dayTokens", 200001)
	clock.advance(5 * time.Second) // refill the bucket token spent above

	_, err = g.CheckMint(ctx, "u1")
	var se *SuspendedError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "hourly_burn", se.Reason)

	// PROFILE flipped to suspended with reason + JWT kill-switch bump.
	statusAV, _ := profileAttr(t, fake, "u1", "status").(*types.AttributeValueMemberS)
	require.NotNil(t, statusAV)
	assert.Equal(t, "suspended", statusAV.Value)
	reasonAV, _ := profileAttr(t, fake, "u1", "suspendReason").(*types.AttributeValueMemberS)
	require.NotNil(t, reasonAV)
	assert.Equal(t, "hourly_burn", reasonAV.Value)
	tvaAV, _ := profileAttr(t, fake, "u1", "tokensValidAfter").(*types.AttributeValueMemberN)
	require.NotNil(t, tvaAV)
	assert.Equal(t, strconv.FormatInt(clock.t.Unix(), 10), tvaAV.Value)

	// Exactly one alert, with the anomaly math attached.
	require.Len(t, *alerts, 1)
	a := (*alerts)[0]
	assert.Equal(t, "u1", a.UserID)
	assert.Equal(t, "hourly_burn", a.Reason)
	assert.Equal(t, 200001.0, a.BurnTokens)
	assert.Equal(t, 200000.0, a.Threshold)

	// Further mints are denied by the status check (pre-bucket) and do
	// NOT fire a second alert.
	tokensBefore, _ := bucketState(t, fake, "u1")
	_, err = g.CheckMint(ctx, "u1")
	require.ErrorAs(t, err, &se)
	tokensAfter, _ := bucketState(t, fake, "u1")
	assert.Equal(t, tokensBefore, tokensAfter, "suspended mint must not spend a bucket token")
	assert.Len(t, *alerts, 1)
}

func TestHourlyBurnAtThresholdIsAllowed(t *testing.T) {
	g, fake, clock := newTestGate()
	ctx := context.Background()
	seedProfile(fake, "u1", "active")
	alerts := captureAlerts(g)
	day := clock.t.UTC().Format("2006-01-02")

	_, err := g.CheckMint(ctx, "u1") // anchor at 0
	require.NoError(t, err)

	seedUsage(fake, "u1", "USAGE#"+day, "dayTokens", 200000) // == threshold
	clock.advance(5 * time.Second)
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err, "burn equal to the threshold must not suspend")
	assert.Empty(t, *alerts)
}

func TestHourlyBurnWindowResetsEachHour(t *testing.T) {
	g, fake, clock := newTestGate()
	ctx := context.Background()
	seedProfile(fake, "u1", "active")
	alerts := captureAlerts(g)
	day := clock.t.UTC().Format("2006-01-02")

	_, err := g.CheckMint(ctx, "u1") // anchor hour 12 at 0
	require.NoError(t, err)

	// 300k tokens land, but the next mint happens in hour 13: the window
	// re-anchors at 300k and the mint is allowed.
	seedUsage(fake, "u1", "USAGE#"+day, "dayTokens", 300000)
	clock.advance(time.Hour)
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err)
	assert.Empty(t, *alerts)

	// Another 250k inside hour 13 -> over threshold -> suspended.
	seedUsage(fake, "u1", "USAGE#"+day, "dayTokens", 550000)
	clock.advance(5 * time.Second)
	_, err = g.CheckMint(ctx, "u1")
	var se *SuspendedError
	require.ErrorAs(t, err, &se)
	assert.Len(t, *alerts, 1)
	assert.Equal(t, 250000.0, (*alerts)[0].BurnTokens)
}

func TestHourlyBurnReanchorsWhenTokensDrop(t *testing.T) {
	g, fake, clock := newTestGate()
	ctx := context.Background()
	seedProfile(fake, "u1", "active")
	day := clock.t.UTC().Format("2006-01-02")

	seedUsage(fake, "u1", "USAGE#"+day, "dayTokens", 100000)
	_, err := g.CheckMint(ctx, "u1") // anchor at 100k
	require.NoError(t, err)

	// dayTokens regresses (rollup recompute / day rollover): re-anchor
	// down instead of computing a bogus negative burn or suspending.
	seedUsage(fake, "u1", "USAGE#"+day, "dayTokens", 50000)
	clock.advance(5 * time.Second)
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err)

	snap := fake.RawItem("USER#u1", "BUCKET#burn")
	require.NotNil(t, snap)
	start, _ := snap["startTokens"].(*types.AttributeValueMemberN)
	require.NotNil(t, start)
	assert.Equal(t, "50000", start.Value, "snapshot must re-anchor at the lower reading")
}

func TestConcurrentSessionLimit(t *testing.T) {
	g, _, clock := newTestGate()
	ctx := context.Background()
	start := clock.t

	// Three sessions minted 5s apart (staying inside the token bucket).
	for i := 0; i < 3; i++ {
		_, err := g.CheckMint(ctx, "u1")
		require.NoError(t, err, "mint %d must pass", i+1)
		require.NoError(t, g.RecordMint(ctx, "u1", fmt.Sprintf("sess-%d", i), "web"))
		clock.advance(5 * time.Second)
	}

	// 4th concurrent session is rejected; Retry-After points at the
	// earliest slot expiry: sess-0 expires at start+600s, now=start+15s.
	_, err := g.CheckMint(ctx, "u1")
	var cle *ConcurrentLimitError
	require.ErrorAs(t, err, &cle)
	assert.Equal(t, 3, cle.Limit)
	assert.Equal(t, 585, cle.RetryAfterSeconds)

	// Once the 10-minute hard cap passes, all slots have expired and a
	// new session is allowed again (expired slot items are ignored even
	// though lazy TTL hasn't physically deleted them).
	clock.t = start.Add(11 * time.Minute)
	_, err = g.CheckMint(ctx, "u1")
	require.NoError(t, err)
}

func TestRecordMintSessionCapBookkeeping(t *testing.T) {
	g, fake, clock := newTestGate()
	require.NoError(t, g.RecordMint(context.Background(), "u1", "sess-1", "web"))

	now := clock.t.UTC()
	end := now.Add(600 * time.Second)

	// Ledger marker carries the FR-V08 hard-cap expiresAt and the
	// RETENTION_DAYS (default 30d) TTL.
	marker := fake.RawItem("USER#u1", "LOG#sess-1#000000")
	require.NotNil(t, marker)
	expAt, _ := marker["expiresAt"].(*types.AttributeValueMemberS)
	require.NotNil(t, expAt)
	assert.Equal(t, end.Format(time.RFC3339), expAt.Value)
	ttlAV, _ := marker["ttl"].(*types.AttributeValueMemberN)
	require.NotNil(t, ttlAV)
	assert.Equal(t, strconv.FormatInt(now.Add(30*24*time.Hour).Unix(), 10), ttlAV.Value)

	// Concurrency slot expires exactly at the session cap.
	slot := fake.RawItem("USER#u1", "BUCKET#sess#sess-1")
	require.NotNil(t, slot)
	expAV, _ := slot["exp"].(*types.AttributeValueMemberN)
	require.NotNil(t, expAV)
	assert.Equal(t, strconv.FormatInt(end.Unix(), 10), expAV.Value)
	slotTTL, _ := slot["ttl"].(*types.AttributeValueMemberN)
	require.NotNil(t, slotTTL)
	assert.Equal(t, strconv.FormatInt(end.Add(time.Hour).Unix(), 10), slotTTL.Value)
}

func TestHardeningEnvOverrides(t *testing.T) {
	t.Setenv("QUOTA_HOURLY_BURN_TOKENS", "1000")
	t.Setenv("QUOTA_MAX_CONCURRENT_SESSIONS", "1")
	t.Setenv("QUOTA_SESSION_CAP_SECONDS", "120")
	t.Setenv("RETENTION_DAYS", "7")

	g := NewGate(testutil.NewFakeDynamo(), "live-ninja-test")
	assert.Equal(t, 1000.0, g.hourlyBurnTokens)
	assert.Equal(t, 1, g.maxConcurrent)
	assert.Equal(t, 120, g.sessionCapSeconds)
	assert.Equal(t, 7, g.retentionDays)

	// Invalid values fall back to defaults.
	t.Setenv("QUOTA_HOURLY_BURN_TOKENS", "-5")
	t.Setenv("QUOTA_MAX_CONCURRENT_SESSIONS", "zero")
	g = NewGate(testutil.NewFakeDynamo(), "live-ninja-test")
	assert.Equal(t, 200000.0, g.hourlyBurnTokens)
	assert.Equal(t, 3, g.maxConcurrent)
}

func TestRecordMintWritesLedger(t *testing.T) {
	g, fake, clock := newTestGate()
	require.NoError(t, g.RecordMint(context.Background(), "u1", "sess-1", "web"))

	day := clock.t.UTC().Format("2006-01-02")
	usage := fake.RawItem("USER#u1", "USAGE#"+day)
	require.NotNil(t, usage)
	mints, ok := usage["dayMints"].(*types.AttributeValueMemberN)
	require.True(t, ok)
	assert.Equal(t, "1", mints.Value)

	marker := fake.RawItem("USER#u1", "LOG#sess-1#000000")
	require.NotNil(t, marker)
	role, _ := marker["role"].(*types.AttributeValueMemberS)
	require.NotNil(t, role)
	assert.Equal(t, "system", role.Value)

	// A second mint the same day increments the counter.
	require.NoError(t, g.RecordMint(context.Background(), "u1", "sess-2", "web"))
	usage = fake.RawItem("USER#u1", "USAGE#"+day)
	mints, _ = usage["dayMints"].(*types.AttributeValueMemberN)
	assert.Equal(t, "2", mints.Value)
}
