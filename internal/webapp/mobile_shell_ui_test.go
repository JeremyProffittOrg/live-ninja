package webapp

// Drift guard for the mobile conversation shell (2026-08-01): the bottom bar,
// the two-panel snap layout, the conversation overlay, the Copy / Screenshot /
// Tag-for-review tools, and the left-edge Help/Settings tabs.
//
// Same reasoning as help_drawer_ui_test.go: this is a template <-> JS <-> CSS
// contract with no compiler in between. conversation.mjs resolves these
// elements by id (and by the data-conv-action attribute), and app.css is what
// makes the bar appear, the panels snap, and the edge tabs move — nothing
// fails loudly if a markup or stylesheet cleanup takes one of them away. The
// tests read the ACTUAL shipped assets out of web.Files.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMobileBottomBarContract: the persistent bottom toolbar and every control
// on it, plus the ARIA wiring that makes "Show Conversation" announce itself as
// a dialog opener.
func TestMobileBottomBarContract(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	css := readAsset(t, "static/css/app.css")

	for _, want := range []string{
		`id="convBottomBar"`,
		`id="convOverlayOpen"`,
		`id="audioQualitySelect"`,
		`href="/history"`,
		`href="/memory"`,
		`href="/downloads"`,
	} {
		assert.Containsf(t, html, want, "the mobile bottom bar must keep %s", want)
	}

	barAt := strings.Index(html, `id="convBottomBar"`)
	if assert.GreaterOrEqual(t, barAt, 0, "bottom bar is missing") {
		bar := html[barAt:min(len(html), barAt+2200)]
		// The overlay opener starts collapsed; conversation.mjs flips it.
		assert.Contains(t, bar, `aria-expanded="false"`, "Show Conversation must start collapsed")
		assert.Contains(t, bar, `aria-controls="conversationOverlay"`,
			"Show Conversation must point at the overlay it opens")
		// Icon-plus-label items: the label is the accessible name, so it has
		// to be real text and not an aria-hidden glyph alone.
		for _, label := range []string{
			">Show Conversation<", ">History<", ">Memory<", ">Downloads<",
		} {
			assert.Containsf(t, bar, label, "bottom bar must carry the %s item", label)
		}
	}

	// The audio picker: the three quality levels the spec names, plus the mic
	// test action, plus 'auto' (the settings-schema default — without it the
	// select could not represent the shipped default state).
	selAt := strings.Index(html, `id="audioQualitySelect"`)
	if assert.GreaterOrEqual(t, selAt, 0, "audio select is missing") {
		sel := html[selAt:min(len(html), selAt+700)]
		for _, want := range []string{
			`value="auto"`, `value="low"`, `value="medium"`, `value="high"`, `value="mictest"`,
		} {
			assert.Containsf(t, sel, want, "the audio select must offer %s", want)
		}
	}
	assert.Contains(t, html, `for="audioQualitySelect"`,
		"the audio select needs a label element, not a bare aria-label")

	// The bar is mobile-only, and it is the stylesheet that says so.
	assert.Contains(t, css, ".conv-bottombar { display: none; }",
		"the bottom bar must be hidden by default (desktop keeps the rail)")
	assert.Contains(t, css, "@media (max-width: 900px)",
		"app.css must carry the <=900px mobile block")
}

// TestMobileSnapPanels: scrolling up on a phone reveals the transcript because
// .conv-body becomes a snap scroller whose two children are each a full
// scrollport tall. If any of these declarations goes, the reveal silently
// reverts to the old stacked layout.
func TestMobileSnapPanels(t *testing.T) {
	css := readAsset(t, "static/css/app.css")
	html := readAsset(t, "templates/pages/conversation.html")

	for _, want := range []string{
		"scroll-snap-type: y mandatory;",
		"grid-auto-rows: 100%;",
		"scroll-snap-align: start;",
	} {
		assert.Containsf(t, css, want, "the mobile snap scroller needs %s", want)
	}

	// The tap/keyboard route to the same reveal (swipe is not an accessible
	// control on its own).
	assert.Contains(t, html, `id="convScrollHint"`,
		"the conversation reveal must also be reachable as a button")
	assert.Contains(t, css, ".conv-scrollhint { display: none; }",
		"the scroll hint belongs to the mobile layout only")
}

