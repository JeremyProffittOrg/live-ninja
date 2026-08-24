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
	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

// fakeGeminiMint scripts the geminiMintAPI seam.
type fakeGeminiMint struct {
	result *realtime.GeminiMintResult
	err    error
	calls  int
	voice  string
	instr  string
}

type fakeRealtimeMint struct {
	result *realtime.MintResult
	err    error
	calls  int
	// callsURL lets a test stand in for an Azure minter, whose only visible
	// difference from the OpenAI one at this seam is the SDP host it names.
	callsURL string
}

func (f *fakeRealtimeMint) Mint(context.Context, string, string, string, string, string) (*realtime.MintResult, error) {
	f.calls++
	return f.result, f.err
}

func (f *fakeRealtimeMint) CallsURL() string {
	if f.callsURL != "" {
		return f.callsURL
	}
	return realtime.OpenAICallsURL
}

func (f *fakeGeminiMint) MintForSurface(_ context.Context, voice, instructions, _ string) (*realtime.GeminiMintResult, error) {
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

func TestMintNovaFailureFallsBackAndAttributesOpenAI(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedEnginePin(t, ddb, "u1", "nova-sonic")
	b := newGeminiTestBroker(ddb, nil)
	openAI := &fakeRealtimeMint{result: &realtime.MintResult{
		ClientSecret: realtime.ClientSecret{Value: "ek_test", ExpiresAt: "2026-07-25T21:00:00Z"},
		Model:        realtime.DefaultRealtimeModel,
		Voice:        "cedar",
	}}
	b.minter = openAI

	resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	assert.Equal(t, "openai-direct", resp.Mode)
	assert.Equal(t, string(voiceengine.EngineOpenAIRealtime), resp.Engine)
	assert.Equal(t, realtime.DefaultRealtimeModel, resp.Model)
	assert.Contains(t, resp.QuotaWarning, "Nova Sonic is unavailable")
	assert.Equal(t, 1, openAI.calls)
}

// TestMintGeminiFailureWithoutDefaultEngineDoesNotPanic guards the fallback's
// escape hatch. When a Gemini mint fails the broker now cascades to the default
// engine rather than returning 502 — but with no OpenAI minter configured there
// is nothing to cascade to, and dereferencing the nil minter would turn a handled
// error into a panicking Lambda. The original Gemini error must survive intact.
func TestMintGeminiFailureWithoutDefaultEngineDoesNotPanic(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedEnginePin(t, ddb, "u1", "gemini-flash-live")
	b := newGeminiTestBroker(ddb, &fakeGeminiMint{err: errors.New("boom")})
	require.Nil(t, b.minter, "this test's premise is that no default engine is wired")

	resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
	require.NoError(t, err)
	assert.Equal(t, "mint_failed", resp.Error)
	assert.Equal(t, 502, resp.Code)
}

func TestMintGeminiFailureFallsBackAndAttributesOpenAI(t *testing.T) {
	ddb := testutil.NewFakeDynamo()
	seedEnginePin(t, ddb, "u1", "gemini-flash-live")
	b := newGeminiTestBroker(ddb, &fakeGeminiMint{err: errors.New("google unavailable")})
	openAI := &fakeRealtimeMint{result: &realtime.MintResult{
		ClientSecret: realtime.ClientSecret{Value: "ek_test", ExpiresAt: "2026-07-25T21:00:00Z"},
		Model:        realtime.DefaultRealtimeModel,
		Voice:        "cedar",
	}}
	b.minter = openAI

	resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	assert.Equal(t, "openai-direct", resp.Mode)
	assert.Equal(t, "openai-realtime", resp.Engine)
	assert.Equal(t, realtime.DefaultRealtimeModel, resp.Model)
	assert.Contains(t, resp.QuotaWarning, "Gemini Live is unavailable")
	assert.Equal(t, 1, openAI.calls)
}

func TestMintOpenAIEnginePinRoutesAndAttributesActualEngine(t *testing.T) {
	const configuredModel = "gpt-realtime-configured-test"

	tests := []struct {
		name              string
		pin               voiceengine.Engine
		wantModel         string
		wantStandardCalls int
		wantMiniCalls     int
	}{
		{
			name:              "standard pin uses configured model minter",
			pin:               voiceengine.EngineOpenAIRealtime,
			wantModel:         configuredModel,
			wantStandardCalls: 1,
		},
		{
			name:          "mini pin uses fixed mini model minter",
			pin:           voiceengine.EngineOpenAIRealtimeMini,
			wantModel:     realtime.MiniRealtimeModel,
			wantMiniCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ddb := testutil.NewFakeDynamo()
			seedEnginePin(t, ddb, "u1", string(tc.pin))
			logs := &bytes.Buffer{}
			standard := &fakeRealtimeMint{result: &realtime.MintResult{
				ClientSecret: realtime.ClientSecret{Value: "ek_standard", ExpiresAt: "2026-07-26T20:00:00Z"},
				Model:        configuredModel,
				Voice:        "cedar",
			}}
			mini := &fakeRealtimeMint{result: &realtime.MintResult{
				ClientSecret: realtime.ClientSecret{Value: "ek_mini", ExpiresAt: "2026-07-26T20:00:00Z"},
				Model:        realtime.MiniRealtimeModel,
				Voice:        "cedar",
			}}
			b := newGeminiTestBroker(ddb, nil)
			b.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			b.minter = standard
			b.miniMinter = mini

			resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
			require.NoError(t, err)
			require.Empty(t, resp.Error)
			assert.Equal(t, "openai-direct", resp.Mode)
			assert.Equal(t, string(tc.pin), resp.Engine)
			assert.Equal(t, tc.wantModel, resp.Model)
			assert.Equal(t, tc.wantStandardCalls, standard.calls)
			assert.Equal(t, tc.wantMiniCalls, mini.calls)

			marker := ddb.RawItem("USER#u1", "LOG#"+resp.SessionID+"#000000")
			require.NotNil(t, marker)
			recordedEngine, ok := marker["engine"].(*ddbtypes.AttributeValueMemberS)
			require.True(t, ok)
			assert.Equal(t, string(tc.pin), recordedEngine.Value)

			minted := logLinesWith(t, logs, "session minted")
			require.Len(t, minted, 1)
			assert.Equal(t, string(tc.pin), minted[0]["engine"])
			assert.Equal(t, tc.wantModel, minted[0]["model"])
		})
	}
}
