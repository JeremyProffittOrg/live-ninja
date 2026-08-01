# Update report — mobile conversation shell (2026-08-01)

Commit `bb488e3` — "Give the conversation page a real phone-and-tablet shell", pushed to
`main`, which is this repo's only deploy trigger (`deploy.md`).

---

## What "the mobile app" turned out to be

The repo ships two mobile surfaces, and the request is specific about which one it means:

| Surface | What it is | Touched? |
| --- | --- | --- |
| `web/` conversation page at phone/tablet widths | Go `html/template` + vanilla ES modules + hand-written CSS, served by `cmd/web` behind the Lambda Web Adapter | **Yes — this is the whole change** |
| `android/` app | Native Jetpack Compose (`ui/screens/ConversationScreen.kt`, `ui/LiveNinjaRoot.kt`), its own bottom `NavigationBar`, its own realtime session | No |

The requirements are written in web terms throughout — "44×44 **CSS** pixels", "max-height
~**80vh**", "a `<select>`", "**html2canvas** or equivalent", "**local storage**" — and every
control they name to move or replace exists on the web page and only there: the Help/Settings
edge tabs, and the `＋` new-conversation glyph. The Android app has no `＋` and already has a
native History/Memory/Files bottom bar. So the target is the web page, and no APK was rebuilt.
The test tablet picks the change up by loading `https://live.jeremy.ninja/conversation`.

Files changed:

- `web/templates/pages/conversation.html` — bottom bar, conversation overlay, tag dialog, tools row, scroll hint, Help copy
- `web/templates/partials/audio_viz.html` — `＋` → `NEW`
- `web/static/css/app.css` — appended `MOBILE CONVERSATION SHELL` section (~250 lines, additive)
- `web/static/js/conversation.mjs` — appended `MOBILE CONVERSATION SHELL` block + two small edits above it
- `internal/webapp/mobile_shell_ui_test.go` — new drift guard (6 tests)
- `internal/webapp/help_drawer_ui_test.go` — four new required Help entries

---

## Requirement by requirement

### 1. Help & Settings to the left, ~25–30% smaller

At `≤900px` the fixed edge tabs flip to `left: 0`, their border radius and border side mirror,
and the glyph goes **22px → 16px (−27%)**. The bar keeps `width: var(--ln-touch)` (44px) and is
tens of dvh tall, so the tap target is untouched — measured in-browser at **44 × 189** (Help)
and **44 × 472** (Settings). The drawer's in-panel close bar mirrors to the right edge and
`.conv-drawer__inner`'s reserved gutter follows it, and the settings suggestion badge moves from
`left: -8px` to `right: -8px` so it still hangs off the tab's outer side.

**One thing this surfaced:** with the tabs on the left and no 360px rail column to hide behind,
the Settings bar landed on top of the rail's *Mic Test* button. Both panels now reserve the
tab's width as a left gutter (`.conv-rail`, `.conv-main`). Caught in the browser, not in review.

### 2. Scroll up reveals a scrollable conversation

`.conv-body` at `≤900px` becomes a single-column scroller with `scroll-snap-type: y mandatory`
and `grid-auto-rows: 100%`, so the rail and the transcript are each exactly one scrollport tall
and the transcript is one swipe below the voice panel. Both panels keep their own overflow, so
native scroll chaining does the return trip: run out of transcript scrolling back down and the
outer scroller takes over and snaps you to the voice panel. Verified: `scrollHeight` 2238 =
2 × `clientHeight` 1119 — exactly two panels, no third.

Swiping is not an accessible control, so a **Conversation ⌄** button under the rail's cluster
scrolls the same container (honouring `prefers-reduced-motion`). Verified it lands at
`scrollTop == clientHeight`.

### 3. Conversation overlay

`Show Conversation` (bottom-left) opens `<dialog id="conversationOverlay">.showModal()`. That is
where the semi-transparent scrim (`::backdrop`), the focus trap, Escape, and the
prevent-scroll-through come from — the page behind a modal dialog is inert. The body is capped
at `80dvh`, scrolls on its own, and adds `overscroll-behavior: contain` so a flick reaching
either end doesn't chain out either. Dismiss via the ✕, via **Hide Conversation**, via Escape,
or by clicking the scrim.

