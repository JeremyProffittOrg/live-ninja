package store

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidRuleName(t *testing.T) {
	for _, ok := range []string{"abc", "location-and-time", "a1-b2-c3", strings.Repeat("a", 48)} {
		assert.True(t, ValidRuleName(ok), ok)
	}
	for _, bad := range []string{"", "ab", "Abc", "a_b", "-abc", "abc-", "a--b", "a b",
		"RULE#x", strings.Repeat("a", 49), "règle"} {
		assert.False(t, ValidRuleName(bad), bad)
	}
}

func TestListRulesSeedsDefaultOnce(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	rules, err := st.ListRules(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	seed := DefaultRule()
	assert.Equal(t, SeedRuleName, rules[0].Name)
	assert.Equal(t, seed.Description, rules[0].Description)
	assert.Equal(t, seed.Body, rules[0].Body)
	assert.True(t, rules[0].Enabled)
	assert.Equal(t, RuleSourceSeed, rules[0].Source)
	assert.Equal(t, 1, rules[0].Version)
	assert.NotEmpty(t, rules[0].CreatedAt)
	assert.NotNil(t, fake.RawItem("USER#u1", "RULE#location-and-time"))

	// A second list does not re-seed or duplicate.
	rules, err = st.ListRules(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, rules, 1)

	// Disabling is the persistent opt-out: the seed stays, disabled.
	_, err = st.SetRuleEnabled(ctx, "u1", SeedRuleName, false)
	require.NoError(t, err)
	rules, err = st.ListRules(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.False(t, rules[0].Enabled)
}

func TestSeedRuleBodyCarriesTheLocationContract(t *testing.T) {
	body := DefaultRule().Body
	for _, want := range []string{"CURRENT location", "HOME location", "America/New_York",
		"BASE KNOWLEDGE", "set_current_location", "home=true", "get_weather",
		"Never ask which timezone"} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "\n", "the seed body is one paragraph")
	assert.True(t, ValidRuleName(SeedRuleName))
	r := DefaultRule()
	r.UserID = "u"
	require.NoError(t, validateRule(&r), "the seed must pass the same validation users get")
}

func TestUpsertRuleCreateUpdatePreservesCreatedAt(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	got, err := st.UpsertRule(ctx, Rule{
		UserID:      "u1",
		Name:        "  packing-list ",
		Description: "Load when the user\nasks what to pack\r\n for a trip.",
		Body:        "  Always include a charger.  ",
		Enabled:     true,
		Source:      RuleSourceAssistant,
	})
	require.NoError(t, err)
	assert.Equal(t, "packing-list", got.Name)
	assert.Equal(t, "Load when the user asks what to pack for a trip.", got.Description)
	assert.Equal(t, "Always include a charger.", got.Body)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, got.CreatedAt, got.UpdatedAt)

	// Backdate createdAt so preservation is observable within one second.
	raw := fake.RawItem("USER#u1", "RULE#packing-list")
	raw["createdAt"] = &types.AttributeValueMemberS{Value: "2020-01-01T00:00:00Z"}
	fake.SeedItem(raw)

	got, err = st.UpsertRule(ctx, Rule{
		UserID: "u1", Name: "packing-list",
		Description: "Load when the user asks what to pack.",
		Body:        "Charger and passport.", Enabled: false, Source: RuleSourceUser,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, got.Version)
	assert.Equal(t, "2020-01-01T00:00:00Z", got.CreatedAt)
	assert.Equal(t, "Charger and passport.", got.Body)
	assert.False(t, got.Enabled)
	assert.Equal(t, RuleSourceUser, got.Source)
}

func TestUpsertRuleValidation(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	base := func() Rule {
		return Rule{UserID: "u1", Name: "good-name", Description: "Load when it matters.",
			Body: "Do the thing.", Enabled: true, Source: RuleSourceUser}
	}
	cases := map[string]func(*Rule){
		"no user":        func(r *Rule) { r.UserID = "" },
		"bad name":       func(r *Rule) { r.Name = "Bad Name" },
		"short desc":     func(r *Rule) { r.Description = "too short" },
		"long desc":      func(r *Rule) { r.Description = strings.Repeat("é", 201) },
		"empty body":     func(r *Rule) { r.Body = "   " },
		"long body":      func(r *Rule) { r.Body = strings.Repeat("x", 4001) },
		"unknown source": func(r *Rule) { r.Source = "webpage" },
	}
	for name, mutate := range cases {
		r := base()
		mutate(&r)
		_, err := st.UpsertRule(ctx, r)
		assert.ErrorIs(t, err, ErrInvalidRule, name)
	}

	// Boundaries are inclusive and measured in runes.
	r := base()
	r.Description = strings.Repeat("é", 200)
	r.Body = strings.Repeat("ü", 4000)
	_, err := st.UpsertRule(ctx, r)
	require.NoError(t, err)
}

func TestUpsertRuleLimitBlocksOnlyNewNames(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	for i := 0; i < MaxRules; i++ {
		_, err := st.UpsertRule(ctx, Rule{UserID: "u1", Name: fmt.Sprintf("rule-%02d", i),
			Description: "Load when rule number applies.", Body: "b", Enabled: true, Source: RuleSourceUser})
		require.NoError(t, err)
	}
	_, err := st.UpsertRule(ctx, Rule{UserID: "u1", Name: "one-too-many",
		Description: "Load when rule number applies.", Body: "b", Enabled: true, Source: RuleSourceUser})
	require.ErrorIs(t, err, ErrRuleLimit)

	// Updating an existing rule at the cap still works.
	got, err := st.UpsertRule(ctx, Rule{UserID: "u1", Name: "rule-07",
		Description: "Load when rule seven applies.", Body: "b2", Enabled: true, Source: RuleSourceUser})
	require.NoError(t, err)
	assert.Equal(t, 2, got.Version)

	// The cap is per user.
	_, err = st.UpsertRule(ctx, Rule{UserID: "u2", Name: "one-too-many",
		Description: "Load when rule number applies.", Body: "b", Enabled: true, Source: RuleSourceUser})
	require.NoError(t, err)
}

func TestListRulesSortedAndIsolated(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	for _, n := range []string{"zeta-rule", "alpha-rule", "mid-rule"} {
		_, err := st.UpsertRule(ctx, Rule{UserID: "u1", Name: n, Description: "Load when it matters.",
			Body: "b", Enabled: true, Source: RuleSourceUser})
		require.NoError(t, err)
	}
	_, err := st.UpsertRule(ctx, Rule{UserID: "u2", Name: "other-user", Description: "Load when it matters.",
		Body: "b", Enabled: true, Source: RuleSourceUser})
	require.NoError(t, err)

	rules, err := st.ListRules(ctx, "u1")
	require.NoError(t, err)
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Name)
		assert.Equal(t, "u1", r.UserID)
	}
	// u1 already had rules, so no seed is added.
	assert.Equal(t, []string{"alpha-rule", "mid-rule", "zeta-rule"}, names)
}

