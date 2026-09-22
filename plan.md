# Plan

Consolidated 2026-09-22. Single source of truth for active work.
Finished history is in [completed/plan-2026-09-22.md](completed/plan-2026-09-22.md).
Deliberately deferred items are in [backlog.md](backlog.md). Those are not plan items.

The job of this file is to push the remaining shippable work to production on `main`.

## Locked decisions (user-confirmed; do not revisit)

1. **2026-09-22 — bypass spoken smokes.** Do not run F1 or the E4 spoken turn. Do not wait for a microphone. Quote: "let's bypass the smoke tests and make the plan about pushing everything out."
2. **2026-09-22 — push the rest.** A push to `main` is the production deploy. Never deploy from a laptop.
3. **`emb-retire` is not before 2026-09-28.** The 2026-09-20 memory smoke already passed. This bypass does not move the date.
4. **Do not flip `SigningDisabled` on authorizer `live-ninja-iot`.** AWS IoT does not allow that update. Tab5 stays on that authorizer. Signing for web and Android is a new authorizer.
5. **G1–G6 stay in the backlog.** Do not pull them in.
6. **2026-09-20 — wake training is off.** Bundled phrases only. Default `hey-jarvis`. Do not resume custom wake training.
7. **No agent handles secret values.** Names only. `scripts/set-secret.sh` is the path for a new secret.

## Verified facts

- Repo `C:\dev\live-ninja`, GitHub `JeremyProffittOrg/live-ninja`, branch `main`. Module `github.com/JeremyProffittOrg/live-ninja`.
- `88c808ff19c2dc15776c0634a6f8440f29d6f26f` is the plan-bypass commit. Deploy run `35716826005` succeeded.
- `e6f52fea7590fcc2c3eee7512c617fdacc99bc35` documents eight voice engines and returns `tokenSignature` with `signingRequired: false`. Deploy run `35625518495` succeeded. The log printed `iot/authorizer_signing_private_key synced`.
- Galaxy S9 serial `4633424442303098`, model `SM-G965U`, package `ninja.jeremy.liveninja`, versionName `0.3.11`, versionCode `17` (rechecked 2026-09-22).
- `internal/realtime/catalog.go` has `SupportedVoices` (default `cedar`) and `SupportedGeminiVoices`. `SupportedAzureRealtimeVoices` does not exist.
- `GET /api/v1/realtime/voices` is `handleListVoices` in `internal/webapp/settings_routes.go`.
- `cmd/realtime-broker/main.go` has `handleAzureDirect` and `handleVoiceLiveDirect`. Both Voice Live pins use `AZURE_VOICELIVE_MODEL`, default `gpt-4o-mini-realtime-preview`.
- Authorizer resource in `template.yaml` is named `live-ninja-iot`. `SigningDisabled` is true while `IotAuthorizerSigningPublicKey` is empty. GitHub secret name `IOT_AUTHORIZER_SIGNING_PRIVATE_KEY` is set. The public half is a GitHub variable. Do not print either value.
- Web `web/static/js/liveevents.mjs` `iotSocketUrl` and Android `android/app/src/main/java/ninja/jeremy/liveninja/net/IotUrl.kt` add the signature query only when `signingRequired` is true.
- Help copy lives in `web/templates/pages/conversation.html`. A user-visible change updates it in the same commit. Guard: `go test ./internal/webapp/ -run TestHelpDrawer`.

## ship-remaining — push the rest to production

### voice-catalog — Azure native voice list

Definition of done: `go test ./internal/realtime/ -run "Voice|Catalog"` passes, and a test asserts every shipped engine's default voice is in that engine's catalog.

- [x] `voice-catalog` — add `SupportedAzureRealtimeVoices` for the `azure-realtime-native` voices, default `ava`, on `GET /api/v1/realtime/voices`, beside `SupportedGeminiVoices`. `gpt-live-azure` keeps `SupportedVoices` and `cedar`. Do not invent voice names. If the published list cannot be quoted from Microsoft docs, mark this `[!]` and name the page that is missing. Update Help in the same commit if the picker gains a new list. depends on: none

### broker-route-close — prove the four Azure routes

Definition of done: `go test ./cmd/realtime-broker/ -count=1` passes.

- [x] `broker-route-close` — the four Azure engines already route in `cmd/realtime-broker/main.go`. Run the broker tests. If they pass, record that in the execution log. If they fail, fix the failure and re-push. depends on: none

