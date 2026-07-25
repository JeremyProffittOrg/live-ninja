# Plan

Consolidated by `/clean-plans` on **2026-07-24**. Single source of truth for **active work**.
Deliberately-deferred future items live in [backlog.md](backlog.md) — those are **not** scheduled
and must not be pulled in here without a decision.

Folded in from (full history + verbose implementation notes preserved in each):

| Archived plan | What it contributed |
|---|---|
| [archive/plan.md](archive/plan.md) | Master M0–M12 plan + the entire §8 implementation-notes / RESUME-STATE history. **Read §8 there before resuming anything** — it is the deepest record of how this system actually works. |
| [archive/gemini-plan.md](archive/gemini-plan.md) | M13 Gemini Flash Live — code-complete + deployed; E1/E2 live-audio verification outstanding |
| [archive/base-knowledge-plan.md](archive/base-knowledge-plan.md) | M15–M17 — **M15 2026-07-24, M16 2026-07-25**; M17's code landed 2026-07-25 but its task block below is unreconciled and its owner Bedrock step is outstanding |
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
- **WS-2 Base Knowledge (M15–M17)** — M15 done 2026-07-24, **M16 done 2026-07-25**; M17's code is in but its checklist below still needs reconciling (and Bedrock Opus access is an owner step).
- **WS-3 Unfinished platform work** — the real code gaps (wake-word training run, deferred cleanup findings).
- **WS-4 Launch (M8)** — distribution, runbook, go/no-go.
- **WS-6 Owner-requested capabilities** — opened 2026-07-25 from live use. M26 device session
  control done; M27 volume, M28 camera/video, M29 budget warning and M30 settings revamp open.
- **WS-5 Android stability & performance** — opened 2026-07-24 from live on-device evidence, and **closed 2026-07-25**: M21 all verified on hardware (21.2 echo audibly gone at 4x the reproducing volume, 21.3 wake detection proven at stock sensitivity, 21.5 auth deadlock fixed), M22 perf done (all-ABI 256 MB → arm64 108.7 MB), M23 verified end to end with a spoken tool-calling round-trip, M24 harness green in CI, M25 cost badge verified on screen *and* in the persisted CONV row.

WS-2 and WS-3 are independent and can run in parallel. WS-1 gates WS-4.

---

## WS-1 — Verification (owner / hardware gated)

**Definition of done:** every "built but never exercised with real audio/hardware" claim in the
archived plans is either confirmed working or converted into a bug with a repro.

> These cannot be automated: the agent profile has **no microphone** (hard block, hit repeatedly
> across M12/M13/M19 verification) and the phone is physically at the owner's desk.

### 1.1 Live voice loop — web  `[ ]` (owner)
⟵ archive/plan.md §8 M14 item 12 · docs/qa-report.md "Live voice / microphone"
- `[x]` Real voice session: mint → WebRTC connect to OpenAI → spoken turn → tool call round-trip via `POST /api/v1/tools/invoke`.  **Owner-verified 2026-07-25.**
- `[x]` Model actually **calls `memory_search`** when asked "what is my home address" (the fix deployed 2026-07-18 was never confirmed live). ⟵ qa-report Surface 5  **Owner-verified 2026-07-25.**
- `[x]` Resolved voice/accent audibly applied — Noir Detective → new-york, Josh Lyman → `ash`. ⟵ qa-report Surfaces 2/3  **Owner-verified 2026-07-25.**
- `[x]` Per-persona voice memory: two personas, switch, each speaks its saved voice; `personaPrefs` persists in DynamoDB. ⟵ qa-report Surface 4  **Owner-verified 2026-07-25.**
- `[x]` Cross-tab live apply: change mic pickup / turn detection in tab B → tab A applies mid-session via `session.update`. ⟵ qa-report Surface 4  **Owner-verified 2026-07-25.**
- `[x]` Mid-session mic-eagerness chip audibly changes end-of-turn behaviour. ⟵ qa-report Surface 4  **Owner-verified 2026-07-25.**
- `[x]` Barge-in / wake-word detection in a browser with a working mic. ⟵ qa-report Surface 8  **Owner-verified 2026-07-25.**
- `[x]` **Android done 2026-07-25** (`costUsd=0.023532`, `surface=android`, read straight from the CONV row; web still unverified). Confirm the cost-persist chain produces a **costed CONV row** (needs one live session; typed fallback turns emit no usage events). ⟵ archive/plan.md §8 M14 item 10

### 1.2 Gemini Flash Live — E1/E2  `[!]` **BLOCKED ON A REAL DEFECT, not on the owner**

