package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Settings storage (M3 subset of the M6 FR-S01 contract): one canonical
// per-user document at PK=USER#<uid>/SK=SETTINGS, shaped by
// contracts/settings.schema.json, guarded by an integer `version`
// optimistic-concurrency counter (ConditionExpression on every write;
// a stale write surfaces ErrVersionConflict → HTTP 409 → the client
// re-reads and re-applies, per contracts/README.md rule 4).
//
// The document is handled as map[string]any end-to-end, NOT a rigid
// struct: additionalProperties is true at every level of the schema and
// every reader/writer (including firmware written years apart) MUST
// preserve unknown fields on write-back — a typed struct would silently
// drop them. Validation of the known fields happens at the HTTP layer
// (internal/webapp/settings_routes.go), not here.

// ErrVersionConflict is returned by PutSettings when the conditional
// version check fails — another surface wrote the document first.
var (
	ErrVersionConflict  = errors.New("store: settings version conflict")
	ErrSettingsTooLarge = errors.New("store: settings document is too large")
)

// MaxSettingsSerializedBytes leaves roughly 100 KiB below DynamoDB's 400 KiB
// item ceiling for attribute-name/type overhead and future server fields.
const MaxSettingsSerializedBytes = 300 * 1024

// settingsSK is the sort key of the canonical settings item.
const settingsSK = "SETTINGS"

const (
	SettingsSectionAboutYou      = "aboutYou"
	SettingsSectionWakeWord      = "wakeWord"
	SettingsSectionPersona       = "persona"
	SettingsSectionVoiceEngine   = "voiceEngine"
	SettingsSectionTurnDetection = "turnDetection"
	SettingsSectionAppearance    = "appearance"
	SettingsSectionMicrophone    = "microphone"
	SettingsSectionPrivacy       = "privacy"

	// MaxDeviceOverrides keeps the single canonical SETTINGS item comfortably
	// below DynamoDB's item limit even when clients retain stale host ids.
	MaxDeviceOverrides = 50
)

var settingsSectionFields = map[string][]string{
	SettingsSectionAboutYou:      {"profile"},
	SettingsSectionWakeWord:      {"wakeWord", "wakeEngine", "sensitivity"},
	SettingsSectionPersona:       {"persona", "voice", "voiceAccent", "personaPrefs"},
	SettingsSectionVoiceEngine:   {"voiceEngine", "geminiVoice"},
	SettingsSectionTurnDetection: {"turnDetection", "micEagerness", "keepListeningSeconds"},
	SettingsSectionAppearance:    {"theme", "appearance"},
	SettingsSectionMicrophone:    {"micDeviceId"},
	SettingsSectionPrivacy:       {"privacy"},
}

// reservedItemAttrs are table plumbing attributes that must never leak
// into (or be accepted from) the settings document itself.
var reservedItemAttrs = []string{"pk", "sk", "ttl", "gsi1pk", "gsi1sk", "gsi2pk", "gsi2sk"}

