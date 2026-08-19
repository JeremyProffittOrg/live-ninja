# Azure migration — gap register

Findings from a seven-reviewer audit of `/c/dev/live-ninja/azure-migration-plan.md` against the
repository, run 2026-08-19 on branch `alexa-version`.

**This file is the findings register. It is not a plan.** Items marked `[x]` are already applied to
`azure-migration-plan.md`; items marked `[ ]` are outstanding and belong to the milestone named in
their ANCHOR. Nothing here is a backlog item — a backlog item goes in `backlog.md`.

## How this was produced

Seven reviewers took non-overlapping slices of the plan, each grounded in the repository rather than
in the plan's own prose: cutover/release, security/identity, data migration, cost, device fleet,
realtime protocol, and platform/observability. Every load-bearing claim was then re-run in the main
thread before being recorded here.

## Verification legend

- **VERIFIED** — a command was run in the main thread, or a file was read, and the output is quoted.
- **REPORTED** — a reviewer's finding whose repo evidence is cited but which was not independently
  re-run in the main thread. Treat as credible, not settled.
- **INFERRED (Azure)** — reasoning about Azure service behaviour, not about this repository. **Check
  against current Microsoft documentation before acting.** No live Azure account was queried.

---

## The four false "Verified facts"

These mattered most, because later milestones were sized and sequenced on them.

- [x] **F1. The daily and monthly quota caps are inert.** BLOCKER.
  ANCHOR: `## Verified facts` → "Quota is already externalised"; WS-B M5.
  The gates read `daySeconds` (`internal/realtime/quota.go:328`), `monthTokens` (`quota.go:750`) and
  `dayTokens` (`quota.go:644`). The only production writer is `quota.go:942`, which `ADD dayMints`
  only. `grep -rn 'AddDayUsage\|AddMonthUsage' --include=*.go .` returns the two definitions
  (`internal/store/usage.go:92`, `:102`) and exactly one caller — `usage.go:112`, `BumpDayMints`,
  passing `0, 0, 1`. So both counters are always zero and neither cap can fire.
  Corroborated independently by `docs/launch-go-no-go-2026-07-26.md:41`, which already carries
  per-user spend enforcement as **HOLD** with the same diagnosis. VERIFIED.
  Pre-existing AWS defect, inherited not caused. Resolved by locked decision 6 (measure, soft-warn,
  never block) rather than by building enforcement.

- [x] **F2. "A 5-query problem, not a schema rewrite" is wrong.** BLOCKER.
  ANCHOR: `## Verified facts` → "Data layer"; WS-C M2.
  17 non-test files outside `internal/store` build DynamoDB requests directly, each with a private
  interface rather than the `ddbAPI` seam. The seam itself is typed entirely in DynamoDB SDK structs
  (`internal/store/store.go:31-40`) and the package carries 30 `UpdateExpression` and 85
  `ConditionExpression` uses, so satisfying it from Cosmos means parsing DynamoDB expression grammar.
  It declares 7 methods, not the 8 the plan claims. VERIFIED.

- [x] **F3. Locked decision 3's premise about `cmd/iot-authorizer` is false.** BLOCKER.
  ANCHOR: `## Locked decisions` item 3; WS-G M2.
  `cmd/iot-authorizer/main.go:1-2` — "the AWS IoT Core custom authorizer fronting MQTT over
  WebSockets for **the web and Android clients**". `LiveEventsClient.kt:318` builds
  `wss://${c.endpoint}/mqtt?x-amz-customauthorizer-name=${c.authorizerName}`. Devices authenticate
  with X.509 and never touch it, so IoT Hub is not its replacement. VERIFIED.
  Decision annotated, not reversed. Resolved by locked decision 8 (Azure Web PubSub, new WS-G M5).

- [x] **F4. "The web tier is already portable" is true of the process, false of the wiring.** BLOCKER.
  ANCHOR: `## Verified facts` → "The web tier is already portable"; WS-D M1, M2.
  `cmd/web/main.go:29-35` imports five AWS service clients — DynamoDB, Firehose, Lambda, S3, SQS —
  and `internal/webapp/api_routes.go:373` reaches `realtime-broker` over `deps.Lambda.Invoke`. WS-D M2
  makes that broker an internal-ingress HTTP Container App and no milestone changes the caller.
  VERIFIED.

