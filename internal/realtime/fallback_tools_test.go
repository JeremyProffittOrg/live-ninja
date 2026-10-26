package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
)

// TestChatCompletionToolsMirrorServerManifest proves the chat-completions
// list is a pure re-wrapping of the server-executable manifest — same names
// and schemas, with device-local tools deliberately absent.
func TestChatCompletionToolsMirrorServerManifest(t *testing.T) {
	serverManifest := toolManifestForServerExecution()
	require.Len(t, chatCompletionTools, len(serverManifest))
	for i, ct := range chatCompletionTools {
		assert.Equal(t, "function", ct["type"])
		fn, ok := ct["function"].(map[string]any)
		require.True(t, ok, "tool %d must nest under \"function\"", i)
		assert.Equal(t, serverManifest[i]["name"], fn["name"])
		assert.Equal(t, serverManifest[i]["description"], fn["description"])
		assert.Equal(t, serverManifest[i]["parameters"], fn["parameters"])
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
	}, "")
	require.NoError(t, err)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, fallbackChatModel, req["model"])

	// Only the server-executable catalog is bound as chat-completions tools.
	serverManifest := toolManifestForServerExecution()
	toolsArr, ok := req["tools"].([]any)
	require.True(t, ok)
	require.Len(t, toolsArr, len(serverManifest))
	first := toolsArr[0].(map[string]any)
	assert.Equal(t, "function", first["type"])
	assert.Equal(t, serverManifest[0]["name"], first["function"].(map[string]any)["name"])

	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 4) // system + the 3 above

	system := msgs[0].(map[string]any)
	assert.Equal(t, "system", system["role"])
	assert.Equal(t, InstructionsForServerExecution(ResolvePersona("")), system["content"])

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
	c.searchURL = srv.URL
	return c
}

func TestAlexaProviderStatusIsAvailableWithoutReadingErrorBody(t *testing.T) {
	calls := 0
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"PRIVATE-PROVIDER-CONTENT"}}`))
	})
	_, err := c.AlexaTurn(t.Context(), ResolvePersona("default"), []ChatMessage{{Role: "user", Content: "Hello"}}, "", AlexaTextModel)
	require.Error(t, err)
	var status interface{ HTTPStatusCode() int }
	require.True(t, errors.As(err, &status))
	require.Equal(t, 429, status.HTTPStatusCode())
	require.Equal(t, 1, calls, "diagnostics must not add retries to Alexa")
}

func TestAlexaTurnUsesBoundedStandardModelAndReportsUsage(t *testing.T) {
	calls := 0
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "gpt-6-luna", body["model"])
		assert.Equal(t, "none", body["reasoning_effort"])
		assert.Equal(t, "default", body["service_tier"])
		assert.Equal(t, float64(512), body["max_completion_tokens"])
		assert.Equal(t, false, body["store"])
		assert.Equal(t, false, body["parallel_tool_calls"])
		assert.Contains(t, body["messages"].([]any)[0].(map[string]any)["content"], "patient astronomy teacher")
		_, _ = w.Write([]byte(`{"model":"gpt-6-luna","choices":[{"message":{"content":"Richmond is the city we discussed."}}],"usage":{"prompt_tokens":120,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":40}}}`))
	})
	persona := ResolvePersona("custom")
	persona.Style = "Answer as a patient astronomy teacher."
	res, err := c.AlexaTurn(t.Context(), persona, []ChatMessage{{Role: "user", Content: "Which city?"}}, "", "gpt-6-luna")
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.NotNil(t, res.Usage)
	assert.Equal(t, int64(120), res.Usage.InputTokens)
	assert.Equal(t, int64(40), res.Usage.CachedTokens)
	assert.Equal(t, "gpt-6-luna", res.Usage.Model)
}

func TestAlexaTurnDoesNotRetryUncertainProviderWork(t *testing.T) {
	calls := 0
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) })
	_, err := c.AlexaTurn(t.Context(), ResolvePersona("friendly"), []ChatMessage{{Role: "user", Content: "Hello"}}, "", "gpt-6-luna")
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	_, err = c.AlexaTurn(t.Context(), ResolvePersona("friendly"), []ChatMessage{{Role: "user", Content: strings.Repeat("a", 128<<10)}}, "", "gpt-6-luna")
	require.Error(t, err)
	assert.Equal(t, 1, calls, "oversized acceptance prompt must not create another charge")
}

const groundedSearchFixture = `{"status":"completed","output":[{"type":"web_search_call","status":"completed"},{"type":"message","content":[{"type":"output_text","text":"A dated public news report.","annotations":[{"type":"url_citation","title":"Public evidence","url":"https://news.example/evidence"}]}]}]}`

