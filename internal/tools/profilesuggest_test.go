package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// suggestInv builds a profile_suggest invocation. The tool is side-effecting,
// so the idempotency key is mandatory — omitting it is its own test below.
func suggestInv(args map[string]any) Invocation {
	inv := invocation("profile_suggest", args)
	inv.IdempotencyKey = "idem-" + strings.ReplaceAll(args["field"].(string), ".", "-")
	return inv
}

func suggestArgs(field, value, reason string) map[string]any {
	return map[string]any{"field": field, "value": value, "reason": reason}
}

// pendingRows reads the caller's whole suggestion queue straight out of the
// store, so assertions are about what was actually persisted rather than about
// what the handler said it persisted.
func pendingRows(t *testing.T, deps *Deps) []store.ProfileSuggestion {
	t.Helper()
	rows, err := deps.Store.ListProfileSuggestions(context.Background(), "user-1", 50)
	require.NoError(t, err)
	return rows
}

// TestProfileSuggestQueuesPendingRowAndTouchesNothing is the tool's core
// contract: it writes a PENDING PROFSUGG# row in the caller's own partition and
// leaves the profile completely alone.
func TestProfileSuggestQueuesPendingRowAndTouchesNothing(t *testing.T) {
	deps, fake := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	// A real profile on file, so any accidental mutation would show.
	_, err := deps.Store.PutSettings(ctx, "user-1", map[string]any{
		"profile": map[string]any{"displayName": "Jeremy", "units": store.UnitsImperial},
	}, 1)
	require.NoError(t, err)
	settingsBefore := fake.RawItem("USER#user-1", "SETTINGS")

	res := r.Invoke(ctx, suggestInv(suggestArgs(
		store.SuggestFieldDisplayName, "Jem", `The user said "call me Jem".`)))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "suggested", res.Output["status"])
	assert.Equal(t, false, res.Output["applied"])
	assert.Equal(t, "Jeremy", res.Output["currentValue"])
	assert.Equal(t, "Jem", res.Output["proposedValue"])
	assert.NotEmpty(t, res.Output["suggestionId"])

	// The result text is what stops the model claiming the change landed.
	note, _ := res.Output["note"].(string)
	assert.Contains(t, note, "Settings")
	assert.Contains(t, strings.ToLower(note), "suggested")
	assert.Contains(t, note, "NOTHING has changed yet")

	rows := pendingRows(t, deps)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SuggestionStatusPending, rows[0].Status)
	assert.Equal(t, store.SuggestFieldDisplayName, rows[0].Field)
	assert.Equal(t, "Jeremy", rows[0].CurrentValue)
	assert.Equal(t, "Jem", rows[0].ProposedValue)
	assert.Equal(t, `The user said "call me Jem".`, rows[0].Reason)
	assert.Equal(t, "assistant", rows[0].Source)
	assert.Equal(t, "sess-1", rows[0].SourceRef, "the session is the audit trail back to the conversation")
	assert.False(t, rows[0].AutoApplied)
	assert.Empty(t, rows[0].ResolvedAt)
	assert.Len(t, rows[0].SuggID, 12, "the id is server-generated hex, never model output")

	assert.Equal(t, settingsBefore, fake.RawItem("USER#user-1", "SETTINGS"),
		"profile_suggest must never mutate the profile")
}

// TestProfileSuggestRefusesAutoApplyForProtectedFields is the LOCKED policy
// test: a client (or a model) asking for autoApply on a location, name or email
// gets a queued suggestion and an untouched profile. The refusal is the
// server's, not the UI's.
func TestProfileSuggestRefusesAutoApplyForProtectedFields(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{store.SuggestFieldHomeLocation, "Charlotte, NC"},
		{store.SuggestFieldWorkLocation, "Uptown Charlotte"},
		{store.SuggestFieldDisplayName, "Jem"},
		{store.SuggestFieldContactEmail, "someone-else@example.com"},
		{store.SuggestFieldPronouns, "they/them"},
		{store.SuggestFieldLocale, "en-GB"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			deps, fake := newTestDepsWithFake()
			r := newTestRegistry(t, deps)
			ctx := context.Background()

			_, err := deps.Store.PutSettings(ctx, "user-1", map[string]any{
				"profile": map[string]any{"displayName": "Jeremy", "units": store.UnitsImperial},
			}, 1)
			require.NoError(t, err)
			before := fake.RawItem("USER#user-1", "SETTINGS")

			args := suggestArgs(tc.field, tc.value, "The user mentioned it in passing.")
			args["autoApply"] = true
			res := r.Invoke(ctx, suggestInv(args))

			require.True(t, res.OK, "error: %+v", res.Error)
			assert.Equal(t, "suggested", res.Output["status"],
				"a protected field must fall back to the approval queue, not apply")
			assert.Equal(t, false, res.Output["applied"])

			rows := pendingRows(t, deps)
			require.Len(t, rows, 1)
			assert.Equal(t, store.SuggestionStatusPending, rows[0].Status)
			assert.False(t, rows[0].AutoApplied,
				"a protected field must never be recorded as auto-applied")

			assert.Equal(t, before, fake.RawItem("USER#user-1", "SETTINGS"),
				"a refused auto-apply must leave the settings document byte-identical")

			// The model has to be told WHY, or it will keep asking.
			note, _ := res.Output["note"].(string)
			assert.Contains(t, note, "Settings")
			if store.LocationProfileField(tc.field) {
				assert.Contains(t, note, "picking the exact place",
					"a location can only be approved by picking a geocoder result")
			} else {
				assert.Equal(t, true, res.Output["autoApplyRefused"])
			}
		})
	}
}