---

## Definitions of done that pass while the work is undone

The most common defect class in the plan, and the most dangerous one for an unattended run.

- [x] **D1. WS-D M4 and WS-E M1 curled the production hostname.** MAJOR.
  Both tested `https://live.jeremy.ninja`, which answers from CloudFront until WS-J M3 repoints it.
  Each would have passed against the live AWS stack. VERIFIED. Both repointed.

- [ ] **D2. WS-C M2 passes with zero Cosmos executed.** BLOCKER.
  `go test ./internal/store/...` "with zero test-file changes" is satisfied today by the unmodified
  DynamoDB implementation, because every store test injects `internal/testutil/ddbfake.go`, a
  633-line DynamoDB expression emulator. VERIFIED.
  FIX: require the Azure Cosmos DB emulator — `COSMOS_EMULATOR_ENDPOINT=... go test ./internal/store/... -tags cosmos`
  — running the same table-driven cases, and keep the DynamoDB path passing alongside.

- [ ] **D3. WS-D M2's job count cannot detect a missing worker.** BLOCKER.
  `az containerapp job list --query "length(@)"` returning "6 or more" passes on the named set alone.
  `Makefile:15` lists 13 functions and `template.yaml` declares 13 `AWS::Serverless::Function`
  resources; `authorizer` and `deliverables-zipper` appear in no workstream. The template has exactly
  one `Type: Schedule` event (line 838, `usage-rollup`), so calling `topics-extract` and
  `account-purge` "scheduled" is wrong — both are direct-invoke with per-request payloads, as is
  `deliverables-zipper`. VERIFIED.
  FIX: name-check every job in a loop; port the three direct-invoke workers to Service Bus queues plus
  event-driven Jobs; decide `authorizer` explicitly (`internal/webapp/middleware.go:295-325` already
  does the same Bearer check, but deleting it costs the 60s per-user cache and adds a read per
  request — put that read in the WS-C M3 RU report).

- [ ] **D4. WS-B M7 and WS-H M4 pass with no Help copy written.** MAJOR.
  `internal/webapp/help_drawer_ui_test.go:119-133` asserts section headings, and "Voice engine"
  already exists at `conversation.html:1313`. No assertion names an individual engine. REPORTED.
  FIX: add a loop asserting the Help copy names all three new engines.

- [ ] **D5. WS-G M2 passes today, before any porting.** MINOR.
  `go test ./cmd/iot-ingest/` returns `[no test files]` with exit 0, and the second clause only
  proves a directory was deleted. REPORTED.

- [ ] **D6. WS-A M6's `what-if` check is true by construction.** MAJOR.
  "Exits 0 with no `Delete` operations" on a resource group WS-A M2 just created — every operation is
  a Create. Nothing in the command inspects a retention value. REPORTED.

- [ ] **D7. WS-B M4 cannot detect a missing engine.** MAJOR.
  `internal/realtime/rates.go:54` sets `defaultRates = modelRates["gpt-realtime"]` and `RatesFor`
  silently returns it for any unknown model; `rates_test.go:33` asserts that fallback is correct.
  `modelRates` has exactly two keys, so `openai-realtime-mini` is billed at full rates today.
  VERIFIED. Fix applied to the plan; the code change is outstanding.

---

## Runtime breakage

- [ ] **R1. The page CSP has no Azure host.** BLOCKER. ANCHOR: WS-H M1.
  `internal/webapp/pages_routes.go:56` lists `https://api.openai.com` and no `*.openai.azure.com`.
  The Azure SDP POST is blocked before it leaves the page. `render_test.go:139` asserts only a
  substring that holds either way. VERIFIED.

