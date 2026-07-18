package voiceengine

import (
	"encoding/json"
	"testing"
)

func doc(t *testing.T, name string, body map[string]any) []byte {
	t.Helper()
	inner, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(map[string]any{"event": map[string]json.RawMessage{name: inner}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNormalizer_TranscriptFinalAndRole(t *testing.T) {
	n := NewNovaNormalizer()
	n.Push(doc(t, "contentStart", map[string]any{
		"contentName": "u1", "type": "TEXT", "role": "USER",
		"additionalModelFields": `{"generationStage":"FINAL"}`,
	}))
	evs := n.Push(doc(t, "textOutput", map[string]any{"contentName": "u1", "content": "hi there", "role": "USER"}))
	if len(evs) != 1 || evs[0].Type != TypeTranscript {
		t.Fatalf("want one transcript event, got %+v", evs)
	}
	if evs[0].Role != RoleUser || evs[0].Text != "hi there" || !evs[0].Final {
		t.Fatalf("bad transcript normalization: %+v", evs[0])
	}
}

func TestNormalizer_SpeculativeNotFinal(t *testing.T) {
	n := NewNovaNormalizer()
	n.Push(doc(t, "contentStart", map[string]any{
		"contentName": "a1", "type": "TEXT", "role": "ASSISTANT",
		"additionalModelFields": `{"generationStage":"SPECULATIVE"}`,
	}))
	evs := n.Push(doc(t, "textOutput", map[string]any{"contentName": "a1", "content": "maybe", "role": "ASSISTANT"}))
	if len(evs) != 1 || evs[0].Final {
		t.Fatalf("speculative text should not be final: %+v", evs)
	}
}

func TestNormalizer_AssistantContentStartYieldsTurnStart(t *testing.T) {
	n := NewNovaNormalizer()
	evs := n.Push(doc(t, "contentStart", map[string]any{"contentName": "a1", "type": "AUDIO", "role": "ASSISTANT"}))
	if len(evs) != 1 || evs[0].Type != TypeTurnStart {
		t.Fatalf("want turn.start, got %+v", evs)
	}
}

func TestNormalizer_AudioOutput(t *testing.T) {
	n := NewNovaNormalizer()
	evs := n.Push(doc(t, "audioOutput", map[string]any{"contentName": "a2", "content": "QUJD"}))
	if len(evs) != 1 || evs[0].Type != TypeAudioOut || evs[0].Audio != "QUJD" {
		t.Fatalf("bad audio.out: %+v", evs)
	}
	if evs[0].SampleRate != NovaOutputSampleRate {
		t.Fatalf("audio.out sample rate = %d, want %d", evs[0].SampleRate, NovaOutputSampleRate)
	}
}

func TestNormalizer_BargeInInterrupted(t *testing.T) {
	n := NewNovaNormalizer()
	n.Push(doc(t, "contentStart", map[string]any{"contentName": "a2", "type": "AUDIO", "role": "ASSISTANT"}))
	evs := n.Push(doc(t, "contentEnd", map[string]any{"contentName": "a2", "type": "AUDIO", "stopReason": "INTERRUPTED"}))
	if len(evs) != 1 || evs[0].Type != TypeTurnEnd || !evs[0].Interrupted {
		t.Fatalf("barge-in should be interrupted turn.end: %+v", evs)
	}
}

func TestNormalizer_EndTurnNotInterrupted(t *testing.T) {
	n := NewNovaNormalizer()
	n.Push(doc(t, "contentStart", map[string]any{"contentName": "a2", "type": "AUDIO", "role": "ASSISTANT"}))
	evs := n.Push(doc(t, "contentEnd", map[string]any{"contentName": "a2", "type": "AUDIO", "stopReason": "END_TURN"}))
	if len(evs) != 1 || evs[0].Type != TypeTurnEnd || evs[0].Interrupted {
		t.Fatalf("END_TURN should be a non-interrupted turn.end: %+v", evs)
	}
}

func TestNormalizer_ToolUseStringArgsUnwrapped(t *testing.T) {
	n := NewNovaNormalizer()
	// Nova may deliver args as a stringified JSON object.
	evs := n.Push(doc(t, "toolUse", map[string]any{
		"toolUseId": "c1", "toolName": "get_weather", "content": `{"city":"Denver"}`,
	}))
	if len(evs) != 1 || evs[0].Type != TypeToolCall {
		t.Fatalf("want tool.call, got %+v", evs)
	}
	if evs[0].ToolName != "get_weather" || evs[0].ToolCallID != "c1" {
		t.Fatalf("bad tool.call fields: %+v", evs[0])
	}
	var args map[string]string
	if err := json.Unmarshal(evs[0].ToolArgs, &args); err != nil {
		t.Fatalf("tool args not valid JSON object: %q (%v)", evs[0].ToolArgs, err)
	}
	if args["city"] != "Denver" {
		t.Fatalf("tool args = %v, want city=Denver", args)
	}
}

func TestNormalizer_ToolUseObjectArgs(t *testing.T) {
	n := NewNovaNormalizer()
	evs := n.Push(doc(t, "toolUse", map[string]any{
		"toolUseId": "c2", "toolName": "get_weather", "content": map[string]any{"city": "Boulder"},
	}))
	if len(evs) != 1 || evs[0].Type != TypeToolCall {
		t.Fatalf("want tool.call, got %+v", evs)
	}
	var args map[string]string
	if err := json.Unmarshal(evs[0].ToolArgs, &args); err != nil || args["city"] != "Boulder" {
		t.Fatalf("object args not passed through: %q", evs[0].ToolArgs)
	}
}

func TestNormalizer_MalformedYieldsError(t *testing.T) {
	n := NewNovaNormalizer()
	evs := n.Push([]byte("not json"))
	if len(evs) != 1 || evs[0].Type != TypeError {
		t.Fatalf("malformed input should yield one error event, got %+v", evs)
	}
}

func TestNormalizer_LifecycleEventsDropped(t *testing.T) {
	n := NewNovaNormalizer()
	for _, name := range []string{"completionStart", "completionEnd", "usageEvent"} {
		if evs := n.Push(doc(t, name, map[string]any{})); len(evs) != 0 {
			t.Errorf("%s should be dropped, got %+v", name, evs)
		}
	}
}

func TestNovaInputBuilders_WellFormed(t *testing.T) {
	// Every builder must emit a single {"event":{<name>:{...}}} document.
	check := func(name string, b []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s builder error: %v", name, err)
		}
		var env struct {
			Event map[string]json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("%s not valid JSON: %v", name, err)
		}
		if _, ok := env.Event[name]; !ok || len(env.Event) != 1 {
			t.Fatalf("%s wrong envelope: %s", name, b)
		}
	}

	ss, err := NovaSessionStart(1024, 0.7, 0.9)
	check("sessionStart", ss, err)

	ps, err := NovaPromptStart("p1", "matthew", []ToolSpec{{Name: "t", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	check("promptStart", ps, err)

	acs, err := NovaAudioContentStart("p1", "c1")
	check("contentStart", acs, err)

	ai, err := NovaAudioInput("p1", "c1", "QUJD")
	check("audioInput", ai, err)

	ce, err := NovaContentEnd("p1", "c1")
	check("contentEnd", ce, err)

	pe, err := NovaPromptEnd("p1")
	check("promptEnd", pe, err)

	se, err := NovaSessionEnd()
	check("sessionEnd", se, err)
}

func TestNovaToolResult_ThreeEvents(t *testing.T) {
	docs, err := NovaToolResult("p1", "tr1", "call-1", `{"ok":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("want contentStart/toolResult/contentEnd, got %d docs", len(docs))
	}
	names := []string{"contentStart", "toolResult", "contentEnd"}
	for i, want := range names {
		var env struct {
			Event map[string]json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(docs[i], &env); err != nil {
			t.Fatal(err)
		}
		if _, ok := env.Event[want]; !ok {
			t.Fatalf("doc %d = %s, want %s", i, docs[i], want)
		}
	}
}

func TestEventRoundTrip(t *testing.T) {
	in := Event{Type: TypeAudioIn, Audio: "QUJD", SampleRate: 16000}
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseEvent(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != TypeAudioIn || out.Audio != "QUJD" || out.SampleRate != 16000 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}
