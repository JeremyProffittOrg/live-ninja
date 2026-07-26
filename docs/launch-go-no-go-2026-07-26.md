# Launch go/no-go — 2026-07-26

## Decision

**Full launch: NO-GO.**

The production web/OpenAI service is healthy and may remain live. This decision concerns the
full active launch scope in [plan.md](../plan.md): web plus Android distribution, with enabled
production capabilities represented truthfully. It does not pull the archived M5Stack scope
back into the release.

The hard blockers are the absent Android signing inputs and Play configuration, plus an
unresolved Gemini duration/metering contract: the active plan requests >30-minute continuation,
while FR-V08 caps a logical session at 10 minutes, the broker cannot renew the same session
identity, and production does not accrue the usage counters its spend gate reads. Objective
holds also remain for the Android legacy-device settings-sync retest, persisted
`openai-realtime-mini` attribution, the remaining Gemini E1 parity checks, and physical PWA
install/offline verification. The code/deploy hold and real within-token Gemini recycle are now
closed.
Final residual-risk
acceptance belongs to the owner, but acceptance cannot relabel a failed or unrun objective test
as passing.

## Gate matrix

| Gate | Evidence as of 2026-07-26 | Status | Completion authority |
|---|---|---:|---|
| Current code, CI, and production deploy | Acceptance `/healthz` served `0.7.0+c0fa76e`. Deploy run `30215330844` is green through Go tests, OIDC/SAM deploy, health smoke, Playwright/axe, and Lighthouse. The only runtime follow-up after that canary throttles duplicate lifecycle diagnostics. The local web suite is 71 passed/21 intentionally skipped out of 92 project executions; Go build/vet/test and SAM lint pass. The last Android-affecting revision `100dddc` has green Deploy and Android Release runs, including 249 JVM tests and debug assembly. | **GO** | Objective |
| Web/default OpenAI core | Real production voice, tools, memory search, cost persistence, authenticated surfaces, and prior barge-in/persona checks passed. | **GO** | Objective |
| OpenAI mini routing | The dedicated `gpt-realtime-mini` minter is deployed and a real mini-pinned response passed. Request/response/ledger/log attribution is regression-tested; one persisted production mini attribution/cost proof remains. | **HOLD (attribution)** | Objective |
| Web wake-word hot-swap | Hands-free stayed live while `"hey automatica"` changed to `"hey live ninja"`; speaker-driven wake/turn passed. The owner setting was restored and `"hey automatica"` passed again. | **GO** | Objective |
| Gemini basic production path | Auth-token mint, audio, tool invocation, topics extraction, `engine`/`surface` tags, Gemini-rate cost persistence, and transcript sequencing pass in production. The latest canary persisted three turns at `~$0.027`. | **GO** | Objective |
| Gemini E1 parity | Cost/tool/transcript basics passed. Paired-engine comparison, memory extraction evidence, barge-in, persona voice mapping, and user voice override remain. | **HOLD** | Objective |
| Gemini E2 lifecycle | The first genuine canary dropped at about 6:21 without pre-deployed lifecycle evidence, so no root cause is assigned. A repeat canary on the corrected provisioning mask and instrumented client received `goAway` at nine minutes with a safe handle, resumed on the same token in about 1.15 seconds, answered a post-resume turn, stayed live past ten minutes, and produced no second web mint. >30 minutes remains blocked on the 10-minute-cap policy conflict, missing same-session renewal, and missing production usage accrual. | **GO (within-token) / BLOCKED (>30m)** | Owner policy, then objective implementation |
| Android voice and hardware | Locked-screen wake, M26 session controls, M27 volume/DND behavior, and M28 front/back photo, 60-second background/locked video, slow upload, and Files refresh passed. | **GO** | Objective |
| Camera2 sequential teardown | The process-lifetime callback-handler patch completed sequential back/front captures with zero dead-thread warnings; the 249-test JVM suite and debug assembly pass. | **GO (local/device)** | Objective |
| M31 device-scoped settings | Legacy `null` collections are normalized in the deployed route and the authenticated web client now loads those rows. Android's production sync retest is still blocked by the owner-secured tablet. | **HOLD (Android retest)** | Owner unlock, then objective test |
| Physical PWA install/offline | HTML is network-first and no-cache. A real stale Chrome profile exposed logical-module cache reuse; cache v4 now reload-backed revalidates only logical JavaScript modules while preserving SWR/offline and immutable model/vendor caching. The same profile upgraded through ordinary navigations without a hard refresh. Physical install/add-to-home/offline remains blocked by the owner's PIN. | **BLOCKED** | Owner unlock, then objective test |
| Signed Android sideload | `ANDROID_RELEASE_KEYSTORE_B64`, `ANDROID_RELEASE_STORE_PASSWORD`, `ANDROID_RELEASE_KEY_ALIAS`, and `ANDROID_RELEASE_KEY_PASSWORD` are absent. The latest-release and asset-links routes return 404 as designed. | **BLOCKED** | Owner inputs, then CI/objective verification |
| Google Play | Play App Signing, listing, AAB upload, data-safety/background-mic/camera declarations, and `ANDROID_PLAY_APP_SIGNING_SHA256` are incomplete. | **BLOCKED** | Owner console work, then objective verification |
| Security, SES, cost, and data access | OIDC-only deploy, SES production/DKIM, AWS budgets, mint-rate/concurrency controls, auth review, and Query/GetItem-only serving paths have evidence. Per-user daily/monthly spend enforcement does not: the gate reads `daySeconds`/`dayTokens`, but production only records `dayMints`, so those counters remain zero. | **HOLD** | Objective |
| Final residual-risk sign-off | Objective review is complete; owner sign-off has not occurred. | **PENDING** | Owner only |

