package store

// Knowledge-refinement vocabulary and mutations (M16, plan.md WS-2).
//
// M17 already writes the PROFSUGG# item (see ProfileSuggestion in rca.go for
// the stored shape and why it lives in the user's own partition). M16 adds the
// three things that turn those rows into a loop: a *vocabulary* of proposable
// fields shared by every producer and consumer, the auto-apply policy encoded
// as code rather than as a UI convention, and the two mutations — resolve one
// suggestion, and apply/revert an auto-applied one.
//
// The auto-apply policy is LOCKED (plan.md M16, owner decision): only
// profile.units and profile.notes[] additions may ever be applied without the
// owner confirming in Settings. Location, name and email always require
// confirmation, because a silently mis-set home location poisons every weather
// and time answer afterwards — the exact failure the Base Knowledge layer
// exists to end.
//
// That policy lives HERE, in one function (AutoApplyableProfileField) that
// ApplyProfileSuggestion refuses to work around, precisely so it cannot be
// "enforced" only in a UI. Both callers — the profile_suggest tool
// (internal/tools, invoked by the model) and the Undo route
// (internal/webapp) — go through the same gate, so a client asking the server
// to auto-apply a protected field gets ErrProfileFieldProtected no matter
// which surface it asks from.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Suggestible profile fields, as dotted settings paths. These strings are the
// wire vocabulary shared by the profile_suggest tool's `field` enum, the RCA
// analyzer's proposal allowlist (internal/rca.FieldProfile*), the stored
// PROFSUGG# item's `field` attribute, and the Settings drawer's pending list —
// one spelling, defined once, so a producer and a consumer can never disagree
// about what "the units field" is called.
//
// The "[]" on notes is meaningful: a notes suggestion is an ADDITION to the
// list, never a replacement, which is why it has no current value.
const (
	SuggestFieldNotes        = "profile.notes[]"
	SuggestFieldUnits        = "profile.units"
	SuggestFieldDisplayName  = "profile.displayName"
	SuggestFieldPronouns     = "profile.pronouns"
	SuggestFieldContactEmail = "profile.contactEmail"
	SuggestFieldLocale       = "profile.locale"
	SuggestFieldHomeLocation = "profile.homeLocation"
	SuggestFieldWorkLocation = "profile.workLocation"
)

// SuggestibleProfileFields is the closed set a suggestion may target, in the
// order the tool advertises it (the two safe fields first — the model reaches
// for the head of an enum, and those are the two it can actually get applied).
var SuggestibleProfileFields = []string{
	SuggestFieldNotes,
	SuggestFieldUnits,
	SuggestFieldDisplayName,
	SuggestFieldPronouns,
	SuggestFieldContactEmail,
	SuggestFieldLocale,
	SuggestFieldHomeLocation,
	SuggestFieldWorkLocation,
}

// profileFieldLabels renders a dotted path as the human label the Settings
// drawer shows. Server-side so the label and the field can't drift apart in a
// client copy, and so an unknown field degrades to something readable rather
// than to a raw path in the owner's face.
var profileFieldLabels = map[string]string{
	SuggestFieldNotes:        "Always-known fact",
	SuggestFieldUnits:        "Preferred units",
	SuggestFieldDisplayName:  "Your name",
	SuggestFieldPronouns:     "Pronouns",
	SuggestFieldContactEmail: "Your email",
	SuggestFieldLocale:       "Locale",
	SuggestFieldHomeLocation: "Home location",
	SuggestFieldWorkLocation: "Work location",
}

// ProfileFieldLabel returns the human label for a suggestible field, or the
// raw field when it is not one of ours (a row written by an older or newer
// producer must still render).
func ProfileFieldLabel(field string) string {
	if label, ok := profileFieldLabels[field]; ok {
		return label
	}
	return field
}

// SuggestibleProfileField reports whether field is in the closed set above.
func SuggestibleProfileField(field string) bool {
	_, ok := profileFieldLabels[field]
	return ok
}

// AutoApplyableProfileField reports whether a suggestion for this field may be
// applied WITHOUT the owner confirming it in Settings.
//
// This is the locked policy, and the list is deliberately tiny: units is a
// two-value enum whose worst case is one wrong temperature scale, and a
// notes[] entry is additive, capped, and visible in the drawer. Everything
// else — name, email, pronouns, locale, and above all the two locations —
// requires confirmation. A location is the sharpest case: it can only be
// stored geocode-verified (validateProfile rejects a location without numeric
// lat/lon), so an auto-applied location is not merely risky, it is not even
// representable from a spoken place name.
func AutoApplyableProfileField(field string) bool {
	return field == SuggestFieldUnits || field == SuggestFieldNotes
}

