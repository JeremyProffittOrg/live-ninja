package webapp

// Drift guards between the server's suggestion vocabulary and the drawer that
// renders it.
//
// The queue's failure mode is silent: a field the server can propose but the
// client has no branch for renders a row whose Approve button does nothing, and
// nobody finds out until an owner tries it. The two tests below read the
// ACTUAL shipped assets out of web.Files and pin the seams that would break —
// the dotted field strings, and the element ids settings.mjs looks up by hand.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/web"
)

func readAsset(t *testing.T, path string) string {
	t.Helper()
	raw, err := web.Files.ReadFile(path)
	require.NoErrorf(t, err, "reading %s out of the embedded asset tree", path)
	return string(raw)
}

// TestSettingsPanelHandlesEverySuggestibleField: every field the server may
// hand the drawer must have an explicit branch in settings.mjs, either an
// applier (`case 'profile.x':`) or the location path. Adding a suggestible
// field in Go without teaching the client about it fails here instead of
// shipping an Approve button that silently no-ops.
func TestSettingsPanelHandlesEverySuggestibleField(t *testing.T) {
	js := readAsset(t, "static/js/settings.mjs")

	for _, field := range store.SuggestibleProfileFields {
		if store.LocationProfileField(field) {
			// Locations are handled by beginLocationPick, which keys off the
			// work-location field name and defaults to home.
			assert.Containsf(t, js, "'"+store.SuggestFieldWorkLocation+"'",
				"settings.mjs must distinguish the work location in beginLocationPick (%s)", field)
			continue
		}
		assert.Containsf(t, js, "case '"+field+"':",
			"settings.mjs has no apply branch for %s — its Approve button would do nothing", field)
	}

	// The two auto-appliable fields drive the Keep/Undo copy, so they must be
	// named in the toast sentence path too.
	assert.Contains(t, js, "/api/v1/profile/suggestions",
		"the drawer must read the queue from the real route")
	for _, action := range []string{suggestActionApprove, suggestActionReject, suggestActionKeep, suggestActionUndo} {
		assert.Containsf(t, js, "'"+action+"'",
			"the drawer must be able to send action=%s to the resolve route", action)
	}
}

// TestConversationPageCarriesTheSuggestionQueueMarkup pins the element ids
// settings.mjs resolves with getElementById. They are a contract between two
// files with no compiler between them, so a rename in the template shows up
// here rather than as a silently dead panel.
func TestConversationPageCarriesTheSuggestionQueueMarkup(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")

	for _, id := range []string{
		"profileSuggestions",      // the block, hidden when the queue is empty
		"profileSuggestionsList",  // the <ul> rows are appended to
		"profileSuggestionsCount", // the in-panel count
		"profileSuggestionsHint",  // described-by for the list
		"profileSuggestionsTitle", // labelled-by for the list
		"profileSuggestionsStatus",
		"settingsTabBadge", // the drawer-tab badge
	} {
		assert.Containsf(t, html, `id="`+id+`"`,
			"conversation.html must carry #%s — settings.mjs looks it up by id", id)
	}

	// Accessibility contract for the block: the list is labelled and
	// described, and the announcement region is a live status region.
	assert.Contains(t, html, `aria-labelledby="profileSuggestionsTitle" aria-describedby="profileSuggestionsHint"`)
	assert.Contains(t, html, `id="profileSuggestionsStatus" role="status" aria-live="polite"`)

	// The badge must not be the only signal: settings.mjs rewrites the tab's
	// aria-label, so the button has to have one to rewrite.
	tabIdx := strings.Index(html, `id="settingsDrawerBtn"`)
	require.Positive(t, tabIdx)
	assert.Contains(t, html[tabIdx-260:tabIdx+260], `aria-label="Settings"`)
}

// The suggestion queue's styles must exist in the shipped stylesheet, in both
// themes — the rules reference only design tokens, which is what makes the
// light/dark contrast automatic, so this asserts no literal colour crept in.
func TestSuggestionQueueStylesUseDesignTokens(t *testing.T) {
	css := readAsset(t, "static/css/app.css")

	start := strings.Index(css, "SUGGESTION QUEUE (M16")
	require.Positive(t, start, "app.css must carry the M16 suggestion-queue block")
	block := css[start:]

	for _, sel := range []string{
		".ln-suggestions {", ".ln-suggestion {", ".ln-suggestion__values {",
		".ln-suggestion__error {", ".ln-suggestion__actions {", ".conv-settings-tab__badge {",
	} {
		assert.Containsf(t, block, sel, "app.css is missing %s", sel)
	}

	// No raw hex/rgb colours: everything routes through the tokens that carry
	// the light-theme overrides.
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		assert.NotContainsf(t, trimmed, "#0", "hardcoded colour in the M16 block breaks light theme: %q", trimmed)
		assert.NotContainsf(t, trimmed, "rgba(", "hardcoded colour in the M16 block breaks light theme: %q", trimmed)
	}
}
