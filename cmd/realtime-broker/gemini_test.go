package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// fakeGeminiMint scripts the geminiMintAPI seam.
type fakeGeminiMint struct {
	result *realtime.GeminiMintResult
	err    error
	calls  int
	voice  string
	instr  string
}

func (f *fakeGeminiMint) Mint(_ context.Context, voice, instructions string) (*realtime.GeminiMintResult, error) {
	f.calls++
	f.voice = voice
	f.instr = instructions
	return f.result, f.err
}

// seedEnginePin writes a settings document pinning the account default
// voiceEngine into the fake table.
func seedEnginePin(t *testing.T, ddb *testutil.FakeDynamo, userID, engine string) {
	t.Helper()
	item, err := attributevalue.MarshalMap(map[string]any{
		"voiceEngine": map[string]any{
			"default": engine,
			"devices": map[string]string{},
		},
	})
	require.NoError(t, err)
	item["pk"] = &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID}
	item["sk"] = &ddbtypes.AttributeValueMemberS{Value: "SETTINGS"}
	ddb.SeedItem(item)
}

// newGeminiTestBroker wires a broker whose gate/settings run over FakeDynamo
// and whose Gemini mint is faked; the OpenAI minter stays nil (any dispatch
// into it would panic, which is exactly the regression the tests watch for).
func newGeminiTestBroker(ddb *testutil.FakeDynamo, gm geminiMintAPI) *broker {
	return &broker{
		log:        slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		gate:       realtime.NewGate(ddb, "live-ninja-test"),
		ddb:        ddb,
		table:      "live-ninja-test",
		settings:   ddb,
		geminiMint: gm,
	}
}

func geminiMintResultFixture() *realtime.GeminiMintResult {
	return &realtime.GeminiMintResult{
		AccessToken: realtime.GeminiAccessToken{
			Value:               "auth_tokens/test-token",
			ExpiresAt:           "2026-07-19T12:30:00Z",
			NewSessionExpiresAt: "2026-07-19T12:02:00Z",
		},
		Model:         "gemini-3.1-flash-live-preview",
		Voice:         "Kore",
		SessionConfig: json.RawMessage(`{"model":"models/gemini-3.1-flash-live-preview"}`),
		ToolManifest:  realtime.ToolManifestJSON(),
	}
}

// TestMintGeminiPinnedReturnsGeminiDirectShape is the M13 bootstrap
// contract: a gemini-flash-live pin yields the gemini-direct shape with the
// constrained endpoint and token — and, critically, NO field in the
// wsUrl/bridgeUrl family anywhere in the marshaled JSON (legacy firmware
// detects Nova by field presence; gemini-plan.md §3.4).
func TestMintGeminiPinnedReturnsGeminiDirectShape(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedEnginePin(t, ddb, "u1", "gemini-flash-live")
	gm := &fakeGeminiMint{result: geminiMintResultFixture()}
	b := newGeminiTestBroker(ddb, gm)

	resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
	require.NoError(t, err)
	require.Empty(t, resp.Error, "unexpected error: %s (%s)", resp.Error, resp.Message)

	assert.Equal(t, "gemini-direct", resp.Mode)
	assert.Equal(t, "gemini-flash-live", resp.Engine)
	assert.Equal(t, "gemini-3.1-flash-live-preview", resp.Model)
	assert.Equal(t, realtime.GeminiLiveEndpoint, resp.GeminiEndpoint)
	require.NotNil(t, resp.AccessToken)
	assert.Equal(t, "auth_tokens/test-token", resp.AccessToken.Value)
	assert.NotEmpty(t, resp.SessionID)
	assert.Equal(t, 1, gm.calls)
	assert.Equal(t, "Achird", gm.voice, "no setting -> the default persona's hand-curated mapping")
	assert.NotEmpty(t, gm.instr, "instructions must carry the persona core")

	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var asMap map[string]any
	require.NoError(t, json.Unmarshal(raw, &asMap))
	for key := range asMap {
		assert.NotContains(t, []string{"wsUrl", "ws_url", "bridgeUrl", "bridge_url"}, key,
			"gemini-direct must never emit a wsUrl-family field (legacy Nova presence heuristic)")
	}
}

// TestMintGeminiVoiceResolution: a stored geminiVoice setting wins over the
// persona mapping; an unknown stored value falls through to the persona's
// hand-curated voice.
func TestMintGeminiVoiceResolution(t *testing.T) {
	cases := []struct {
		name        string
		geminiVoice string
		persona     string
		want        string
	}{
		{"setting wins", "Puck", "", "Puck"},
		{"unknown setting falls to persona", "not-a-voice", "zen-monk", "Vindemiatrix"},
		{"persona mapping when unset", "", "pirate-captain", "Algenib"},
		{"default persona mapping when unset", "", "", "Achird"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ddb := testutil.NewFakeDynamo()
			doc := map[string]any{
				"voiceEngine": map[string]any{"default": "gemini-flash-live", "devices": map[string]string{}},
			}
			if tc.geminiVoice != "" {
				doc["geminiVoice"] = tc.geminiVoice
			}
			item, err := attributevalue.MarshalMap(doc)
			require.NoError(t, err)
			item["pk"] = &ddbtypes.AttributeValueMemberS{Value: "USER#u1"}
			item["sk"] = &ddbtypes.AttributeValueMemberS{Value: "SETTINGS"}
			ddb.SeedItem(item)

			gm := &fakeGeminiMint{result: geminiMintResultFixture()}
			b := newGeminiTestBroker(ddb, gm)
			resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web", Persona: tc.persona})
			require.NoError(t, err)
			require.Empty(t, resp.Error)
			assert.Equal(t, tc.want, gm.voice)
		})
	}
}

// TestMintGeminiUnavailable: a gemini pin with no minter wired degrades to a
// structured 502 (mirrors nova_bridge_unavailable), never a panic.
func TestMintGeminiUnavailable(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedEnginePin(t, ddb, "u1", "gemini-flash-live")
	b := newGeminiTestBroker(ddb, nil)

	resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
	require.NoError(t, err)
	assert.Equal(t, "gemini_unavailable", resp.Error)
	assert.Equal(t, 502, resp.Code)
}

// TestMintGeminiMintFailure: a Google-side mint failure maps to the standard
// mint_failed 502 the clients' fallback cascade already handles.
func TestMintGeminiMintFailure(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedEnginePin(t, ddb, "u1", "gemini-flash-live")
	gm := &fakeGeminiMint{err: errors.New("boom")}
	b := newGeminiTestBroker(ddb, gm)

	resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
	require.NoError(t, err)
	assert.Equal(t, "mint_failed", resp.Error)
	assert.Equal(t, 502, resp.Code)
}

// TestMintNovaPinNeverTouchesGemini: regression guard — a nova pin dispatches
// to the bridge path (unavailable here) and the Gemini minter is never called.
func TestMintNovaPinNeverTouchesGemini(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedEnginePin(t, ddb, "u1", "nova-sonic")
	gm := &fakeGeminiMint{result: geminiMintResultFixture()}
	b := newGeminiTestBroker(ddb, gm)

	resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
	require.NoError(t, err)
	assert.Equal(t, "nova_bridge_unavailable", resp.Error)
	assert.Equal(t, 0, gm.calls)
}
