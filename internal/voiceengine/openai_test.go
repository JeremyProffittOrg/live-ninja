package voiceengine

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestNormalizeOpenAI_ServerEvents exercises the server-event -> neutral
// direction across every recognized OpenAI Realtime event type.
func TestNormalizeOpenAI_ServerEvents(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Event
	}{
		{
			name: "session created",
			raw:  `{"type":"session.created"}`,
			want: Event{Type: TypeSessionStart},
		},
		{
			name: "user speech started -> user turn start",
			raw:  `{"type":"input_audio_buffer.speech_started"}`,
			want: Event{Type: TypeTurnStart, Role: RoleUser},
		},
		{
			name: "user speech stopped -> user turn end",
			raw:  `{"type":"input_audio_buffer.speech_stopped"}`,
			want: Event{Type: TypeTurnEnd, Role: RoleUser},
		},
		{
			name: "user transcript delta",
			raw:  `{"type":"conversation.item.input_audio_transcription.delta","delta":"hel"}`,
			want: Event{Type: TypeTranscript, Role: RoleUser, Text: "hel"},
		},
		{
			name: "user transcript completed is final",
			raw:  `{"type":"conversation.item.input_audio_transcription.completed","transcript":"hello there"}`,
			want: Event{Type: TypeTranscript, Role: RoleUser, Text: "hello there", Final: true},
		},
		{
			name: "assistant transcript delta",
			raw:  `{"type":"response.audio_transcript.delta","delta":"hi"}`,
			want: Event{Type: TypeTranscript, Role: RoleAssistant, Text: "hi"},
		},
		{
			name: "assistant transcript done is final (GA alias)",
			raw:  `{"type":"response.output_audio_transcript.done","transcript":"hi, how can I help?"}`,
			want: Event{Type: TypeTranscript, Role: RoleAssistant, Text: "hi, how can I help?", Final: true},
		},
		{
			name: "assistant audio delta (GA alias)",
			raw:  `{"type":"response.output_audio.delta","delta":"QUJD"}`,
			want: Event{Type: TypeAudioOut, Role: RoleAssistant, Audio: "QUJD", SampleRate: OpenAISampleRate},
		},
		{
			name: "response created -> assistant turn start",
			raw:  `{"type":"response.created"}`,
			want: Event{Type: TypeTurnStart, Role: RoleAssistant},
		},
		{
			name: "response done -> assistant turn end",
			raw:  `{"type":"response.done"}`,
			want: Event{Type: TypeTurnEnd, Role: RoleAssistant, StopReason: "END_TURN"},
		},
		{
			name: "error carries code and message",
			raw:  `{"type":"error","error":{"type":"invalid_request_error","code":"rate_limit_exceeded","message":"slow down"}}`,
			want: Event{Type: TypeError, Code: "rate_limit_exceeded", Message: "slow down"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeOpenAI([]byte(tc.raw))
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1: %+v", len(got), got)
			}
			if !reflect.DeepEqual(got[0], tc.want) {
				t.Fatalf("normalized event mismatch\n got: %+v\nwant: %+v", got[0], tc.want)
			}
		})
	}
}

func TestNormalizeOpenAI_ToolCall(t *testing.T) {
	raw := `{"type":"response.function_call_arguments.done","call_id":"call_42","name":"get_weather","arguments":"{\"location\":\"Raleigh\"}"}`
	got := NormalizeOpenAI([]byte(raw))
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Type != TypeToolCall || ev.ToolCallID != "call_42" || ev.ToolName != "get_weather" {
		t.Fatalf("tool call fields wrong: %+v", ev)
	}
	// Arguments must be raw JSON object bytes, not a JSON string.
	var args map[string]string
	if err := json.Unmarshal(ev.ToolArgs, &args); err != nil {
		t.Fatalf("tool args not a JSON object: %s (%v)", ev.ToolArgs, err)
	}
	if args["location"] != "Raleigh" {
		t.Fatalf("tool args location = %q, want Raleigh", args["location"])
	}
}

