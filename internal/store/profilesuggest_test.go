package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newSuggestStore() (*Store, *testutil.FakeDynamo) {
	fake := testutil.NewFakeDynamo()
	return NewWithClient(fake, "live-ninja-test"), fake
}

// ---- the locked auto-apply policy ----

// TestAutoApplyableProfileFieldIsExactlyUnitsAndNotes pins the locked M16
// policy as a set, not as a pair of hand-checked cases: every other
// suggestible field must be protected. Written this way so ADDING a
// suggestible field can never quietly widen what the assistant may write —
// a new field lands on the protected side by default and this test says so.
func TestAutoApplyableProfileFieldIsExactlyUnitsAndNotes(t *testing.T) {
	allowed := map[string]bool{}
	for _, f := range SuggestibleProfileFields {
		if AutoApplyableProfileField(f) {
			allowed[f] = true
		}
	}
	assert.Equal(t, map[string]bool{
		SuggestFieldUnits: true,
		SuggestFieldNotes: true,
	}, allowed, "only profile.units and profile.notes[] may be applied without confirmation")

	for _, protected := range []string{
		SuggestFieldHomeLocation, SuggestFieldWorkLocation, SuggestFieldDisplayName,
		SuggestFieldPronouns, SuggestFieldContactEmail, SuggestFieldLocale,
	} {
		_, _, err := ApplyProfileSuggestion(map[string]any{}, protected, "anything")
		assert.ErrorIsf(t, err, ErrProfileFieldProtected,
			"%s must be refused by ApplyProfileSuggestion", protected)
		assert.ErrorIsf(t, RevertProfileSuggestion(map[string]any{}, protected, "a", "b"),
			ErrProfileFieldProtected, "%s must be refused by RevertProfileSuggestion", protected)
	}
}

// TestAutoApplyProfileSuggestionRefusesProtectedFieldWithoutReading is the
// structural half of the policy: the refusal must happen in the STORE, before
// any document is touched, so no caller (model, client, or a future route) can
// route around it. Asserting on the fake's call surface proves the refusal is
// not merely "the write was reverted".
func TestAutoApplyProfileSuggestionRefusesProtectedFieldWithoutReading(t *testing.T) {
	st, fake := newSuggestStore()

	// A real profile already on file, so a successful write would be visible.
	_, err := st.PutSettings(context.Background(), "u1", map[string]any{
		"profile": map[string]any{"units": UnitsImperial},
	}, 1)
	require.NoError(t, err)
	before := fake.RawItem("USER#u1", settingsSK)

	for _, field := range []string{
		SuggestFieldHomeLocation, SuggestFieldWorkLocation,
		SuggestFieldDisplayName, SuggestFieldContactEmail,
		SuggestFieldPronouns, SuggestFieldLocale,
	} {
		_, changed, version, err := st.AutoApplyProfileSuggestion(context.Background(), "u1", field, "Charlotte, NC")
		assert.ErrorIsf(t, err, ErrProfileFieldProtected, "field %s", field)
		assert.Falsef(t, changed, "field %s must not report a change", field)
		assert.Zerof(t, version, "field %s must not commit a version", field)
	}

	assert.Equal(t, before, fake.RawItem("USER#u1", settingsSK),
		"a refused auto-apply must leave the settings document byte-identical")
}

func TestAutoApplyProfileSuggestionRejectsUnknownField(t *testing.T) {
	st, _ := newSuggestStore()
	_, _, _, err := st.AutoApplyProfileSuggestion(context.Background(), "u1", "profile.favouriteColour", "teal")
	assert.ErrorIs(t, err, ErrProfileFieldUnknown)
}

// ---- the document mutations ----

func TestApplyProfileSuggestionUnits(t *testing.T) {
	doc := map[string]any{"profile": map[string]any{"units": UnitsImperial}}

	prev, changed, err := ApplyProfileSuggestion(doc, SuggestFieldUnits, "Metric")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, UnitsImperial, prev, "the replaced value is what an undo restores")
	assert.Equal(t, UnitsMetric, doc["profile"].(map[string]any)["units"],
		"the model's mixed-case value must be normalized to the stored enum")

	// Re-applying the same value is a no-op, and reporting changed=false is
	// what stops a later undo from "restoring" something this never wrote.
	prev, changed, err = ApplyProfileSuggestion(doc, SuggestFieldUnits, UnitsMetric)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, UnitsMetric, prev)
}