// TestProfileSuggestAutoAppliesUnits covers the allowed half of the policy: the
// profile really changes, the row records it as auto-applied with the value it
// replaced (so Settings can offer an Undo), and the result lets the model say
// it is set.
func TestProfileSuggestAutoAppliesUnits(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	_, err := deps.Store.PutSettings(ctx, "user-1", map[string]any{
		"profile": map[string]any{"units": store.UnitsImperial},
	}, 1)
	require.NoError(t, err)

	args := suggestArgs(store.SuggestFieldUnits, "metric", `The user said "I prefer Celsius".`)
	args["autoApply"] = true
	res := r.Invoke(ctx, suggestInv(args))
	require.True(t, res.OK, "error: %+v", res.Error)

	assert.Equal(t, "applied", res.Output["status"])
	assert.Equal(t, true, res.Output["applied"])
	assert.Equal(t, store.UnitsImperial, res.Output["previousValue"])
	note, _ := res.Output["note"].(string)
	assert.Contains(t, note, "undone")

	profile, err := deps.Store.GetProfile(ctx, "user-1")
	require.NoError(t, err)
	assert.Equal(t, store.UnitsMetric, profile.Units, "units must actually be applied")

	rows := pendingRows(t, deps)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SuggestionStatusApproved, rows[0].Status)
	assert.True(t, rows[0].AutoApplied)
	assert.Equal(t, store.UnitsImperial, rows[0].CurrentValue, "the undo needs the replaced value")
	assert.Empty(t, rows[0].ResolvedAt, "the owner has not kept or undone it yet")
}

func TestProfileSuggestAutoAppliesANote(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	args := suggestArgs(store.SuggestFieldNotes, "I keep bees",
		`The user said "remember that I keep bees".`)
	args["autoApply"] = true
	res := r.Invoke(ctx, suggestInv(args))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "applied", res.Output["status"])

	profile, err := deps.Store.GetProfile(ctx, "user-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"I keep bees"}, profile.Notes)
}

// An auto-apply of something already true must not queue a phantom undo row.
func TestProfileSuggestAutoApplyOfAnAlreadyTrueValueQueuesNothing(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	_, err := deps.Store.PutSettings(ctx, "user-1", map[string]any{
		"profile": map[string]any{"units": store.UnitsMetric},
	}, 1)
	require.NoError(t, err)

	args := suggestArgs(store.SuggestFieldUnits, "metric", "The user restated their preference.")
	args["autoApply"] = true
	res := r.Invoke(ctx, suggestInv(args))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "already_true", res.Output["status"])
	assert.Empty(t, pendingRows(t, deps), "nothing changed, so there is nothing to review")
}

func TestProfileSuggestRejectsAnUnstorableValue(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), suggestInv(suggestArgs(
		store.SuggestFieldUnits, "celsius", "The user asked for Celsius.")))
	require.False(t, res.OK)
	require.NotNil(t, res.Error)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	assert.Contains(t, res.Error.Message, "imperial")
	assert.Empty(t, pendingRows(t, deps), "an unstorable value must never reach the queue")
}

// The enum is enforced by the router's own schema gate, so a field the model
// invented never reaches the handler.
func TestProfileSuggestRejectsAnUnknownField(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)

	res := r.Invoke(context.Background(), suggestInv(map[string]any{
		"field": "profile.favouriteColour", "value": "teal", "reason": "why not",
	}))
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
}

// Re-hearing a fact across turns is normal, so a duplicate proposal is a
// success that points at the row already waiting — not a second row, and not an
// error the model would apologise for.
func TestProfileSuggestDoesNotQueueTheSameProposalTwice(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	first := r.Invoke(ctx, suggestInv(suggestArgs(
		store.SuggestFieldNotes, "I keep bees", "The user mentioned their bees.")))
	require.True(t, first.OK, "error: %+v", first.Error)

	// A fresh idempotency key, so this is a genuinely new call rather than the
	// router's duplicate short-circuit.
	inv := suggestInv(suggestArgs(store.SuggestFieldNotes, "I keep bees", "They mentioned bees again."))
	inv.IdempotencyKey = "idem-second"
	second := r.Invoke(ctx, inv)
	require.True(t, second.OK, "error: %+v", second.Error)

	assert.Equal(t, "already_suggested", second.Output["status"])
	assert.Equal(t, first.Output["suggestionId"], second.Output["suggestionId"])
	assert.Len(t, pendingRows(t, deps), 1)
}

