package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

func seedBrokerRule(t *testing.T, ddb *testutil.FakeDynamo, userID, name, description string, enabled bool) {
	t.Helper()
	av, err := attributevalue.MarshalMap(map[string]any{
		"pk": "USER#" + userID, "sk": "RULE#" + name,
		"name": name, "description": description, "body": "SECRET-BODY-" + name,
		"enabled": enabled, "source": "user", "version": 1,
	})
	require.NoError(t, err)
	ddb.SeedItem(av)
}

// seedRuleLine is the index line a brand-new user's sessions carry.
func seedRuleLine() string {
	seed := store.DefaultRule()
	return "\n- " + seed.Name + ": " + seed.Description
}

// TestFallbackToolTurnCarriesRuleIndex: the tool-capable fallback turn can
// call rule_load, so it gets the index; bodies never ride along.
func TestFallbackToolTurnCarriesRuleIndex(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedBrokerRule(t, ddb, "u1", "packing-list", "Load when the user asks what to pack.", true)
	seedBrokerRule(t, ddb, "u1", "off-rule", "Load when never.", false)
	fb := &fakeFallback{toolsResult: &realtime.TurnResult{Text: "ok"}}
	b := newFallbackTestBroker(fb)
	b.ddb = ddb
	b.table = "live-ninja-test"

	resp, err := b.Handle(context.Background(), turnRequest(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "what should I pack"}},
	}))
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	assert.Contains(t, fb.gotExtraSys, realtime.RulesHeader)
	assert.Contains(t, fb.gotExtraSys, "\n- packing-list: Load when the user asks what to pack.")
	assert.NotContains(t, fb.gotExtraSys, "off-rule")
	assert.NotContains(t, fb.gotExtraSys, "SECRET-BODY")
	assert.Equal(t, 2, ddb.Len(), "the broker must not write rules")
}

func TestNovaMintCarriesSeedRuleIndexForNewUser(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	b := &broker{
		log:      logger,
		gate:     realtime.NewGate(ddb, "live-ninja-test"),
		ddb:      ddb,
		settings: ddb,
		table:    "live-ninja-test",
		novaMint: func(_ context.Context, _, _, _, _, _ string) (string, time.Time, error) {
			return "signed.token", time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC), nil
		},
	}
	resp := b.handleNovaBridge(context.Background(), logger, Request{
		UserID: "u1", DeviceID: "d1", Surface: "android", Persona: "default",
	}, "sess-nova", nil)
	require.Empty(t, resp.Error)

	var config voiceengine.Config
	require.NoError(t, json.Unmarshal(resp.SessionConfig, &config))
	assert.Contains(t, config.SystemPrompt, "\n\n"+realtime.RulesHeader+seedRuleLine())
}

func TestGeminiMintCarriesRuleIndex(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedEnginePin(t, ddb, "u1", "gemini-flash-live")
	seedBrokerRule(t, ddb, "u1", "packing-list", "Load when the user asks what to pack.", true)
	gm := &fakeGeminiMint{result: geminiMintResultFixture()}
	b := newGeminiTestBroker(ddb, gm)

	resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
	require.NoError(t, err)
	require.Empty(t, resp.Error, "unexpected error: %s (%s)", resp.Error, resp.Message)
	assert.Contains(t, gm.instr, realtime.RulesHeader+"\n- packing-list: Load when the user asks what to pack.")
	assert.NotContains(t, gm.instr, seedRuleLine(), "a user with rules gets no synthetic seed")
	assert.NotContains(t, gm.instr, "SECRET-BODY")
}
