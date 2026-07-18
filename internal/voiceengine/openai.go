package voiceengine

import (
	"encoding/json"
	"strings"
)

// This file maps between the engine-neutral [Event] schema and the OpenAI
// Realtime API's server/client event protocol — the Go counterpart of nova.go.
//
// At runtime the OpenAI path is client-direct (WebRTC/WSS straight to OpenAI),
// so the browser/Android/firmware clients normalize OpenAI events natively in
// JS/Kotlin/C. This Go implementation is the CANONICAL reference for that
// mapping (kept in one authoritative place so the three native clients don't
// drift) and is used by any Go consumer that needs to interpret an OpenAI
// Realtime stream in the neutral schema. Both directions are covered:
// [NormalizeOpenAI] (server event -> neutral) and [ToOpenAIClientEvents]
// (neutral outbound event -> client wire events).
//
// OpenAI Realtime GA tags every event with "type"; transcripts, audio deltas,
// and function-call arguments live in a small, well-known set of fields.
// Field spellings that shifted between the beta and GA event names are handled
// by accepting both aliases.

// OpenAISampleRate is the PCM16 sample rate (Hz) the OpenAI Realtime API uses
// in both directions.
const OpenAISampleRate = 24000

// openAIServerEvent is the subset of an OpenAI Realtime *server* event read
// here.
type openAIServerEvent struct {
	Type       string `json:"type"`
	Transcript string `json:"transcript"`
	Delta      string `json:"delta"`
	Name       string `json:"name"`
	CallID     string `json:"call_id"`
	Arguments  string `json:"arguments"`
	Error      *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// NormalizeOpenAI converts one raw OpenAI Realtime server event into the
// neutral events it yields. Transport/framing events the platform ignores
// (rate_limits.updated, output_item.added, buffer commits, …) yield nil, so a
// caller can range over the result without special-casing. A malformed frame
// yields a single TypeError event (never a Go error), matching
// NovaNormalizer.Push, so one bad frame can't tear down a healthy session.
func NormalizeOpenAI(raw []byte) []Event {
	var e openAIServerEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return []Event{{Type: TypeError, Code: "openai_decode", Message: "malformed openai event"}}
	}
	switch e.Type {
	case "session.created", "session.updated":
		return []Event{{Type: TypeSessionStart}}
	case "input_audio_buffer.speech_started":
		return []Event{{Type: TypeTurnStart, Role: RoleUser}}
	case "input_audio_buffer.speech_stopped":
		return []Event{{Type: TypeTurnEnd, Role: RoleUser}}
	case "conversation.item.input_audio_transcription.delta":
		return []Event{{Type: TypeTranscript, Role: RoleUser, Text: e.Delta}}
	case "conversation.item.input_audio_transcription.completed":
		return []Event{{Type: TypeTranscript, Role: RoleUser, Text: e.Transcript, Final: true}}
	case "response.audio_transcript.delta", "response.output_audio_transcript.delta":
		return []Event{{Type: TypeTranscript, Role: RoleAssistant, Text: e.Delta}}
	case "response.audio_transcript.done", "response.output_audio_transcript.done":
		return []Event{{Type: TypeTranscript, Role: RoleAssistant, Text: e.Transcript, Final: true}}
	case "response.audio.delta", "response.output_audio.delta":
		if e.Delta == "" {
			return nil
		}
		return []Event{{Type: TypeAudioOut, Role: RoleAssistant, Audio: e.Delta, SampleRate: OpenAISampleRate}}
	case "response.created":
		return []Event{{Type: TypeTurnStart, Role: RoleAssistant}}
	case "response.done":
		return []Event{{Type: TypeTurnEnd, Role: RoleAssistant, StopReason: "END_TURN"}}
	case "response.function_call_arguments.done":
		return []Event{{
			Type:       TypeToolCall,
			ToolCallID: e.CallID,
			ToolName:   e.Name,
			ToolArgs:   openAIArgsToRaw(e.Arguments),
		}}
	case "error":
		ev := Event{Type: TypeError}
		if e.Error != nil {
			ev.Code = firstNonEmpty(e.Error.Code, e.Error.Type)
			ev.Message = e.Error.Message
		}
		return []Event{ev}
	default:
		return nil
	}
}

// ToOpenAIClientEvents renders a neutral *outbound* Event as the OpenAI
// Realtime *client* event(s) to write over the data channel / WebSocket. Only
// audio.in and tool.result are outbound-meaningful; a tool.result expands to an
// item.create plus a response.create so the model resumes generation. Any other
// event type yields nil (nothing to send).
func ToOpenAIClientEvents(ev Event) ([][]byte, error) {
	switch ev.Type {
	case TypeAudioIn:
		b, err := json.Marshal(map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": ev.Audio,
		})
		if err != nil {
			return nil, err
		}
		return [][]byte{b}, nil
	case TypeToolResult:
		item, err := json.Marshal(map[string]any{
			"type": "conversation.item.create",
			"item": map[string]any{
				"type":    "function_call_output",
				"call_id": ev.ToolCallID,
				"output":  openAIRawToString(ev.ToolResult),
			},
		})
		if err != nil {
			return nil, err
		}
		resp, err := json.Marshal(map[string]any{"type": "response.create"})
		if err != nil {
			return nil, err
		}
		return [][]byte{item, resp}, nil
	default:
		return nil, nil
	}
}

// openAIArgsToRaw turns OpenAI's stringified-JSON `arguments` field into raw
// JSON object bytes (an empty string becomes an empty object), so downstream
// tool routing always sees a JSON object — matching nova.go's toolArgs.
func openAIArgsToRaw(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// openAIRawToString renders a tool result's raw JSON as the compact string
// OpenAI expects in a function_call_output's `output` field. Empty becomes
// "{}".
func openAIRawToString(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	if s == "" {
		return "{}"
	}
	return s
}
