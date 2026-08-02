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
	"github.com/stretchr/testify/require"
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

	// The bar is shown at EVERY width (owner 2026-08-01: "make the icons on
	// the bottom the default, regardless of window size"). The old
	// `display: none` default, and the mobile-only copy of these rules inside
	// the <=900px block, are both gone.
	assert.NotContains(t, css, ".conv-bottombar { display: none; }",
		"the bottom bar is no longer hidden at desktop widths")
	assert.Contains(t, css, "@media (max-width: 900px)",
		"app.css must carry the <=900px mobile block")

	mobileAt := strings.LastIndex(css, "@media (max-width: 900px)")
	if assert.GreaterOrEqual(t, mobileAt, 0) {
		assert.NotContains(t, css[mobileAt:], ".conv-bottombar {",
			"the bar's layout must live at the top level, not inside the mobile block")
	}
	// It is a flex:none row inside .conv-app, which is what pins it to the
	// bottom of the viewport without position:fixed at any width.
	barCSSAt := strings.Index(css, ".conv-bottombar {")
	if assert.GreaterOrEqual(t, barCSSAt, 0, "the bottom bar has no layout rule") {
		barCSS := css[barCSSAt:min(len(css), barCSSAt+320)]
		assert.Contains(t, barCSS, "flex: none;",
			"the bar must take its height out of the scroller above it")
		assert.Contains(t, barCSS, "display: flex;", "the bar must be shown by default")
	}
}

// TestConversationPageDoesNotScrollTheRoot: .conv-app is a 100dvh flex column
// that owns its own scrollers, but its own `overflow: hidden` was NOT enough —
// below 900px .conv-body's second snap panel extends a full viewport past
// .conv-app's box, and the ROOT scroller still counted it. That gave the page a
// phantom ~viewport-tall scroll range which carried the bottom bar off screen
// (owner 2026-08-01). Measured on a 390x844 viewport before the fix:
// documentElement.scrollHeight 1526 against clientHeight 844.
//
// The fix is a body class stamped by pages_routes.go for /conversation only, so
// every other page keeps normal page scrolling. Both halves are asserted here
// because either one alone is a silent no-op.
func TestConversationPageDoesNotScrollTheRoot(t *testing.T) {
	css := readAsset(t, "static/css/app.css")

	assert.Equal(t, "ln-body--fixed", pageMetas["pages/conversation"].BodyClass,
		"the conversation page must stamp the fixed-shell body class")

	at := strings.Index(css, "body.ln-body--fixed {")
	if assert.GreaterOrEqual(t, at, 0, "app.css must define body.ln-body--fixed") {
		rule := css[at:min(len(css), at+220)]
		assert.Contains(t, rule, "overflow: hidden;",
			"the root scroller must be switched off on the conversation page")
		assert.Contains(t, rule, "overscroll-behavior-y: none;",
			"a flick past the end of the transcript must not rubber-band the page")
	}
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

	// Exactly one scrollbar on screen (owner 2026-08-01). This scroller only
	// switches panels — each child is one scrollport tall — so its scrollbar
	// is hidden and the panel you are reading shows the only visible one.
	// Hiding it does not disable scrolling.
	assert.Contains(t, css, ".conv-body { scrollbar-width: none; }",
		"the panel-switching scroller must not show a second scrollbar")
	assert.Contains(t, css, ".conv-body::-webkit-scrollbar",
		"the WebKit half of hiding the panel-switcher's scrollbar is missing")

	// The old scroll-hint button was a second control for what the bottom
	// bar's "Show Conversation" does, and was removed for that reason.
	assert.NotContains(t, html, `id="convScrollHint"`,
		"the scroll hint duplicated Show Conversation and must stay gone")
}

// TestBottomBarControlsAreNotDuplicatedAbove: whatever the bottom bar carries
// must not also appear on the panel above it. Now that the bar is shown at
// every width (owner 2026-08-01), that is a top-level rule rather than a
// media-query one — but it is still a hide, not a deletion: settings.mjs and
// conversation.mjs bind the rail's copies by id.
func TestBottomBarControlsAreNotDuplicatedAbove(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	css := readAsset(t, "static/css/app.css")

	assert.Contains(t, html, `class="conv-rail__nav"`,
		"the rail nav must survive in the markup (JS binds it)")
	assert.Contains(t, html, `id="micSensGroup"`,
		"the rail mic line-up must survive in the markup (JS binds it)")

	assert.Contains(t, css, ".conv-rail__nav,\n.conv-miclineup { display: none; }",
		"History/Memory/Downloads and the mic line-up live on the bottom bar at every width")

	mobileAt := strings.LastIndex(css, "@media (max-width: 900px)")
	if assert.GreaterOrEqual(t, mobileAt, 0, "the mobile media block is missing") {
		assert.NotContains(t, css[mobileAt:], ".conv-rail__nav,",
			"the hide is top-level now; a mobile-only copy would imply desktop still shows them")
	}
}

