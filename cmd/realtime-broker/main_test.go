package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/JeremyProffittOrg/live-ninja/internal/voiceengine"
)

// newTestBroker builds a broker whose only wired dependency is a JSON logger
// writing to buf. The error paths exercised here (missing userId, invalid
// surface, unknown mode) all return before any gate/minter/ddb touch, so the
// remaining nil dependencies are never dereferenced.
func newTestBroker(buf *bytes.Buffer) *broker {
	return &broker{
		log: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func logLinesWith(t *testing.T, buf *bytes.Buffer, sub string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "log line must be valid JSON: %s", line)
		if msg, _ := m["msg"].(string); strings.Contains(msg, sub) {
			out = append(out, m)
		}
	}
	return out
}

// fakeFallback scripts the fallbackAPI seam so fallback-turn handler tests
// need no HTTP. Only the methods a test scripts are expected to run.
type fakeFallback struct {
	turnText      string
	turnErr       error
	toolsResult   *realtime.TurnResult
	toolsErr      error
	gotTurnText   string
	gotMessages   []realtime.ChatMessage
	turnCalls     int
	turnWithTools int
}

func (f *fakeFallback) TurnForSurface(_ context.Context, _, _ string, text string, _ string) (string, error) {
	f.turnCalls++
	f.gotTurnText = text
	return f.turnText, f.turnErr
}

func (f *fakeFallback) TurnWithToolsForSurface(_ context.Context, _, _ string, messages []realtime.ChatMessage, _ string) (*realtime.TurnResult, error) {
	f.turnWithTools++
	f.gotMessages = messages
	return f.toolsResult, f.toolsErr
}

func (f *fakeFallback) Transcribe(context.Context, []byte, string, string) (string, error) {
	return "", errors.New("not scripted")
}

func (f *fakeFallback) Speak(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("not scripted")
}

func (f *fakeFallback) ExtractTopics(context.Context, string, []realtime.TopicOption) (*realtime.ExtractResult, error) {
	return nil, errors.New("not scripted")
}

// newFallbackTestBroker wires a broker whose gate runs over FakeDynamo (so
// the fallback quota path really executes) and whose OpenAI legs are faked.
func newFallbackTestBroker(fb *fakeFallback) *broker {
	return &broker{
		log:      slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		gate:     realtime.NewGate(testutil.NewFakeDynamo(), "live-ninja-test"),
		fallback: fb,
	}
}

func turnRequest(t *testing.T, payload map[string]any) Request {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return Request{Mode: "fallback-turn", UserID: "u1", Surface: "web", Payload: raw}
}

// TestFallbackTurnReturnsToolCallsUntouched is the broker-side tool
// contract: mode "fallback-turn" with a messages payload passes the
// model's tool_calls back verbatim and never executes anything.
func TestFallbackTurnReturnsToolCallsUntouched(t *testing.T) {
	fb := &fakeFallback{toolsResult: &realtime.TurnResult{
		ToolCalls: []realtime.ChatToolCall{
			{ID: "call_1", Name: "send_email", Arguments: `{"subject":"Hi","body":"Hello"}`},
		},
	}}
	b := newFallbackTestBroker(fb)

	resp, err := b.Handle(context.Background(), turnRequest(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "email me"}},
	}))
	require.NoError(t, err)
	require.Empty(t, resp.Error, "unexpected error: %s (%s)", resp.Error, resp.Message)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, realtime.ChatToolCall{
		ID: "call_1", Name: "send_email", Arguments: `{"subject":"Hi","body":"Hello"}`,
	}, resp.ToolCalls[0])
	assert.Equal(t, 1, fb.turnWithTools)
	assert.Equal(t, 0, fb.turnCalls, "the legacy text leg must not run in messages mode")
	require.Len(t, fb.gotMessages, 1)
	assert.Equal(t, "email me", fb.gotMessages[0].Content)
}

func TestFallbackTurnLegacyTextStillWorks(t *testing.T) {
	fb := &fakeFallback{turnText: "hello there"}
	b := newFallbackTestBroker(fb)

	resp, err := b.Handle(context.Background(), turnRequest(t, map[string]any{"text": "hi"}))
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	assert.Equal(t, "hello there", resp.Text)
	assert.Empty(t, resp.ToolCalls)
	assert.Equal(t, 1, fb.turnCalls)
	assert.Equal(t, 0, fb.turnWithTools)
	assert.Equal(t, "hi", fb.gotTurnText)
}

