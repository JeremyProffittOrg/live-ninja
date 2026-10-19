# Backlog

## Moved out of plan.md on 2026-09-22

These were unfinished checkboxes in the archived plan. They are not part of `ship-remaining`. Do not build them unless the operator promotes one back into `plan.md`.

- `codeupdate-spoken-run` — one spoken code-update run was never exercised. Spoken smokes were bypassed 2026-09-22. ⟵ completed/plan-2026-09-22.md §1
- `fleet-apply-failure-status` — publish `last_update_error` on ghost-cli node status. That code is not in this repo. ⟵ completed/plan-2026-09-22.md §2
- `windows1-updater` — a stale ghost-cli node that is not this machine. The archived note says it is not plan content. ⟵ completed/plan-2026-09-22.md §2
- `gemini-long-session` — Gemini E1 continuation past 30 minutes was implementation-blocked. Mint, audio, tools, and cost were already proven. ⟵ completed/plan-2026-09-22.md §3.1
- `tool-manifest-owner-smoke` — owner smoke of the tool catalog, including `code_update_*`. Not a build task. ⟵ completed/plan-2026-09-22.md §3.1
- `android-tablet-signin` — visual check of the persona picker needs the owner signed in on tablet `R52XC06P9KJ`. ⟵ completed/plan-2026-09-22.md §4
- `android-legacy-row-retest` — web path passed; the Android retest was still open. ⟵ completed/plan-2026-09-22.md §3.3
- `wake-custom-training` — superseded 2026-09-20. Owner picked bundled phrases only. Default `hey-jarvis`. Do not resume. ⟵ completed/plan-2026-09-22.md §7.4

Future actions **deliberately kept OUT of the plan**. Not scheduled work. Consolidated by
`/clean-plans` on **2026-07-24**.

Promote an item by moving it into [plan.md](plan.md) under a workstream/milestone. Nothing here
should be picked up as "next" without that decision being made explicitly.

## Voice engines

- **Re-enable Nova Sonic (M12).** Fully built and connect-chain-verified, then **disabled by the owner** because the ALB + Fargate standing cost wasn't worth it. Re-enable = flip `NovaBridgeEnable` (two-phase: repo/ALB → image push → `NovaBridgeReady=true`, isolated deploy, nothing else in the same push) + restore the picker option; the ECR repo was force-deleted. ⟵ archive/plan.md M12 / §8 M14 item 3
- **Speed / Energy / Register voice knobs.** Proposed to the owner, never picked. ⟵ archive/plan.md §8 M14 item 11b

## Tab5 / M5Stack — whole surface removed from the plan (owner, 2026-07-24)

The Tab5 is **out of scope as scheduled work**. The shipped firmware still functions (HIL-verified
multi-turn voice loop, "Hi Lily" wake word, WSS direct to OpenAI), and the backend serves it
unchanged — but nothing below is planned. Promote items back into `plan.md` only if the surface is
picked up again. Full history: [archive/plan.md](archive/plan.md) M5 + §8 M5 notes.

- **`ProvisionIoT` is an empty hook** — `internal/auth/device.go`. The IoT identity leg (Fleet Provisioning by Claiming Certificate, on-chip keypair X.509, 10-yr lineage, per-device topic policy `${iot:Connection.Thing.ThingName}`, `IOT_DATA_ENDPOINT`) has never been exercised end to end. This was the project's **one genuinely unimplemented stub**; it is unimplemented by choice now. The `DELETE /devices/{id}` revoke path (detach + delete cert/Thing) is likewise written but unproven. ⟵ archive/plan.md M5 / §8 M14 item 12
- **Tab5 hardware pairing e2e** — on-screen user code → confirm page → PKCE claim → 10-yr refresh. Blocked by the hook above. ⟵ docs/qa-report.md Surface 1
- **CSRF middleware route exemption** for the device-pairing confirm page's no-JS native POST (the inline progressive-enhancement fetch covers the JS case today). ⟵ archive/plan.md §8 M7
- **Shadow `ln-` prefix collapse** — lower security-review finding, was queued for M8 cleanup. ⟵ archive/plan.md §8 M7
- **OTA never exercised** — A/B partitions, IoT-Jobs canary → fleet, mark-valid-after-check-in, rollback-on-fail: all implemented, never run.
- **Security hardening never enabled** — flash encryption + Secure Boot v2 + NVS encryption were deferred through the entire build. **Do this before the device ever leaves the bench.**
- **HIL rig never wired to CI** — PlatformIO flash + serial/telemetry MQTT assert exists as scaffolding only.
- **`device_control` smoke step** — "reboot the terminal" → `device_control` with `action:reboot` was step 3 of the post-M19 tool-manifest smoke; unverifiable without a Tab5. ⟵ archive/tool-parity-plan.md §Verification
- **Live voice round-trip capture from the Tab5** into the shared transcript sink (parity with web/Android) — unverified. ⟵ docs/qa-report.md
- **Owner eyeball of the reworked LCD screens** — onboarding slide-outs, WiFi list-select, subnet picker, QR at bottom, conversation-fills-screen layout: all flashed and serial-verified, never visually reviewed.
- **Nova signed-config handshake on firmware.** The active web/Android bridge now requires the
  broker's `sessionConfig` as the first frame and verifies its signed digest before Bedrock opens.
  Tab5's old Nova branch parses only URL/token and must add this handshake before Nova can ever be
  enabled on that surface. The former empty tool/persona-config backend defect is resolved.