The body is a **live mirror, not a move**: `#transcript` is cloned in on open and re-cloned on
mutation (coalesced to one re-clone per animation frame), so `transcript.mjs` keeps owning the
real subtree and turns that arrive while the overlay is up still appear. Buttons inside cloned
tool cards are stripped — a duplicate "Details" control would be dead. Verified: 16 bubbles
mirrored, 0 buttons, `overscroll-behavior-y: contain`, `max-height: 944px` (80dvh of 1180).

### 4. Bottom bar

A `flex: none` row inside `.conv-app`, so it takes its height out of the scroller above rather
than floating over it, with `env(safe-area-inset-bottom)` padding. Hidden above 900px, where the
rail already carries these controls and a second copy would be noise.

- **Show Conversation** — opens the overlay (`aria-haspopup="dialog"`, `aria-controls`, `aria-expanded`)
- **History / Memory / Downloads** — icon + real text label (the label is the accessible name)
- **Audio** `<select>` — `auto / low / medium / high / mic test`

**Narrow phones (≤520px).** At 390px the first version of the bar didn't fit: "Show Conversation"
truncated and the Audio select clipped. Below 520px the three destination links go icon-only —
their `aria-label` (which duplicates the visible text on purpose) becomes the accessible name —
and *Show Conversation* keeps its label, because it is the one control the spec asks to be
clearly labelled. Re-measured at 390px: bar `scrollWidth == clientWidth == 375` (no horizontal
overflow), items 107×54 / 44×54 / 44×54 / 44×54, select 104×44 — every target ≥44×44.

On the audio picker, two decisions worth stating plainly:

- There is **no audio-quality setting in this product**. The nearest real thing — and the one the
  rail's own `Low / Med / High` chips write — is `turnDetection.micEagerness`, mic *pickup*
  sensitivity. The select writes that, through the same optimistic `saveQuickSwitch` path, and
  `syncMicChips()` now drives both so the chips and the select can never disagree. It is labelled
  **Audio** rather than "Audio quality", and the Help panel says in so many words that it is the
  same setting as *Mic pickup*. Calling it quality in the UI would have been a lie.
- **`auto` is a fifth option** because it is the schema default. The four the spec names are all
  present; without `auto` the select could not display the state a fresh account actually ships in.
- **Mic test** is an action parked in a value list: it opens the existing `#micTestDialog` and
  snaps the select back to the stored setting, so it never sits displaying a state nothing holds.

Verified in-browser: choosing *Mic test…* opened `#micTestDialog` and the select returned to
`auto`; choosing *high* attempted the save and — with no backend behind the local preview —
reverted with the standard error toast, which is the revert path working.

### 5. Copy and Screenshot

Both live in a tools row over the transcript **and** in the overlay, bound by
`data-conv-action` so the two copies share one handler each and cannot drift apart.

- **Copy** serialises the rendered transcript to `Role (time): text`, blank-line separated,
  through the page's existing `copyText()` (clipboard API with an `execCommand` fallback).
  Verified output: `You (9:01 AM): What is the weather?\n\nLive Ninja (9:01 AM): Seventy-two and clear.`
- **Screenshot** paints the conversation to a 2D canvas and downloads a PNG.
  **Not html2canvas**: this page has no bundler and no third-party script tags, and the transcript
  is plain text plus role labels, so a direct canvas render is smaller and exact — and nothing
  leaves the device. Colours are read from the live CSS custom properties, so the image matches
  the user's theme. Verified: a **118,662-byte `image/png`, 760 × 932**, filename
  `live-ninja-conversation-2026-08-01-12-03-20.png`, and the rendered image was eyeballed —
  title, timestamp, rule, `Role · time` headers, correctly wrapped body text.

  **Changed during verification:** the first version tried `navigator.share({files})` on mobile
  first. In Chromium `navigator.canShare({files})` returns `true` and `share()` then never
  settles without a user-activation gesture — the button produced no image, no download, and no
  error. It is now an unconditional download; the OS share sheet is one tap from there.