- [ ] **R2. New engine constants never reach the pin path.** BLOCKER. ANCHOR: WS-B M2.
  `internal/realtime/mint.go:485-493` `validEngine` hardcodes the four current engines; anything else
  returns false and `PinToEngine` falls back to `openai-realtime`. The broker routes on a direct
  string compare, `cmd/realtime-broker/main.go:328` `if engine == voiceengine.EngineNovaSonic`, so
  there is no "mint-time alias" hook to hang `nova-sonic` → `azure-voice-live` on. `IsClientDirect`
  has zero callers outside its own definition. VERIFIED.

- [ ] **R3. The settings write path rejects all three new engines.** BLOCKER. ANCHOR: WS-B M2, WS-H M4.
  `internal/webapp/settings_routes.go:502` and `:510` hardcode the four current strings; selecting a
  new engine returns 400. `contracts/settings.schema.json` enums likewise list only four — the plan
  says `nova-sonic` must not be removed but never says the three new members are added. VERIFIED.

- [ ] **R4. An installed Android build sends the Azure credential to OpenAI.** BLOCKER. ANCHOR: WS-I M1.
  `RealtimeSessionApi.kt:55` declares `callsUrl` from the compile-time constant and `grep` shows it is
  **never** populated from the session JSON. `RealtimeSessionCoordinator.kt:204-221` has explicit
  branches for `nova-bridge` and `gemini-direct` and an `else ->` selecting WebRTC. So an old build
  handed `mode: "azure-openai-direct"` does not fail closed — it POSTs the Azure ephemeral credential
  to `https://api.openai.com`. VERIFIED.
  FIX: gate `azure-*` modes server-side on the `X-LN-Client` version header, which already exists
  (`contracts/headers.md:7-33`), before WS-I ships.

- [ ] **R5. A stale web bundle hard-fails on the new mode.** MAJOR. ANCHOR: WS-H M2.
  `web/static/js/realtime.mjs:266-284` falls through an unrecognised `mode` to a `clientSecret` check
  and throws `mint_failed`; a bridged bootstrap has no client secret. The plan's compatibility shim
  points the wrong way — it keeps the new bundle accepting the old mode, but the failure is the old
  bundle meeting the new server. REPORTED.

- [ ] **R6. Pending user reminders are stranded in AWS.** BLOCKER. ANCHOR: WS-J M1.
  `internal/tools/scheduler.go:146` creates one-shot EventBridge `at()` schedules in the group at
  `template.yaml:2512`. WS-J M1 migrates only DynamoDB, so every pending timer dies with WS-K.
  VERIFIED.

- [ ] **R7. The J1 delta cannot observe a delete.** BLOCKER. ANCHOR: WS-J M1, M3.
  The table has no `StreamSpecification` (`grep` returns nothing; the definition at
  `template.yaml:1484-1530` has none), so a repeated full export can add and update but never see a
  DELETE. Sessions revoked during dual-run would come back to life at cutover. PITR **is** enabled
  (`template.yaml:1529-1530`). VERIFIED.
  FIX: DynamoDB incremental export from a recorded watermark, applying DELETE records as Cosmos
  deletes.

- [ ] **R8. Two transactions span two partitions.** BLOCKER. ANCHOR: WS-C M2.
  `internal/store/sessions.go:211` and `:391` issue `TransactWriteItems` across `userPK` = `USER#…`
  and `devicePK` = `DEVICE#…` (`types.go:209`, `:218`). Refresh-token rotate-exactly-once and the
  device-revocation interlock both depend on it. VERIFIED for the repo facts; INFERRED (Azure) that a
  Cosmos transactional batch cannot cross logical partitions.
  FIX: co-locate the device-binding META item into the user partition.

- [ ] **R9. TTL semantics invert on every write, not just the import.** BLOCKER. ANCHOR: WS-C M2, WS-J M1.
  `internal/store/types.go:106` and every writer store an absolute unix epoch. The plan mentions the
  relative-vs-absolute difference once, inside WS-J M1, and only for the one-time import. REPORTED;
  INFERRED (Azure) that Cosmos reads `ttl` as seconds relative to `_ts`.
  Also: expired-but-unreaped rows exist by design (`internal/store/oauth.go:68`, `:214`) and WS-J M1's
  "zero mismatches" DoD forbids the correct behaviour of dropping them.

