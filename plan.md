# Plan

Consolidated by `/clean-plans` on **2026-07-24**. Single source of truth for **active work**.
Deliberately-deferred future items live in [backlog.md](backlog.md) — those are **not** scheduled
and must not be pulled in here without a decision.

Folded in from (full history + verbose implementation notes preserved in each):

| Archived plan | What it contributed |
|---|---|
| [archive/plan.md](archive/plan.md) | Master M0–M12 plan + the entire §8 implementation-notes / RESUME-STATE history. **Read §8 there before resuming anything** — it is the deepest record of how this system actually works. |
| [archive/gemini-plan.md](archive/gemini-plan.md) | M13 Gemini Flash Live — code-complete + deployed; E1/E2 live-audio verification outstanding |
| [archive/base-knowledge-plan.md](archive/base-knowledge-plan.md) | M15–M17 — **M15 shipped 2026-07-24**; M17 then M16 remain the largest block of real work below |
| [archive/tool-parity-plan.md](archive/tool-parity-plan.md) | M18–M20 — complete; only the owner live-audio smoke remains |
| [archive/android-revamp-plan.md](archive/android-revamp-plan.md) | Android v0.2.1-hal — shipped; wake-word training kickoff outstanding |

Harvested (not archived — still live documents): [docs/qa-report.md](docs/qa-report.md) (manual
verification checklist), [SETUP.md](SETUP.md) (one-time owner setup checklist).

**Status markers:** `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked.
**Model routing:** **H** Haiku · **S** Sonnet · **F** Fable (→ Opus if unavailable, never Sonnet) · **O** Opus.

---

## Where the project actually stands (2026-07-24)

The platform is **built and deployed to production**. M0–M13 are code-complete and live at
`live.jeremy.ninja`; the QA campaign found **0 blockers and 0 functional bugs** across 8 surfaces.
The Android app shipped as v0.2.1-hal and the tool manifest is single-sourced.

> **Scope decision (2026-07-24): the M5Stack Tab5 surface is OUT of this plan.** All Tab5 /
> firmware / IoT-provisioning work — including the `ProvisionIoT` hook, device pairing, OTA,
> Secure Boot, and the HIL rig — moved to [backlog.md](backlog.md). The shipped firmware still
> works (HIL-verified multi-turn voice loop); it is simply not scheduled work. Active surfaces
> are **web** and **Android**.

What is left divides cleanly into four buckets — the workstreams below:

- **WS-1 Verification** — human/mic/hardware-gated checks that no agent can run. Mostly owner work.
- **WS-2 Base Knowledge (M15–M17)** — **M15 done 2026-07-24**; M17 (tool-failure RCA) is next, then M16.
- **WS-3 Unfinished platform work** — the real code gaps (wake-word training run, deferred cleanup findings).
- **WS-4 Launch (M8)** — distribution, runbook, go/no-go.
- **WS-5 Android stability & performance** — opened 2026-07-24 from live on-device evidence. M21.0/21.1/21.4 done, 21.2 open, 21.3 UI done + detection unproven; **M23 done (16 KB verified)**; M22 perf and M24 harness open.

WS-2 and WS-3 are independent and can run in parallel. WS-1 gates WS-4.

---

## WS-1 — Verification (owner / hardware gated)

**Definition of done:** every "built but never exercised with real audio/hardware" claim in the
archived plans is either confirmed working or converted into a bug with a repro.

> These cannot be automated: the agent profile has **no microphone** (hard block, hit repeatedly
> across M12/M13/M19 verification) and the phone is physically at the owner's desk.

### 1.1 Live voice loop — web  `[ ]` (owner)
⟵ archive/plan.md §8 M14 item 12 · docs/qa-report.md "Live voice / microphone"
- `[ ]` Real voice session: mint → WebRTC connect to OpenAI → spoken turn → tool call round-trip via `POST /api/v1/tools/invoke`.
- `[ ]` Model actually **calls `memory_search`** when asked "what is my home address" (the fix deployed 2026-07-18 was never confirmed live). ⟵ qa-report Surface 5
- `[ ]` Resolved voice/accent audibly applied — Noir Detective → new-york, Josh Lyman → `ash`. ⟵ qa-report Surfaces 2/3
- `[ ]` Per-persona voice memory: two personas, switch, each speaks its saved voice; `personaPrefs` persists in DynamoDB. ⟵ qa-report Surface 4
- `[ ]` Cross-tab live apply: change mic pickup / turn detection in tab B → tab A applies mid-session via `session.update`. ⟵ qa-report Surface 4
- `[ ]` Mid-session mic-eagerness chip audibly changes end-of-turn behaviour. ⟵ qa-report Surface 4
- `[ ]` Barge-in / wake-word detection in a browser with a working mic. ⟵ qa-report Surface 8
- `[ ]` Confirm the cost-persist chain produces a **costed CONV row** (needs one live session; typed fallback turns emit no usage events). ⟵ archive/plan.md §8 M14 item 10

### 1.2 Gemini Flash Live — E1/E2  `[!]` blocked on owner (S)
⟵ archive/gemini-plan.md §4 Phase E · exact 6-step script in that file's §10 "Phase E status"
- `[!]` **E1 cross-engine parity:** pin one device to `gemini-flash-live`, one to `openai-realtime` — transcripts land in the same sink with correct `engine` tags, tools invoke identically, topics/memory extraction runs, cost priced at Gemini rates, barge-in cuts playback, persona switch changes the Gemini voice per the D4b mapping, user `geminiVoice` overrides it.
- `[!]` **E2 lifecycle:** a >10-min session survives the `goAway` recycle via resumption handle; a >30-min session re-fetches a fresh token and resumes; the quota gate still fires pre-mint.
  Notes: Android `GeminiLiveTransport` was compile-unverified when written — the later v0.2.1-hal build compiled it, so that gate is satisfied.

### 1.3 Tool-manifest live smoke (post-M19)  `[ ]` (owner)
⟵ archive/tool-parity-plan.md §Verification
- `[ ]` "Set a timer for 20 minutes" → fires; no `invalid_args` in the `LOG#` audit rows.
- `[ ]` "Set a timer for 3 days" → model hands off to `set_reminder` (one `invalid_args` row naming `set_reminder`, then a successful `set_reminder`, is the healthy shape).
- `[ ]` "What's the weather in London in celsius" → `units:metric` actually requested.
- `[ ]` "What notes do I have tagged work" → tag filter used; "read me my recent notes" with no query succeeds.
  (The `device_control` / "reboot the terminal" step from the original smoke needs a Tab5 — moved to `backlog.md`.)