// LocationProfileField reports whether the field holds a geocode-verified
// location, i.e. one that can only be approved by PICKING a geocoder result
// (the Settings drawer turns Approve into "find this place" for these).
func LocationProfileField(field string) bool {
	return field == SuggestFieldHomeLocation || field == SuggestFieldWorkLocation
}

// Bounds on the profile fields a suggestion can carry. These mirror
// contracts/settings.schema.json and are the same numbers
// internal/webapp.validateProfile enforces on the settings PUT — shared here
// so an auto-apply cannot write a document the settings validator would then
// reject on the owner's next edit.
const (
	MaxProfileNotes        = 20
	MaxProfileNoteChars    = 200
	MaxProfileNameChars    = 80
	MaxProfilePronounChars = 32
	MaxProfileLocaleChars  = 20
	MaxProfileEmailChars   = 254
	MaxProfileLabelChars   = 160
)

// Suggestion-mutation failures. Each is a distinct, caller-visible outcome:
// the tool maps them onto tool-error codes, and the HTTP layer onto statuses.
var (
	// ErrProfileFieldProtected — the field requires the owner's confirmation
	// in Settings and can never be auto-applied. See AutoApplyableProfileField.
	ErrProfileFieldProtected = errors.New("store: this profile field requires confirmation in Settings")
	// ErrProfileFieldUnknown — the field is outside SuggestibleProfileFields.
	ErrProfileFieldUnknown = errors.New("store: unknown profile field")
	// ErrProfileNotesFull — the notes list is already at MaxProfileNotes.
	ErrProfileNotesFull = errors.New("store: the profile notes list is full")
	// ErrSuggestionResolved — the suggestion already carries a decision, so
	// this one lost the race (a double-clicked Approve, or a duplicate POST).
	ErrSuggestionResolved = errors.New("store: this suggestion was already resolved")
)

// NormalizeProfileSuggestionValue validates and normalizes a proposed value
// for one field, returning the exact string that should be stored. It is the
// single value-validator behind both the tool (which rejects a bad proposal
// before it ever reaches the queue) and ApplyProfileSuggestion (which must
// never write a value the settings validator would reject).
func NormalizeProfileSuggestionValue(field, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("%s must not be empty", ProfileFieldLabel(field))
	}
	switch field {
	case SuggestFieldUnits:
		// The one genuinely enumerable field: normalize case so "Metric"
		// from a model is accepted rather than bounced.
		switch strings.ToLower(v) {
		case UnitsImperial:
			return UnitsImperial, nil
		case UnitsMetric:
			return UnitsMetric, nil
		default:
			return "", fmt.Errorf("units must be %q or %q, got %q", UnitsImperial, UnitsMetric, v)
		}
	case SuggestFieldNotes:
		return clampErr(v, MaxProfileNoteChars, "an always-known fact")
	case SuggestFieldDisplayName:
		return clampErr(v, MaxProfileNameChars, "a name")
	case SuggestFieldPronouns:
		return clampErr(v, MaxProfilePronounChars, "pronouns")
	case SuggestFieldLocale:
		return clampErr(v, MaxProfileLocaleChars, "a locale tag")
	case SuggestFieldContactEmail:
		if !strings.Contains(v, "@") || strings.ContainsAny(v, " \t") {
			return "", fmt.Errorf("%q is not an email address", v)
		}
		return clampErr(v, MaxProfileEmailChars, "an email address")
	case SuggestFieldHomeLocation, SuggestFieldWorkLocation:
		// A location suggestion carries only the place NAME the assistant
		// heard. It is deliberately not a storable Location: the owner picks
		// the matching geocoder result in Settings, which is what attaches
		// real coordinates and a timezone.
		return clampErr(v, MaxProfileLabelChars, "a place name")
	default:
		return "", ErrProfileFieldUnknown
	}
}

// clampErr rejects (rather than silently truncates) an over-long value. A
// truncated fact is a subtly wrong fact, and this one goes into every future
// session's instructions.
func clampErr(v string, max int, what string) (string, error) {
	if utf8.RuneCountInString(v) > max {
		return "", fmt.Errorf("%s must be at most %d characters", what, max)
	}
	return v, nil
}

