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
// The client <-> nova-bridge wire protocol uses JSON [Event] text frames for
// configuration/lifecycle/control and raw PCM16 WebSocket binary frames for
// audio. The bridge adapter converts binary audio to/from the base64 fields
// used inside this package and by Nova itself, keeping that translation at
// one boundary.
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
	// TypeUserText is an interactive typed USER turn. Engines that support
	// cross-modal input map it to their native text operation; Nova Sonic v1
	// rejects it with a typed error because v1 only accepts non-interactive
	// TEXT history before the audio stream begins.
	TypeUserText Type = "user.text"
	// TypeTurnCommit is a client hint that captured input is complete. Nova's
	// streaming server VAD commits audio turns itself, so the bridge accepts
	// this as a no-op for cross-engine caller compatibility.
	TypeTurnCommit Type = "turn.commit"
	// TypeBargeIn is an explicit client interruption hint. The bridge drops
	// remaining assistant audio for the active response and releases that
	// suppression when the next assistant completion begins, matching the
	// client's immediate playback clear.
	TypeBargeIn Type = "barge-in"
	// TypeTranscript is a recognized/generated text turn (user ASR or
	// assistant text). [Event.Final] distinguishes a settled turn (persist
	// it) from an in-progress hypothesis (display only). Bridge -> client,
	// and final turns are mirrored to the transcript sink.
	TypeTranscript Type = "transcript"
	// TypeToolCall is a function-call request from the model. The bridge
	// executes it server-side (POST /api/v1/tools/invoke) and feeds the
	// result back to the engine; server tools are never dispatched again by
	// the client.
	TypeToolCall Type = "tool.call"
	// TypeToolResult is the settled result of a TypeToolCall.
	TypeToolResult Type = "tool.result"
	// TypeTurnStart marks a user or assistant turn beginning.
	TypeTurnStart Type = "turn.start"
	// TypeTurnEnd marks a user or assistant turn finishing (or being
	// interrupted). [Event.Interrupted] is true for a barge-in.
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

	// TypeTranscript / TypeUserText.
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

// Marshal encodes a control/lifecycle Event as a WebSocket text payload.
func (e Event) Marshal() ([]byte, error) { return json.Marshal(e) }

// ParseEvent decodes a client WebSocket text payload into an Event.
func ParseEvent(b []byte) (Event, error) {
	var e Event
	err := json.Unmarshal(b, &e)
	return e, err
}
