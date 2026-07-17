# Web Client (M3) — UI Flow Spec

**Status:** frozen for M3 implementation. This is the authors' contract for WS-D. Any
deviation (new field, new route, new control type) requires updating this file first.

**Inputs consulted:** `plan.md` M3 (§4, WS-D row §2), `contracts/api.md` (route inventory),
`contracts/settings.schema.json`, `mockups/web/01-landing-login.html`,
`mockups/web/02-conversation.html`, `mockups/web/03-wakeword-settings.html`,
`mockups/web/04-settings.html`, `internal/webapp/{api_routes,auth_routes,middleware}.go`,
`internal/store/store.go`.

**Design pass:** run inline below as four hats (UX-flow, product, accessibility, copy) per
screen, per the house "mandatory agentic design pass" rule. Findings are folded directly
into the field/state tables rather than kept as a separate transcript — this file *is* the
terse output of that pass.

**Scope note — mockups vs. this spec:** the mockups are visual/motion references only, not
literal contracts. Three deliberate departures, called out again inline where relevant:
1. Mockup voice names (Cove/Ember/Marlow/Sable) are placeholders — the real control is
   populated from `settings.schema.json#/properties/voice`'s 10-value enum via
   `GET /api/v1/realtime/voices`.
2. Mockup 04's "Assistant volume", "Speaking rate", and "Language" controls have **no
   corresponding field in `settings.schema.json`**. Adding them means an additive schema
   change, which is outside this design pass's authority. **Cut from M3.** Flagged here
   rather than silently dropped, per the "no stubs / no silent scope invention" rule.
3. Mockup 02's opening assistant bubble ("Good morning, Jeremy...") and all tool-result
   cards are demo dressing for a conversation that never happened — real empty state has
   **no fabricated transcript turns** (see §2.5).

---

## 0. Shared platform decisions (apply to all three screens)

- **Template engine:** Go `html/template` via Fiber's `html/v2` engine (not a second
  templating DSL) — `html/template`'s contextual auto-escaping is load-bearing for a page
  that embeds server-fetched settings JSON into a `<script>` block (§3.2); Fiber's default
  engine wraps it directly, no extra dependency.
- **Static assets:** fingerprinted at build (`sha256` prefix or `?v=` query on
  `/static/*`), embedded via `go:embed`; served `Cache-Control: public, max-age=31536000,
  immutable`. HTML documents: `Cache-Control: no-cache`.
- **CSP:** `default-src 'self'; connect-src 'self' https://api.openai.com; media-src 'self'
  blob:; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self' blob:; style-src 'self'
  'unsafe-inline'` (inline `<style>` blocks in the design-system CSS are pre-existing and
  static, not user data — `'unsafe-inline'` on `style-src` only, never `script-src`;
  `'wasm-unsafe-eval'` permits only WebAssembly compilation — needed for the vendored,
  SHA-256-pinned onnxruntime wake-word engine — and never enables JS `eval`; `worker-src`
  covers the AudioWorklet module and onnxruntime's blob workers). Canonical policy string:
  `internal/webapp/pages_routes.go` `pageCSP`.
- **Design tokens:** reuse the mockups' CSS custom-property system verbatim (`--ln-*`
  tokens) as the shared stylesheet `static/css/base.css` — already WCAG-AA-checked in the
  mockups (focus ring `--ln-focus`, 44px `--ln-touch` minimum, dark-first with the tokens
  structured for a future light override).
- **Light theme:** the mockups are dark-only. M3 must ship a light override for the same
  token set (per house "both themes WCAG AA" rule) even though no light mockup exists —
  this is new work, not a mockup transcription: invert `--ln-bg`/`--ln-surface*`/
  `--ln-text*` to light equivalents, re-check contrast, keep `--ln-teal`/`--ln-error`/
  `--ln-success` accents as-is (already ≥4.5:1 on a white surface at their current
  saturation — verify at implementation time, adjust lightness only if a check fails).
  `theme=system` follows `prefers-color-scheme`; `light`/`dark` set `data-theme` on
  `<html>` and override the media query, matching the artifact convention already used
  elsewhere in this codebase.
- **Reduced motion:** every animation (orb spin/pulse, visualizer bars, typing dots) is
  already gated behind `@media (prefers-reduced-motion: reduce)` in the mockups — carry
  that forward unchanged; do not add new unguarded animations.