- `[ ]` Repeat the first two on a `gemini-flash-live`-pinned device.

### 1.4 Authed web surfaces  `[ ]` (owner or owner-assisted browser session)
⟵ docs/qa-report.md "Requires an authenticated session"
- `[ ]` Full LWA web sign-in end-to-end → `__Host-ln_rt` cookie → `/conversation`.
- `[ ]` Android Custom-Tabs PKCE exchange (`POST /auth/lwa/exchange`) on a real device.
- `[ ]` `GET /personas` renders the grouped library (builtin/mine/shared) when authed.
- `[ ]` Persona editor round-trip: create → edit voice/accent → share → copy a shared one → `personachanged` refresh + mid-session pending banner.
- `[ ]` Settings autosave + 409 reconcile (concurrent second-device edit → remote-wins toast).
- `[ ]` `/history` authed rendering: tool-call Details disclosure, top toggle persists across reloads.
- `[ ]` `/conversation` authed runtime: drawer focus-trap/Escape, mic-sens chips live-apply, persona `<select>` populated, transcript streams, cost badge on session start.
- `[ ]` Settings **drawer** opened and exercised in a real browser — the drawer relocation was only ever statically screenshotted, never hydrated live (`initSettingsPanel`). ⟵ archive/plan.md §8 Task #8 Request 3

### 1.5 Android device  `[ ]` (owner, on the phone)
⟵ docs/qa-report.md "Device / hardware" · archive/plan.md §8
- `[ ]` Live voice round-trip capture on Android — confirm its transcript sink feeds user turns identically to web.
- `[ ]` PWA install + offline: install prompt / add-to-homescreen / real offline navigation fallback on a device.
- `[ ]` Android wake / lock-screen paths on real hardware (shipped untested; first-run checklist was in the v0.2.1-hal email).
- `[ ]` Android FRR/FAR wake-engine corpus harness gated in CI + on-device instrumented runs. **M4 DoD gap.** (S)