func TestApplyProfileSuggestionUnitsDefaultsPreviousToImperial(t *testing.T) {
	// A document with no units at all behaves as imperial, so that — not "" —
	// is what an undo has to put back.
	doc := map[string]any{}
	prev, changed, err := ApplyProfileSuggestion(doc, SuggestFieldUnits, UnitsMetric)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, UnitsImperial, prev)
}

func TestApplyProfileSuggestionNotesAddsWithoutReplacing(t *testing.T) {
	doc := map[string]any{"profile": map[string]any{"notes": []any{"I work in Eastern time"}}}

	prev, changed, err := ApplyProfileSuggestion(doc, SuggestFieldNotes, "  I keep bees  ")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "", prev, "a notes addition replaces nothing, so it has no previous value")
	assert.Equal(t, []any{"I work in Eastern time", "I keep bees"},
		doc["profile"].(map[string]any)["notes"],
		"the existing facts must survive and the new one must be trimmed")

	// A fact already on file is a no-op success, not a duplicate.
	_, changed, err = ApplyProfileSuggestion(doc, SuggestFieldNotes, "I keep bees")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Len(t, doc["profile"].(map[string]any)["notes"], 2)
}

func TestApplyProfileSuggestionNotesRespectsTheCap(t *testing.T) {
	notes := make([]any, 0, MaxProfileNotes)
	for i := 0; i < MaxProfileNotes; i++ {
		notes = append(notes, "fact "+string(rune('a'+i)))
	}
	doc := map[string]any{"profile": map[string]any{"notes": notes}}

	_, changed, err := ApplyProfileSuggestion(doc, SuggestFieldNotes, "one too many")
	assert.ErrorIs(t, err, ErrProfileNotesFull)
	assert.False(t, changed)
	assert.Len(t, doc["profile"].(map[string]any)["notes"], MaxProfileNotes)
}

func TestRevertProfileSuggestionRestoresUnits(t *testing.T) {
	doc := map[string]any{"profile": map[string]any{"units": UnitsMetric}}
	require.NoError(t, RevertProfileSuggestion(doc, SuggestFieldUnits, UnitsMetric, UnitsImperial))
	assert.Equal(t, UnitsImperial, doc["profile"].(map[string]any)["units"])

	// An unknown/empty "previous" restores the documented default rather than
	// writing "" into an enum field.
	doc = map[string]any{"profile": map[string]any{"units": UnitsMetric}}
	require.NoError(t, RevertProfileSuggestion(doc, SuggestFieldUnits, UnitsMetric, ""))
	assert.Equal(t, UnitsImperial, doc["profile"].(map[string]any)["units"])
}

func TestRevertProfileSuggestionRemovesExactlyOneNote(t *testing.T) {
	// Two identical entries can only exist if the owner typed one by hand, so
	// undoing the auto-applied one must leave theirs alone.
	doc := map[string]any{"profile": map[string]any{"notes": []any{"I keep bees", "other", "I keep bees"}}}
	require.NoError(t, RevertProfileSuggestion(doc, SuggestFieldNotes, "I keep bees", ""))
	assert.Equal(t, []any{"other", "I keep bees"}, doc["profile"].(map[string]any)["notes"])
}