// DefaultSettings returns a fresh full settings document with every
// schema default (voice default cedar per the locked project decision,
// PRD Q-14). version starts at 1 (schema minimum); the first successful
// PUT against a not-yet-persisted document stores version 2.
func DefaultSettings() map[string]any {
	return map[string]any{
		"version":     1,
		"wakeWord":    "hey-live-ninja",
		"wakeEngine":  "openwakeword",
		"sensitivity": 0.5,
		"persona":     map[string]any{"presetId": "default", "systemInstructions": nil},
		"voice":       "cedar",
		// voiceAccent: speech-accent directive id ("" = none). Not a separate
		// voice — the broker turns it into an instruction line at mint
		// (internal/realtime AccentDirective; catalog in SupportedAccents).
		"voiceAccent": "",
		// personaPrefs: per-persona voice identity map {personaId: {voice,
		// accent, updatedAt}} — personas are the unit of voice identity. The
		// top-level voice/voiceAccent above are only the account-wide
		// fallback default; the broker resolves personaPrefs[persona] first
		// at mint (internal/realtime ResolveSessionVoice). Stored documents
		// that predate this field are seeded on read by
		// migratePersonaPrefs below.
		"personaPrefs":  map[string]any{},
		"turnDetection": "semantic_vad",
		"micEagerness":  "auto",
		// keepListeningSeconds: post-reply client session lifetime. 0 = the
		// mic keeps listening until the user or the voice provider ends the
		// session (owner decision 2026-07-19 — no client-side timeout).
		"keepListeningSeconds": 0,
		// Two style zones (owner-locked defaults): the conversation page's
		// live panel (orb/mic rail) runs hal9000 (red glowing eye), while the
		// rest of the app runs the original ninja navy-and-teal look.
		// accentColor "" means "each zone uses its style's own default accent".
		"appearance": map[string]any{"appStyle": "ninja", "liveStyle": "hal9000", "accentColor": ""},
		// Light is the app-zone default look (ninja-light); the live panel's
		// chrome comes from liveStyle, not from this axis.
		"theme":       "light",
		"micDeviceId": nil,
		"voiceEngine": map[string]any{"default": "openai-realtime", "devices": map[string]any{}},
		"privacy":     map[string]any{"storeAudio": false, "storeTranscripts": true, "retentionDays": 30},
		// deviceOverrides is an additive sparse overlay. Top-level fields
		// remain the account defaults consumed by every legacy client.
		"deviceOverrides": map[string]any{},
		// profile: the Base Knowledge block (M15) — stable facts injected
		// server-side into every session's instructions and used as default
		// arguments for profile-aware tools. Locations are null until the
		// owner picks one from the geocoder (never free-typed), so a fresh
		// document mints exactly as it did pre-M15: an empty profile
		// contributes no instruction block at all. See profile.go.
		"profile": map[string]any{
			"displayName":  "",
			"pronouns":     "",
			"homeLocation": nil,
			"workLocation": nil,
			"units":        UnitsImperial,
			"locale":       "",
			"contactEmail": "",
			"quietHours":   nil,
			"notes":        []any{},
		},
	}
}

// SettingsSectionFields returns a defensive copy of the stable top-level
// field group owned by section.
func SettingsSectionFields(section string) ([]string, bool) {
	fields, ok := settingsSectionFields[section]
	if !ok {
		return nil, false
	}
	return append([]string(nil), fields...), true
}

// SettingsSectionIDs returns the stable API section ids.
func SettingsSectionIDs() []string {
	return []string{
		SettingsSectionAboutYou,
		SettingsSectionWakeWord,
		SettingsSectionPersona,
		SettingsSectionVoiceEngine,
		SettingsSectionTurnDetection,
		SettingsSectionAppearance,
		SettingsSectionMicrophone,
		SettingsSectionPrivacy,
	}
}

// GetEffectiveSettings returns the canonical account defaults overlaid with
// the sparse section settings for deviceID. The returned runtime document
// never exposes the other devices' overrides.
func (s *Store) GetEffectiveSettings(ctx context.Context, userID, deviceID string) (map[string]any, error) {
	doc, err := s.GetSettings(ctx, userID)
	if err != nil {
		return nil, err
	}
	return EffectiveSettings(doc, deviceID), nil
}

// EffectiveSettings overlays one device's sparse section values on a deep
// copy of the account defaults. Legacy voiceEngine.devices pins are treated
// as a Voice Engine override until that section is next written through the
// scoped API.
func EffectiveSettings(doc map[string]any, deviceID string) map[string]any {
	out := cloneSettingsMap(doc)
	delete(out, "deviceOverrides")

	if deviceID != "" {
		if ve, ok := out["voiceEngine"].(map[string]any); ok {
			ve = cloneSettingsMap(ve)
			if devices, ok := ve["devices"].(map[string]any); ok {
				if pin, ok := devices[deviceID].(string); ok && pin != "" {
					ve["default"] = pin
				}
			}
			// This deprecated compatibility map can contain every host's
			// engine pin. An effective view exposes only this host's resolved
			// default, never the cross-host map itself.
			ve["devices"] = map[string]any{}
			out["voiceEngine"] = ve
		}

		if override := deviceOverrideEntry(doc, deviceID); override != nil {
			if sections, ok := override["sections"].(map[string]any); ok {
				for _, section := range SettingsSectionIDs() {
					raw, ok := sections[section].(map[string]any)
					if !ok {
						continue
					}
					for _, field := range settingsSectionFields[section] {
						if value, present := raw[field]; present {
							out[field] = mergeSettingsValue(out[field], value)
						}
					}
				}
			}
		}
	}
	return out
}

