package webapp

// Route tests for M16's approval queue. The three things worth pinning are the
// three things a UI could otherwise get quietly wrong:
//
//  1. the list route hands back exactly the shape M17's analyzer wrote (a
//     renamed attribute would silently empty the drawer);
//  2. approving really is the ordinary versioned settings PUT, complete with
//     its 409 on a stale version — not a private write path;
//  3. an auto-apply for a protected field is refused BY THE SERVER, on both
//     the tool path and the undo route, whatever a client asks for.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// newSuggestionsApp mounts the suggestion routes AND the settings routes behind
// a stub auth middleware. Both are needed: approving is a settings PUT followed
// by a resolve, and the point of the test is that those are the same handlers
// production uses.
func newSuggestionsApp(t *testing.T) (*fiber.App, *Deps) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	deps := &Deps{
		Store: store.NewWithClient(fake, "live-ninja"),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	app.Get("/api/v1/settings", handleGetSettings(deps))
	app.Put("/api/v1/settings", handlePutSettings(deps))
	app.Get("/api/v1/profile/suggestions", handleListProfileSuggestions(deps))
	app.Post("/api/v1/profile/suggestions/:id/resolve", handleResolveProfileSuggestion(deps))
	interceptShadowPublish(t) // no IoT clients in a unit test
	return app, deps
}

func seedSuggestion(t *testing.T, deps *Deps, sg *store.ProfileSuggestion) *store.ProfileSuggestion {
	t.Helper()
	require.NoError(t, deps.Store.PutProfileSuggestion(context.Background(), "u1", sg))
	return sg
}

// TestListProfileSuggestionsReturnsTheStoredRCAShape seeds a row exactly as the
// RCA analyzer writes one and asserts the drawer gets every field it renders,
// including the server-derived label and needsPick flags the client must not
// re-derive.
func TestListProfileSuggestionsReturnsTheStoredRCAShape(t *testing.T) {
	app, deps := newSuggestionsApp(t)

	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID:        "aaaaaaaaaaaa",
		Field:         store.SuggestFieldUnits,
		CurrentValue:  store.UnitsImperial,
		ProposedValue: store.UnitsMetric,
		Reason:        "The user asked for Celsius twice.",
		Source:        "rca",
		SourceRef:     "rca-0001",
	})
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID:        "bbbbbbbbbbbb",
		Field:         store.SuggestFieldHomeLocation,
		ProposedValue: "Charlotte, NC",
		Reason:        "The user said they moved.",
		Source:        "assistant",
		SourceRef:     "sess-9",
	})
	// An auto-applied row (not yet kept/undone) and a resolved one.
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "cccccccccccc", Field: store.SuggestFieldNotes, ProposedValue: "I keep bees",
		Status: store.SuggestionStatusApproved, AutoApplied: true, Source: "assistant",
	})
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "dddddddddddd", Field: store.SuggestFieldNotes, ProposedValue: "old news",
		Status: store.SuggestionStatusRejected, ResolvedAt: "2026-07-20T10:00:00Z", Source: "assistant",
	})

	resp, out := doJSON(t, app, http.MethodGet, "/api/v1/profile/suggestions", nil)
	require.Equal(t, 200, resp.StatusCode)

	rows, ok := out["suggestions"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 3, "resolved rows must be filtered out server-side")
	assert.EqualValues(t, 2, out["pendingCount"])
	assert.EqualValues(t, 1, out["autoAppliedCount"])
	assert.EqualValues(t, 3, out["reviewCount"], "the drawer badge counts both kinds")

	byID := map[string]map[string]any{}
	for _, r := range rows {
		row := r.(map[string]any)
		byID[row["id"].(string)] = row
	}

	units := byID["aaaaaaaaaaaa"]
	require.NotNil(t, units)
	assert.Equal(t, store.SuggestFieldUnits, units["field"])
	assert.Equal(t, "Preferred units", units["fieldLabel"], "the label is server-rendered")
	assert.Equal(t, store.UnitsImperial, units["currentValue"])
	assert.Equal(t, store.UnitsMetric, units["proposedValue"])
	assert.Equal(t, "The user asked for Celsius twice.", units["reason"])
	assert.Equal(t, "rca", units["source"])
	assert.Equal(t, "rca-0001", units["sourceRef"])
	assert.Equal(t, store.SuggestionStatusPending, units["status"])
	assert.Equal(t, false, units["autoApplied"])
	assert.Equal(t, false, units["needsPick"])
	assert.NotEmpty(t, units["createdAt"])

	home := byID["bbbbbbbbbbbb"]
	require.NotNil(t, home)
	assert.Equal(t, true, home["needsPick"],
		"a location can only be approved by picking a geocoder result")

	auto := byID["cccccccccccc"]
	require.NotNil(t, auto)
	assert.Equal(t, true, auto["autoApplied"])
}

