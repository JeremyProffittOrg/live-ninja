package store

import (
	"context"
	"errors"
	"fmt"
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
var ErrVersionConflict = errors.New("store: settings version conflict")

// settingsSK is the sort key of the canonical settings item.
const settingsSK = "SETTINGS"

// reservedItemAttrs are table plumbing attributes that must never leak
// into (or be accepted from) the settings document itself.
var reservedItemAttrs = []string{"pk", "sk", "ttl", "gsi1pk", "gsi1sk", "gsi2pk", "gsi2sk"}

// DefaultSettings returns a fresh full settings document with every
// schema default (voice default cedar per the locked project decision,
// PRD Q-14). version starts at 1 (schema minimum); the first successful
// PUT against a not-yet-persisted document stores version 2.
func DefaultSettings() map[string]any {
	return map[string]any{
		"version":       1,
		"wakeWord":      "hey-live-ninja",
		"wakeEngine":    "openwakeword",
		"sensitivity":   0.5,
		"persona":       map[string]any{"presetId": "default", "systemInstructions": nil},
		"voice":         "cedar",
		// voiceAccent: speech-accent directive id ("" = none). Not a separate
		// voice — the broker turns it into an instruction line at mint
		// (internal/realtime AccentDirective; catalog in SupportedAccents).
		"voiceAccent": "",
		"turnDetection": "semantic_vad",
		"micEagerness":  "auto",
		// Two style zones (owner-locked defaults): the conversation page's
		// live panel (orb/mic rail) runs hal9000 (red glowing eye), while the
		// rest of the app runs the original ninja navy-and-teal look.
		// accentColor "" means "each zone uses its style's own default accent".
		"appearance": map[string]any{"appStyle": "ninja", "liveStyle": "hal9000", "accentColor": ""},
		// Light is the app-zone default look (ninja-light); the live panel's
		// chrome comes from liveStyle, not from this axis.
		"theme":         "light",
		"micDeviceId":   nil,
		"voiceEngine":   map[string]any{"default": "openai-realtime", "devices": map[string]any{}},
		"privacy":       map[string]any{"storeAudio": false, "storeTranscripts": true, "retentionDays": 30},
	}
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
		TableName: aws.String(s.table),
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

// fillSettingsDefaults deep-fills missing required fields (top level
// plus the required sub-keys of persona/voiceEngine/privacy) from
// DefaultSettings without touching anything already present.
func fillSettingsDefaults(doc map[string]any) {
	migrateLegacyAppearance(doc)
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
