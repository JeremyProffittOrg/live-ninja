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
// enum), human display name, and a short spoken-style description shown
// in the settings radio rows.
type VoiceInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
}

// SupportedVoices is the canonical, ordered voice catalog — the same
// 10-value set as allowedRealtimeVoices (personas.go) and
// contracts/settings.schema.json#/properties/voice, in the schema's
// canonical enum order. `cedar` is the locked project default
// (DefaultVoice). Keep all three lists in sync when OpenAI ships new
// realtime voices (additive-only, per contracts/README.md rule 3).
var SupportedVoices = []VoiceInfo{
	{ID: "alloy", Name: "Alloy", Description: "Neutral and balanced, even pace"},
	{ID: "ash", Name: "Ash", Description: "Warm, low-pitched, and steady"},
	{ID: "ballad", Name: "Ballad", Description: "Calm, expressive storyteller tone"},
	{ID: "cedar", Name: "Cedar", Description: "Warm and natural, tuned for realtime — default", Default: true},
	{ID: "coral", Name: "Coral", Description: "Bright, friendly, and upbeat"},
	{ID: "echo", Name: "Echo", Description: "Clear, confident, and direct"},
	{ID: "marin", Name: "Marin", Description: "Crisp and lively, tuned for realtime"},
	{ID: "sage", Name: "Sage", Description: "Soft-spoken and gentle"},
	{ID: "shimmer", Name: "Shimmer", Description: "Light, quick, and energetic"},
	{ID: "verse", Name: "Verse", Description: "Versatile and articulate"},
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