func TestNormalizeProfileSuggestionValue(t *testing.T) {
	cases := []struct {
		name, field, in, want string
		wantErr               bool
	}{
		{name: "units normalize case", field: SuggestFieldUnits, in: "METRIC", want: UnitsMetric},
		{name: "units reject celsius", field: SuggestFieldUnits, in: "celsius", wantErr: true},
		{name: "note trimmed", field: SuggestFieldNotes, in: "  I keep bees ", want: "I keep bees"},
		{name: "note too long", field: SuggestFieldNotes, in: strings.Repeat("x", MaxProfileNoteChars+1), wantErr: true},
		{name: "email needs @", field: SuggestFieldContactEmail, in: "jeremy at example.com", wantErr: true},
		{name: "email ok", field: SuggestFieldContactEmail, in: "jeremy@example.com", want: "jeremy@example.com"},
		{name: "empty rejected", field: SuggestFieldDisplayName, in: "   ", wantErr: true},
		{name: "name ok", field: SuggestFieldDisplayName, in: "Jem", want: "Jem"},
		{name: "place name passes through", field: SuggestFieldHomeLocation, in: "Charlotte, NC", want: "Charlotte, NC"},
		{name: "unknown field", field: "profile.nope", in: "x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeProfileSuggestionValue(tc.field, tc.in)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// An over-long value is REJECTED rather than truncated: this string ends up in
// every future session's instructions, and a half-sentence fact is a subtly
// wrong fact.
func TestNormalizeProfileSuggestionValueNeverTruncates(t *testing.T) {
	_, err := NormalizeProfileSuggestionValue(SuggestFieldDisplayName, strings.Repeat("n", MaxProfileNameChars+1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most")
}

// ---- the versioned write path ----

// TestAutoApplyProfileSuggestionUsesOptimisticConcurrency proves the auto-apply
// rides the ordinary settings write path: the version advances, and every
// unrelated field of the document survives (the whole-document PUT rule).
func TestAutoApplyProfileSuggestionUsesOptimisticConcurrency(t *testing.T) {
	st, _ := newSuggestStore()
	ctx := context.Background()

	doc := DefaultSettings()
	doc["voice"] = "cedar"
	doc["theme"] = "dark"
	v, err := st.PutSettings(ctx, "u1", doc, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, v)

	prev, changed, newVersion, err := st.AutoApplyProfileSuggestion(ctx, "u1", SuggestFieldUnits, UnitsMetric)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, UnitsImperial, prev)
	assert.EqualValues(t, 3, newVersion, "the auto-apply must advance the settings version like any other write")

	got, err := st.GetSettings(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, UnitsMetric, got["profile"].(map[string]any)["units"])
	assert.Equal(t, "cedar", got["voice"], "an auto-apply must not drop unrelated settings")
	assert.Equal(t, "dark", got["theme"])

	// And the undo puts it back, through the same path.
	undoVersion, err := st.UndoProfileSuggestion(ctx, "u1", SuggestFieldUnits, UnitsMetric, prev)
	require.NoError(t, err)
	assert.EqualValues(t, 4, undoVersion)
	got, err = st.GetSettings(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, UnitsImperial, got["profile"].(map[string]any)["units"])
}

// A profile written by the auto-apply must still be readable through the typed
// view every consumer uses (mint, tool defaults) — the write is not a
// side-channel with its own shape.
func TestAutoApplyProfileSuggestionIsVisibleToGetProfile(t *testing.T) {
	st, _ := newSuggestStore()
	ctx := context.Background()

	_, _, _, err := st.AutoApplyProfileSuggestion(ctx, "u1", SuggestFieldNotes, "I keep bees")
	require.NoError(t, err)

	p, err := st.GetProfile(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, []string{"I keep bees"}, p.Notes)
}

// ---- PROFSUGG# round-trip ----

// TestProfileSuggestionRoundTripReadsTheRCAShape writes a row the way M17's
// analyzer does and reads it back through the two accessors M16's UI uses. It
// is deliberately spelled with the attribute names, not just the struct, so a
// rename on either side of that contract fails here.
func TestProfileSuggestionRoundTripReadsTheRCAShape(t *testing.T) {
	st, fake := newSuggestStore()
	ctx := context.Background()

	rec := &ProfileSuggestion{
		SuggID:        "abc123def456",
		Field:         SuggestFieldUnits,
		CurrentValue:  UnitsImperial,
		ProposedValue: UnitsMetric,
		Reason:        "The forecast was reported in Fahrenheit but the user asked for Celsius.",
		Source:        "rca",
		SourceRef:     "rca-0001",
	}
	require.NoError(t, st.PutProfileSuggestion(ctx, "u1", rec))
	assert.Equal(t, SuggestionStatusPending, rec.Status, "a filed suggestion defaults to pending")
	assert.NotZero(t, rec.TTL, "a pending suggestion must expire")

	raw := fake.RawItem("USER#u1", rec.SK)
	require.NotNil(t, raw, "the row must land in the user's own partition")
	for _, attr := range []string{"suggId", "status", "field", "currentValue", "proposedValue",
		"reason", "source", "sourceRef", "createdAt", "updatedAt", "ttl"} {
		assert.Containsf(t, raw, attr, "attribute %q is part of the M16/M17 contract", attr)
	}
	assert.True(t, strings.HasPrefix(rec.SK, "PROFSUGG#"))

	listed, err := st.ListProfileSuggestions(ctx, "u1", 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "abc123def456", listed[0].SuggID)
	assert.Equal(t, SuggestFieldUnits, listed[0].Field)
	assert.Equal(t, "rca", listed[0].Source)
	assert.False(t, listed[0].AutoApplied)
	assert.Empty(t, listed[0].ResolvedAt, "a freshly-filed row is unresolved")

	found, err := st.FindProfileSuggestion(ctx, "u1", "abc123def456")
	require.NoError(t, err)
	assert.Equal(t, rec.SK, found.SK)
	assert.Equal(t, UnitsMetric, found.ProposedValue)

	_, err = st.FindProfileSuggestion(ctx, "u1", "no-such-id")
	assert.ErrorIs(t, err, ErrNotFound)

	// Another user's id must not resolve out of this partition.
	_, err = st.FindProfileSuggestion(ctx, "u2", "abc123def456")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestResolveProfileSuggestionIsResolveOnce is the double-click / duplicate-POST
// guard: the second decision on a row must lose, whatever it asks for.
func TestResolveProfileSuggestionIsResolveOnce(t *testing.T) {
	st, _ := newSuggestStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	require.NoError(t, st.PutProfileSuggestion(ctx, "u1", &ProfileSuggestion{
		SuggID: "aaaabbbbcccc", Field: SuggestFieldUnits, ProposedValue: UnitsMetric,
	}))

	got, err := st.ResolveProfileSuggestion(ctx, "u1", "aaaabbbbcccc", SuggestionStatusApproved, now)
	require.NoError(t, err)
	assert.Equal(t, SuggestionStatusPending, got.Status,
		"the returned row is the PRE-update state, which is what an undo needs to read")

	_, err = st.ResolveProfileSuggestion(ctx, "u1", "aaaabbbbcccc", SuggestionStatusRejected, now)
	assert.ErrorIs(t, err, ErrSuggestionResolved)

	listed, err := st.ListProfileSuggestions(ctx, "u1", 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, SuggestionStatusApproved, listed[0].Status, "the first decision stands")
	assert.NotEmpty(t, listed[0].ResolvedAt)
}

func TestResolveProfileSuggestionRejectsUnknownStatus(t *testing.T) {
	st, _ := newSuggestStore()
	_, err := st.ResolveProfileSuggestion(context.Background(), "u1", "x", "maybe", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid suggestion status")
}

func TestProfileFieldLabelDegradesForUnknownFields(t *testing.T) {
	assert.Equal(t, "Preferred units", ProfileFieldLabel(SuggestFieldUnits))
	assert.Equal(t, "profile.somethingNew", ProfileFieldLabel("profile.somethingNew"))
	for _, f := range SuggestibleProfileFields {
		assert.NotEqualf(t, f, ProfileFieldLabel(f), "%s needs a human label for the drawer", f)
	}
}

func TestCurrentProfileValueRendersWhatTheDrawerShows(t *testing.T) {
	p := Profile{
		DisplayName:  "Jeremy",
		ContactEmail: "jeremy@example.com",
		Units:        UnitsMetric,
		HomeLocation: &Location{Label: "Huntersville, North Carolina, United States", Lat: 35.4, Lon: -80.8},
	}
	assert.Equal(t, UnitsMetric, CurrentProfileValue(p, SuggestFieldUnits))
	assert.Equal(t, "Jeremy", CurrentProfileValue(p, SuggestFieldDisplayName))
	assert.Equal(t, "jeremy@example.com", CurrentProfileValue(p, SuggestFieldContactEmail))
	assert.Equal(t, "Huntersville, North Carolina, United States", CurrentProfileValue(p, SuggestFieldHomeLocation))
	assert.Equal(t, "", CurrentProfileValue(p, SuggestFieldNotes),
		"a notes ADDITION has no current value — showing the list would imply it gets replaced")
	// An unset profile reports imperial for units (the behaving default), not "".
	assert.Equal(t, UnitsImperial, CurrentProfileValue(Profile{}, SuggestFieldUnits))
}