func TestGetRuleSetEnabledDelete(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	_, err := st.GetRule(ctx, "u1", "missing-rule")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = st.SetRuleEnabled(ctx, "u1", "missing-rule", true)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, st.DeleteRule(ctx, "u1", "missing-rule"), ErrNotFound)

	_, err = st.UpsertRule(ctx, Rule{UserID: "u1", Name: "my-rule", Description: "Load when it matters.",
		Body: "b", Enabled: true, Source: RuleSourceUser})
	require.NoError(t, err)

	off, err := st.SetRuleEnabled(ctx, "u1", "my-rule", false)
	require.NoError(t, err)
	assert.False(t, off.Enabled)
	assert.Equal(t, 2, off.Version)

	require.NoError(t, st.DeleteRule(ctx, "u1", "my-rule"))
	_, err = st.GetRule(ctx, "u1", "my-rule")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGetRuleSeedsForBrandNewUserOnly(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	// A brand-new user's mint index advertises the seed; rule_load must find it.
	got, err := st.GetRule(ctx, "fresh", SeedRuleName)
	require.NoError(t, err)
	assert.Equal(t, DefaultRule().Body, got.Body)
	assert.NotNil(t, fake.RawItem("USER#fresh", "RULE#"+SeedRuleName))

	// A user who has rules but deleted the seed does not get it back on load.
	_, err = st.UpsertRule(ctx, Rule{UserID: "u1", Name: "my-rule", Description: "Load when it matters.",
		Body: "b", Enabled: true, Source: RuleSourceUser})
	require.NoError(t, err)
	_, err = st.GetRule(ctx, "u1", SeedRuleName)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Nil(t, fake.RawItem("USER#u1", "RULE#"+SeedRuleName))
}

// reviewRaceDDB changes the fake immediately before DynamoDB evaluates all
// transaction conditions, exercising the gap between validation and commit.
type reviewRaceDDB struct {
	*testutil.FakeDynamo
	before func(*dynamodb.TransactWriteItemsInput)
	last   *dynamodb.TransactWriteItemsInput
}