// TestConversationOverlayContract: the overlay is a modal <dialog>, which is
// what supplies the scrim, the focus trap, Escape, and the inert page behind
// (the spec's "prevent scroll-through"). Pin the dialog, both dismiss controls,
// the ~80vh scrollable body, and the overscroll containment.
func TestConversationOverlayContract(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	css := readAsset(t, "static/css/app.css")

	assert.Contains(t, html, `<dialog class="conv-overlay" id="conversationOverlay"`,
		"the conversation overlay must be a <dialog> so showModal() supplies the scrim + focus trap")
	for _, want := range []string{
		`id="conversationOverlayClose"`,
		`id="conversationOverlayHide"`,
		`id="conversationOverlayBody"`,
		`aria-labelledby="conversationOverlayTitle"`,
		`id="conversationOverlayTitle"`,
	} {
		assert.Containsf(t, html, want, "the conversation overlay must keep %s", want)
	}

	closeAt := strings.Index(html, `id="conversationOverlayClose"`)
	if assert.GreaterOrEqual(t, closeAt, 0, "overlay close control is missing") {
		closeBtn := html[closeAt:min(len(html), closeAt+220)]
		assert.Contains(t, closeBtn, "autofocus",
			"the overlay's close control must take initial focus")
	}

	assert.Contains(t, css, "dialog.conv-overlay::backdrop",
		"the overlay needs its semi-transparent backdrop")
	assert.Contains(t, css, "overscroll-behavior: contain;",
		"the overlay body must not chain its scroll into the page behind it")
	assert.Contains(t, css, "max-height: 80dvh;",
		"the overlay body keeps the spec's ~80vh cap")
}

// TestConversationToolsContract: Copy / Screenshot / Tag for review exist in
// BOTH places a user can be reading the conversation, and both copies are
// bound by data-conv-action so they cannot drift onto different handlers.
func TestConversationToolsContract(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	js := readAsset(t, "static/js/conversation.mjs")

	for _, action := range []string{"copy", "screenshot", "tag"} {
		attr := `data-conv-action="` + action + `"`
		assert.Equalf(t, 2, strings.Count(html, attr),
			"%s must appear once over the transcript and once in the overlay", attr)
		assert.Containsf(t, js, `case '`+action+`':`,
			"conversation.mjs must handle the %s action", action)
	}
	assert.Contains(t, js, "[data-conv-action]",
		"conversation.mjs must bind the tools by attribute, not by id")

	// The screenshot path is deliberately dependency-free: no CDN script tag
	// is allowed on this page, so a reintroduced html2canvas would be a
	// runtime 404 rather than a working button.
	assert.Contains(t, js, "canvas.toBlob",
		"the screenshot must be rendered locally to a canvas")
	assert.NotContains(t, html, "html2canvas",
		"no external screenshot library may be script-tagged onto this page")

	// Tag for review: the dialog, its required note, and the local store the
	// Help panel promises.
	for _, want := range []string{
		`id="reviewTagDialog"`,
		`id="reviewTagForm"`,
		`id="reviewTagNote"`,
		`aria-labelledby="reviewTagTitle"`,
	} {
		assert.Containsf(t, html, want, "the review-tag dialog must keep %s", want)
	}
	assert.Contains(t, js, "'ln.reviewTags'",
		"review tags must be persisted under the documented key")
}

// TestMobileEdgeTabsMoveLeft: on a phone the Help and Settings tabs sit on the
// LEFT edge with smaller glyphs, and the in-drawer close bar mirrors to the
// right so the two never overlap. The tab keeps its var(--ln-touch) width, so
// shrinking the glyph does not shrink the tap target.
func TestMobileEdgeTabsMoveLeft(t *testing.T) {
	css := readAsset(t, "static/css/app.css")

	mobileAt := strings.LastIndex(css, "@media (max-width: 900px)")
	if !assert.GreaterOrEqual(t, mobileAt, 0, "the mobile media block is missing") {
		return
	}
	mobile := css[mobileAt:]

	assert.Contains(t, mobile, "left: 0; right: auto;",
		"the edge tabs must move to the left edge on mobile")
	assert.Contains(t, mobile, ".conv-settings-tab__icon { font-size: 16px; }",
		"the edge-tab glyph shrinks from 22px to 16px (~27%) on mobile")
	assert.Contains(t, mobile, "left: auto; right: 0;",
		"the in-drawer close bar must mirror to the right edge on mobile")
	assert.Contains(t, mobile, ".conv-settings-tab__badge { left: auto; right: -8px; }",
		"the suggestion badge must hang off the tab's outer (now right) side")

	// The desktop rule still owns the tap target; assert it is untouched.
	assert.Contains(t, css, "width: var(--ln-touch);\n  height: 40vh;",
		"the edge tab must keep its 44px-wide touch target")
}

// TestNewConversationSaysNEW: the + glyph was replaced by the word, in the same
// position with the same id and the same accessible name.
func TestNewConversationSaysNEW(t *testing.T) {
	html := readAsset(t, "templates/partials/audio_viz.html")

	assert.Contains(t, html, `id="newConversationBtn"`,
		"the new-conversation button keeps its id (conversation.mjs binds it)")
	assert.Contains(t, html, `<span aria-hidden="true">NEW</span>`,
		"the new-conversation button must read NEW")
	assert.NotContains(t, html, "＋",
		"the + glyph must be gone from the new-conversation button")
	assert.Contains(t, html, `aria-label="New conversation"`,
		"the accessible name stays the full phrase, not the shorthand")
}
