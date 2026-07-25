package store

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedNow is the base instant every claim test advances from, so the tests
// assert on window arithmetic rather than on wall-clock luck.
var fixedNow = time.Date(2026, 7, 25, 14, 3, 11, 482913000, time.UTC)

const testFamily = "RCA#get_weather#invalid_args"

func TestClaimRCACooldownFirstWins(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	ok, err := st.ClaimRCACooldown(ctx, testFamily, "sig1", fixedNow, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "the first claim must win")

	// A second attempt one minute later is the duplicate the cooldown exists
	// to absorb.
	ok, err = st.ClaimRCACooldown(ctx, testFamily, "sig1", fixedNow.Add(time.Minute), time.Hour)
	require.NoError(t, err)
	assert.False(t, ok)

	// A DIFFERENT signature in the same family is a different failure.
	ok, err = st.ClaimRCACooldown(ctx, testFamily, "sig2", fixedNow.Add(time.Minute), time.Hour)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestClaimRCACooldownReopensAfterExpiry(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	ok, err := st.ClaimRCACooldown(ctx, testFamily, "sig1", fixedNow, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	first := numAttr(t, fake.RawItem(testFamily, "COOLDOWN#sig1"), "expiresAt")

	later := fixedNow.Add(61 * time.Minute)
	ok, err = st.ClaimRCACooldown(ctx, testFamily, "sig1", later, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "the window reopens once expiresAt is in the past")

	second := numAttr(t, fake.RawItem(testFamily, "COOLDOWN#sig1"), "expiresAt")
	assert.Greater(t, second, first, "the re-claim must move expiresAt forward")
}

// TestClaimRCACooldownIgnoresStaleTTL pins the guarantee the whole design rests
// on: DynamoDB TTL sweeps can lag up to 48h, so the decision must come from the
// marker's own expiresAt, never from whether the item was deleted yet.
func TestClaimRCACooldownIgnoresStaleTTL(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	past := fixedNow.Add(-30 * 24 * time.Hour)
	fake.SeedItem(map[string]types.AttributeValue{
		"pk":        &types.AttributeValueMemberS{Value: testFamily},
		"sk":        &types.AttributeValueMemberS{Value: "COOLDOWN#sig1"},
		"signature": &types.AttributeValueMemberS{Value: "sig1"},
		"expiresAt": &types.AttributeValueMemberN{Value: strconv.FormatInt(past.Unix(), 10)},
		"ttl":       &types.AttributeValueMemberN{Value: strconv.FormatInt(past.Unix(), 10)},
	})

	ok, err := st.ClaimRCACooldown(ctx, testFamily, "sig1", fixedNow, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "an expired-but-unswept marker must lose the condition")
}

func TestClaimRCADailyBudgetCap(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	day := "2026-07-25"
	for i := 1; i <= 10; i++ {
		count, ok, err := st.ClaimRCADailyBudget(ctx, day, 10, fixedNow)
		require.NoError(t, err)
		require.True(t, ok, "claim %d of 10 must be granted", i)
		assert.Equal(t, i, count, "post-increment count")
	}

	_, ok, err := st.ClaimRCADailyBudget(ctx, day, 10, fixedNow)
	require.NoError(t, err)
	assert.False(t, ok, "the 11th claim of the day must be refused")

	// The counter is per UTC day, so tomorrow starts fresh.
	count, ok, err := st.ClaimRCADailyBudget(ctx, "2026-07-26", 10, fixedNow.Add(24*time.Hour))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 1, count)
}

func TestClaimRCANoticeOncePerWindow(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	ok, err := st.ClaimRCANotice(ctx, "model_unavailable", fixedNow, 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = st.ClaimRCANotice(ctx, "model_unavailable", fixedNow.Add(12*time.Hour), 24*time.Hour)
	require.NoError(t, err)
	assert.False(t, ok, "a second notice inside the window is suppressed")

	ok, err = st.ClaimRCANotice(ctx, "model_unavailable", fixedNow.Add(25*time.Hour), 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "the window reopens after 24h")

	// Kinds are independent: a malformed-reply notice is not gated by the
	// model-unavailable one.
	ok, err = st.ClaimRCANotice(ctx, "malformed_response", fixedNow.Add(25*time.Hour), 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestRecentRCAsNewestFirstExcludesMarkers(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// A cooldown marker shares the partition with the records; begins_with(sk,
	// "AT#") is what keeps it out of the results.
	ok, err := st.ClaimRCACooldown(ctx, testFamily, "sigX", fixedNow, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)

	for i := 0; i < 7; i++ {
		occurred := fixedNow.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		require.NoError(t, st.PutRCA(ctx, &RCARecord{
			PK:         testFamily,
			RCAID:      fmt.Sprintf("rca%09d", i),
			Status:     RCAStatusAnalyzed,
			Tool:       "get_weather",
			ErrorCode:  "invalid_args",
			OccurredAt: occurred,
			CreatedAt:  occurred,
			Symptom:    fmt.Sprintf("symptom %d", i),
		}))
	}

	recs, err := st.RecentRCAs(ctx, testFamily, 5)
	require.NoError(t, err)
	require.Len(t, recs, 5)
	assert.Equal(t, "symptom 6", recs[0].Symptom, "newest first")
	assert.Equal(t, "symptom 2", recs[4].Symptom)
	for _, rec := range recs {
		assert.NotEmpty(t, rec.RCAID, "a COOLDOWN# marker leaked into the results")
	}
}

func TestPutRCASetsTTLAndKeys(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	created := fixedNow.Format(time.RFC3339Nano)
	rec := &RCARecord{
		PK:         testFamily,
		RCAID:      "ab12cd34ef56",
		Status:     RCAStatusAnalyzed,
		OccurredAt: created,
		CreatedAt:  created,
	}
	require.NoError(t, st.PutRCA(ctx, rec))

	assert.Equal(t, "AT#"+created+"#ab12cd34ef56", rec.SK)
	assert.Equal(t, fixedNow.Add(30*24*time.Hour).Unix(), rec.TTL)
	require.NotNil(t, fake.RawItem(testFamily, rec.SK))

	// A '#' in the id would corrupt the sort key's structure.
	require.Error(t, st.PutRCA(ctx, &RCARecord{PK: testFamily, RCAID: "bad#id", OccurredAt: created}))
}

func TestIncrementRCASuppressed(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	created := fixedNow.Format(time.RFC3339Nano)
	rec := &RCARecord{PK: testFamily, RCAID: "aa11bb22cc33", OccurredAt: created, CreatedAt: created}
	require.NoError(t, st.PutRCA(ctx, rec))

	require.NoError(t, st.IncrementRCASuppressed(ctx, testFamily, rec.SK))
	assert.Equal(t, float64(1), numAttr(t, fake.RawItem(testFamily, rec.SK), "suppressedCount"))
	require.NoError(t, st.IncrementRCASuppressed(ctx, testFamily, rec.SK))
	assert.Equal(t, float64(2), numAttr(t, fake.RawItem(testFamily, rec.SK), "suppressedCount"))

	// A missing item surfaces ErrNotFound rather than resurrecting a ghost row;
	// the analyzer swallows it.
	err := st.IncrementRCASuppressed(ctx, testFamily, "AT#nope#nope")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Nil(t, fake.RawItem(testFamily, "AT#nope#nope"))
}

func TestPutProfileSuggestionIsCreateOnly(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	// A settings document in the same partition: the create-only condition is
	// what guarantees the RCA role's unavoidably-broad USER#* PutItem grant can
	// never overwrite it.
	fake.SeedItem(map[string]types.AttributeValue{
		"pk":      &types.AttributeValueMemberS{Value: "USER#u1"},
		"sk":      &types.AttributeValueMemberS{Value: "SETTINGS"},
		"version": &types.AttributeValueMemberN{Value: "7"},
	})

	sg := &ProfileSuggestion{
		SuggID:        "ab12cd34ef56",
		Field:         "profile.units",
		CurrentValue:  "imperial",
		ProposedValue: "metric",
		Reason:        "the user asked for celsius twice",
		Source:        "rca",
		SourceRef:     "rca123456789",
		CreatedAt:     fixedNow.Format(time.RFC3339Nano),
	}
	require.NoError(t, st.PutProfileSuggestion(ctx, "u1", sg))
	assert.Equal(t, "USER#u1", sg.PK)
	assert.Equal(t, "PROFSUGG#"+sg.CreatedAt+"#ab12cd34ef56", sg.SK)
	assert.Equal(t, SuggestionStatusPending, sg.Status)
	assert.Equal(t, fixedNow.Add(30*24*time.Hour).Unix(), sg.TTL)

	// Re-writing the same key loses the conditional put.
	dup := *sg
	require.ErrorIs(t, st.PutProfileSuggestion(ctx, "u1", &dup), ErrAlreadyExists)

	// The settings item is untouched.
	assert.Equal(t, float64(7), numAttr(t, fake.RawItem("USER#u1", "SETTINGS"), "version"))

	// And the M16 read path sees it.
	list, err := st.ListProfileSuggestions(ctx, "u1", 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "profile.units", list[0].Field)
	assert.Equal(t, "rca", list[0].Source)
}

// numAttr reads a numeric attribute off a raw fake item.
func numAttr(t *testing.T, item map[string]types.AttributeValue, attr string) float64 {
	t.Helper()
	require.NotNil(t, item, "item is absent")
	n, ok := item[attr].(*types.AttributeValueMemberN)
	require.True(t, ok, "attribute %q is not numeric: %#v", attr, item[attr])
	f, err := strconv.ParseFloat(n.Value, 64)
	require.NoError(t, err)
	return f
}