### 6. Tag for review

A button in both tool rows opens `<dialog id="reviewTagDialog">` with a required note. Saving
writes `{conversationId, note, at}` to `localStorage['ln.reviewTags']`, newest first, capped at 50.
The conversation id is the live session id, falling back to the last session seen on the page —
because the natural moment to tag a conversation is right after it goes wrong, when the session
has already closed.

**It is local on purpose.** There is no server-side review queue in this repo to post to, and a
`POST` that quietly went nowhere would be worse than an honest local record. The Help panel says
where the note lives and points at Copy/Screenshot as the way to carry it off the device. If a
review queue is wanted, that is an API + table, not a follow-up tweak.

Verified: dialog opened, note saved, dialog closed, toast shown, and
`localStorage['ln.reviewTags']` held
`[{"conversationId":"","note":"It misheard the street name twice.","at":"2026-08-01T12:03:58.544Z"}]`
(empty id is correct — the local preview has no realtime session).

### 7. `＋` → `NEW`

Same position (right edge of the orb row), same `id="newConversationBtn"`, same handler, same
`aria-label="New conversation"`. The round icon button becomes a pill wide enough for the word;
height stays `var(--ln-touch)`.

---

## Verification

**Automated** — `go build ./...`, `go vet ./...`, `go test ./...` all pass.
New `internal/webapp/mobile_shell_ui_test.go` (6 tests) pins the bottom bar and its five audio
options, the snap-scroller declarations, the overlay dialog + `autofocus` + `overscroll-behavior`
+ `80dvh`, the exact count of each `data-conv-action` (2 apiece) and its handler, the absence of
any `html2canvas` reference, the review-tag storage key, the left-edge tab rules including the
16px glyph, and the `NEW` label. `help_drawer_ui_test.go` now also requires the four new Help
entries, so shipping one of these features without its help copy fails the suite.

**In a browser** — the page was rendered from the real embedded assets and driven at **800 × 1180**
(tablet) and **1440 × 900** (desktop). Everything quoted above as "verified" was measured there.

Desktop regression check at 1440px: bottom bar `display: none`, scroll hint `display: none`,
Settings tab still flush to the **right** edge with a **22px** glyph, `.conv-body` still
`360px 1080px` with `overflow-y: visible` (no snap scroller), rail padding unchanged. The only
change visible on desktop is the tools row, which is intentional — it is what makes Copy,
Screenshot and Tag reachable on a machine that has no bottom bar.

## A regression I caused, and fixed

Run 30699017769's `web-quality` job (Playwright + axe + Lighthouse) came back **failed**. It is
`continue-on-error: true` in this repo, so the run's own conclusion was still `success` — but that
job had been **green on the two runs immediately before mine**, so the failure was mine, not noise.

`tests/web/specs/settings-accordion.spec.mjs:165`, under the `mobile-chrome` project (Pixel 7,
412px), asserted the settings opener was flush to the **right** edge:

```
expect(openerBox.x + openerBox.width).toBeCloseTo(viewport.width, 0);
  Expected: 412   Received: 44
```

Which is exactly the change: at ≤900px the tab is now at `x: 0, width: 44`. The spec was pinning a
contract this work deliberately changes, so the spec was wrong, not the CSS. It now asserts what is
actually intended — the two bars are the same size and sit on **opposite** edges at the same
height, opener-right/close-left on a computer and opener-left/close-right on a phone — and is
viewport-aware rather than unconditional. Full local Playwright run afterwards: **71 passed,
21 skipped, 0 failed** across both projects.

The second failure in that log (`device-settings.spec.mjs:89`, a device-ID rotation assertion) was
reported by Playwright itself as **flaky** — it passed on retry, and it is untouched by this work.

## Deploy

Three pushes to `main`, which is this repo's only deploy trigger:

| Commit | Run | `deploy` | Run conclusion |
| --- | --- | --- | --- |
| `bb488e3` mobile shell | 30699017769 | success | success (`web-quality` failed — see above) |
| `5ee4cc2` 390px bottom bar | 30699237771 | success | success |
| `e5baa39` edge-bar spec fix | 30699422933 | success | **success, `web-quality` back to green** |