func TestHandleNovaBridgeReturnsDigestBoundServerConfig(t *testing.T) {
	fakeDDB := testutil.NewFakeDynamo()
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	var signedDigest string
	b := &broker{
		log:      logger,
		gate:     realtime.NewGate(fakeDDB, "live-ninja-test"),
		ddb:      fakeDDB,
		settings: fakeDDB,
		table:    "live-ninja-test",
		novaMint: func(_ context.Context, _, _, _, _, configDigest string) (string, time.Time, error) {
			signedDigest = configDigest
			return "signed.token", time.Date(2026, 7, 25, 20, 0, 0, 0, time.UTC), nil
		},
	}

	resp := b.handleNovaBridge(context.Background(), logger, Request{
		UserID: "u1", DeviceID: "d1", Surface: "android", Persona: "default",
	}, "sess-nova", nil)
	require.Empty(t, resp.Error)
	require.NotEmpty(t, resp.SessionConfig)
	require.NotEmpty(t, resp.ToolManifest)
	assert.Equal(t, "nova-bridge", resp.Mode)

	var config voiceengine.Config
	require.NoError(t, json.Unmarshal(resp.SessionConfig, &config))
	assert.Contains(t, config.SystemPrompt, "Always speak and respond in English")
	assert.Contains(t, config.SystemPrompt, realtime.SessionDirectives)
	assert.NotContains(t, config.SystemPrompt, "stop_listening")
	assert.Equal(t, realtime.NovaConfigDigest(config), signedDigest)

	names := make(map[string]bool, len(config.Tools))
	for _, spec := range config.Tools {
		names[spec.Name] = true
	}
	assert.True(t, names["send_email"])
	for _, local := range []string{
		"stop_listening", "start_new_conversation", "set_volume", "take_photo", "record_video",
	} {
		assert.False(t, names[local], local)
	}
}

func TestFallbackTurnValidatesPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"empty payload", map[string]any{}},
		{"blank text", map[string]any{"text": "   "}},
		{"bad role", map[string]any{"messages": []map[string]any{{"role": "system", "content": "x"}}}},
		{"tool message without call id", map[string]any{"messages": []map[string]any{{"role": "tool", "content": "x"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeFallback{}
			b := newFallbackTestBroker(fb)
			resp, err := b.Handle(context.Background(), turnRequest(t, tc.payload))
			require.NoError(t, err)
			assert.Equal(t, "invalid_request", resp.Error)
			assert.Equal(t, 0, fb.turnCalls+fb.turnWithTools, "no OpenAI leg may run on invalid payload")
		})
	}
}

func TestHandleErrorsCarryTxID(t *testing.T) {
	cases := []struct {
		name string
		req  Request
	}{
		{"missing userId", Request{Surface: "web"}},
		{"invalid surface", Request{UserID: "u1", Surface: "bogus"}},
		{"unknown mode", Request{UserID: "u1", Surface: "web", Mode: "teleport"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBroker(&bytes.Buffer{})
			resp, err := b.Handle(context.Background(), tc.req)
			require.NoError(t, err)
			assert.NotEmpty(t, resp.Error, "expected an error response")
			assert.NotEmpty(t, resp.TxID, "every error response must carry a txId")
		})
	}
}

func TestHandleGeneratesTxIDWhenAbsent(t *testing.T) {
	b := newTestBroker(&bytes.Buffer{})
	resp, err := b.Handle(context.Background(), Request{Surface: "web"}) // missing userId
	require.NoError(t, err)
	require.NotEmpty(t, resp.TxID)
	assert.NotEqual(t, "", resp.Error)
}

func TestHandlePreservesSuppliedTxID(t *testing.T) {
	b := newTestBroker(&bytes.Buffer{})
	resp, err := b.Handle(context.Background(), Request{
		TxID:    "web-forwarded-txn",
		Surface: "web", // missing userId -> error, but txId must be echoed
	})
	require.NoError(t, err)
	assert.Equal(t, "web-forwarded-txn", resp.TxID)
}

func TestHandleLogsIncludeTxID(t *testing.T) {
	buf := &bytes.Buffer{}
	b := newTestBroker(buf)
	_, err := b.Handle(context.Background(), Request{
		TxID:    "tx-broker-logged",
		UserID:  "u1",
		Surface: "web",
		Mode:    "teleport", // unknown mode -> error, but start/done still log
	})
	require.NoError(t, err)

	starts := logLinesWith(t, buf, "invoke start")
	dones := logLinesWith(t, buf, "invoke done")
	require.NotEmpty(t, starts, "expected an 'invoke start' log line")
	require.NotEmpty(t, dones, "expected an 'invoke done' log line")
	assert.Equal(t, "tx-broker-logged", starts[0]["txId"])
	assert.Equal(t, "tx-broker-logged", dones[0]["txId"])
	assert.Equal(t, "error", dones[0]["outcome"])
}

