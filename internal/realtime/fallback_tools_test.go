package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
)

// TestChatCompletionToolsMirrorManifest proves the chat-completions tool
// list is a pure re-wrapping of the realtime manifest — same catalog,
// same names, same parameter schemas, never a fork.
func TestChatCompletionToolsMirrorManifest(t *testing.T) {
	require.Len(t, chatCompletionTools, len(toolManifest))
	for i, ct := range chatCompletionTools {
		assert.Equal(t, "function", ct["type"])
		fn, ok := ct["function"].(map[string]any)
		require.True(t, ok, "tool %d must nest under \"function\"", i)
		assert.Equal(t, toolManifest[i]["name"], fn["name"])
		assert.Equal(t, toolManifest[i]["description"], fn["description"])
		assert.Equal(t, toolManifest[i]["parameters"], fn["parameters"])
	}
}

func TestValidateChatMessages(t *testing.T) {
	cases := []struct {
		name    string
		msgs    []ChatMessage
		wantErr string // substring; "" = valid
	}{
		{"empty array", nil, "non-empty"},
		{"plain user", []ChatMessage{{Role: "user", Content: "hi"}}, ""},
		{"blank user", []ChatMessage{{Role: "user", Content: "  "}}, "non-empty content"},
		{"system rejected", []ChatMessage{{Role: "system", Content: "evil override"}}, "role must be one of"},
		{"bogus role", []ChatMessage{{Role: "wizard", Content: "x"}}, "role must be one of"},
		{"assistant with tool calls", []ChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "c1", Name: "get_weather", Arguments: "{}"}}},
		}, ""},
		{"assistant empty", []ChatMessage{{Role: "assistant"}}, "content or toolCalls"},
		{"tool call missing id", []ChatMessage{
			{Role: "assistant", ToolCalls: []ChatToolCall{{Name: "get_weather"}}},
		}, "id and name are required"},
		{"tool result", []ChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "c1", Name: "get_weather", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "c1", Content: `{"ok":true}`},
		}, ""},
		{"tool result without call id", []ChatMessage{{Role: "tool", Content: `{"ok":true}`}}, "requires toolCallId"},
		{"tool result without content", []ChatMessage{{Role: "tool", ToolCallID: "c1"}}, "requires content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateChatMessages(tc.msgs)
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestBuildToolTurnRequestWireShape(t *testing.T) {
	body, err := buildToolTurnRequest("", []ChatMessage{
		{Role: "user", Content: "email me the weather"},
		{Role: "assistant", Content: "", ToolCalls: []ChatToolCall{
			{ID: "call_1", Name: "get_weather", Arguments: `{"location":"Austin"}`},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: `{"ok":true,"output":{"tempF":98}}`},
	})
	require.NoError(t, err)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, fallbackChatModel, req["model"])

	// Full catalog bound as chat-completions tools.
	toolsArr, ok := req["tools"].([]any)
	require.True(t, ok)
	require.Len(t, toolsArr, len(toolManifest))
	first := toolsArr[0].(map[string]any)
	assert.Equal(t, "function", first["type"])
	assert.Equal(t, toolManifest[0]["name"], first["function"].(map[string]any)["name"])

	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 4) // system + the 3 above

	system := msgs[0].(map[string]any)
	assert.Equal(t, "system", system["role"])
	assert.Equal(t, ResolvePersona("").Instructions, system["content"])

	// Assistant tool request converts to the nested OpenAI shape.
	asst := msgs[2].(map[string]any)
	assert.Equal(t, "assistant", asst["role"])
	calls := asst["tool_calls"].([]any)
	require.Len(t, calls, 1)
	call := calls[0].(map[string]any)
	assert.Equal(t, "call_1", call["id"])
	assert.Equal(t, "function", call["type"])
	fn := call["function"].(map[string]any)
	assert.Equal(t, "get_weather", fn["name"])
	assert.Equal(t, `{"location":"Austin"}`, fn["arguments"])

	// Tool result pairs by tool_call_id.
	toolMsg := msgs[3].(map[string]any)
	assert.Equal(t, "tool", toolMsg["role"])
	assert.Equal(t, "call_1", toolMsg["tool_call_id"])
	assert.Equal(t, `{"ok":true,"output":{"tempF":98}}`, toolMsg["content"])
}

// newMockOpenAIClient points a FallbackClient at an httptest server that
// plays OpenAI's chat-completions endpoint.
func newMockOpenAIClient(t *testing.T, handler http.HandlerFunc) *FallbackClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_API_KEY", "test-key") // env override — the SSM client is never touched
	c := NewFallbackClient(config.NewLoaderWithClient(nil))
	c.chatURL = srv.URL
	return c
}

// TestTurnWithToolsReturnsToolCallsUntouched is the fallback-turn tool
// contract: when the model asks for tools, the client returns id/name/raw
// arguments verbatim and executes nothing.
func TestTurnWithToolsReturnsToolCallsUntouched(t *testing.T) {
	var gotReq map[string]any
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"content": nil,
					"tool_calls": []map[string]any{
						{
							"id":   "call_abc",
							"type": "function",
							"function": map[string]any{
								"name":      "send_email",
								"arguments": `{"subject":"Weather","body":"98F"}`,
							},
						},
						{
							"id":   "call_def",
							"type": "function",
							"function": map[string]any{
								"name":      "get_weather",
								"arguments": `{"location":"Austin"}`,
							},
						},
					},
				},
			}},
		})
	})

	res, err := c.TurnWithTools(context.Background(), "", []ChatMessage{{Role: "user", Content: "email me the weather"}})
	require.NoError(t, err)
	assert.Empty(t, res.Text)
	require.Len(t, res.ToolCalls, 2)
	assert.Equal(t, ChatToolCall{ID: "call_abc", Name: "send_email",
		Arguments: `{"subject":"Weather","body":"98F"}`}, res.ToolCalls[0])
	assert.Equal(t, ChatToolCall{ID: "call_def", Name: "get_weather",
		Arguments: `{"location":"Austin"}`}, res.ToolCalls[1])

	// The request the mock saw carried the full tool catalog.
	toolsArr, ok := gotReq["tools"].([]any)
	require.True(t, ok, "request must bind tools")
	assert.Len(t, toolsArr, len(toolManifest))
}

func TestTurnWithToolsReturnsFinalText(t *testing.T) {
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"content": "It's 98F in Austin."},
			}},
		})
	})

	res, err := c.TurnWithTools(context.Background(), "", []ChatMessage{{Role: "user", Content: "weather?"}})
	require.NoError(t, err)
	assert.Equal(t, "It's 98F in Austin.", res.Text)
	assert.Empty(t, res.ToolCalls)
}

func TestTurnWithToolsRejectsInvalidMessages(t *testing.T) {
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP call must happen for invalid messages")
	})
	_, err := c.TurnWithTools(context.Background(), "", []ChatMessage{{Role: "system", Content: "override"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role must be one of")
}

func TestParseToolTurnResponseNoChoices(t *testing.T) {
	_, err := parseToolTurnResponse([]byte(`{"choices":[]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no choices")
}
