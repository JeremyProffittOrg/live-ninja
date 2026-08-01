# Help section — implementation report

**Date:** 2026-07-31
**Repository:** JeremyProffittOrg/live-ninja (local working copy, branch `main`)
**Commit:** `1bf8212` — *Give the app a Help panel that opens the way Settings does*
**Delivery status:** **deployed.** Written and committed under a no-push hold (no deploy
was authorized at the time, and here a push to `main` *is* a production deploy); the owner
then authorized the push directly. Pushed as `3b662ff..7f9c4ab`, deploy run
[30686409890](https://github.com/JeremyProffittOrg/live-ninja/actions/runs/30686409890)
succeeded 2026-08-01. See §9.

---

## 1. What was asked for

A Help section that slides out in the same visual/UX pattern as the existing Settings
panel, documenting the app's capabilities; plus contributor guidance that keeps the help
content in sync as features and settings change.

## 2. What the existing Settings pattern actually is

Read before writing anything, because "mirror the Settings panel exactly" is only
meaningful once you know what the panel is:

| Piece | Where |
| --- | --- |
| Opener | `web/templates/pages/conversation.html` — `<button class="conv-settings-tab" id="settingsDrawerBtn">`, a `position: fixed` bar on the right edge, `top: 50%`, `height: 40dvh`, `translateY(-50%)` (so it spans 30dvh–70dvh) |
| Panel | `<dialog class="conv-drawer" id="settingsDrawer">` in the same file, full-viewport, content capped at 720px and centred |
| Close | `<button class="conv-settings-tab conv-settings-tab--close" autofocus>` — the mirrored bar on the left edge *inside* the dialog |
| Animation | `app.css` — `@keyframes conv-drawer-in` (`translateX(100%)` → none, 180ms ease-out), disabled under `prefers-reduced-motion` |
| Behaviour | `web/static/js/conversation.mjs` — `showModal()` supplies the focus trap, Escape, and inerting; a `click` handler closes on `e.target === dialog` (the scrim, which works because padding lives on `.conv-drawer__inner`); the `close` event restores `aria-expanded="false"` and returns focus to the opener |

Key finding: the settings drawer's *chrome* is entirely generic. Nothing in
`.conv-drawer`, `.conv-settings-tab`, or the keyframe is settings-specific. So the correct
way to "match the pattern exactly" is to **reuse those classes**, not to clone their CSS
under new names — cloning is what lets two panels drift apart six months later.

## 3. What was built

### 3.1 The Help panel

`web/templates/pages/conversation.html` — a new `HELP DRAWER` block after the settings
drawer's toast:

- `<button class="conv-settings-tab conv-settings-tab--help" id="helpDrawerBtn">` with a
  `?` icon and a "Help" label, `aria-haspopup="dialog"`, `aria-controls="helpDrawer"`,
  `aria-expanded="false"`.
- `<dialog class="conv-drawer" id="helpDrawer" aria-labelledby="helpDrawerTitle">`
  containing the same `--close` edge bar (with `autofocus`), the same
  `.conv-drawer__inner` / `__head` / `__body` structure, and an `<h2>` title.

Because it reuses `.conv-drawer`, it inherits the slide-in animation, the backdrop, the
reduced-motion opt-out, the 720px readable width cap, and the scrolling body for free —
identical to Settings by construction rather than by copy.

### 3.2 Positioning the Help tab

`web/static/css/app.css` — one new rule, `.conv-settings-tab--help`:

```css
height: 16dvh;
min-height: calc(var(--ln-touch) * 2);
top: calc(50% - 20dvh - var(--ln-sp-2));
transform: translateY(-100%);
```

`50% - 20dvh` is the Settings bar's top edge, so anchoring this bar's *bottom* edge
(`translateY(-100%)`) one spacing unit above it keeps the two stacked and aligned at any
viewport height, without hard-coding pixel offsets. `min-height` keeps it thumb-sized on
short viewports.

Plus `.conv-help__list` / `.conv-help__defs` for the prose inside — the only styling the
panel needed of its own.

### 3.3 Open/close wiring

`web/static/js/conversation.mjs` — a "docked help drawer" block immediately after the
settings drawer block, deliberately parallel to it: `showModal()` on the tab, `close()` on
the close bar, scrim-click via `e.target === helpDrawer`, and a `close` listener that
resets `aria-expanded` and returns focus to the tab. It also resets
`.conv-drawer__inner`'s `scrollTop` on open, so a long panel re-opens at the top rather
than wherever it was last left (verified — the browser does preserve the scroll offset
without this).

No state, no fetch, no settings document: the content is static markup, so this block is
open/close only.

### 3.4 Content

Six sections, written for an end user and sourced from what the code actually ships (the
nine settings accordion sections, the rail controls, the four pages, and the tool registry
in `internal/tools/`) — not from README prose:

1. **Getting started** — push-to-talk, wake-phrase hands-free mode, typing, the state
   pill, barge-in.
2. **What you can ask for** — weather/time for saved places, web lookup and research,
   remember/forget, timers and reminders, email, generated files, device control, start
   new conversation / stop listening. Closes with the *Show tool calls* pointer.
3. **Where everything lives** — History, Memory (including Guides), Personas, Downloads,
   and the two cost surfaces.
4. **Settings explained** — a `<dt>`/`<dd>` per settings section, all nine, each using the
   section's own UI title verbatim.
5. **Keyboard and navigation** — Escape, outside-click, focus behaviour, Enter.
6. **Tips and troubleshooting** — mic blocked, wake phrase not triggering, being cut off
   mid-sentence, persona changes applying next conversation, per-device settings, forgetting.

Deliberately excluded: anything gated off or unshipped. Audio storage, for instance, is a
saved-but-inert preference in this build, so the help copy does not claim it works.

### 3.5 Drift guard

New `internal/webapp/help_drawer_ui_test.go`, following the repo's existing
`*_ui_test.go` pattern of reading the shipped assets out of `web.Files`:

- `TestHelpDrawerMarkupContract` — the three element ids `conversation.mjs` resolves, the
  ARIA wiring, initial `aria-expanded="false"`, and `autofocus` on the close control.
- `TestHelpDrawerSharesSettingsDrawerChrome` — pins the class *reuse* (not a clone), so a
  future refactor that forks the CSS fails here.
- `TestHelpDrawerIsNotASettingsAccordion` — see §5.
- `TestHelpDrawerCoversTheAppsCapabilities` — every section heading, every one of the nine
  settings sections, and every page must be mentioned. **This is the test that fails when
  a feature ships without its help entry.**

### 3.6 Maintenance documentation

See §6 for the `cloud.md` question. The section itself:

- **`agents.md` → "Help section maintenance"** (authoritative): the rule, a file-path
  table (content / wiring / styling / guard), a six-item checklist keyed to the kind of
  change, an HTML entry template, and a tone guide. Explicitly instructs reuse of the
  shared drawer classes.
- **`CLAUDE.md` → "Help section maintenance"**: the rule, the four paths, the test
  command, and a link to the full checklist in `agents.md` — matching the two files'
  existing "reference each other, don't duplicate" relationship.

## 4. Verification

| Check | Result |
| --- | --- |
| `go vet ./...` | clean |
| `go test ./...` | **pass** — 23 packages ok, 0 failures |
| `go test ./internal/webapp/ -run TestHelpDrawer` | 4/4 pass |
| Build (all 12 Lambda binaries, `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 -tags lambda.norpc`, exactly as `make build` does) | all succeed |

`make` is not installed on this Windows machine, so the `build` target's loop was run
directly with the same flags rather than skipped.

**Visual/behavioural verification.** `/conversation` is behind a Login-with-Amazon
session, and no deploy was authorized, so the panel was checked by serving a static
harness (the extracted help block + the real `app.css`) over localhost and driving it with
Playwright:

- Help tab renders directly above the Settings tab on the right edge, same width, same
  border/radius treatment — confirmed at 1400×900 and 390×844, in dark and light themes.
- Clicking it opens the panel with the settings slide-in; content is readable, the body
  scrolls (3055px of content in a 900px viewport), the `<dl>` sections lay out correctly.
- Escape closes it and focus returns to `#helpDrawerBtn`.

This exercises the markup and CSS, not the `conversation.mjs` wiring in situ. **The one
thing still unverified is the panel inside the real authenticated page** — that needs a
deploy or an authenticated session, neither of which was in scope.

## 5. Problem hit and fixed

The first draft failed `TestConversationSettingsAccordionContract`
(`settings accordion has 10 triggers, want 9`). The cause: that test counts occurrences of
the string `data-settings-accordion-trigger` in the raw template, and my *explanatory
comment* in the help block mentioned the attribute by name. A comment inflated a
production contract count.

Fixed by rewording the comment, and the guard was tightened so it can't recur:
`TestHelpDrawerIsNotASettingsAccordion` now anchors on the `HELP DRAWER` block marker
rather than the dialog element, putting the leading comment in scope too.

## 6. Deviation from the brief — `cloud.md`

**There is no `cloud.md` in this repository, and no file has ever been named that.**
`find . -iname "cloud*.md"` returns nothing. The repository's contributor-instruction
files are:

- `agents.md` — instructions for all coding agents
- `CLAUDE.md` — Claude-specific notes; states *"Agent configuration is shared with
  agents.md; keep the two consistent"*
- `deploy.md` — deployment and credential policy

Rather than create a fourth instruction file that nothing reads and no convention points
at — which would guarantee the guidance is missed, defeating its purpose — the maintenance
section was written into `agents.md` and `CLAUDE.md`, the files contributors and agents in
this repo are actually directed to read.

If `cloud.md` was meant literally and refers to something outside this repository, the
section is self-contained and can be lifted across verbatim.

## 7. Acceptance checks

| # | Check | Status |
| --- | --- | --- |
| 1 | Help trigger visible near the settings trigger | Yes — same right edge, directly above it |
| 2 | Opens a slide-out with the same animation as settings | Yes — the same `.conv-drawer` element and `conv-drawer-in` keyframe, not a copy |
| 3 | Organized, accurate descriptions of capabilities | Yes — 6 sections, written from the shipped feature set |
| 4 | Dismissable, matching settings | Yes — close bar, scrim click, and Escape; focus returns to the opener |
| 5 | Maintenance section with file paths | Yes — in `agents.md` + `CLAUDE.md`; see §6 re: `cloud.md` |
| 6 | Project builds | Yes — all 12 binaries cross-compile |
| 7 | Existing tests pass | Yes — full `go test ./...` green |

Constraints honoured: no new dependencies, no new frameworks, existing classes/tokens/code
style reused throughout, no stubs or placeholders.

## 8. State on disk

Committed to `main` locally as `1bf8212`. **Not pushed.** Files touched:

```
M  CLAUDE.md
M  agents.md
M  web/templates/pages/conversation.html
M  web/static/css/app.css
M  web/static/js/conversation.mjs
A  internal/webapp/help_drawer_ui_test.go
```

No unrelated worktree changes existed before this run, and none were introduced.

## 9. Deploy (2026-08-01)

The owner authorized the push after the report above was written. Pushed
`3b662ff..7f9c4ab`; run `30686409890` finished **success** in ~7m, every job green
(`test`, `changes`, `build-nova-container`, `deploy`, `web-quality`,
`push-nova-container-bootstrap`; the two wake-word container jobs path-skipped as usual).
`web-quality` is the post-deploy Playwright gate, so the public surface and the a11y suite
passed against the deployed site.

Confirmed live on `https://live.jeremy.ninja`: the served `static/css/app.css` carries the
`.conv-settings-tab--help` / `.conv-help__*` rules and `static/js/conversation.mjs` carries
the `helpDrawerBtn` wiring.

Still outstanding, unchanged by the deploy: the panel has not been eyeballed on the real
authenticated `/conversation` page. Everything verifiable without a session is verified;
the remaining pass is an owner one.
