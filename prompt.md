# Continuation prompt — live-ninja WS-5 (Android stability & performance)

You are continuing work on **live-ninja**, a voice-assistant platform (Go/AWS backend + web app + Android app). The previous session ended mid-way through **WS-5, the Android stability & performance workstream**. There is **uncommitted work on disk that is verified-good and needs committing**. Read this whole document before touching anything.

---

## 1. Objective

The user's words, verbatim, escalating through the session:

1. `"resolve any issue"` (during live on-device testing)
2. `"make the android ap extremely stable and performant!"`
3. `"build all you can!!!"` — i.e. run autonomously through the plan without check-ins

So: **make the Android app extremely stable and performant**, working autonomously through the WS-5 milestones in `plan.md`, committing and pushing in logical increments.

Two scope decisions the user explicitly answered (do NOT relitigate):

- **16 KB page alignment IS in scope**, with a mandatory on-device voice re-verify after the dependency bumps ("Yes, but verify voice still works after").
- **Verification goes as far as an instrumented on-device harness** ("Also build an instrumented on-device test harness"), not just unit tests.

---

## 2. Current status — blunt snapshot

| Item | State |
|---|---|
| M21.0 wake-service deadlock | ✅ **Fixed, committed, pushed, device-verified** (`74c0651`) |
| M21.1 Android transcript upload (data loss) | ✅ **Fixed, committed, pushed, device-verified** (`76ca04a`) |
| M21.3 wake-phrase mismatch | ⚠️ **Implemented + compiles + 121 unit tests green — UNCOMMITTED** |
| M23 16 KB alignment dep bumps | ⚠️ **Implemented + built + alignment VERIFIED — UNCOMMITTED, voice loop NOT yet re-verified on device** |
| M21.2 AEC self-echo loop | ❌ Not started (root cause understood, candidate fix identified) |
| M22 performance (jank / cold start / APK size) | ❌ Not started |
| M24.1 unit tests | 🔶 Partial — done for M21.1 only |
| M24.2/24.3 instrumented harness + emulator CI | ❌ Not started |
| M17 (backend, separate workstream) | ⛔ Blocked on AWS Bedrock entitlement propagation |

**The single most important thing to know:** the working tree has uncommitted changes for M21.3 and M23. They compile, unit tests pass, and the 16 KB alignment is objectively verified. What is **missing is the on-device voice re-verification** that the user explicitly required before accepting the dep bump.

---

## 3. Repo, environment, tooling

- **Repo root:** `c:\dev\live-ninja` — branch **`main`**, work directly on it (house rule: no feature branches/PRs unless asked).
- **Push to `main` IS the production deploy trigger** (GitHub Actions). `deploy.yml` has **no path filters**, so even a docs/Android-only commit triggers a full backend deploy. That is expected and harmless — do not "fix" it.
- **Never deploy to AWS from the local machine.** No `aws deploy`, `sam deploy`, `sam sync`. CI + OIDC only. Read-only `aws` CLI calls are fine.
- **No `Co-Authored-By: Claude` trailer** in commit messages.

### Android build (this machine has quirks)

```bash
cd /c/dev/live-ninja/android
export JAVA_HOME="C:/Users/Jeremy/jdk-temurin17/jdk-17.0.19+10"
"$JAVA_HOME/bin/java.exe" -cp gradle/wrapper/gradle-wrapper.jar org.gradle.wrapper.GradleWrapperMain :app:assembleDebug --console=plain -q
```

- **`JAVA_HOME` in the environment is stale/broken** — always export it explicitly as above.
- **Do NOT use `./gradlew` or `cmd //c gradlew.bat`** — both fail silently under git-bash. The `java -cp gradle-wrapper.jar org.gradle.wrapper.GradleWrapperMain` invocation is the only reliable one.
- Useful tasks: `:app:compileDebugKotlin`, `:app:testDebugUnitTest`, `:app:assembleDebug`, `:app:lintDebug`.

### Physical test device (attached, authorized)