## Risk-table reconciliation

### Archived execution risks

This review uses [archive/plan.md](../archive/plan.md) §7 while preserving that file as history.

| Archived risk | Current disposition |
|---|---|
| #1 critical-path platform, #2 contract drift, #4 auth | Mitigated by the deployed platform, shared contracts, auth/security review, and green deployed gates through `c0fa76e`. |
| #3/#6 M5Stack transport/schedule and #12 ten-year devices | **Deferred, not mitigated.** The 2026-07-24 scope decision moved M5Stack firmware, OTA, provisioning, and long-lived-device work to backlog. |
| #5 production-only/no staging | Residual operating posture. Main-only OIDC deploy, tests, health checks, additive contracts, and runbook reduce risk. The owner decided on 2026-07-19 to keep CloudWatch alarms removed; budgets email directly. |
| #7 wake-word training | Mitigated: training, SHA-pinned models, Android detection, and web/Android hot-swap are verified. |
| #8 model routing and #11 subagent execution | Delivery-process risks, not product launch gates; the standing routing and collaboration rules remain in force. |
| #9 settings conflicts | Contract and 409 tests exist; the deployed legacy-null normalization passes on web, but the Android runtime gate remains open until the owner-unlocked retest. |
| #10 DynamoDB Scan | Mitigated by Query/GetItem-only design, tests, and the recorded flat-read load probe. |

### PRD launch risks

This review uses [PRD.md](../PRD.md) §12.3.

| PRD risk group | Current disposition |
|---|---|
| Provider-key leakage, session-token theft, auth substitution | Mitigated by broker-only credentials, constrained ephemeral tokens, KMS signing, rotating refresh, cookies/session binding, and security tests. |
| DynamoDB Scan/cost blowout | Mitigated as above; no serving-path Scan is accepted. |
| Realtime media relayed through AWS | Mitigated for enabled direct OpenAI/Gemini paths. Nova remains disabled. |
| Engine/model routing drift | OpenAI Realtime is verified. The dedicated mini minter is deployed and a real pinned response passed; persisted production mini attribution remains. |
| SES mail drop | Mitigated: DKIM production identity, bounce/complaint handling, and owner-confirmed delivery. |
| Runaway provider cost | AWS budgets and mint-rate/concurrency controls are deployed. **Not mitigated at the per-user usage layer:** daily/monthly quota reads exist, but actual seconds/tokens are not accrued into them. |
| Android OEM assistant behavior and Play rejection | Hardware wake does not depend on the assistant role, but signed-release installation, role-flow smoke, Play policies, and disclosures cannot close before distribution exists. |
| Stale HTML/module graph | Network-first HTML, `no-cache`, stale-while-revalidate assets, cache-v4 cleanup, and reload-backed revalidation for logical JavaScript modules mitigate stale deploys. An already-stale Chrome profile upgraded with ordinary navigation; physical install/offline acceptance remains open. |
| Cross-device settings conflicts | The legacy-row normalization is deployed and passes on web; Android still needs the owner-unlocked production retest. |
| OpenAI outage/429 | Existing fallback/backoff paths and prior failure fallback evidence reduce risk; no new defect was found in this pass. |
| No staging | Same recorded residual as archived risk #5. |
| ESP32 capability, long-lived M5 credentials, bad OTA, field-lifetime API drift | Deferred with the M5Stack scope. The additive `/v1` commitment remains documented, but deferred hardware risks are not marked mitigated. |

## Owner-only actions and acceptances

- Unlock the physical tablet for the PWA test. No PIN or other credential should be given to an
  agent.
- Supply the four Android signing secrets only through `scripts/set-secret.sh` or
  `scripts\set-secret.bat`; never paste their values into chat or logs.
- Complete Play App Signing, listing, AAB upload, data-safety/background-microphone/camera
  declarations, then set the public signing-certificate SHA-256 repository variable.
- Rotate the QA password exposed in the 2026-07-18 transcript. Adding the QA address to the
  allowlist is a separate optional owner decision.
- Resolve the duration-policy conflict: retain FR-V08's 10-minute logical-session cap, or approve
  redefining it as a renewable authorization lease so the active plan may require >30-minute
  conversations. Implementation and testing follow that decision.
- Give final residual-risk sign-off after the objective holds close. Existing recorded decisions
  are: production-only/no staging, no CloudWatch alarms, M5Stack out of active scope, and a voice
  camera command serving as capture confirmation.

## Objective work required to reach GO

1. Reconcile FR-V08 with the active plan, add an authenticated same-session renewal flow if
   >30-minute conversations remain in scope, and implement idempotent production accrual for
   daily seconds/tokens before exercising the real spend gate.
2. Finish the remaining Gemini E1 voice/parity checks.
3. Persist and inspect one real `openai-realtime-mini` row/log to close model and cost/accounting
   attribution beyond the already-passing live response.
4. Re-run Android settings sync against the deployed legacy-row normalization and confirm the banner is
   gone without losing local or remote settings.
5. After owner unlock, install the PWA and prove add-to-home plus real offline navigation.
6. After owner signing setup, publish and install the signed APK, verify its signer,
   `/v1/app/android/latest`, `assetlinks.json`, app links, update behavior, and production voice
   smoke.
7. Complete Play Console publication evidence and obtain owner residual-risk sign-off.
