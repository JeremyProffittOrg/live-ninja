# Live Ninja — Operations Runbook

> Production-only stack in `759775734231` / `us-east-1`, domain `live.jeremy.ninja`.
> Owner: jeremy. Infrastructure/app deploys run through GitHub Actions from `main`; signed
> Android publishing is a manual Actions dispatch — **never** deploy from a local machine.
> This runbook is the M8 launch artifact: what to do when something breaks.

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
| Budgets | AWS Budgets $20/$50/$100 on `Project=live-ninja` | Direct budget email to the owner |
| SES bounces/complaints | OpsTopic | **SNS email subscription must be confirmed (owner click)** |
| Lambda logs | `/live-ninja/lambda/<fn>` (custom names, NOT `/aws/lambda/*`) | 5-day retention, `LOG_LEVEL=debug` |
| ECS bridge logs | `/live-ninja/ecs/nova-bridge` | |
| Request tracing | `X-LN-Txn` response header ↔ `txId` in logs | Canonical error envelope `{error:{code,message,txId}}` |
| Service metrics | CloudWatch Metrics / Logs Insights | No stack-managed dashboard; use Lambda, DynamoDB, API Gateway, and `LiveNinja/*` namespaces |
| Telemetry lake | Athena over `live-ninja-analytics-*` | Event schema only, no transcript content |

CloudWatch *alarm resources* were intentionally removed (owner request, 2026-07-18).
Budgets and SES event mail are the only automated paging paths. The following is the
alarm/signal-to-action map; rows marked “manual” require an operator to notice the signal.

| Alert or observed signal | First check | Immediate action | Recovery proof |
|---|---|---|---|
| $20 budget email | Cost Explorer, group by service and usage type | Identify the growing service; pause nonessential Batch/Nova work | Daily forecast flattens |
| $50 budget email | Same, plus DynamoDB consumed reads and provider usage | Apply the matching kill switch in §5; do not wait for $100 | Hourly spend returns to baseline |
| $100 budget email | Billing console and current GitHub deploy state | Disable the offending subsystem immediately; preserve logs before rollback | No new billable usage for one hour |
| SES bounce/complaint email | Email-dispatch logs and the SES event | Stop retrying the bad recipient; inspect template/recipient before resuming | A controlled message reaches a verified address |
| Deploy workflow failure | Failed step and CloudFormation stack events | Revert the bad commit and push; never run a local deploy | Revert run is green and `/healthz` is 200 |
| Lambda/API error spike (manual) | Correlate `X-LN-Txn` with the relevant log group | Roll back if release-correlated; otherwise disable only the affected provider/subsystem | Error rate and a representative request recover |
| DynamoDB read spike (manual) | `ConsumedReadCapacityUnits`, then logs for the hot route | Disable the caller or roll back; a serving-path `Scan` is a defect | Reads return to normal with no throttles |
| Nova task churn/5xx (manual) | ECS service events and `/live-ninja/ecs/nova-bridge` | Set `NovaBridgeEnable=false` and push (§1) | Core web/OpenAI/Gemini paths stay healthy |

## 3. Cost runaways

1. Cost Explorer → group by `USAGE_TYPE`, filter tag `Project=live-ninja`. DynamoDB blowups
   are almost always `ReadRequestUnits`.
2. Per-table read graph: CloudWatch `ConsumedReadCapacityUnits`. Flat-but-high = a hot
   serving path; a `Scan` on a serving path is a bug by definition here (Query/GetItem only).
3. OpenAI spend: quota gate is pre-spend (`USAGE#<month>` + daily counters; mint token
   bucket capacity 6 / refill 1 per 3s). Hourly-burn anomaly auto-suspends a user
   (`status=suspended`, SES alert) — after review, reinstate with the approved
   `store.ReinstateUser` operation (status returns to `active`; do not clear it).
4. Wake-word training: Batch capped conc≤2, 20-min timeout, 3/day/user (`WWTRAIN#<day>`
   counter under the USER partition — deleting that item is the admin quota reset).

## 4. Credential rotation

Secrets live in GitHub Actions and are re-put into SSM by the manual `Deploy` workflow.
Agents/operators never read, paste, echo, or pass secret values on a command line.

1. Create the replacement at the provider while the old credential still works, when the
   provider supports overlap.
2. Put it into GitHub through the hidden prompt:
   `./scripts/set-secret.sh OPENAI_API_KEY`,
   `./scripts/set-secret.sh GEMINI_API_KEY`,
   `./scripts/set-secret.sh GEMINI_SERVICE_ACCOUNT_JSON --file <path>`, or
   `./scripts/set-secret.sh LWA_CLIENT_SECRET`. `LWA_CLIENT_ID` is a non-secret GitHub
   variable, not an SSM SecureString.
3. Re-put SSM without an empty/no-op commit by dispatching the existing deployment path:
   `gh workflow run deploy.yml --ref main`. Get the run id with
   `gh run list --workflow deploy.yml --limit 1`, then run
   `gh run watch <id> --exit-status`.
4. Verify metadata only—never request a decrypted value. In SSM, confirm the expected
   parameter's `Version` and `LastModifiedDate` advanced. The workflow maps the GitHub names
   to `/live-ninja/prod/openai/api_key`, `/live-ninja/prod/gemini/api_key`,
   `/live-ninja/prod/gemini/service_account_json`, and
   `/live-ninja/prod/lwa/client_secret`.