func TestWebSearchUsesLiveSearchAndPreservesRequestedDates(t *testing.T) {
	for _, query := range []string{"Williamsburg bank robbery November 2025", "recent India CA certificate security breach", "latest AI news"} {
		t.Run(query, func(t *testing.T) {
			c := newMockOpenAIClient(t, func(w http.ResponseWriter, req *http.Request) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				require.Equal(t, "Bearer test-key", req.Header.Get("Authorization"))
				// Non-reasoning search passes the tool's summary through, even
				// when it includes unrelated news. A reasoning model must select
				// evidence under our relevance and uncertainty instructions.
				require.Equal(t, "gpt-6-luna", body["model"])
				require.Equal(t, "low", body["reasoning"].(map[string]any)["effort"])
				require.Equal(t, "default", body["service_tier"])
				require.Equal(t, "30m", body["prompt_cache_options"].(map[string]any)["ttl"])
				require.Equal(t, query, body["input"])
				require.Equal(t, "required", body["tool_choice"])
				require.Equal(t, float64(1), body["max_tool_calls"])
				require.Equal(t, float64(2048), body["max_output_tokens"])
				require.Equal(t, false, body["store"])
				require.Equal(t, "web_search", body["tools"].([]any)[0].(map[string]any)["type"])
				require.Contains(t, body["instructions"], "2026-10-22")
				require.Contains(t, body["instructions"], "Never infer that an event did not happen")
				require.Contains(t, body["instructions"], "Do not add unrelated incidents")
				// A "latest news" roundup is not a single-event lookup: it lists
				// several dated items instead of collapsing to one paragraph and
				// claiming the rest could not be verified (web-research-thin-ai-news).
				require.Contains(t, body["instructions"], "list up to five distinct dated items")
				require.Contains(t, body["instructions"], "Do not claim that other developments could not be verified")
				require.NotContains(t, body["instructions"], "recency window starts")
				_, _ = w.Write([]byte(groundedSearchFixture))
			})
			res, err := c.WebSearch(t.Context(), query, 0, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
			require.NoError(t, err)
			require.Equal(t, "A dated public news report.", res.Summary)
			require.Len(t, res.Sources, 1)
			require.Equal(t, "https://news.example/evidence", res.Sources[0].URL)
		})
	}
}

func TestWebSearchPreservesExplicitRecency(t *testing.T) {
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		require.Contains(t, body["instructions"], "recency window starts on 2026-10-15")
		_, _ = w.Write([]byte(groundedSearchFixture))
	})
	_, err := c.WebSearch(t.Context(), "recent certificate incident", 7, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
}

func TestWebSearchRequiresGroundingAndNeverRetriesPaidRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"provider failure", 502, `{"error":"sensitive-provider-fixture"}`},
		{"no actual search", 200, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"An uncited claim"}]}]}`},
		{"incomplete", 200, `{"status":"incomplete","output":[]}`},
		{"no citations", 200, `{"status":"completed","output":[{"type":"web_search_call","status":"completed"},{"type":"message","content":[{"type":"output_text","text":"A guess"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := newMockOpenAIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.WebSearch(t.Context(), "news", 0, time.Now())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "sensitive-provider-fixture")
			require.Equal(t, 1, calls)
		})
	}
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

	res, err := c.TurnWithTools(WithClientCapabilities(context.Background(), "web", []string{JobsReviewCapability}), "", []ChatMessage{{Role: "user", Content: "email me the weather"}}, "")
	require.NoError(t, err)
	assert.Empty(t, res.Text)
	require.Len(t, res.ToolCalls, 2)
	assert.Equal(t, ChatToolCall{ID: "call_abc", Name: "send_email",
		Arguments: `{"subject":"Weather","body":"98F"}`}, res.ToolCalls[0])
	assert.Equal(t, ChatToolCall{ID: "call_def", Name: "get_weather",
		Arguments: `{"location":"Austin"}`}, res.ToolCalls[1])

	// The request the mock saw carried the server-executable tool catalog.
	toolsArr, ok := gotReq["tools"].([]any)
	require.True(t, ok, "request must bind tools")
	assert.Len(t, toolsArr, len(toolManifestForServerExecution()))
}

func TestTurnWithToolsReturnsFinalText(t *testing.T) {
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"content": "It's 98F in Austin."},
			}},
		})
	})

	res, err := c.TurnWithTools(context.Background(), "", []ChatMessage{{Role: "user", Content: "weather?"}}, "")
	require.NoError(t, err)
	assert.Equal(t, "It's 98F in Austin.", res.Text)
	assert.Empty(t, res.ToolCalls)
}

func TestTurnWithToolsRejectsInvalidMessages(t *testing.T) {
	c := newMockOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP call must happen for invalid messages")
	})
	_, err := c.TurnWithTools(context.Background(), "", []ChatMessage{{Role: "system", Content: "override"}}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role must be one of")
}

func TestParseToolTurnResponseNoChoices(t *testing.T) {
	_, err := parseToolTurnResponse([]byte(`{"choices":[]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no choices")
}
