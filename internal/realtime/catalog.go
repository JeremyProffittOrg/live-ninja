package realtime

import "sort"

// This file is the *static* voice/persona catalog surface. Unlike the
// rest of the package (broker-only: mint, quota, fallback — never
// imported by the web function, which reaches the broker via
// Lambda:Invoke), the catalog carries no secrets and no OpenAI-key
// dependency, so internal/webapp deliberately imports it directly to
// populate the Settings/Conversation pickers (GET /api/v1/realtime/
// voices and /personas, docs/web-ui-spec.md §3.3/§4). Keep anything
// key-adjacent out of this file.

// VoiceInfo describes one selectable OpenAI Realtime voice for UI
// pickers: stable ID (the wire value in settings.schema.json's `voice`
// enum), human display name, a short spoken-style description shown in
// the settings radio rows, and the voice's commonly *perceived* gender
// presentation ("female" | "male" | "neutral") used purely as a UI
// filter tag — OpenAI does not publish official gender labels, so these
// are best-judgment perception tags, not facts about the voices.
type VoiceInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Gender      string `json:"gender"` // perceived: "female" | "male" | "neutral"
	Default     bool   `json:"default"`
}

// SupportedVoices is the canonical, ordered voice catalog — the same
// 10-value set as allowedRealtimeVoices (personas.go) and
// contracts/settings.schema.json#/properties/voice, in the schema's
// canonical enum order. `cedar` is the locked project default
// (DefaultVoice). Keep all three lists in sync when OpenAI ships new
// realtime voices (additive-only, per contracts/README.md rule 3).
var SupportedVoices = []VoiceInfo{
	{ID: "alloy", Name: "Alloy", Description: "Neutral and balanced, even pace", Gender: "neutral"},
	{ID: "ash", Name: "Ash", Description: "Warm, low-pitched, and steady", Gender: "male"},
	{ID: "ballad", Name: "Ballad", Description: "Calm, expressive storyteller tone", Gender: "male"},
	{ID: "cedar", Name: "Cedar", Description: "Warm and natural, tuned for realtime — default", Gender: "male", Default: true},
	{ID: "coral", Name: "Coral", Description: "Bright, friendly, and upbeat", Gender: "female"},
	{ID: "echo", Name: "Echo", Description: "Clear, confident, and direct", Gender: "male"},
	{ID: "marin", Name: "Marin", Description: "Crisp and lively, tuned for realtime", Gender: "female"},
	{ID: "sage", Name: "Sage", Description: "Soft-spoken and gentle", Gender: "female"},
	{ID: "shimmer", Name: "Shimmer", Description: "Light, quick, and energetic", Gender: "female"},
	{ID: "verse", Name: "Verse", Description: "Versatile and articulate", Gender: "male"},
}

// SupportedGeminiVoices is the Gemini Live prebuilt-HD voice catalog for the
// gemini-flash-live engine's voice picker (M13, D4). Every entry was
// runtime-validated against gemini-3.1-flash-live-preview in the Phase 0
// spike (2026-07-19): setup accepted + real audio synthesized. Descriptions
// are Google's published one-word style adjectives; gender tags are
// best-judgment perception tags like SupportedVoices'. `Kore` is the locked
// engine default (D4). Served as the `geminiVoices` sibling of `voices` on
// GET /api/v1/realtime/voices.
var SupportedGeminiVoices = []VoiceInfo{
	{ID: "Zephyr", Name: "Zephyr", Description: "Bright", Gender: "female"},
	{ID: "Puck", Name: "Puck", Description: "Upbeat", Gender: "male"},
	{ID: "Charon", Name: "Charon", Description: "Informative", Gender: "male"},
	{ID: "Kore", Name: "Kore", Description: "Firm — default", Gender: "female", Default: true},
	{ID: "Fenrir", Name: "Fenrir", Description: "Excitable", Gender: "male"},
	{ID: "Leda", Name: "Leda", Description: "Youthful", Gender: "female"},
	{ID: "Orus", Name: "Orus", Description: "Firm", Gender: "male"},
	{ID: "Aoede", Name: "Aoede", Description: "Breezy", Gender: "female"},
	{ID: "Callirrhoe", Name: "Callirrhoe", Description: "Easy-going", Gender: "female"},
	{ID: "Autonoe", Name: "Autonoe", Description: "Bright", Gender: "female"},
	{ID: "Enceladus", Name: "Enceladus", Description: "Breathy", Gender: "male"},
	{ID: "Iapetus", Name: "Iapetus", Description: "Clear", Gender: "male"},
	{ID: "Umbriel", Name: "Umbriel", Description: "Easy-going", Gender: "male"},
	{ID: "Algieba", Name: "Algieba", Description: "Smooth", Gender: "male"},
	{ID: "Despina", Name: "Despina", Description: "Smooth", Gender: "female"},
	{ID: "Erinome", Name: "Erinome", Description: "Clear", Gender: "female"},
	{ID: "Algenib", Name: "Algenib", Description: "Gravelly", Gender: "male"},
	{ID: "Rasalgethi", Name: "Rasalgethi", Description: "Informative", Gender: "male"},
	{ID: "Laomedeia", Name: "Laomedeia", Description: "Upbeat", Gender: "female"},
	{ID: "Achernar", Name: "Achernar", Description: "Soft", Gender: "female"},
	{ID: "Alnilam", Name: "Alnilam", Description: "Firm", Gender: "male"},
	{ID: "Schedar", Name: "Schedar", Description: "Even", Gender: "male"},
	{ID: "Gacrux", Name: "Gacrux", Description: "Mature", Gender: "female"},
	{ID: "Pulcherrima", Name: "Pulcherrima", Description: "Forward", Gender: "female"},
	{ID: "Achird", Name: "Achird", Description: "Friendly", Gender: "male"},
	{ID: "Zubenelgenubi", Name: "Zubenelgenubi", Description: "Casual", Gender: "male"},
	{ID: "Vindemiatrix", Name: "Vindemiatrix", Description: "Gentle", Gender: "female"},
	{ID: "Sadachbia", Name: "Sadachbia", Description: "Lively", Gender: "male"},
	{ID: "Sadaltager", Name: "Sadaltager", Description: "Knowledgeable", Gender: "male"},
	{ID: "Sulafat", Name: "Sulafat", Description: "Warm", Gender: "female"},
}