### 1.6 Delivery / infra spot-checks  `[ ]`
⟵ docs/qa-report.md "Delivery / infra / out-of-band"
- `[ ]` Confirm the memory-fix commit is the currently-deployed `WebFunction` version. (H)
- `[x]` Security emails delivered to the owner inbox — owner confirmed working 2026-07-19.
- `[x]` `Project`/`CostCenter` Cost Allocation Tags active — activated via `ce update-cost-allocation-tags-status`, Errors:[] (archive/plan.md §8 M0).
- `[ ]` M9/M10/M11 (deliverables, memory/guides, topics/history) exercised with **real data**, not just deployed. (S)
- `[ ]` Playwright e2e + Lighthouse/axe WCAG-AA gates wired into CI (M3 remainder, deferred into M7 and never landed). (S)

---

## WS-2 — Base Knowledge Layer + Tool-Failure RCA (M15–M17)

⟵ archive/base-knowledge-plan.md — **fully authored, never started.** That file carries the
grounded problem statement (P1–P4, each citing the real seam), the full architecture sketch for
M17, and the sequencing/cost/risk analysis. Read it before starting; the task lists below are
verbatim.

**Sequencing (locked in the source plan): M15 → M17 → M16-polish.** M15 is done; **M17 is next.** M15 kills the daily
annoyances immediately (weather, location, clock); M17 needs M15's profile + system map to write
good RCAs. Estimated: M15 one focused session, M17 one session, M16 rides along.

> **Open questions — answer before build (defaults baked in, so this is not a blocker):**
> 1. RCA email recipient stays `proffitt.jeremy@gmail.com`? *(default: yes)*
> 2. RCA daily cap 10 / cooldown 1 h per failure signature OK? *(default: yes)*
> 3. Do validation-only errors email too, or just persist + weekly digest? *(default: email — best early signal of prompt/schema drift)*
> 4. Seed the profile from the existing memory entity automatically on first deploy, or wait for the Settings form? *(default: pre-fill a pending suggestion, owner approves in Settings)*
> 5. Opus specifically, or "best available Anthropic model on Bedrock at build time"? *(default: Opus; fallback is hold-disabled, never downgrade)*

### M15 — Base Knowledge Layer  `[x]`  (built + deployed 2026-07-24)

**Definition of Done:** every minted session (web, Android, fallback turns) carries a
server-built BASE KNOWLEDGE block — identity, home location, local date/time, timezone, units,
contact email; the weather tool works with **no location argument** (profile default, straight to
coordinates, no geocode leg) and correctly resolves "City, ST" when a location *is* given; the
profile is owner-editable in the web Settings drawer and versioned like the rest of the settings doc.

- `[x]` **O** — **Profile schema** (`contracts/settings.schema.json` + `internal/store`): new `profile` section of the settings document (rides the existing optimistic-concurrency version + cross-tab sync for free): `displayName`, `pronouns?`, `homeLocation {label, postalCode?, city?, admin1?, country?, lat, lon, timezone}`, `workLocation?`, `units (imperial|metric)`, `locale?`, `contactEmail`, `quietHours?`, `notes[]` (≤200 chars each, cap ~20). Locations stored **geocode-verified** (lat/lon resolved at save time, never at question time).
- `[x]` **S** — **Server-side directive builder** (`internal/realtime/baseknowledge.go`): `BuildBaseKnowledge(profile, now)` → compact block appended in the mint chain (after `memoryUsageDirective`, before the guide suffix — same anti-injection posture: server-resolved, never client-supplied). Includes **current local date + time** from the profile timezone at mint (the model has no clock at all today). Also injected into fallback-turn completions.
- `[x]` **S** — **Tool-context defaults** (`internal/tools`): `Deps` gains the resolved profile. `get_weather.location` becomes **optional** → defaults to profile coordinates (skips geocoding entirely); `units` defaults from profile; scheduler/timer tools default to profile timezone.
- `[x]` **S** — **Weather geocoding hardening:** split "City, admin" on comma → `name=City`, `count=5`, rank candidates by admin1/country match and proximity to profile home (kills "Paris, TX → France"); pass bare postal codes through unchanged. Table-driven tests for the shapes that fail today: "Huntersville, NC", "Paris, TX", "Charlotte", "28078".
- `[x]` **S** — **Settings UI "About you"** (`conversation.html` + `settings.mjs`): name, pronouns, home/work location (typeahead against the Open-Meteo geocoder — **selection only** per house UI rules; ZIP accepted; saves resolved lat/lon+timezone, shows the resolved label back), units toggle, notes list. Server PUT validates via schema.
- `[x]` **S** — **Bootstrap from memory:** one-time assisted seed — "Suggest my profile" runs `memory_search` for home/work/name facts and pre-fills the form for confirmation (never silently copies memory → profile).
- `[x]` **H** — `contracts/api.md` + `settings.schema.json` docs; plan notes.