// TestApproveSuggestionRidesTheVersionedSettingsPut is the approve path end to
// end, in the order the client performs it: PUT the value at the version it
// read (the M15 optimistic-concurrency path), then resolve. It also proves the
// concurrency is real by replaying the same PUT at the now-stale version.
func TestApproveSuggestionRidesTheVersionedSettingsPut(t *testing.T) {
	app, deps := newSuggestionsApp(t)
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "aaaaaaaaaaaa", Field: store.SuggestFieldUnits,
		CurrentValue: store.UnitsImperial, ProposedValue: store.UnitsMetric,
		Reason: "Asked for Celsius.", Source: "assistant",
	})

	// The client reads, applies the proposal into the document it holds, PUTs.
	_, doc := doJSON(t, app, http.MethodGet, "/api/v1/settings", nil)
	version := doc["version"].(float64)
	require.EqualValues(t, 1, version)
	settings := map[string]any{}
	for k, v := range doc {
		settings[k] = v
	}
	profile := settings["profile"].(map[string]any)
	profile["units"] = store.UnitsMetric

	resp, put := doJSON(t, app, http.MethodPut, "/api/v1/settings",
		map[string]any{"settings": settings, "version": int64(version)})
	require.Equal(t, 200, resp.StatusCode, "approve must go through the normal settings PUT")
	assert.EqualValues(t, 2, put["version"])

	// Replaying at the stale version must 409 — the approve path inherits the
	// same cross-device safety every other settings edit has.
	resp, conflict := doJSON(t, app, http.MethodPut, "/api/v1/settings",
		map[string]any{"settings": settings, "version": int64(version)})
	require.Equal(t, 409, resp.StatusCode)
	assert.Equal(t, "version_conflict", conflict["error"].(map[string]any)["code"])

	// Only then is the decision recorded.
	resp, res := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "approve"})
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, store.SuggestionStatusApproved, res["status"])
	assert.NotContains(t, res, "version", "approve writes no settings itself")

	// The profile really carries the approved value, through the same typed
	// view the mint chain reads.
	p, err := deps.Store.GetProfile(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, store.UnitsMetric, p.Units)

	// And the row is gone from the queue.
	_, list := doJSON(t, app, http.MethodGet, "/api/v1/profile/suggestions", nil)
	assert.Empty(t, list["suggestions"])
	assert.EqualValues(t, 0, list["reviewCount"])

	// Resolve-once: a second decision loses.
	resp, again := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "reject"})
	require.Equal(t, 409, resp.StatusCode)
	assert.Equal(t, "already_resolved", again["error"].(map[string]any)["code"])
}

func TestRejectSuggestionLeavesTheProfileAlone(t *testing.T) {
	app, deps := newSuggestionsApp(t)
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "aaaaaaaaaaaa", Field: store.SuggestFieldDisplayName,
		ProposedValue: "Jem", Reason: "Heard it once.", Source: "assistant",
	})

	resp, res := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "reject"})
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, store.SuggestionStatusRejected, res["status"])

	p, err := deps.Store.GetProfile(context.Background(), "u1")
	require.NoError(t, err)
	assert.Empty(t, p.DisplayName)
}

