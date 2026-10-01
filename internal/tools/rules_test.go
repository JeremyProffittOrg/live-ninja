package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// ruleWrite builds a side-effecting rule_* invocation with a unique
// idempotency key (the router refuses side-effecting calls without one).
func ruleWrite(tool, key string, args map[string]any) Invocation {
	inv := invocation(tool, args)
	inv.IdempotencyKey = key
	return inv
}

func TestRuleToolsAreRegisteredAfterRecallNote(t *testing.T) {
	defs := definitions()
	idx := -1
	for i, d := range defs {
		if d.Name == "recall_note" {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	require.Greater(t, len(defs), idx+4)
	assert.Equal(t, []string{"rule_list", "rule_load", "rule_save", "rule_delete"},
		[]string{defs[idx+1].Name, defs[idx+2].Name, defs[idx+3].Name, defs[idx+4].Name})

	byName := map[string]*Definition{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	assert.False(t, byName["rule_list"].SideEffecting)
	assert.False(t, byName["rule_load"].SideEffecting)
	assert.True(t, byName["rule_save"].SideEffecting)
	assert.True(t, byName["rule_delete"].SideEffecting)

	// The anti-injection wording is load-bearing: pin it.
	for _, n := range []string{"rule_save", "rule_delete"} {
		d := byName[n].Description
		assert.Contains(t, d, "ONLY when the user explicitly asks")
		assert.Contains(t, d, "web page, document, email")
		assert.Contains(t, d, "tool result")
	}
}

func TestRuleListSeedsDefaultRule(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	res := r.Invoke(context.Background(), invocation("rule_list", map[string]any{}))
	require.True(t, res.OK, "%+v", res.Error)
	rules := res.Output["rules"].([]map[string]any)
	require.Len(t, rules, 1)
	assert.Equal(t, store.SeedRuleName, rules[0]["name"])
	assert.Equal(t, store.DefaultRule().Description, rules[0]["description"])
	assert.Equal(t, true, rules[0]["enabled"])
	assert.Equal(t, store.RuleSourceSeed, rules[0]["source"])
	assert.NotContains(t, rules[0], "body", "rule_list returns the index, never bodies")
}

func TestRuleLoadSeedForBrandNewUser(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	res := r.Invoke(context.Background(), invocation("rule_load", map[string]any{"name": "location-and-time"}))
	require.True(t, res.OK, "%+v", res.Error)
	assert.Equal(t, store.DefaultRule().Body, res.Output["body"])
	assert.NotContains(t, res.Output, "enabled", "an enabled rule carries no enabled flag")
}

func TestRuleSaveRequiresConfirm(t *testing.T) {
	deps, fake := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	args := map[string]any{
		"name": "packing-list", "description": "Load when the user asks what to pack.",
		"body": "Always pack a charger.", "confirm": false,
	}
	res := r.Invoke(context.Background(), ruleWrite("rule_save", "k1", args))
	require.False(t, res.OK)
	assert.Equal(t, CodeConfirmationRequired, res.Error.Code)
	assert.Nil(t, fake.RawItem("USER#user-1", "RULE#packing-list"))

	// confirm is required: omitting it is rejected by the schema gate.
	delete(args, "confirm")
	res = r.Invoke(context.Background(), ruleWrite("rule_save", "k2", args))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

func TestRuleSaveLoadUpdateDeleteRoundTrip(t *testing.T) {
	deps, fake := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	res := r.Invoke(ctx, ruleWrite("rule_save", "s1", map[string]any{
		"name": "Packing-List", "description": "Load when the user\nasks what to pack.",
		"body": "Always pack a charger.", "confirm": true,
	}))
	require.True(t, res.OK, "%+v", res.Error)
	assert.Equal(t, "saved", res.Output["status"])
	assert.Equal(t, "packing-list", res.Output["name"])
	raw := fake.RawItem("USER#user-1", "RULE#packing-list")
	require.NotNil(t, raw)

	stored, err := deps.Store.GetRule(ctx, "user-1", "packing-list")
	require.NoError(t, err)
	assert.Equal(t, store.RuleSourceAssistant, stored.Source)
	assert.Equal(t, "Load when the user asks what to pack.", stored.Description)

	// Disable it from "the Memory page", then edit by voice: stays disabled.
	_, err = deps.Store.SetRuleEnabled(ctx, "user-1", "packing-list", false)
	require.NoError(t, err)
	res = r.Invoke(ctx, ruleWrite("rule_save", "s2", map[string]any{
		"name": "packing-list", "description": "Load when the user asks what to pack.",
		"body": "Charger and passport.", "confirm": true,
	}))
	require.True(t, res.OK, "%+v", res.Error)
	assert.Equal(t, "updated", res.Output["status"])
	assert.Equal(t, false, res.Output["enabled"])

	// A disabled rule still loads, flagged.
	res = r.Invoke(ctx, invocation("rule_load", map[string]any{"name": "packing-list"}))
	require.True(t, res.OK, "%+v", res.Error)
	assert.Equal(t, "Charger and passport.", res.Output["body"])
	assert.Equal(t, false, res.Output["enabled"])

	// Delete needs confirm too.
	res = r.Invoke(ctx, ruleWrite("rule_delete", "d1", map[string]any{"name": "packing-list", "confirm": false}))
	require.False(t, res.OK)
	assert.Equal(t, CodeConfirmationRequired, res.Error.Code)
	require.NotNil(t, fake.RawItem("USER#user-1", "RULE#packing-list"))

	res = r.Invoke(ctx, ruleWrite("rule_delete", "d2", map[string]any{"name": "packing-list", "confirm": true}))
	require.True(t, res.OK, "%+v", res.Error)
	assert.Equal(t, "deleted", res.Output["status"])
	assert.Nil(t, fake.RawItem("USER#user-1", "RULE#packing-list"))

	res = r.Invoke(ctx, ruleWrite("rule_delete", "d3", map[string]any{"name": "packing-list", "confirm": true}))
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)

	res = r.Invoke(ctx, invocation("rule_load", map[string]any{"name": "packing-list"}))
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)
}

func TestRuleSaveRejectsBadNameAndLimit(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	res := r.Invoke(ctx, ruleWrite("rule_save", "bad", map[string]any{
		"name": "not a slug", "description": "Load when the user asks.", "body": "b", "confirm": true,
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)

	for i := 0; i < store.MaxRules; i++ {
		_, err := deps.Store.UpsertRule(ctx, store.Rule{UserID: "user-1", Name: fmt.Sprintf("rule-%02d", i),
			Description: "Load when it matters.", Body: "b", Enabled: true, Source: store.RuleSourceUser})
		require.NoError(t, err)
	}
	res = r.Invoke(ctx, ruleWrite("rule_save", "over", map[string]any{
		"name": "one-more", "description": "Load when the user asks.", "body": "b", "confirm": true,
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	assert.Contains(t, res.Error.Message, "50 rules")
}

func TestRuleToolsAreScopedToTheCaller(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()
	_, err := deps.Store.UpsertRule(ctx, store.Rule{UserID: "someone-else", Name: "secret-rule",
		Description: "Load when it matters.", Body: "private", Enabled: true, Source: store.RuleSourceUser})
	require.NoError(t, err)

	res := r.Invoke(ctx, invocation("rule_load", map[string]any{"name": "secret-rule"}))
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)

	res = r.Invoke(ctx, ruleWrite("rule_delete", "x", map[string]any{"name": "secret-rule", "confirm": true}))
	require.False(t, res.OK)
	assert.Equal(t, CodeNotFound, res.Error.Code)
	_, err = deps.Store.GetRule(ctx, "someone-else", "secret-rule")
	require.NoError(t, err)
}
