package voiceengine

import "encoding/json"

// This file maps between the engine-neutral [Event] schema and Amazon Nova
// Sonic's bidirectional streaming protocol (the JSON documents carried in
// each Bedrock InvokeModelWithBidirectionalStream chunk).
//
// Nova's protocol wraps every message as {"event": {"<name>": {...}}}.
// Input (client -> model) events used by the bridge: sessionStart,
// promptStart, contentStart, textInput, audioInput, toolResult, contentEnd,
// promptEnd, sessionEnd. Output (model -> client) events normalized here:
// contentStart, textOutput, audioOutput, toolUse, contentEnd, plus the
// completion lifecycle and usage events (dropped — not user-visible).
//
// Field names follow the published Nova Sonic bidirectional API. The exact
// stopReason spellings and the additionalModelFields shape are the two
// places most likely to drift; both are handled defensively (unknown
// stopReasons still close a turn, absent stage defaults to non-final) and
// are called out for HIL verification against a live session.

// Nova audio format constants (LPCM, mono). Input is 16 kHz, output 24 kHz.
const (
	NovaInputSampleRate  = 16000
	NovaOutputSampleRate = 24000
	NovaSampleSizeBits   = 16
	NovaChannelCount     = 1
)

// Nova content types and roles as they appear on the wire.
const (
	novaTypeText  = "TEXT"
	novaTypeAudio = "AUDIO"
	novaTypeTool  = "TOOL"

	novaRoleUser      = "USER"
	novaRoleAssistant = "ASSISTANT"
	novaRoleSystem    = "SYSTEM"
	novaRoleTool      = "TOOL"
)

// novaEnvelope is the {"event": {...}} wrapper on every Nova message.
type novaEnvelope struct {
	Event map[string]json.RawMessage `json:"event"`
}

// --- input event builders (bridge -> Nova) -------------------------------

func wrap(name string, body any) ([]byte, error) {
	inner, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(novaEnvelope{Event: map[string]json.RawMessage{name: inner}})
}

// NovaSessionStart builds the sessionStart event that opens a Nova session.
func NovaSessionStart(maxTokens int, temperature, topP float64) ([]byte, error) {
	return wrap("sessionStart", map[string]any{
		"inferenceConfiguration": map[string]any{
			"maxTokens":   maxTokens,
			"temperature": temperature,
			"topP":        topP,
		},
	})
}

// NovaPromptStart builds the promptStart event: it declares the output
// audio voice/format, the text output format, and (optionally) the tool
// configuration the model may call. promptName must be a stable id reused
// by every subsequent content event in this prompt.
func NovaPromptStart(promptName, voice string, tools []ToolSpec) ([]byte, error) {
	body := map[string]any{
		"promptName": promptName,
		"textOutputConfiguration": map[string]any{
			"mediaType": "text/plain",
		},
		"audioOutputConfiguration": map[string]any{
			"mediaType":       "audio/lpcm",
			"sampleRateHertz": NovaOutputSampleRate,
			"sampleSizeBits":  NovaSampleSizeBits,
			"channelCount":    NovaChannelCount,
			"voiceId":         voice,
			"encoding":        "base64",
			"audioType":       "SPEECH",
		},
	}
	if len(tools) > 0 {
		body["toolUseOutputConfiguration"] = map[string]any{"mediaType": "application/json"}
		body["toolConfiguration"] = novaToolConfiguration(tools)
	}
	return wrap("promptStart", body)
}

func novaToolConfiguration(tools []ToolSpec) map[string]any {
	specs := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := string(t.InputSchema)
		if schema == "" {
			schema = "{}"
		}
		specs = append(specs, map[string]any{
			"toolSpec": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				// Nova expects the JSON Schema as a stringified document
				// under inputSchema.json.
				"inputSchema": map[string]any{"json": schema},
			},
		})
	}
	return map[string]any{"tools": specs}
}

// NovaTextContent emits the three events for a one-shot text turn (used for
// the SYSTEM prompt): contentStart(TEXT, role), textInput, contentEnd.
func NovaTextContent(promptName, contentName, role, text string) ([][]byte, error) {
	start, err := wrap("contentStart", map[string]any{
		"promptName":  promptName,
		"contentName": contentName,
		"type":        novaTypeText,
		"role":        role,
		"interactive": false,
		"textInputConfiguration": map[string]any{
			"mediaType": "text/plain",
		},
	})
	if err != nil {
		return nil, err
	}
	input, err := wrap("textInput", map[string]any{
		"promptName":  promptName,
		"contentName": contentName,
		"content":     text,
	})
	if err != nil {
		return nil, err
	}
	end, err := NovaContentEnd(promptName, contentName)
	if err != nil {
		return nil, err
	}
	return [][]byte{start, input, end}, nil
}

