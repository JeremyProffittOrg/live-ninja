# Live Ninja Android Same-Day Revamp — Execution Plan (2026-07-20)

**Goal:** installable debug APK link emailed to proffitt.jeremy@gmail.com TODAY, containing: crash fixes, screen-off/locked voice sessions, HAL 9000 theme, verbose exportable logging. Style picker (ninja/minimal/terminal) and polish ship in a second build if time allows.

**Specs of record** (all decisions pre-baked there; agents read them, never ask questions):
`C:\Users\Jeremy\AppData\Local\Temp\claude\c--dev-live-ninja\3a08fce1-9abd-45ca-8f09-a5b9964f137e\scratchpad\reviews\00-requirements-decisions.md` plus `01-platform-crash.md`, `02-voice.md`, `03-theme.md`, `04-logging-delivery.md` (same directory).

Status markers: `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked.

---

## Locked design decision: session FGS architecture

**Decision: extend `WakeWordService` with a SESSION run mode (02-voice §B). No separate `RealtimeSessionService`.** Rationale: (1) Android 12+ background-FGS-start rules — re-calling `ServiceCompat.startForeground()` on the *already-running* wake FGS with expanded types (`microphone|mediaPlayback`) is unambiguously legal, whereas starting a *new* FGS at wake time with the screen off relies on the "app has an active FGS" exemption chain that is under-documented and OEM-variable. (2) Mic while-in-use continuity — Android 11+ delivers silence to microphone FGS instances judged background-started; `WakeWordService` already holds a valid while-in-use mic grant, and continuing capture in the same service carries that grant through the session; a second service started while locked risks silent-mic classification. For the UI-tap entry path (app foreground, wake service disabled), starting `WakeWordService` from the visible Activity is also while-in-use eligible — so one code path covers both entry points. (3) Notification UX — one notification updated in place ("Listening…" → "Conversation live — End") beats two stacked FGS notifications. (4) Blast radius — `WakeWordService` already owns the run-mode state machine (WakeWordService.kt:130-177), supervision loop, and notification builder; a fourth mode is additive, while a second service would duplicate all of that plus need cross-service mic-handoff coordination. Mitigation for the "god service" concern from 01-platform: all session *logic* lives in a new `@Singleton SessionOrchestrator`; the service only reflects mode and foreground-type. **All tasks below are written against this choice.** Everything else from 01-platform §B (audio focus, wakelock, wake-screen paths, keep-screen-on) is retained unchanged.

---

## File-ownership matrix (parallel-conflict prevention)

Mechanism: **single-owner assignment + explicit sequencing** (no worktrees needed — parallel agents have disjoint file sets; shared files each have exactly one owner per phase):

| Shared file | Owner | Mechanism |
|---|---|---|
| `android/app/src/main/AndroidManifest.xml` | M2 (Voice) during wave 1; M6.1 (Integration) afterward | Sequencing: only M2.2 touches it in wave 1; M3's FileProvider entry is deferred to M6.1, which runs after M2 completes |
| `ui/state/SettingsStore.kt` | M1.2 only | Schema-first: ALL new keys for all workstreams (voice toggles + appStyle + diagnostics) land in one task before dependents start; no other task ever edits this file today |
| `ui/screens/SettingsScreen.kt` (+`ui/settings/SettingsViewModel.kt`) | M6.2 only, then M8.2 | Deferral: wave-1 workstreams add store keys/logic only, zero Settings UI; all Settings UI lands in the serialized integration milestone; style picker appended later by sequencing |
| `MainActivity.kt` | M2.7 only | Single owner: voice workstream applies its own changes AND the theme's fixed-constant edge-to-edge call (values pinned in 03-theme §C, no coordination needed) |
| `android/app/build.gradle.kts` | M5.2 only (then M8.4 for WorkManager dep, post-ship serial) | Single owner + sequencing |
| `ui/LiveNinjaRoot.kt` | M4.4 (theme) in wave 1; M6.4 adds log-viewer route after | Sequencing |
| `wake/WakeWordService.kt` | M2 exclusively (crash-guard fix A2 is MOVED from the stability workstream into M2.1) | Single owner |
| `LiveNinjaApplication.kt` | M1.3 (wave 1); M6.4 afterward (eager LogSink instantiation) | Sequencing |
| `ui/conversation/ConversationViewModel.kt` | M2.6 (voice); theme's MicUiState→OrbState mapping lives in `ConversationScreen.kt` (M4.3), not the ViewModel | Boundary assignment |

Merge/commit rules: all agents work on `main` directly but on disjoint files; each milestone ends with one logical commit (clear message, **no Co-Authored-By trailers**) and a push (owner standing rule). Wave-1 milestones commit independently; if two finish simultaneously, commit order M1 → M3 → M2 → M4 → M5 (dependency order).

---

## Verification strategy (no device attached)

- Per-milestone gates: `./gradlew testDebugUnitTest assembleDebug` (working dir `android/`); `lintDebug` at M6.
- New unit tests required: TokenStore self-heal (via injectable prefs-factory seam), redaction scrubber (fixtures from `AuthInterceptor`/`TokenAuthenticator` header shapes), SessionOrchestrator state machine (fake flows/clock), SettingsStore serialization round-trip, LogSink rotation/ring behavior.
- Runtime verification is best-effort via the OPTIONAL emulator milestone (M12); if it succeeds, a smoke gate is inserted before M7 dispatch. If it fails, ship on static gates — crash fixes are defensive by design (00-requirements).

---

## Milestone graph

```
Wave 1 (parallel, start now):  M1  M2  M3  M4  M5     [M11 owner-assist, M12 emulator: parallel, non-blocking]
  intra-wave deps: M3.1 → (M2.3+); M1.2 → (M2.3, M2.7, M4.1)