- **Fleet registry row** — reconcile the Tab5 in `c:\dev\fleet\esp32.md` (eFuse MAC `30:ED:A0:E3:01:1E`, last seen COM58). House rule, still worth doing if the board is touched.
- **Watch item:** one `sdio_rx_get_buffer` assert on esp-hosted slave v1.4.7 during a rapid mint-retry storm; next lever is SDIO RX buffer tuning. Only matters if the device is used again.

**If this surface is ever resumed, two build gotchas that cost real time:** `set "MSYSTEM="` before
`export.bat` and set `IDF_PYTHON_ENV_PATH` (export otherwise picks a Python 3.14 env that isn't
installed); open Espressif native-USB consoles with **DTR=RTS deasserted**, or the chip straps into
silent download mode on reset and looks bricked. Flash recipe and the P4/C6 esp-hosted slave-OTA
rule live in `c:\dev\fleet\esp32.md`.

### Tab5 technical debt (was already backlog before the surface was dropped)

- **On-device tool invocation — OpenAI path.** Gemini `functionCall`s now run through `POST /api/v1/tools/invoke` from the `ln_rt` worker task (HIL-verified 2026-09-25 with `get_weather`). The OpenAI-direct branch still ignores every `function_call` except `stop_listening`; only matters if a Tab5 is pinned to an OpenAI engine. ⟵ archive/gemini-plan.md §10 · archive/tool-parity-plan.md §0
- **Uplink shedding during playback — watch.** The 20–90 KB mic-audio drops while the device spoke were internal-RAM starvation, not an SDIO ceiling: LVGL draw buffers held ~110 KB internal, so esp-hosted SDIO RX fell back to PSRAM and the whole network stack ran at 40–115 KB/s. With the buffers in PSRAM (2026-09-25) a 17 s reply had 0 drops and arrived at 0.99x realtime. Reopen only if `uplink behind` or `playback underrun` returns in the serial log. ⟵ archive/plan.md §8
- **Echo-triggered self-barge-in at high volume.** AEC is imperfect at volume; also produces a benign `response_cancel_not_active` server warn. Same class the web solved with `micEagerness=low`; mitigable today via the device sensitivity setting. ⟵ archive/plan.md §8
- **Transient `Could not lock ws-client within 1000 timeout`** during heavy downlink — self-recovers; watch only. ⟵ archive/plan.md §8
- **Root-cause the internal DMA heap exhaustion during session setup** (lwIP TCP buffers + esp-tls internals + AFE + fragmentation suspected). Explicitly filed "for someday" — the PSRAM-fallback patches made it a non-issue. ⟵ archive/plan.md §8 item 11
- **Slim the mint response for the device.** The Tab5 only consumes `clientSecret`/`model`/`mode`/`wsUrl`, but receives rates + full sessionConfig + the ~20-tool manifest (which is why the HTTP body cap had to go 16→64 KB). ⟵ archive/plan.md §8 bug 4
- **More wake phrases from the esp-sr zoo** (Jarvis, Computer, Hey Willow, Mycroft, Sophia, Hi Jason…) — one sdkconfig bool each; ~340 KB partition headroom. ⟵ archive/plan.md §8 item 10

## Web / conversation UX

- **Wake-buffer replay.** First words spoken during "Connecting" are lost to the model entirely. The batch-5 connect-latency prefetch may be enough; replay is the full fix. ⟵ archive/plan.md §8 M14 item 11c
- **Multi-message tool confirmations in fallback turns.** Fallback turns are stateless per message, so "yes, send it" needs a live session. Candidate feature. ⟵ archive/plan.md §8
- **Ship args with fallback-turn tool results.** `tools.Result` has no `Args` field server-side, so the Details dialog shows "(no input recorded)" for fallback turns. Possible future backend tweak. ⟵ archive/plan.md §8 Task #9

## agentcore-knowledge — replace the DynamoDB memory store and the home knowledge relay with AWS managed services

Added **2026-09-14** from the research in
[docs/agentcore-knowledge-research.md](docs/agentcore-knowledge-research.md) (pricing, sources,
concerns). **Not scheduled.** Promote a workstream into `plan.md` only after the owner answers the
decisions below. Estimated run cost at today's scale is about $7–10 per month with no fixed floor;
today's layer costs cents, so this is a capability change, not a saving.

### Decisions the owner must make before promotion (write the answers into plan.md as Locked decisions)