// TestAzureClientGateFailsClosed covers WS-D M1. The broker is invoked with a
// marshaled struct and sees no HTTP headers of its own, so before this gate
// existed there was no way to distinguish a client that can handle an Azure
// credential from one compiled against api.openai.com. The gate must therefore
// treat "said nothing" as "old client".
func TestAzureClientGateFailsClosed(t *testing.T) {
	azure := []voiceengine.Engine{
		voiceengine.EngineGPTLiveAzure,
		voiceengine.EngineGPTLiveAzureMini,
		voiceengine.EngineAzureVoiceLive,
		voiceengine.EngineAzureVoiceLiveLite,
	}

	for _, engine := range azure {
		// No version, no capabilities: the shape every already-installed
		// client has. Must NOT be trusted with an Azure credential.
		if clientSupportsAzure(Request{Surface: "web"}, engine) {
			t.Errorf("%s: a client sending neither version nor capabilities was trusted", engine)
		}
		// A garbage header must not be trusted either.
		if clientSupportsAzure(Request{Surface: "web", ClientVersion: "not-a-version"}, engine) {
			t.Errorf("%s: an unparseable X-LN-Client was trusted", engine)
		}
		// The live Android build's real header does not match the
		// contracts/headers.md grammar, so it must fail closed too.
		if clientSupportsAzure(Request{Surface: "android", ClientVersion: "android/0.2.2-hal+r5"}, engine) {
			t.Errorf("%s: the live android header was trusted despite not parsing", engine)
		}
		// Declaring the WRONG mode must not unlock the other one.
		wrong := "azure-direct"
		if !engine.IsVoiceLive() {
			wrong = "voice-live-direct"
		}
		if clientSupportsAzure(Request{Surface: "web", Capabilities: []string{wrong}}, engine) {
			t.Errorf("%s: declaring %q unlocked the wrong bootstrap mode", engine, wrong)
		}
		// Declaring the RIGHT mode is what unlocks it.
		if !clientSupportsAzure(Request{Surface: "web", Capabilities: []string{modeForEngine(engine)}}, engine) {
			t.Errorf("%s: declaring %q did not unlock it", engine, modeForEngine(engine))
		}
	}

	// A new-enough Android build qualifies on version alone, so clients that
	// shipped before the capability header still work.
	if !clientSupportsAzure(
		Request{Surface: "android", ClientVersion: "android/0.3.0+r6"},
		voiceengine.EngineGPTLiveAzure,
	) {
		t.Error("android 0.3.0 should meet the surface minimum")
	}
	if clientSupportsAzure(
		Request{Surface: "android", ClientVersion: "android/0.2.9+r5"},
		voiceengine.EngineGPTLiveAzure,
	) {
		t.Error("android 0.2.9 is below the surface minimum and must fail closed")
	}
	// m5stack has no Azure-capable build; it must never qualify on version.
	if clientSupportsAzure(
		Request{Surface: "m5stack", ClientVersion: "m5stack/8.0.0+2026"},
		voiceengine.EngineGPTLiveAzure,
	) {
		t.Error("no m5stack firmware supports Azure; it must not qualify on version")
	}
}

