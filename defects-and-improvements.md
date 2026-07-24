# Live Ninja — Defects & Improvements

> Running backlog of defects and improvements found in shipped surfaces, tracked outside the
> formal M0–M13 [plan.md](plan.md). Each item carries a status marker and a suggested model
> route, same conventions as the master plan. Add new items at the top of the relevant section.

**Status markers:** `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked
**Model routing:** **H** Haiku · **S** Sonnet · **F** Fable · **O** Opus (Fable unavailable → promote to Opus, never Sonnet)

---

## Android

### D1 — No in-app control to stop/pause listening  `[ ]`  · **S** · _found 2026-07-24 (Tab S9 FE test)_

**Symptom.** On the tablet the app listens (wake-word foreground service) with no discoverable way
to turn it off. The user could not find an off switch anywhere in the app.

**Root cause.** A stop/pause control *does* exist, but only as **notification actions** on the
persistent foreground-service notification — nothing in the app's own UI surfaces it:

- **"Mute"** (pauses mic, service stays resident; toggles to "Resume") — `WakeWordService.kt:507-519`, handled `:128-129` → `prefs.muted`, drives `Mode.MUTED`/`stopEngine()` `:247-250`.
- **"Turn off"** (full stop: `serviceEnabled=false`, cancel watchdog, `stopEngineBlocking()`, `stopSelf()`) — `WakeWordService.kt:532-538`, handled `:131-138`.
- Public helper `WakeWordService.stop(context)` exists (`:594-606`) but **has zero callers** — the notification's `ACTION_STOP` is the only stop path.
- `SettingsScreen.kt` (~line 170) has wake-word/engine/sensitivity/privacy controls but **no listening on/off or mute toggle**. `MainActivity.kt:161` only ever *starts* the service.

**Aggravating factor.** The service is **self-resurrecting** — `WakeWatchdogWorker` (15-min periodic,
`WakeWatchdogWorker.kt:54-106`) and `WakeBootReceiver` (`:40-43`) relaunch it whenever
`serviceEnabled` is true. So force-killing the app does **not** durably stop listening; only the
notification's "Turn off" (which flips `serviceEnabled=false`) breaks the loop. This makes the
missing in-app off-switch a genuine usability/privacy defect, not just a nicety.

**Definition of Done.** A discoverable in-app control stops and pauses listening:
- A **"Listening" master switch** in Settings (near the wake-word controls, `SettingsScreen.kt` ~170) that calls the existing `WakeWordService.start(context)` / `WakeWordService.stop(context)` (`:594-606`) and reflects real service state.
- A **Mute/Resume toggle** in the same section wired to `prefs.muted` (`WakePreferences.kt:78`).
- Optionally a prominent **mic on/off button** on the home/conversation screen with the same wiring.
- State stays consistent with the notification actions (toggling in-app updates the notification and vice-versa; both read/write the same `serviceEnabled`/`muted` prefs).
- Off is durable: turning listening off in-app must set `serviceEnabled=false` so the watchdog/boot receiver don't resurrect it.

**Notes / gotchas.**
- Reuse the existing `serviceEnabled` and `muted` prefs as the single source of truth — don't add a parallel flag. Currently `prefs.muted`/`ACTION_STOP`/`ACTION_MUTE` are referenced *only* inside `WakeWordService.kt`; the UI will be the first outside reader/writer, so expose them via the settings ViewModel.
- Confirm live state on screen open (service may have been stopped from the notification while the app was backgrounded) rather than trusting a cached toggle value.
- UI rule (house style): the toggle row's control and any adjacent label/button share one uniform height. Prefer a real Material switch, not a custom `div`-style control.

**Files.** `WakeWordService.kt` (128-168, 455-540, 594-606) · `WakePreferences.kt` (27, 78) ·
`SettingsScreen.kt` (~170) · `MainActivity.kt` (159-162) · `WakeBootReceiver.kt` (40-43) ·
`WakeWatchdogWorker.kt` (54-106) · `res/values/strings.xml` (205-211).
