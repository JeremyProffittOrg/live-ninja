# Live Ninja — Operations Runbook

> Production-only stack in `759775734231` / `us-east-1`, domain `live.jeremy.ninja`.
> Owner: jeremy. Everything deploys via GitHub Actions on push to `main` — **never** from a
> local machine. This runbook is the M8 launch artifact: what to do when something breaks.

## 1. Deploy & rollback

- **Deploy** = push/merge to `main`. Runs serialize on the `deploy-main` concurrency group.
  Monitor with `gh run watch <id>` (summarize; details only on failure).
- **Rollback** = `git revert` the offending commit (or reset to the last good SHA on a
  branchless emergency) and push. CloudFormation rolls back failed changesets automatically;
  a wedged `ROLLBACK_COMPLETE` on the *first* create of a resource means delete the stack
  remnant, not the whole stack — see the M0 notes in plan.md.
- **Nova bridge special case:** the ECS service rolls onto a new image only via the
  workflow's roll step. To gate the whole Nova subsystem off in an emergency, set
  `NovaBridgeEnable=false` in `.github/workflows/deploy.yml` and push — that removes the
  ECS/ALB resources cleanly (CloudFront keeps the inert `/nova/*` behavior).
- **Firmware:** flash from this PC only (see §6); fleet OTA via signed IoT Jobs, canary
  first, `mark-valid-after-check-in` guards a bad image.

## 2. Monitoring & where to look

| Signal | Where | Notes |
|---|---|---|
| Budgets | AWS Budgets $20/$50/$100 on `Project=live-ninja` | Email via OpsTopic (SNS) |
| SES bounces/complaints | OpsTopic | **SNS email subscription must be confirmed (owner click)** |
| Lambda logs | `/live-ninja/lambda/<fn>` (custom names, NOT `/aws/lambda/*`) | 5-day retention, `LOG_LEVEL=debug` |
| ECS bridge logs | `/live-ninja/ecs/nova-bridge` | |
| Request tracing | `X-LN-Txn` response header ↔ `txId` in logs | Canonical error envelope `{error:{code,message,txId}}` |
| Dashboard | CloudWatch `live-ninja-ops` | |
| Telemetry lake | Athena over `live-ninja-analytics-*` | Event schema only, no transcript content |

CloudWatch *alarms* were intentionally removed (owner request, 2026-07-18). Budgets and SES
event mail remain the paging path; check the dashboard manually during incidents.

## 3. Cost runaways

1. Cost Explorer → group by `USAGE_TYPE`, filter tag `Project=live-ninja`. DynamoDB blowups
   are almost always `ReadRequestUnits`.
2. Per-table read graph: CloudWatch `ConsumedReadCapacityUnits`. Flat-but-high = a hot
   serving path; a `Scan` on a serving path is a bug by definition here (Query/GetItem only).
3. OpenAI spend: quota gate is pre-spend (`USAGE#<month>` + daily counters; mint token
   bucket capacity 6 / refill 1 per 3s). Hourly-burn anomaly auto-suspends a user
   (`status=suspended`, SES alert) — un-suspend by clearing the status field on the USER item.
4. Wake-word training: Batch capped conc≤2, 20-min timeout, 3/day/user (`WWTRAIN#<day>`
   counter under the USER partition — deleting that item is the admin quota reset).

## 4. Credential rotation

Secrets live in SSM SecureString, synced from GitHub secrets by the deploy workflow.
Agents/operators never handle values directly.

1. Rotate at the source (OpenAI dashboard / LWA console).
2. Update the GitHub secret: `scripts/set-secret.sh` (or repo Settings → Secrets):
   `OPENAI_API_KEY`, `LWA_CLIENT_ID`, `LWA_CLIENT_SECRET`.
3. Push any commit (or re-run the last deploy) — the workflow re-puts SSM params.
4. Broker/config caches SSM for 5 min; no restart needed.
5. KMS keys (`alias/live-ninja-auth`, `alias/live-ninja-jwt`) do not rotate manually; the
   JWT key is non-extractable — compromise response is a new CMK + template change.

## 5. Kill switches

| Scope | Action |
|---|---|
| One user's sessions | "Log out everywhere" (sets `tokensValidAfter=now`; authorizer enforces ≤60s) |
| One device | `DELETE /api/v1/devices/{id}` — revokes the 10-yr refresh family + detaches/deactivates the IoT cert |
| A user entirely | Set `status=suspended` on the USER item (denied at broker + authorizer) |
| All voice minting | Emergency: remove the broker's `ssm:GetParameter` grant or blank the SSM key (mint fails closed, text fallback keeps working until it needs OpenAI too) |
| Nova subsystem | `NovaBridgeEnable=false` + push (§1) |
| Access control | Allowlist is owner-managed (`CONFIG/ALLOW#`); removing an entry blocks new sign-ins immediately |

## 6. Device (Tab5) ops

- Bench device: COM58, MAC `30:ED:A0:E3:01:1E` (fleet registry `c:\dev\fleet\esp32.md`).
- Flash recipe (git-bash breaks `export.bat`): a `.bat` with `set "MSYSTEM="` →
  `set IDF_PYTHON_ENV_PATH=%USERPROFILE%\.espressif\python_env\idf5.4_py3.13_env` →
  `call C:\esp\esp-idf-v5.4.4\export.bat` → `idf.py -p COM58 flash`.
- Serial console: python/pyserial COM58 115200 — open only after esptool's reset releases
  the port.
- Pairing security: RFC 8628 user code shown on the LCD/portal; 5 wrong browser entries
  invalidate the pairing and the device restarts with a fresh code.

## 7. Common incidents (seen in prod, with fixes landed)

- **Mint 429 "concurrent session limit"**: leaked `BUCKET#sess#` slots (10-min TTL). Fixed
  by final-flush slot release; if it recurs, delete the stale slot items for the user.
- **UI changes "not showing"**: stable-URL JS modules are SW-cached stale-while-revalidate —
  reload **twice**. HTML itself is network-first (one reload).
- **Wake-word model won't load**: check manifest fetch is authFetch (JWT), bucket CORS for
  `live.jeremy.ninja`, and the `WAKEWORD#` item status + Batch job log (`/aws/batch/job`,
  queue `live-ninja-wakeword-train`).
- **`aws logs` in git-bash mangles paths**: prefix `MSYS_NO_PATHCONV=1`.
- **SES mail vanishing**: always send From `jeremy@jeremy.ninja` (DKIM), Reply-To the gmail;
  never From the gmail (DMARC drop).

## 8. `/v1` compatibility commitment

Field devices live ~10 years. `/v1` routes are additive-only for the device-facing surface
(pairing, session mint, tools invoke, shadow/settings, wake-word manifest). Capability
negotiation via `X-LN-Client` / `X-LN-Server` + `GET /v1/compat`; below-minimum clients get
explicit "please update" responses, never silent breakage. Wake models are content-addressed
(SHA-256) with a safe bundled fallback on every platform.