- **Error envelope:** all `/api/v1/*` JSON errors follow the existing shape
  (`api_routes.go`): `{"error": "<code>", "message"?: "<human copy>"}`. The client's generic
  error toast falls back to a canned message keyed by `error` when `message` is absent;
  screen-specific mappings are in each section below.
- **CSRF:** any state-changing `fetch()` (settings `PUT`, tools invoke, logout) must send
  `X-LN-CSRF: <value of the __Host-ln_csrf cookie>` per `middleware.go`'s `CSRFProtect`;
  centralize this in one `apiFetch()` wrapper in `static/js/api.mjs` (reads the cookie,
  attaches the header, throws a typed error on non-2xx with the parsed envelope) — every
  screen below calls through it, never raw `fetch`.

---

## 1. Screen: Landing / Login (`GET /`)

### 1.1 Design-pass notes
- **UX-flow:** one decision only — sign in. Cut the mockup's "Sign in from a phone or
  M5Stack instead" button: there is no reciprocal backend route for a *browser tab* to
  redeem a device-pairing code (the pairing flow in `contracts/api.md` runs the other
  direction — a device registers a nonce, a human completes LWA in a *separate* browser
  leg to bind that device). Shipping the button with no working handler would be a dead
  affordance; cut it rather than stub it.
- **Product:** already-authenticated visitors must never see the marketing/login page —
  redirect server-side before render (see 1.3). Nothing else on this page needs live data.
- **Accessibility:** the whole page is static content plus one real action; the bar is
  keyboard reachability and focus order, both already satisfied by the mockup's markup
  (single tab-stop `<button>`/`<a>` elements, no custom widgets).