> **Root cause found 2026-07-25 — Gemini Live has never worked.** A mint attempt returns
> `mint_failed` and the broker log gives the reason exactly:
> `Error 401 ... Expected OAuth 2 access token ... Status: UNAUTHENTICATED`,
> `reason: ACCESS_TOKEN_TYPE_UNSUPPORTED`, on
> `google.ai.generativelanguage.v1alpha.AuthTokenService.CreateToken`.
> The broker authenticates with an **API key** (`genai.NewClient{APIKey, BackendGeminiAPI}` —
> `internal/realtime/gemini_mint.go:401`), and Google's ephemeral-token endpoint **does not
> accept API-key auth**. So E1/E2 were never a verification gap; the feature could not have
> passed. Owner decision required — see backlog/decision note below. Observed txIds:
> `BE8YPi-HoAMEPkQ=`, `BE8YSgrMoAMEQ0Q=`, `BE8ZCjrdoAMEPpA=`.
>
> **Mitigated 2026-07-25 (`c1b302b`), but not fixed.** A device pinned to Gemini could not start
> a conversation at all — every mint returned 502. The mint errors have always said *"use the
> fallback cascade"*, and a broker test even asserted "the clients' fallback cascade already
> handles" it — **no client has ever implemented one** (no cascade exists in `web/realtime.mjs`
> or the Android coordinator). The cascade now lives in the broker, which is the only component
> that knows which engines are configured and covers every surface at once; the user is told
> once via the warnings channel rather than having the voice silently change. Guarded on a nil
> minter. The happy-path cascade is **not unit-covered** — `broker.minter` is a concrete
> `*realtime.Minter`, not an interface.
>
> **ATTEMPTED 2026-07-25 (`9c989e8`) — credential wired, and it hit a HARD SDK LIMIT.**
> The service-account key is in SSM, the broker's IAM grant covers it, and the mint now builds
> `auth.Credentials` from it. The Go SDK rejects the result at client-init:
> `api key is required for Google AI backend`. Reading `google.golang.org/genai@v1.64.0`:
> `Credentials` and `APIKey` are **mutually exclusive** (client.go:214), and `Credentials` is
> only ever wired for **`BackendVertexAI`** (client.go:369) — on `BackendGeminiAPI` the only
> auth the SDK sends is the `x-goog-api-key` header. **The SDK cannot do service-account OAuth2
> against the Gemini Developer API at all.**
>
> **Remaining options, owner's call:** (i) bypass the SDK for this one call — POST
> `v1alpha/auth_tokens` directly with an OAuth2 bearer, hand-rolling the request/response
> (small surface: one POST, and the repo already renders the constraints as raw JSON for the
> setup frame, but it is a hand-maintained Google API call); (ii) move Gemini Live to
> `BackendVertexAI`, which supports `Credentials` — but the Phase-0 spike found ephemeral
> tokens only work on the v1alpha *generativelanguage* Constrained method, so this may not
> exist there; (iii) drop Gemini Live to backlog and stay on `openai-realtime`.
>
> **Meanwhile the user is NOT blocked:** the broker fallback (`c1b302b`) is confirmed working in
> production — log shows `pinned engine could not mint; falling back` → `session minted`. A
> Gemini-pinned device now gets a working OpenAI session instead of a 502.
>
> **Credential SUPPLIED 2026-07-25** — `GEMINI_SERVICE_ACCOUNT_JSON` is set as a GitHub secret
> (20:14:18Z). Remaining work is code, not owner action: (1) add an SSM put for it in
> `.github/workflows/deploy.yml` alongside `/live-ninja/prod/gemini/api_key`, (2) swap
> `internal/realtime/gemini_mint.go` from `genai.NewClient{APIKey, BackendGeminiAPI}` to
> service-account OAuth2, keeping the API-key path as a fallback so nothing regresses if the
> credential is absent, (3) deploy and retry a Gemini-pinned mint. Only then can E1/E2 be
> attempted. **Not yet started.**
>
> **What the owner had to supply (now done):** a **GCP service-account JSON key** with
> Generative Language API access, stored via `scripts/set-secret.sh` (SSM SecureString). Option
> (b) is ruled out — owner will not run a 24x7 container, and option (a) needs none: it runs
> entirely in the existing Lambda.
>
> **Options:** (a) OAuth2 service-account credentials for `CreateToken` (SSM SecureString, no
> secrets manager); (b) proxy Gemini Live through the backend like `nova-bridge` so the key
> never reaches the client — more infra and standing cost; (c) drop Gemini Live to backlog and
> stay on `openai-realtime`. Until one is chosen, E1/E2 cannot be attempted.
⟵ archive/gemini-plan.md §4 Phase E · exact 6-step script in that file's §10 "Phase E status"
- `[!]` **E1 cross-engine parity:** pin one device to `gemini-flash-live`, one to `openai-realtime` — transcripts land in the same sink with correct `engine` tags, tools invoke identically, topics/memory extraction runs, cost priced at Gemini rates, barge-in cuts playback, persona switch changes the Gemini voice per the D4b mapping, user `geminiVoice` overrides it.
- `[!]` **E2 lifecycle:** a >10-min session survives the `goAway` recycle via resumption handle; a >30-min session re-fetches a fresh token and resumes; the quota gate still fires pre-mint.
  Notes: Android `GeminiLiveTransport` was compile-unverified when written — the later v0.2.1-hal build compiled it, so that gate is satisfied.

### 1.3 Tool-manifest live smoke (post-M19)  `[ ]` (owner)
⟵ archive/tool-parity-plan.md §Verification
- `[x]` "Set a timer for 20 minutes" → fires; no `invalid_args` in the `LOG#` audit rows.  **Owner-verified 2026-07-25.**
- `[x]` "Set a timer for 3 days" → model hands off to `set_reminder` (one `invalid_args` row naming `set_reminder`, then a successful `set_reminder`, is the healthy shape).  **Owner-verified 2026-07-25.**
- `[x]` "What's the weather in London in celsius" → `units:metric` actually requested.  **Owner-verified 2026-07-25.**
- `[x]` "What notes do I have tagged work" → tag filter used; "read me my recent notes" with no query succeeds.  **Owner-verified 2026-07-25.**
  (The `device_control` / "reboot the terminal" step from the original smoke needs a Tab5 — moved to `backlog.md`.)
- `[ ]` Repeat the first two on a `gemini-flash-live`-pinned device.

### 1.4 Authed web surfaces  `[ ]` (owner or owner-assisted browser session)
⟵ docs/qa-report.md "Requires an authenticated session"
- `[x]` Full LWA web sign-in end-to-end → `__Host-ln_rt` cookie → `/conversation`.  **Owner-verified 2026-07-25.**
- `[ ]` Android Custom-Tabs PKCE exchange (`POST /auth/lwa/exchange`) on a real device.
- `[x]` `GET /personas` renders the grouped library (builtin/mine/shared) when authed.  **Owner-verified 2026-07-25.**
- `[x]` Persona editor round-trip: create → edit voice/accent → share → copy a shared one → `personachanged` refresh + mid-session pending banner.  **Owner-verified 2026-07-25.**
- `[x]` Settings autosave + 409 reconcile (concurrent second-device edit → remote-wins toast).  **Owner-verified 2026-07-25.**
- `[x]` `/history` authed rendering: tool-call Details disclosure, top toggle persists across reloads.  **Owner-verified 2026-07-25.**
- `[x]` `/conversation` authed runtime: drawer focus-trap/Escape, mic-sens chips live-apply, persona `<select>` populated, transcript streams, cost badge on session start.  **Owner-verified 2026-07-25.**
- `[x]` Settings **drawer** opened and exercised in a real browser — the drawer relocation was only ever statically screenshotted, never hydrated live (`initSettingsPanel`). ⟵ archive/plan.md §8 Task #8 Request 3  **Owner-verified 2026-07-25.**

### 1.5 Android device  `[ ]` (owner, on the phone)
⟵ docs/qa-report.md "Device / hardware" · archive/plan.md §8
- `[x]` Live voice round-trip capture on Android — done 2026-07-25: spoken turn → `get_weather` tool call → spoken answer → `final:true` flush → CONV row in History tagged `gpt-realtime`.
- `[ ]` PWA install + offline: install prompt / add-to-homescreen / real offline navigation fallback on a device.
- `[~]` Android wake / lock-screen paths on real hardware. **Wake path done 2026-07-25** (wake phrase → `SessionOrchestrator` → live session, unlocked). **Lock-screen/keyguard path still untested.**
- `[ ]` Android FRR/FAR wake-engine corpus harness gated in CI + on-device instrumented runs. **M4 DoD gap.** (S)

