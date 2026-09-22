You are Grok, working in C:\dev\live-ninja on branch main. Build every unfinished item in plan.md. Do not stop after one of them. Do not ask whether to start. The operator is not watching.

Read plan.md, deploy.md, agents.md, and this file before editing. plan.md is the only list of work. backlog.md is not work. completed/ is history. Do not edit completed/.

## Objective

Push the remaining Live Ninja work to production. The operator said, on 2026-09-22: "let's bypass the smoke tests and make the plan about pushing everything out." Then: "create a prompt.md to fully engage grok to build everything in the plan."

A push to main is the production deploy. Never deploy with a local sam or aws cloudformation command. Never print a secret, token, PEM, or keystore.

## What to build, in this order

Do voice-catalog and broker-route-close first. They do not depend on each other. Then iot-signed-authorizer. Then android-signature-release. Leave emb-retire untouched until the calendar date is 2026-09-28 or later. If you are running before that date, finish the other four, write the date in the execution log, and stop. Do not "prepare" emb-retire early.

### voice-catalog

Add SupportedAzureRealtimeVoices in internal/realtime/catalog.go, next to SupportedGeminiVoices (that slice starts at catalog.go line 48). Default voice id ava. Serve it on GET /api/v1/realtime/voices from internal/webapp/settings_routes.go handleListVoices, as a sibling field the way gemini voices are served. gpt-live-azure and gpt-live-azure-mini keep SupportedVoices and the default cedar. Do not map cedar to another name.

Quote every voice id from a Microsoft document you actually open. If you cannot quote the list, mark voice-catalog [!] in plan.md with the URL you tried, and go on to the next item. Do not invent names. Do not copy a blog's unsourced list.

If the picker shows the new list to a person, update the Help drawer in web/templates/pages/conversation.html in the same commit. The section title in Help must match the UI label. Then run:

go test ./internal/realtime/ -run "Voice|Catalog"
go test ./internal/webapp/ -run TestHelpDrawer

A test must assert that every shipped engine's default voice is a member of that engine's own catalog.

### broker-route-close

The four Azure engines already have handlers in cmd/realtime-broker/main.go: handleAzureDirect and handleVoiceLiveDirect. Run:

go test ./cmd/realtime-broker/ -count=1

If the package fails, fix the failure, commit, push, and watch Deploy. If it passes, record the pass in plan.md's execution log. Do not rewrite the handlers because the checkbox was open.

### iot-signed-authorizer

The current authorizer is live-ninja-iot in template.yaml (AuthorizerName around line 768). SigningDisabled is true while IotAuthorizerSigningPublicKey is empty. AWS IoT does not allow SigningDisabled to be changed on an existing authorizer. Do not update that flag. Do not delete live-ninja-iot. Tab5 firmware does not send a signature and must keep using live-ninja-iot.

Create a second authorizer with signing enabled and a new name. Use the public key that is already stored as a GitHub variable. Do not print the variable value. The private key is already in SSM at /live-ninja/prod/iot/authorizer_signing_private_key, synced from GitHub secret IOT_AUTHORIZER_SIGNING_PRIVATE_KEY. internal/auth/iotsign.go signs with RSA SHA-256 PKCS1v15. The mint in internal/webapp/iot_routes.go already returns tokenSignature when that key reads, and it returns signingRequired false.

Change only the web and Android credential response so those clients get the new authorizer name and signingRequired true. Tab5's credential response, if it uses the same route, must stay on live-ninja-iot with signingRequired false. If one route serves every client, add a field or a query the Tab5 does not send, and default that path to the old authorizer. Do not point Tab5 at the signed authorizer.

web/static/js/liveevents.mjs function iotSocketUrl and android/.../net/IotUrl.kt already append x-amz-customauthorizer-signature only when signingRequired is true. Keep that gate. URL-encode the signature.

Done when all three are true:

aws iot describe-authorizer --authorizer-name live-ninja-iot

shows signing disabled.

The new authorizer exists and is active.

go test ./internal/webapp/ -run IoT

passes, including a case that the default credential response still names live-ninja-iot.

### android-signature-release

Depends on iot-signed-authorizer being deployed. The phone Galaxy S9 serial 4633424442303098, model SM-G965U, package ninja.jeremy.liveninja, is on versionName 0.3.11, versionCode 17. That build ignores unknown JSON and does not send the signature.

Ship the Android code that is already in IotDtos.kt and LiveEventsClient.kt, bump versionCode above 17, and publish with GitHub Actions workflow "Android Release" (file .github/workflows/android-release.yml, workflow_dispatch on main). Do not assemble a release APK on the laptop and sideload it as the publish step. After the workflow is green, install that APK on serial 4633424442303098 only. Two other adb devices may be attached. Every adb command uses -s 4633424442303098.

Done when https://live.jeremy.ninja/v1/app/android/latest shows versionCode greater than 17, and this prints the same versionName:

adb -s 4633424442303098 shell dumpsys package ninja.jeremy.liveninja

### emb-retire

Not before 2026-09-28. On or after that date, remove EMB# writes, ListEmbeddings, the cosine path, and the Titan IAM statement. Rewrite the memory sections of contracts/api.md, PRD.md, and docs/system-map.md. Done when this prints nothing:

grep -rn "EMB#\|Cosine(" internal/ cmd/

and go test ./... passes. DynamoDB ENT# rows stay.

## How to ship

Work on main. Commit only the files for the milestone. Push to origin main. Watch the Deploy workflow until it is success or failure. A red run is fixed and re-pushed. Three attempts, then mark the milestone [!] with the run id and start the next milestone. Do not add a Co-Authored-By trailer.

CloudWatch: do not add alarms or dashboards. Every new log group has RetentionInDays 7.

Help: any user-visible setting, page, or tool updates web/templates/pages/conversation.html in the same commit.

On host OFFICEPC, ask before opening a browser and before using Windows MCP. This machine's hostname may be OFFICEPC. adb, go test, and gh do not need a browser.

## Do not do

- Do not run spoken smoke tests. F1 and the E4 spoken turn are bypassed.
- Do not resume wake-word training. Default phrase is hey-jarvis. Custom training is off.
- Do not build backlog.md items. That includes Nova re-enable, Tab5 provisioning, G1 through G6 in azure-voice-plan.md, and the 2026-09-22 "Moved out" list.
- Do not flip SigningDisabled on live-ninja-iot.
- Do not print secrets. Do not commit PEM, keystores, or tokens.
- Do not install a debug APK over the S9 release build except as the published Android Release artifact.
- Do not start emb-retire before 2026-09-28.

## First action

Read plan.md. Run go test ./cmd/realtime-broker/ -count=1. Record the result in the execution log. Then start voice-catalog by opening the Microsoft voice list and quoting it before you add a name.