// ExtractSettingsSection projects one stable section from an account or
// effective settings document. voiceEngine.devices is storage compatibility
// plumbing, not a value users copy between hosts, so it is stripped.
func ExtractSettingsSection(doc map[string]any, section string) (map[string]any, bool) {
	fields, ok := settingsSectionFields[section]
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, present := doc[field]; present {
			out[field] = cloneSettingsValue(value)
		}
	}
	if section == SettingsSectionVoiceEngine {
		if ve, ok := out["voiceEngine"].(map[string]any); ok {
			ve["devices"] = map[string]any{}
		}
	}
	return out, true
}

// MergeSettingsSection applies a possibly-partial section payload to a copy
// of doc. It rejects top-level fields owned by another section; nested
// objects are deep-merged so additive unknown sub-fields survive.
func MergeSettingsSection(doc, payload map[string]any, section string) (map[string]any, error) {
	fields, ok := settingsSectionFields[section]
	if !ok {
		return nil, fmt.Errorf("store: unknown settings section %q", section)
	}
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	out := cloneSettingsMap(doc)
	for field, value := range payload {
		if !allowed[field] {
			return nil, fmt.Errorf("store: field %q does not belong to section %q", field, section)
		}
		out[field] = mergeSettingsValue(out[field], value)
	}
	return out, nil
}

// DeviceSectionInherited reports whether deviceID has no explicit override
// for section. A legacy voiceEngine.devices pin counts as a custom value.
func DeviceSectionInherited(doc map[string]any, deviceID, section string) bool {
	if section == SettingsSectionVoiceEngine && deviceID != "" {
		if ve, ok := doc["voiceEngine"].(map[string]any); ok {
			if devices, ok := ve["devices"].(map[string]any); ok {
				if pin, ok := devices[deviceID].(string); ok && pin != "" {
					return false
				}
			}
		}
	}
	entry := deviceOverrideEntry(doc, deviceID)
	if entry == nil {
		return true
	}
	sections, _ := entry["sections"].(map[string]any)
	_, present := sections[section]
	return !present
}

// ApplySettingsSection mutates doc in place after the HTTP layer has merged
// and validated payload. For all=true it updates account defaults and clears
// that section on every host. Otherwise it sets or inherits the section for
// deviceIDs. Unknown fields and unrelated sections survive untouched.
func ApplySettingsSection(doc map[string]any, section string, payload map[string]any,
	deviceIDs []string, all, inherit bool, now time.Time) error {
	fields, ok := settingsSectionFields[section]
	if !ok {
		return fmt.Errorf("store: unknown settings section %q", section)
	}
	if all {
		if inherit {
			return errors.New("store: all devices cannot inherit from themselves")
		}
		for _, field := range fields {
			if value, present := payload[field]; present {
				doc[field] = cloneSettingsValue(value)
			}
		}
		if section == SettingsSectionVoiceEngine {
			clearLegacyVoicePins(doc, nil)
		}
		clearSectionFromAllDeviceOverrides(doc, section, now)
		return nil
	}
	if len(deviceIDs) == 0 {
		return errors.New("store: at least one target device is required")
	}
	if !inherit {
		allOverrides, _ := doc["deviceOverrides"].(map[string]any)
		missing := map[string]bool{}
		for _, deviceID := range deviceIDs {
			if strings.TrimSpace(deviceID) == "" {
				return errors.New("store: target device id is required")
			}
			if _, present := allOverrides[deviceID]; !present {
				missing[deviceID] = true
			}
		}
		if len(allOverrides)+len(missing) > MaxDeviceOverrides {
			return fmt.Errorf("store: device override limit is %d", MaxDeviceOverrides)
		}
	}
	for _, deviceID := range deviceIDs {
		if inherit {
			clearDeviceSection(doc, deviceID, section, now)
		} else {
			setDeviceSection(doc, deviceID, section, payload, now)
		}
	}
	if section == SettingsSectionVoiceEngine {
		clearLegacyVoicePins(doc, deviceIDs)
	}
	return nil
}

func deviceOverrideEntry(doc map[string]any, deviceID string) map[string]any {
	if deviceID == "" {
		return nil
	}
	all, _ := doc["deviceOverrides"].(map[string]any)
	entry, _ := all[deviceID].(map[string]any)
	return entry
}