Serial:  M6 (integration + first shippable build) → M7 (SHIP v1) → M8 (picker + polish) → M9 (optional reship) → M10 (EOD wrap)
```

---

## M1 — Stability: crash-on-load fixes + settings schema `[x]` (done 09:30 ET, commits 704e91d + 8aa6a7f + 8415e17; all tests green; TokenStore self-heal + storeReset flow + STORAGE_RESET login notice; WARNING noted: something ran repo-wide git reset --hard loops during wave 1 — all completed work is committed; M6 integrator must verify no lost edits)

**Agent:** Stability agent. **DoD:** app cannot die from credential-store corruption; all new settings keys exist with owner defaults and round-trip tests pass; `testDebugUnitTest` + `assembleDebug` green; committed & pushed.

- `[ ]` **M1.1** (Opus) TokenStore self-healing factory per 01-platform §A1: extract injectable `prefsFactory` seam in `auth/TokenStore.kt`; on `GeneralSecurityException|IOException` → `deleteSharedPreferences("liveninja_auth")` + `KeyStore("AndroidKeyStore").deleteEntry("_androidx_security_master_key_")` → retry once; on persistent failure `session()`/`accessToken()` return null, `saveSession` no-ops, emit SignedOut signal. New test `auth/TokenStoreSelfHealTest.kt` (throw-once→heals, throw-always→null-mode). *Files:* `auth/TokenStore.kt`, new test. *Verify:* unit tests. *Deps:* none.
- `[ ]` **M1.2** (Sonnet) Settings schema — add ALL new keys to `ui/state/SettingsStore.kt` in one pass, with these PINNED Kotlin shapes (dependents code against them; do not deviate): `lockedSessions: Boolean = true`, `wakeScreenOnWake: Boolean = true`, `keepScreenOn: Boolean = false`, `appStyle: String = "hal9000"`, `diagnostics: DiagnosticsConfig(enabled: Boolean = true, minLevel: String = "VERBOSE", categories: Map<String, Boolean> = all 8 on)` + matching setters (additive JSON keys, 01-platform §B-iv + 04 §A4 + 03 note; safe server-side — Android has no PUT /v1/settings sync call site today). Serialization round-trip + unknown-key-tolerance unit tests. *Files:* `ui/state/SettingsStore.kt`, new `ui/state/SettingsStoreTest.kt`. *Verify:* unit tests. *Deps:* none. **No other task edits SettingsStore.kt today.**
- `[ ]` **M1.3** (Sonnet) `CoroutineExceptionHandler` (log + `AuthState.SignedOut`) on the `AuthRepository` scope; confirm `LiveNinjaApplication.onCreate` start path survives store failure. *Files:* `auth/AuthRepository.kt`, `LiveNinjaApplication.kt`. *Verify:* build + existing auth tests. *Deps:* M1.1.
- `[ ]` **M1.4** (Sonnet) Auto-wipe explanation: one-line notice on login screen after forced re-login from store wipe ("Sign-in data was reset after a storage error — please sign in again."). *Files:* `auth/AuthState.kt` (or `AuthUiBridge.kt`), `ui/onboarding/LoginScreen.kt`. *Deps:* M1.1.
- `[ ]` **M1.5** (Haiku) Gate: `./gradlew testDebugUnitTest assembleDebug`; commit "android: self-healing token store, auth crash guards, settings schema" + push.

**Implementation notes:**
_(placeholder)_

---

## M2 — Voice: screen-off/locked sessions, wake→session handoff `[x]`

**Agent:** Voice agent. **DoD:** wake detection and assist trigger both start a real session with screen off/locked (per `lockedSessions` toggle); mic FGS never crash-loops; audio focus + BT routing + session wakelock in place; wake engine resumes immediately post-session; `assembleDebug` + orchestrator tests green; committed & pushed.

- `[x]` **M2.1** (Opus) `WakeWordService` hardening (crash A2, moved here for file ownership): top of `onStartCommand` — if `RECORD_AUDIO` not granted → post tap-to-resume notification, `stopSelf()`, `START_NOT_STICKY`; wrap `startForeground` in try/catch (`SecurityException`, `ForegroundServiceStartNotAllowedException`) with same degrade path. *Files:* `wake/WakeWordService.kt`. *Verify:* build. *Deps:* none.
- `[x]` **M2.2** (Sonnet) Manifest (sole wave-1 owner): `WakeWordService` `foregroundServiceType="microphone|mediaPlayback"`; add `FOREGROUND_SERVICE_MEDIA_PLAYBACK`, `USE_FULL_SCREEN_INTENT`, `REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`, `WAKE_LOCK` permissions; high-importance FSI notification channel constants where needed. *Files:* `AndroidManifest.xml`. *Deps:* none.
- `[x]` **M2.3** (Opus) New `realtime/SessionOrchestrator.kt` (@Singleton): collects `WakeEvents.detections` + `AssistantEvents.triggers` — PRIMARY duplicate guard = ignore ALL triggers while a session is starting/active; timestamp dedupe only for AssistantEvents replay=1 re-delivery. Fixes the zero-collector defect. On trigger: acquire `PARTIAL_WAKE_LOCK` tag `liveninja:realtime-session` (30-min hard cap), pause wake engine, earcon = `android.media.ToneGenerator(STREAM_NOTIFICATION, ~80).startTone(TONE_PROP_ACK)` ~150ms (NO raw asset today — pre-baked decision), `AudioFocusRequest(GAIN_TRANSIENT, USAGE_VOICE_COMMUNICATION/CONTENT_TYPE_SPEECH)`, then `RealtimeSessionCoordinator.start()`; on `connected=false`/stop: abandon focus, release wakelock, **immediately** resume wake engine (bypass 60s ENGINE_RETRY_MS); stamp `launchedWhileLocked`; locked/asleep gate: `lockedSessions=false` ⇒ ignore wake triggers when `!powerManager.isInteractive || keyguard.isKeyguardLocked` (screen-asleep counts even with no lock screen configured). **Instantiation:** Hilt singletons are lazy — inject SessionOrchestrator into BOTH `WakeWordService` and `MainActivity` (both M2-owned) so the assist/manual path works with the wake service disabled; do NOT touch LiveNinjaApplication.kt for this. State-machine unit tests with fake flows/clock: new `realtime/SessionOrchestratorTest.kt`. *Deps:* M1.2, M3.1 (use `LNLog` from birth).
- `[x]` **M2.4** (Opus) `WakeWordService` SESSION mode: 4th run mode driven by `coordinator.connected`; re-call `ServiceCompat.startForeground` with `microphone|mediaPlayback` when session begins — wrapped in the SAME try/catch guard as M2.1 (OEM variance); on failure continue the session under the existing mic-only type. Notification updates in place ("Conversation live" + End action → orchestrator.stop); wake-screen path on detection: if `wakeScreenOnWake` and ROLE_ASSISTANT held → `VoiceInteractionService.showSession()` (existing `LiveNinjaSession` path); else FSI notification (API 34+: check `NotificationManager.canUseFullScreenIntent()` first); else audio-only + heads-up (01-platform §B-ii). *Files:* `wake/WakeWordService.kt`. *Deps:* M2.1, M2.3.
- `[x]` **M2.5** (Opus) Transport audio: fix `configureAudioForCall` forced-speaker — prefer `availableCommunicationDevices` order BT_SCO → wired → speaker; mirror in Gemini twin; focus request/abandon plumbed from orchestrator (transport exposes hooks, no dup logic). *Files:* `realtime/WebRtcTransport.kt`, `realtime/GeminiLiveTransport.kt`. *Deps:* M2.3. **PRE-AUTHORIZED CUT #1: if deadline pressure, demote to M8 (forced-speaker is today's shipped behavior anyway).**
- `[x]` **M2.6** (Sonnet) UI attach with screen off→on: new `realtime/TranscriptStore.kt` (@Singleton, StateFlow of turns); `RealtimeSessionCoordinator` accumulates there instead of ViewModel; `ConversationViewModel` renders TranscriptStore and derives `micState` from `coordinator.connected` on init. *Files:* new `realtime/TranscriptStore.kt`, `realtime/RealtimeSessionCoordinator.kt`, `ui/conversation/ConversationViewModel.kt`. *Deps:* M2.3.
- `[x]` **M2.7** (Sonnet) `MainActivity.kt` (sole owner): `FLAG_KEEP_SCREEN_ON` via DisposableEffect watching `keepScreenOn` flow; clear `setShowWhenLocked(false)` when `wakeScreenOnWake` off (sticky-flag hygiene); apply theme's `enableEdgeToEdge(SystemBarStyle.dark(0xFF050507))` when `appStyle=="hal9000"` (constants pinned by 03-theme §C — no coordination with M4 required). *Files:* `MainActivity.kt`. *Deps:* M1.2.
- `[x]` **M2.8** (Haiku) Gate: `./gradlew testDebugUnitTest assembleDebug`; commit "android: session orchestrator, WakeWordService SESSION mode, screen-off voice" + push.

**Implementation notes:** DONE (all M2.1–M2.8 + M2.5 implemented; gate green — `testDebugUnitTest assembleDebug` BUILD SUCCESSFUL; SessionOrchestratorTest 7/7, RealtimeSessionCoordinatorTest 9/9).
- **M2.3 arch:** `SessionOrchestrator` (@Singleton, Hilt) wraps a testable `SessionOrchestratorCore` (LogSink/LogSinkCore precedent) — Android side-effects behind `SessionEffects` (wakelock/focus/earcon) + `DeviceLockState`, so the state machine unit-tests with no Robolectric. Injects `RealtimeSessionController` directly (bound non-optional by RealtimeModule @Binds). Duplicate guard = phase (IDLE/STARTING/ACTIVE); replay dedupe by `AssistTrigger.timestampMillis`. Wake-originated sessions also emit a `WAKE_WORD` AssistTrigger for UI nav + KeyguardGate. Injected into BOTH WakeWordService and MainActivity.
- **M2.4 wake-screen:** implemented via FSI notification (launch MainActivity ACTION_ASSIST, shows over keyguard) + `canUseFullScreenIntent()` check on API 34+, high-importance `wakeword_alert` channel, heads-up fallback. **DEVIATION:** the "role held → `VoiceInteractionService.showSession()`" optimization is NOT implemented because it requires editing `LiveNinjaVoiceInteractionService.kt` (not in M2's file set) to expose the running instance; the FSI path is universal and covers the requirement. SESSION run mode = 4th `combine` input `orchestrator.sessionActive`; `awaitCancellation()` holds the branch, exit → CONTINUOUS starts engine immediately (bypasses 60 s retry). `startForegroundGuarded` catches SecurityException + IllegalStateException (FGS-not-allowed superclass, resolves on minSdk 29). New notification strings inlined in the service (strings.xml not in M2's file set).
- **M2.6:** `TranscriptStore` (@Singleton) holds turns process-wide; coordinator writes deltas/tool-chips there (+ clears on start); ConversationViewModel renders the store and derives micState from `controller.connected` transitions (init collector, `lastConnected` guard). RealtimeSessionCoordinatorTest updated for the new 6th constructor param (`TranscriptStore()`).
- **M2.5 (CUT #1) IMPLEMENTED** (not demoted): `configureAudioForCall` in both transports now prefers BT_SCO → wired/USB headset → speaker via `availableCommunicationDevices`; audio focus stays centralized in the orchestrator (no transport dup).
- **M2.7:** MainActivity injects SessionOrchestrator (manual/assist path); `enableEdgeToEdge(SystemBarStyle.dark(0xFF050507))` gated on `appStyle=="hal9000"`; keep-screen-on DisposableEffect on `keepScreenOn`; setShowWhenLocked(false) hygiene when `wakeScreenOnWake` off.
- **Build env note:** shell `JAVA_HOME` was invalid; built with `JAVA_HOME="/c/Program Files/Eclipse Adoptium/jdk-17.0.19.10-hotspot"`.

---

## M3 — Logging core `[x]` (done 09:20 ET, commits 99de377 + f52ae25; 30/30 tests green; deviations: LogSinkCore split for JVM testability, LogViewerViewModel co-located in LogViewerScreen.kt, entriesFlow added to LogSink)

**Agent:** Logging agent. **DoD:** `LNLog`/`LogSink` operational with redaction proven by tests; export zip + viewer composable built (manifest/nav hookup deferred to M6); tests + build green; committed & pushed.

- `[ ]` **M3.1** (Sonnet) **Runs first — M2.3+ depends on it.** New package `ninja.jeremy.liveninja.log`: `LNLog.kt` (facade matching `android.util.Log` signatures; `@Volatile var sink: LogSink?` — null ⇒ logcat passthrough only; `LogSink.init{}` self-registers into `LNLog.sink`; eager instantiation happens in M6.4, NOT wave 1 — do not touch LiveNinjaApplication.kt), `LogCategory.kt` (WAKE, AUDIO, REALTIME, AUTH, TOOLS, UI, NET, GENERAL), `LogSink.kt` (@Singleton: 2000-entry ring, rotating `filesDir/logs/liveninja-current.log` → gz at 5MB keep 10, single Dispatchers.IO writer, format `ts|level|category|tag: message [stack]`), `Redactor.kt` (Bearer regex, JWT `eyJ…` shape, Authorization/X-Api-Key/Cookie value-redaction — applied BEFORE buffer/disk). Tests: `log/RedactorTest.kt` (fixtures mirroring `net/AuthInterceptor.kt` + `net/TokenAuthenticator.kt` header construction), `log/LogSinkTest.kt` (ring cap, rotation, level/category filtering). *Deps:* none.
- `[ ]` **M3.2** (Sonnet) Wire diagnostics config: `LogSink` observes `SettingsStore` diagnostics flow (keys from M1.2) for enabled/minLevel/category gating; defaults VERBOSE/all-on. *Files:* `log/LogSink.kt`. *Deps:* M1.2, M3.1.
- `[ ]` **M3.3** (Sonnet) Export: new `log/LogExporter.kt` — flush ring → zip current+rotated → `liveninja-logs-<ts>.zip` → `ACTION_SEND` (application/zip) via FileProvider authority `ninja.jeremy.liveninja.fileprovider`; create `res/xml/file_paths.xml` exposing `files/logs/`. **Manifest `<provider>` entry deferred to M6.1.** *Deps:* M3.1.
- `[ ]` **M3.4** (Sonnet) In-app viewer: new `ui/screens/LogViewerScreen.kt` reading ring live (filter by category/level, copy-to-clipboard). Standalone composable only — nav route added in M6.4 (LiveNinjaRoot owned by M4 in wave 1). *Deps:* M3.1.
- `[ ]` **M3.5** (Haiku) Gate: tests + `assembleDebug`; commit "android: LNLog/LogSink verbose logging with redaction + export" + push.

**Implementation notes:**
_(placeholder)_

---

## M4 — HAL 9000 theme `[x]` (done 09:14 ET, commit 8907467; TOOLCALL fully implemented — cut #3 unused; note: M8 must pass appStyle from settings into LiveNinjaTheme when wiring the picker; JAVA_HOME env var on this machine is stale — real JDK at C:/Users/Jeremy/jdk-temurin17/jdk-17.0.19+10, pass explicitly if Gradle complains)

**Agent:** Theme agent. **DoD:** HAL is the default look (tokens, orb, nav bar, launcher icon) exactly per 03-theme spec; accessibility constraints honored (no small text on red); build + lint green; committed & pushed.

- `[ ]` **M4.1** (Sonnet) Tokens: rewrite `ui/theme/Theme.kt` — HAL M3 colorScheme per 03-theme §A (background #050507, primary teal #22e0d0, tertiary red #e32636 decorative-only, etc.), `LocalLiveNinjaColors` static composition local (textDim/success/warn/track/accentGlow/orbCoreStops/backgroundGradient Brush), `dynamicColor=false` under HAL, HAL pins dark; structure as a style registry keyed by `appStyle` (only hal9000 populated today; ninja/minimal/terminal slots for M8). *Deps:* M1.2.
- `[ ]` **M4.2** (Opus) `HalOrb`: new `ui/theme/HalOrb.kt` — `OrbState{IDLE,LISTENING,THINKING,SPEAKING,TOOLCALL,ERROR}`, core radial brush (px-space via drawWithCache, center 50%,42%, exact colorStops from spec), 3-layer glow approximation, ring animations (24s idle spin; SPEAKING 3 rings 2400ms stagger 0/350/700; TOOLCALL reverse-spin+scale; ERROR static #ff5c72 ring), reduced-motion via `ANIMATOR_DURATION_SCALE==0f`. *Deps:* M4.1. **PRE-AUTHORIZED CUT #3: TOOLCALL may render as THINKING if deadline pressure.**
- `[ ]` **M4.3** (Sonnet) Orb placement: `ui/screens/ConversationScreen.kt` — replace IdleHero FilledIconButton (~:234-249) with persistent orb (200dp idle → 96-118dp live above TranscriptList); MicUiState→OrbState mapping per 03-theme (mapping lives HERE, not in ConversationViewModel); ≥48dp tap target; ControlBar unchanged. *Deps:* M4.2.
- `[ ]` **M4.4** (Sonnet) Nav bar: `ui/LiveNinjaRoot.kt` — NavigationBarItemDefaults teal selected/indicator@.14, textDim unselected, container #180c0e + 1dp hairline drawBehind; 5 tabs unchanged. *Deps:* M4.1.
- `[ ]` **M4.5** (Sonnet) Launcher icon: port `web/static/icons/ninja.svg` flat shapes → `res/drawable/ic_launcher_foreground.xml` (108dp canvas, ÷4.74 scale), background #050507, monochrome reuses foreground; mipmap-anydpi-v26 XML only. *Deps:* none.
- `[ ]` **M4.6** (Haiku) Gate: `assembleDebug` (+ compose previews compile); commit "android: HAL 9000 theme — tokens, orb, nav, icon" + push.

**Implementation notes:**
_(placeholder)_

---

## M5 — CI delivery pipeline prep `[~]` (launched 08:10 ET)

**Agent:** Delivery agent. **DoD:** `android-release.yml` merged; debug keystore stored as GH secret; APK slimmed to arm64; SES-permission path resolved with fallback; ready for one-click dispatch at M7.

- `[ ]` **M5.1** (Haiku) Keystore secret: `android/keystores/debug.keystore` exists locally (2,666 B) and is gitignored (`android/.gitignore:9`) — CI checkout will NOT contain it; AGP auto-keystore would change signature vs the phone's installed build (INSTALL_FAILED_UPDATE_INCOMPATIBLE risk). **Locked resolution:** `base64 -w0 android/keystores/debug.keystore` → GitHub secret `ANDROID_DEBUG_KEYSTORE_B64` via `scripts/set-secret.sh`. `android/app/build.gradle.kts:34-42` already picks up `keystores/debug.keystore` when present — the workflow just decodes the secret to that path; **zero Gradle signing changes**. *Verify:* `gh secret list`. *Deps:* none.
- `[ ]` **M5.2** (Haiku) APK size + version: `android/app/build.gradle.kts` — abiFilters GATED behind a property so local/emulator builds stay all-ABI (x86_64 emulator would hit INSTALL_FAILED_NO_MATCHING_ABIS otherwise): `if ((findProperty("liveninja.arm64Only") as String?)?.toBoolean() == true) ndk { abiFilters += "arm64-v8a" }`; CI passes `-Pliveninja.arm64Only=true` (183MB → ~50-60MB). Also bump `versionCode = 3`, `versionName = "0.2.0-hal"` (side-grade clarity). Sole build.gradle.kts owner today. *Verify:* local `assembleDebug -Pliveninja.arm64Only=true`, check APK size + `lib/` contents; then plain `assembleDebug` still all-ABI. *Deps:* none.
- `[ ]` **M5.3** (Sonnet) New `.github/workflows/android-release.yml` (workflow_dispatch, `variant` input default `assembleDebug`, `id-token: write`): checkout → setup-java temurin 17 → android-actions/setup-android@v3 → decode `ANDROID_DEBUG_KEYSTORE_B64` → `android/keystores/debug.keystore` → `./gradlew assembleDebug -Pliveninja.arm64Only=true` (working-directory `android`) → aws-actions/configure-aws-credentials@v4 (role `vars.AWS_DEPLOY_ROLE_ARN` — confirmed the real variable name, deploy.yml:105; us-east-1) → `aws s3 cp` APK → **`s3://live-ninja-assets-759775734231/static/models/downloads/liveninja-<shortsha>.apk`** (`--content-type application/vnd.android.package-archive --cache-control no-cache`). CRITICAL: the link is **`https://live.jeremy.ninja/static/models/downloads/liveninja-<shortsha>.apk`** — CloudFront routes only `/static/models/*` and `/static/vendor/*` to the S3 origin (template.yaml:2125-2156); bare `/static/*` goes to the Lambda's embedded FS and would 404. Unique per-sha filename makes cache policy moot. NOTE deploy.yml has NO path filters (on: push main) — today's milestone pushes each trigger a full backend deploy; expected, harmless, do not "fix" mid-day. *Deps:* none (dispatch waits for M6).
- `[ ]` **M5.4** (Sonnet) Link email: CI email step `continue-on-error: true` (the org-wide gha-deploy role likely lacks ses:SendEmail; it is NOT defined in template.yaml — do not touch template.yaml for this). PRIMARY delivery confirmation = orchestrator sends the link email locally via SES after M7.2 verification. *Deps:* M5.3.
- `[ ]` **M5.5** (Haiku) Gate: YAML parse check; commit "ci: android-release workflow (debug APK → S3 → SES link)" + push.