- **Copy:** keep the existing mockup copy verbatim (trust row, feature blurbs, legal line)
  — it is accurate and was not a placeholder. Only the CTA behavior changes (real
  navigation instead of the mockup's `setTimeout` fake).

### 1.2 Route & server-side branch
`GET /` (public, per the authorizer's public allowlist):
1. Run `ExtractAuthContext` (already global middleware). If `UserID(c) != ""` → `302` to
   `/conversation` (an authenticated visitor never sees the landing page).
2. Else render `pages/landing.html` (server-rendered, no client data fetch).

### 1.3 Fields / controls

| Element | Type | Required | Default | Data source | Behavior |
|---|---|---|---|---|---|
| "Continue with Amazon" | link styled as button (`<a href="/auth/lwa/login">`) | — | — | static | Full-page navigation (not `fetch` — the LWA flow is a server-side 302 chain that must carry real cookies). On click: swap label to "Redirecting to Amazon…" and set `aria-busy="true"` via a `beforeunload`-safe inline listener (page navigates away immediately after, so this is cosmetic only, not a real loading state). |
| "See how it works" | button, in-page anchor scroll | — | — | static | `scrollIntoView({behavior: reduced-motion ? 'auto' : 'smooth'})` to `#features`. |
| Nav "Sign in" | button, in-page anchor scroll | — | — | static | Scrolls to `#login`. |
| Marketing content (hero, features, trust row, footer) | static | — | — | static copy | No interactivity beyond the two scroll links above. |

Cut from M3 (see 1.1): "Sign in from a phone or M5Stack instead" button and its `alert()`
stub.

### 1.4 States
- **Loading:** none — fully SSR, zero client fetches before the user acts.
- **Error (LWA denies / not on allowlist / Amazon error):** handled entirely by the
  existing `htmlMessage()` full-page response in `auth_routes.go` (`callback`) — the user
  lands on a *different*, already-implemented plain page ("Access restricted", "Sign-in
  cancelled", etc.) with a "Back to Live Ninja" link, not an inline banner on this page.
  **Non-blocking polish note for a later pass:** `htmlMessage()`'s inline styles don't use
  the `--ln-*` token system yet; restyling it to match is cosmetic and out of M3's critical
  path — flagged, not required to ship M3.
- **Empty state:** n/a, page has no list/collection content.

### 1.5 Keyboard & ARIA
- Natural tab order: nav links → "Sign in" → hero CTAs → login-card button → device-code
  button (cut) → footer links. No custom widgets, no ARIA beyond what's already in the
  mockup (`role="img"` + `aria-label` on the decorative orb, which is `aria-hidden` under
  reduced motion since it conveys no state on this static page — set `aria-hidden="true"`
  outright here, since unlike the conversation screen this orb never reflects real state).
- Visible focus ring on every interactive element (already the global `:focus-visible`
  rule from §0's shared stylesheet).

---

## 2. Screen: Conversation (`GET /conversation`)

### 2.1 Design-pass notes
- **UX-flow:** the mockup conflates two *modes* under one "wake word" toggle —
  hands-free (ambient wake-word listening) vs. push-to-talk (explicit tap each turn). Kept
  as designed; it already matches the DoD's "push-to-talk + hands-free modes" line and the
  toggle's own copy ("Off — use the push-to-talk button to start a turn"). Default flips
  from the mockup's `checked` (demo-only) to **off**, per the M3 DoD: *"Optional WASM wake
  word (off by default)"*.
- **Product:** persona/voice quick-switch in the header writes to the same settings
  document as the Settings page (single source of truth, FR-S01) — no separate
  "session-only" override state to reconcile. OpenAI Realtime voice cannot change on an
  already-connected session (server constraint), so a mid-call voice change is deferred to
  the *next* connection, not applied live — surfaced honestly (§2.6) rather than silently
  ignored or faked.
- **Accessibility:** transcript is `role="log"` + `aria-live="polite"` (incremental
  text-node appends, per plan.md); the state pill is `role="status"`; the big mic control
  is a real `<button aria-pressed>`, not a `<div onclick>`.
- **Copy:** cut the mockup's fabricated opening assistant turn and both fake tool-result
  cards from the *shipped* empty state — they're demo flavor, not real history for a
  brand-new session (see 2.5). Tool-result cards remain the correct *live* rendering for a
  real `function_call_output`, just never pre-seeded.

### 2.2 Mic state machine

States: `idle`, `requesting-mic`, `connecting`, `live-listening`, `live-thinking`,
`live-speaking`, `ending`, `denied`, `error`. `live-listening/-thinking/-speaking` are one
connected "live" super-state (single `RTCPeerConnection`, single mint) — turns cycle inside
it without re-minting.

```mermaid
stateDiagram-v2
    [*] --> idle
    idle --> requesting_mic: tap mic (PTT mode) / wake-word match (hands-free)
    requesting_mic --> connecting: getUserMedia resolves
    requesting_mic --> denied: permission denied
    connecting --> live_listening: mint + SDP answer + ontrack ok
    connecting --> error: mint 402/429/5xx, SDP/ICE failure
    live_listening --> live_thinking: end-of-turn (VAD commit or PTT manual commit)
    live_thinking --> live_speaking: response audio starts
    live_speaking --> live_listening: response.done, mic reopens (VAD/hands-free)
    live_speaking --> live_listening: barge-in (speech_started while speaking)
    live_listening --> ending: user taps "End" / PTT-mode idle-grace timeout / hands-free idle-grace timeout
    live_thinking --> ending: user taps "End"
    live_speaking --> ending: user taps "End"
    live_listening --> error: datachannel/ICE drop
    live_thinking --> error: datachannel/ICE drop
    live_speaking --> error: datachannel/ICE drop
    ending --> idle: peer connection closed, tracks stopped
    denied --> requesting_mic: "Try again" after fixing browser permission
    error --> requesting_mic: "Retry"
```

**Mode-specific triggers:**

| Trigger | Hands-free mode (wake toggle on) | Push-to-talk mode (wake toggle off, default) |
|---|---|---|
| Enter `live-listening` | Local WASM wake-word match (in-tab AudioWorklet) | Tap the big mic button |
| Mid-turn tap of the mic button | Forces barge-in / re-opens mic immediately | Same — also lets a user manually end a turn early (sends `input_audio_buffer.commit` + `response.create`) before VAD fires |
| Return-to-idle after a turn | 60s keep-warm grace (session stays connected, mic track disabled, local wake-word engine resumes listening) then full teardown if no new wake | 10s keep-warm grace (covers a quick follow-up without a re-mint round trip) then full teardown |
| First-ever mic grant | Requested once, the first time the user turns the wake-word toggle on | Requested on first mic tap |

Barge-in (either mode), on `input_audio_buffer.speech_started` while state is
`live-speaking`:
1. Ramp the remote `<audio>` element's `GainNode` to 0 over ~30ms (not an abrupt cut —
   avoids a click).
2. Send `{"type": "response.cancel"}` on the `oai-events` datachannel.
3. Transition to `live-listening` immediately (don't wait for `response.cancel` ack).

### 2.3 Fields / controls

| Element | Type | Required | Default | Data source | Notes |
|---|---|---|---|---|---|
| State pill | status indicator, `role="status"` | — | `idle` | client state machine | Text + color per state; `aria-live="polite"` so a screen reader announces state changes without focus theft. |
| Listening orb | decorative animation | — | idle breathing | client state | `aria-hidden="true"` (the state pill is the real accessible status; the orb is pure decoration). Animation frozen under reduced motion (already default per §0). |
| Visualizer bars | decorative | — | flat/idle | Web Audio `AnalyserNode` on the local mic track (listening) and remote track (speaking) | `aria-hidden="true"`. |
| Push-to-talk button | button, `aria-pressed` | — | `false` | client state | Big (84px) circular control; label swaps ("Start talking" / "Stop"/"Cancel") per state; see 2.2 for tap semantics per mode. |
| Wake-word toggle | toggle switch | — | **off** | client-local preference (see below) | Enables/disables hands-free mode. Persisted in `localStorage` per browser (not a settings-doc field — `settings.schema.json` has no "wake word enabled" boolean, only which *phrase* is active). If the WASM wake-word bundle fails to load or the browser lacks `AudioWorklet` support, the toggle is hidden entirely and a one-line note replaces it: "Hands-free listening isn't available in this browser — use the mic button." (guaranteed click-to-talk fallback, per DoD). |
| Wake phrase label | static text | — | — | `settings.wakeWord` (resolved display name via the wake-word catalog) | Read-only here; editing lives on the Settings page. |
| Persona quick-switch | `<select>` | — | current `settings.persona.presetId` | `GET /api/v1/realtime/personas` | Changing it `PUT`s `persona.presetId` (clears `systemInstructions` unless already `custom`) immediately; toast confirms; see 2.6 for mid-call semantics. |
| Voice quick-switch | `<select>` | — | current `settings.voice` | `GET /api/v1/realtime/voices` | Same write pattern as persona; see 2.6 — mid-call changes apply to the *next* connection only. |
| Settings gear icon | icon button | — | — | static | Navigates to `/settings`. |
| Transcript | `role="log"`, `aria-live="polite"` | — | empty (see 2.5) | realtime events (client-side only; not fetched from history) | Incremental text-node append per plan.md — no full re-render per token. |
| Composer text input | `<input type="text">` | — | empty | — | See 2.4 fallback behavior. `label` is visually hidden (`ln-sr-only`) but present, matching the mockup. |
| Composer send button | icon button | — | disabled while empty | — | `type="submit"`, disabled state prevents empty sends. |
| Tool-result cards | definition-list card, rendered live | — | none until a real `function_call_output` arrives | `POST /api/v1/tools/invoke` responses, rendered inline in transcript order | Never pre-seeded (see 2.1/2.5). |

### 2.4 Realtime wiring (client)

1. On entering `connecting`: `GET /api/v1/realtime/session` → `{clientSecret:{value,
   expiresAt}, model, voice, sessionConfig, toolManifest}`.
2. Create `RTCPeerConnection`; add local mic track
   (`getUserMedia({audio:{echoCancellation:true,noiseSuppression:true,autoGainControl:
   true}})`); create the `oai-events` datachannel; create SDP offer.
3. `POST https://api.openai.com/v1/realtime/calls` with `Authorization: Bearer
   <clientSecret.value>`, `Content-Type: application/sdp`, body = offer SDP → response body
   = answer SDP → `setRemoteDescription`.
4. `ontrack` → attach remote stream to a hidden `<audio autoplay>` element routed through a
   `GainNode` (needed for the barge-in ramp-to-0 in 2.2).
5. Datachannel event handling:
   - `input_audio_buffer.speech_started` → if `live-speaking`, run barge-in (2.2); else
     confirm `live-listening`.
   - `response.output_audio_transcript.delta` / `response.output_text.delta` → append to
     the in-progress assistant bubble (create it on first delta of a turn).
   - `conversation.item.input_audio_transcription.*` → append/finalize the user bubble for
     the turn just completed.
   - `response.function_call_arguments.done` → `POST /api/v1/tools/invoke {tool, args,
     idempotencyKey, callId}` → on response, send `conversation.item.create` with a
     `function_call_output` item (`call_id`, `output` = JSON-stringified result) then
     `response.create` to resume.
   - Session/turn bookkeeping (seq numbers, role, text) batched and flushed to
     `POST /api/v1/transcript` every ~5s or on state transition to `ending`.

### 2.5 Empty / loading / error states
- **Empty (fresh page load):** transcript container is empty — **no fabricated turns**.
  Status text under the orb reads: *"Tap the mic or say your wake phrase to start"* (wake
  phrase text only shown when hands-free is on) or *"Tap the mic to start"* (PTT mode).
  This is real history-free-by-design for M3 (session history browsing across page loads
  is M11's Conversation History feature, out of scope here).
- **Loading (`connecting`):** state pill shows "Connecting…"; orb pulses a "waking" one-shot
  animation (already in the mockup's `wakeOrb()` pattern, minus the fake timers); mic
  button disabled during this state (no double-mint on a second tap).
- **Error states and copy:**

| Condition | Detection | Status-pill / toast copy | Recovery |
|---|---|---|---|
| Mic permission denied | `getUserMedia` rejects `NotAllowedError` | "Microphone access is blocked. Enable it in your browser's site settings, then try again." | "Try again" re-runs `getUserMedia`. |
| No mic device present | `getUserMedia` rejects `NotFoundError` | "No microphone found. Connect one and try again." | "Try again". |
| Quota exceeded (`402 quota_exceeded`) | session mint response | "You've reached today's/this month's voice limit. It resets {resetAt}." (kind-specific wording from `resp.kind`) | No retry button — informational only; composer (text) still works via `/api/v1/fallback/turn`. |
| Rate limited (`429 rate_limited`) | session mint response | "Too many requests — try again in a few seconds." | Auto-retry once after `retryAfterSeconds`, then manual "Retry". |
| Broker unreachable (`502 broker_unavailable`) | session mint response | "Couldn't reach the voice service. You can still type below." | "Retry" button; composer remains available (routes through `/api/v1/fallback/turn`). |
| SDP/ICE failure | `RTCPeerConnection` `iceconnectionstate` → `failed`, or the `/v1/realtime/calls` fetch throws | "Connection to the voice service dropped." | "Retry" restarts from `requesting-mic` (mic already granted, so this effectively skips straight to `connecting`). |
| Mid-call datachannel/ICE drop | `oai-events.onclose` / `iceconnectionstate` change during any `live-*` state | Same copy as SDP/ICE failure, transcript is preserved (not cleared) | Same retry; a fresh session is minted, transcript continues appending. |

### 2.6 Persona/voice quick-switch semantics
- Both are optimistic `PUT /api/v1/settings {settings: {...}, version}` writes (see §3.6 for
  the shared version-conflict handling — identical logic, reused, not duplicated).
- **If no live session:** takes effect on the very next connection.
- **If a live session is connected:** persona changes are safe to apply mid-call — send a
  `session.update` datachannel event with the newly-resolved persona's server-side
  instructions is *not* done client-side (anti-injection: the client never holds raw
  instructions, per `internal/realtime`'s persona-resolution rule) — instead, changing
  persona mid-call shows a toast: *"Applies to your next conversation — this one keeps
  {old persona}."* Voice changes mid-call show the same toast (OpenAI Realtime does not
  allow changing `voice` after the session's first audio response). Both selects still
  save immediately; only the toast differs from the no-live-session case ("Persona
  updated." / "Voice updated.").

### 2.7 Keyboard map

| Key | Context | Action |
|---|---|---|
| `Space` | Anywhere except while focus is in the composer input | Triggers the same action as clicking the mic/PTT button |
| `Enter` | Composer input focused | Submits the typed message |
| `Esc` | Any state | If a modal/settings panel is open, closes it; otherwise no-op (does not end a live call — ending is a deliberate click, not an accidental key) |
| `Tab` / `Shift+Tab` | — | Natural order: state pill (not focusable, `role=status`) is skipped; persona select → voice select → settings gear → mic button → wake toggle → composer input → send button |

### 2.8 ARIA notes
- State pill: `role="status" aria-live="polite"`, text content changes drive the
  announcement (no separate `aria-label` needed beyond the visible text).
- Orb: `role="img"` with a state-reflecting `aria-label` (e.g. "Listening orb, currently
  speaking") **only while it tracks real state** here (unlike the landing page's static
  orb) — still `aria-hidden` for its inner decorative rings/glow/core, the outer wrapper
  carries the one label.
- Visualizer: `aria-hidden="true"` always (redundant with the state pill).
- Mic/PTT button: `aria-pressed` reflects "mic currently open", `aria-label` restates the
  full state ("Push to talk, currently on — listening") matching the mockup pattern
  verbatim.
- Wake toggle: native `<input type="checkbox">` + visually-hidden label span, exactly as
  in the mockup.
- Transcript: `role="log" aria-live="polite" aria-relevant="additions"` on the scroll
  container; each turn is a normal text node append, not a full list re-render.
- Tool-result cards: use a real `<dl>` for the key/value pairs (already correct in the
  mockup) — never a raw object dump.

---

## 3. Screen: Settings (`GET /settings`)

### 3.1 Design-pass notes
- **UX-flow:** group by the schema's own object boundaries (wake word, persona, voice,
  turn detection, appearance, privacy, account) rather than the mockup's ungrouped list —
  this also happens to match the mockup's card-per-concern layout, so no restructuring
  needed, just re-scoping field contents to the real schema (see 3.3 vs. mockup diffs
  below).
- **Product:** every field maps 1:1 to `settings.schema.json`; nothing invented, nothing
  dropped silently. Three mockup fields have no schema backing (assistant volume, speaking
  rate, language) — cut, flagged in §0. `voiceEngine` **is** in the schema but not in the
  task's explicit field list and has no usable second option yet (`nova-sonic` ships in
  M12) — round-tripped on every `GET`/`PUT` but given **no visible control** in M3 (showing
  a single always-selected radio option violates the "don't show a field whose value is
  already determined" progressive-disclosure rule). Revisit when M12 lands a real second
  engine.
- **Accessibility:** every field already has (or gets) a persistent `<label>`; the
  wake-engine radio group discloses per-surface availability rather than hiding
  surface-inapplicable options outright, so a user understands *why* Porcupine/WakeNet are
  greyed out here instead of just vanishing.
- **Copy:** autosave replaces the mockup's explicit "Save changes" button model (matches
  house UI rule: reduce typing/clicks, real-time validation) — the save bar becomes a
  passive status line ("All changes saved" / "Saving…" / "Couldn't save — retry") instead
  of an actionable button, except for the two danger-zone actions which keep explicit
  confirm buttons (irreversible-action rule).

### 3.2 Load strategy (SSR, no loading spinner)
`GET /settings` handler calls `store.GetSettings(ctx, userID)` (new, §4) **server-side**
and renders the current document directly into the page as
`<script type="application/json" id="settings-data">{...}</script>`, alongside the fully
rendered initial control states (selected radio, slider position, etc.) computed
server-side in the template. Client JS hydrates from that inline JSON — **no client-side
`GET /api/v1/settings` fetch on first paint**, so there is no settings-page loading
skeleton state to design. Subsequent `PUT`s go through `fetch`. `GET /api/v1/settings` (the
JSON endpoint) still exists and is used by: the conversation page's persona/voice
quick-switch hydration, and this page's own conflict-recovery re-fetch (§3.6).

### 3.3 Fields / controls (every `settings.schema.json` field)

| Schema path | Control | Required | Default | Data source | Validation / error copy |
|---|---|---|---|---|---|
| `version` | hidden (not rendered) | yes | — | GET response | Never user-edited; held in a JS variable, sent back on every `PUT`. |
| `wakeWord` | combobox (searchable `<input role="combobox">` + listbox, per house rule for a small-but-growing enumerable set) | yes | `hey-live-ninja` | `GET /static/wakewords/catalog.json` (new static asset, §4 — built-in phrases only in M3; user-trained custom phrases are M6) | If the stored value isn't in the catalog (future/foreign value), show it as a disabled "Unknown (kept as-is)" option per the schema's forward-compat rule — never silently drop it. |
| `wakeEngine` | radio group, 3 options | yes | `openwakeword` | schema enum (static, finite, meaningful across surfaces) | `porcupine` row disabled, hint "Available on the Android app"; `wakenet` row disabled, hint "Available on M5Stack hardware"; only `openwakeword` selectable on web. |
| `sensitivity` | slider, 0–100 (displayed as %), stored as 0–1 float | yes | `0.5` (→ 50%) | schema `minimum`/`maximum` | No invalid range possible (native `<input type=range>` clamps); commits on `pointerup`/`change`, not every `input` tick. |
| `persona.presetId` | `<select>` | yes | `default` | `GET /api/v1/realtime/personas` (+ a client-appended literal `"custom"` option, always last) | — |
| `persona.systemInstructions` | `<textarea maxlength="4000">`, shown only when `presetId==="custom"` | conditionally (null otherwise) | `null` | user input | Live counter "N / 4000"; a paste that exceeds the limit is trimmed to 4000 with a toast ("Instructions were shortened to fit the 4000-character limit."), never a hard rejection. Selecting a non-custom preset clears this field and hides the textarea (progressive disclosure). |
| `voice` | radio group with inline preview button per row | yes | `cedar` | `GET /api/v1/realtime/voices` (10 rows, canonical enum order) | Preview button calls `POST /api/v1/fallback/tts {text: "<fixed sample line>", voice: "<row's id>"}` (existing endpoint, reused — no new backend surface needed) and plays the returned audio; only one preview plays at a time (stops any other on new click). |
| `turnDetection` | radio group, 2 options | yes | `semantic_vad` | schema enum (static) | Helper copy per option: "Semantic VAD — Live Ninja judges when you're done speaking from meaning, not just silence (recommended)." / "Server VAD — ends your turn after a fixed silence gap." |
| `theme` | segmented control, 3 options | yes | `system` | schema enum (static) | Applies instantly client-side (`data-theme` attribute + `localStorage` cache to avoid flash-of-wrong-theme on next load) *and* persists via `PUT`. |
| `micDeviceId` | `<select>` | no (nullable) | `null` ("System default") | `navigator.mediaDevices.enumerateDevices()` filtered to `kind==='audioinput'` | If device labels are empty (mic permission never granted in this browser), show one row: "Grant microphone access to see device names" — a button, not a dead dropdown — that calls `getUserMedia` once (immediately released) purely to unlock labels, then re-populates. If the previously-selected device id has since been unplugged, fall back to "System default" and show a one-time toast: "Your saved microphone isn't connected — using the system default." |
| `voiceEngine.*` | **no control in M3** (see 3.1) | yes | `{default:"openai-realtime", devices:{}}` | — | Round-tripped unedited on every `PUT` (client must echo back whatever it received on `GET`, per `additionalProperties:true` / forward-compat rule — never drop it because the UI doesn't expose it). |
| `privacy.storeAudio` | toggle | no | `false` | — | Off is the privacy-preserving default (PRD §10) — no code path in M3 currently honors "on" (no audio-store pipeline is built yet), so flip it on shows an inline note: "Audio storage isn't wired up yet in this build — this preference is saved for when it is." (Honest about current capability rather than silently pretending the toggle does something today.) |
| `privacy.storeTranscripts` | toggle | no | `true` | — | — |
| `privacy.retentionDays` | radio group, 4 options (`0/7/30/90`) | no | `30` | schema enum (static) | `0` row labeled "Don't keep transcripts" (not "0 days", clearer). |
| — (not a schema field) | "Sign out" button | — | — | — | `POST /auth/logout` (or `/api/v1/auth/logout`), then client-side redirect to `/`. |
| — (not a schema field) | "Sign out everywhere" button | — | — | — | `POST /api/v1/auth/logout-all` (`RequireAuth`) — destructive-but-recoverable (just re-login), so a lightweight `confirm()`-style inline confirmation ("This signs out every device, including this one.") is enough; does **not** need the typed-"DELETE" pattern reserved for irreversible data loss. |

### 3.4 Cut from M3 (flagged, not silently dropped)
- Mockup 04's **Assistant volume**, **Speaking rate**, **Language** controls — no
  corresponding `settings.schema.json` field. Needs a schema addition + explicit approval
  before any of these get built; not part of this design pass's authority.
- Mockup 03's full **wake-word management table** (add/edit/delete/train custom phrases,
  per-phrase sensitivity, mic test modal) — that whole surface is **M6** ("Programmable
  wake-word system"), which needs the training pipeline, `POST /v1/wakewords`, and content-
  addressed model distribution to exist first. M3 ships only a **read-select** combobox
  over the built-in catalog (`wakeWord` row above) plus the sensitivity slider for
  whichever phrase is active — no add/train/delete UI yet.
- Mockup 05 (Account & Devices full page: device table, pairing QR flow, per-device revoke)
  — separate route, separate milestone-adjacent surface; M3 only needs the two sign-out
  buttons on the Settings page per the task brief. `GET /v1/devices` /
  `DELETE /v1/devices/{id}` already exist server-side (`api_routes.go`) for whenever that
  page is built.

### 3.5 Empty / loading / error states
- **Loading:** none on first paint (§3.2, SSR-inlined). A field-level "Saving…" micro-state
  (small spinner or dimmed control) appears only on the specific field being written,
  never a full-page skeleton.
- **Error — save failure (network/5xx):** revert the optimistic UI value for that field
  back to its last-confirmed value; show a toast: "Couldn't save your changes — check your
  connection and try again." with a "Retry" action that resubmits the same `PUT`.
- **Error — version conflict (`409`):** see §3.6.
- **Empty state:** n/a — `GET /api/v1/settings` always returns a full document (defaults
  synthesized server-side if the row is absent, per the task brief), so there is never a
  genuinely empty settings page.

### 3.6 Autosave + optimistic-concurrency (shared logic, used by both Settings and the
conversation page's quick-switches)
1. On any field change, debounce 400ms (sliders/text) or fire immediately (radio/select/
   toggle/segmented — these are discrete, not continuous), then:
   `PUT /api/v1/settings {settings: <full merged document with the one field changed>,
   version: <last-known version>}`.
2. **200:** update the in-memory `version` to the response's new value; save-bar shows "All
   changes saved" with a timestamp.
3. **409 (version conflict):** another surface (Android/M5Stack) wrote first.
   Re-`GET /api/v1/settings`, diff against the in-flight local change:
   - If the *same field* also changed remotely, remote wins (last-write-wins is the
     documented reconciliation rule) — discard the local pending value, show: "Someone
     updated your settings from another device — refreshed." Re-render the affected
     control(s) from the fresh document.
   - If a *different* field changed remotely, re-apply the local pending change on top of
     the freshly-fetched document and retry the `PUT` once automatically (no user-visible
     interruption — this is the common case: two unrelated settings touched from two
     surfaces close in time).
4. Never blocks the user from continuing to edit other fields while a save is in flight —
   each field's write is independent.

### 3.7 Keyboard map
Standard form tab order top-to-bottom, section-by-section (wake word → persona → voice →
turn detection → appearance → privacy → account). Radio groups and the segmented control
use native arrow-key roving tab-index (free from `<input type=radio>`/native radio
grouping — no custom ARIA needed). Voice preview buttons are reachable via `Tab` after
their radio; `Enter`/`Space` activates. Danger-adjacent (sign-out) buttons are last in tab
order, consistent with "destructive actions subordinate" placement.

### 3.8 ARIA notes
- Every field has a persistent `<label for>`; radio/checkbox groups are wrapped in
  `<fieldset><legend>` (wake engine, voice, turn detection, retention days, theme).
- Disabled radio rows (Porcupine/WakeNet) carry `aria-disabled` plus the hint text
  associated via `aria-describedby` — never just a lower-opacity visual cue alone.
- Slider: native `<input type="range">` with `aria-valuetext` set to the formatted "72%"
  string (screen readers otherwise announce the raw 0–100 number, not the intended
  percentage framing already shown visually).
- Save-bar status line: `role="status" aria-live="polite"`.
- Delete-account confirm panel (existing danger-zone pattern, unchanged from the mockup):
  `aria-expanded`/`aria-controls` on the trigger button, focus moves into the panel on
  expand and returns to the trigger on cancel/collapse — already correct in the mockup,
  carry forward as-is.

---

## 4. New backend surface this spec requires (for the implementing agent, not prescriptive
   beyond what's already named in the task brief)

| Item | Kind | Notes |
|---|---|---|
| `internal/store/settings.go` | new file | `PK=USER#<uid>`, `SK=SETTINGS`; `GetSettings` (defaults-if-absent, voice default `cedar`), `PutSettings` (`ConditionExpression version = :expected`, `store.ErrVersionConflict` on failure mapped to `409`). |
| `internal/webapp/settings_routes.go` | new file | `GET /api/v1/settings`, `PUT /api/v1/settings`. Registered from `cmd/web/main.go` alongside `RegisterAPIRoutes`/`RegisterAuthRoutes`. |
| `GET /api/v1/realtime/voices` | new tiny handler | Returns the 10-entry `realtime.SupportedVoices` list with id + display label/description (static, no DB read). |
| `GET /api/v1/realtime/personas` | new tiny handler | Returns the persona catalog (id/name/description) the broker already resolves server-side; client never sees raw instructions for non-custom presets. |
| `static/wakewords/catalog.json` | new static asset | Built-in wake phrases only for M3 (e.g. `hey-live-ninja` default, `ok-ninja`, `ninja`) — the full user-training pipeline is M6; this file just needs to exist so the combobox in §3.3 has real, non-blind options. |

## 5. Definition of done for this spec
Every field in `contracts/settings.schema.json` has a named control, data source, default,
and validation rule above (or an explicit, reasoned cut). Every mic state in the DoD's
`idle→requesting-mic→connecting→live-listening⇄live-speaking→ending` chain plus
`error`/`denied` is transitioned into and out of by a named trigger. Every screen has a
loading, empty, and error state defined. Keyboard and ARIA behavior is specified per screen.
Implementers should treat any gap discovered during coding as a spec bug — fix this file in
the same change, don't silently improvise.