func (f *reviewRaceDDB) TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.last = in
	if f.before != nil {
		f.before(in)
	}
	return f.FakeDynamo.TransactWriteItems(ctx, in, opts...)
}
func reviewStore(t *testing.T) (*Store, *reviewRaceDDB) {
	t.Helper()
	f := &reviewRaceDDB{FakeDynamo: testutil.NewFakeDynamo()}
	st := NewWithClient(f, "table")
	require.NoError(t, st.CreateUser(context.Background(), &User{UserID: "alice", AmazonUserID: "amzn.alice", Email: "alice@example.com", Role: RoleOwner, Status: UserStatusActive}))
	require.NoError(t, st.CreateUser(context.Background(), &User{UserID: "bob", AmazonUserID: "amzn.bob", Email: "bob@example.com", Role: RoleOwner, Status: UserStatusActive}))
	return st, f
}
func newRuleReview() RuleReview {
	return RuleReview{RequestID: uuid.NewString(), ReviewedAt: time.Now().UTC().Format(time.RFC3339Nano), Operation: "save", Proposed: Rule{Name: "trip-check", Description: "Load for trip planning.", Body: "Check weather.", Enabled: true, Source: RuleSourceAssistant}}
}
func existingRuleReview(r *Rule, op string) RuleReview {
	return RuleReview{RequestID: uuid.NewString(), ReviewedAt: time.Now().UTC().Format(time.RFC3339Nano), Operation: op, ExpectedVersion: r.Version, ExpectedUpdatedAt: r.UpdatedAt,
		ExpectedRule: &RuleReviewSnapshot{Description: r.Description, Body: r.Body, Enabled: r.Enabled},
		Proposed:     *r}
}
func TestRuleReviewCreateUpdateDeleteAndAudit(t *testing.T) {
	st, f := reviewStore(t)
	ctx := context.Background()
	review := newRuleReview()
	review.Proposed.Description = " Load   for trip\nplanning. "
	got, err := st.ApplyRuleReview(ctx, "alice", review)
	require.NoError(t, err)
	require.Equal(t, "saved", got.Status)
	require.Equal(t, RuleSourceUser, got.Rule.Source)
	require.Equal(t, "Load for trip planning.", got.Rule.Description)
	require.Equal(t, 1, got.Rule.Version)
	audit := f.RawItem("USER#alice", "RULEREVIEW#"+got.ReviewID)
	require.NotNil(t, audit)
	require.Contains(t, audit, "afterHash")
	require.NotContains(t, audit, "body")
	require.NotContains(t, audit, "description")
	require.Contains(t, audit, "ttl")
	// Duplicate create cannot overwrite or add another receipt.
	_, err = st.ApplyRuleReview(ctx, "alice", review)
	require.ErrorIs(t, err, ErrRuleReviewAlreadyApplied)
	update := existingRuleReview(got.Rule, "save")
	update.Proposed.Body = "Check weather and passport."
	next, err := st.ApplyRuleReview(ctx, "alice", update)
	require.NoError(t, err)
	require.Equal(t, 2, next.Rule.Version)
	require.Equal(t, got.Rule.CreatedAt, next.Rule.CreatedAt)
	_, err = st.ApplyRuleReview(ctx, "alice", update)
	require.ErrorIs(t, err, ErrRuleReviewAlreadyApplied)
	// A valid review for Alice's version cannot mutate Bob's partition.
	_, err = st.ApplyRuleReview(ctx, "bob", existingRuleReview(next.Rule, "delete"))
	require.ErrorIs(t, err, ErrRuleReviewConflict)
	require.NotNil(t, f.RawItem("USER#alice", "RULE#trip-check"))
	deletion := existingRuleReview(next.Rule, "delete")
	deleted, err := st.ApplyRuleReview(ctx, "alice", deletion)
	require.NoError(t, err)
	require.Equal(t, "deleted", deleted.Status)
	require.Nil(t, deleted.Rule)
	require.Nil(t, f.RawItem("USER#alice", "RULE#trip-check"))
	require.NotNil(t, f.RawItem("USER#alice", "RULEREVIEW#"+deleted.ReviewID))
	_, err = st.ApplyRuleReview(ctx, "alice", deletion)
	require.ErrorIs(t, err, ErrRuleReviewAlreadyApplied)
}