// TestUndoRevertsAnAutoAppliedChange covers the other half of the toast: the
// server put the value there, so the server takes it back — and the response
// carries the committed version so the client's next PUT does not 409 against
// its own undo.
func TestUndoRevertsAnAutoAppliedChange(t *testing.T) {
	app, deps := newSuggestionsApp(t)

	// The state an auto-apply leaves behind: metric on file, plus an
	// unresolved autoApplied row remembering imperial.
	_, err := deps.Store.PutSettings(context.Background(), "u1", map[string]any{
		"profile": map[string]any{"units": store.UnitsMetric, "notes": []any{"I keep bees"}},
	}, 1)
	require.NoError(t, err)
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "aaaaaaaaaaaa", Field: store.SuggestFieldUnits,
		CurrentValue: store.UnitsImperial, ProposedValue: store.UnitsMetric,
		Status: store.SuggestionStatusApproved, AutoApplied: true, Source: "assistant",
	})

	resp, res := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "undo"})
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, store.SuggestionStatusRejected, res["status"])
	assert.NotNil(t, res["version"], "the undo wrote settings, so the client needs the new version")

	p, err := deps.Store.GetProfile(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, store.UnitsImperial, p.Units, "the undo must restore the replaced value")
	assert.Equal(t, []string{"I keep bees"}, p.Notes, "and touch nothing else")
}

func TestUndoOfANoteRemovesOnlyThatNote(t *testing.T) {
	app, deps := newSuggestionsApp(t)
	_, err := deps.Store.PutSettings(context.Background(), "u1", map[string]any{
		"profile": map[string]any{"notes": []any{"I work in Eastern time", "I keep bees"}},
	}, 1)
	require.NoError(t, err)
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "aaaaaaaaaaaa", Field: store.SuggestFieldNotes, ProposedValue: "I keep bees",
		Status: store.SuggestionStatusApproved, AutoApplied: true, Source: "assistant",
	})

	resp, _ := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "undo"})
	require.Equal(t, 200, resp.StatusCode)

	p, err := deps.Store.GetProfile(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, []string{"I work in Eastern time"}, p.Notes)
}

func TestKeepAnAutoAppliedChangeLeavesItInPlace(t *testing.T) {
	app, deps := newSuggestionsApp(t)
	_, err := deps.Store.PutSettings(context.Background(), "u1", map[string]any{
		"profile": map[string]any{"units": store.UnitsMetric},
	}, 1)
	require.NoError(t, err)
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "aaaaaaaaaaaa", Field: store.SuggestFieldUnits,
		CurrentValue: store.UnitsImperial, ProposedValue: store.UnitsMetric,
		Status: store.SuggestionStatusApproved, AutoApplied: true, Source: "assistant",
	})

	resp, res := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "keep"})
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, store.SuggestionStatusApproved, res["status"])
	assert.NotContains(t, res, "version", "keeping writes nothing")

	p, err := deps.Store.GetProfile(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, store.UnitsMetric, p.Units)

	_, list := doJSON(t, app, http.MethodGet, "/api/v1/profile/suggestions", nil)
	assert.EqualValues(t, 0, list["reviewCount"], "an acknowledged change leaves the queue")
}

// TestUndoRefusesAProtectedField is the server-side refusal on the OTHER path.
// A row claiming a location was auto-applied cannot have been written by this
// server, so the undo must refuse rather than guess at what to restore — the
// same gate that refuses the apply.
func TestUndoRefusesAProtectedField(t *testing.T) {
	app, deps := newSuggestionsApp(t)
	_, err := deps.Store.PutSettings(context.Background(), "u1", map[string]any{
		"profile": map[string]any{
			"homeLocation": map[string]any{"label": "Huntersville, NC", "lat": 35.41, "lon": -80.84},
		},
	}, 1)
	require.NoError(t, err)
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "aaaaaaaaaaaa", Field: store.SuggestFieldHomeLocation,
		CurrentValue: "Huntersville, NC", ProposedValue: "Charlotte, NC",
		Status: store.SuggestionStatusApproved, AutoApplied: true, Source: "assistant",
	})

	resp, body := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "undo"})
	require.Equal(t, 400, resp.StatusCode)
	assert.Equal(t, "invalid_request", body["error"].(map[string]any)["code"])

	// The refusal must not have half-written anything: the real, resolved home
	// location is still on file and the row is still unresolved.
	p, err := deps.Store.GetProfile(context.Background(), "u1")
	require.NoError(t, err)
	assert.Equal(t, "Huntersville, NC", p.Home().Label)
	_, list := doJSON(t, app, http.MethodGet, "/api/v1/profile/suggestions", nil)
	assert.EqualValues(t, 1, list["reviewCount"])
}