No local deploy was used at any point; `deploy.md`'s single path (push → GitHub Actions → OIDC)
was the only mechanism. The second push was held until the first run's `deploy` job had finished
so two CloudFormation deploys could not overlap.

## Test tablet — verified on the deployed build

The Galaxy Tab S9 FE (`R52XC06P9KJ`, Android 16, 1440×2304) is attached over adb. It is not a
separate build target: the web app *is* the deliverable, so "deploying to the tablet" means the
tablet loading the deployed page, with no install step. Signed in via Samsung Internet 30
(Chromium 143), on `https://live.jeremy.ninja/conversation`, against the shipped build:

- **Layout** — Help and Settings tabs on the **left** edge with the small glyphs; **NEW** beside
  the orb; the **Conversation ⌄** hint; the bottom bar with Show Conversation, History, Memory,
  Downloads and the Audio picker. Screenshotted.
- **Swipe-up reveal (req. 2)** — a real touch swipe snapped the page from the voice panel to the
  transcript panel, showing the tools row, the transcript and the composer with the bottom bar
  still pinned. This is the actual gesture, on the actual device.
- **Bottom-bar navigation (req. 4)** — pressing *History* navigated to `/history`.
- **Audio picker binding (req. 4)** — on the live settings document the select reads `high` and
  the rail's `High` chip reads `aria-pressed="true"` **at the same time**. The two controls agree
  against real server state, which is the thing worth proving.
- **Conversation overlay (req. 3)** — opened over the app with its dimmed backdrop, the
  Copy/Screenshot/Tag row, the empty-state line and *Hide Conversation*;
  `max-height: 922.97px` (80dvh of this viewport) and `overscroll-behavior-y: contain` measured
  on the device. Closed cleanly.

## A pre-existing hazard this surfaced (not fixed — needs your call)

On the first tablet load, **nothing driven by JavaScript worked** — not the new overlay, and not
the Settings or Help drawers that have been shipping since July. Reading the device's real console
over the Samsung Internet DevTools socket gave the cause in one line:

```
Uncaught SyntaxError: The requested module './wakeword.mjs' does not provide
an export named 'applyWakeWordSettings'
  @ https://live.jeremy.ninja/static/js/conversation.fe215b7cbdb9.mjs:40
```

A module-linking failure kills the **whole** of `conversation.mjs`, which is why every button on
the page was inert while plain `<a>` links and CSS scrolling still worked.

The origin was fine — `curl https://live.jeremy.ninja/static/js/wakeword.mjs` has the export, and
`fetch(..., {cache:'reload'})` on the device confirmed the network copy has it. The tablet was
holding a **stale cached `wakeword.mjs`**. Forcing a revalidate and reloading cleared it
permanently, and everything above was then verified.

The structural reason is worth your attention, because it is not specific to this change:

> `conversation.mjs` is loaded by its **fingerprinted, `immutable`** URL, but it imports its
> siblings by **logical** path (`./wakeword.mjs`). So any deploy that changes `conversation.mjs`
> mints a new, guaranteed-fresh URL for it while its siblings keep URLs a browser may still have
> cached from an older deploy. If the new module expects an export the cached sibling predates,
> the page dies completely — silently, with no visible error.

That makes **every** future `conversation.mjs` change a coin-flip for any client holding an old
sibling. Mine happened to land on this tablet.

I have not changed the caching or the service worker: this is the asset/module strategy, the
repo has explicit rules about not altering cache behaviour without diagnosing it first, and it is
well outside the seven requested items. The fix worth considering is making the import specifiers
resolve to fingerprinted URLs too (an import map stamped by `assets.go`, so a module graph is
always internally consistent), which would remove the failure mode rather than paper over it.

---

## Audio verification request (drafted, not sent)

This machine's speakers do not work, so the two audio-dependent checks — that the Audio picker
actually changes how quickly Live Ninja takes a turn, and that *Mic test…* shows a live level —
need someone with working audio. The spec allowed either sending this or drafting it here with a
placeholder recipient; it is drafted, because no recipient was named and this run was unattended,
and mailing an unnamed party is not a call to make without an addressee.