func TestRuleReviewRejectsChangedSnapshotAndABARace(t *testing.T) {
	st, f := reviewStore(t)
	ctx := context.Background()
	got, err := st.ApplyRuleReview(ctx, "alice", newRuleReview())
	require.NoError(t, err)
	review := existingRuleReview(got.Rule, "save")
	review.Proposed.Body = "The reviewed replacement."
	f.before = func(*dynamodb.TransactWriteItemsInput) {
		current := f.RawItem("USER#alice", "RULE#trip-check")
		// Simulate a legacy recreate with equal version and second-resolution
		// timestamp but changed text after the route/store's validation reads.
		current["body"] = &types.AttributeValueMemberS{Value: "Different unreviewed text."}
		f.SeedItem(current)
		f.before = nil
	}
	_, err = st.ApplyRuleReview(ctx, "alice", review)
	require.ErrorIs(t, err, ErrRuleReviewConflict)
	current, _ := st.GetRule(ctx, "alice", "trip-check")
	require.Equal(t, "Different unreviewed text.", current.Body)
	auditKey := f.last.TransactItems[len(f.last.TransactItems)-1].Put.Item["sk"].(*types.AttributeValueMemberS).Value
	require.Nil(t, f.RawItem("USER#alice", auditKey), "audit and mutation must fail together")
}

func TestRuleReviewPurgeAndGrantRevocationFence(t *testing.T) {
	for _, mode := range []string{"purge", "inactive", "role-change", "grant-revoked", "identity-change"} {
		t.Run(mode, func(t *testing.T) {
			st, f := reviewStore(t)
			ctx := context.Background()
			if mode == "grant-revoked" || mode == "identity-change" {
				p := f.RawItem("USER#alice", "PROFILE")
				p["role"] = &types.AttributeValueMemberS{Value: RoleMember}
				f.SeedItem(p)
				require.NoError(t, st.AddAllow(ctx, "alice@example.com", "owner"))
			}
			f.before = func(*dynamodb.TransactWriteItemsInput) {
				p := f.RawItem("USER#alice", "PROFILE")
				switch mode {
				case "purge":
					_, err := f.DeleteItem(ctx, &dynamodb.DeleteItemInput{Key: keyOf("USER#alice", "PROFILE")})
					require.NoError(t, err)
				case "inactive":
					p["status"] = &types.AttributeValueMemberS{Value: "deleting"}
					f.SeedItem(p)
				case "role-change":
					p["role"] = &types.AttributeValueMemberS{Value: RoleMember}
					f.SeedItem(p)
				case "grant-revoked":
					require.NoError(t, st.RemoveAllow(ctx, "alice@example.com"))
				case "identity-change":
					p["email"] = &types.AttributeValueMemberS{Value: "elsewhere@example.com"}
					f.SeedItem(p)
				}
				f.before = nil
			}
			_, err := st.ApplyRuleReview(ctx, "alice", newRuleReview())
			require.ErrorIs(t, err, ErrRuleReviewForbidden)
			require.Nil(t, f.RawItem("USER#alice", "RULE#trip-check"))
			auditKey := f.last.TransactItems[len(f.last.TransactItems)-1].Put.Item["sk"].(*types.AttributeValueMemberS).Value
			require.Nil(t, f.RawItem("USER#alice", auditKey))
		})
	}
}

func TestRuleReviewValidMemberGrantAndValidation(t *testing.T) {
	st, f := reviewStore(t)
	ctx := context.Background()
	p := f.RawItem("USER#alice", "PROFILE")
	p["role"] = &types.AttributeValueMemberS{Value: RoleMember}
	p["email"] = &types.AttributeValueMemberS{Value: "ALICE@Example.com"}
	f.SeedItem(p)
	_, err := st.ApplyRuleReview(ctx, "alice", newRuleReview())
	require.ErrorIs(t, err, ErrRuleReviewForbidden)
	require.NoError(t, st.AddAllow(ctx, "alice@example.com", "owner"))
	got, err := st.ApplyRuleReview(ctx, "alice", newRuleReview())
	require.NoError(t, err)
	require.Len(t, f.last.TransactItems, 4, "profile+allowlist+rule+audit")
	bad := existingRuleReview(got.Rule, "save")
	bad.ExpectedRule = nil
	_, err = st.ApplyRuleReview(ctx, "alice", bad)
	require.ErrorIs(t, err, ErrInvalidRule)
	bad = existingRuleReview(got.Rule, "save")
	bad.ExpectedUpdatedAt = ""
	_, err = st.ApplyRuleReview(ctx, "alice", bad)
	require.ErrorIs(t, err, ErrInvalidRule)
	bad = existingRuleReview(got.Rule, "save")
	bad.ExpectedRule.Enabled = false
	_, err = st.ApplyRuleReview(ctx, "alice", bad)
	require.ErrorIs(t, err, ErrRuleReviewConflict)
	bad = newRuleReview()
	bad.Operation = "delete"
	_, err = st.ApplyRuleReview(ctx, "alice", bad)
	require.ErrorIs(t, err, ErrInvalidRule)
	bad = newRuleReview()
	bad.Proposed.Body = ""
	_, err = st.ApplyRuleReview(ctx, "alice", bad)
	require.ErrorIs(t, err, ErrInvalidRule)
}