func setDeviceSection(doc map[string]any, deviceID, section string, payload map[string]any, now time.Time) {
	all, ok := doc["deviceOverrides"].(map[string]any)
	if !ok {
		all = map[string]any{}
		doc["deviceOverrides"] = all
	}
	entry, ok := all[deviceID].(map[string]any)
	if !ok {
		entry = map[string]any{}
		all[deviceID] = entry
	}
	sections, ok := entry["sections"].(map[string]any)
	if !ok {
		sections = map[string]any{}
		entry["sections"] = sections
	}
	existing, _ := sections[section].(map[string]any)
	sections[section] = mergeSettingsValue(existing, payload)
	entry["updatedAt"] = now.UTC().Format(time.RFC3339)
}

func clearDeviceSection(doc map[string]any, deviceID, section string, now time.Time) {
	all, _ := doc["deviceOverrides"].(map[string]any)
	entry := deviceOverrideEntry(doc, deviceID)
	if entry == nil {
		return
	}
	sections, _ := entry["sections"].(map[string]any)
	clearKnownSectionFields(sections, section)
	if len(sections) == 0 {
		delete(all, deviceID)
		return
	}
	entry["updatedAt"] = now.UTC().Format(time.RFC3339)
}

func clearSectionFromAllDeviceOverrides(doc map[string]any, section string, now time.Time) {
	all, _ := doc["deviceOverrides"].(map[string]any)
	for deviceID, raw := range all {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if sections, ok := entry["sections"].(map[string]any); ok {
			clearKnownSectionFields(sections, section)
			if len(sections) == 0 {
				delete(all, deviceID)
				continue
			}
			entry["updatedAt"] = now.UTC().Format(time.RFC3339)
		}
	}
}

func clearKnownSectionFields(sections map[string]any, section string) {
	payload, ok := sections[section].(map[string]any)
	if !ok {
		return
	}
	// Inherit/apply-all may clear only fields this server version owns.
	// Additive keys written by a newer client remain device-specific until
	// a version that understands them decides how to merge or clear them.
	for _, field := range settingsSectionFields[section] {
		value, present := payload[field]
		if !present {
			continue
		}
		if unknown, keep := retainUnknownSettingsField(field, value); keep {
			payload[field] = unknown
		} else {
			delete(payload, field)
		}
	}
	if len(payload) == 0 {
		delete(sections, section)
	}
}

func retainUnknownSettingsField(field string, value any) (any, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		// Scalars, arrays, null, and malformed known values have no
		// independently preservable additive properties.
		return nil, false
	}
	switch field {
	case "persona":
		// "hidden" is a known key (persona picker off-switch, 2026-08-01):
		// without it here it would be mistaken for a foreign additive field
		// and preserved as device-specific through an inherit/apply-all.
		deleteKnownMapKeys(object, "presetId", "systemInstructions", "hidden")
	case "personaPrefs":
		for personaID, raw := range object {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue // preserve an unknown/malformed future representation
			}
			deleteKnownMapKeys(entry, "voice", "accent", "updatedAt")
			if len(entry) == 0 {
				delete(object, personaID)
			}
		}
	case "voiceEngine":
		deleteKnownMapKeys(object, "default", "devices")
	case "appearance":
		deleteKnownMapKeys(object, "appStyle", "liveStyle", "accentColor", "themeStyle")
	case "privacy":
		deleteKnownMapKeys(object, "storeAudio", "storeTranscripts", "retentionDays")
	case "profile":
		deleteKnownMapKeys(object,
			"displayName", "pronouns", "units", "locale", "contactEmail", "notes")
		retainUnknownNestedMap(object, "homeLocation",
			"label", "postalCode", "city", "admin1", "country", "lat", "lon", "timezone")
		retainUnknownNestedMap(object, "workLocation",
			"label", "postalCode", "city", "admin1", "country", "lat", "lon", "timezone")
		retainUnknownNestedMap(object, "quietHours", "start", "end")
	default:
		// Known scalar fields and map fields without an additive object
		// schema are removed as one value.
		return nil, false
	}
	return object, len(object) > 0
}

func deleteKnownMapKeys(object map[string]any, keys ...string) {
	for _, key := range keys {
		delete(object, key)
	}
}

func retainUnknownNestedMap(parent map[string]any, field string, knownKeys ...string) {
	raw, present := parent[field]
	if !present {
		return
	}
	nested, ok := raw.(map[string]any)
	if !ok {
		delete(parent, field)
		return
	}
	deleteKnownMapKeys(nested, knownKeys...)
	if len(nested) == 0 {
		delete(parent, field)
	}
}

