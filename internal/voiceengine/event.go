// Package voiceengine defines the engine-agnostic session/tool/transcript
// event schema that every Live Ninja voice engine normalizes to (FR-VE-01),
// so topics, memory, and tool routing work identically no matter which
// engine produced a turn. Two engines exist:
//
//   - openai-realtime / openai-realtime-mini — client-direct WebRTC; the
//     browser/Android/firmware client normalizes OpenAI Realtime server
//     events into this schema locally (its own JS/Kotlin/C mapping).
//   - nova-sonic — Amazon Nova Sonic on Bedrock, reached through the
//     backend media bridge (cmd/nova-bridge). The bridge is the Go
//     consumer of this package: it translates the client's schema events
//     into Nova Sonic's bidirectional protocol and normalizes Nova's
//     output events back into this schema before forwarding them on and
//     emitting transcript turns / tool calls.
//
// The client <-> nova-bridge wire protocol IS this schema, one JSON
// [Event] per WebSocket text frame (audio carried base64 in-band, matching
// how Nova itself frames audio). That keeps a single representation across
// the whole path and makes the bridge's translation layer the only place
// Nova's protocol details live.
package voiceengine

import "encoding/json"

// Type is the discriminator on every [Event]. The set is deliberately
// small and engine-neutral; anything an engine expresses that does not map
// cleanly is dropped or folded into the nearest neighbour (documented at
// each normalizer) rather than leaking engine-specific types upward.
type Type string

const (
	// TypeSessionStart opens a session. Client -> bridge it carries the
	// session configuration (voice, sample rates, tools, system prompt);
	// the bridge uses it to build Nova's sessionStart/promptStart. It is
	// not forwarded back to the client.
	TypeSessionStart Type = "session.start"
	// TypeAudioIn is a chunk of captured microphone audio, base64 PCM16
	// mono at [Event.SampleRate] Hz (default 16 kHz for Nova input).
	// Client -> bridge only.
	TypeAudioIn Type = "audio.in"
	// TypeAudioOut is a chunk of synthesized assistant audio, base64 PCM16
	// mono at [Event.SampleRate] Hz (24 kHz for Nova output). Bridge ->
	// client only.
	TypeAudioOut Type = "audio.out"
	// TypeTranscript is a recognized/generated text turn (user ASR or
	// assistant text). [Event.Final] distinguishes a settled turn (persist
	// it) from an in-progress hypothesis (display only). Bridge -> client,
	// and final turns are mirrored to the transcript sink.
	TypeTranscript Type = "transcript"
	// TypeToolCall is a function-call request from the model. The bridge
	// executes it server-side (POST /api/v1/tools/invoke) and both forwards
	// this to the client (for UI) and feeds the result back to the engine.
	TypeToolCall Type = "tool.call"
	// TypeToolResult is the settled result of a TypeToolCall, echoed to the
	// client after the bridge has fed it back to the engine.
	TypeToolResult Type = "tool.result"
	// TypeTurnStart marks the assistant beginning a response turn.
	TypeTurnStart Type = "turn.start"
	// TypeTurnEnd marks the assistant finishing (or being interrupted on)
	// a response turn. [Event.Interrupted] is true for a barge-in.
	TypeTurnEnd Type = "turn.end"
	// TypeError is a terminal or non-terminal engine/bridge error.
	TypeError Type = "error"
)

// Role values for transcript turns and tool ownership.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// ToolSpec is one function the model may call, in the engine-neutral shape
// the client passes at session start. The bridge rewrites it into Nova's
// toolConfiguration; a client normalizing OpenAI events uses the same shape
// for its own tool wiring. InputSchema is a JSON Schema object.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Config is the per-session bootstrap carried on a TypeSessionStart event.
// Every field has a safe default (see the bridge) so an empty session.start
// still yields a working Nova session.
type Config struct {
	// Voice is the engine voice id. For Nova Sonic this is a Nova voice
	// (e.g. "matthew", "tiffany", "amy") — NOT an OpenAI voice name; voice
	// resolution/mapping is the session broker's concern, the bridge takes
	// whatever id it is handed.
	Voice string `json:"voice,omitempty"`
	// SampleRateIn/Out are the PCM16 sample rates in Hz for captured and
	// synthesized audio (default 16000 / 24000 for Nova).
	SampleRateIn  int `json:"sampleRateIn,omitempty"`
	SampleRateOut int `json:"sampleRateOut,omitempty"`
	// SystemPrompt, when set, is sent to the engine as a SYSTEM turn before
	// audio begins (persona + guide instructions resolved upstream).
	SystemPrompt string `json:"systemPrompt,omitempty"`
	// Tools the model may call this session.
	Tools []ToolSpec `json:"tools,omitempty"`
}

// Event is the single wire/normalization unit. Only the fields relevant to
// [Event.Type] are populated; the rest stay zero and omitempty keeps frames
// compact.
type Event struct {
	Type Type `json:"type"`

	// TypeSessionStart.
	Config *Config `json:"config,omitempty"`

	// TypeAudioIn / TypeAudioOut: base64 PCM16 mono at SampleRate Hz.
	Audio      string `json:"audio,omitempty"`
	SampleRate int    `json:"sampleRate,omitempty"`

	// TypeTranscript.
	Role  string `json:"role,omitempty"`
	Text  string `json:"text,omitempty"`
	Final bool   `json:"final,omitempty"`

	// TypeToolCall / TypeToolResult.
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	ToolArgs   json.RawMessage `json:"toolArgs,omitempty"`
	ToolResult json.RawMessage `json:"toolResult,omitempty"`

	// TypeTurnEnd.
	Interrupted bool   `json:"interrupted,omitempty"`
	StopReason  string `json:"stopReason,omitempty"`

	// TypeError.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Marshal encodes an Event as a WebSocket text-frame payload.
func (e Event) Marshal() ([]byte, error) { return json.Marshal(e) }

// ParseEvent decodes a client WebSocket text-frame payload into an Event.
func ParseEvent(b []byte) (Event, error) {
	var e Event
	err := json.Unmarshal(b, &e)
	return e, err
}
