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
// completionStart, contentStart, textOutput, audioOutput, toolUse, contentEnd,
// completionEnd, plus usage events (dropped — not user-visible).
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

// contentMeta tracks the role/stage/completion declared by contentStart so
// the output/contentEnd events that follow can be attributed correctly.
type contentMeta struct {
	role         string
	final        bool
	completionID string
}

// completionMeta collapses Nova's several assistant content blocks
// (speculative text, audio, and final text) into one engine-neutral turn.
type completionMeta struct {
	assistantStarted bool
	assistantEnded   bool
}

// NovaNormalizer converts the stream of Nova output events into
// engine-neutral [Event]s. It is stateful (it remembers the currently open
// content blocks) and is NOT safe for concurrent use — drive it from the
// single goroutine reading the Bedrock stream.
type NovaNormalizer struct {
	// byName remembers metadata per output contentId; lastName is the fallback
	// for output events that omit contentId. Nova input events use contentName,
	// but the model's output contract deliberately uses contentId.
	byName            map[string]contentMeta
	lastName          string
	completions       map[string]*completionMeta
	currentCompletion string
}

// NewNovaNormalizer returns a ready normalizer.
func NewNovaNormalizer() *NovaNormalizer {
	return &NovaNormalizer{
		byName:      make(map[string]contentMeta),
		completions: make(map[string]*completionMeta),
	}
}

// nova output event payloads (only the fields we consume).
type novaCompletionStart struct {
	CompletionID string `json:"completionId"`
}

type novaContentStart struct {
	ContentID             string `json:"contentId"`
	CompletionID          string `json:"completionId"`
	Type                  string `json:"type"`
	Role                  string `json:"role"`
	AdditionalModelFields string `json:"additionalModelFields"`
}

type novaTextOutput struct {
	ContentID string `json:"contentId"`
	Content   string `json:"content"`
	Role      string `json:"role"`
}

type novaAudioOutput struct {
	ContentID string `json:"contentId"`
	Content   string `json:"content"`
}

type novaToolUse struct {
	ContentID string          `json:"contentId"`
	ToolUseId string          `json:"toolUseId"`
	ToolName  string          `json:"toolName"`
	Content   json.RawMessage `json:"content"`
}

type novaContentEnd struct {
	ContentID    string `json:"contentId"`
	CompletionID string `json:"completionId"`
	Type         string `json:"type"`
	StopReason   string `json:"stopReason"`
}

type novaCompletionEnd struct {
	CompletionID string `json:"completionId"`
	StopReason   string `json:"stopReason"`
}

// Push decodes one Nova output document and returns the neutral events it
// yields (possibly none — usage and internal lifecycle events are dropped). A decode
// error is returned as a single TypeError event, never as a Go error, so
// one malformed frame cannot tear down an otherwise-healthy session.
func (n *NovaNormalizer) Push(raw []byte) []Event {
	var env novaEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Event) == 0 {
		return []Event{{Type: TypeError, Code: "nova_decode", Message: "malformed nova event"}}
	}
	for name, body := range env.Event {
		switch name {
		case "completionStart":
			return n.onCompletionStart(body)
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
		case "completionEnd":
			return n.onCompletionEnd(body)
		default:
			// usageEvent and future internal events carry nothing the neutral
			// lifecycle schema currently exposes.
			return nil
		}
	}
	return nil
}

func (n *NovaNormalizer) onCompletionStart(body json.RawMessage) []Event {
	var cs novaCompletionStart
	if err := json.Unmarshal(body, &cs); err != nil || cs.CompletionID == "" {
		return nil
	}
	n.currentCompletion = cs.CompletionID
	n.completions[cs.CompletionID] = &completionMeta{}
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
	completionID := firstNonEmpty(cs.CompletionID, n.currentCompletion)
	if completionID != "" {
		n.currentCompletion = completionID
		if n.completions[completionID] == nil {
			// Be tolerant of a lost completionStart frame while retaining
			// completion-level lifecycle semantics for the remaining blocks.
			n.completions[completionID] = &completionMeta{}
		}
	}
	if cs.ContentID != "" {
		n.byName[cs.ContentID] = contentMeta{
			role: cs.Role, final: final, completionID: completionID,
		}
		n.lastName = cs.ContentID
	}
	role := roleToNeutral(cs.Role)
	if role == RoleUser {
		return []Event{{Type: TypeTurnStart, Role: role}}
	}
	if role == RoleAssistant {
		if completionID == "" {
			// The published contract always supplies completionId. Preserve a
			// useful boundary if an older/malformed producer omits it.
			return []Event{{Type: TypeTurnStart, Role: role}}
		}
		state := n.completions[completionID]
		if !state.assistantStarted {
			state.assistantStarted = true
			return []Event{{Type: TypeTurnStart, Role: role}}
		}
	}
	return nil
}

func (n *NovaNormalizer) onTextOutput(body json.RawMessage) []Event {
	var to novaTextOutput
	if err := json.Unmarshal(body, &to); err != nil {
		return nil
	}
	meta := n.meta(to.ContentID)
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
	meta := n.meta(ce.ContentID)
	completionID := firstNonEmpty(ce.CompletionID, meta.completionID, n.currentCompletion)
	if ce.ContentID != "" {
		delete(n.byName, ce.ContentID)
	}
	role := roleToNeutral(meta.role)
	if role == RoleUser {
		if ce.StopReason == "" {
			return nil
		}
		return []Event{{
			Type: TypeTurnEnd, Role: role, StopReason: ce.StopReason,
			Interrupted: ce.StopReason == "INTERRUPTED",
		}}
	}
	if role != RoleAssistant {
		return nil
	}

	// The assistant's speculative TEXT, AUDIO, and final TEXT blocks all end
	// independently inside one completion. Only an interruption is surfaced
	// immediately so clients can purge faster-than-real-time queued audio;
	// normal completionEnd owns the single settled turn boundary.
	if ce.StopReason == "INTERRUPTED" {
		if state := n.completions[completionID]; state != nil {
			if state.assistantEnded {
				return nil
			}
			state.assistantEnded = true
		}
		return []Event{{
			Type: TypeTurnEnd, Role: RoleAssistant, StopReason: ce.StopReason,
			Interrupted: true,
		}}
	}
	if completionID != "" {
		return nil
	}
	// Defensive fallback for an output producer that omits completionId.
	if ce.StopReason == "" {
		return nil
	}
	return []Event{{
		Type:       TypeTurnEnd,
		Role:       RoleAssistant,
		StopReason: ce.StopReason,
	}}
}

func (n *NovaNormalizer) onCompletionEnd(body json.RawMessage) []Event {
	var ce novaCompletionEnd
	if err := json.Unmarshal(body, &ce); err != nil {
		return nil
	}
	completionID := firstNonEmpty(ce.CompletionID, n.currentCompletion)
	state := n.completions[completionID]
	if completionID != "" {
		delete(n.completions, completionID)
		if n.currentCompletion == completionID {
			n.currentCompletion = ""
		}
	}
	if state == nil || !state.assistantStarted || state.assistantEnded {
		return nil
	}
	state.assistantEnded = true
	return []Event{{
		Type:        TypeTurnEnd,
		Role:        RoleAssistant,
		StopReason:  ce.StopReason,
		Interrupted: ce.StopReason == "INTERRUPTED",
	}}
}

func (n *NovaNormalizer) meta(contentID string) contentMeta {
	if contentID == "" {
		contentID = n.lastName
	}
	return n.byName[contentID]
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
