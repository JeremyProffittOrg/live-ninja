package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