**Backend confirmation (scope guard):** delivery rides the existing CloudFront `/static/*` → `live-ninja-assets` path; `GET /v1/app/android/latest` is spec-only and NOT needed today; Deliverables Store disqualified (1MiB cap). **No Go changes required — confirmed.**

**Implementation notes:**
_(placeholder)_

---

## M6 — Integration + first shippable build `[ ]`

**Single integration agent, serialized (this milestone owns every shared file).** **DoD:** all wave-1 work merged coherently; Settings UI complete (minus style picker); logging wired app-wide; `lintDebug` + `testDebugUnitTest` + `assembleDebug` green; ONE commit pushed = the shippable tree. *Deps:* M1–M5 complete.

- `[ ]` **M6.1** (Sonnet) Manifest consolidation: add FileProvider `<provider>` (authority `ninja.jeremy.liveninja.fileprovider`, `res/xml/file_paths.xml`); verify M2.2 entries; single coherent manifest. *Files:* `AndroidManifest.xml`.
- `[ ]` **M6.2** (Sonnet) Settings UI (sole SettingsScreen owner): voice section (lockedSessions / wakeScreenOnWake / keepScreenOn toggles), Diagnostics section (master toggle, 5-level radio, 8 category checkboxes + all/none, Export logs → LogExporter, Clear logs w/ confirm), battery-optimization health card + action row (`ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` after `isIgnoringBatteryOptimizations` check). *Files:* `ui/screens/SettingsScreen.kt`, `ui/settings/SettingsViewModel.kt`.
- `[ ]` **M6.3** (Sonnet) Onboarding: one-time battery-exemption prompt step + persistent-green-mic-indicator copy. *Files:* `ui/onboarding/OnboardingScreen.kt`, `ui/onboarding/OnboardingViewModel.kt`, `ui/state/OnboardingStore.kt`. **PRE-AUTHORIZED CUT #2: if deadline pressure, demote to M8 (the M6.2 Settings health card covers the function).**
- `[ ]` **M6.4** (Haiku) Mechanical: import-swap all 66 `android.util.Log` call sites (14 files) → `LNLog` with per-file category; add LogViewer nav route to `ui/LiveNinjaRoot.kt` (entry from Diagnostics section); eager LogSink instantiation (`@Inject lateinit var logSink: LogSink` in `LiveNinjaApplication.kt` — M6 owns it now) so file logging runs from process start.
- **Scheduling note:** M6.1 may start as soon as M2+M3 are done (doesn't need M4/M5).
- `[ ]` **M6.5** (Sonnet) Full gate: `./gradlew lintDebug testDebugUnitTest assembleDebug`; fix all fallout; sanity-check APK size (arm64-only); commit "android: integrate voice/theme/logging revamp — shippable v1" + push.

**Implementation notes:**
_(placeholder)_

---

## M7 — SHIP v1 (APK link emailed) `[ ]`

**Agent:** Delivery agent. **DoD: owner has a working APK download link in his inbox.** *Deps:* M6 (and M12 smoke gate ONLY if the emulator provisioned successfully).

- `[ ]` **M7.0** (conditional, Sonnet) If M12 emulator is up: install the debug APK, cold-launch (no crash), start conversation UI, screenshot HAL theme. Failures here BLOCK dispatch; if emulator never provisioned, skip — non-blocking.
- `[ ]` **M7.1** (Sonnet) `gh workflow run android-release.yml`; monitor `gh run watch` (summarize, don't paste); on failure, fix-forward and re-dispatch.
- `[ ]` **M7.2** (Sonnet) Verify `curl -I https://live.jeremy.ninja/static/models/downloads/liveninja-<shortsha>.apk` → 200, correct content-type/size; then ORCHESTRATOR sends the link email via local SES (primary path — CI email is best-effort only).

**Implementation notes:**
_(placeholder)_

---

## M8 — Style picker + voice polish (post-ship) `[ ]`

**Two agents may parallelize (disjoint: M8.1/M8.2 theme-side vs M8.3/M8.5 voice-side; M8.4 sequenced last for build.gradle.kts).** **DoD:** 4-style picker working per web semantics; latency and reliability polish landed; gates green; pushed.

- `[ ]` **M8.1** (Sonnet) Port ninja/minimal/terminal token sets from `web/static/css/app.css` style blocks into the Theme.kt registry; these styles follow light/dark axis; HAL pins dark. *Files:* `ui/theme/Theme.kt`.
- `[ ]` **M8.2** (Sonnet) Style picker in SettingsScreen (4 options, `appStyle` key); gray out light/dark control with caption "HAL 9000 is always dark" while HAL selected. *Files:* `ui/screens/SettingsScreen.kt`, `ui/settings/SettingsViewModel.kt`. *Deps:* M8.1.
- `[ ]` **M8.3** (Opus) Latency: parallelize `RealtimeSessionCoordinator.start()` (fetchSession ∥ ensureFactory+PC+offer+ICE, join at SDP POST); pre-warm `ensureFactory()` + ADM at WakeWordService start; earcon-at-detection confirmed from M2. Target ~0.9-1.6s to listening. *Files:* `realtime/RealtimeSessionCoordinator.kt`, `realtime/WebRtcTransport.kt`, `wake/WakeWordService.kt`.
- `[ ]` **M8.4** (Sonnet) Reliability: 15-min WorkManager watchdog (serviceEnabled && dead → tap-to-resume notification, never FGS-from-background) — new `wake/WakeWatchdogWorker.kt` + WorkManager dep in `build.gradle.kts`; per-OEM guidance card in Settings. **Owner's phone = Samsung Galaxy (One UI): guidance must cover Settings → Battery → Never sleeping apps + 'Unrestricted' battery for Live Ninja + disable 'Put unused apps to sleep'.** *Deps:* M8.2.
- `[ ]` **M8.5** (Sonnet) Duty-cycle policy (charging always-continuous; saver 12s/2s; thermal SEVERE 8s/4s) + surface degraded state in notification; EnergyVad adaptive/lower threshold while charging. *Files:* `wake/WakeWordService.kt`, `wake/EnergyVad.kt`.
- `[ ]` **M8.6** (Haiku) Gate: lint + tests + assembleDebug; commit + push.

**Implementation notes:**
_(placeholder)_

---

## M9 — Optional reship v2 `[ ]`

- `[ ]` **M9.1** (Sonnet) If M8 lands before EOD: re-dispatch `android-release.yml`, verify link, note in EOD email. *Deps:* M8. Skip cleanly if time runs out — v1 already shipped.

**Implementation notes:**
_(placeholder)_

---

## M10 — End-of-day wrap `[ ]`

**DoD:** owner has final summary email; repo memory current; all plan statuses finalized.

- `[ ]` **M10.1** (orchestrator sends; Sonnet drafts) Final SES email: APK link(s), what shipped, known gaps — **wake word inert until the "Hey Live Ninja" model trains** (packaged model is hey_jarvis; ModelManager hot-swaps automatically when training lands); picker styles if M8 deferred; wake/lock-screen paths untested on hardware (first-run checklist included); emulator outcome.
- `[ ]` **M10.2** (Sonnet) Memory updates: SESSION-mode architecture decision, LNLog conventions, keystore-secret CI mechanism, new settings keys; finalize all plan status markers.

**Implementation notes:**
_(placeholder)_

---

## M11 — Owner-assist: "Hey Live Ninja" training kickoff `[ ]` (parallel, non-blocking)

- `[~]` **M11.1** (owner-assist) Owner confirmed 2026-07-20: kicking off "Hey Live Ninja" training NOW from their phone via live.jeremy.ninja (signed-in session → wake words → create custom phrase). Android `ModelManager.sync` fetches + hot-swaps the model when ready — zero Android code needed. Do NOT relabel to "Hey Jarvis" (owner decision).

**Implementation notes:**
_(placeholder)_

---

## M12 — OPTIONAL emulator provisioning `[x]` (done 08:35 ET)

- `[x]` **M12.1** SUCCESS: AVD `liveninja-test` (pixel profile, API 35 google_apis/x86_64), WHPX acceleration, ~35s cold boot. Relaunch: `%LOCALAPPDATA%\Android\Sdk\emulator\emulator.exe -avd liveninja-test -no-window -no-audio -no-snapshot -no-boot-anim -gpu swiftshader_indirect`; poll `adb shell getprop sys.boot_completed`; kill via `adb emu kill`. Gotcha: `avdmanager create avd` prompts interactively — pipe `echo "no" |`.
- `[x]` **M12.2** M7.0 smoke gate is now ENABLED (install all-ABI local APK — emulator is x86_64, do NOT use the arm64-only CI flag for the smoke build). Details in scratchpad notes-M12.md.

**Implementation notes:** emulator verified working end-to-end; shut down cleanly, AVD retained.

---

## Hourly status emails (ORCHESTRATOR ONLY — agents never send email)

Cadence: hourly, today only. `aws sesv2 send-email --region us-east-1`, From `"Jeremy Proffitt <jeremy@jeremy.ninja>"`, To/Reply-To `proffitt.jeremy@gmail.com`. Template — Subject: `Live Ninja Android — status <HH>:00`; Body: done milestones · in progress · blockers · next hour · ETA to APK link.

## Scope guard (binding on all agents)

Fix-forward only; **no backend Go changes and no template.yaml changes**; **no new AWS resources**; local AWS creds for SES sends only — all uploads via GitHub Actions OIDC; commit to `main` in logical increments, push after each milestone, **no Co-Authored-By trailers**; no questions mid-execution — every decision is baked above or in the five spec files. Critical path ≈ M3.1 → M2.3..M2.8 → M6 → M7 (~7h); apply pre-authorized cuts #1-#3 in order if the APK-today goal is at risk.