- Samsung Galaxy Tab S9 FE, `SM-X518U`, serial **`R52XC06P9KJ`**, Android 16, screen **1440x2304**.
- **adb is NOT on PATH:**
  ```bash
  ADB="C:/Users/Jeremy/AppData/Local/Android/Sdk/platform-tools/adb.exe"
  "$ADB" -s R52XC06P9KJ <cmd>
  ```
- **Install with `-g`** to pre-grant runtime permissions: `"$ADB" -s R52XC06P9KJ install -r -g app/build/outputs/apk/debug/app-debug.apk`
- Screenshot → analyze loop: `"$ADB" -s R52XC06P9KJ exec-out screencap -p > shot.png` then read the PNG.
- **Screenshots come back 1440x2304 but are displayed at 1250x2000 — multiply displayed coords by 1.15** to get tap coordinates.
- `adb input text` does **not** URL-decode; spaces must be `%s`.

### Known device UI tap coordinates (already scaled, ready to use)

| Target | Tap |
|---|---|
| Conversation tab | `input tap 137 2174` |
| History tab | `input tap 427 2174` |
| Settings tab | `input tap 1297 2174` |
| Mic FAB (tap to talk) | `input tap 719 1992` |
| End-session (red stop) | `input tap 1089 1992` |
| History refresh (top-right) | `input tap 1388 147` |
| "Always listening" switch (Settings, first control) | `input tap 1363 247` |

### Speaking to the device through the PC speakers (this is how voice tests are driven)

A helper already exists at
`C:/Users/Jeremy/AppData/Local/Temp/claude/c--dev-live-ninja/6735240f-44ec-4e27-bf2e-6241823a3255/scratchpad/say.ps1`
(recreate if the scratchpad is gone):

```powershell
param([string]$Text, [int]$Rate = 0)
Add-Type -AssemblyName System.Speech
$s = New-Object System.Speech.Synthesis.SpeechSynthesizer
$s.Volume = 100; $s.Rate = $Rate
$s.Speak($Text); $s.Dispose()
```

Invoke: `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "<path>/say.ps1" -Text "Hey Jarvis" -Rate 0`

**Critical audio-test setup learned the hard way:**
- **PC output volume must be near max** or the tablet mic hears nothing (first attempt failed silently — data channel OPEN but zero speech events). Max it with:
  `powershell.exe -NoProfile -Command "$w = New-Object -ComObject WScript.Shell; 1..50 | ForEach-Object { $w.SendKeys([char]175) }"`
- **Tablet media volume must be LOW** (~3/15) or the assistant's own speech feeds its mic and triggers the echo loop (see M21.2):
  `"$ADB" -s R52XC06P9KJ shell cmd media_session volume --stream 3 --set 3`
- **The wake phrase that actually works today is "Hey Jarvis"**, NOT "Hey Live Ninja" (see M21.3).

---

## 4. What has been done (with evidence)

### Committed and pushed

```
76ca04a android: upload transcripts so conversations actually persist (WS-5 M21.1)
74c0651 android: add the Always listening switch that actually starts the wake service
17bba3f M15: Base Knowledge layer — profile, session directive, profile-aware tools
3342815 plans: drop the Tab5/M5Stack surface from active work
a3d0ff9 chore(plans): consolidate plans → plan.md, backlog → backlog.md; archive under archive/
```

**M21.0 — wake service could never start (`74c0651`).**
Root cause: `WakeWordService.start()` had exactly two callers — `WakeBootReceiver` and `MainActivity.handleWakeResume()` — and **both were gated on `WakePreferences.serviceEnabled`**, while that flag was only ever set `true` *inside* `WakeWordService.onStartCommand` (WakeWordService.kt:153). Default `false` ⇒ unbreakable cycle; no onboarding step or Settings control called `start()`. `dumpsys activity services ninja.jeremy.liveninja` returned **nothing** while the home screen advertised "just say Hey Live Ninja".
Fix: added an **"Always listening"** switch as the first control in the Settings → Wake word section, plus `WakePreferences.serviceEnabledFlow` so the switch follows the service (the service flips the flag itself, incl. from its notification Stop action). Requests `RECORD_AUDIO` first and starts from the grant callback (starting a mic FGS without it throws).
Verified: toggling on yields `isForeground=true ... types=0x00000080` (microphone) + persistent notification; a spoken wake phrase then opens a session.