// TestConversationOverlayContract: the overlay takes the whole screen above
// the bottom bar and is toggled by that bar's own button (owner 2026-08-01).
// Both facts force it to be a NON-MODAL dialog laid out in flow — a modal
// would inert the bar and strand the Hide half of the toggle — so what
// showModal() used to supply is pinned here in its new form.
func TestConversationOverlayContract(t *testing.T) {
	html := readAsset(t, "templates/pages/conversation.html")
	css := readAsset(t, "static/css/app.css")
	js := readAsset(t, "static/js/conversation.mjs")

	assert.Contains(t, html, `<dialog class="conv-overlay" id="conversationOverlay"`,
		"the conversation overlay must stay a <dialog>")
	for _, want := range []string{
		`id="conversationOverlayClose"`,
		`id="conversationOverlayHide"`,
		`id="conversationOverlayBody"`,
		`id="convOverlayToggleLabel"`,
		`aria-labelledby="conversationOverlayTitle"`,
		`id="conversationOverlayTitle"`,
	} {
		assert.Containsf(t, html, want, "the conversation overlay must keep %s", want)
	}

	// In flow inside .conv-app and ahead of the bar: that is what makes it
	// stop exactly where the bar starts without measuring the bar.
	overlayAt := strings.Index(html, `id="conversationOverlay"`)
	barAt := strings.Index(html, `id="convBottomBar"`)
	if assert.GreaterOrEqual(t, overlayAt, 0) && assert.GreaterOrEqual(t, barAt, 0) {
		assert.Less(t, overlayAt, barAt,
			"the overlay must precede the bottom bar inside .conv-app")
	}

	// Non-modal, plus the two things that costs us, supplied by hand.
	assert.Contains(t, js, "convOverlay.show()",
		"the overlay must open non-modally so the bottom-bar toggle stays live")
	assert.NotContains(t, js, "convOverlay.showModal()",
		"showModal() would inert the bar and strand the Hide toggle")
	assert.Contains(t, js, "'Hide Conversation' : 'Show Conversation'",
		"the bottom-bar button must flip between Show and Hide")
	assert.Contains(t, js, "e.key === 'Escape' && convOverlay.open",
		"a non-modal dialog does not close on Escape by itself")

	// Full screen, with nothing left behind it to scroll.
	assert.Contains(t, css, "dialog.conv-overlay[open] { display: flex; }",
		"the overlay must fill its flex slot when open")
	assert.Contains(t, css, ".conv-app.is-overlay-open .conv-body { display: none; }",
		"hiding the panel behind is what prevents scroll-through and a second scrollbar")
	assert.Contains(t, css, "overscroll-behavior: contain;",
		"the overlay body must not chain its scroll anywhere else")
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

// TestEdgeTabsSitInTheUpperLeft: Settings and Help are two ~40px tabs stacked
// in the upper-left corner at EVERY width (owner 2026-08-01), replacing the old
// vertically-centred 40dvh bars on the right edge — which were tall enough to
// paint over the transcript's right-hand bubbles. At 40px there is no room for
// the rotated word, so each tab is icon-only with a native `title` tooltip.
func TestEdgeTabsSitInTheUpperLeft(t *testing.T) {
	css := readAsset(t, "static/css/app.css")
	html := readAsset(t, "templates/pages/conversation.html")

	assert.Contains(t, css, "--ln-edge-tab: 40px;",
		"the owner-specified 40px tab height must stay a named token")

	at := strings.Index(css, ".conv-settings-tab {")
	if assert.GreaterOrEqual(t, at, 0, "the edge tab has no layout rule") {
		tab := css[at:min(len(css), at+900)]
		assert.Contains(t, tab, "left: 0;", "the tabs live on the left edge")
		assert.Contains(t, tab, "top: var(--ln-sp-4);", "the tabs live at the TOP of that edge")
		assert.Contains(t, tab, "height: var(--ln-edge-tab);", "the tab is 40px tall")
		assert.Contains(t, tab, "width: var(--ln-touch);",
			"the tab must keep its 44px-wide touch target")
		assert.NotContains(t, tab, "40dvh", "the full-height bar is gone")
	}

	// Help is stacked one gap directly BELOW Settings, so the two read as one
	// cluster at any viewport height.
	assert.Contains(t, css,
		"top: calc(var(--ln-sp-4) + var(--ln-edge-tab) + var(--ln-sp-2));",
		"the Help tab must sit one gap below the Settings tab")

	// Icon-only: the label is hidden and the tooltip carries the word.
	assert.Contains(t, css, ".conv-settings-tab__label { display: none; }",
		"a 40px tab has no room for the rotated label")
	assert.Contains(t, html, `title="Settings"`, "the gear tab needs a hover tooltip")
	assert.Contains(t, html, `title="Help"`, "the ? tab's tooltip must read Help")

	// The in-drawer close bar mirrors to the upper right, so it can never sit
	// under the opener it replaces; the badge hangs off the tab's outer side.
	assert.Contains(t, css, ".conv-settings-tab--close {\n  left: auto;\n  right: 0;",
		"the in-drawer close bar must mirror to the right edge")
	assert.Contains(t, css, "position: absolute; top: -8px; right: -8px; left: auto;",
		"the suggestion badge must hang off the tab's outer (right) side")
}

// TestNewConversationSaysNEW: the + glyph was replaced by the word, with the
// same id and the same accessible name — and, since 2026-08-01, on the LEFT of
// the orb row and 15px below the orb's bottom edge.
func TestNewConversationSaysNEW(t *testing.T) {
	html := readAsset(t, "templates/partials/audio_viz.html")
	css := readAsset(t, "static/css/app.css")

	at := strings.Index(css, ".ln-orb-newconv {")
	if assert.GreaterOrEqual(t, at, 0, "the new-conversation button has no layout rule") {
		rule := css[at:min(len(css), at+260)]
		assert.Contains(t, rule, "left: 0;", "NEW moved to the left of the orb row")
		assert.Contains(t, rule, "right: auto;", "the old right anchor must be cleared")
		assert.Contains(t, rule, "bottom: -15px;", "NEW sits 15px below the orb's bottom edge")
	}

	assert.Contains(t, html, `id="newConversationBtn"`,
		"the new-conversation button keeps its id (conversation.mjs binds it)")
	assert.Contains(t, html, `<span aria-hidden="true">NEW</span>`,
		"the new-conversation button must read NEW")
	assert.NotContains(t, html, "＋",
		"the + glyph must be gone from the new-conversation button")
	assert.Contains(t, html, `aria-label="New conversation"`,
		"the accessible name stays the full phrase, not the shorthand")
}

// TestIoTOriginIsInTheCSP (§6 WS-3 M3.2): the browser opens an MQTT-over-
// WebSocket connection to the account's IoT ATS endpoint for the cross-device
// change fan-out. Without this origin in connect-src the socket never opens,
// and the failure is a CSP violation in the console rather than a connect
// error the client can report — which is exactly the kind of thing that gets
// diagnosed as "the feature is broken" instead of "a header is missing".
func TestIoTOriginIsInTheCSP(t *testing.T) {
	const origin = "wss://a17oe0gnthrosw-ats.iot.us-east-1.amazonaws.com"
	assert.Contains(t, pageCSP, origin, "the IoT data endpoint must be an allowed connect-src")

	// It has to land inside connect-src, not merely somewhere in the policy —
	// the same trap pageCSPWith exists for.
	connectAt := strings.Index(pageCSP, "connect-src")
	require.GreaterOrEqual(t, connectAt, 0)
	end := strings.Index(pageCSP[connectAt:], ";")
	require.Greater(t, end, 0)
	assert.Contains(t, pageCSP[connectAt:connectAt+end], origin,
		"the origin must be inside the connect-src directive")
}

// TestAutoNudgeGuards (§6 WS-3 M3.4). An unprompted voice is the most
// intrusive thing this app does, and the owner chose it over the two quieter
// options with the cross-device echo risk stated. These are the three guards
// that make that choice survivable, and each is one line that a refactor could
// drop with nothing else failing.
func TestAutoNudgeGuards(t *testing.T) {
	js := readAsset(t, "static/js/conversation.mjs")
	events := readAsset(t, "static/js/liveevents.mjs")

	// 1. Never speak over the assistant's own turn.
	assert.Contains(t, js, "state === MicState.THINKING || state === MicState.SPEAKING",
		"a nudge must be held while the assistant is mid-turn")
	assert.Contains(t, js, "flushPendingNudge()",
		"a held nudge must be delivered once the turn finishes")

	// 2. Never announce this device's OWN edit — the comparison is against the
	//    actor id the SERVER said it would stamp, not a locally derived one.
	assert.Contains(t, events, "ev.actorDeviceId === creds.actorDeviceId",
		"a device must ignore its own changes")

	// 3. No live session means no voice; it degrades to a toast.
	assert.Contains(t, js, "if (!isLive()) {", "with no session a nudge must not try to speak")

	// The Last Will is what makes presence self-clearing when a tab crashes.
	assert.Contains(t, events, "will: { topic: creds.presenceTopic",
		"presence must be cleared by an MQTT Last Will")

	// The credential is short-lived; a reconnect must fetch a FRESH one rather
	// than replay the expired token.
	assert.Contains(t, events, "reconnecting with a fresh credential")
}