### iot-signed-authorizer — new authorizer for web and Android

Definition of done: `aws iot describe-authorizer --authorizer-name live-ninja-iot` still shows signing disabled, a new authorizer exists and is active, and `go test ./internal/webapp/ -run IoT` passes.

- [~] `iot-signed-authorizer` — create a new IoT authorizer with signing enabled. Do not change `live-ninja-iot`. Point web and Android at the new authorizer and set `signingRequired` true for those clients only. Tab5 keeps calling `live-ninja-iot` with no signature. The private key is already in SSM at `/live-ninja/prod/iot/authorizer_signing_private_key`. Never print it. depends on: none

### android-signature-release — phone build that sends the signature

Definition of done: `https://live.jeremy.ninja/v1/app/android/latest` shows a versionCode greater than 17, and `adb -s 4633424442303098 shell dumpsys package ninja.jeremy.liveninja` shows that versionName.

- [ ] `android-signature-release` — publish an Android build that sends the signature when `signingRequired` is true. The installed `0.3.11` ignores an unknown JSON field and does not send the signature. Publish with the `Android Release` workflow (`workflow_dispatch` on `main`), not a laptop build. depends on: `iot-signed-authorizer`

### emb-retire — drop the embedding rows

Definition of done: `grep -rn "EMB#\|Cosine(" internal/ cmd/` prints nothing and `go test ./...` passes.

- [ ] `emb-retire` — not before 2026-09-28. Remove `EMB#` writes, `ListEmbeddings`, the cosine path, and the Titan IAM statement. Rewrite `contracts/api.md` memory lines, `PRD.md` memory sections, and `docs/system-map.md`. depends on: the date 2026-09-28

## Restart policy

Each code milestone is one push to `main`. A red Deploy run is fixed and re-pushed. Ceiling 3 attempts for that milestone, then mark it `[!]` with the run id and continue. Do not retry a deterministic failure unchanged.

## Stop conditions (only these)

- The operator says to stop.
- A milestone would delete or update `SigningDisabled` on `live-ninja-iot`.
- A milestone would start `emb-retire` before 2026-09-28.
- A milestone would pull G1–G6 or resume custom wake training.
- The Azure voice list cannot be quoted, so `voice-catalog` is `[!]` instead of a guessed list.

## Execution log

- 2026-09-22 — spoken smokes bypassed. Commit `88c808f`. Deploy `35716826005` success.
- 2026-09-22 — completed history moved to `completed/plan-2026-09-22.md`. This file is only the unfinished build.
- 2026-09-22 — `broker-route-close` passed. Command: `go test ./cmd/realtime-broker/ -count=1`. Output: `ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.578s`. Exit 0. No handler change.
- 2026-09-22 — `voice-catalog` quoted 34 ids from https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live-how-to section "Supported voices". Older notes said 35. That count was one high. Default `ava`. No picker, so Help was not changed. `gpt-live-azure` stays on `SupportedVoices` / `cedar`. `go test ./internal/realtime/ -run "Voice|Catalog" -count=1` ok. `go test ./internal/webapp/ -run TestHelpDrawer -count=1` ok. `go test ./... -count=1` exit 0. Pushed as `d18202d2303b8c3c81c300c1d5862507f25a851f`. Deploy run `35737704441` succeeded. Android Release run `35737704501` failed in `Instrumented tests (emulator)`: `SettingsRevampComposeTest > hostPickerKeepsUnsupportedHostVisibleAndExplicitlyDisabled` with `RootViewWithoutFocusException` and `Failed to start Emulator console`. JVM unit tests passed. That test does not read the voice catalog.
- 2026-09-22 — `iot-signed-authorizer` code is local. New authorizer name `live-ninja-iot-signed`, `SigningDisabled: false`. `live-ninja-iot` keeps `SigningDisabled: !If [IotAuthorizerSigningEnabled, false, true]`. Public key is written to SSM `/live-ninja/prod/iot/authorizer_signing_public_key` and read with a CloudFormation dynamic reference. Web surface gets `signingRequired` true. Android gets it only with header `X-LN-IoT-Signing: 1`. Device and the default response stay on `live-ninja-iot`. Connect URLs add the `token` query parameter when signing is required, because AWS IoT checks the signature against that query value. `go test ./internal/webapp/ -run IoT -count=1` ok. `node --test tests/web/unit/liveevents.test.mjs` 18 pass. AWS describe-authorizer is not run yet.
