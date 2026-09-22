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

// SupportedAzureRealtimeVoices is the azure-realtime-native voice catalog.
// Every id is quoted from Microsoft Learn "How to use the Voice Live API",
// section "azure-realtime model" / "Supported voices", opened 2026-09-22:
// https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live-how-to
// The default is ava. gpt-live-azure keeps SupportedVoices and cedar; this
// list is not that catalog. Served as azureRealtimeVoices on
// GET /api/v1/realtime/voices. That table has 34 rows; do not add a name.
var SupportedAzureRealtimeVoices = []VoiceInfo{
	{ID: "aarti", Name: "Aarti", Description: "Warm, rich Indian-accented English female voice with a dark, inviting tone. Best for premium support, guided learning, and trusted brand experiences.", Gender: "female"},
	{ID: "alvaro", Name: "Alvaro", Description: "Confident, animated Spanish male voice with strong presence. Best for sales, promotions, and assertive service communication.", Gender: "male"},
	{ID: "andrew", Name: "Andrew", Description: "Textured, relaxed, trustworthy US male voice designed for low-pressure chat.", Gender: "male"},
	{ID: "antonio", Name: "Antonio", Description: "Bright, upbeat Brazilian Portuguese male voice with strong enthusiasm. Best for campaigns, product intros, and energetic customer engagement.", Gender: "male"},
	{ID: "ava", Name: "Ava", Description: "Bright, confident, high-energy US voice. Best for product demos, customer support, and polished branded experiences — default", Gender: "neutral", Default: true},
	{ID: "clara", Name: "Clara", Description: "Clear, versatile Canadian voice with broad usability. Best for general-purpose assistants, education, and customer support.", Gender: "neutral"},
	{ID: "dalia", Name: "Dalia", Description: "Bright, upbeat Mexican Spanish female voice with warm energy. Best for retail, customer engagement, and lively assistant experiences.", Gender: "female"},
	{ID: "denise", Name: "Denise", Description: "Bright, engaging French female voice that keeps attention high. Best for lively customer engagement and onboarding.", Gender: "female"},
	{ID: "diego", Name: "Diego", Description: "Animated, upbeat Italian male voice full of energy. Best for lively conversations, promotions, and entertainment-focused experiences.", Gender: "male"},
	{ID: "diya", Name: "Diya", Description: "Crisp, clear bilingual Hindi and Indian-accented English female voice. Best for troubleshooting, issue resolution, and multilingual support.", Gender: "female"},
	{ID: "elsa", Name: "Elsa", Description: "Confident, crisp Italian female voice with clear delivery. Best for service guidance, explainers, and professional support.", Gender: "female"},
	{ID: "emma", Name: "Emma", Description: "Warm, conversational, mid-pitch US female voice with a dynamic conversational style. Best for routine service help, onboarding, and fast-moving support flows.", Gender: "female"},
	{ID: "florian", Name: "Florian", Description: "Warm, cheerful German male voice with strong clarity and versatility. Best for explainers, education, and approachable support.", Gender: "male"},
	{ID: "francisca", Name: "Francisca", Description: "Cheerful, crisp Brazilian Portuguese female voice with positive clarity. Best for support, onboarding, and service messaging.", Gender: "female"},
	{ID: "hyunsu", Name: "Hyunsu", Description: "Rich, resonant Korean male voice with steady professionalism. Best for formal guidance, explainers, and trusted information delivery.", Gender: "male"},
	{ID: "jorge", Name: "Jorge", Description: "Deep, confident Mexican Spanish male voice with authority and assurance. Best for announcements, logistics, and trust-focused support.", Gender: "male"},
	{ID: "keita", Name: "Keita", Description: "Casual, engaging Japanese male voice with a relaxed but lively feel. Best for chat-based assistants and informal service interactions.", Gender: "male"},
	{ID: "liam", Name: "Liam", Description: "Young Canadian male voice with an enthusiastic, articulate delivery. Best for tech content, tutorials, and educational products.", Gender: "male"},
	{ID: "meera", Name: "Meera", Description: "Calm, warm bilingual Hindi and Indian-accented English female voice with a soothing presence. Best for wellness, care, hospitality, and reflective guidance.", Gender: "female"},
	{ID: "nanami", Name: "Nanami", Description: "Bright, cheerful Japanese female voice with an uplifting tone. Best for welcome messages, retail, and friendly lifestyle experiences.", Gender: "female"},
	{ID: "natasha", Name: "Natasha", Description: "Clear, versatile Australian female voice that adapts easily across use cases. Best for general assistants, support, and instructional content.", Gender: "female"},
	{ID: "niwat", Name: "Niwat", Description: "Confident Thai male voice with smooth, measured professionalism. Best for corporate presentations, podcasts, and formal service messaging.", Gender: "male"},
	{ID: "premwadee", Name: "Premwadee", Description: "Young Thai female voice with a formal, professional tone. Best for announcements, training, and structured communication.", Gender: "female"},
	{ID: "rayn", Name: "Rayn", Description: "Straightforward British male voice with an efficient, neutral style. Best for transactional support, enterprise tools, and service updates.", Gender: "male"},
	{ID: "remy", Name: "Remy", Description: "Cheerful French male voice with an uplifting, conversational tone. Best for chat, retail, and light branded storytelling.", Gender: "male"},
	{ID: "seraphina", Name: "Seraphina", Description: "Casually charming German female voice with a relaxed, engaging style. Best for audiobooks, casual chat, and lifestyle content.", Gender: "female"},
	{ID: "sonia", Name: "Sonia", Description: "Gentle, soft British female voice with a calm, soothing presence. Best for premium support, wellness, and thoughtful onboarding.", Gender: "female"},
	{ID: "sunhi", Name: "Sunhi", Description: "Calm, soothing Korean female voice with dark warmth and measured pacing. Best for wellness, hospitality, and reassuring guidance.", Gender: "female"},
	{ID: "sylvie", Name: "Sylvie", Description: "Calm, soothing Canadian French female voice with steady professionalism. Best for announcements, support, and trusted public-facing communication.", Gender: "female"},
	{ID: "thierry", Name: "Thierry", Description: "Calm Canadian French male voice with a dark, warm timbre. Best for premium narration, wellness, and thoughtful brand experiences.", Gender: "male"},
	{ID: "william", Name: "William", Description: "Calm Australian male voice with warm depth and reassuring confidence. Best for onboarding, support, and premium narration.", Gender: "male"},
	{ID: "xiaoxiao", Name: "Xiaoxiao", Description: "Sweet, soft, welcoming Mandarin female voice with rich emotional range. Best for hospitality, premium care, and warm customer-facing experiences.", Gender: "female"},
	{ID: "ximena", Name: "Ximena", Description: "Crisp, cheerful Spanish female voice with clear positivity. Best for hospitality, support, and guided shopping.", Gender: "female"},
	{ID: "yunxi", Name: "Yunxi", Description: "Lively Mandarin male voice with vivid, expressive emotion. Best for storytelling, engaging assistants, and interactive education.", Gender: "male"},
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
	// Group is the picker section (GroupGeneral/GroupPDLC/GroupESP32/
	// GroupFun). Presentation only; the picker renders one section per
	// group, in GroupOrder.
	Group string `json:"group"`
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
		ordered = append(ordered, PersonaInfo{ID: p.ID, Name: p.Name, Description: personaDescriptions[p.ID], Group: p.Group})
	}
	for _, id := range rest {
		p := personas[id]
		ordered = append(ordered, PersonaInfo{ID: p.ID, Name: p.Name, Description: personaDescriptions[p.ID], Group: p.Group})
	}
	return ordered
}