- [ ] **R10. The JWKS has no dual-key window.** BLOCKER. ANCHOR: WS-E M1.
  `internal/auth/jwks.go:94` publishes exactly one key and `:219` hard-fails on an unknown `kid`, with
  a 24-hour document cache (`jwks.go:31`). During dual-run and at rollback, tokens minted by the other
  stack are rejected in both directions. "Byte-identical JWKS" is unachievable anyway: the `kid` is
  derived from the KMS ARN (`session.go:218-225`) and KMS private key material cannot be exported.
  VERIFIED.

- [ ] **R11. WS-E M1 misses the second signer.** MAJOR. ANCHOR: WS-E M1.
  `auth.NewSigner` has two live call sites: `cmd/web/main.go:194` and
  `cmd/realtime-broker/main.go:925`. The milestone names only the web one, so the broker could keep
  signing under the old key and every bridged session 401s. VERIFIED.

- [ ] **R12. `AuthKey` is unused and should be retired, not migrated.** MAJOR. ANCHOR: WS-E M1.
  It is `SYMMETRIC_DEFAULT` / `ENCRYPT_DECRYPT` (`template.yaml:1535-1544`), and a repo-wide grep for
  `kms.Encrypt`/`kms.Decrypt` returns no call sites. Nothing is encrypted under it. VERIFIED.

---

## Missing milestones

- [x] **M1. No DNS milestone existed.** BLOCKER. Resolved: WS-D M5 and M5a added, WS-J M3 rewritten.
- [x] **M2. No client live-events migration.** BLOCKER. Resolved: WS-G M5 added (locked decision 8).
- [x] **M3. Android distribution left on S3.** MAJOR. Resolved: WS-I M7 added.
- [ ] **M4. No Key Vault, and no secret migration.** BLOCKER. ANCHOR: WS-A.
  The service mapping assigns "SSM Parameter Store → Key Vault" to WS-A, and no WS-A milestone creates
  a vault or moves any of the five secrets. Mitigating: `internal/config/config.go:100-105` shows
  `Loader.Get` short-circuits to an env override, so the five override names at `config.go:39-45` can
  be mounted as Container Apps secret refs with **no** code rewrite. VERIFIED.
  Note the pepper cannot come from `set-secret.sh` — it is machine-generated
  (`.github/workflows/deploy.yml:315-327`) and regenerating it invalidates every device credential.
- [ ] **M5. No managed identity, and Contributor cannot create role assignments.** BLOCKER. ANCHOR: WS-A M4.
  The mapping assigns "IAM roles → Managed identities" to WS-A; no milestone creates one. REPORTED;
  INFERRED (Azure) that built-in Contributor denies `roleAssignments/write`, which would fail the
  Bicep deploy the moment it assigns any data-plane role.
- [ ] **M6. No write-freeze mechanism.** BLOCKER. ANCHOR: WS-J M3.
  "Take a write freeze" names no mechanism and none exists — a repo-wide grep for read-only or
  maintenance mode returns one unrelated comment. Six SQS queues keep draining into DynamoDB.
  REPORTED.
- [ ] **M7. No restart policy outside WS-A.** MAJOR. ANCHOR: all workstreams.
  `grep -n "Restart policy"` returns a single line. WS-J M3 runs inside a write freeze and WS-J M4 is
  a 72-hour unattended window, both with no retry ceiling or abort trigger. VERIFIED.
- [ ] **M8. No Log Analytics destination or retention.** MAJOR. ANCHOR: WS-A M2, M6.
  WS-A M2's DoD checks the resource group, not the workspace. Nothing sets `retentionInDays`, and the
  org rule requires 7 days; Log Analytics defaults to 30. Nothing points Container Apps at the
  workspace, so WS-J M4's "zero errors in Log Analytics" may have no data to read. REPORTED.

---

## Sequencing and dependency defects

- [ ] **S1. The workstream graph is inconsistent and cyclic.** MAJOR.
  WS-G declares "Blocks: WS-J" but WS-J omits WS-G. WS-F declares "Blocks: nothing" while WS-D M4
  routes `/voice-live/*` to it and WS-J M2 must exercise it. `## Sequencing` states a third version.
  WS-B M5's DoD needed the container apps, making WS-B → WS-D → WS-F → WS-B at workstream
  granularity. REPORTED.