// NovaAudioContentStart opens the long-lived interactive USER audio content
// that streams microphone frames; Nova's server-side VAD segments turns
// within it, so it stays open for the whole conversation.
func NovaAudioContentStart(promptName, contentName string) ([]byte, error) {
	return wrap("contentStart", map[string]any{
		"promptName":  promptName,
		"contentName": contentName,
		"type":        novaTypeAudio,
		"role":        novaRoleUser,
		"interactive": true,
		"audioInputConfiguration": map[string]any{
			"mediaType":       "audio/lpcm",
			"sampleRateHertz": NovaInputSampleRate,
			"sampleSizeBits":  NovaSampleSizeBits,
			"channelCount":    NovaChannelCount,
			"audioType":       "SPEECH",
			"encoding":        "base64",
		},
	})
}

// NovaAudioInput builds an audioInput event carrying one base64 PCM16 chunk.
func NovaAudioInput(promptName, contentName, audioBase64 string) ([]byte, error) {
	return wrap("audioInput", map[string]any{
		"promptName":  promptName,
		"contentName": contentName,
		"content":     audioBase64,
	})
}

// NovaToolResult emits the three events returning a tool result to the
// model: contentStart(TOOL, role TOOL), toolResult, contentEnd. content is
// the raw JSON result body from POST /api/v1/tools/invoke.
func NovaToolResult(promptName, contentName, toolUseID, content string) ([][]byte, error) {
	start, err := wrap("contentStart", map[string]any{
		"promptName":  promptName,
		"contentName": contentName,
		"type":        novaTypeTool,
		"role":        novaRoleTool,
		"interactive": false,
		"toolResultInputConfiguration": map[string]any{
			"toolUseId": toolUseID,
			"type":      "TEXT",
			"textInputConfiguration": map[string]any{
				"mediaType": "text/plain",
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if content == "" {
		content = "{}"
	}
	result, err := wrap("toolResult", map[string]any{
		"promptName":  promptName,
		"contentName": contentName,
		"content":     content,
	})
	if err != nil {
		return nil, err
	}
	end, err := NovaContentEnd(promptName, contentName)
	if err != nil {
		return nil, err
	}
	return [][]byte{start, result, end}, nil
}

// NovaContentEnd closes a content block.
func NovaContentEnd(promptName, contentName string) ([]byte, error) {
	return wrap("contentEnd", map[string]any{
		"promptName":  promptName,
		"contentName": contentName,
	})
}

// NovaPromptEnd closes the prompt.
func NovaPromptEnd(promptName string) ([]byte, error) {
	return wrap("promptEnd", map[string]any{"promptName": promptName})
}

// NovaSessionEnd closes the session.
func NovaSessionEnd() ([]byte, error) {
	return wrap("sessionEnd", map[string]any{})
}

// --- output normalization (Nova -> Event) --------------------------------

// contentMeta tracks the role/stage/type declared by a Nova contentStart so
// the textOutput/audioOutput/contentEnd events that follow (which may omit
// those attributes) can be attributed correctly.
type contentMeta struct {
	typ   string
	role  string
	final bool
}

// NovaNormalizer converts the stream of Nova output events into
// engine-neutral [Event]s. It is stateful (it remembers the currently open
// content blocks) and is NOT safe for concurrent use — drive it from the
// single goroutine reading the Bedrock stream.
type NovaNormalizer struct {
	// byName remembers metadata per contentName; lastName is the fallback
	// for output events that omit contentName.
	byName   map[string]contentMeta
	lastName string
}

// NewNovaNormalizer returns a ready normalizer.
func NewNovaNormalizer() *NovaNormalizer {
	return &NovaNormalizer{byName: make(map[string]contentMeta)}
}

// nova output event payloads (only the fields we consume).
type novaContentStart struct {
	ContentName           string `json:"contentName"`
	Type                  string `json:"type"`
	Role                  string `json:"role"`
	AdditionalModelFields string `json:"additionalModelFields"`
}

type novaTextOutput struct {
	ContentName string `json:"contentName"`
	Content     string `json:"content"`
	Role        string `json:"role"`
}

type novaAudioOutput struct {
	ContentName string `json:"contentName"`
	Content     string `json:"content"`
}

type novaToolUse struct {
	ContentName string          `json:"contentName"`
	ToolUseId   string          `json:"toolUseId"`
	ToolName    string          `json:"toolName"`
	Content     json.RawMessage `json:"content"`
}

type novaContentEnd struct {
	ContentName string `json:"contentName"`
	Type        string `json:"type"`
	StopReason  string `json:"stopReason"`
}

// Push decodes one Nova output document and returns the neutral events it
// yields (possibly none — lifecycle/usage events are dropped). A decode
// error is returned as a single TypeError event, never as a Go error, so
// one malformed frame cannot tear down an otherwise-healthy session.
func (n *NovaNormalizer) Push(raw []byte) []Event {
	var env novaEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Event) == 0 {
		return []Event{{Type: TypeError, Code: "nova_decode", Message: "malformed nova event"}}
	}
	for name, body := range env.Event {
		switch name {
		case "contentStart":
			return n.onContentStart(body)
		case "textOutput":
			return n.onTextOutput(body)
		case "audioOutput":
			return n.onAudioOutput(body)
		case "toolUse":
			return n.onToolUse(body)
		case "contentEnd":
			return n.onContentEnd(body)
		default:
			// completionStart, completionEnd, usageEvent, and any future
			// lifecycle events carry nothing the neutral schema needs.
			return nil
		}
	}
	return nil
}

func (n *NovaNormalizer) onContentStart(body json.RawMessage) []Event {
	var cs novaContentStart
	if err := json.Unmarshal(body, &cs); err != nil {
		return nil
	}
	final := false
	if cs.AdditionalModelFields != "" {
		var amf struct {
			GenerationStage string `json:"generationStage"`
		}
		if json.Unmarshal([]byte(cs.AdditionalModelFields), &amf) == nil {
			final = amf.GenerationStage == "FINAL"
		}
	}
	if cs.ContentName != "" {
		n.byName[cs.ContentName] = contentMeta{typ: cs.Type, role: cs.Role, final: final}
		n.lastName = cs.ContentName
	}
	// An assistant content block beginning is the start of a response turn.
	if roleToNeutral(cs.Role) == RoleAssistant {
		return []Event{{Type: TypeTurnStart, Role: RoleAssistant}}
	}
	return nil
}

func (n *NovaNormalizer) onTextOutput(body json.RawMessage) []Event {
	var to novaTextOutput
	if err := json.Unmarshal(body, &to); err != nil {
		return nil
	}
	meta := n.meta(to.ContentName)
	role := roleToNeutral(firstNonEmpty(to.Role, meta.role))
	if role == "" {
		role = RoleAssistant
	}
	return []Event{{
		Type:  TypeTranscript,
		Role:  role,
		Text:  to.Content,
		Final: meta.final,
	}}
}

func (n *NovaNormalizer) onAudioOutput(body json.RawMessage) []Event {
	var ao novaAudioOutput
	if err := json.Unmarshal(body, &ao); err != nil || ao.Content == "" {
		return nil
	}
	return []Event{{
		Type:       TypeAudioOut,
		Audio:      ao.Content,
		SampleRate: NovaOutputSampleRate,
	}}
}

func (n *NovaNormalizer) onToolUse(body json.RawMessage) []Event {
	var tu novaToolUse
	if err := json.Unmarshal(body, &tu); err != nil || tu.ToolName == "" {
		return nil
	}
	// Nova sends the arguments as a JSON value; normalize whatever shape
	// (object or stringified object) into raw JSON args.
	args := toolArgs(tu.Content)
	return []Event{{
		Type:       TypeToolCall,
		ToolCallID: tu.ToolUseId,
		ToolName:   tu.ToolName,
		ToolArgs:   args,
	}}
}

func (n *NovaNormalizer) onContentEnd(body json.RawMessage) []Event {
	var ce novaContentEnd
	if err := json.Unmarshal(body, &ce); err != nil {
		return nil
	}
	meta := n.meta(ce.ContentName)
	if ce.ContentName != "" {
		delete(n.byName, ce.ContentName)
	}
	// Only assistant content ending is a turn boundary the client cares
	// about; ending the user's ASR text block or a tool block is internal.
	typ := firstNonEmpty(ce.Type, meta.typ)
	if roleToNeutral(meta.role) != RoleAssistant && typ != novaTypeAudio {
		return nil
	}
	if ce.StopReason == "" {
		return nil
	}
	return []Event{{
		Type:        TypeTurnEnd,
		StopReason:  ce.StopReason,
		Interrupted: ce.StopReason == "INTERRUPTED",
	}}
}

func (n *NovaNormalizer) meta(contentName string) contentMeta {
	if contentName == "" {
		contentName = n.lastName
	}
	return n.byName[contentName]
}

// toolArgs normalizes Nova's toolUse content into a JSON object. Nova may
// deliver the args either as a JSON object or as a JSON string containing
// an object; unwrap the string form so downstream tool routing always sees
// an object.
func toolArgs(raw json.RawMessage) json.RawMessage {
	trimmed := trimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage("{}")
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			if s == "" {
				return json.RawMessage("{}")
			}
			return json.RawMessage(s)
		}
	}
	return trimmed
}

func roleToNeutral(role string) string {
	switch role {
	case novaRoleUser:
		return RoleUser
	case novaRoleAssistant:
		return RoleAssistant
	case novaRoleSystem:
		return RoleSystem
	default:
		return ""
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// String returns the raw event-type discriminator.
func (t Type) String() string { return string(t) }