**M21.1 — every Android conversation was being lost (`76ca04a`).**
Root cause was worse than a missing final flush: **the app had no transcript upload path at all.** `LiveNinjaApi` never declared `POST /api/v1/transcript`. Turns accumulated only in the in-memory process-wide `TranscriptStore`, so no `LOG#` rows were written, the `final:true` flush that triggers `cmd/topics-extract` never fired, and **no `CONV` record was ever created**. Reproduced: three real sessions (21:22, 21:24, 21:31) produced zero History entries; History's newest row stayed 2026-07-24 16:43 even after refresh.
Fix (new file `android/app/src/main/java/ninja/jeremy/liveninja/realtime/TranscriptUploader.kt`):
- Batches finished turns like the web sink (`BATCH_SIZE = 25`, `BATCH_INTERVAL_MS = 5_000`) so a long conversation survives mid-session process death.
- Always sends the session-end `final:true` flush — valid with zero turns, and the seam the server uses to persist the conversation.
- `TranscriptSink` is a **one-method seam** over the Retrofit call so the uploader is testable without faking the whole API surface; `ApiTranscriptSink` is the production impl, bound in `NetModule.provideTranscriptSink`.
- The **CoroutineScope is constructor-injected** (secondary `@Inject constructor` supplies the production IO scope) so tests drive batching deterministically with a `TestScope`.
- Upload failures are logged and swallowed — losing a history row is bad, interrupting a live conversation is worse.
- Wired in `RealtimeSessionCoordinator`: `begin(session.sessionId, engineForMode(session.mode))` after the session fetch, `record(role, fullText)` in `emitFinal`, `finish()` in `stop()`.
- DTOs appended to `net/HistoryDtos.kt` as **`TranscriptUploadTurnDto`** / `TranscriptUploadRequest` — note the name, because a *different* `TranscriptTurnDto` already exists in that file for *reading* history (the first attempt collided).
- 7 unit tests in `app/src/test/java/ninja/jeremy/liveninja/realtime/TranscriptUploaderTest.kt`.
Verified on device: a session at 22:04 produced a History row **"What's on your mind?" · Jul 24, 2026, 10:04 PM · gpt-realtime**, where three earlier sessions had produced none.

**Earlier in the session (also committed):** M15 Base Knowledge layer (`17bba3f`) — and it was **verified live on the tablet**: asked "what is the weather right now?" with **no location** and got *"It's currently overcast in Lancaster, South Carolina, with a temperature of about 72 degrees"* — profile home + imperial units, from stored coordinates with no geocoding leg.

### UNCOMMITTED on disk right now (this is the handoff hot-spot)

`git status --porcelain`:
```
 M android/app/src/main/java/ninja/jeremy/liveninja/ui/conversation/ConversationViewModel.kt
 M android/app/src/main/java/ninja/jeremy/liveninja/ui/screens/SettingsScreen.kt
 M android/app/src/main/java/ninja/jeremy/liveninja/ui/settings/SettingsViewModel.kt
 M android/app/src/main/java/ninja/jeremy/liveninja/ui/state/WakeWordCatalogRepository.kt
 M android/app/src/main/res/values/strings.xml
 M android/gradle/libs.versions.toml
```

These are **two logically separate changes that should be committed as two commits**:

