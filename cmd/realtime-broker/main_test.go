package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
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

func (f *fakeFallback) Turn(_ context.Context, _ string, text string, _ string) (string, error) {
	f.turnCalls++
	f.gotTurnText = text
	return f.turnText, f.turnErr
}

func (f *fakeFallback) TurnWithTools(_ context.Context, _ string, messages []realtime.ChatMessage, _ string) (*realtime.TurnResult, error) {
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
