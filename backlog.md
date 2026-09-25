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