5. Allow five minutes for the in-process SSM cache to expire, exercise the affected provider,
   then revoke the old credential at the source. If the check fails, restore the previous
   GitHub secret through the same hidden prompt and dispatch `Deploy` again.
6. `cred_pepper` is create-once and is deliberately not overwritten by deploy; rotating it
   invalidates device credentials and requires a separate migration plan.
7. KMS keys (`alias/live-ninja-auth`, `alias/live-ninja-jwt`) do not rotate manually; the
   JWT key is non-extractable — compromise response is a new CMK + template change.

## 5. Kill switches

| Scope | Action |
|---|---|
| One user's sessions | "Log out everywhere" (sets `tokensValidAfter=now`; authorizer enforces ≤60s) |
| One paired Tab5 device | Authenticated `DELETE /api/v1/devices/{id}` — revokes its refresh family and marks the device `revoked`; confirm a refresh and device-control request are denied |
| One lost Android install | Android installs are not independently addressable device rows; use "Log out everywhere" to invalidate its session along with the user's other sessions |
| Suspected stolen Tab5 certificate | After the device revoke, deactivate that certificate in AWS IoT Core and detach it from the Thing. The API does **not** perform certificate lifecycle operations |
| A user entirely | Set `status=suspended` on the USER item (denied at broker + authorizer) |
| All provider minting | Remove the realtime broker's provider-parameter `ssm:GetParameter` grants in `template.yaml` and push; mint fails closed. Restore the grants and push to recover. Never blank an SSM credential |
| Nova subsystem | `NovaBridgeEnable=false` + push (§1) |
| Access control | Allowlist is owner-managed (`CONFIG/ALLOW#`); removing an entry blocks new sign-ins immediately |

For the one-device switch, record the device id and certificate id before acting. The API
revocation is the fast application-layer cut: new access tokens, refreshes, reconnects, and
device-control ownership checks fail. Certificate deactivation is the transport-layer cut
for a lost or compromised Tab5 and terminates its ability to authenticate to IoT. Do not
delete the device row first; it contains the evidence needed to identify the Thing/certificate.

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

Field devices live about ten years, so every documented device-facing `/v1` and `/api/v1`
contract remains usable for that lifetime unless a security issue makes continued operation
unsafe. Within v1:

- Existing routes, required fields, field types, enum meanings, authentication requirements,
  and successful-response semantics are not removed or silently redefined.
- Changes are additive: new routes, optional request fields with old-behavior defaults, and
  optional response fields. Clients must ignore response fields and enum values they do not
  understand.
- A genuinely breaking redesign gets a new API version while v1 keeps an adapter. A v1 route
  may be retired only after all registered field devices have migrated or an explicit
  security incident is documented with a recovery/update path.
- `X-LN-Client`, `X-LN-Server`, and public `GET /v1/compat` advertise support. A client below
  the minimum receives an explicit 426/update message, never a changed payload masquerading
  as success.
- Bootstrap and recovery remain reachable to obsolete clients: auth/pairing,
  `GET /v1/compat`, and `GET /v1/app/android/latest` are exempt from the version gate.
- Wake models and signed APKs are content-addressed by SHA-256. Every platform keeps a safe
  bundled wake-model fallback, and Android release metadata is updated only after the APK
  signature and object upload have succeeded.

## 9. Android signed release

The `Android Release` workflow is manual-only and always builds `assembleRelease` plus
`bundleRelease`; callers cannot substitute a debug/arbitrary Gradle task. Four owner-managed
GitHub secrets must
exist: `ANDROID_RELEASE_KEYSTORE_B64`, `ANDROID_RELEASE_STORE_PASSWORD`,
`ANDROID_RELEASE_KEY_ALIAS`, and `ANDROID_RELEASE_KEY_PASSWORD`. Set them only through the
sanctioned hidden-prompt/file flow in `scripts/set-secret.sh` / `scripts\set-secret.bat`.
The first value is a base64 encoding of the owner-held release keystore, not the binary file
itself. Never print the encoding or add the keystore to the repository.

1. Confirm names only with `gh secret list`; do not request values.
2. Dispatch with `gh workflow run android-release.yml --ref main`, then watch the run with
   `gh run watch <id> --exit-status`.
3. CI builds both the signed APK and a Play Console AAB, verifies the APK signature, and
   uploads both in a 30-day Actions artifact. It then publishes the immutable APK, Digital
   Asset Links document, and finally the `android-latest.json` pointer. A failed upload cannot
   advance `/latest`.
4. Verify unauthenticated `GET /v1/app/android/latest` and
   `GET /.well-known/assetlinks.json` return 200 with the advertised version/fingerprint.
   Download the APK URL and confirm its SHA-256 equals the metadata before installing it.
5. The previously distributed v0.2.1-hal APK was debug-signed. Before the first owner-signed
   release, compare signer fingerprints with `apksigner verify --print-certs`. If they differ
   (the expected case), Android cannot update in place: back up anything needed, uninstall the
   debug build, and install the release APK, accepting that local app data is removed. Every
   later release must update in place with the same owner release signer.
6. Launch the app on a physical arm64 device and run sign-in/realtime smoke checks.

The Google Play listing, Play App Signing enrollment, AAB upload, and data-safety declaration
remain owner/Play Console work. After enrollment, set the public repository variable
`ANDROID_PLAY_APP_SIGNING_SHA256` to Play's app-signing certificate SHA-256 fingerprint before
the next manual release. The workflow then includes both the owner sideload certificate and
the Play certificate in `assetlinks.json`; without that second fingerprint, the Play-installed
app cannot claim the verified HTTPS App Link.