**Implementation notes (2026-07-24).** Built and shipped in one pass. `go build/vet/test ./...`,
`node --check` on every `.mjs`, and `sam validate --lint` all green. No new AWS resources, no IAM
changes, no new secrets — the profile rides the existing settings document.

- **Files added:** `internal/store/profile.go` (typed read view, `LoadProfile` projected GetItem,
  `ProfileFromDoc`, the shared `USStateName` table), `internal/realtime/baseknowledge.go`
  (`BuildBaseKnowledge`), `internal/tools/geocode.go` (split + rank),
  `internal/webapp/profile_routes.go` (`GET /api/v1/geocode`, `POST /api/v1/profile/suggest`,
  `validateProfile`), plus a test file for each.
- **Files changed:** `contracts/settings.schema.json` (`profile` + `$defs/profileLocation`),
  `internal/store/settings.go` (profile in `DefaultSettings`), `cmd/realtime-broker/main.go` (all
  three mint paths), `internal/realtime/fallback{,_tools}.go` (new `extraSystem` parameter),
  `internal/tools/{weather,scheduler,registry}.go`, `internal/webapp/settings_routes.go`,
  `cmd/web/main.go`, `web/templates/pages/conversation.html`, `web/static/js/settings.mjs`,
  `web/static/css/app.css`, `contracts/api.md`.
- **Composition order (a contract, test-enforced by `TestBaseKnowledgeComposesAfterSessionDirectives`):**
  persona → `SessionDirectives` (memory + silence) → **BASE KNOWLEDGE** → accent → guides.
  Applied identically on the OpenAI mint, the Gemini mint, and the text fallback turn — a degraded
  turn now knows the same facts a voice session does, which is why `Turn`/`TurnWithTools` grew an
  `extraSystem` parameter rather than the block being bolted on at one call site.
- **`time/tzdata` is a load-bearing import** in `baseknowledge.go`. Lambda's `provided.al2023`
  image ships no `/usr/share/zoneinfo`, so without it every `LoadLocation` would fail and the clock
  line would silently render in UTC **in production while passing locally**. `TestTimezoneDatabaseIsAvailable`
  guards it. Do not remove the blank import.
- **The weather fix is two independent fixes.** (1) With a home on file the model omits `location`
  entirely and the handler goes straight to the stored coordinates — **zero geocoding requests**,
  asserted in `TestGetWeatherWithNoLocationUsesProfileHome`. (2) When a location *is* given, the
  `"City, ST"` compound is split before the call (the geocoder's name index has no compound
  entries — this is exactly why "Huntersville, NC" returned nothing while "28078" worked) and up to
  five candidates are ranked: +40 admin1 match (via the US state-abbreviation table), +30 country,
  +0–9 proximity to home. Proximity deliberately can never outrank an explicit hint, so
  "Paris, France" from a Charlotte home still resolves to France.
- **Locations are pickers, not text boxes**, per the house rule. `GET /api/v1/geocode` returns
  exactly the stored shape so the client saves the selected record verbatim, and `validateProfile`
  **rejects** any location without numeric lat/lon. That rejection is what makes the geocode-free
  weather path trustworthy — accepting a coordinate-less location would quietly recreate the
  free-text field this design exists to remove.
- **`Deps.Profile` is a loader, not a value** (`internal/tools/registry.go`): `Deps` is
  process-wide while a profile is per-user, so the invocation's verified `UserID` picks the row and
  one user's profile can never leak into another's tool call. `profileFor` is nil-safe — a registry
  built without it sees the zero profile and behaves exactly as pre-M15.
- **Bug caught by its own test:** the profile-home path first passed `home.Label` as the candidate
  *name*, so `Label()` re-appended admin1/country and rendered "Huntersville, North Carolina,
  United States, North Carolina, United States". The city now feeds the composer.
- **Scheduler:** `at` accepts an offset-less local datetime interpreted in the profile timezone.
  This only became worth doing *because* the model now has a clock — it emits `2026-07-25T09:00`
  far more often than correctly-offset RFC3339, which used to be a hard `invalid_args`.
- **Not in this milestone:** the `profile_suggest` *tool* (that is M16). `POST /api/v1/profile/suggest`
  is an owner-triggered button that writes nothing, and it never auto-applies a location — only
  plain-text fields prefill; a location must still be picked so it carries real coordinates.