func clearLegacyVoicePins(doc map[string]any, deviceIDs []string) {
	ve, ok := doc["voiceEngine"].(map[string]any)
	if !ok {
		return
	}
	devices, ok := ve["devices"].(map[string]any)
	if !ok {
		devices = map[string]any{}
		ve["devices"] = devices
	}
	if deviceIDs == nil {
		ve["devices"] = map[string]any{}
		return
	}
	for _, id := range deviceIDs {
		delete(devices, id)
	}
}

func cloneSettingsMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneSettingsValue(value)
	}
	return out
}

func cloneSettingsValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneSettingsMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneSettingsValue(item)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(typed))
		for key, item := range typed {
			out[key] = item
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func mergeSettingsValue(base, override any) any {
	baseMap, baseOK := base.(map[string]any)
	overrideMap, overrideOK := override.(map[string]any)
	if !baseOK || !overrideOK {
		return cloneSettingsValue(override)
	}
	out := cloneSettingsMap(baseMap)
	if out == nil {
		out = map[string]any{}
	}
	for key, value := range overrideMap {
		out[key] = mergeSettingsValue(out[key], value)
	}
	return out
}

// GetSettings fetches the caller's settings document, synthesizing the
// full default document when none has ever been written (there is never
// an "empty settings" response — docs/web-ui-spec.md §3.5). A stored
// document that predates newly-added schema fields gets those fields
// filled from defaults on read (top-level and the persona/voiceEngine/
// privacy required sub-keys) while every stored field — known or
// unknown — is preserved verbatim.
func (s *Store) GetSettings(ctx context.Context, userID string) (map[string]any, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}

	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "USER#" + userID},
			"sk": &types.AttributeValueMemberS{Value: settingsSK},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get settings: %w", err)
	}
	if out.Item == nil {
		return DefaultSettings(), nil
	}

	var doc map[string]any
	if err := attributevalue.UnmarshalMap(out.Item, &doc); err != nil {
		return nil, fmt.Errorf("store: unmarshal settings: %w", err)
	}
	for _, k := range reservedItemAttrs {
		delete(doc, k)
	}
	fillSettingsDefaults(doc)
	return doc, nil
}

// PutSettings stores doc as the caller's full settings document iff the
// stored version still equals expectedVersion (or no document exists
// yet). On success the stored (and returned) version is
// expectedVersion+1. On a lost race it returns ErrVersionConflict and
// writes nothing. doc's own version/plumbing keys are overwritten
// server-side — never trusted from the caller.
func (s *Store) PutSettings(ctx context.Context, userID string, doc map[string]any, expectedVersion int64) (int64, error) {
	if userID == "" {
		return 0, errors.New("store: userID is required")
	}
	if expectedVersion < 1 {
		return 0, errors.New("store: expectedVersion must be >= 1")
	}
	if doc == nil {
		return 0, errors.New("store: settings document is required")
	}

	newVersion := expectedVersion + 1

	item := make(map[string]any, len(doc)+4)
	for k, v := range doc {
		item[k] = v
	}
	for _, k := range reservedItemAttrs {
		delete(item, k)
	}
	item["pk"] = "USER#" + userID
	item["sk"] = settingsSK
	item["version"] = newVersion
	item["updatedAt"] = time.Now().UTC().Format(time.RFC3339)

	serialized, err := json.Marshal(item)
	if err != nil {
		return 0, fmt.Errorf("store: serialize settings size check: %w", err)
	}
	if len(serialized) > MaxSettingsSerializedBytes {
		return 0, fmt.Errorf("%w: %d bytes exceeds %d-byte headroom limit",
			ErrSettingsTooLarge, len(serialized), MaxSettingsSerializedBytes)
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return 0, fmt.Errorf("store: marshal settings: %w", err)
	}

	// attribute_not_exists covers first-ever write (the GET synthesized
	// defaults at version 1, so the first PUT arrives expecting 1 with no
	// stored item); the version equality covers every later write. Two
	// racing first writes still conflict correctly: the loser finds an
	// existing item whose version (2) no longer matches its expected (1).
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk) OR version = :expected"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":expected": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", expectedVersion)},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return 0, ErrVersionConflict
		}
		return 0, fmt.Errorf("store: put settings: %w", err)
	}
	return newVersion, nil
}