func TestRuleReviewSingleUseCreateCannotResurrectAfterDelete(t *testing.T) {
	st, f := reviewStore(t)
	ctx := context.Background()
	creation := newRuleReview()
	got, err := st.ApplyRuleReview(ctx, "alice", creation)
	require.NoError(t, err)
	deletion := existingRuleReview(got.Rule, "delete")
	_, err = st.ApplyRuleReview(ctx, "alice", deletion)
	require.NoError(t, err)
	_, err = st.ApplyRuleReview(ctx, "alice", creation)
	require.ErrorIs(t, err, ErrRuleReviewAlreadyApplied)
	require.Nil(t, f.RawItem("USER#alice", "RULE#trip-check"))
	// Reusing the consumed ID with different text is also blocked.
	changed := creation
	changed.Proposed.Body = "A different action under the same approval."
	_, err = st.ApplyRuleReview(ctx, "alice", changed)
	require.ErrorIs(t, err, ErrRuleReviewAlreadyApplied)
	require.Nil(t, f.RawItem("USER#alice", "RULE#trip-check"))
	receipt := f.RawItem("USER#alice", "RULEREVIEW#"+got.ReviewID)
	require.Equal(t, creation.RequestID, receipt["requestId"].(*types.AttributeValueMemberS).Value)
	require.Len(t, receipt["payloadHash"].(*types.AttributeValueMemberS).Value, 64)
	// Even after a receipt is physically removed by TTL, old review times
	// are independently rejected; TTL cleanup is never authorization.
	_, err = f.DeleteItem(ctx, &dynamodb.DeleteItemInput{Key: keyOf("USER#alice", "RULEREVIEW#"+got.ReviewID)})
	require.NoError(t, err)
	expired := creation
	expired.ReviewedAt = time.Now().Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
	_, err = st.ApplyRuleReview(ctx, "alice", expired)
	require.ErrorIs(t, err, ErrRuleReviewExpired)
	require.Nil(t, f.RawItem("USER#alice", "RULE#trip-check"))
	// An explicitly fresh review with a new ID is a new authorized action.
	fresh := newRuleReview()
	_, err = st.ApplyRuleReview(ctx, "alice", fresh)
	require.NoError(t, err)
}

func TestRuleReviewRejectsExpiredFutureAndMissingIdentity(t *testing.T) {
	for _, mode := range []string{"expired", "future", "missing-time", "bad-time", "missing-id", "zero-id", "bad-id"} {
		t.Run(mode, func(t *testing.T) {
			st, f := reviewStore(t)
			review := newRuleReview()
			want := ErrInvalidRule
			switch mode {
			case "expired":
				review.ReviewedAt = time.Now().Add(-RuleReviewWindow - time.Second).Format(time.RFC3339Nano)
				want = ErrRuleReviewExpired
			case "future":
				review.ReviewedAt = time.Now().Add(2 * time.Minute).Format(time.RFC3339Nano)
				want = ErrRuleReviewExpired
			case "missing-time":
				review.ReviewedAt = ""
			case "bad-time":
				review.ReviewedAt = "tomorrow"
			case "missing-id":
				review.RequestID = ""
			case "zero-id":
				review.RequestID = uuid.Nil.String()
			case "bad-id":
				review.RequestID = "not-a-uuid"
			}
			_, err := st.ApplyRuleReview(context.Background(), "alice", review)
			require.ErrorIs(t, err, want)
			require.Nil(t, f.last, "invalid approval never reaches a transaction")
			require.Nil(t, f.RawItem("USER#alice", "RULE#trip-check"))
		})
	}
}

func TestRuleReviewConsumedReceiptRaceRollsBackRule(t *testing.T) {
	st, f := reviewStore(t)
	ctx := context.Background()
	review := newRuleReview()
	f.before = func(in *dynamodb.TransactWriteItemsInput) {
		// A second use wins after the receipt pre-read but before commit.
		receipt := in.TransactItems[len(in.TransactItems)-1].Put.Item
		f.SeedItem(receipt)
		f.before = nil
	}
	_, err := st.ApplyRuleReview(ctx, "alice", review)
	require.ErrorIs(t, err, ErrRuleReviewAlreadyApplied)
	require.Nil(t, f.RawItem("USER#alice", "RULE#trip-check"))
}
