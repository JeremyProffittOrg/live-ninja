package realtime

// Persona-embedded voice identity ("we need a persona, not a voice"):
// personas are the unit of voice identity. The settings document carries a
// personaPrefs map {personaId: {voice, accent, updatedAt}} (contracts/
// settings.schema.json) and the broker resolves the per-mint voice and
// accent through the locked precedence chain:
//
//	voice  = personaPrefs[persona].voice ?? persona's suggested voice
//	         ?? top-level voice (the caller's voiceOverride, then the
//	         stored document field) ?? cedar
//	accent = personaPrefs[persona].accent ?? top-level voiceAccent
//
// Every step is lenient by design: unknown/unset candidates fall through
// (forward-compat, contracts/README.md rule 3), and any read failure
// degrades to the remainder of the chain — a voice lookup can never take
// a mint down. An entry's accent that is PRESENT but "" means "explicitly
// no accent" and wins over the top-level fallback (the pointer-typed
// dynamodbav field below is what distinguishes present-empty from
// absent).

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// SessionVoice is the per-mint resolved voice identity.
type SessionVoice struct {
	Voice    string // always a valid realtime voice
	AccentID string // accents-catalog id; ""/unknown mints without a directive
}

// PersonaPrefsKey reduces a mint persona ref to the personaPrefs map key.
// personaPrefs is keyed by the id persona.presetId stores (built-in id,
// custom-persona id, shared-catalog id, or the literal "custom"), so the
// server-composed refs the broker receives ("user:<uid>:<pid>",
// "shared:<pid>" — personas_store.go) reduce to <pid>; every other id is
// the key itself. An empty ref keys as "default" (the id the web layer
// would have resolved it to).
func PersonaPrefsKey(personaRef string) string {
	switch {
	case strings.HasPrefix(personaRef, userPersonaRefPrefix):
		rest := personaRef[len(userPersonaRefPrefix):]
		if _, pid, ok := strings.Cut(rest, ":"); ok && pid != "" {
			return pid
		}
	case strings.HasPrefix(personaRef, sharedPersonaRefPrefix):
		if pid := personaRef[len(sharedPersonaRefPrefix):]; pid != "" {
			return pid
		}
	}
	if personaRef == "" {
		return "default"
	}
	return personaRef
}

// personaPrefEntry is the stored personaPrefs entry subset resolution
// reads. Accent is a pointer so a present-but-"" accent (explicitly none)
// is distinguishable from an absent one (fall through to the top-level
// fallback).
type personaPrefEntry struct {
	Voice  string  `dynamodbav:"voice"`
	Accent *string `dynamodbav:"accent"`
}

// ResolveVoiceChain applies the pure voice precedence rule. Candidates in
// order: the persona's personaPrefs voice, the persona's suggested voice,
// the caller's explicit override, the stored top-level voice — the first
// that names a known realtime voice wins; anything unknown/empty falls
// through, and the chain bottoms out at DefaultVoice (cedar). Never fails:
// a stale stored voice or an unknown override mints on the next candidate
// instead of erroring.
func ResolveVoiceChain(prefVoice, suggestedVoice, override, topVoice string) string {
	for _, c := range []string{prefVoice, suggestedVoice, override, topVoice} {
		if allowedRealtimeVoices[c] {
			return c
		}
	}
	return DefaultVoice
}

// ResolveAccentChain applies the pure accent precedence rule: a
// personaPrefs accent that is PRESENT wins even when "" (explicitly no
// accent); otherwise the top-level voiceAccent fallback applies. The
// catalog's "none" id normalizes to its stored form "". Unknown ids pass
// through untouched — AccentDirective already mints them without a
// directive (forward-compat).
func ResolveAccentChain(prefAccent *string, suggestedAccent, topAccent string) string {
	// A personaPrefs accent present (even "") wins outright. Otherwise the
	// built-in's suggested accent is the baseline, then the top-level
	// voiceAccent fallback.
	accent := topAccent
	if suggestedAccent != "" {
		accent = suggestedAccent
	}
	if prefAccent != nil {
		accent = *prefAccent
	}
	if accent == "none" {
		return ""
	}
	return accent
}

// GeminiSessionVoice is the per-mint resolved voice identity for a
// gemini-flash-live session (M13).
type GeminiSessionVoice struct {
	Voice    string // always a valid Gemini Live prebuilt voice
	AccentID string // same accents catalog as OpenAI; delivered as an instructions directive
}