// PurgeDeviceSettingsOverride advances the canonical settings version on
// every device revoke, removing that device's sparse override when present.
// The unconditional version bump is also the revoke/write barrier: a section
// PATCH that validated the device before it was marked revoked must either
// commit first (and then be purged here) or lose its stale version write.
// It returns the committed document/version for shadow fan-out.
func (s *Store) PurgeDeviceSettingsOverride(ctx context.Context, userID, deviceID string) (map[string]any, int64, bool, error) {
	if userID == "" || deviceID == "" {
		return nil, 0, false, errors.New("store: userID and deviceID are required")
	}
	for attempt := 0; attempt < 5; attempt++ {
		doc, err := s.GetSettings(ctx, userID)
		if err != nil {
			return nil, 0, false, err
		}
		overrides, _ := doc["deviceOverrides"].(map[string]any)
		delete(overrides, deviceID)
		expected := settingsDocumentVersion(doc)
		newVersion, err := s.PutSettings(ctx, userID, doc, expected)
		if errors.Is(err, ErrVersionConflict) {
			continue
		}
		if err != nil {
			return nil, 0, false, err
		}
		doc["version"] = newVersion
		return doc, newVersion, true, nil
	}
	return nil, 0, false, ErrVersionConflict
}

func settingsDocumentVersion(doc map[string]any) int64 {
	switch value := doc["version"].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case json.Number:
		n, _ := value.Int64()
		return n
	default:
		return 0
	}
}

// fillSettingsDefaults deep-fills missing required fields (top level
// plus the required sub-keys of persona/voiceEngine/privacy) from
// DefaultSettings without touching anything already present.
func fillSettingsDefaults(doc map[string]any) {
	migrateLegacyAppearance(doc)
	migratePersonaPrefs(doc)
	defaults := DefaultSettings()
	for k, dv := range defaults {
		cur, ok := doc[k]
		if !ok {
			doc[k] = dv
			continue
		}
		// micDeviceId / persona.systemInstructions are legitimately null;
		// only object sub-defaults need the deep pass.
		dm, dIsMap := dv.(map[string]any)
		cm, cIsMap := cur.(map[string]any)
		if dIsMap && cIsMap {
			for sk, sdv := range dm {
				if _, ok := cm[sk]; !ok {
					cm[sk] = sdv
				}
			}
		}
	}
}

// migrateLegacyAppearance rewrites the pre-split appearance shape
// ({themeStyle, accentColor}) to the two-zone shape on read: the legacy
// single themeStyle becomes liveStyle (it styled the conversation
// orb/mic panel), appStyle falls to the fill-pass default (ninja), and
// the deprecated key is dropped so there is one source of truth. Runs
// before fillSettingsDefaults' deep fill so the migrated value is never
// clobbered by a default.
func migrateLegacyAppearance(doc map[string]any) {
	ap, ok := doc["appearance"].(map[string]any)
	if !ok {
		return
	}
	if ts, ok := ap["themeStyle"].(string); ok && ts != "" {
		if _, has := ap["liveStyle"]; !has {
			ap["liveStyle"] = ts
		}
	}
	delete(ap, "themeStyle")
}

// migratePersonaPrefs seeds the per-persona voice-identity map on read for
// stored documents that predate it (contracts/settings.schema.json
// personaPrefs): the existing top-level voice/voiceAccent become
// personaPrefs[current persona.presetId] ONCE, so the user's current
// persona keeps sounding exactly as it did, and the top-level fields
// degrade to the account-wide fallback default. Runs before
// fillSettingsDefaults' fill pass so a document that already carries
// personaPrefs (even empty — key presence is the "already migrated"
// signal) is never re-seeded.
func migratePersonaPrefs(doc map[string]any) {
	if _, has := doc["personaPrefs"]; has {
		return
	}
	voice, _ := doc["voice"].(string)
	if voice == "" {
		// Nothing to seed from; the fill pass supplies the empty map.
		return
	}
	accent, _ := doc["voiceAccent"].(string)
	presetID := "default"
	if p, ok := doc["persona"].(map[string]any); ok {
		if id, ok := p["presetId"].(string); ok && id != "" {
			presetID = id
		}
	}
	doc["personaPrefs"] = map[string]any{
		presetID: map[string]any{
			"voice":     voice,
			"accent":    accent,
			"updatedAt": time.Now().UTC().Format(time.RFC3339),
		},
	}
}