// CurrentProfileValue renders what the profile holds today for a suggestible
// field, so the drawer can show "imperial → metric" rather than a bare
// proposal. profile.notes[] is an ADDITION, so it has no current value by
// construction — showing the existing notes there would imply that approving
// the suggestion replaces them.
func CurrentProfileValue(p Profile, field string) string {
	switch field {
	case SuggestFieldUnits:
		return p.UnitsOrDefault()
	case SuggestFieldDisplayName:
		return p.DisplayName
	case SuggestFieldPronouns:
		return p.Pronouns
	case SuggestFieldContactEmail:
		return p.ContactEmail
	case SuggestFieldLocale:
		return p.Locale
	case SuggestFieldHomeLocation:
		return p.Home().Label
	case SuggestFieldWorkLocation:
		return p.Work().Label
	default:
		return ""
	}
}

// ApplyProfileSuggestion applies an auto-applyable suggestion to a settings
// document IN PLACE, returning the value it replaced (for the undo) and
// whether anything actually changed.
//
// It refuses any field AutoApplyableProfileField rejects, with
// ErrProfileFieldProtected. That refusal is the structural half of the locked
// policy: callers do not get to pass a flag that skips it, and there is no
// second code path that writes a profile field without asking here first.
//
// The caller owns the versioned write (GetSettings → this → PutSettings with
// the version it read), so an auto-apply rides the same optimistic-concurrency
// path as every other settings change rather than inventing a new one.
func ApplyProfileSuggestion(doc map[string]any, field, value string) (previous string, changed bool, err error) {
	if doc == nil {
		return "", false, errors.New("store: settings document is required")
	}
	if !SuggestibleProfileField(field) {
		return "", false, ErrProfileFieldUnknown
	}
	if !AutoApplyableProfileField(field) {
		return "", false, ErrProfileFieldProtected
	}
	v, err := NormalizeProfileSuggestionValue(field, value)
	if err != nil {
		return "", false, err
	}

	p := profileSection(doc)
	switch field {
	case SuggestFieldUnits:
		previous, _ = p["units"].(string)
		if previous == "" {
			previous = UnitsImperial // the documented default an absent value means
		}
		if previous == v {
			return previous, false, nil
		}
		p["units"] = v
		return previous, true, nil

	case SuggestFieldNotes:
		notes := profileNotes(p)
		for _, existing := range notes {
			if existing == v {
				// Already known. Not an error — the assistant re-hearing a
				// fact it already stored is normal — but reporting
				// changed=false keeps a later undo from removing a note this
				// call did not add.
				p["notes"] = toAnySlice(notes)
				return "", false, nil
			}
		}
		if len(notes) >= MaxProfileNotes {
			return "", false, ErrProfileNotesFull
		}
		p["notes"] = toAnySlice(append(notes, v))
		return "", true, nil
	}
	// Unreachable: the AutoApplyableProfileField gate above admits exactly
	// the two cases handled here, and this returns an error rather than a
	// silent success if that ever stops being true.
	return "", false, ErrProfileFieldProtected
}

// RevertProfileSuggestion undoes an ApplyProfileSuggestion on a settings
// document in place — the Undo behind the auto-apply toast. appliedValue is
// what was written; previous is what ApplyProfileSuggestion reported.
//
// It goes through the same protected-field gate, so an undo can never be used
// as a back door to write a field an apply was refused.
func RevertProfileSuggestion(doc map[string]any, field, appliedValue, previous string) error {
	if doc == nil {
		return errors.New("store: settings document is required")
	}
	if !SuggestibleProfileField(field) {
		return ErrProfileFieldUnknown
	}
	if !AutoApplyableProfileField(field) {
		return ErrProfileFieldProtected
	}

	p := profileSection(doc)
	switch field {
	case SuggestFieldUnits:
		restore := strings.TrimSpace(previous)
		if restore != UnitsMetric {
			// Anything unrecognized (including "") restores the default the
			// profile behaves as when units is absent.
			restore = UnitsImperial
		}
		p["units"] = restore
		return nil

	case SuggestFieldNotes:
		want := strings.TrimSpace(appliedValue)
		notes := profileNotes(p)
		kept := make([]string, 0, len(notes))
		removed := false
		for _, n := range notes {
			if !removed && n == want {
				removed = true
				continue // drop exactly the one entry the apply added
			}
			kept = append(kept, n)
		}
		p["notes"] = toAnySlice(kept)
		return nil
	}
	return ErrProfileFieldProtected
}