- [ ] **S2. WS-A M3's region gate does not gate.** MAJOR.
  Its command targets a Foundry resource no milestone creates — WS-B M1 creates it, and WS-B declares
  "Depends on: A3 only", which is circular. Neither WS-C nor WS-E depends on A3, so Cosmos, Blob,
  Service Bus and Event Hubs get built in a region A3 has not approved. REPORTED.
- [ ] **S3. WS-J M2 can pass without touching Azure.** MAJOR.
  Android is pinned to `live.jeremy.ninja`, which answers from AWS until WS-J M3, so the Android half
  of the dual-run checklist can be filled in green against the old stack. The DoD is also a
  hand-written checklist, against the plan's own rule that every DoD is a command. VERIFIED (the
  pinning); REPORTED (the rest).
- [ ] **S4. WS-J M3's rollback restores DNS but not data.** BLOCKER.
  Any rollback during the 72-hour soak discards every write made on Azure since the freeze. No
  reverse sync is defined. REPORTED.
- [ ] **S5. WS-J M4 and all of WS-K have no usable DoD.** MAJOR.
  WS-J M4 gives no KQL, no definition of "unhandled error", no watcher, and no action on failure.
  `grep -c "DoD:"` returns 53 for the plan and zero for K1-K3. WS-K M3 defers a credential problem
  that becomes live at WS-D M2: `internal/ghost/client.go` authenticates by the Lambda role's IAM
  grant, which a Container App does not have. REPORTED. WS-K M4 now has a DoD.

---

## Device fleet

- [ ] **G1. Firmware shadow and OTA topics are not ported.** BLOCKER. ANCHOR: WS-G M3.
  `ln_iot_shadow.c:45` and `ln_iot_ota.c:61` build `$aws/things/...` topics; `ln_iot_provision.c:274-318`
  uses `$aws/certificates/create-from-csr/json`. WS-G M3 ports only the endpoint, so a reflashed
  device loses settings sync and its only remote-update channel, and `idf.py build` cannot detect it.
  REPORTED.
  Compounding: the OTA channel **is** AWS IoT, so a device never reflashed before WS-K M2 is
  unreachable forever. Ship the Azure-capable firmware over the still-live AWS IoT first.
- [ ] **G2. Device identity continuity is unspecified.** MAJOR. ANCHOR: WS-G M1, M4.
  The backend resolves by `thingName == deviceId` (`cmd/shadow-ingest/main.go:211-225`) and drops an
  unmatched registration **in silence** (`:104`). The operational key is generated on-chip and cannot
  be exported. Fleet size is never stated. REPORTED.
- [ ] **G3. The additive-only rule is applied to one of three surfaces.** MAJOR.
  The plan preserves `nova-sonic` but never schedules the three new enum members being added, and
  silently removes two other `/v1` things the same rule forbids removing: the `mode: "nova-bridge"`
  response value and `WSS /nova/session` (`contracts/api.md:74`), which is what fielded firmware is
  built around (`ln_rt_session.c:8-42`). REPORTED.

---

## Realtime protocol

- [ ] **P1. The SDP host has no plumbing.** BLOCKER. ANCHOR: WS-H M1.
  `realtime.mjs:98` is a module constant consumed through a default parameter that the only
  construction site (`mic.mjs:190`) never overrides, and the session JSON
  (`internal/webapp/api_routes.go:556-569`) carries no host field. Two further OpenAI-specific values
  ride the same path: the `oai-events` data-channel label (`realtime.mjs:699`) and a hardcoded
  `gpt-4o-mini-transcribe` in a live `session.update` (`realtime.mjs:2269`), which on Azure names a
  deployment. REPORTED.
  "Protocol-identical" is an assumption the plan has not tested. Test all three before claiming reuse.