// AccentInfo is one selectable speech accent for the settings "Accent"
// picker. Accents are NOT separate voices: the realtime voice set is
// fixed, so an accent is delivered as a short speech-style directive
// appended to the session instructions at mint (gpt-realtime follows
// accent directives well). ID "none" is the no-directive default and
// maps to the stored settings value "" (voiceAccent).
type AccentInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// SupportedAccents is the ordered accent catalog for UI pickers.
// "none"/"" means no accent directive. Every non-none ID here must have
// a matching directive in accentDirectives (mint.go) — a unit test
// enforces the pairing.
var SupportedAccents = []AccentInfo{
	{ID: "none", Label: "Default"},
	{ID: "irish", Label: "Irish"},
	{ID: "british", Label: "British"},
	{ID: "scottish", Label: "Scottish"},
	{ID: "australian", Label: "Australian"},
	{ID: "southern-us", Label: "Southern US"},
	{ID: "french", Label: "French"},
	{ID: "german", Label: "German"},
	{ID: "indian", Label: "Indian"},
	{ID: "new-york", Label: "New York"},
}

// IsSupportedAccent reports whether id is a selectable accent value.
// "" (stored form of "none") and "none" are both accepted.
func IsSupportedAccent(id string) bool {
	if id == "" {
		return true
	}
	for _, a := range SupportedAccents {
		if a.ID == id {
			return true
		}
	}
	return false
}

// PersonaInfo is the client-visible slice of a Persona: ID, display
// name, and a short description. Instructions are deliberately absent —
// clients only ever reference personas by ID (anti-injection rule in
// personas.go); the raw instruction text never leaves the server.
type PersonaInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// personaDescriptions supplies the picker blurb per registry entry.
// Entries without a blurb still list (empty description) so a persona
// added to the registry can never silently vanish from the picker.
var personaDescriptions = map[string]string{
	"default": "Fast, warm, and practical — the standard Live Ninja personality.",
}

// ListPersonas returns the persona catalog for UI pickers, derived from
// the same registry ResolvePersona serves, so the picker can never
// offer an ID the broker would not resolve. Order is stable: "default"
// first, then the rest alphabetically by ID. The literal "custom"
// option (free-text instructions, settings.schema.json persona rule) is
// appended by the UI layer, not listed here — it is not a server-side
// persona.
func ListPersonas() []PersonaInfo {
	rest := make([]string, 0, len(personas))
	for id := range personas {
		if id != "default" {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)

	ordered := make([]PersonaInfo, 0, len(personas))
	if p, ok := personas["default"]; ok {
		ordered = append(ordered, PersonaInfo{ID: p.ID, Name: p.Name, Description: personaDescriptions[p.ID]})
	}
	for _, id := range rest {
		p := personas[id]
		ordered = append(ordered, PersonaInfo{ID: p.ID, Name: p.Name, Description: personaDescriptions[p.ID]})
	}
	return ordered
}