> **To:** `<recipient>`
> **From:** live-ninja@jeremy.ninja
> **Subject:** 5-minute audio check on the new Live Ninja mobile conversation screen
>
> Hi —
>
> A mobile shell for the Live Ninja conversation screen went out today
> (commit `bb488e3`). Everything visual is verified, but the machine that built it has no
> working speakers, so two things need ears and a microphone. It should take five minutes.
>
> On a phone or tablet, open **https://live.jeremy.ninja/conversation** and sign in.
>
> 1. **Audio picker** — bottom right of the screen, the dropdown reading *Audio: Auto*.
>    Set it to **Low**, start a conversation, and pause mid-sentence: it should wait for you.
>    Then set it to **High** and pause the same way: it should jump in sooner. Confirm the
>    setting sticks after a reload, and that changing it mid-conversation says it applied to
>    the conversation you're in.
> 2. **Mic test** — same dropdown, last entry, **Mic test…**. The microphone-check dialog
>    should open, ask for microphone permission, and show a meter that moves when you speak.
>    Close it and confirm the dropdown snaps back to whatever level you had set.
>
> Also worth a glance while you're there: tap **Show Conversation** at the bottom left and
> confirm the assistant keeps speaking normally with the overlay open.
>
> Reply with what you saw — especially if either dropdown behaviour felt identical between
> Low and High, which would mean the setting isn't reaching the session.
>
> Thanks.

---

## Deliberately not done

- **No Android APK change.** The native app is a separate surface with its own Compose shell; see
  the table at the top. Porting these seven items into Compose is a different piece of work.
- **No server-side review queue.** Tag-for-review is local, and says so.
- **No "Show Conversation" button on desktop.** The conversation is permanently on screen there;
  the overlay would be a worse view of something already visible. Copy/Screenshot/Tag are still
  reachable on desktop through the tools row.

- ~~**No caching / service-worker change.**~~ **Superseded** — the owner asked for the fix the same
  day. See "The hazard, fixed" below.

---

## The hazard, fixed — fingerprinted import specifiers (`63d8e2c`)

The owner asked for the fix, so it shipped. `internal/webapp/assets.go` now stamps an import map
into every page mapping all 24 `/static/**/*.mjs` to their fingerprinted twins. The specifier stays
`./wakeword.mjs` in the source; the browser resolves it to `/static/js/wakeword.<hash>.mjs` before
fetching. A cache hit on a content-addressed URL is by construction the bytes the importer was built
against — which is what finally makes `sw.js`'s "serving cached is always safe" true rather than
merely asserted.

Three constraints shaped it:

- **CSP forbids inline scripts** (`script-src 'self' 'wasm-unsafe-eval'`, with a test pinning the
  absence of `'unsafe-inline'`), so `script-src` carries the **sha256 of the map's exact bytes**;
  map and hash are built from the same buffer. External import maps were removed from the spec and
  are implemented nowhere, so a hash was the only route. `pageCSPWith()` splices it *inside*
  `script-src` — appending to the policy string would have landed it in `frame-ancestors`.
- **Import maps do not cover worklets**, so `wakeword-worklet.js` stays logical. It imports nothing,
  so it cannot fail to *link* — out of scope, not overlooked.
- **Browsers without import-map support** ignore the element and use the logical specifier, i.e.
  degrade to the old behaviour rather than break.

Rejected: rewriting specifiers inside the module bodies. It needs transitive hashing with cycle
detection, or it silently pins clients to a consistent-but-*stale* graph.

**A regression this caused, and the test gap behind it** (`1d4e0fa`). The first cut mapped *every*
`.mjs`, including the vendored onnxruntime module — but `template.yaml` routes `/static/vendor/*`
and `/static/models/*` to an **S3 origin**, not to this app. That bucket holds the real filenames
only, so the hashed key does not exist and S3 answers **403**:

```
403  /static/vendor/ort/ort.wasm.min.f53ed4792e75.mjs
200  /static/vendor/ort/ort.wasm.min.mjs
```