func TestNormalizeOpenAI_UnknownDropped(t *testing.T) {
	for _, raw := range []string{
		`{"type":"rate_limits.updated"}`,
		`{"type":"response.output_item.added"}`,
		`{"type":"input_audio_buffer.committed"}`,
	} {
		if got := NormalizeOpenAI([]byte(raw)); got != nil {
			t.Fatalf("expected nil for %s, got %+v", raw, got)
		}
	}
}

func TestNormalizeOpenAI_MalformedYieldsError(t *testing.T) {
	got := NormalizeOpenAI([]byte(`{not json`))
	if len(got) != 1 || got[0].Type != TypeError || got[0].Code != "openai_decode" {
		t.Fatalf("malformed frame handling wrong: %+v", got)
	}
}

// TestToOpenAIClientEvents_AudioIn covers the neutral -> client direction for
// microphone audio.
func TestToOpenAIClientEvents_AudioIn(t *testing.T) {
	out, err := ToOpenAIClientEvents(Event{Type: TypeAudioIn, Audio: "QUJD"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	var m map[string]any
	if err := json.Unmarshal(out[0], &m); err != nil {
		t.Fatalf("output not valid json: %v", err)
	}
	if m["type"] != "input_audio_buffer.append" || m["audio"] != "QUJD" {
		t.Fatalf("audio append event wrong: %v", m)
	}
}

// TestToOpenAIClientEvents_ToolResult covers the neutral -> client direction
// for a tool result: an item.create carrying the function_call_output plus a
// response.create to resume generation.
func TestToOpenAIClientEvents_ToolResult(t *testing.T) {
	out, err := ToOpenAIClientEvents(Event{
		Type:       TypeToolResult,
		ToolCallID: "call_42",
		ToolResult: json.RawMessage(`{"tempF":81}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d events, want 2 (item.create + response.create)", len(out))
	}

	var item struct {
		Type string `json:"type"`
		Item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		} `json:"item"`
	}
	if err := json.Unmarshal(out[0], &item); err != nil {
		t.Fatalf("item.create not valid json: %v", err)
	}
	if item.Type != "conversation.item.create" || item.Item.Type != "function_call_output" {
		t.Fatalf("item.create shape wrong: %+v", item)
	}
	if item.Item.CallID != "call_42" || item.Item.Output != `{"tempF":81}` {
		t.Fatalf("item.create call_id/output wrong: %+v", item)
	}

	var resp map[string]any
	if err := json.Unmarshal(out[1], &resp); err != nil {
		t.Fatalf("response.create not valid json: %v", err)
	}
	if resp["type"] != "response.create" {
		t.Fatalf("second event should be response.create, got %v", resp)
	}
}

// TestOpenAIToolRoundTrip proves a tool call normalized from a server event can
// be answered with a client tool-result event carrying the same call id — the
// end-to-end "both directions" contract for a function call.
func TestOpenAIToolRoundTrip(t *testing.T) {
	call := NormalizeOpenAI([]byte(
		`{"type":"response.function_call_arguments.done","call_id":"c1","name":"set_timer","arguments":"{\"seconds\":60}"}`))
	if len(call) != 1 || call[0].Type != TypeToolCall {
		t.Fatalf("expected a tool call, got %+v", call)
	}
	result := Event{Type: TypeToolResult, ToolCallID: call[0].ToolCallID, ToolResult: json.RawMessage(`{"ok":true}`)}
	out, err := ToOpenAIClientEvents(result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected item.create + response.create, got %d", len(out))
	}
	var item struct {
		Item struct {
			CallID string `json:"call_id"`
		} `json:"item"`
	}
	_ = json.Unmarshal(out[0], &item)
	if item.Item.CallID != "c1" {
		t.Fatalf("round-trip call id mismatch: %q", item.Item.CallID)
	}
}