// profileSection returns doc["profile"] as a mutable map, creating it when the
// document has no profile yet (the common case for a first auto-apply).
func profileSection(doc map[string]any) map[string]any {
	if p, ok := doc[profileAttr].(map[string]any); ok && p != nil {
		return p
	}
	p := map[string]any{}
	doc[profileAttr] = p
	return p
}

// profileNotes reads the notes list out of a profile section, tolerating both
// shapes it arrives in: []any from JSON/DynamoDB, []string from Go callers.
// Non-string and blank entries are dropped, matching validateProfile.
func profileNotes(p map[string]any) []string {
	switch raw := p["notes"].(type) {
	case []string:
		out := make([]string, 0, len(raw))
		for _, s := range raw {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	default:
		return nil
	}
}

// toAnySlice stores notes back as []any — the shape encoding/json and
// attributevalue both produce, so a round-tripped document is byte-identical
// whether or not an auto-apply touched it.
func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// ---- the versioned auto-apply / undo write path ----

// autoApplyMaxAttempts bounds the optimistic-concurrency retry. Three is
// generous: the only writers of this document are the owner's own surfaces, so
// a second conflict already means two devices editing settings in the same
// second.
const autoApplyMaxAttempts = 3

// AutoApplyProfileSuggestion applies an auto-applyable suggestion to the user's
// settings document through the SAME optimistic-concurrency path every other
// settings write uses: GetSettings → mutate → PutSettings(version I read),
// retrying on a lost race. It returns the replaced value (for the undo),
// whether anything changed, and the committed version.
//
// This is the only function in the server that writes a profile field on the
// assistant's behalf, and it delegates the field check to
// ApplyProfileSuggestion — so ErrProfileFieldProtected is returned for a
// location/name/email no matter who asks, including a client that hand-crafts
// the tool call with autoApply set.
func (s *Store) AutoApplyProfileSuggestion(ctx context.Context, userID, field, value string) (
	previous string, changed bool, version int64, err error) {
	if userID == "" {
		return "", false, 0, errors.New("store: userID is required")
	}
	// Fail the policy check BEFORE the first read: a refused field must cost
	// nothing and must be indistinguishable (from the caller's side) from a
	// refusal decided with no document at all.
	if !SuggestibleProfileField(field) {
		return "", false, 0, ErrProfileFieldUnknown
	}
	if !AutoApplyableProfileField(field) {
		return "", false, 0, ErrProfileFieldProtected
	}

	for attempt := 0; attempt < autoApplyMaxAttempts; attempt++ {
		doc, err := s.GetSettings(ctx, userID)
		if err != nil {
			return "", false, 0, err
		}
		expected := settingsDocVersion(doc)
		prev, did, err := ApplyProfileSuggestion(doc, field, value)
		if err != nil {
			return "", false, 0, err
		}
		if !did {
			// Already true (units unchanged, or the note is already on file).
			// Nothing to write, and nothing for an undo to revert.
			return prev, false, expected, nil
		}
		newVersion, err := s.PutSettings(ctx, userID, doc, expected)
		if errors.Is(err, ErrVersionConflict) {
			continue // another surface wrote first — re-read and re-apply
		}
		if err != nil {
			return "", false, 0, err
		}
		return prev, true, newVersion, nil
	}
	return "", false, 0, ErrVersionConflict
}

// UndoProfileSuggestion reverses an auto-apply through the same versioned path.
// appliedValue is the value that was written; previous is what
// AutoApplyProfileSuggestion reported it replaced.
func (s *Store) UndoProfileSuggestion(ctx context.Context, userID, field, appliedValue, previous string) (int64, error) {
	if userID == "" {
		return 0, errors.New("store: userID is required")
	}
	if !SuggestibleProfileField(field) {
		return 0, ErrProfileFieldUnknown
	}
	if !AutoApplyableProfileField(field) {
		return 0, ErrProfileFieldProtected
	}

	for attempt := 0; attempt < autoApplyMaxAttempts; attempt++ {
		doc, err := s.GetSettings(ctx, userID)
		if err != nil {
			return 0, err
		}
		expected := settingsDocVersion(doc)
		if err := RevertProfileSuggestion(doc, field, appliedValue, previous); err != nil {
			return 0, err
		}
		newVersion, err := s.PutSettings(ctx, userID, doc, expected)
		if errors.Is(err, ErrVersionConflict) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return newVersion, nil
	}
	return 0, ErrVersionConflict
}

// settingsDocVersion reads the server-owned `version` off a settings document,
// tolerating every numeric shape encoding/json and attributevalue produce. A
// missing version yields 1, which is what GetSettings synthesizes for a user
// who has never written settings — exactly the expectedVersion PutSettings
// wants for a first write.
func settingsDocVersion(doc map[string]any) int64 {
	switch n := doc["version"].(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 1
}

// ---- PROFSUGG# reads and the resolve mutation ----

// profSuggPageLimit / profSuggMaxPages bound FindProfileSuggestion's walk of
// the user's suggestion partition. The queue is small by construction (30-day
// TTL, the analyzer's 10-per-day cap, and the tool's own pending cap), so this
// ceiling is generous — but it is a ceiling: a lookup must not turn into an
// unbounded read just because a suggestion id was mistyped.
const (
	profSuggPageLimit = 100
	profSuggMaxPages  = 10
)

// FindProfileSuggestion locates one suggestion by its id inside the user's own
// partition. The sort key embeds createdAt, so a point GetItem is impossible
// from the id alone; this is a paginated single-partition Query with a
// server-side filter on suggId — never a Scan, and never another user's
// partition. Returns ErrNotFound when the id is unknown (or expired).
//
// The pagination is deliberate rather than defensive: DynamoDB applies
// FilterExpression AFTER Limit, so a single page can legitimately come back
// empty with more items still to walk.
func (s *Store) FindProfileSuggestion(ctx context.Context, userID, suggID string) (*ProfileSuggestion, error) {
	if userID == "" || suggID == "" {
		return nil, errors.New("store: userID and suggId are required")
	}
	var startKey map[string]types.AttributeValue
	for page := 0; page < profSuggMaxPages; page++ {
		out, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
			FilterExpression:       aws.String("suggId = :id"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
				":pfx": &types.AttributeValueMemberS{Value: profSuggSKPrefix},
				":id":  &types.AttributeValueMemberS{Value: suggID},
			},
			ScanIndexForward:  aws.Bool(false),
			Limit:             aws.Int32(profSuggPageLimit),
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, fmt.Errorf("store: find profile suggestion: %w", err)
		}
		if len(out.Items) > 0 {
			var sg ProfileSuggestion
			if err := attributevalue.UnmarshalMap(out.Items[0], &sg); err != nil {
				return nil, fmt.Errorf("store: unmarshal profile suggestion: %w", err)
			}
			return &sg, nil
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return nil, ErrNotFound
}

// ResolveProfileSuggestion stamps a terminal decision on one suggestion and
// returns the row as it stood BEFORE the update (so an undo can read the value
// it has to restore from the same call that claims the right to undo).
//
// One conditional UpdateItem, resolve-once:
//
//	ConditionExpression: attribute_exists(pk) AND attribute_not_exists(resolvedAt)
//
// resolvedAt — not status — is the guard, because both lifecycles converge on
// it: a PENDING row is resolved by Approve/Reject, and an AUTO-APPLIED row
// (already status=approved when written) is resolved by Keep/Undo. One atomic
// claim covers both, so a double-clicked button, a duplicate POST, or two open
// tabs can never apply the same change twice or undo an approval nobody made.
// A lost race returns ErrSuggestionResolved; an unknown id returns ErrNotFound.
func (s *Store) ResolveProfileSuggestion(ctx context.Context, userID, suggID, status string,
	now time.Time) (*ProfileSuggestion, error) {
	switch status {
	case SuggestionStatusApproved, SuggestionStatusRejected:
	default:
		return nil, fmt.Errorf("store: invalid suggestion status %q", status)
	}

	sg, err := s.FindProfileSuggestion(ctx, userID, suggID)
	if err != nil {
		return nil, err
	}

	ts := now.UTC().Format(time.RFC3339Nano)
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: sg.PK},
			"sk": &types.AttributeValueMemberS{Value: sg.SK},
		},
		ConditionExpression: aws.String("attribute_exists(pk) AND attribute_not_exists(resolvedAt)"),
		UpdateExpression:    aws.String("SET #s = :status, updatedAt = :ts, resolvedAt = :ts"),
		ExpressionAttributeNames: map[string]string{
			"#s": "status", // reserved word
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status": &types.AttributeValueMemberS{Value: status},
			":ts":     &types.AttributeValueMemberS{Value: ts},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil, ErrSuggestionResolved
		}
		return nil, fmt.Errorf("store: resolve profile suggestion: %w", err)
	}
	return sg, nil
}