// approve/reject and keep/undo are not interchangeable: crossing them would let
// a client "undo" a change that was never made, or silently mark a pending
// proposal as kept.
func TestResolveRejectsCrossedActions(t *testing.T) {
	app, deps := newSuggestionsApp(t)
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "pending00000", Field: store.SuggestFieldUnits, ProposedValue: store.UnitsMetric,
		Source: "assistant",
	})
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "applied00000", Field: store.SuggestFieldUnits, ProposedValue: store.UnitsMetric,
		Status: store.SuggestionStatusApproved, AutoApplied: true, Source: "assistant",
	})

	resp, _ := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/pending00000/resolve", map[string]any{"action": "undo"})
	assert.Equal(t, 400, resp.StatusCode, "cannot undo something that was never applied")

	resp, _ = doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/applied00000/resolve", map[string]any{"action": "approve"})
	assert.Equal(t, 400, resp.StatusCode, "an already-applied change is kept or undone, not approved")
}

func TestResolveValidatesItsInputs(t *testing.T) {
	app, deps := newSuggestionsApp(t)
	seedSuggestion(t, deps, &store.ProfileSuggestion{
		SuggID: "aaaaaaaaaaaa", Field: store.SuggestFieldUnits, ProposedValue: store.UnitsMetric,
		Source: "assistant",
	})

	resp, _ := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "obliterate"})
	assert.Equal(t, 400, resp.StatusCode)

	resp, _ = doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/nope00000000/resolve", map[string]any{"action": "approve"})
	assert.Equal(t, 404, resp.StatusCode)

	// A '#' in the id would be an attempt to steer the sort key. Whether the
	// router rejects the escaped path or the handler's own guard does, the one
	// thing that must never happen is a 2xx: the id is a component of the
	// PROFSUGG# sort key, and only a server-generated hex id belongs there.
	resp, _ = doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/a%23b/resolve", map[string]any{"action": "approve"})
	assert.GreaterOrEqual(t, resp.StatusCode, 400,
		"an id carrying '#' must be refused, never resolved")
}

// The queue is per-user: one account's suggestion id must not resolve from
// another's session, even though both hit the same route.
func TestSuggestionsAreScopedToTheCaller(t *testing.T) {
	fake := testutil.NewFakeDynamo()
	deps := &Deps{
		Store: store.NewWithClient(fake, "live-ninja"),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	require.NoError(t, deps.Store.PutProfileSuggestion(context.Background(), "someone-else", &store.ProfileSuggestion{
		SuggID: "aaaaaaaaaaaa", Field: store.SuggestFieldUnits, ProposedValue: store.UnitsMetric,
		Source: "assistant",
	}))

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	app.Get("/api/v1/profile/suggestions", handleListProfileSuggestions(deps))
	app.Post("/api/v1/profile/suggestions/:id/resolve", handleResolveProfileSuggestion(deps))

	_, list := doJSON(t, app, http.MethodGet, "/api/v1/profile/suggestions", nil)
	assert.Empty(t, list["suggestions"])

	resp, _ := doJSON(t, app, http.MethodPost,
		"/api/v1/profile/suggestions/aaaaaaaaaaaa/resolve", map[string]any{"action": "approve"})
	assert.Equal(t, 404, resp.StatusCode)
}