// ResolveSessionGeminiVoice is ResolveSessionVoice's gemini-flash-live
// sibling (M13 D4/D4b): one GetItem reads the caller's geminiVoice,
// voiceAccent, and personaPrefs, then applies
//
//	voice  = geminiVoice setting ?? persona's GeminiVoice ?? Kore
//	accent = personaPrefs[persona].accent ?? persona suggested ?? voiceAccent
//
// with the same lenient degrade-to-the-chain posture: any read failure or
// unknown candidate falls through, so the result is always mintable.
func ResolveSessionGeminiVoice(ctx context.Context, g SettingsGetter, table, userID, personaRef string) GeminiSessionVoice {
	return ResolveSessionGeminiVoiceForDevice(ctx, g, table, userID, "", personaRef)
}

// ResolveSessionGeminiVoiceForDevice resolves the same chain from the named
// host's effective settings view.
func ResolveSessionGeminiVoiceForDevice(ctx context.Context, g SettingsGetter, table, userID, deviceID, personaRef string) GeminiSessionVoice {
	doc := loadEffectiveVoiceSettings(ctx, g, table, userID, deviceID)

	var prefAccent *string
	if pref, ok := doc.PersonaPrefs[PersonaPrefsKey(personaRef)]; ok {
		prefAccent = pref.Accent
	}

	persona := ResolvePersona(personaRef)
	return GeminiSessionVoice{
		Voice:    ResolveGeminiVoiceChain(doc.GeminiVoice, persona.GeminiVoice),
		AccentID: ResolveAccentChain(prefAccent, persona.SuggestedAccent, doc.VoiceAccent),
	}
}

// ResolveSessionVoice reads the caller's voice/voiceAccent/personaPrefs in
// one GetItem (same single-read posture as ResolveEngine) and applies the
// precedence chains above for the persona being minted. personaRef is the
// server-composed mint ref (or bare id) the broker received; the persona's
// suggested voice comes from the same ResolvePersona registry the mint
// itself uses. Every failure path — nil getter, read error, missing
// document, unmarshal trouble — degrades to the remaining candidates, so
// the result is always a mintable voice.
func ResolveSessionVoice(ctx context.Context, g SettingsGetter, table, userID, personaRef, voiceOverride string) SessionVoice {
	return ResolveSessionVoiceForDevice(ctx, g, table, userID, "", personaRef, voiceOverride)
}

// ResolveSessionVoiceForDevice resolves voice identity from the named host's
// effective persona section while retaining the legacy wrapper above.
func ResolveSessionVoiceForDevice(ctx context.Context, g SettingsGetter, table, userID, deviceID, personaRef, voiceOverride string) SessionVoice {
	doc := loadEffectiveVoiceSettings(ctx, g, table, userID, deviceID)

	var prefVoice string
	var prefAccent *string
	if pref, ok := doc.PersonaPrefs[PersonaPrefsKey(personaRef)]; ok {
		prefVoice = pref.Voice
		prefAccent = pref.Accent
	}

	persona := ResolvePersona(personaRef)
	return SessionVoice{
		Voice:    ResolveVoiceChain(prefVoice, persona.Voice, voiceOverride, doc.Voice),
		AccentID: ResolveAccentChain(prefAccent, persona.SuggestedAccent, doc.VoiceAccent),
	}
}

type effectiveVoiceSettings struct {
	Voice        string
	GeminiVoice  string
	VoiceAccent  string
	PersonaPrefs map[string]personaPrefEntry
}

func loadEffectiveVoiceSettings(ctx context.Context, g SettingsGetter, table, userID, deviceID string) effectiveVoiceSettings {
	var doc effectiveVoiceSettings
	if g == nil || userID == "" {
		return doc
	}
	out, err := g.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &ddbtypes.AttributeValueMemberS{Value: settingsSK},
		},
		ProjectionExpression: aws.String("#v, #g, #a, #p, #o"),
		ExpressionAttributeNames: map[string]string{
			"#v": "voice",
			"#g": "geminiVoice",
			"#a": "voiceAccent",
			"#p": "personaPrefs",
			"#o": "deviceOverrides",
		},
	})
	if err != nil || len(out.Item) == 0 {
		return doc
	}
	var raw map[string]any
	if attributevalue.UnmarshalMap(out.Item, &raw) != nil {
		return doc
	}
	effective := store.EffectiveSettings(raw, deviceID)
	doc.Voice, _ = effective["voice"].(string)
	doc.GeminiVoice, _ = effective["geminiVoice"].(string)
	doc.VoiceAccent, _ = effective["voiceAccent"].(string)
	doc.PersonaPrefs = map[string]personaPrefEntry{}
	prefs, _ := effective["personaPrefs"].(map[string]any)
	for id, value := range prefs {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		pref := personaPrefEntry{}
		pref.Voice, _ = entry["voice"].(string)
		if accent, present := entry["accent"]; present {
			if value, ok := accent.(string); ok {
				pref.Accent = &value
			}
		}
		doc.PersonaPrefs[id] = pref
	}
	return doc
}
