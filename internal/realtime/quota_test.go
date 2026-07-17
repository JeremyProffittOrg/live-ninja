package realtime

import (
	"context"
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