- **Owner action to make this live:** open Settings → **About you**, pick your home location, and
  confirm units. Until a profile exists every mint is byte-identical to before — the feature is
  inert, not half-on.

### M16 — Knowledge Refinement Loop  `[ ]`

**Definition of Done:** the assistant (and M17's RCA pipeline) can *propose* base-knowledge
additions; proposals queue as pending suggestions the owner approves/rejects in Settings; approved
ones merge into the profile with version history; nothing ever auto-writes identity/location facts
without confirmation.

- `[ ]` **S** — `profile_suggest` tool (assistant-callable, in the session manifest): proposes a field change or a new `notes[]` fact with a reason; writes a `PROFSUGG#` item (pending, TTL 30 days), **never** mutates the profile. Result tells the model "suggested — Jeremy will confirm in Settings."
- `[ ]` **S** — Suggestions UI in "About you": pending list with Approve/Reject (approve = normal versioned settings PUT); badge on the drawer tab when suggestions are pending.
- `[ ]` **O** — **Policy (locked, revisit later):** auto-apply allowed only for `units` and `notes[]` additions the owner spoke *explicitly* — and even those surface a toast + undo. Location/name/email always require Settings confirmation. Rationale: a mis-set home location silently poisons every weather/time answer.
- `[ ]` **H** — `memoryUsageDirective` updated so the model knows the split: *stable facts → profile (visible in your instructions); episodic facts → memory tools*.

### M17 — Tool-Failure Agentic RCA (Bedrock Opus → email)  `[ ]`

**Definition of Done:** when a tool invocation ends `outcome=error` in prod, an automated RCA runs
within ~1 minute: pulls the failing call + surrounding conversation window + the tool's contract,
asks **Claude Opus on Bedrock** for a structured RCA, emails the report to the owner, and files any
base-knowledge / code-fix suggestions into the M16 queue. Deduped, rate-capped, off the request path.
Full architecture diagram in [archive/base-knowledge-plan.md](archive/base-knowledge-plan.md) §M17.

- `[ ]` **S** — SQS `live-ninja-rca` + enqueue in `Registry.finish` on `outcome=error` (include `CodeNotFound`/`CodeUpstreamError`/validation errors — malformed-args failures are exactly the prompt/schema bugs RCA should catch; skip `duplicate`). Non-blocking send, errors logged never raised. Template: queue, DLQ, Lambda, per-function role (`bedrock:InvokeModel` on the Opus inference-profile ARN, `ses:SendEmail` scoped to the identity, Dynamo RW on `RCA#`/`PROFSUGG#`, transcript partition **read-only**).
- `[ ]` **O** — `rca-analyzer` Lambda: context gathering + analysis prompt. The prompt embeds a repo-versioned `docs/system-map.md` (≤2K tokens: surfaces, mint chain, tool registry, memory layer, settings/profile — reviewed like code) so Opus reasons about *this* system. Token budget ≤8K in / ≤2K out.
- `[ ]` **S** — Report email formatting + `RCA#` persistence + dedupe/cooldown/cap logic (caps are the cost story: worst case 10 Opus calls/day ≈ low single-digit dollars/month; normal case ≈ pennies).
- `[ ]` **H** — **Owner manual step:** enable Anthropic Claude Opus model access in Bedrock `us-east-1` (same console flow as the Nova Sonic request). If denied/slow: hold RCA disabled rather than shipping a weaker analyst — never downgrade.
- `[ ]` **S** — Tests: fake Bedrock + fake SES; dedupe window; cap; a golden RCA prompt snapshot test so context-gathering regressions are visible in review.
- `[ ]` **F** — Phase 2 (after server RCA proves out): the web `toolerror` path POSTs a lightweight `/api/v1/rca/client-event` breadcrumb onto the same queue — catches failures that never reach the tool router.

---

## WS-3 — Unfinished platform work

### 3.1 Wake-word training: complete one full run  `[~]`
⟵ archive/plan.md §8 line 667 (M6) · archive/android-revamp-plan.md M11.1
A full **train → model → hot-swap** run has never completed. The owner kicked off "Hey Live Ninja"
training on 2026-07-20 from their phone; Android `ModelManager.sync` fetches + hot-swaps on
completion (zero Android code needed). Do **not** relabel to "Hey Jarvis" (owner decision).
- `[~]` Confirm the 2026-07-20 training job finished and produced per-platform models in S3 (SHA-256 pinned).
- `[ ]` Verify hot-swap on web + Android (SHA verify + live swap).
- `[ ]` Until then the Android wake word is **inert** (packaged model is `hey_jarvis`) — say so in any user-facing note.

### 3.2 Deferred security/cleanup findings  `[ ]`
⟵ archive/plan.md §8 M7 "Lower findings ... noted for M8 cleanup"
- `[ ]` **S** — Idempotency-before-execute ordering in the tool router.
- `[ ]` **H** — `scripts/gen-icons/main.go` still emits the old teal icon design (dev-only; the HAL-eye PNGs are committed).

### 3.3 Owner decision needed  `[ ]`
- `[ ]` Add `proffitt.jeremy+qa@gmail.com` to the allowlist for two-account QA? (A QA password was pasted in-transcript on 2026-07-18 — **rotate it**. Clean path: owner signs the QA account into a separate Chrome profile once, then an agent can drive it; agents never type credentials.) ⟵ archive/plan.md §8 M14 item 11

---

## WS-5 — Android stability & performance  `[~]`

Opened 2026-07-24 after a live on-device session on the Tab S9 FE (`R52XC06P9KJ`). Every item
below is backed by a measurement or a reproduced defect from that session, not by inspection.
Owner decisions taken up front: **16 KB alignment is in scope** (with a mandatory voice re-verify
after the dependency bumps), and verification goes as far as an **instrumented on-device harness**.

Measured baseline (2026-07-24, v0.2.1-hal / versionCode 4, debug build):

| Signal | Value |
|---|---|
| Cold start, `am start -W` | 1168 ms (WaitTime 1176 ms) |
| Debug APK | 177 MB, native libs for 4 ABIs |
| Crashes in buffer | 0 · one stale ANR (2026-07-02) |
| Main-thread jank | `Skipped 55 frames`, `Davey! duration=705ms` on Settings; 12 MB spent JIT-compiling one composable |
| Lint | 1 error (`RemoveWorkManagerInitializer`, pre-existing via merged manifest), 92 warnings |
| 16 KB page alignment | Android 16 flagged `libonnxruntime.so`, `libonnxruntime4j_jni.so`, `libjingle_peerconnection_so.so`, `libandroidx.graphics.path.so` |

### M21 — Correctness defects reproduced on hardware  `[~]`

- `[x]` **21.0 Wake service could never start.** `WakeWordService.start()`'s only two callers were both gated on `WakePreferences.serviceEnabled`, which is only ever set from inside the service — an unbreakable cycle on a fresh install. Added the **Always listening** switch + `serviceEnabledFlow`; verified a running FGS (`types=0x80`) and speaker activation. Commit `74c0651`.
- `[x]` **21.1 Android sessions never reach History — data loss.** FIXED 2026-07-24. Root cause was worse than a missing flush: the app had **no transcript upload path at all** — `LiveNinjaApi` never declared `POST /api/v1/transcript`, so turns lived only in the in-memory `TranscriptStore` and every Android conversation was unrecoverable. Added `TranscriptUploader` (web-parity batching: 25 turns / 5 s, plus the load-bearing `final:true` session-end flush), the `TranscriptSink` seam so it is testable without the whole API surface, and 7 unit tests. **Verified on device:** a session at 22:04 produced a History row tagged `gpt-realtime`, where three earlier sessions had produced none. Original finding: Three sessions (21:22, 21:24, 21:31) produced no CONV row; History's newest entry stayed 2026-07-24 16:43 even after refresh. Suspect the client never sends a `{final:true}` transcript flush on session end, so `cmd/topics-extract` is never invoked. **Highest value item in this workstream** — every Android conversation is currently unrecoverable.
- `[ ]` **21.2 AEC self-echo loop.** The assistant's own speech is transcribed as user input ("You: Right now in…"), so it answers itself repeatedly. Hardware AEC is not cancelling loudspeaker output; reproduced at tablet volume 3/15 and 15/15. Candidate fix: stop trusting the hardware canceller (`setUseHardwareAcousticEchoCanceler(false)`) and let WebRTC's software AEC3 run, with a half-duplex guard as fallback.
- `[~]` **21.3 Wake phrase truthfulness — UI fixed 2026-07-25 (`7036853`), detection still unproven.** The UI half is done: the caption now derives from `ModelManager.headModel` (the loaded head model) instead of the selected catalog id, Settings warns when the selection has no model, and the catalog lists `hey-jarvis` truthfully as the bundled offline phrase. **Correction to the original finding below:** the trained `hey-live-ninja` model *is* present after all — it is downloaded, not bundled. On device: `files/wakeword/active_openwakeword.json` = `{"id":"hey-live-ninja","sha256":"d7282ac…"}` with a real 209 KB `.onnx`, written 2026-07-24 21:30, and the service logs `model sync: active hey-live-ninja`. **But it does not detect:** speaking "Hey Live Ninja" through the PC speakers produced no wake event while that model was active, whereas "Hey Jarvis" fired the night before. So the remaining work is detection quality, not packaging — try raising `sensitivity` above the 0.5 default, and verify with a real human voice (TTS is a poor proxy). This also means **WS-3 §3.2's training run did complete.** Original finding: Settings and the home screen both say "Hey Live Ninja"; the APK bundles only `hey_jarvis_v0.1.onnx`, so only "Hey Jarvis" actually triggers. Either ship the trained model (WS-3 §3.1) or surface the phrase the loaded model really detects — never a phrase that cannot match.

- `[x]` **21.4 The Always-listening switch reported intent, not reality.** Found by the reboot. It was bound to the persisted `serviceEnabled` flag, so it read **ON while nothing was listening**: Android 15+ will not let `WakeBootReceiver` start a microphone FGS from `BOOT_COMPLETED` unless the process happens to be foreground, so after a reboot the service stays down until the app is opened or the tap-to-resume notification (id 1002) is tapped — both observed on device. Fixed by mirroring `WakeWordService.isRunning` into an observable `runningFlow`, binding the switch to that, and rendering an explanation + **Resume listening** action in the paused state. Commit `7036853`. **Caveat:** the paused UI itself was not visually confirmed — the boot receiver/watchdog restore the service within about a second of the app being opened, so the state is too transient to screenshot. Worth a deliberate check later (e.g. deny the FGS start) before calling it observed.

### M22 — Performance  `[ ]`

- `[ ]` **22.1 Settings screen jank.** One ~1300-line composable in a single scrolling Column drops 55 frames and costs a 705 ms frame plus 12 MB of JIT on first open. Split into section composables, hoist `remember`ed state, and make the list lazy so composition is incremental.
- `[ ]` **22.2 Cold start 1168 ms.** Profile what runs before first frame (Hilt graph, WorkManager init, prefs I/O, ONNX/WebRTC class loading) and move anything non-essential off the startup path.
- `[ ]` **22.3 Release APK size.** 177 MB with 4 ABIs. Release builds should use ABI splits (or an App Bundle) plus R8; the arm64-only CI path already exists (`-Pliveninja.arm64Only=true`) and should be the default for anything shipped.

### M23 — 16 KB page-size alignment  `[ ]`  (owner-approved, verify-after)

- `[x]` **23.1** Done 2026-07-25 (`c51096e`): `onnxruntime` 1.20.0 → **1.27.0**, `webrtc-sdk` 125.6422.07 → **144.7559.09** (versions read from Maven Central metadata, not guessed). Verified by parsing ELF program headers straight out of the APK — all four previously-flagged libs now report `max LOAD p_align = 16384`. `zipalign -c -P 16` was inconclusive on this machine; the Python ELF check is the reliable one and is worth keeping. **Cost: all-ABI debug APK 177 MB → 256 MB**, which raises the priority of 22.3.
- `[~]` **23.2 Post-bump verification — partial.** Confirmed on the Tab S9 FE: app launches with **no** `dlopen`/`UnsatisfiedLink`/alignment errors, the wake FGS runs (`types=0x80`), `model sync` succeeds, a session mints, and `WebRtcTransport: oai-events channel state: OPEN`. **Not yet confirmed: a full spoken round-trip on the new deps** — the TTS-through-speakers rig stopped reaching the tablet mic after the reboot (PC audio routing/volume), which is a test-rig problem, not an app one. Re-run with the audio rig fixed: max PC output volume, tablet media volume ~3/15, then tap-to-talk and ask for the weather.

### M24 — Verification harness  `[ ]`  (owner chose instrumented, on-device)

- `[~]` **24.1** JVM unit tests for each M21 defect — done for 21.1 (`TranscriptUploaderTest`, 7 cases: final-flush-always-posts, seq/role/engine, blank-turn drop, no-sessionId, full-batch flush, failure-never-propagates, mode→engine mapping); remaining for 21.2/21.3 (transcript final-flush, wake-phrase resolution, AEC config, service/prefs state machine) so regressions fail in CI today.
- `[ ]` **24.2** Instrumented Espresso/UiAutomator tests for the flows that actually broke: onboarding → signed-in home, Always-listening toggle → running FGS, tap-to-talk → live session.
- `[ ]` **24.3** Wire an emulator job into `.github/workflows/android-release.yml` (CI has no attached device). The local AVD `liveninja-test` is the working reference.

## WS-4 — M8 Launch  `[ ]`

**Definition of Done:** SES production access granted; Cost Allocation Tags confirmed active; the
web and Android surfaces pass end-to-end smoke on production; distribution channels live; budgets confirmed
emailing (**no CloudWatch alerts — owner decision 2026-07-19; alarms stay removed**); runbook +
`/v1` long-horizon compatibility commitment documented.

- `[x]` **H** — SES production access + DKIM `@jeremy.ninja`, bounce/complaint SNS suppression wired (owner confirmed 2026-07-18).
- `[x]` **H** — `Project`/`CostCenter` Cost Allocation Tags active; budgets alerting (activated at M0 via CLI).
- `[ ]` **S** — Production end-to-end smoke: web voice turn, Android wake → WebRTC turn + tool call. **Gated on WS-1.**
- `[ ]` **S** — Distribution: web live ✅; **Android signed release APK** (release keystore `C:\dev\live-ninja-keys\release.keystore`, alias `liveninja`, held by owner) + `.well-known/assetlinks.json` + `GET /v1/app/android/latest` updater + **Google Play listing** (Play signing, data-safety).
- `[ ]` **H** — Runbook + on-call: alarm→action mapping, credential-rotation steps (re-put SSM), device kill-switch, `/v1` compatibility-lifetime commitment.
- `[ ]` **O** — Launch go/no-go review against every risk table; sign off residual-risk acceptances.

---

## Standing rules (carried forward — these do not expire)

- **Deploy = push to `main`.** GitHub Actions + OIDC only. No local `aws`/`sam deploy`/`sam sync`. Production-only; every push is a prod deploy.
- **arm64 everywhere**, `provided.al2023`, built `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 -tags lambda.norpc -o bootstrap`. Flip architecture and build step together.
- **No `Scan` on a serving path.** `Query`/`GetItem` only; read-mostly catalogs from S3/CloudFront snapshots; `ConsumedReadCapacityUnits` alarm armed.
- **Secrets:** SSM SecureString + KMS only. No Secrets Manager. Agents never see values — owner sets them via `scripts/set-secret.sh`.
- **Cost tags** (stack-level, `samconfig.toml`): `Project=live-ninja CostCenter=voice-ai Environment=prod ManagedBy=sam DeployedVia=github-actions Owner=jeremy`.
- **No CloudWatch alarms** (owner, 2026-07-19). Budgets email directly; the `live-ninja-ops` SNS topic's only producer is SES bounce/complaint.
- **Every new UI form** runs the mandatory multi-persona design pass before code.
- **Gate before every push:** `go build ./... && go vet ./... && go test ./...`, `node --check` on touched `.mjs`, `sam validate --lint`.
- **Model routing:** security/auth/contracts/architecture → Opus; audio/wake/sync/concurrency → Fable (→ Opus if unavailable, never Sonnet); mechanical → Haiku.

## Gotchas that cost real time (don't re-learn these)

⟵ archive/plan.md §8 — the full versions live there; these are the ones most likely to bite again.
- **Never put a query string on a ConvID path route** (`<ts>#<sid>`): the CloudFront/API-GW/LWA chain treats the decoded `#` as a fragment and silently drops everything after it. Local Fiber tests pass either way.
- **Gradle on this machine:** `java -cp gradle/wrapper/gradle-wrapper.jar org.gradle.wrapper.GradleWrapperMain` — the `cmd //c gradlew.bat` route silently fails under git-bash. `JAVA_HOME` is stale; the real JDK is `C:/Users/Jeremy/jdk-temurin17/jdk-17.0.19+10`.
- **Never blanket `taskkill //IM <interpreter>`** — killing all `python.exe` once took down the `windows-mcp` server mid-session. Target the PID.
- **Broker mint slots:** 3 concurrent sessions, ~10-min TTL. Burned slots make retests wait — budget for it.
- **GitHub "cancelled" runs** = queue replacement by a newer push, not a failure.