`import(ORT_MODULE_URL)` rejected, so **the wake-word engine could not start in production for
about 25 minutes.** It was caught on the tablet, not by a test.

The gap is the part worth stating: all five original guards asked *"is every module mapped?"* and
none asked *"is every mapped URL actually servable?"* — and the local harness served everything from
the Go handler, so it could not reproduce the CDN's origin split.
`TestImportMapSkipsEveryS3BackedPath` now parses `template.yaml` for behaviours whose
`TargetOriginId` is the S3 origin and requires `assets.go` to exclude each one, so a new S3-backed
behaviour fails the build rather than 403-ing in production. Final map: 22 entries, all under
`/static/js/`.

Verified against the real `NewAssets` + `NewRenderer` + `SecurityHeaders` path in Chromium:
**16/16 modules fetched from fingerprinted URLs, zero logical stragglers**; dynamic
`import('./personaeditor.mjs')` and the vendored ORT module both resolved fingerprinted; no CSP
violation. Deploy run 30700830177 — success, `web-quality` green.

## Owner corrections to the shell (`5b23986`)

1. **The bottom bar owns what it carries.** The rail's History/Memory/Downloads and its Mic Test +
   Low/Med/High line-up are hidden at ≤900px; the scroll-hint button is gone. Both rail blocks stay
   for desktop, where there is no bar.
2. **Show Conversation is the entire screen, and the same button says Hide.**
3. **Never more than one scrollbar.**

(2) has a consequence worth stating plainly: **it makes a modal dialog impossible.** `showModal()`
inerts everything outside the dialog, so the button meant to hide it again could not be pressed. The
overlay is now a non-modal `<dialog>` laid out in flow inside `.conv-app` — which also makes it stop
exactly where the bar starts without measuring the bar. Escape and focus handling are supplied by
hand, and `.conv-app.is-overlay-open .conv-body { display: none }` replaces the modal's inertness.

(3) is achieved by hiding the snap scroller's scrollbar: it is a *panel switcher*, not content —
each child is one scrollport tall, so its scrollbar only ever said "there is another panel" while
the panel being read showed its own. Hiding a scrollbar does not disable scrolling.

**Found by looking at the rendered page:** the fixed Help/Settings tabs painted over the in-flow
overlay and clipped its left edge — timestamps read "01 AM" instead of "9:01 AM". They now stand
down while it is open.

Measured at 800×1180: overlay spans 0–1119 with the bar starting at 1119; toggle, Escape and the
in-overlay controls all close it; exactly one visible scrollbar open or closed. Desktop re-checked
at 1440×900: rail controls back, bar hidden, tabs flush right at 22px, grid still `360px 1080px`.

## Acceptance checks

| # | Check | Status |
| --- | --- | --- |
| 1 | Help and settings icons on the left of the mobile toolbar, reduced size | Yes — 22px → 16px glyphs with the 44px bar retained; seen on the tablet |
| 2 | Scrolling up reveals a scrollable conversation | Yes — a real swipe on the tablet snapped to the transcript panel |
| 3 | Bottom button toggles a scrollable conversation overlay | Yes — opened and closed on the tablet; 80dvh body, scroll contained |
| 4 | Bottom bar has history/memory/downloads icons + audio dropdown with the four options | Yes, plus `auto`; picker and rail chips agree on live settings |
| 5 | Copy places text on the clipboard; screenshot produces a downloadable image | Yes — exact clipboard string, and a 118,662-byte 760×932 PNG |
| 6 | Tag for Review captures an explanation and persists it | Yes — stored record quoted above |
| 7 | New-conversation trigger displays NEW instead of + | Yes — seen on the tablet |
| 8 | App builds without errors; tests pass | Yes — `go build` / `go vet` / `go test` green; Playwright 71 passed, 0 failed |
| 9 | Deployment to test tablet attempted/completed | Yes — deployed through the pipeline and verified on the device |
| 10 | Email requesting audio testing drafted/sent | Drafted above, with a placeholder recipient and the reason it was not sent |