### 1.6 Delivery / infra spot-checks  `[ ]`
⟵ docs/qa-report.md "Delivery / infra / out-of-band"
- `[ ]` Confirm the memory-fix commit is the currently-deployed `WebFunction` version. (H)
- `[x]` Security emails delivered to the owner inbox — owner confirmed working 2026-07-19.
- `[x]` `Project`/`CostCenter` Cost Allocation Tags active — activated via `ce update-cost-allocation-tags-status`, Errors:[] (archive/plan.md §8 M0).
- `[ ]` M9/M10/M11 (deliverables, memory/guides, topics/history) exercised with **real data**, not just deployed. (S)
- `[x]` Playwright e2e + Lighthouse/axe WCAG-AA gates wired into CI — done 2026-07-25 (`b21ed31`, `52c8d44`). `tests/web/`: **34 Playwright tests** (desktop + Pixel 7) with axe WCAG 2.1 AA in **both colour schemes**, focus-ring, no-cache HTML, sw.js root scope, manifest icons actually fetched, 1.4.10 reflow at 320 px and 200 % zoom, no console errors; plus Lighthouse CI asserting **accessibility ≥ 1.0** / best-practices ≥ 0.9 / SEO ≥ 0.9. Verified green in CI against the deployed origin (every step, not just the job). **The gates found and fixed two real defects:** Lighthouse accessibility was 0.98 (`heading-order` — the landing page's feature cards were `h3` directly under the `h1`) and SEO 0.90 (no meta description). **Scope limit, deliberate:** unauthenticated surface only — the authed screens have no CI credentials, so the suite asserts they *redirect* anonymous visitors rather than faking a session; their authed behaviour stays owner-gated under 1.4. Runs **after** deploy and is advisory (`continue-on-error`), because the Fiber app needs Dynamo/KMS/SSM to boot so a local pre-deploy harness could only reach `/healthz`.

---

## WS-2 — Base Knowledge Layer + Tool-Failure RCA (M15–M17)

⟵ archive/base-knowledge-plan.md — **fully authored, never started.** That file carries the
grounded problem statement (P1–P4, each citing the real seam), the full architecture sketch for
M17, and the sequencing/cost/risk analysis. Read it before starting; the task lists below are
verbatim.

**Sequencing (locked in the source plan): M15 → M17 → M16-polish.** M15 (2026-07-24) and M16
(2026-07-25) are done; M17's code landed alongside M16 but its task list below is not yet ticked
off. M15 killed the daily
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

### M16 — Knowledge Refinement Loop  `[x]`  (built + deployed 2026-07-25)

**Definition of Done:** the assistant (and M17's RCA pipeline) can *propose* base-knowledge
additions; proposals queue as pending suggestions the owner approves/rejects in Settings; approved
ones merge into the profile with version history; nothing ever auto-writes identity/location facts
without confirmation.

- `[x]` **S** — `profile_suggest` tool (assistant-callable, in the session manifest): proposes a field change or a new `notes[]` fact with a reason; writes a `PROFSUGG#` item (pending, TTL 30 days), **never** mutates the profile. Result tells the model "suggested — Jeremy will confirm in Settings."
- `[x]` **S** — Suggestions UI in "About you": pending list with Approve/Reject (approve = normal versioned settings PUT); badge on the drawer tab when suggestions are pending.
- `[x]` **O** — **Policy (locked, revisit later):** auto-apply allowed only for `units` and `notes[]` additions the owner spoke *explicitly* — and even those surface a toast + undo. Location/name/email always require Settings confirmation. Rationale: a mis-set home location silently poisons every weather/time answer.
- `[x]` **H** — `memoryUsageDirective` updated so the model knows the split: *stable facts → profile (visible in your instructions); episodic facts → memory tools*.

**Implementation notes (2026-07-25).** Built on top of M15's profile section and M17's `PROFSUGG#`
item — no new AWS resources, no IAM change, no template change, no new secret. `go build/vet/test
./...`, `node --check` on both touched `.mjs`, and `sam validate --lint` all green.

- **Files added:** `internal/store/profilesuggest.go` (+`_test.go`) — the shared field vocabulary,
  the auto-apply policy gate, the document mutations, and the versioned apply/undo;
  `internal/tools/profilesuggest.go` (+`_test.go`) — the tool;
  `internal/webapp/profile_suggestions_routes.go` (+`_test.go`, +`profile_suggestions_ui_test.go`)
  — the queue's two routes and the client/server drift guards.
- **Files changed:** `internal/store/rca.go` (`ProfileSuggestion` gains `autoApplied` + `resolvedAt`),
  `internal/rca/bedrock.go` (`FieldProfileNotes`/`FieldProfileUnits` now alias the store constants),
  `internal/tools/registry.go` (tool 21 registered), `internal/realtime/mint.go`
  (`memoryUsageDirective`), `internal/realtime/personas.go` (`coreInstructions` names the tool),
  `internal/webapp/profile_routes.go` (routes mounted; `validateProfile` bounds now come from store),
  `web/templates/pages/conversation.html`, `web/static/js/settings.mjs`,
  `web/static/js/conversation.mjs`, `web/static/css/app.css`, `contracts/api.md`,
  `contracts/settings.schema.json`, `docs/system-map.md`, `internal/rca/testdata/golden_prompt.txt`
  (re-recorded), and the three tool-count/comment sites (`tool_manifest_test.go` 20→21,
  `persona_tool_coverage_test.go`, `gemini_mint.go`).
- **The policy is one function, in the store.** `store.AutoApplyableProfileField` is the only
  definition of "may be written without confirmation" (units + a `notes[]` ADDITION), and
  `ApplyProfileSuggestion`/`RevertProfileSuggestion`/`AutoApplyProfileSuggestion`/
  `UndoProfileSuggestion` all refuse anything else with `ErrProfileFieldProtected` **before reading
  the document**. That is why a hand-crafted `POST /api/v1/tools/invoke` with
  `{"field":"profile.homeLocation","autoApply":true}` cannot land a location: the refusal is not in
  the tool and not in the UI. `TestProfileSuggestRefusesAutoApplyForProtectedFields` asserts the
  settings item is **byte-identical** afterwards (not merely "reverted"), and
  `TestAutoApplyableProfileFieldIsExactlyUnitsAndNotes` pins the allowlist as a SET so a future
  suggestible field lands on the protected side by default.
- **Approve is not a write path.** The client puts the proposed value into the document it already
  holds and lets settings.mjs's existing autosave PUT it (`flushProfileNow` just drives that loop to
  completion), then POSTs the resolve. So an approved value passes the same `validateProfile` a
  manual edit does, and inherits the same 409 reconcile —
  `TestApproveSuggestionRidesTheVersionedSettingsPut` proves the versioning is real by replaying the
  PUT at the now-stale version. The **undo** is the one server-side write, because the tab never saw
  the value the auto-apply replaced.
- **A location suggestion has no Approve button.** Its primary action is "Find this place", which
  prefills the existing geocode combobox and resolves the row only once a real result is *picked*
  (`locationSuggestionPicked`, hooked into the picker's `choose()`). M15's `validateProfile` rejects
  a coordinate-less location and that rejection is what makes the geocode-free weather path
  trustworthy — an Approve button that wrote a spoken place name would have quietly undone M15.
- **Resolve-once is `attribute_not_exists(resolvedAt)`, not a status check.** Both lifecycles
  converge on it: a pending row is resolved by Approve/Reject, an auto-applied row (written
  `status=approved`, `resolvedAt` empty) by Keep/Undo. One atomic conditional update covers a
  double-clicked button, a duplicate POST, and two open tabs. `resolvedAt == ""` is also exactly the
  "needs the owner" predicate the list route and the badge use.
- **`FindProfileSuggestion` paginates on purpose.** The sort key embeds `createdAt`, so an id alone
  cannot address an item; the lookup is a bounded single-partition Query with a `suggId` filter, and
  it pages because DynamoDB applies `FilterExpression` AFTER `Limit` (the FakeDynamo filters first,
  so a single-page implementation would have passed the tests and failed in prod).
- **The toast.** `#drawerToast` sits at page level, so the auto-apply toast + Undo works while the
  drawer is closed — the state that matters, since the model applies units mid-conversation. With
  the drawer OPEN a page-level toast would be painted behind it (`showModal()` puts the dialog in
  the top layer, above any z-index), so that branch scrolls to the row and announces instead; the
  row carries the same Undo. Shown-once tracking is a localStorage id list pruned to live rows.
- **`memoryUsageDirective` deliberately avoids the literal string "BASE KNOWLEDGE".**
  `TestBaseKnowledgeComposesAfterSessionDirectives` locates the block by that header with
  `strings.Index`, so a second mention inside the directive would have made all three ordering
  assertions measure the wrong occurrence and pass regardless. The test now also asserts the header
  appears **exactly once**, which is the guard that keeps the ordering contract meaningful.
- **`docs/system-map.md` is on a hard byte budget** (`maxSystemMapChars = 8000`; it rides in every
  RCA prompt). The M16 paragraph was written to fit in the remaining ~295 bytes — ASCII only,
  because an em dash costs three. Adding to that file means measuring first, then re-recording
  `internal/rca/testdata/golden_prompt.txt` with `go test ./internal/rca -run TestGoldenRCAPrompt -update`.
- **Caps:** 12 undecided rows per user from the tool (a queue nobody reads has no value), and a
  duplicate proposal returns `already_suggested` with the existing id rather than an error the model
  would apologise for. A resolved row does not block re-proposing the same thing later.
- **Owner action to make this live:** nothing. The tool is in every mint from the next deploy; the
  drawer badge stays hidden until something is actually suggested.

### M17 — Tool-Failure Agentic RCA (Bedrock Opus → email)  `[x]`  (built + deployed 2026-07-25)

**Definition of Done:** when a tool invocation ends `outcome=error` in prod, an automated RCA runs
within ~1 minute: pulls the failing call + surrounding conversation window + the tool's contract,
asks **Claude Opus on Bedrock** for a structured RCA, emails the report to the owner, and files any
base-knowledge / code-fix suggestions into the M16 queue. Deduped, rate-capped, off the request path.
Full architecture diagram in [archive/base-knowledge-plan.md](archive/base-knowledge-plan.md) §M17.

- `[x]` **S** — SQS `live-ninja-rca` + enqueue in `Registry.finish` on `outcome=error` (include `CodeNotFound`/`CodeUpstreamError`/validation errors — malformed-args failures are exactly the prompt/schema bugs RCA should catch; skip `duplicate`). Non-blocking send, errors logged never raised. Template: queue, DLQ, Lambda, per-function role (`bedrock:InvokeModel` on the Opus inference-profile ARN, `ses:SendEmail` scoped to the identity, Dynamo RW on `RCA#`/`PROFSUGG#`, transcript partition **read-only**).
- `[x]` **O** — `rca-analyzer` Lambda (arm64/`provided.al2023`, verified live in `us-east-1`): context gathering + analysis prompt. The prompt embeds a repo-versioned `docs/system-map.md` (≤2K tokens: surfaces, mint chain, tool registry, memory layer, settings/profile — reviewed like code) so Opus reasons about *this* system. Token budget ≤8K in / ≤2K out.
- `[x]` **S** — Report email formatting + `RCA#` persistence + dedupe/cooldown/cap logic (caps are the cost story: worst case 10 Opus calls/day ≈ low single-digit dollars/month; normal case ≈ pennies).
- `[x]` **H** — **Owner manual step — DONE (owner confirmed 2026-07-25):** Anthropic Claude Opus model access in Bedrock `us-east-1` (same console flow as the Nova Sonic request). If denied/slow: hold RCA disabled rather than shipping a weaker analyst — never downgrade.
- `[x]` **S** — Tests: fake Bedrock + fake SES; dedupe window; cap; a golden RCA prompt snapshot test so context-gathering regressions are visible in review.
- `[ ]` **F** — Phase 2 (after server RCA proves out): the web `toolerror` path POSTs a lightweight `/api/v1/rca/client-event` breadcrumb onto the same queue — catches failures that never reach the tool router.

---

## WS-3 — Unfinished platform work

### 3.1 Wake-word training: complete one full run  `[~]`
⟵ archive/plan.md §8 line 667 (M6) · archive/android-revamp-plan.md M11.1
A full **train → model → hot-swap** run has never completed. The owner kicked off "Hey Live Ninja"
training on 2026-07-20 from their phone; Android `ModelManager.sync` fetches + hot-swaps on
completion (zero Android code needed). Do **not** relabel to "Hey Jarvis" (owner decision).
- `[x]` Confirm the 2026-07-20 training job finished and produced per-platform models in S3 (SHA-256 pinned). Proven on device: `active_openwakeword.json` pins `hey-live-ninja` sha256 `d7282ac…` against a real 209 KB `.onnx`.
- `[~]` Verify hot-swap on web + Android (SHA verify + live swap). **Android done 2026-07-25** — the downloaded trained model is the loaded head model and it detected (`score=0.696`). **Web still unverified.**
- `[x]` ~~Until then the Android wake word is **inert**~~ — **no longer true as of 2026-07-25.** "Hey Live Ninja" detects on device with the downloaded trained model. Do not repeat the "inert / only Hey Jarvis works" caveat in user-facing notes.

### 3.2 Deferred security/cleanup findings  `[~]`
⟵ archive/plan.md §8 M7 "Lower findings ... noted for M8 cleanup"
- `[x]` **S** — Idempotency-before-execute ordering in the tool router. Fixed 2026-07-25 in the M17 commit: the `IDEMP#<userId>#<key>` conditional put is now claimed *before* the side-effecting handler runs, so a retried invocation cannot execute twice.
- `[x]` **H** — `scripts/gen-icons/main.go` now reproduces the shipped HAL-eye art (`c9bb28a`): all four assets to **MAE ~1/255 with 95–97% of pixels within 8/255**, residual being sub-pixel edge placement from integer-unit radii. Every value measured off the committed `icon-512.png`, not approximated: flat navy disc **r=199** (not 140), plate `#060d18`, navy `#16294a`, the eye as **translucent** red discs (alpha 0.167/0.36 over `#ff5f4a`) sitting **over** a lens band that darkens the navy, and the band a **stadium** (flat 51-unit half-height to x=106, then radius-51 caps) which predicts 51/38/26/14 at x=110/140/150/155 exactly as measured. **Correction:** at 192 px the shipped art reads as a smooth glow and I briefly rewrote the generator around gradients on that basis; at 512 px the shipped art is a hard-edged disc stack, so that was a redesign of the app icon, not a fix, and was reverted. Renders to a scratch cwd (the output path is relative), so the committed PNGs stay untouched.

### 3.4 Secret tooling  `[x]`  (2026-07-25, `96b9462`)

- `[x]` **`scripts/set-secret.bat` had two defects, both found by exercising the guards.**
  (1) `findstr /r /x "[A-Z][A-Z0-9_]*"` is not reliably case-sensitive — findstr's character
  ranges are collation-dependent — so a lowercase name passed the UPPER_SNAKE_CASE guard and a
  secret was created under it. Now validated with PowerShell `-cmatch`. (2) The interactive path
  piped stdin straight to `gh secret set`, so run without a console (agent, pipe, CI) `gh` read
  EOF and set an **empty secret with no error**; the `.sh` has always guarded this with
  `[ -t 0 ]`, the `.bat` never did. It now refuses without a real console.
  **Implication worth remembering:** any secret set non-interactively with the old `.bat` could
  have been silently blanked. The four live secrets all look intact, but an unexplained auth
  failure should suspect this first.
- `[x]` A stray `gemini_key` secret created during that discovery was deleted; it was referenced
  by no workflow.

### 3.3 Owner decision needed  `[ ]`
- `[ ]` Add `proffitt.jeremy+qa@gmail.com` to the allowlist for two-account QA? (A QA password was pasted in-transcript on 2026-07-18 — **rotate it**. Clean path: owner signs the QA account into a separate Chrome profile once, then an agent can drive it; agents never type credentials.) ⟵ archive/plan.md §8 M14 item 11

---

## WS-5 — Android stability & performance  `[x]`  (closed 2026-07-25)

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

### M21 — Correctness defects reproduced on hardware  `[x]`  (all verified on hardware 2026-07-25)

- `[x]` **21.0 Wake service could never start.** `WakeWordService.start()`'s only two callers were both gated on `WakePreferences.serviceEnabled`, which is only ever set from inside the service — an unbreakable cycle on a fresh install. Added the **Always listening** switch + `serviceEnabledFlow`; verified a running FGS (`types=0x80`) and speaker activation. Commit `74c0651`.
- `[x]` **21.1 Android sessions never reach History — data loss.** FIXED 2026-07-24. Root cause was worse than a missing flush: the app had **no transcript upload path at all** — `LiveNinjaApi` never declared `POST /api/v1/transcript`, so turns lived only in the in-memory `TranscriptStore` and every Android conversation was unrecoverable. Added `TranscriptUploader` (web-parity batching: 25 turns / 5 s, plus the load-bearing `final:true` session-end flush), the `TranscriptSink` seam so it is testable without the whole API surface, and 7 unit tests. **Verified on device:** a session at 22:04 produced a History row tagged `gpt-realtime`, where three earlier sessions had produced none. Original finding: Three sessions (21:22, 21:24, 21:31) produced no CONV row; History's newest entry stayed 2026-07-24 16:43 even after refresh. Suspect the client never sends a `{final:true}` transcript flush on session end, so `cmd/topics-extract` is never invoked. **Highest value item in this workstream** — every Android conversation is currently unrecoverable.
- `[x]` **21.2 AEC self-echo loop — FIXED AND AUDIBLY VERIFIED 2026-07-25.** Proven with a real spoken turn through the PC speakers at tablet media volume **12/30 — four times the 3/30 that originally reproduced the loop**: the transcript shows exactly one `You` turn and one assistant reply, where the 06:16 run had every assistant sentence come back as `You`. Original finding and fix detail below. Reproduced beyond doubt 2026-07-25: a session left open 06:02–06:16 looped for 14 unattended minutes, and the transcript alternates the assistant's own sentence back as the user's ("Live Ninja: I'm right here with you." / "You: Got it." / "Live Ninja: Understood…" / "You: Understood."). Fixed by `VoiceAudioProcessing.SOFTWARE_APM` (hardware AEC + NS off so libwebrtc's AEC3 runs) plus `EchoGate`, a real playback-driven half-duplex mic guard.
  **The load-bearing discovery is a missing permission.** All three transports already called `configureAudioForCall()` to set `MODE_IN_COMMUNICATION`, but `MODIFY_AUDIO_SETTINGS` was never declared, and `AudioManager.setMode()` without it is dropped by AudioService **with no exception and no log** — so the readback was `call audio: mode=0` (MODE_NORMAL) and AEC3 never got a render reference. Declaring it flipped the readback to `mode=3`, corroborated by WebRTC's own `VolumeLogger: audio mode is: MODE_IN_COMMUNICATION` and Samsung's `BWU@AecWakeupService: audio mode is changed : 3`. Without this the rest of the AEC fix was inert.
  Verified on device: `AcousticEchoCanceler: was enabled, enable: false, is now: disabled`, `half-duplex mic guard: true`, `mode=3 route=2`. **Still unproven:** that the echo is *audibly* gone — 13 s of open mic produced zero self-triggered turns where it previously looped within seconds, but nothing was playing to echo, so that is suggestive, not conclusive. Needs a real voice with audible playback (same test as 23.2). Commit `d86adfa`.
- `[x]` **21.3 Wake phrase truthfulness — UI fixed (`7036853`) and DETECTION PROVEN 2026-07-25.** `OpenWakeWordEngine: wake detected: "hey live ninja" score=0.696 thr=0.50` followed by `SessionOrchestrator: wake detected (hey live ninja) → starting session`. **At the stock 0.5 threshold — no sensitivity change was involved** (an attempt to raise it failed with `cp: not directory`, and `wake.xml` still holds only `serviceEnabled`, so the run was unmodified). So the earlier "does not detect" conclusion was wrong: the trained model is fine at default sensitivity. The distinguishing factor was a **freshly started wake service** — the failing attempts ran the service through a preceding WebRTC session, and Samsung's own wake stack logs `BWU@ApWakeupService: isRecognitionAllowed? false` in that window, so mic contention after a realtime session releases the device is the likely cause and is worth its own look. **Diagnostic gap found:** `OpenWakeWordEngine` logs the score *only when it crosses the threshold* (line 222), so a near-miss is completely invisible — log scores at debug to make the next such investigation tractable. Original UI finding below. The UI half is done: the caption now derives from `ModelManager.headModel` (the loaded head model) instead of the selected catalog id, Settings warns when the selection has no model, and the catalog lists `hey-jarvis` truthfully as the bundled offline phrase. **Correction to the original finding below:** the trained `hey-live-ninja` model *is* present after all — it is downloaded, not bundled. On device: `files/wakeword/active_openwakeword.json` = `{"id":"hey-live-ninja","sha256":"d7282ac…"}` with a real 209 KB `.onnx`, written 2026-07-24 21:30, and the service logs `model sync: active hey-live-ninja`. **But it does not detect:** speaking "Hey Live Ninja" through the PC speakers produced no wake event while that model was active, whereas "Hey Jarvis" fired the night before. So the remaining work is detection quality, not packaging — try raising `sensitivity` above the 0.5 default, and verify with a real human voice (TTS is a poor proxy). This also means **WS-3 §3.2's training run did complete.** Original finding: Settings and the home screen both say "Hey Live Ninja"; the APK bundles only `hey_jarvis_v0.1.onnx`, so only "Hey Jarvis" actually triggers. Either ship the trained model (WS-3 §3.1) or surface the phrase the loaded model really detects — never a phrase that cannot match.

- `[x]` **21.4 The Always-listening switch reported intent, not reality.** Found by the reboot. It was bound to the persisted `serviceEnabled` flag, so it read **ON while nothing was listening**: Android 15+ will not let `WakeBootReceiver` start a microphone FGS from `BOOT_COMPLETED` unless the process happens to be foreground, so after a reboot the service stays down until the app is opened or the tap-to-resume notification (id 1002) is tapped — both observed on device. Fixed by mirroring `WakeWordService.isRunning` into an observable `runningFlow`, binding the switch to that, and rendering an explanation + **Resume listening** action in the paused state. Commit `7036853`. **Caveat:** the paused UI itself was not visually confirmed — the boot receiver/watchdog restore the service within about a second of the app being opened, so the state is too transient to screenshot. Worth a deliberate check later (e.g. deny the FGS start) before calling it observed.

- `[x]` **21.5 The app bricked its own API access after 15 minutes — "Couldn't start the conversation / Forbidden".** Owner-reported 2026-07-25 and reproduced against production. The access JWT lives 15 min (`internal/auth/session.go` `AccessTokenTTL`), but **the edge never answers an expired one with 401**: the API Gateway Lambda authorizer returns `IsAuthorized:false` and HTTP API synthesizes its own `403 {"message":"Forbidden"}` (confirmed by curl — 23 bytes), so the request never reaches Fiber and nothing can choose a 401. `okhttp3.Authenticator` is invoked **only on 401/407**, so `TokenAuthenticator` never saw the denial and never refreshed — every authed call 403'd until the app happened to be foregrounded again (`AuthRepository.onStart`), which is why it looked intermittent and why force-stopping "fixed" it.
  Fixed in `AuthInterceptor`, which now owns both halves: refresh proactively within 60 s of expiry (kills the 403 at source) and, if the edge still denies, refresh once and replay once. It discriminates the edge's bare `{"message":"Forbidden"}` from Fiber's `{"error":...}` taxonomy so a genuine authorization 403 never burns a token rotation or loops. 8 unit tests including the app-level-403 guard and same-token-no-replay. Commit `d86adfa`.
  **Note for any future 403 debugging: this whole class of bug is invisible to the client's 401 path.** If another surface ever relies on `Authenticator` for re-auth, it has the same latent hole.

### M22 — Performance  `[x]`  (2026-07-25)

- `[x]` **22.1 Settings screen jank.** One ~1300-line composable in a single scrolling Column drops 55 frames and costs a 705 ms frame plus 12 MB of JIT on first open. Split into section composables, hoist `remember`ed state, and make the list lazy so composition is incremental.
- `[x]` **22.2 Cold start 1168 ms.** Profile what runs before first frame (Hilt graph, WorkManager init, prefs I/O, ONNX/WebRTC class loading) and move anything non-essential off the startup path.
- `[x]` **22.3 Release APK size.** Measured result: arm64-only debug APK is **108.7 MB** vs **256 MB** all-ABI — a 58% cut. `arm64Only` is now the shipped default with R8 + resource shrinking. 177 MB with 4 ABIs. Release builds should use ABI splits (or an App Bundle) plus R8; the arm64-only CI path already exists (`-Pliveninja.arm64Only=true`) and should be the default for anything shipped.

### M23 — 16 KB page-size alignment  `[x]`  (verified end to end 2026-07-25)

- `[x]` **23.1** Done 2026-07-25 (`c51096e`): `onnxruntime` 1.20.0 → **1.27.0**, `webrtc-sdk` 125.6422.07 → **144.7559.09** (versions read from Maven Central metadata, not guessed). Verified by parsing ELF program headers straight out of the APK — all four previously-flagged libs now report `max LOAD p_align = 16384`. `zipalign -c -P 16` was inconclusive on this machine; the Python ELF check is the reliable one and is worth keeping. **Cost: all-ABI debug APK 177 MB → 256 MB**, which raises the priority of 22.3.
- `[x]` **23.2 Post-bump verification — COMPLETE 2026-07-25.** The missing spoken round-trip is done: "What is the weather in Charlotte, North Carolina?" through the PC speakers produced a real `get_weather` tool call (`Used get_weather / completed`) and a spoken answer with live data (70.7 °F, overcast, feels like 75.8, high 78.5) on the bumped `onnxruntime` 1.27.0 / `webrtc-sdk` 144.7559.09. **The rig problem blamed below was not the rig** — see the working procedure in Gotchas. Earlier partial result: Confirmed on the Tab S9 FE: app launches with **no** `dlopen`/`UnsatisfiedLink`/alignment errors, the wake FGS runs (`types=0x80`), `model sync` succeeds, a session mints, and `WebRtcTransport: oai-events channel state: OPEN`. **Not yet confirmed: a full spoken round-trip on the new deps** — the TTS-through-speakers rig stopped reaching the tablet mic after the reboot (PC audio routing/volume), which is a test-rig problem, not an app one. Re-run with the audio rig fixed: max PC output volume, tablet media volume ~3/15, then tap-to-talk and ask for the weather.

### M24 — Verification harness  `[x]`  (green in CI 2026-07-25)

- `[x]` **24.1** JVM unit tests for each M21 defect — done for 21.1 (`TranscriptUploaderTest`, 7 cases: final-flush-always-posts, seq/role/engine, blank-turn drop, no-sessionId, full-batch flush, failure-never-propagates, mode→engine mapping) and now 21.0/21.3/21.4. 21.2's AEC coverage landed as `EchoGateTest` + `VoiceAudioProcessingTest`, and 21.5's as `AuthInterceptorTest` (8 cases). 165 JVM tests green.
  - **21.3 wake-phrase resolution:** `ModelManager`/`WakeWordCatalogRepository`/`SettingsScreen` had the comparison (selected catalog id vs. loaded head model) inlined in the composable — not testable without Compose/Robolectric. Extracted the pure decision into `ui/settings/WakePhraseResolution.kt` (`resolveWakePhrase(selectedId, active: WakeModelRef) -> WakePhraseResolution(activeId, mismatched)`), mirroring SettingsScreen's existing `activeWake.isNotEmpty() && doc.wakeWord.isNotEmpty() && activeWake != doc.wakeWord` check exactly. Tested in `WakePhraseResolutionTest.kt`: downloaded model matching the selection, selection with no model synced yet (mismatch, warns with the truthful `hey-jarvis` phrase), bundled-offline fallback resolving truthfully, a downloaded custom-trained phrase, and the empty-selection edge. **Not wired into SettingsScreen.kt** (owned by the concurrent M21.2 agent this session) — the extraction mirrors the inline logic byte-for-byte today, but SettingsScreen should be pointed at this function directly as a follow-up so one definition backs both the UI and the test.
  - **21.0/21.4 service/prefs state machine:** same problem — the paused/resume decision (`wakePaused = wakeServiceEnabled && !wakeServiceRunning`, the switch's `onCheckedChange`) was inline in SettingsScreen.kt. Extracted to `wake/WakeSwitchState.kt` (`wakeSwitchDisplay(serviceEnabled, serviceRunning) -> OFF|RUNNING|PAUSED`, `decideWakeSwitchAction(toggledOn, serviceEnabled, serviceRunning) -> START|STOP`). `WakeSwitchStateTest.kt` covers: intent-on-not-running shows PAUSED with a resume action, actually-running shows RUNNING regardless of the persisted intent, and — the direct M21.0 regression guard — toggling on resolves to START identically whether `serviceEnabled` is already true or still false (i.e. START is never gated on a flag only the service itself sets). Same SettingsScreen.kt wiring caveat as above. Also added `WakePreferencesTest.kt` (previously untested): every setter mirrors synchronously into its `MutableStateFlow`, sensitivity clamps into 0..1, defaults match `settings.schema.json`.
- `[x]` **24.2** Instrumented tests added under `app/src/androidTest/` (source set created fresh this session — did not previously exist), scoped honestly to what's verifiable offline in CI (no signed-in account, no live realtime backend):
  - `OnboardingToSignInGateTest` — drives every onboarding step's decline/skip path via `MainActivity` and asserts the wizard lands on `LoginScreen`, not the signed-in home scaffold. Boundary stated in the test doc comment: reaching an actually signed-in home needs a live LWA round trip, not exercised.
  - `wake/WakeServiceLifecycleTest` — the one flow that had to run on real Android: clears `WakePreferences`' persisted `serviceEnabled` to false (reproducing a fresh install), grants `RECORD_AUDIO` via `UiAutomation`, calls `WakeWordService.start()` (the exact call the Settings switch makes), and asserts `WakeWordService.runningFlow` reports `true` — the M21.0/M21.4 regression this milestone exists to catch, and the one no JVM test can (`WakeWordService` is a real `android.app.Service`).
  - `TapToTalkConnectingStateTest` — hosts `ConversationScreen` on a new test-only `TestHarnessActivity` (`@AndroidEntryPoint`, declared in `src/androidTest/AndroidManifest.xml`) because the real conversation screen sits behind the onboarding/sign-in gate and there's no account to pass it with in CI; taps the tap-to-talk orb and asserts the mic state machine reaches `CONNECTING`. Deliberately stops there — never asserts `LISTENING`, since that depends on a live backend round-trip that would make the test flaky against network conditions.
  - **Hand-off note:** no new Gradle dependencies were needed for any of this (`androidx.test.ext:junit`/`espresso-core`/`compose-ui-test-junit4` already present cover `InstrumentationRegistry`, `UiAutomation.grantRuntimePermission`, and `createAndroidComposeRule`) — build.gradle.kts was not touched.
- `[x]` **24.3** Wired into `.github/workflows/android-release.yml`: kept `workflow_dispatch` for the manual release, added a `push` trigger (path-filtered to `android/**`) so `unit-tests` (`testDebugUnitTest`) and `instrumented-tests` (`connectedDebugAndroidTest` via `reactivecircus/android-emulator-runner`, pinned to `v2.38.0` by commit SHA) run on every relevant push — that's the actual CI signal M24.1 exists for; workflow_dispatch alone would mean nobody notices a break until the next manual release. Emulator: API 35 / `google_apis` / x86_64, matching the app's compileSdk/targetSdk 35 and the local reference AVD `liveninja-test` exactly (confirmed by reading `~/.android/avd/liveninja-test.avd/config.ini`); x86_64 rather than arm64-v8a because GitHub-hosted Linux runners are x86_64 and the debug build is all-ABI by default for exactly this reason (`arm64Only` is opt-in). **Release-gating decision:** `build-and-publish` now `needs: unit-tests` (fast, deterministic — a real regression should block a release) but deliberately does **not** need `instrumented-tests` — an emulator run on a shared CI runner is materially less reliable than a JVM test (boot timeouts, KVM flakiness), and the owner's one physical tester getting a build blocked by a flaky emulator is a worse failure mode than occasionally shipping past an instrumented-test failure unit-tests didn't already catch. `instrumented-tests` still runs on every push so a real regression is visible in the Actions tab, it's just not a required check for the release job. The debug-keystore/AWS/SES steps stayed exactly as they were, untouched, in `build-and-publish` only.

- `[x]` **Near-miss wake scores are now logged** (`c9bb28a`). `OpenWakeWordEngine` logged the score only when it crossed the threshold, so a model scoring 0.4 against a 0.5 threshold looked identical to one scoring 0.0 — which is exactly how the trained model got written off as non-detecting when it actually scores ~0.7. Debug level, peak-only, one line per 5 s above a 0.10 floor so it cannot flood a per-chunk inference loop.

### M25 — Live cost visibility (Android)  `[x]`  (2026-07-25)

Owner-requested: the cost of a live conversation was nowhere on the Android screen, which the 14-minute
runaway echo loop above made pointed — it burned tokens with no on-screen indication.

- `[x]` Android had **none** of the chain the web badge uses: the mint response's `rates` was parsed and thrown away, `response.done` usage was never read, and the final transcript flush carried no `cost`, so Android conversations produced **uncosted CONV rows**. Added `SessionCostTracker` (formula pinned to `web/static/js/conversation.mjs` so the two surfaces cannot disagree about the same session), a badge beside the session timer, and `cost` on the `final:true` flush. Also closes the Android half of WS-1's "costed CONV row" gap.
- `[x]` The subtlety, pinned by a test: **cached tokens are a subset of `input_token_details`, not a sibling** — adding rather than subtracting them bills 1.1M tokens for a 1M-token turn.
- `[x]` A null cost renders **no badge at all** rather than `~$0.000`: nova-bridge surfaces no usage, and showing zero for an unpriced engine is a lie rather than a zero. `Locale.US` is pinned so a comma-decimal locale cannot render `~$0,003`.
- `[x]` **Verified on device 2026-07-25.** The badge rendered `~$0.040` beside the `0:28` timer during a live spoken session. The persistence half is confirmed straight from DynamoDB: the CONV row for that session carries `costUsd=0.023532`, `costTextTokens=4434`, `costAudioTokens=95`, `surface=android` — while today's **earlier** CONV rows (02:04Z, 10:16Z) have no cost attributes at all, which is exactly the before/after. **This also closes WS-1's "confirm the cost-persist chain produces a costed CONV row" for Android.**

## WS-6 — Owner-requested capabilities  `[~]`

Opened 2026-07-25 from live use. Everything here was asked for directly by the owner during
the session that closed WS-5, so it is scheduled work, not backlog.

### M26 — Device session control  `[x]`  (2026-07-25, `54b95c9`)

**Definition of Done:** the user can stop listening and start a fresh conversation both by
hand and by voice, from wherever they are.

- `[x]` **In-app stop control.** The wake FGS notification always had a Stop action, but it is
  `PRIORITY_LOW`/`CATEGORY_SERVICE` and is **not visible while the app is open** — so from
  inside the app the only off switch was the toggle buried in Settings. The Conversation screen
  now shows "Always listening is on" + **Turn off listening** whenever something really is
  listening (bound to `runningFlow`, never the persisted intent). **Verified on hardware:**
  service stops, `serviceEnabled=false` persists, still stopped 25 s later.
- `[x]` **Stop actually sticks.** `onStartCommand` returned `START_STICKY`, so after an OEM
  task-kill Android recreated the service with a **null intent** — which was treated as an
  explicit start and re-wrote `serviceEnabled = true`. Listening the user had switched off came
  back on its own, which is why the owner had to reboot the tablet. A sticky restart now
  honours the stored intent (`wake/WakeStartDecision.kt`, 6 tests).
- `[x]` **`stop_listening` + `start_new_conversation` voice tools.** Declared server-side with
  the new `Definition.DeviceLocal` flag so the manifest advertises them, executed by the
  Android client, which intercepts before `POST /api/v1/tools/invoke` — the backend cannot stop
  a microphone it does not hold. The action is deferred until the assistant finishes speaking,
  because stopping the session when the tool fires cuts off the reply explaining what happened.
  `start_new_conversation` is a real stop/start, not a transcript clear: the session id is what
  the backend keys `LOG#`/`CONV` on, so only a new session earns its own History row.
- `[ ]` **Not yet verified by voice** — the tools are wired and unit-tested, but nobody has
  said "stop listening" to a live session yet.
- `[ ]` **Web parity.** Web has no equivalent of the tools. Its manual control exists but is
  labelled **"Use Wake Word"** beside the mic, which is why the owner could not find it —
  relabel to read as a listening state. (S)

### M27 — Volume control  `[ ]`

- `[ ]` **S** — Voice-controllable volume. **Owner decision taken:** all streams are
  addressable, **media is the default** when unspecified. Device-local like M26, so it routes
  through the same `DeviceLocal` interception rather than the backend.

### M28 — Camera: photo + video capture  `[ ]`

**Owner decisions taken 2026-07-25:** back camera by default and overridable per request
("record a 30 second video on back camera"); **no confirmation before capture — the voice
command IS the confirmation**; stored in the existing **S3** user bucket; and surfaced in
**Files** alongside deliverables.

- `[ ]` **O** — `CAMERA` permission (the app holds none today) + onboarding/settings copy. The
  privacy posture changes materially: an assistant that captures on command without a
  confirmation step is a deliberate choice and must be stated plainly in-app.
- `[ ]` **F** — Capture path: photo, and video defaulting to **60 s** unless the user states a
  duration. Camera selection parsed from the request, defaulting to back.
- `[ ]` **S** — Upload to the user bucket + a `FILE#` row so it appears in Files and in the
  conversation. **Cost note:** 60 s of video is roughly 50–100 MB, which changes the storage
  profile — worth a retention decision before this ships.
- `[ ]` **S** — Tools declared in the manifest (`take_photo`, `record_video`) so the model can
  discover them.

### M29 — OpenAI budget warning  `[ ]`

- `[ ]` **Owner decision needed.** Warn when the OpenAI budget drops under **$20**. OpenAI
  spend is not visible from the AWS account and their billing API exposes no reliable
  remaining-credit figure, so there are two shapes: **(a)** sum the `costUsd` this project
  already persists on every `CONV` row against an owner-set budget — self-contained, no new
  credential, and it reuses the M25 chain; or **(b)** poll OpenAI's billing endpoint, needing
  an admin API key in SSM and possibly not returning the wanted number. **Recommendation: (a).**

### M30 — Settings revamp (web + Android)  `[ ]`  (owner: **both** surfaces)

- `[ ]` **Owner spec, verbatim:** each settings section becomes a bar you tap to drop down the
  options beneath it (accordion). The settings affordance stops being a circle on the right and
  becomes a **vertical bar covering 40% of the window height** on the right; when open, a
  matching **40% vertical bar on the left** closes it.
- `[ ]` **O** — Mandatory multi-persona design pass BEFORE code (house rule: non-trivial UI).
  Produce the terse flow spec first — section grouping, expand/collapse behaviour, keyboard and
  screen-reader semantics for the accordion, and what the open/close bars announce.
- `[ ]` **S** — Web implementation. `[ ]` **S** — Android implementation. Android's Settings was
  just restructured into lazy sections under M22.1, which is a good base for the accordion but
  means the two surfaces must be kept deliberately consistent rather than drifting.

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
- **The PC-speakers → tablet-mic voice rig works; use this exact procedure.** Windows master volume ~95% (it was already 100%), tablet media volume **12/30 set with `cmd media_session volume --stream 3 --set 12`** — note `media volume ...` does **not** exist on this device and fails silently, so verify with `dumpsys audio` (`streamVolume:` in the `STREAM_MUSIC` block). Speak with `System.Speech.Synthesis.SpeechSynthesizer` at `Volume=100, Rate=-2`. Allow ~7 s after tapping the mic for the session to connect before speaking. **Always end the session** — tap the stop control (≈`input tap 1089 1993`), not `am force-stop`, because force-stop kills the process before the `final:true` flush and you lose the costed CONV row.
- **A wake-word test needs a freshly started wake service.** Detection failed three times in a row when the service had been running through a preceding WebRTC session, then succeeded immediately after an app restart — Samsung's stack logs `BWU@ApWakeupService: isRecognitionAllowed? false` in that window. Restart the app before concluding a wake model does not work.
- **A post-deploy job in `deploy.yml` MUST use `if: always() && needs.deploy.result == 'success'`.** `deploy`'s own upstream container jobs are normally skipped, so `deploy` only runs because of its own `always()`; GitHub then propagates "skipped" down the chain and a dependent job **without** `always()` is silently skipped even though deploy succeeded — zero steps, job reports `skipped`, nothing fails. Cost one round trip on the new `web-quality` job. Also: `continue-on-error: true` makes a job report **success even when its steps failed**, so check step conclusions, not the job badge, before believing a gate passed.
- **A zero-parameter tool used to break the Gemini schema contract.** `renderManifest` emitted
  `"properties":{}`, and the Gemini Go SDK omits an empty property map when it marshals the same
  schema into a minted token's constraints — so the raw wire setup frame and the SDK-typed
  constraints disagreed about a tool the model must see identically. Fixed at the source in
  `renderManifest` (omit `properties` when empty), NOT in the Gemini sanitizer, because
  `gemini_mint_test.go` asserts the sanitized copy stays exactly equal to the manifest and its
  own comment says divergence is drift to fix upstream. Surfaced the moment the catalog gained
  its first parameterless tools.
- **Adding a tool trips three guards on purpose.** `tool_manifest_test.go` pins the catalog
  count, `persona_tool_coverage_test.go` requires the tool be named in `coreInstructions` (or
  explicitly allow-listed as unmentioned), and the Gemini schema tests require both Gemini paths
  to agree. Budget for all three; they are the reason a new tool cannot be silently
  undiscoverable to the model.
- **`aws` CLI under git-bash mangles a leading-slash argument** into a Windows path — a log group
  like `/live-ninja/lambda/web` fails validation. Prefix the command with `MSYS_NO_PATHCONV=1`.
  Also note the log groups are `/live-ninja/lambda/<fn>`, NOT `/aws/lambda/<fn>`.
- **Broker mint slots:** 3 concurrent sessions, ~10-min TTL. Burned slots make retests wait — budget for it.
- **GitHub "cancelled" runs** = queue replacement by a newer push, not a failure.