**(a) M21.3 — wake phrase advertised but not shipped.**
The bug: Settings and the home screen both said "Hey Live Ninja", but the APK bundles only `assets/wakeword/hey_jarvis_v0.1.onnx`. `ModelManager.DEFAULT_ASSET_WAKE_WORD_ID = "hey-jarvis"` while `WakePreferences.DEFAULT_WAKE_WORD_ID = "hey-live-ninja"`, and the catalog's `BUILT_IN` list claimed `hey-live-ninja` was "Default phrase · bundled model, always available" — **false**. Only "Hey Jarvis" actually triggers.
What was implemented (principle: *never advertise a phrase that cannot match*):
- `ConversationViewModel`: the wake caption is now driven by **`modelManager.headModel`** (the loaded head model's real `wakeWordId`), **not** `doc.wakeWord`. Added `selectedWakeWordId` + `activeWakeWordId` to the UI state; default `wakePhraseLabel` changed to `"Hey Jarvis"`.
- `WakeWordCatalogRepository.BUILT_IN`: added a truthful `hey-jarvis` entry first ("Bundled model · works offline, no training needed"); `hey-live-ninja` re-described as "Platform default · needs a trained model synced to this device".
- `SettingsViewModel`: added `activeWakeWordId` to `SettingsUiState`, collected from `modelManager.headModel` in a new `init` block. **Note:** `modelManager` was *already* an injected constructor param — a duplicate injection was added and then removed; don't re-add it.
- `SettingsScreen`: renders an error-coloured warning under the wake-phrase combobox when `activeWakeWordId != doc.wakeWord`, using new string `settings_wake_phrase_unavailable`.
- Status: compiles, `:app:testDebugUnitTest` **BUILD SUCCESSFUL**. **Not yet visually confirmed on device.**

**(b) M23 — 16 KB page-size alignment dep bumps.**
`android/gradle/libs.versions.toml`:
```toml
webrtc = "144.7559.09"     # was 125.6422.07
onnxruntime = "1.27.0"     # was 1.20.0
```
Versions were chosen by querying Maven Central metadata (not guessed):
`curl -s "https://repo1.maven.org/maven2/io/github/webrtc-sdk/android/maven-metadata.xml"` and the equivalent for `com/microsoft/onnxruntime/onnxruntime-android`.
- `:app:assembleDebug` **succeeded**.
- **Alignment objectively verified** — all four previously-flagged libs now report `max LOAD p_align = 16384`:
  `libandroidx.graphics.path.so`, `libjingle_peerconnection_so.so`, `libonnxruntime.so`, `libonnxruntime4j_jni.so` → **ALL 16KB-ALIGNED**.
  (Android 16 had previously flagged all four with "LOAD segment not aligned".)
- **Side effect:** debug APK grew **177 MB → 256 MB** (all-ABI). This makes M22.3 (ABI splits + R8 for release) more urgent, but is acceptable for a debug build.
- **NOT DONE: the on-device voice re-verify the user required.** This is the gate on committing (b).

The alignment checker (no NDK/readelf needed — parses ELF program headers in pure Python) is worth keeping; recreate it as needed:

```python
import zipfile, struct
apk='app/build/outputs/apk/debug/app-debug.apk'
z=zipfile.ZipFile(apk)
for n in sorted(x for x in z.namelist() if x.startswith('lib/arm64-v8a/') and x.endswith('.so')):
    d=z.read(n)
    phoff=struct.unpack_from('<Q',d,0x20)[0]
    phentsize=struct.unpack_from('<H',d,0x36)[0]
    phnum=struct.unpack_from('<H',d,0x38)[0]
    aligns=[struct.unpack_from('<Q',d,phoff+i*phentsize+0x30)[0]
            for i in range(phnum)
            if struct.unpack_from('<I',d,phoff+i*phentsize)[0]==1]
    mx=max(aligns) if aligns else 0
    print(f"{n.split('/')[-1]:44} {mx:>8} {'YES' if mx>=16384 else 'NO'}")
```

---

## 5. Immediate next steps (ordered — start at #1)

### 1. Re-verify the voice loop on device, then commit M21.3 and M23 separately

This is the gate the user set on the dep bump. Sequence:

```bash
cd /c/dev/live-ninja/android
export JAVA_HOME="C:/Users/Jeremy/jdk-temurin17/jdk-17.0.19+10"
ADB="C:/Users/Jeremy/AppData/Local/Android/Sdk/platform-tools/adb.exe"
S=<scratchpad dir>

# build + install the current (uncommitted) tree
"$JAVA_HOME/bin/java.exe" -cp gradle/wrapper/gradle-wrapper.jar org.gradle.wrapper.GradleWrapperMain :app:assembleDebug --console=plain -q
"$ADB" -s R52XC06P9KJ install -r -g app/build/outputs/apk/debug/app-debug.apk

# app volume hygiene for audio tests
"$ADB" -s R52XC06P9KJ shell cmd media_session volume --stream 3 --set 3
powershell.exe -NoProfile -Command "\$w = New-Object -ComObject WScript.Shell; 1..50 | ForEach-Object { \$w.SendKeys([char]175) }"

# launch, enable Always listening if off, then wake by speaker
"$ADB" -s R52XC06P9KJ shell monkey -p ninja.jeremy.liveninja -c android.intent.category.LAUNCHER 1
sleep 8
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$S/say.ps1" -Text "Hey Jarvis" -Rate 0
sleep 6
"$ADB" -s R52XC06P9KJ exec-out screencap -p > "$S/verify1.png"
```

**Definition of done for the re-verify:** wake fires → session goes Listening → a spoken question gets an audio answer → the session appears in History after ending it. Check `logcat` for `WebRtcTransport ... oai-events channel state: OPEN` and for absence of `onnxruntime`/`jingle` loader errors.

Then confirm the new libs load without the Android 16 alignment warning (the launch dialog previously listed all four).

**If the voice loop is broken by the bump:** `git checkout android/gradle/libs.versions.toml` reverts M23 alone (that's exactly why it must be its own commit), then commit M21.3 by itself and re-plan M23.

**If it works, commit as two commits**, e.g.:
- `android: stop advertising a wake phrase with no model behind it (WS-5 M21.3)`
- `android: 16 KB page-aligned onnxruntime + webrtc (WS-5 M23)`

Then update `plan.md` WS-5 statuses (`[ ]` → `[x]`) with verbose implementation notes, and push.

### 2. M21.2 — AEC self-echo loop

**Reproduced twice**, at tablet volume 15/15 and 3/15: the assistant's own speech is transcribed as user input, so it answers itself in a loop. Observed transcript:
```
You: What is the                      ← my question, clipped by VAD
Live Ninja: Hey there! What can I help you with tonight?
You: Hey there.                       ← the assistant's own words, transcribed as user
Live Ninja: Hey there! How's it going?
You: How's it going?                  ← again
```
and later, repeatedly re-answering the weather question: `You: Right now in…`, `You: It's currently…`.

Current audio config (`android/app/src/main/java/ninja/jeremy/liveninja/realtime/WebRtcTransport.kt`):
- `ensureFactory()` ~line 395-410: `JavaAudioDeviceModule.builder(context).setUseHardwareAcousticEchoCanceler(true).setUseHardwareNoiseSuppressor(true)`
- `configureAudioForCall()` ~line 413-429: `am.mode = MODE_IN_COMMUNICATION`, then picks from `PREFERRED_ROUTE_TYPES` (line ~606: BT SCO → wired headset → wired headphones → USB headset → **BUILTIN_SPEAKER**).
- Logcat shows `W/AudioManager: getAvailableCommunicationDevices: no EARPIECE!` at session start (tablet has no earpiece).

**Candidate fix:** the *hardware* canceller is tuned for handset use and is not cancelling loudspeaker echo on this tablet. Try `setUseHardwareAcousticEchoCanceler(false)` so WebRTC's **software AEC3** runs instead, and add a half-duplex guard (suppress/ignore input while assistant audio is actively playing) as a fallback. Note the plan file records the same failure class from the Tab5 work, mitigated there via `micEagerness=low`.

**This one needs several on-device audio iterations** — the user was told it's the item where having them nearby helps. It is legitimate to make the change, verify, and iterate; do not claim it fixed without an observed clean multi-turn exchange with no self-transcription.

### 3. M22 — performance

Measured baseline (2026-07-24, debug build, versionCode 4 / v0.2.1-hal):

| Signal | Value |
|---|---|
| Cold start `am start -W` | **TotalTime 1168 ms**, WaitTime 1176 ms |
| Debug APK | 177 MB before the M23 bump, **256 MB after**; native libs for **4 ABIs** |
| Crashes in buffer | 0 (one stale ANR dir `anr_2026-07-02-08-21-20-676`) |
| Jank | `Skipped 55 frames!`, `Davey! duration=705ms`, and `Compiler allocated 12MB to compile … SettingsScreenKt$SettingsScreen$5` |
| Lint | **1 error** + 92 warnings |

- **M22.1** — `SettingsScreen.kt` is ~1300+ lines rendering everything inline inside one `verticalScroll(rememberScrollState())` `Column`. Split into per-section composables, hoist `remember`ed state, consider making it lazy so composition is incremental.
- **M22.2** — profile what runs before first frame (Hilt graph, WorkManager init, prefs I/O, ONNX/WebRTC class loading) and move non-essential work off the startup path.
- **M22.3** — release builds should use **ABI splits (or an App Bundle) + R8**. The arm64-only path already exists: `-Pliveninja.arm64Only=true` (see `android/app/build.gradle.kts` ~line 15 `arm64Only`, ~line 29 `abiFilters`). This got more urgent with the 256 MB all-ABI debug APK.

**Pre-existing lint error — do NOT attribute it to your changes:** `AndroidManifest.xml:39: Error: Remove androidx.work.WorkManagerInitializer from your AndroidManifest.xml when using on-demand initialization. [RemoveWorkManagerInitializer]`. It comes from the **merged** manifest via `androidx.work` (the committed manifest contains no such literal string; verified with `git show HEAD:android/app/src/main/AndroidManifest.xml | grep -c WorkManagerInitializer` → `0`). `:app:lintDebug` therefore **fails the build** — factor that in if you wire lint into CI (either fix it properly with a `Configuration.Provider` + manifest removal, or baseline it deliberately and say so).

### 4. M24 — verification harness (the user chose the instrumented option)

- **24.1** (partial) — JVM unit tests exist for M21.1 only. Still needed for M21.2 (AEC config), M21.3 (wake-phrase resolution: `activeWakeWordId` vs `doc.wakeWord`), and the service/prefs state machine.
- **24.2** — Espresso/UiAutomator tests for the flows that actually broke: onboarding → signed-in home; Always-listening toggle → running FGS; tap-to-talk → live session.
- **24.3** — wire an emulator job into `.github/workflows/android-release.yml` (CI has **no attached device**). Local reference AVD is **`liveninja-test`** (API 35 google_apis x86_64, WHPX works):
  ```
  %LOCALAPPDATA%\Android\Sdk\emulator\emulator.exe -avd liveninja-test -no-window -no-audio -no-snapshot -no-boot-anim -gpu swiftshader_indirect
  ```
  poll `adb shell getprop sys.boot_completed`. Note `avdmanager create avd` prompts interactively — pipe `echo no |`. **Do not** pass `-Pliveninja.arm64Only=true` for an emulator smoke build (emulator is x86_64).

---

## 6. Locked decisions & constraints

- **16 KB alignment in scope, verify voice after** (user). **Instrumented on-device harness in scope** (user).
- **Tab5 / M5Stack is OUT of the plan entirely** (user decision this session) — all firmware/IoT items, incl. the `ProvisionIoT` empty hook, live in `backlog.md`. Do not pick them up.
- **No CloudWatch alarms** (owner decision 2026-07-19). Budgets email directly.
- **No stubs / placeholders / "not implemented" returns.** Ask instead of papering over.
- **No secrets managers** (cost). SSM Parameter Store / env vars / GitHub secrets only. Agents never see secret values — the owner sets them via `scripts/set-secret.sh`.
- **DynamoDB: never `Scan` on a serving path.**
- **Any row mixing a text box and a button must render them the same height, sized to the text box** (global UI rule).
- **Fields with enumerable/queryable values must be pickers, never blind text boxes** (global UI rule) — this is why M15's location fields are geocoder-backed comboboxes.
- Commit style: no `Co-Authored-By`. Commit + push to `main` once work lands.

---

## 7. Key references

- **`plan.md`** (repo root) — the single source of truth for active work. **WS-5** is the Android workstream (M21 defects, M22 perf, M23 16 KB, M24 harness) and carries the measured baseline table. WS-1 verification, WS-2 Base Knowledge (M15 done / M16 / M17), WS-3 leftovers, WS-4 launch are the other workstreams.
- **`backlog.md`** — deliberately deferred: all Tab5/M5Stack, Nova Sonic re-enable, and other parked items.
- **`archive/`** — the five pre-consolidation plans (`plan.md`, `gemini-plan.md`, `tool-parity-plan.md`, `base-knowledge-plan.md`, `android-revamp-plan.md`), each with a MIGRATED banner. `archive/plan.md` §8 holds the deepest RESUME-STATE history of how the system works — **read it before resuming anything unfamiliar**.
- **`docs/qa-report.md`** — the manual-verification checklist (harvested into WS-1, not archived).

Code anchors:
- `android/.../realtime/TranscriptUploader.kt` — M21.1 uploader + `TranscriptSink` seam.
- `android/.../realtime/RealtimeSessionCoordinator.kt` — `start()` (~line 76), `stop()` (~157), `emitFinal()` (~263); uploader wired at all three.
- `android/.../realtime/WebRtcTransport.kt` — `ensureFactory()` ~395, `configureAudioForCall()` ~413, `PREFERRED_ROUTE_TYPES` ~606. **M21.2 lives here.**
- `android/.../wake/WakeWordService.kt` — `serviceEnabled = true` at ~153; companion `start()`/`stop()` at ~592-607.
- `android/.../wake/WakePreferences.kt` — `serviceEnabled` + `serviceEnabledFlow`.
- `android/.../wake/ModelManager.kt` — `ASSET_DEFAULT_HEAD = "wakeword/hey_jarvis_v0.1.onnx"`, `DEFAULT_ASSET_WAKE_WORD_ID = "hey-jarvis"`, `headModel: StateFlow<WakeModelRef>`.
- `internal/webapp/api_routes.go` — `handleTranscript` (~677) is the server contract the Android uploader matches: `{sessionId, final, cost?, turns[{seq,role,text,engine}]}`; `final:true` triggers `cmd/topics-extract`; a final-only flush with zero turns is valid.

---

## 8. Gotchas & failure modes (these already cost time)

1. **Gradle:** use the `java -cp gradle-wrapper.jar ...GradleWrapperMain` form; `./gradlew` silently fails under git-bash. Export `JAVA_HOME` explicitly.
2. **adb not on PATH**; screenshot coords need the **×1.15** scale factor.
3. **Audio tests fail silently if PC volume is low** — the data channel reports OPEN and simply no speech events arrive. Max PC volume; drop tablet media volume to ~3/15.
4. **Wake phrase is "Hey Jarvis"**, not "Hey Live Ninja" (M21.3).
5. **Sign-in:** the app lands on a "Continue with Amazon" wall. Amazon already has a live session in Samsung Internet, so tapping through only needs the OAuth **Allow** consent — **never type the user's credentials.** (Consent button was at `input tap 942 958`.)
6. **DTO name collision:** `TranscriptTurnDto` already existed in `net/HistoryDtos.kt` for reading history; the upload DTO is `TranscriptUploadTurnDto`.
7. **`SettingsViewModel` already injects `modelManager`** — adding it again is a "Conflicting declarations" compile error.
8. **Android strings.xml:** an escaped apostrophe (`can\'t`) in a string containing `%1$s` produced `Failed to flatten XML ... Invalid unicode escape sequence`. Avoid apostrophes; use "cannot".
9. **`:app:lintDebug` fails** on the pre-existing `RemoveWorkManagerInitializer` error (see M22 above). `:app:testDebugUnitTest` is clean.
10. **Coroutine tests:** a class that creates its own `CoroutineScope(Dispatchers.IO)` escapes `runTest`'s scheduler — 3 tests failed until the scope became constructor-injected. Follow that pattern.
11. **History is async:** a `CONV` row appears only after the `final:true` flush → `topics-extract` → extraction completes. Allow ~30 s and hit the refresh button before concluding it failed.
12. **`git status` LF→CRLF warnings** on every Android file are normal noise on this machine.
13. **Pushing `main` triggers a full backend deploy** even for Android-only commits (no path filters). Expected.

---

## 9. Separate blocker (backend, not Android) — Bedrock Opus 5 for M17

The next backend milestone, **M17 (tool-failure agentic RCA via Bedrock Claude Opus)**, is blocked on an AWS entitlement that is **already accepted but still propagating**:

- The account's Anthropic access had never actually been granted. Diagnosis via current boto3 (the installed `aws` CLI 2.22.31 predates these APIs; a venv with current boto3 was used because the CLI MSI upgrade needed a UAC prompt the user declined):
  `get_foundation_model_availability` → `authorizationStatus: AUTHORIZED`, `entitlementAvailability: AVAILABLE`, `regionAvailability: AVAILABLE`, but **`agreementAvailability: NOT_AVAILABLE`** — the model EULA had never been accepted. The use-case form was already on file. Not a sales gate.
- `create_foundation_model_agreement(modelId='anthropic.claude-opus-5', offerToken=…)` was called; status went `NOT_AVAILABLE → PENDING → AVAILABLE` in us-east-1, us-east-2 **and** us-west-2. Terms are pure pay-per-use ($5/M in, $25/M out, $0.50/M cache read), no commitment.
- **`bedrock-runtime:InvokeModel` still returned `AccessDeniedException` ~40 minutes later.** Control plane says granted; runtime disagrees. Pure propagation — re-check before starting M17.

Facts already established for when it clears:
- **Model id must be `us.anthropic.claude-opus-5`** — Opus 5's only `inferenceTypesSupported` on Bedrock is `INFERENCE_PROFILE`, so invoking the bare `anthropic.claude-opus-5` can never work.
- **IAM needs FOUR ARNs, not one:** the inference-profile ARN `arn:aws:bedrock:us-east-1:759775734231:inference-profile/us.anthropic.claude-opus-5` **plus** the foundation-model ARN in **us-east-1, us-east-2 and us-west-2** (the `us.` profile fans out to all three). `plan.md`'s original "Opus inference profile ARN" alone would deploy and then fail at runtime.
- Request body shape: Messages API + `"anthropic_version": "bedrock-2023-05-31"`, **no** `model` field. Structured outputs (`output_config.format`) and `effort` are supported on Bedrock; **task budgets, mid-conversation system messages and fast mode are NOT** — so the plan's ≤8K-in/≤2K-out budget must be enforced by our code, not by `task_budget`.

---

## 10. Open questions for the user

1. **M22.3 release packaging** — ABI splits vs. an App Bundle? (Bundle is better for Play, splits are simpler for the current S3-hosted APK delivery.)
2. **The pre-existing lint error** — fix `WorkManagerInitializer` properly (on-demand init via `Configuration.Provider` + manifest node removal) or add a deliberate lint baseline?
3. **`plan.md` overstates what's left in WS-1**: three items were incidentally closed on device this session — Android Custom-Tabs PKCE exchange, Android live voice round-trip, and the wake path (wake proven; lock-screen still untested). The user was asked whether to fold these in and had not answered when the session ended.
4. **WS-3:** the `proffitt.jeremy+qa@gmail.com` allowlist decision — and the user should **rotate the QA password** that was pasted in-transcript on 2026-07-18.

---

## 11. Definition of done for WS-5

- **M21**: no defect reproducible on the tablet — wake starts from a fresh install, conversations persist to History, no self-echo loop, and no advertised phrase that cannot match.
- **M22**: cold start measurably below the 1168 ms baseline; Settings opens without dropping frames; release artifact is a fraction of the all-ABI debug size.
- **M23**: ✅ alignment already verified (all four libs `p_align = 16384`) — **plus** an observed clean wake→session→answer cycle on the bumped deps.
- **M24**: unit tests cover every M21 defect; instrumented tests cover onboarding/toggle/tap-to-talk; an emulator job runs them in CI.
- Every fix committed and pushed to `main`, with `plan.md` statuses and verbose implementation notes updated in place.

---

## First action

```bash
cd /c/dev/live-ninja && git status --porcelain && git log --oneline -3
```

Confirm the six modified files listed in §4 are still present and uncommitted, then go straight to **§5 step 1**: build + install the current tree, drop tablet media volume to 3, max the PC volume, say **"Hey Jarvis"**, and verify the voice loop survives the 16 KB dependency bump. Commit M21.3 and M23 as two separate commits once it does.