- [ ] **P2. Bridged usage needs three changes, not one.** MAJOR. ANCHOR: WS-F M4.
  The neutral event schema has no usage field, so `NormalizeOpenAI` discards `response.done` usage;
  the bridged bootstrap returns no `rates` object, so `conversation.mjs:1091` returns before any
  arithmetic. REPORTED.
- [ ] **P3. The module-graph guard reads a file the migration retires.** MAJOR. ANCHOR: WS-H M3.
  `internal/webapp/import_map_test.go:108` reads `template.yaml` as its source of truth for
  object-store-backed prefixes, with `require.NoError`. This is the guard for the 2026-08-01
  `/static/vendor/ort/` 403 incident. REPORTED.
- [ ] **P4. WS-F M2's ordering is unenforced and WS-F has an undeclared dependency.** MAJOR.
  Its DoD passes with `nova.go` still present, so "delete only after F3" is a sentence, not a
  constraint. And the bridged engine is not selectable in the web picker today
  (`conversation.html:707-711` records its removal on 2026-07-18), so WS-F M3 and M4 cannot be
  demonstrated until WS-H M4 restores it — yet WS-F declares "Blocks: nothing". REPORTED.

---

## Platform

- [ ] **PL1. The telemetry producer is never ported.** MAJOR. ANCHOR: WS-E M5.
  `internal/webapp/telemetry_routes.go:153-155` is compiled against the Firehose SDK and returns 503
  when unconfigured, logging only a startup warning. This is the same route WS-B M6's cache-ratio
  signal rides. REPORTED. The "within 5 minutes" DoD also races the Capture window it depends on —
  INFERRED (Azure).
- [ ] **PL2. WS-E M7 creates a notification target and nothing that fires into it.** MAJOR.
  An action group with zero rules returns `enabled: true`. On AWS the topic has three named
  publishers (`template.yaml:2799-2837`); the plan drops all three. REPORTED; INFERRED (Azure) that
  reaching an action group requires an alert rule, which conflicts with the org's no-fixed-cost rule
  — that conflict needs an explicit decision, not silence.
- [ ] **PL3. The wake-word training image does not move unchanged.** MAJOR. ANCHOR: WS-E M6.
  `containers/wakeword-train/train.py:661-663` uses boto3 to S3, and the caller
  `internal/wakeword/service.go:99-105` depends on four AWS Batch operations with no named
  replacement. The DoD is an async command that returns on queue, not on success. REPORTED.
  **Collision:** `plan.md` §7.4 is mid-flight against `live-ninja-wakeword-train` on AWS Batch and is
  not governed by this plan. WS-E M6 must not delete it.
- [ ] **PL4. The assets container cannot reproduce the S3 posture.** MAJOR. ANCHOR: WS-D M4, WS-E M2.
  `AssetsBucket` is fully private and read only through CloudFront OAC (`template.yaml:1641-1663`).
  REPORTED; INFERRED (Azure) that Front Door Standard has no Private Link origin, making this an
  explicit choice between anonymous blob read and a Premium profile.

---

## Security

- [ ] **SEC1. WS-A M4's federated subject form is left as a hedge.** MAJOR.
  The plan says "use the immutable-ID subject form if the org requires it"; the recorded org fact is
  that it does. The DoD reads back the string it just wrote, so it passes with the wrong form and the
  failure surfaces later as an `azure/login` failure. REPORTED.
- [ ] **SEC2. WS-B M1's DoD prints a minted credential.** MAJOR.
  It ends `| jq -e '.value'`, which prints the ephemeral secret, and the plan separately requires
  recording "what each verification actually returned" into the committed Execution log. REPORTED.
  FIX: `| jq -e 'has("value")' > /dev/null && echo MINT_OK`, and never run it under `set -x`.

---

## Applied to the plan so far

`c055948` DNS milestone and Route 53 decision · `d4e545f` alias records have no settable TTL, plus the
`workflow_dispatch` cutover hazard · `168bace` three false Verified facts withdrawn · `7ec645b` D5
ordering (custom domain before validation token) · `c4c9474` the quota fact withdrawn · `274e90a`
locked decisions 6-10 · `dfaff92` cost model corrected against its own arithmetic.