// TestSessionResponseCarriesCallsURL covers WS-D M2. callsUrl rides the
// DEFAULT openai-direct path, not just the Azure one, so the field is
// exercised by every session and cannot rot between releases.
func TestSessionResponseCarriesCallsURL(t *testing.T) {
	var resp Response
	if err := json.Unmarshal([]byte(`{"mode":"openai-direct","callsUrl":"https://api.openai.com/v1/realtime/calls"}`), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.CallsURL != realtime.OpenAICallsURL {
		t.Errorf("callsUrl round-trip = %q, want %q", resp.CallsURL, realtime.OpenAICallsURL)
	}

	// It must serialize under exactly the name the clients read.
	out, err := json.Marshal(Response{Mode: "openai-direct", CallsURL: realtime.OpenAICallsURL})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"callsUrl":"https://api.openai.com/v1/realtime/calls"`) {
		t.Errorf("callsUrl missing or misnamed in %s", out)
	}
}

// TestAzurePinRoutesToTheAzureMinter covers WS-B M6. Three things must hold:
// an Azure-capable client pinned to an Azure engine reaches the AZURE minter
// and is told the AZURE SDP host; the same pin on an unconfigured broker
// cascades to openai-realtime with a warning instead of failing; and the
// OpenAI minter is never handed an Azure session.
func TestAzurePinRoutesToTheAzureMinter(t *testing.T) {
	const azureCalls = "https://ln-aoai-eastus2.openai.azure.com/openai/v1/realtime/calls"

	newMint := func(secret, model, calls string) *fakeRealtimeMint {
		return &fakeRealtimeMint{
			result: &realtime.MintResult{
				ClientSecret: realtime.ClientSecret{Value: secret, ExpiresAt: "2026-08-24T20:00:00Z"},
				Model:        model,
				Voice:        "cedar",
			},
			callsURL: calls,
		}
	}

	t.Run("routes to azure and names the azure host", func(t *testing.T) {
		ddb := testutil.NewFakeDynamo()
		seedEnginePin(t, ddb, "u1", string(voiceengine.EngineGPTLiveAzure))
		openai := newMint("ek_openai", "gpt-realtime", "")
		azure := newMint("ek_azure", "gpt-realtime-2-1", azureCalls)

		b := newGeminiTestBroker(ddb, nil)
		b.log = slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
		b.minter = openai
		b.azureMinter = azure

		resp, err := b.Handle(context.Background(), Request{
			UserID: "u1", Surface: "web",
			Capabilities: []string{"azure-direct"},
		})
		require.NoError(t, err)
		require.Empty(t, resp.Error)
		assert.Equal(t, "azure-direct", resp.Mode)
		assert.Equal(t, string(voiceengine.EngineGPTLiveAzure), resp.Engine)
		assert.Equal(t, "gpt-realtime-2-1", resp.Model)
		assert.Equal(t, azureCalls, resp.CallsURL, "the client must be told the Azure SDP host")
		assert.Equal(t, 1, azure.calls, "the Azure minter should have been used")
		assert.Equal(t, 0, openai.calls, "the OpenAI minter must never serve an Azure session")
	})

	t.Run("unconfigured azure cascades to openai instead of failing", func(t *testing.T) {
		ddb := testutil.NewFakeDynamo()
		seedEnginePin(t, ddb, "u1", string(voiceengine.EngineGPTLiveAzure))
		openai := newMint("ek_openai", "gpt-realtime", "")

		b := newGeminiTestBroker(ddb, nil)
		b.log = slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
		b.minter = openai
		b.azureMinter = nil // endpoint not configured

		resp, err := b.Handle(context.Background(), Request{
			UserID: "u1", Surface: "web",
			Capabilities: []string{"azure-direct"},
		})
		require.NoError(t, err)
		require.Empty(t, resp.Error, "an unconfigured Azure engine must not 502 a session openai could serve")
		assert.Equal(t, "openai-direct", resp.Mode)
		assert.Equal(t, string(voiceengine.EngineOpenAIRealtime), resp.Engine)
		assert.Equal(t, realtime.OpenAICallsURL, resp.CallsURL)
		assert.Equal(t, 1, openai.calls)
		assert.Contains(t, resp.QuotaWarning, "Azure voice engine is unavailable",
			"the user should be told once, plainly, that the engine changed")
	})

	t.Run("a client that declares nothing never reaches azure", func(t *testing.T) {
		ddb := testutil.NewFakeDynamo()
		seedEnginePin(t, ddb, "u1", string(voiceengine.EngineGPTLiveAzure))
		openai := newMint("ek_openai", "gpt-realtime", "")
		azure := newMint("ek_azure", "gpt-realtime-2-1", azureCalls)

		b := newGeminiTestBroker(ddb, nil)
		b.log = slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
		b.minter = openai
		b.azureMinter = azure

		// No Capabilities, no ClientVersion: an already-installed client.
		resp, err := b.Handle(context.Background(), Request{UserID: "u1", Surface: "web"})
		require.NoError(t, err)
		require.Empty(t, resp.Error)
		assert.Equal(t, "openai-direct", resp.Mode)
		assert.Equal(t, string(voiceengine.EngineOpenAIRealtime), resp.Engine)
		assert.Equal(t, realtime.OpenAICallsURL, resp.CallsURL,
			"an old client must never be handed an Azure host")
		assert.Equal(t, 0, azure.calls, "an old client must never reach the Azure minter")
	})
}