1. **Scope:** memory only (`agentcore-memory`), or memory + owner knowledge (`agentcore-memory` then `managed-kb-knowledge`)?
2. **Extraction residency:** accept AWS built-in strategies (cross-region inference inside the US, $0.75 per 1,000 records), or the override strategy with an owner-chosen Bedrock model ($0.25 per 1,000 + model cost, more code)?
3. **Event granularity:** one event per exchange (user turn + assistant turn) is the default; confirm, because events are the largest line and scale with turns.
4. **Budget ceiling:** the monthly AWS Budgets amount for `bedrock-agentcore` + `bedrock` at which the run stops enabling surfaces (proposed $25).
5. **Retention:** `eventExpiryDuration` (proposed 30 days) and the long-term record pruning rule (proposed: delete records never retrieved in 180 days).
6. **Knowledge corpus export:** whether the `knowledge-plane` repo may gain a nightly S3 export (compact text + source metadata) that a Managed Knowledge Base ingests. Without it, `managed-kb-knowledge` cannot start.

### agentcore-memory — PROMOTED to plan.md on 2026-09-14 (owner took the proposed defaults)

The milestones below are kept for reference only; `plan.md` section `agentcore-memory` is the source of truth.

- [ ] `memory-resource` — `template.yaml` gains one `AWS::BedrockAgentCore::Memory` (or a custom resource if CloudFormation coverage is missing on the day) in us-east-1 with `SemanticMemoryStrategy` + `UserPreferenceMemoryStrategy`, namespaces `/users/{actorId}/facts/` and `/users/{actorId}/preferences/`, `eventExpiryDuration` per decision 5; IAM on the web and broker roles limited to `bedrock-agentcore:CreateEvent`, `RetrieveMemoryRecords`, `ListMemoryRecords`, `ListSessions`, `ListEvents`, `DeleteEvent`, `DeleteMemoryRecord`, `BatchDeleteMemoryRecords` on that one memory ARN, with `bedrock-agentcore:namespacePath` conditions. Done when: `gh run watch` on the deploy is green and `aws bedrock-agentcore-control get-memory --memory-id <id> --query status` prints `ACTIVE`.
- [ ] `cost-guard` — one AWS Budgets monthly cost budget (amount per decision 4) filtered to the `Amazon Bedrock AgentCore` and `Amazon Bedrock` services, notification to the owner's email at 80% and 100% (Budgets, not CloudWatch alarms — house rule); `cmd/usage-rollup` writes `memEvents`/`memRetrievals` per user-month. Done when: `aws budgets describe-budgets --account-id 759775734231` lists the budget and `go test ./cmd/usage-rollup/` passes with a case for the new counters.
- [ ] `event-writer` — the transcript sink path (`internal/webapp/api_routes.go` handleTranscript) writes one `CreateEvent` per exchange for the session's `actorId = userId`, `sessionId = sessionId`, behind a per-user feature flag stored on the user row (default off); failures are logged and never fail the transcript write. Done when: `go test ./internal/webapp/ -run TestTranscript` covers on/off/failure and `-race` is clean.
- [ ] `mint-preload` — `cmd/realtime-broker` calls `RetrieveMemoryRecords` (topK 10, namespace `/users/{actorId}/`) once per mint with a 400 ms deadline and appends a `REMEMBERED` block after BASE KNOWLEDGE; on timeout the mint proceeds unchanged. Done when: `go test ./internal/realtime/ -run TestBaseKnowledge` includes the preload rendering and `go test ./cmd/realtime-broker/` passes with a timeout case.
- [ ] `memory-tools-cutover` — `memory_search` calls `RetrieveMemoryRecords`; `memory_write` and `plan_upsert` write a synchronous record (`BatchCreateMemoryRecords`) and keep the DynamoDB `ENT#` row for the Memory page; `forget` deletes the record and the row; `entity_get` unchanged. `internal/tools/memory.go` tool descriptions updated; the Help drawer and Memory page copy updated in the same commit (`go test ./internal/webapp/ -run TestHelpDrawer`). Done when: `go test ./internal/tools/ ./internal/memory/` passes and a 10-question owner smoke set (home address, spouse's name, current project, last car service, a preference stated in the previous session) answers 9 of 10 from voice on web and Android.
- [ ] `purge-and-export` — `cmd/account-purge` pages `ListSessions` → `ListEvents` → `DeleteEvent` and `ListMemoryRecords` → `BatchDeleteMemoryRecords` for the actor; the account export includes `ListMemoryRecords` output. Done when: `go test ./cmd/account-purge/` covers both paths against a fake client and a purged test user returns zero records from `ListMemoryRecords`.
Restart policy for this workstream: every step is a normal push-to-main deploy; a red deploy is fixed and re-pushed (ceiling 3 per milestone, then `[!]`). Feature flag off = full rollback with no data loss because DynamoDB rows are kept until `emb-retire`.

### agentcore-memory follow-ups (deferred on 2026-09-14, not plan items)

- `android-learned-list` — DONE 2026-09-14 (same day): the Android Memory screen gained a "Learned" tab with Forget (`ui/screens/MemoryScreen.kt`, `ui/memory/MemoryViewModel.kt`, `net/MemoryDtos.kt`).
- `record-pruning` — see plan.md `[!]`: needs an owner decision between age-based pruning and accepting growth.

### managed-kb-knowledge — owner knowledge on a Bedrock Managed Knowledge Base, relay kept as fallback

depends on: `agentcore-memory` shipped, decision 6, and an S3 export job in the `knowledge-plane` repo.

Note (2026-09-26): curated documents no longer wait on this item. `scripts/knowledge-load` writes them straight into AgentCore Memory under `/knowledge/<actor>/<collection>/` (outside the conversation namespaces), and `knowledge_search` answers from there first when a record scores at least 0.45, falling back to the home relay. First collection: `tahoe-2027`, 570 records from the curated 2027 Tahoe owner's manual. This item remains the plan for the bulk knowledge-plane corpus (sessions, mail, pages).

- [ ] `corpus-export` (knowledge-plane repo) — nightly job writes each source item as compact text with metadata (`source`, `source_id`, `title`, `url`, `occurred_at`, `repo`) to `s3://live-ninja-knowledge-<acct>/corpus/<source>/<id>.md`; raw email bodies and session audio are excluded; total under 1 GB. Done when: `aws s3 ls --summarize --recursive s3://…/corpus/ | tail -2` shows the object count and a size under 1 GB.
- [ ] `kb-resource` — `template.yaml` gains the Managed Knowledge Base with the S3 data source and an EventBridge Scheduler nightly `StartIngestionJob`; IAM `bedrock:Retrieve` on the KB ARN for the web role. Done when: `aws bedrock-agent get-knowledge-base --knowledge-base-id <id> --query knowledgeBase.status` prints `ACTIVE` and the first sync reports zero failed documents.
- [ ] `knowledge-tools-cutover` — `knowledge_search` and `knowledge_recent` call `Retrieve` (standard, never agentic) with the same 2,500 ms budget, mapping chunks to the existing result shape; the SQS relay is tried only when the KB call fails or returns nothing; Help drawer copy loses "if the store is asleep". Done when: `go test ./internal/tools/ -run TestKnowledge` passes and the owner's 10-question knowledge smoke set scores at least as well as the relay on the same day.
- [ ] `relay-retire` — after one month with the KB winning the smoke set, remove `KNOWLEDGE_QUERY_QUEUE_URL`/`KNOWLEDGE_RESULTS_TABLE`, the SQS/DynamoDB IAM, and the `knowledge-plane` relay worker's Live Ninja branch. Done when: `grep -rn "kp-query" template.yaml internal/` prints nothing and `go test ./...` passes.

### Stop conditions (only these) when promoted

- The AWS Budgets notification for the run's budget fires: stop enabling surfaces, keep the flag off for new users, report.
- `RetrieveMemoryRecords` p50 measured above 800 ms from the broker for a full day: stop `mint-preload`, keep tools-only recall, report.
- CloudFormation has no resource type for AgentCore Memory on the day and a custom resource would need a new Lambda: report with the alternative (one-time `aws bedrock-agentcore-control create-memory` recorded in `SETUP.md`) and wait for the owner.

## Deferral decisions the owner explicitly parked

⟵ docs/qa-report.md "Deferral decisions (owner call)" — each is a real finding the owner chose not to schedule.

- **History device/topic filter pagination edge case.** Acceptable to defer until history exceeds one page; the fix would be a server-side fill-the-page loop, or keeping "Load more" visible while a cursor exists.
- **Strict refresh-reuse posture.** A benign multi-tab web refresh race triggers a full family-revoke + security alert. Confirm that's acceptable UX for real multi-tab usage, or soften it.
- **Custom-accent-as-text-color AA contrast risk.** Either accept, or derive an independent accent-ink colour / clamp contrast.

## Idea catalogs (unscheduled proposals — not plan items)

Two large proposal documents exist and are **not** scheduled. They are kept where they are as
reference; nothing in them is committed work until an item is promoted into `plan.md`.

- **[docs/agentic-expansion-review.md](docs/agentic-expansion-review.md)** (2026-07-19) — 84 suggestions from a 16-agent review, each anchored to real files, effort-rated S/M/L/XL, with adversarial verifier corrections quoted inline. Themes: remote coding sessions, media playback, news/feeds, briefings, safety.
- **[docs/agentic-expansion-suggestions.md](docs/agentic-expansion-suggestions.md)** (2026-07-20) — 43 capabilities across nine themes + six foundational fixes, sequenced as a proposed **M15–M24** roadmap for an in-car assistant.
  ⚠️ **Numbering collision:** that document reuses **M15–M17**, which are already taken by the Base Knowledge / RCA milestones in `plan.md`. Renumber before promoting anything from it. (Its own F1 correctly identifies Base Knowledge as the hard dependency to ship first — that part is already in the plan.)

## ghost-cli hardening (different repo)

- **`GET /launch/branches` has no `Authorize` call.** Identical gap to the one closed on
  `GET /launch/repos` on 2026-08-02 (`9438054`): it checks only that a principal is non-empty. The
  fix is the same one line — `authz.AuthorizeAnyNode(ctx, principal, authz.ActionLaunch)` — since
  the route also names no node. Deliberately left out of that change because, unlike `/launch/repos`,
  this route is **not internal-invoke reachable** (`internal_invoke.go` lists five pairs and this is
  not one), so the tautology that made the repos gap urgent does not apply here. ⟵ plan.md §2.2

## Platform capabilities not being pursued

- **FCM push for settings fan-out.** No Firebase account — web and Android use poll/foreground reconcile instead; the Tab5 uses the IoT shadow. Locked at M6. ⟵ archive/plan.md §8 M6
- **Porcupine wake engine.** Catalog-flagged unavailable (needs Picovoice seats); openWakeWord is the free default and the reason training is never blocked. ⟵ archive/plan.md §8 M6
- **SNS ops-topic email subscription confirmation.** Optional by design: the owner wants **no CloudWatch alerts**, budgets email directly, and the topic's only producer is SES bounce/complaint. Confirm the subscription only if bounce/complaint notices are wanted. ⟵ SETUP.md · archive/plan.md §8 M14 item 12
- **Bedrock Nova Sonic model access in `us-east-1`.** Was only ever needed for M12, which is disabled. ⟵ SETUP.md


## android-auto-deferred — owner deferred all Android Auto work on 2026-10-17

Owner instruction, verbatim: "put anything for android auto in the backlog". Not scheduled. Do not implement, test, distribute, or provision Android Auto until the owner explicitly restores it to plan.md. Bluetooth and ordinary native-phone calling remain in the active plan.

The original workstream and mixed car test/release requirements follow verbatim so projection, DHU, Play distribution, supported-phone and acceptance details are retained. The duplicated Bluetooth/native portions below are historical context only; their active requirements remain in plan.md. Original dependency names and commands are retained for a future promotion and are not active gates.

## android-auto — integrate real calling with the car display
depends on: phone-targets, car-route-fix, call-client, assistant-call-task

Jobs: local-test-job, delivery-job, human-hardware-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat auto-test-distribution" to check the terminal gate and its dependency evidence.


### auto-platform-gate — use supported calling APIs and distribution
depends on: phone-targets

- [ ] auto-eligibility — recheck Google's calling guide and supported categories immediately before implementation. Current documented route is Android Auto calling via Core-Telecom, with optional Car App Library calling templates. There is no documented general-purpose AI-assistant category. Keep direct assistant chat on the phone/Bluetooth unless Google explicitly supports the proposed car surface.
- [ ] auto-release-access — confirm Play account/application identity and Internal/Closed Testing access. Calling is currently a labs feature restricted to those tracks; do not promise Open Testing or Production. Trusted-source installation is required for real-car Car App Library tests; the current sideload APK flow alone is insufficient.
- [ ] auto-phone-support — reserve an eligible newer phone and compatible car. Google's current requirements state Android 11+; S9 Android 10 remains a Bluetooth regression device despite an installed Android Auto package. No OS flashing or phone purchase is implied.



Definition of done: from C:\dev\live-ninja, "scripts\verify-phone-evidence.bat auto-platform-gate" exits 0. Required checks: read-only Play/device/category eligibility checks using the locked package, account and recorded newer-phone serial. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### auto-call-integration — native call lifecycle and supported car controls
depends on: auto-platform-gate, car-route-fix, call-client, assistant-call-task

- [ ] auto-telecom — integrate Android Twilio calls with Core-Telecom in ordinary phone use as well as projection. Map answer/active/inactive/disconnect/mute and remote actions; observe endpoint changes and request routes through Telecom. Handle each remote action within Google's five-second requirement. Use correct call notification/foreground lifecycle and do not make two audio controllers compete.
- [ ] auto-car-ui — add the permitted calling service/templates and manifest metadata needed for call launch/contact/history navigation. Android Auto owns its in-call UI. Do not assume custom Listen/Take over buttons are available in that UI; prove the supported control path and keep phone controls honest where the host limits them.
- [ ] auto-session-continuity — preserve the correct call, permissions, participant roles, and audio route when projection connects/disconnects, phone locks, the app backgrounds, and a cellular call interrupts. Handle assistant takeover/take-back through the previously approved call state machine.

Expected additional files: Android manifest and car XML resources, existing Android session/call integration, a scoped CarAppService if required, version catalog/Gradle entries for selected Core-Telecom/Car App Library/Twilio SDKs, related tests, Help and delivery docs. Dependency additions belong to this requested feature only; verify exact compatible versions before implementation.





Definition of done: from C:\dev\live-ninja, "scripts\verify-phone-evidence.bat auto-call-integration" exits 0. Required checks: Android gradlew.bat :app:testDebugUnitTest :app:lintDebug :app:assembleDebug; scoped connected tests for Telecom lifecycle. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### auto-test-distribution — publish the test artifact before vehicle acceptance
depends on: auto-call-integration

- [ ] auto-test-artifact — create a versioned signed test artifact through GitHub Android Release, with a version code above the actually installed code. Publish to the approved Play Internal/Closed test route. Record package, signing verification result, version, commit, track and artifact receipt without key material.
- [ ] auto-test-install — install the trusted-source test build on the selected supported phone and verify the installed package/version and Android Auto discovery. Do not require final car acceptance before making this test artifact available. This milestone does not claim public production release.


Definition of done: from C:\dev\live-ninja, "scripts\verify-phone-evidence.bat auto-test-distribution" exits 0. Required checks: GitHub/Play artifact receipts and installed-device version/commit checks. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## car-call-testing — extensive device, head-unit, and real-call verification
depends on: car-route-fix, auto-test-distribution, call-modes-acceptance, recording-app-access

Jobs: local-test-job, delivery-job, controlled-call-job, human-hardware-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat car-release-proof" to check the terminal gate and its dependency evidence.


### car-test-matrix — automated and physical coverage
depends on: car-route-fix, auto-test-distribution, call-modes-acceptance, recording-app-access

- [ ] car-unit-matrix — test API 29/30 legacy routing and API 31+ routing; connection arriving late; refused route; missing permissions; route removal during capture; focus denied/lost/regained; wake mic contention; duplicate end; process/session cancellation; Telecom endpoint ownership; stale AI output after Take back. Existing tests first, new test classes only for new observable behavior.
- [ ] car-device-matrix — on the S9, run Bluetooth HFP and handset/wired checks. On a supported newer phone, run modern HFP and BLE if hardware supports it, plus wired/wireless Android Auto where the actual car supports them. Unsupported hardware modes are explicitly not covered, never silently passed.
- [ ] auto-dhu-matrix — run Google's Desktop Head Unit with the eligible phone; test supported call UI, microphone granted/denied, answer/reject/end/mute, reconnect, screen configurations, and simulated driving restrictions. Record host version and commands. DHU does not prove car acoustics or Bluetooth microphone routing.
- [ ] car-real-call-matrix — place controlled inbound and outbound Twilio calls through the parked car, covering human speaking, assistant Listen, Take over, Take back, and assistant-only continuation. Verify separate microphone uplink and speaker downlink with a test phrase at each end. Check remote party cannot hear muted user audio and hears no AI audio in Listen mode.
- [ ] car-failure-matrix — during controlled calls, test car/Bluetooth disconnect, headset/handset route change, USB projection unplug, phone lock/background, cellular interruption, network loss, remote hangup, and owner stop. Verify no duplicate dial, no dropped remote leg during an intended handoff, no persistent capture, and no orphaned paid call legs.
- [ ] car-soak — after basic success, run 10 connect/start/end cycles on the S9 and on the supported modern phone, plus one 20-minute controlled call with interruptions and projection reconnects. Use the locked budget; lack of budget is not permission to run unbounded calls.

Acceptance thresholds: 10/10 lifecycle cycles release their resources; every required mode has audible two-way evidence; zero duplicate calls, unintended microphone disclosure, AI speech in Listen mode, or orphaned call legs; every supported car remote action is acknowledged within five seconds. Capture route-acquisition and handoff timing rather than invent a universal Bluetooth latency limit. Any failed criterion fails its scenario; preserve partial results and fix before repeating. Allow at most three attempts per failing scenario, with a classified change between attempts.



Definition of done: from C:\dev\live-ninja, "scripts\verify-phone-evidence.bat car-test-matrix" exits 0. Required checks: Android gradlew.bat :app:testDebugUnitTest :app:lintDebug :app:connectedDebugAndroidTest; scripts\verify-phone-evidence.bat run-case auto-dhu-matrix; scripts\verify-phone-evidence.bat run-case car-real-call-matrix; scripts\verify-phone-evidence.bat run-case car-failure-matrix; scripts\verify-phone-evidence.bat run-case car-soak. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### car-release-proof — publish only verified behavior
depends on: car-test-matrix

- [ ] car-help-release — update Help in the same feature commits, including Bluetooth setup, route choice, microphone state, call controls, supported Android Auto test distribution, and recovery instructions.
- [ ] car-ci-release — use existing GitHub Android Release workflow, select a valid version above the installed code 21, verify signature/hash and installed version without reading keys. Add the approved Play testing delivery step through GitHub if needed; preserve existing AWS delivery and APK updates.
- [ ] car-installed-acceptance — repeat the key parked-car audio and controlled-call checks on the exact released artifact installed through the appropriate channel. Passing a debug build alone does not prove the release.





Definition of done: from C:\dev\live-ninja, "scripts\verify-phone-evidence.bat car-release-proof" exits 0. Required checks: go test ./internal/webapp/ -run TestHelpDrawer -count=1; scripts\verify-phone-evidence.bat run-case car-installed-acceptance; GitHub/Play/installed-version checks. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


### android-auto-source-context — original related requirements and decisions

Single active source: repository-root plan.md; current implementation checkout C:\dev\live-ninja-plan-run\plan.md. Completed history: C:\dev\live-ninja\completed\plan-2026-10-15-history.md and earlier completed/ archives. Deferred work: C:\dev\live-ninja\backlog.md. The current plan covers telephony, Jobs, S3 recordings, car Bluetooth, Android Auto, and their acceptance tests.
13. **2026-10-15 — Bluetooth repair, Android Auto, and extensive call tests.** Verbatim: "Also, when I use live=ninja in my car, it does now alloe me to use my car's hands free over bluetooth, research and add that fix, and add update the live-ninja app to also be an android auto application.  Add extensive testing include test calls." This adds research and planned implementation/testing; the start hold remains.
19. **2026-10-15 — target vehicle.** Verbatim: "chevy suburban 2027 stock head unit". Target the 2027 Chevrolet Suburban stock head unit; test window, symptom surface, newer-phone access and Play access remain unconfirmed.
23. **2026-10-15 — execute today's trial and email results.** Verbatim: "make 20 test calls today using that, and email me the results, along with temporary s3 links to the recordings (7 day ttl)" This authorizes only the bounded 20-call trial and its required backend/recording/email deployment now. Broader app/Android Auto implementation remains on hold.
- Current USB evidence (2026-10-15): serial 4633424442303098, SM-G965U, Android 10/API 29, app 0.3.15/code 21. Windows USB status OK; wake and MainActivity control passed. Full evidence and current Android Auto requirements: C:\dev\live-ninja\docs\android-auto-research.md. Physical car audio not tested.
Authority: deployments, deployment retries, normal deployment cost, and required restarts have standing authorization dated 2026-08-16; do not ask for deployment approval again. The 2026-10-16 implementation instruction lifts the historical no-start hold; the separate call stop remains effective. Number acquisition and test calls must fit the locked spending rule; recording retention is 180 days. Play test audience/access and any expanded autonomy must be resolved before affected execution. No unrelated deletion, OS flashing, new hardware purchase, new local schedule, or third-party contact outside the approved list is authorized.
Definition-of-done rule: all commands named by a milestone must exit 0 and all required evidence must exist. Missing hardware, failed provider calls, partial hotel answers, and unsupported Android Auto modes remain explicit statuses; they cannot be converted to green by omitting cases. A partial hotel report can be delivered while the unmet five-interview target remains open.
- vehicle-test-input — resolved model: 2027 Chevrolet Suburban, stock head unit. Remaining: native app/browser and affected directions, parked test window, Android 11+ test phone and Play testing access. Discover available resources before asking for anything missing.
Discover exact account/resource identities, existing credentials by names only, model quota, eligible credit offer, official hotel numbers, installed artifact SHA, existing Play access, and driver/tool availability from the environment before asking the operator to resolve ambiguity. A missing SDK tool is an implementation/preflight dependency, not an operator question by default.
- Android Auto eligibility, supported newer phone availability, Play testing access, and seamless bot-to-conference transport are unverified. They are explicit gates, not promised facts.
- human-hardware-job — parked-car/DHU test with a named available operator and device. Exact case, duration, observed evidence, and timeout are registered before launch. No response means unverified, not passed. Do independent work while waiting.
- unresolved-run-input — an essential owner-only decision, required credential/resource, parked-car/newer-phone access, or Play test access is absent. Mark the dependent work [!] and continue unrelated authorized work.
- platform-boundary — Android Auto policy or provider capabilities prohibit the planned feature; do not bypass the category/distribution rule.
Execution sequence: resolve the front-loaded batch and verification tools first. Start provider/Play eligibility checks and car reproduction access early. Then run independent Bluetooth repair, Azure/call foundation, and media topology work where their actual dependencies permit. Recording ingestion is required before final calling acceptance. Deliver a Play testing artifact before real-car Android Auto acceptance; final release verification follows those tests.
- [ ] auto-platform-gate — blocked by: phone-targets; gate: scripts\verify-phone-evidence.bat auto-platform-gate
- [ ] auto-call-integration — blocked by: auto-platform-gate, car-route-fix, call-client, assistant-call-task; gate: scripts\verify-phone-evidence.bat auto-call-integration
- [ ] auto-test-distribution — blocked by: auto-call-integration; gate: scripts\verify-phone-evidence.bat auto-test-distribution
- [ ] car-test-matrix — blocked by: car-route-fix, auto-test-distribution, call-modes-acceptance, recording-app-access; gate: scripts\verify-phone-evidence.bat car-test-matrix
- Surfaces: Android is now explicitly requested by the car/Android Auto addition. Native SDK, audio-focus and Telecom integration are in planned scope. Preserve the proposed web dialer; lock whether web and Android ship together before execution.
- [ ] phone-run-lock — replace tentative paths and verification commands with exact account-specific values. No unresolved decision required by a runnable workstream may be deferred to that workstream. Missing Play/car access blocks only its dependents.
- [ ] recording-storage-index — use private encrypted S3 plus existing owner-scoped index patterns. Reuse the deliverables bucket only if the locked retention fits its 180-day lifecycle; otherwise define a recording-specific bucket/lifecycle without changing unrelated deliverables. Playback-ready requires a verified S3 object and metadata commit. Never overwrite a different recording on a repeated event.
Non-goals: replacing the phone OS, resetting Bluetooth pairings, moving app data, enabling disabled voice engines, building Android Automotive OS, disguising assistant audio as a music app, or modifying unrelated Jobs work.

## parked-car-deferred — owner set aside the parked Suburban session on 2026-10-19

Owner quote: "parked suburban session, ignore". Promote back to plan.md only on an explicit decision.

- [!] parked-native-baseline — Jeremy deferred the actual audio test with "I will check later". S9 Android10/app0.3.17/code23 and microphone permission verified. One-time session6920/PID29884 stopped:31 sanitized samples20:38:25–20:41:02UTC showed ADB online, no active HFP device, connection0/audio10; A2DP spot check also had no active device. No microphone/speaker observation or routing transition occurred, so this is not proof of an app defect. Jeremy must reconnect the actual test phone for car phone-call audio and provide a parked test window. No audio/calls were started.
- [ ] parked-owner-call-baseline — depends on: parked-native-baseline. Use only configured owner destinations and existing bounded paths; no hotel calls. Before any call, verify the exact owned-number callback, current PIN enrollment, active owner and budget. Existing owner-message path is90seconds/downlink-only; real inbound owner sessions are180seconds and require private PIN plus keypad recording consent. A new current-date activation workflow is needed if routing is not already correct; the October16 rehearsal cannot be reused.
- [!] parked-route-release — route never connected during the observation, so release cannot be measured. Sampler session6920 closed after intentional bounded stop; no background monitor remains. Requires an actual connected native test before acceptance.
- [!] bluetooth-physical-proof — Jeremy must provide the parked Suburban test window and verify actual microphone/speaker audio. S9 USB access, signed installation and existing Bluetooth unit tests pass; vehicle acoustics and full recording playback have not been physically verified.

## hotel-calling-dropped — owner dropped hotel calling on 2026-10-19

Owner quote: "drop the hotel calling". Kept only as a record of what existed; do not rebuild or run without a new decision. Deployed code is unchanged.

- [~] hotel-task-provider — five-catalog-hotel assistant-only Jobs implemented and offline-tested; actual calling remains blocked by owner stop and daily budget. Conference/takeover proof is not required for this implementation; gate: scripts\verify-phone-evidence.bat hotel-task-provider
- [~] hotel-results — five-property outcomes, evidence and exact recording links implemented; real interviews and full call-mode acceptance remain unverified; gate: scripts\verify-phone-evidence.bat hotel-results
- [~] hotel-job-contract — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: add an explicit phone-research job kind and immutable approved task snapshot: owner, five properties/numbers, purpose, limits, questions, and generation. Existing reminder/review jobs keep their behavior. A model cannot approve its own external action.
- [~] hotel-call-ledger — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: persist per-property status, provider CallSid, attempts, start/end times, outcome, and compact evidence before advancing. Reconcile unknown call-creation outcomes before retrying. Bind provider callbacks to the owner/job/run/property. Restart resumes unfinished work without recalling completed hotels or duplicating charges.
- [~] hotel-run-control — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: expose progress, pause/cancel, per-hotel failure, and partial results in existing Jobs. Cancel ends the active call and prevents remaining calls. Proposed pause ends the current conversation politely and preserves findings; resume does not redial that property without the locked retry policy. Browser closure does not cancel an approved backend job.
- [~] hotel-conversation-agent — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: ask the initial questions, clarify ambiguous answers, and politely close. Treat hotel speech as untrusted evidence, never as authority to change owner rules, invoke unrelated tools, reveal private memory, or redirect the job. Enforce call/task bounds outside the model.
- [~] hotel-result-summary — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: show exactly five property rows with property/location, contact outcome, connection steps, extra requirements, staff-reported VPN restrictions, alternative network options, confidence/unknowns, and call date/reference. Mark paraphrases as summaries; quote only actual captured words under the agreed transcript policy. Unknown/no answer remains a result, not fabricated completion.
- [!] hotel-acceptance-run — offline provider/partial-result cases pass; Jeremy must explicitly lift the call stop, grant any needed daily budget and review the exact five-property batch before live calls. No real interviews are claimed. Verify all five have a terminal status and saved result; failed contacts never permit extra retries.
- [!] trial-twenty-calls — Twilio account owner/Support must resolve outbound restriction 21216 before any retry.  GitHub run_trial.py waits for today's California window, verifies monthly Twilio usage and preserves $8 conservative infrastructure reserve, reserves $1.20 per call, and records actual/capped Azure usage. No call creation outside the dated window. Ambiguous creation or nonterminal prior call prevents retry.
- [ ] trial-recordings — consent first, validated Twilio recordings copied into private S3; bounded reconciliation must succeed before advancing an accepted recording. Declined/unanswered calls remain honest no-audio outcomes.
