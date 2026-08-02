package webapp

// Drift guard for the conversation page's Help slide-out.
//
// The help panel is a template/JS contract with no compiler between the two:
// conversation.mjs looks its three elements up by id and wires showModal() /
// close(), and nothing fails loudly if a markup cleanup renames one of them —
// the tab simply stops opening. These tests read the ACTUAL shipped assets out
// of web.Files and pin the seams, plus the accessibility attributes the panel
// depends on (aria-haspopup/aria-controls/aria-expanded, the labelled dialog,
// and the autofocus close control).

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHelpDrawerMarkupContract: the ids conversation.mjs resolves, and the
// ARIA wiring that makes the tab announce itself as a dialog opener.
func TestHelpDrawerMarkupContract(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")

	for _, want := range []string{
		`id="helpDrawerBtn"`,
		`id="helpDrawer"`,
		`id="helpDrawerClose"`,
		`aria-controls="helpDrawer"`,
		`aria-haspopup="dialog"`,
		`aria-labelledby="helpDrawerTitle"`,
		`id="helpDrawerTitle"`,
	} {
		assert.Containsf(t, html, want, "conversation.html must keep %s for the help drawer", want)
	}

	// The opener starts collapsed; conversation.mjs flips it on open/close.
	btnAt := strings.Index(html, `id="helpDrawerBtn"`)
	if assert.GreaterOrEqual(t, btnAt, 0, "help drawer opener is missing") {
		btn := html[max(0, btnAt-300):min(len(html), btnAt+300)]
		assert.Contains(t, btn, `aria-expanded="false"`,
			"the help tab must start collapsed")
	}

	// showModal() only traps focus usefully if something inside takes it.
	closeAt := strings.Index(html, `id="helpDrawerClose"`)
	if assert.GreaterOrEqual(t, closeAt, 0, "help drawer close control is missing") {
		closeBtn := html[closeAt:min(len(html), closeAt+200)]
		assert.Contains(t, closeBtn, "autofocus",
			"the help drawer's close control must take initial focus")
	}
}

// TestHelpDrawerSharesSettingsDrawerChrome: the help panel is specified to be
// the SAME slide-out as settings, which in this codebase means reusing the
// .conv-drawer / .conv-settings-tab classes rather than cloning their CSS.
// Reusing them is what guarantees the identical slide-in animation, backdrop,
// and edge-tab geometry, so pin the reuse.
func TestHelpDrawerSharesSettingsDrawerChrome(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	css := readAsset(t, "static/css/app.css")

	assert.Contains(t, html, `<dialog class="conv-drawer" id="helpDrawer"`,
		"the help panel must be a .conv-drawer dialog, like #settingsDrawer")
	assert.Contains(t, html, `class="conv-settings-tab conv-settings-tab--help"`,
		"the help opener must reuse the settings edge-tab styling")
	assert.Contains(t, css, ".conv-settings-tab--help",
		"app.css must position the help edge tab relative to the settings tab")

	// The animation is declared once, on the shared selector — a help-only
	// copy would be free to drift away from settings.
	assert.Contains(t, css, "dialog.conv-drawer[open]",
		"the shared drawer open animation must still be defined")
}

// TestHelpDrawerIsNotASettingsAccordion: the settings accordion contract test
// counts data-settings-accordion-trigger occurrences exactly, and
// initSettingsAccordion() is scoped to #settingsDrawer — help sections that
// borrowed the attribute would break the count and never be wired.
func TestHelpDrawerIsNotASettingsAccordion(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")

	// Anchored on the block marker, not the dialog, so the leading template
	// comment is in scope too: even MENTIONING the attribute there inflates
	// the count settings_accordion_ui_test.go asserts on.
	helpAt := strings.Index(html, "HELP DRAWER")
	if !assert.GreaterOrEqual(t, helpAt, 0, "help drawer block marker is missing") {
		return
	}
	assert.NotContains(t, html[helpAt:], "data-settings-accordion-trigger",
		"the help block must not contain the settings accordion attribute, "+
			"not even inside a comment")
}

// TestHelpDrawerCoversTheAppsCapabilities: the panel exists to describe what
// the app does, so hold it to covering the surfaces a user can reach. This is
// the test that fails when a feature ships without its help entry.
func TestHelpDrawerCoversTheAppsCapabilities(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	helpAt := strings.Index(html, `id="helpDrawer"`)
	if !assert.GreaterOrEqual(t, helpAt, 0, "help drawer is missing") {
		return
	}
	help := html[helpAt:]

	// Section headings.
	for _, want := range []string{
		"Getting started",
		"What you can ask for",
		"Where everything lives",
		"Settings explained",
		"Keyboard and navigation",
		"Tips and troubleshooting",
	} {
		assert.Containsf(t, help, want, "help panel is missing the %q section", want)
	}

	// Every settings section a user can open must be explained. These mirror
	// the accordion sections pinned by settings_accordion_ui_test.go.
	for _, want := range []string{
		"About you",
		"Wake word",
		"Persona",
		"Voice engine",
		"Turn detection",
		"Appearance",
		"Microphone",
		"Privacy",
		"Account",
	} {
		assert.Containsf(t, help, ">"+want+"<",
			"help panel must explain the %q settings section", want)
	}

	// Every page reachable from the app shell.
	for _, want := range []string{"History", "Memory", "Personas", "Downloads"} {
		assert.Containsf(t, help, ">"+want+"<", "help panel must describe the %s page", want)
	}

	// The mobile shell (2026-08-01). These are the controls a phone/tablet
	// user sees that a desktop user never does, so the panel is the only
	// place they are explained. Guarded here rather than in
	// mobile_shell_ui_test.go because the rule they enforce is the Help
	// rule: shipping the feature without its help entry is an incomplete
	// change.
	for _, want := range []string{
		"Show Conversation",
		"The bar along the bottom",
		"Copy, Screenshot and Tag for review",
		"Help and Settings",
	} {
		assert.Containsf(t, help, ">"+want+"<",
			"help panel must explain the %q control", want)
	}

	// Cross-device behaviour (2026-08-02). Without these, the topic can be
	// deleted silently: the panel is the only place a user is told why an
	// agent spoke without being asked, and why the device next to it stayed
	// quiet instead of repeating the same news.
	for _, want := range []string{
		"Your other devices",
		"Which device speaks",
	} {
		assert.Containsf(t, help, ">"+want+"<",
			"help panel must explain %q", want)
	}

	// The per-device halves of Wake word and Persona. Both sections already
	// have a <dt>, so the loop above them passes whether or not the copy says
	// anything about devices — these pin the sentences themselves. The wake
	// phrase one is load-bearing: distinct phrases per device are the only
	// thing stopping one device's spoken reply from waking the one beside it,
	// and a user who is never told that will never do it.
	for _, want := range []string{
		"each device can have a different wake phrase",
		"each device can run a different persona",
	} {
		assert.Containsf(t, help, want,
			"help panel must say that %q", want)
	}
}