// A resolved row must not block a fresh proposal of the same thing: rejecting
// "call me Jem" and then hearing it again should queue it again.
func TestProfileSuggestReQueuesAfterTheOwnerResolved(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	first := r.Invoke(ctx, suggestInv(suggestArgs(store.SuggestFieldDisplayName, "Jem", "They said so.")))
	require.True(t, first.OK, "error: %+v", first.Error)
	id, _ := first.Output["suggestionId"].(string)
	_, err := deps.Store.ResolveProfileSuggestion(ctx, "user-1", id, store.SuggestionStatusRejected, deps.Now())
	require.NoError(t, err)

	inv := suggestInv(suggestArgs(store.SuggestFieldDisplayName, "Jem", "They said so again."))
	inv.IdempotencyKey = "idem-again"
	second := r.Invoke(ctx, inv)
	require.True(t, second.OK, "error: %+v", second.Error)
	assert.Equal(t, "suggested", second.Output["status"])
	assert.Len(t, pendingRows(t, deps), 2, "one resolved, one freshly queued")
}

// The queue is a thing a human reads, so it has a ceiling. Past it the tool
// fails loudly instead of silently growing a wall of proposals.
func TestProfileSuggestCapsThePendingQueue(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	for i := 0; i < maxPendingSuggestions; i++ {
		inv := suggestInv(suggestArgs(store.SuggestFieldNotes, "fact number "+string(rune('a'+i)), "heard it"))
		inv.IdempotencyKey = "idem-cap-" + string(rune('a'+i))
		res := r.Invoke(ctx, inv)
		require.Truef(t, res.OK, "seeding row %d: %+v", i, res.Error)
	}

	inv := suggestInv(suggestArgs(store.SuggestFieldNotes, "one fact too many", "heard it"))
	inv.IdempotencyKey = "idem-cap-overflow"
	res := r.Invoke(ctx, inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeForbidden, res.Error.Code)
	assert.Contains(t, res.Error.Message, "Settings")
	assert.Len(t, pendingRows(t, deps), maxPendingSuggestions)
}

// Side-effecting tools require an idempotency key; profile_suggest is one, so a
// duplicate SQS/realtime delivery cannot file the proposal twice.
func TestProfileSuggestRequiresAnIdempotencyKey(t *testing.T) {
	deps, _ := newTestDepsWithFake()
	r := newTestRegistry(t, deps)

	inv := invocation("profile_suggest", suggestArgs(store.SuggestFieldNotes, "I keep bees", "heard it"))
	res := r.Invoke(context.Background(), inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	assert.Contains(t, res.Error.Message, "idempotencyKey")
}

// TestProfileSuggestIsAdvertisedWithTheSharedFieldVocabulary keeps the tool's
// enum, the store's vocabulary, and the RCA analyzer's field names as one thing:
// if a field is added to store.SuggestibleProfileFields it appears here
// automatically, and if the manifest ever hardcodes a copy this fails.
func TestProfileSuggestIsAdvertisedWithTheSharedFieldVocabulary(t *testing.T) {
	var entry map[string]any
	for _, e := range CatalogManifest() {
		if e["name"] == "profile_suggest" {
			entry = e
			break
		}
	}
	require.NotNil(t, entry, "profile_suggest must be in the single-sourced tool manifest")

	params := entry["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	field := props["field"].(map[string]any)
	assert.Equal(t, store.SuggestibleProfileFields, field["enum"])
	assert.ElementsMatch(t, []string{"field", "value", "reason"}, params["required"])
	assert.Contains(t, props, "autoApply")

	desc, _ := entry["description"].(string)
	assert.Contains(t, desc, "QUEUES", "the description must tell the model this does not apply the change")
	assert.Contains(t, desc, "memory_write", "and where episodic facts go instead")
}

// A profile_suggest call with no profile loader wired (Deps.Profile nil-safe
// path) must still work — it just has no current value to show.
func TestProfileSuggestWorksWithoutAProfileLoader(t *testing.T) {
	fake := testutil.NewFakeDynamo()
	deps := newTestDeps()
	deps.Store = store.NewWithClient(fake, "live-ninja-test")
	deps.DDB = fake
	deps.Profile = nil // NewRegistry defaults it; force the zero-profile path after
	r := newTestRegistry(t, deps)
	deps.Profile = func(ctx context.Context, userID string) store.Profile { return store.Profile{} }

	res := r.Invoke(context.Background(), suggestInv(suggestArgs(
		store.SuggestFieldDisplayName, "Jem", "They said so.")))
	require.True(t, res.OK, "error: %+v", res.Error)
	assert.Equal(t, "", res.Output["currentValue"])
}
