# Azure Migration Plan — live-ninja

**Branch:** `alexa-version` (created 2026-08-18 from `main` @ `a1e30eb`, pushed to `origin/alexa-version`).
**Governs:** the full migration of Live Ninja from AWS account `759775734231` to Azure, including the
web app, the Android app, and the ESP32/M5Stack firmware.
**Does not govern:** the AWS product work still tracked in `/c/dev/live-ninja/plan.md` (the wake-word
training workstream §7.4 is mid-flight there and is untouched by this plan).

Written for a cold reader with no session context. Every path is absolute. Every milestone's
definition of done is a command with a pass/fail result.

**Status markers:** `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked.

---

## Locked decisions (user-confirmed 2026-08-18; do not revisit)

1. **Nova Sonic is replaced by Azure Voice Live.** The `nova-sonic` engine,
   `/c/dev/live-ninja/cmd/nova-bridge`, the Bedrock `InvokeModelWithBidirectionalStream` path, the
   ECS/Fargate cluster, the ALB, and the `BatchVpc` are all retired. In their place a new
   `azure-voice-live` engine is served by a new bridge service on Azure Container Apps speaking the
   Voice Live WebSocket API. The bridge keeps the shape of `cmd/nova-bridge` (session-slot
   redemption, JWT verify, event normalisation) and replaces only the upstream protocol.
2. **Login with Amazon is kept.** `/c/dev/live-ninja/internal/auth/lwa.go` moves unchanged. The Amazon
   `user_id` remains the primary key across the datastore. No user re-registers. The only identity
   work is moving JWT signing from AWS KMS to Azure Key Vault.
3. **IoT migrates to Azure IoT Hub.** Device twins replace AWS IoT device shadows. X.509 via Device
   Provisioning Service replaces fleet-provisioning-by-claim and the custom authorizer.
   `/c/dev/live-ninja/cmd/iot-authorizer` is deleted. Every fielded device requires a firmware reflash.

   > **`[!]` VERIFIED CONTRADICTION, recorded 2026-08-19 — the decision itself is NOT revisited here;
   > this records a false premise inside it for the operator to rule on.** The premise "X.509 replaces
   > the custom authorizer" does not hold, because `cmd/iot-authorizer` does not serve devices.
   > `cmd/iot-authorizer/main.go:1-2` states it is "the AWS IoT Core custom authorizer fronting MQTT
   > over WebSockets for **the web and Android clients**", and
   > `android/.../realtime/LiveEventsClient.kt:318` builds
   > `wss://${c.endpoint}/mqtt?x-amz-customauthorizer-name=${c.authorizerName}`. Devices authenticate
   > with X.509 and never touch it. IoT Hub's MQTT admits only fixed `devices/{id}/...` topics with
   > per-device credentials, so it has no counterpart for a per-user `liveninja/user/<uid>/#` subtree
   > gated by a first-party JWT. Deleting the authorizer therefore removes live events, presence, and
   > the turn-taking lock for **every web and Android client**, which is a separate migration
   > (Azure Web PubSub or SignalR) that no milestone currently owns. See WS-G.
4. **Azure target is the existing org tenant and subscription.**
   - Tenant ID: `d0695ba8-1211-4da6-81a4-05427c842a2a`
   - Subscription ID: `adc40fff-bab3-4bd2-b961-1832d0375052`
   - A **new** federated credential is created for `JeremyProffittOrg/live-ninja` on
     `refs/heads/alexa-version`. The `JeremyProffittOrg/event` credential
     (`f16364f7-e9d4-4b28-95aa-7b11e2fe8ea7`) is **not** reused or broadened.
5. **Voice engines after migration — six, not four:**

   | Engine constant | Model | Transport | Status |
   |---|---|---|---|
   | `openai-realtime` | `gpt-realtime` | client-direct WebRTC to `api.openai.com` | KEPT unchanged |
   | `openai-realtime-mini` | `gpt-realtime-mini` | client-direct WebRTC to `api.openai.com` | KEPT unchanged |
   | `gemini-flash-live` | `gemini-3.1-flash-live-preview` | client-direct WSS to Google | KEPT unchanged |
   | `azure-openai-realtime` | `gpt-realtime-2.1` (fallback `gpt-realtime-2`) | client-direct WebRTC to Azure | **NEW** |
   | `azure-openai-realtime-mini` | `gpt-realtime-2.1-mini` | client-direct WebRTC to Azure | **NEW** |
   | `azure-voice-live` | `gpt-realtime` via Voice Live | bridged WSS through Container Apps | **NEW**, replaces `nova-sonic` |

6. **60 minutes a day is a planning figure, not an enforced ceiling** (operator, 2026-08-19, verbatim:
   "60 minutes should not be a hard cap"). Chosen handling: **measure and soft-warn, never block.**
   - Write `daySeconds` and `dayTokens` at session close so spend is visible, `usage-rollup` stops
     summing zeros, and the WS-B M6 cache-ratio signal has a server-side source.
   - Surface an in-product warning past the threshold. **Never return 402 on daily minutes.**
   - The controls that are real today stay exactly as they are: `QUOTA_SESSION_CAP_SECONDS=600`,
     3 concurrent sessions, and the mint bucket (`internal/realtime/quota.go:57-58`).
   - **Accepted consequence:** with no daily ceiling, per-user spend is bounded only by session
     concurrency, so WS-A M8's budgets are the only backstop. They are sized for that in decision 10.

7. **Route 53 stays authoritative and the run may write two record sets there** (operator,
   2026-08-19, verbatim: "you'll need to update dns"). This is a narrow exception to the blanket
   prohibition on modifying AWS account `759775734231`, and it is the **only** AWS write this plan
   authorizes. It covers the `live.jeremy.ninja` and `azure.live.jeremy.ninja` records in the
   `jeremy.ninja` hosted zone and nothing else. No domain is registered in Azure and no Azure DNS
   zone is created. See WS-D M5 and WS-J M3.

8. **Client live events migrate to Azure Web PubSub** (operator, 2026-08-19). `cmd/iot-authorizer`
   fronts MQTT over WebSockets for the web and Android clients, not for devices
   (`cmd/iot-authorizer/main.go:1-2`), so IoT Hub's X.509 is not its replacement and deleting it
   would remove live events, presence, and the turn-taking lock for every client. A new WS-G
   milestone ports the client fan-out to Azure Web PubSub with a server-minted per-user token. This
   supersedes the premise inside decision 3; decision 3 itself is otherwise unchanged.

9. **`alexa-version` merges to `main` once WS-J is green** (operator, 2026-08-19). Until then,
   pushing to `main` remains a stop condition. WS-I M6 (Android release) and WS-K M4 both require
   `main` and therefore both wait for the merge rather than being worked around.

10. **Cost Management budgets are $100 / $250 / $500** (operator, 2026-08-19), replacing the
    $20 / $50 / $100 the plan first carried, which all sat below the plan's own $90-$120 monthly
    baseline and would have fired every normal month. See WS-A M8 for scope, filter, and recipients.

---

## Verified facts (confirmed by reading the repo on 2026-08-18)

Each line names the file or command that established it. Anything not in this section is an
assumption and is marked as such where it appears.

**Scale**
- 123,819 lines of Go across the repo (`find . -name '*.go' -not -path './archive/*' | xargs wc -l`).
- 14 programs under `/c/dev/live-ninja/cmd/`, 8,954 lines total. Largest: `nova-bridge` 2,543
  (being deleted), `realtime-broker` 1,580, `account-purge` 999.
- 170 Kotlin files under `/c/dev/live-ninja/android/app/src/`.
- `/c/dev/live-ninja/template.yaml` is 2,970 lines and declares roughly 120 resources.

**Data layer — smaller than it looks**
- `/c/dev/live-ninja/internal/store/` is 20 non-test files over one DynamoDB table (`pk`/`sk` +
  `GSI1` + `GSI2`, TTL on `ttl`, PAY_PER_REQUEST).
- **Exactly 5 secondary-index queries exist in the entire codebase**
  (`grep -rn 'IndexName' internal/store/*.go`):
  - `internal/store/users.go:31` — `GetUserByLWA` (GSI1)
  - `internal/store/sessions.go:80` — `GetSessionByID` (GSI1)
  - `internal/store/sessions.go:562` — `ListSessions` (GSI2)
  - `internal/store/devices.go:325` — `ListDevices` (GSI2)
  - `internal/store/deliverables.go:182` (GSI1)

  Everything else is a key `GetItem`/`PutItem` or a single-partition `Query`. There are no `Scan`
  calls on any serving path.

**CORRECTION 2026-08-19 — the sizing claim above was wrong and is withdrawn.** The original text read
"This makes the Cosmos DB port a 5-query problem, not a schema rewrite." Three commands disprove it:
- `grep -rl 'aws-sdk-go-v2/service/dynamodb' --include=*.go . | grep -v _test | grep -v '^./internal/store/' | grep -v testutil`
  returns **17 files** that build DynamoDB requests outside `internal/store`, each with its own private
  interface rather than the `ddbAPI` seam: `internal/realtime/{quota,mint,guides,personas_store,voiceprefs}.go`,
  `internal/codeupdate/store.go`, `internal/tools/{notes,registry}.go`, `internal/webapp/api_routes.go`,
  and `cmd/{account-purge,codeupdate-dispatch,deliverables-zipper,email-dispatch,nova-bridge,realtime-broker,usage-rollup,web}`.
  WS-C M2 scopes the port to `internal/store` only, so all 17 would still address a table WS-K deletes.
- The seam is **not** storage-neutral. `internal/store/store.go:31-40` is typed entirely in DynamoDB SDK
  structs, and the package carries 30 `UpdateExpression` and 85 `ConditionExpression` uses. "Satisfying
  the same seam" from Cosmos means parsing DynamoDB expression grammar.
- The seam declares **7** methods, not the "8 DynamoDB operations" WS-C M2 claims.

  The accurate statement is: **a 5-index-query problem, plus 17 out-of-package call sites, plus a seam
  that must be re-expressed in storage-neutral terms before Cosmos can satisfy it.**

**Quota is already externalised**
- `/c/dev/live-ninja/internal/realtime/quota.go:110-114` reads `QUOTA_DAILY_SECONDS`,
  `QUOTA_MONTH_TOKENS`, and `QUOTA_SESSION_CAP_SECONDS` from the environment.
- Defaults today: `defaultDailySecondsCap = 1800.0` (30 min/day), `defaultMonthlyTokenCap = 375000.0`
  (~$15/month), `defaultSessionCapSeconds = 600` (10 min).
- **CORRECTION 2026-08-19 — "Raising to 60 min/day is an environment-variable change, not a code
  change" is withdrawn. The caps are inert: nothing in production writes the counters they read.**
  The gates read `daySeconds` (`internal/realtime/quota.go:328`), `monthTokens` (`quota.go:750`) and
  `dayTokens` (`quota.go:644`). The only production writer is `internal/realtime/quota.go:942`,
  `UpdateExpression: "SET updatedAt = :ts ADD dayMints :one"` — mints only.
  `grep -rn 'AddDayUsage\|AddMonthUsage' --include=*.go .` returns the two definitions
  (`internal/store/usage.go:92`, `:102`) and exactly one caller, `usage.go:112`, which is
  `BumpDayMints` passing `0, 0, 1`. So `daySeconds` and `dayTokens` are always zero and neither the
  daily nor the monthly cap can ever trigger. `cmd/usage-rollup/main.go:192` sums a field that is
  always zero.
  This is a **pre-existing AWS defect, not something the migration introduces**, and it was already
  recorded: `docs/launch-go-no-go-2026-07-26.md:41` marks per-user spend enforcement **HOLD** with the
  same diagnosis. The live limits today are the mint bucket (`quota.go:57-58`, burst 6, one token per
  3s) and 3 concurrent 600-second sessions — roughly 3 x 24h of session time per day, not one hour.
  **Consequence for this plan, as resolved by locked decision 6:** the operator has ruled that 60
  minutes is a planning figure and must never hard-block, so the fix is *not* to build enforcement.
  WS-B M5a writes the counters for visibility only — so spend is measurable, `usage-rollup` stops
  summing zeros, and WS-B M6 gains a server-side data source — and WS-B M5 turns the threshold into an
  advisory warning. No 402 on daily minutes. The spend backstop is WS-A M8's budgets, which were
  resized for exactly that reason.

**Client endpoint surfaces are centralised**
- Android: every endpoint is a constant in
  `/c/dev/live-ninja/android/app/src/main/java/ninja/jeremy/liveninja/config/BackendConfig.kt`.
  `BASE_URL = "https://live.jeremy.ninja"`; `OPENAI_REALTIME_CALLS_URL` is the only non-first-party host.
  **Qualifier added 2026-08-19: "centralised" does not mean "server-steerable."** `callsUrl` is declared
  at `RealtimeSessionApi.kt:55` as `val callsUrl: String = BackendConfig.OPENAI_REALTIME_CALLS_URL` and
  is **never populated from the session JSON** — `grep -n 'callsUrl' RealtimeSessionApi.kt` returns only
  that declaration. The mode dispatch at `RealtimeSessionCoordinator.kt:204-221` has explicit branches
  for `nova-bridge` and `gemini-direct` and an `else ->` that selects the WebRTC transport. So an
  installed build handed `mode: "azure-openai-direct"` does not fail closed: it takes the `else` branch
  and POSTs the **Azure** ephemeral credential to the compile-time `https://api.openai.com` host. Any
  rollout of an `azure-*` mode must be gated server-side on the `X-LN-Client` version header
  (`contracts/headers.md:7-33`) before WS-I ships. This is why "the cutover needs no Android release"
  is true only for the hostname, not for the engine flip.
- Android transports live in `/c/dev/live-ninja/android/app/src/main/java/ninja/jeremy/liveninja/realtime/`:
  `WebRtcTransport.kt`, `GeminiLiveTransport.kt`, `NovaBridgeTransport.kt` (to be replaced).
- Web: `/c/dev/live-ninja/web/static/js/realtime.mjs` (2,435 lines) holds all three transports and
  branches on the `mode` field of `GET /api/v1/realtime/session`
  (`openai-direct` | `nova-bridge` | `gemini-direct`).
- Firmware: `/c/dev/live-ninja/firmware/components/ln_realtime/ln_realtime.c:64` hardcodes
  `wss://api.openai.com/v1/realtime?model=%s`; `ln_rt_session.c:18` handles the `nova-bridge` mode.
- Firmware IoT: `/c/dev/live-ninja/firmware/components/ln_iot/include/ln_iot.h:99` expects an AWS ATS
  endpoint (`xxxx-ats.iot.us-east-1.amazonaws.com`) and does mTLS plus fleet-provisioning-by-claim.

**DNS and TLS today (confirmed 2026-08-19)**
- `jeremy.ninja` is registered and its zone is authoritative on Route 53. `nslookup -type=NS
  jeremy.ninja 8.8.8.8` returns `ns-997.awsdns-60.net`, `ns-232.awsdns-29.com`,
  `ns-1344.awsdns-40.org`, `ns-2020.awsdns-60.co.uk`.
- `live.jeremy.ninja` is a record **inside** the `jeremy.ninja` zone, not a zone of its own
  (`nslookup -type=SOA live.jeremy.ninja 8.8.8.8` returns the `jeremy.ninja` SOA). It is a subdomain,
  not the apex, so a plain `CNAME` to Front Door is legal — no ALIAS/ANAME constraint applies.
- Today it is an A + AAAA ALIAS pair pointing at the CloudFront distribution
  (`/c/dev/live-ninja/template.yaml:2741-2759`), with the zone supplied as the `HostedZoneId` stack
  parameter (`template.yaml:17-19`).
- **No client pins TLS.** `grep -rniE 'certificatePinner|pinning|network_security_config|sha256/'
  /c/dev/live-ninja/android/app/src/main` returns nothing, and
  `/c/dev/live-ninja/android/app/src/main/AndroidManifest.xml` declares no `networkSecurityConfig`.
  The ACM-to-Front-Door certificate swap is therefore invisible to every fielded client.
- `BASE_URL` is a single constant at
  `/c/dev/live-ninja/android/app/src/main/java/ninja/jeremy/liveninja/config/BackendConfig.kt:9`, and
  every other Android endpoint is derived from it.

**Secrets and config**
- SSM parameters in use: `/live-ninja/prod/openai/api_key`, `/live-ninja/prod/gemini/api_key`,
  `/live-ninja/prod/lwa/client_id`, `/live-ninja/prod/lwa/client_secret`,
  `/live-ninja/prod/device/cred_pepper`.
- `/c/dev/live-ninja/internal/config/config.go` reads 12 environment variables: `AUTH_KMS_KEY_ID`,
  `DEVICE_CRED_PEPPER`, `DOMAIN_NAME`, `EMAIL_QUEUE_URL`, `GEMINI_API_KEY`, `JWT_KMS_KEY_ID`,
  `LOG_LEVEL`, `LWA_CLIENT_ID`, `LWA_CLIENT_SECRET`, `OPENAI_API_KEY`,
  `OPENAI_MONTHLY_BUDGET_USD`, `TABLE_NAME`.

**The web tier is already portable**
- `/c/dev/live-ninja/cmd/web/main.go` is a plain Fiber HTTP server behind the AWS Lambda Web Adapter.
  Its own header states there is "no Lambda SDK involved". It runs as an ordinary container with no
  code change.

**Contracts are additive-only**
- `/c/dev/live-ninja/contracts/README.md:27-30` — within `/v1`, enum members may be **added** at any
  time, but the 10-year device horizon means nothing may be removed. Therefore `nova-sonic` is
  **deprecated and aliased**, never deleted from
  `/c/dev/live-ninja/contracts/settings.schema.json#/properties/voiceEngine`.

**Azure endpoint shapes (from Microsoft Learn, read 2026-08-18)**
- Azure OpenAI Realtime mint: `https://<resource>.openai.azure.com/openai/v1/realtime/client_secrets`
- Azure OpenAI Realtime SDP: `https://<resource>.openai.azure.com/openai/v1/realtime/calls`
- Azure requires the GA `/openai/v1` path form with **no** `api-version` query parameter.
- Voice Live WebSocket: `wss://<resource>.services.ai.azure.com/voice-live/realtime?api-version=2026-04-10&model=gpt-realtime`
- Voice Live default quota: 100,000 tokens/minute per resource.
- Azure OpenAI Realtime max session duration: 60 minutes.

**Open verification item (NOT a verified fact)**
- Microsoft's pricing page carries live rates for `GPT-Realtime-2.1` Global and Data Zone, but
  `https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/realtime-audio` does **not** list
  2.1 among supported models (it lists `gpt-realtime-1.5` @ `2026-02-23` and `gpt-realtime-2` @
  `2026-05-07`). The same page still quotes 1.5's 32k/4k token limits. **WS-B M1 resolves this by
  attempting an actual deployment.** The plan falls back to `gpt-realtime-2` if 2.1 is unavailable.

---

## Standing authorizations (granted 2026-08-18)

Granted, no mid-run confirmation needed:
- Create, configure, and delete Azure resources inside subscription `adc40fff-bab3-4bd2-b961-1832d0375052`.
- Incur Azure spend for the resources named in WS-A through WS-G.
- Create one federated credential for `JeremyProffittOrg/live-ninja` on `refs/heads/alexa-version`.
- Set GitHub repository **variables** on `JeremyProffittOrg/live-ninja`.
- Push commits to `alexa-version` and let the Azure workflow deploy from it.
- Read AWS resources for migration purposes.
- **Create and modify the `live.jeremy.ninja` and `azure.live.jeremy.ninja` record sets in the
  `jeremy.ninja` Route 53 hosted zone in AWS account `759775734231`** (granted 2026-08-19, locked
  decision 7). WS-D M5 and WS-J M3 only. This is the sole AWS write this plan authorizes; every
  other resource in that account remains read-only.
- **Merge `alexa-version` to `main` once WS-J M4 has passed** (granted 2026-08-19, locked decision 9),
  which unblocks WS-I M6 and WS-K M4. Before WS-J M4 passes, pushing to `main` remains forbidden.

**NOT granted — these are stop conditions:**
- Deleting or modifying any AWS resource in account `759775734231` (WS-K), **other than the two
  Route 53 record sets named above**.
- Reprovisioning or deleting the existing M5Stack IoT Thing or its certificate (WS-G M4).
- Pushing anything to `main` **before WS-J M4 has passed** (see locked decision 9).
- Broadening the `JeremyProffittOrg/event` federated credential.

---

## Stop conditions (only these)

Anything not on this list is worked around, marked `[!]` with what unblocks it, and reported at the
end. The run does not pause for anything else.

1. Azure subscription `adc40fff-bab3-4bd2-b961-1832d0375052` is unreachable, or the identity lacks
   Contributor rights on it.
2. A secret value would have to pass through the conversation or a log. Use
   `/c/dev/live-ninja/scripts/set-secret.sh` and wait.
3. Any AWS deletion in WS-K.
4. Any action that would reprovision or delete the existing M5Stack Thing or certificate.
5. A required Azure service is unavailable in the chosen region with no substitute. Record the region
   and the service, then continue on other workstreams.

---

## Service mapping (AWS to Azure)

| AWS today | Azure replacement | Workstream |
|---|---|---|
| Lambda `WebFunction` + Lambda Web Adapter | Azure Container Apps (HTTP ingress) | WS-D |
| 12 background Lambdas | Azure Container Apps Jobs (event + cron) | WS-D |
| API Gateway HTTP API | Container Apps ingress | WS-D |
| CloudFront + ACM + Route53 | Azure Front Door Standard + managed cert. **Route 53 stays the authoritative zone — no Azure DNS, no domain registration in Azure.** | WS-D (D4, D5) |
| DynamoDB single table | Cosmos DB for NoSQL (serverless) | WS-C |
| S3 x 6 buckets | Azure Blob Storage containers | WS-E |
| KMS `AuthKey` + `JwtKey` | Azure Key Vault keys (sign) | WS-E |
| SQS x 3 + DLQ x 3 | Service Bus queues (native dead-lettering) | WS-E |
| SSM Parameter Store | Key Vault secrets + Container Apps secret refs | WS-A |
| Kinesis Firehose -> S3 -> Glue -> Athena | Event Hubs + Capture -> Blob -> Azure Data Explorer | WS-E |
| SES + configuration set | Azure Communication Services Email | WS-E |
| SNS `OpsTopic` | Azure Monitor action group | WS-E |
| AWS Budgets x 3 | Cost Management budgets | WS-A |
| EventBridge Scheduler | Container Apps Jobs cron | WS-E |
| AWS Batch + ECR (wakeword train) | Container Apps Job + Azure Container Registry | WS-E |
| ECS/Fargate + ALB + VPC (nova-bridge) | Container Apps (`voice-live-bridge`) | WS-F |
| IoT Core + custom authorizer Lambda | Azure IoT Hub + Device Provisioning Service (X.509) | WS-G |
| Bedrock `amazon.titan-embed-text-v2:0` | Azure OpenAI `text-embedding-3-small` @ 512 dims | WS-C |
| Bedrock (RCA analyzer) | Azure OpenAI `gpt-5.2` | WS-D |
| IAM roles | Managed identities | WS-A |
| CloudWatch Logs | Azure Monitor Log Analytics | WS-A |
| CloudFormation / SAM | Bicep | WS-A |
| GitHub OIDC -> AWS `gha-deploy` | GitHub OIDC -> Entra (`azure/login@v2`) | WS-A |

**Unchanged, no migration:** Login with Amazon, OpenAI Realtime (client-direct), Gemini Live
(client-direct), `internal/ghost` (keeps its AWS IAM invoke path to ghost-cli — see WS-K M3).

---

## Workstreams

Dependencies are annotated. WS-A blocks every deploy but blocks no code work, so WS-B and WS-C start
immediately and in parallel with it.

### WS-A — Azure foundation and CI/CD

*Blocks: every deploy step in every other workstream. Depends on: nothing.*

- [ ] **A1. Confirm subscription access.**
      DoD: `az account show --subscription adc40fff-bab3-4bd2-b961-1832d0375052 --query id -o tsv`
      prints the subscription id, exit 0.
- [ ] **A2. Create the resource group and Log Analytics workspace.** Region `eastus2`, chosen because
      it carries both the Azure Speech HD voices and the realtime models — confirm in A3. Names:
      `rg-liveninja-prod`, `log-liveninja-prod`.
      DoD: `az group show -n rg-liveninja-prod --query properties.provisioningState -o tsv` returns
      `Succeeded`.
- [ ] **A3. Confirm model availability in region.** Verify `gpt-realtime-2.1`, `gpt-realtime-2.1-mini`,
      and Voice Live are all deployable in the chosen region before anything is built on top.
      DoD: `az cognitiveservices account list-models -g rg-liveninja-prod -n <foundry-resource> --query "[?contains(name,'realtime')].name" -o tsv`
      lists at least one of `gpt-realtime-2.1` / `gpt-realtime-2`, and one mini variant.
      If neither is present in `eastus2`, re-run A2 and A3 in `swedencentral` and record the change here.
- [ ] **A4. Create the Entra app and federated credential.** Subject
      `repo:JeremyProffittOrg/live-ninja:ref:refs/heads/alexa-version`. Use the immutable-ID subject
      form if the org requires it, matching the `event` repo's pattern. Assign Contributor on
      `rg-liveninja-prod` only — never at subscription scope.
      DoD: `az ad app federated-credential list --id <new-app-id> --query "[].subject" -o tsv`
      contains the `live-ninja` subject and does NOT contain any `event` subject.
- [ ] **A5. Set GitHub repository variables** (variables, not secrets — these are identifiers):
      `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`, `AZURE_RESOURCE_GROUP`,
      `AZURE_REGION`, `AZURE_OPENAI_ENDPOINT`, `AZURE_FOUNDRY_ENDPOINT`.
      DoD: `gh variable list --repo JeremyProffittOrg/live-ninja | grep -c '^AZURE_'` returns 7 or more.
- [ ] **A6. Author the Bicep root.** `/c/dev/live-ninja/infra/main.bicep` plus one module per service.
      Apply the org stack standards: cost-allocation tags set once at deployment scope
      (`Project=live-ninja CostCenter=voice-ai Environment=prod ManagedBy=bicep DeployedVia=github-actions Owner=jeremy`),
      no third-party secrets manager beyond Key Vault, explicit retention on every log resource.
      DoD: `az deployment group what-if -g rg-liveninja-prod -f /c/dev/live-ninja/infra/main.bicep --no-pretty-print`
      exits 0 with no `Delete` operations listed.
- [ ] **A7. Author `/c/dev/live-ninja/.github/workflows/deploy-azure.yml`.** Triggers on
      `push: branches: [alexa-version]`. Uses `azure/login@v2` with
      `permissions: { id-token: write, contents: read }`. **No client secret, no static credential.**
      Mirrors `deploy.yml`'s `concurrency` group so two pushes serialise instead of racing.
      DoD: `gh workflow view deploy-azure.yml --repo JeremyProffittOrg/live-ninja` shows the workflow and
      `gh run list --workflow=deploy-azure.yml --branch alexa-version --limit 1 --json conclusion -q '.[0].conclusion'`
      returns `success`.
- [ ] **A8. Cost Management budgets** at **$100 / $250 / $500** (locked decision 10), replacing the
      three `AWS::Budgets::Budget` resources. The original $20 / $50 / $100 all sat below the plan's
      own $60-$90 standing-infrastructure estimate plus $30.32 default-engine usage, so every one of
      them would have fired in a normal month and none could have signalled a cache collapse.
      Because locked decision 6 removes any hard daily cap, **these budgets are the only spend
      backstop in the system** — size and wire them accordingly.
      - Scope each budget to `/subscriptions/adc40fff-bab3-4bd2-b961-1832d0375052/resourceGroups/rg-liveninja-prod`,
        **never subscription scope** — the subscription is shared with `JeremyProffittOrg/event`, so a
        subscription-scoped budget mixes in another project's spend and a subscription-scoped
        list-count check fails for the wrong reason.
      - Filter on the A6 tag `Project=live-ninja`, mirroring `template.yaml:2853-2855`.
      - Give each an Actual notification at 100% **and** a Forecasted notification at 100%, to the
        operator address, mirroring `template.yaml:2856-2864`. A budget with no notification is silent.
      - Assumed user count is **N = 1** (single-owner instance, `internal/realtime/quota.go:56`).
        Re-derive all three amounts if N changes.
      Per the org rule, no dashboards and no fixed-cost per-metric alarms — Cost Management budgets
      carry no fixed charge.
      DoD: `az consumption budget list --scope "/subscriptions/adc40fff-bab3-4bd2-b961-1832d0375052/resourceGroups/rg-liveninja-prod" --query "[?contains(name,'liveninja')].{n:name,a:amount,c:length(notifications)}" -o tsv`
      lists exactly 3 rows with amounts `100`, `250`, `500` and a non-zero notification count on each.

**Restart policy (WS-A):** Bicep deployment failures are deterministic. Read the
`az deployment group show` error, fix the template, redeploy. Ceiling 3 attempts per milestone; on
the 4th, mark `[!]` with the exact `Code` and `Message` and move to another workstream.

---

### WS-B — Voice engines

*Blocks: WS-F, WS-H, WS-I. Depends on: A3 only, and only for the live test.*

This is the workstream that delivers what was asked for and the one that can ship first. B2 through
B4 are pure Go with no Azure infrastructure dependency.

- [ ] **B1. Resolve the model-version question.** Deploy `gpt-realtime-2.1` in the Foundry portal. If
      it is not offerable, deploy `gpt-realtime-2`. Record which one won, verbatim, in the Execution
      log. This single fact propagates to B3, B4, WS-H, and WS-I.
      DoD: `curl -sS -X POST "$AZURE_OPENAI_ENDPOINT/openai/v1/realtime/client_secrets" -H "Authorization: Bearer $(az account get-access-token --resource https://cognitiveservices.azure.com --query accessToken -o tsv)" -H 'Content-Type: application/json' -d '{"session":{"type":"realtime","model":"<chosen>"}}' | jq -e '.value'`
      exits 0.
- [ ] **B2. Extend the engine enum.** In `/c/dev/live-ninja/internal/voiceengine/engine.go` add
      `EngineAzureOpenAIRealtime = "azure-openai-realtime"`,
      `EngineAzureOpenAIRealtimeMini = "azure-openai-realtime-mini"`, and
      `EngineAzureVoiceLive = "azure-voice-live"`. Keep `EngineNovaSonic` as a **deprecated alias**
      resolving to `azure-voice-live` at mint time — the contract is additive-only
      (`contracts/README.md:27-30`) and a 10-year device may still send it.
      `IsClientDirect()` must return `false` for `azure-voice-live` and `true` for both Azure OpenAI
      engines.
      DoD: `cd /c/dev/live-ninja && go test ./internal/voiceengine/ ./internal/realtime/` passes.
- [ ] **B3. Azure mint path.** `/c/dev/live-ninja/internal/realtime/mint.go:40` currently hardcodes
      `clientSecretsURL = "https://api.openai.com/v1/realtime/client_secrets"`. Add an Azure mint
      targeting `$AZURE_OPENAI_ENDPOINT/openai/v1/realtime/client_secrets` with an Entra bearer token
      from the container's managed identity — **no API key**. The OpenAI mint stays untouched so
      `openai-realtime` keeps working exactly as it does today.
      DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run 'Mint|Azure' -v` passes.
- [ ] **B4. Rate table.** Add to `/c/dev/live-ninja/internal/realtime/rates.go`. Azure Global list
      price per 1M tokens, read from the Azure pricing page on 2026-08-18:
      - `gpt-realtime-2.1` — text in 4.00, cached text 0.40, text out 24.00, audio in 32.00, cached audio 0.40, audio out 64.00
      - `gpt-realtime-2.1-mini` — text in 0.60, cached text 0.06, text out 2.40, audio in 10.00, cached audio 0.30, audio out 20.00
      - `gpt-realtime-2` — identical to `gpt-realtime-2.1`
      - **`gpt-realtime-mini`** — the OpenAI mini rates. This entry is missing today, so
        `openai-realtime-mini` is already billed in the UI badge at full `gpt-realtime` rates
        (`internal/realtime/mint.go:34` defines the model id; `rates.go:27-48` has no key for it).
      - **Voice Live Pro** rates for `azure-voice-live`, keyed on the model id the bridge sends.
      Also re-verify the two existing entries: the `rates.go:20-26` header dates them to 2025-08.
      **Change the fallback so a shipped engine can never be served by it.** `internal/realtime/rates.go:54`
      sets `defaultRates = modelRates["gpt-realtime"]` and `RatesFor` (`rates.go:58-63`) silently
      returns it for any unknown model — and `rates_test.go:33` asserts that behaviour, which is why
      the current DoD cannot detect a missing engine. Keep the fallback for genuinely unknown ids, and
      add `RatesForEngine` returning `(Rates, bool)` that logs `code=rates_missing` instead.
      DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run Rates` passes **and** a new
      `TestRatesCoverEveryShippedEngine` asserts every model id reachable from the six engine constants
      in `internal/voiceengine` has an explicit `modelRates` key. `TestRatesForUnknownModelFallsBack`
      must not be the thing that makes this pass.
- [ ] **B5a. Make the usage counters real — measurement only, no enforcement.** Per locked
      decision 6, 60 minutes a day is a planning figure and must **never** hard-block a session. The
      counters are still written, because without them spend is invisible, `usage-rollup` sums zeros,
      and B6 has no server-side data source. Persist per-session seconds and tokens at session close
      by calling `store.AddDayUsage(userID, day, tokens, seconds, 0)` and the month equivalent —
      bridged engines from the Voice Live `response.done` usage that WS-F M4 forwards, client-direct
      engines from the authenticated `POST /api/v1/transcript` cost body already parsed at
      `internal/webapp/api_routes.go:812-822`.
      **Do not add a 402 path on daily minutes.** Leave the existing real controls untouched:
      `QUOTA_SESSION_CAP_SECONDS=600`, 3 concurrent sessions, and the mint bucket
      (`internal/realtime/quota.go:57-58`).
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run 'Usage|Transcript'` passes, a new
      test asserts a completed session leaves non-zero `daySeconds` **and** that exceeding
      `QUOTA_DAILY_SECONDS` still returns 200 rather than 402, and after one real session the
      `USER#<uid>/USAGE#<YYYY-MM-DD>` item shows non-zero `daySeconds` and `dayTokens`.
- [ ] **B5. Set the advisory threshold and the soft warning.** Depends on B5a. Set
      `QUOTA_DAILY_SECONDS=3600` on the web and broker container apps as the **advisory** threshold —
      crossing it raises the existing `X-LN-Quota-Warning` header and an in-product notice, and
      nothing else. `QUOTA_SESSION_CAP_SECONDS=600` is unchanged: it is a cost control, not a UX rule,
      because it holds context near 14,600 tokens (see Cost model).
      `QUOTA_MONTH_TOKENS` is advisory on the same terms; set it from the cost model's stated
      derivation and record in the Execution log **whether the counter sums all billed tokens
      including cached re-reads (~35,000,000) or only uncached tokens (~1,300,000)** — the two differ
      by 27x and the plan cannot be read either way.
      Update the Help drawer in the same commit per `/c/dev/live-ninja/CLAUDE.md`: a user-visible
      warning is a user-visible capability.
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run 'Quota|TestHelpDrawer'` passes,
      and a test user pushed past `QUOTA_DAILY_SECONDS` receives `X-LN-Quota-Warning` with HTTP 200
      and a usable session — **a 402 on daily minutes is a test failure, not a pass.**
- [ ] **B6. Cache-hit-rate telemetry.** `/c/dev/live-ninja/web/static/js/conversation.mjs:1089` already
      reads `input_token_details.cached_tokens_details`. Emit the cached-to-total input ratio as a
      telemetry event and alert below **95%**, measured over a rolling 24 hours rather than per
      session. **Not 80%.** Steady state is ~97.8% cached, derived from the cost model's own figures.
      At an 80% cached share the mini engine already costs about $88/month against a $30.32 baseline —
      a 2.9x overrun standing exactly at the old threshold, still silent. 95% corresponds to about
      $42/month (1.4x). Emit a second, higher-severity signal below 85%.
      **Compute the ratio server-side**, from the usage WS-F M4 forwards and from the authenticated
      `POST /api/v1/transcript` cost body — not only in the browser, or Android and M5Stack sessions
      contribute nothing. The browser path also returns early when rates are absent
      (`conversation.mjs:1090`, `if (!rates) return;`), so on the bridged engine it computes nothing
      at all until WS-F M4 lands. **Depends on: WS-F M4.**
      **Signal path, to stay inside the org's no-fixed-cost rule:** emit a structured error log at
      `level=error, code=cache_ratio_degraded` from the web container and let it route through the
      WS-E M7 action group. Do not add a scheduled-query or per-metric alert rule for this.
      **This is not optional:** a cache collapse takes the mini engine from $30/month to $329/month and
      the full engine from $83 to $1,056, with no error and no other signal. With no hard daily cap
      (locked decision 6), this and the WS-A M8 budgets are the entire cost-safety story.
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run Telemetry` passes and the new
      `cache_ratio` event exists in `/c/dev/live-ninja/contracts/telemetry.schema.json`.
- [ ] **B7. Update the Help drawer.** Mandatory in the same commit per `/c/dev/live-ninja/CLAUDE.md`
      and `agents.md` — three new engines are user-visible settings. Edit the `HELP DRAWER` block in
      `/c/dev/live-ninja/web/templates/pages/conversation.html`.
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run TestHelpDrawer` passes.

---

### WS-C — Data layer (Cosmos DB)

*Blocks: WS-D. Depends on: A2.*

- [ ] **C1. Provision Cosmos DB for NoSQL, serverless.** Container `main` in database `liveninja`,
      partition key `/pk`, TTL enabled on `ttl`. Serverless avoids a provisioned-RU standing charge at
      this scale.
      DoD: `az cosmosdb sql container show -g rg-liveninja-prod -a <acct> -d liveninja -n main --query resource.partitionKey.paths -o tsv`
      returns `/pk`.
- [ ] **C2. Port `internal/store` behind the existing seam.** The `ddbAPI` interface at
      `/c/dev/live-ninja/internal/store/store.go:31-40` already abstracts the 8 DynamoDB operations and
      the tests already inject a fake. Implement a Cosmos-backed type satisfying the same seam.
      DoD: `cd /c/dev/live-ninja && go test ./internal/store/...` passes with zero test-file changes.
- [ ] **C3. Port the 5 secondary-index queries.** Cosmos indexes every property by default, so each
      becomes a cross-partition query rather than a GSI read. Convert exactly these five and measure
      the RU cost of each: `users.go:31`, `sessions.go:80`, `sessions.go:562`, `devices.go:325`,
      `deliverables.go:182`. If any exceeds 50 RU at production cardinality, add a materialised view
      fed by the change feed.
      DoD: an RU report exists at `/c/dev/live-ninja/docs/cosmos-ru-report.md` naming all five queries
      with a measured RU figure each, and `go test ./internal/store/...` passes.
- [ ] **C4. Re-embed the memory vectors.** `/c/dev/live-ninja/internal/memory/embedder.go:26` pins
      `amazon.titan-embed-text-v2:0` at 512 dims. Replace with Azure OpenAI `text-embedding-3-small`
      requesting `dimensions: 512`, so the stored vector width is unchanged. Every `EMB` item records
      its model, so stale vectors are detectable and a one-shot re-embed job can find them.
      DoD: `cd /c/dev/live-ninja && go test ./internal/memory/` passes, and a query for items whose
      recorded model is still the Titan id returns 0 rows.

---

### WS-D — Compute

*Blocks: WS-J. Depends on: WS-C, A6.*

- [ ] **D1. Containerise the web app.** `/c/dev/live-ninja/cmd/web/main.go` needs no code change — it
      is already a plain Fiber server listening on `$PORT`. Write
      `/c/dev/live-ninja/containers/web/Dockerfile` (multi-stage, distroless, arm64) and deploy to
      Container Apps with HTTP scaling.
      DoD: `curl -fsS https://<container-app-fqdn>/healthz` returns 200.
- [ ] **D2. Port the 12 background workers.** `realtime-broker` becomes an **internal-ingress**
      Container App — it must stay unreachable from the internet exactly as it is today, because it
      holds the OpenAI key. Queue consumers (`email-dispatch`, `rca-analyzer`, `codeupdate-dispatch`)
      become Container Apps Jobs with Service Bus scale rules. Scheduled ones (`usage-rollup`,
      `account-purge`, `topics-extract`) become cron Jobs. `iot-ingest` and `shadow-ingest` move in
      WS-G. **`cmd/iot-authorizer` and `cmd/nova-bridge` are deleted, not ported.**
      DoD: `az containerapp job list -g rg-liveninja-prod --query "length(@)" -o tsv` returns 6 or more,
      and `cd /c/dev/live-ninja && make build` succeeds for the remaining `FUNCTIONS` list in
      `/c/dev/live-ninja/Makefile`.
- [ ] **D3. Repoint the RCA analyzer.** `/c/dev/live-ninja/cmd/rca-analyzer/` calls Bedrock. Repoint to
      Azure OpenAI `gpt-5.2`. Keep the daily cap and cooldown parameters that exist today
      (`RcaDailyCap`, `RcaCooldownMinutes` in `template.yaml`).
      DoD: `cd /c/dev/live-ninja && go test ./internal/rca/ ./cmd/rca-analyzer/` passes.
- [ ] **D4. Front Door routing and cache behaviour.** Reproduce the CloudFront behaviour table
      from `template.yaml:2618-2740`: default to the web app, `/static/vendor/*` and
      `/static/models/*` to Blob, `/static/*` to the web app, and a new `/voice-live/*` to the WS-F
      bridge. Keep a security headers policy equivalent to `SecurityHeadersPolicy`. Per the repo's
      web-cache rule, HTML is served `no-cache`, fingerprinted assets keep long-lived immutable
      caching, and nothing intercepts API, SSE, or WebSocket traffic.
      **Test against the Front Door default endpoint, never against `live.jeremy.ninja`.** At this
      point in the sequence `live.jeremy.ninja` still resolves to CloudFront — J3 is what repoints it
      — so curling the production name here answers from the live AWS stack and would pass without
      Front Door being involved at all.
      DoD: with
      `FD=$(az afd endpoint show -g rg-liveninja-prod --profile-name <profile> --endpoint-name <ep> --query hostName -o tsv)`,
      `curl -sI "https://$FD/" | grep -i '^cache-control'` shows a no-cache form, and
      `curl -sI "https://$FD/static/<fingerprinted-asset>"` shows `immutable`.
- [ ] **D5. DNS records and certificate validation — Route 53 stays authoritative.** The application
      keeps the URL `https://live.jeremy.ninja`. Nothing is registered in Azure, no Azure DNS zone is
      created, and the `jeremy.ninja` nameservers do not change. Only records are added inside the
      existing Route 53 hosted zone. Rationale: moving the zone would migrate every unrelated record
      in `jeremy.ninja` at once — including the mail sender records that WS-E M4 has not replaced yet
      — and its rollback is a nameserver change with propagation delay, where the record-level
      approach rolls back one record at a time.
      **D5a runs first — the records cannot be written before it.** No milestone created the Front
      Door custom domains, and the `_dnsauth` validation token below is a *property of an existing
      custom domain*, not something that can be known in advance. Create both domains and attach them
      to the route:
      `az afd custom-domain create -g rg-liveninja-prod --profile-name <profile> --custom-domain-name azure-live --host-name azure.live.jeremy.ninja --minimum-tls-version TLS12 --certificate-type ManagedCertificate`,
      the same again for `live` / `live.jeremy.ninja`, then
      `az afd route update -g rg-liveninja-prod --profile-name <profile> --endpoint-name <ep> --route-name default --custom-domains azure-live live`.
      Read each token with
      `az afd custom-domain show -g rg-liveninja-prod --profile-name <profile> --custom-domain-name <name> --query validationProperties.validationToken -o tsv`.
      Attaching `live.jeremy.ninja` to the route before J3 is safe: the route only serves traffic that
      DNS actually sends it, and DNS still points at CloudFront until J3.

      Then add three records in Route 53:
      1. `TXT` at `_dnsauth.azure.live.jeremy.ninja` — the Front Door managed-certificate validation
         token for the WS-J M2 preview host. Front Door writes this record automatically only when
         the zone lives in Azure DNS; on Route 53 it is added by hand. **Leave it in place after
         validation** — it is read again on certificate rotation.
      2. `CNAME` `azure.live.jeremy.ninja` -> the Front Door endpoint hostname from D4. This is the
         hostname WS-J M2 dual-runs against; that milestone assumes it exists but never creates it.
      3. `TXT` at `_dnsauth.live.jeremy.ninja` — the validation token for the production host. Add it
         **here, during D5**, not at cutover. Domain validation and certificate issuance are
         independent of where the A/CNAME record points, so the production certificate can be issued
         and reach `Approved` well before J3. Doing it inside the J3 freeze window would put
         certificate issuance latency on the critical path of a write freeze.
      Do **not** touch the `live.jeremy.ninja` A/AAAA ALIAS records here — those are J3.
      **Authorization: GRANTED 2026-08-19 (locked decision 7).** These records live in AWS account
      `759775734231`, where every other resource stays read-only. The grant covers
      `live.jeremy.ninja` and `azure.live.jeremy.ninja` and nothing else — touching any other record
      in the `jeremy.ninja` zone is still a stop condition.
      DoD: `dig +short azure.live.jeremy.ninja` returns the Front Door endpoint hostname,
      `curl -fsS https://azure.live.jeremy.ninja/healthz` returns 200 over a valid certificate, and
      `az afd custom-domain show -g rg-liveninja-prod --profile-name <profile> --custom-domain-name <name> --query domainValidationState -o tsv`
      returns `Approved` for **both** `azure.live.jeremy.ninja` and `live.jeremy.ninja`.

---

### WS-E — Supporting services

*Blocks: WS-J. Depends on: A6. Runs parallel with WS-C and WS-D.*

- [ ] **E1. Key Vault signing.** Replace the two KMS keys (`AuthKey`, `JwtKey` at
      `template.yaml:1535-1587`) with Key Vault keys. `/c/dev/live-ninja/internal/auth/session.go`
      signs JWTs through KMS today; swap the signer implementation and keep the JWKS surface
      byte-identical so no issued token and no client breaks.
      DoD: `cd /c/dev/live-ninja && go test ./internal/auth/` passes and
      `curl -fsS https://<azure-web-host>/.well-known/jwks.json | jq -e '.keys[0].kid'` returns a kid,
      where `<azure-web-host>` is the D1 Container Apps FQDN or the D4 Front Door endpoint.
      **Not `live.jeremy.ninja`** — until J3 that name answers from CloudFront and the AWS stack, so
      testing it here would pass against the old KMS-backed JWKS and prove nothing about Key Vault.
- [ ] **E2. Blob Storage.** Six containers replacing the six S3 buckets (user, wakewords, assets, logs,
      analytics, deliverables). Preserve the 180-day deliverables lifecycle rule (`template.yaml:1877`).
      DoD: `az storage container list --account-name <acct> --query "length(@)" -o tsv` returns `6`.
- [ ] **E3. Service Bus.** Three queues replacing three SQS queues; native dead-lettering replaces the
      three explicit DLQ resources.
      DoD: `az servicebus queue list -g rg-liveninja-prod --namespace-name <ns> --query "[].name" -o tsv | wc -l`
      returns `3`.
- [ ] **E4. Email.** Azure Communication Services Email replaces SES. Verify the `@jeremy.ninja` sender
      domain. Keep the bounce-event destination behaviour of `SesBounceEventDestination`.
      DoD: a test send to the operator address returns a message id, and that id appears in the
      delivery log.
- [ ] **E5. Telemetry pipeline.** Event Hubs with Capture writing to Blob, replacing Firehose to S3.
      Azure Data Explorer replaces Glue and Athena for query.
      DoD: an event posted to `/api/v1/telemetry` appears in a Capture blob within 5 minutes.
- [ ] **E6. Wake-word training job.** Container Apps Job plus Azure Container Registry, replacing AWS
      Batch, ECR, `BatchVpc`, and its 8 networking resources. The training image at
      `/c/dev/live-ninja/containers/wakeword-train/` moves unchanged.
      DoD: `az containerapp job start -g rg-liveninja-prod -n wakeword-train` reaches `Succeeded`.
- [ ] **E7. Ops notifications.** Azure Monitor action group replacing the SNS `OpsTopic`. Per the org
      rule, no dashboards and no fixed-cost per-metric alarms — route on structured error logs and
      dead-letter queue depth instead.
      DoD: `az monitor action-group show -g rg-liveninja-prod -n ag-liveninja-ops --query enabled -o tsv`
      returns `true`.

---

### WS-F — Voice Live bridge (replaces nova-bridge)

*Depends on: B2, A6. Blocks: nothing.*

- [ ] **F1. New service `/c/dev/live-ninja/cmd/voice-live-bridge/`.** Keep the four-step connection
      contract documented at `/c/dev/live-ninja/cmd/nova-bridge/main.go:16-24` — verify the
      first-party session JWT from the query parameter using the existing JWKS verifier with no cloud
      call, validate the broker-created session slot, atomically redeem the token, then pump audio.
      Replace only the upstream: the Bedrock bidirectional stream becomes a Voice Live WebSocket to
      `wss://<resource>.services.ai.azure.com/voice-live/realtime?api-version=2026-04-10&model=gpt-realtime`,
      authenticated with an Entra bearer token from the container's managed identity.
      DoD: `cd /c/dev/live-ninja && go test ./cmd/voice-live-bridge/` passes.
- [ ] **F2. Normaliser.** Add `/c/dev/live-ninja/internal/voiceengine/voicelive.go` beside `nova.go`
      and `openai.go`. Voice Live reuses the Azure OpenAI Realtime event names, so this is
      substantially thinner than `nova.go` (596 lines) — most events pass through the existing
      `NormalizeOpenAI` path. Delete `nova.go` and `nova_test.go` only after F3 proves the replacement
      works end to end.
      DoD: `cd /c/dev/live-ninja && go test ./internal/voiceengine/` passes.
- [ ] **F3. Surface the Voice Live conversational features** that motivated choosing it over a plain
      bridge, via `session.update`: `turn_detection.type = azure_semantic_vad`,
      `remove_filler_words = true`, `input_audio_noise_reduction = azure_deep_noise_suppression`, and
      `input_audio_echo_cancellation = server_echo_cancellation`. `remove_filler_words` is the
      off-the-shelf fix for the false-barge-in behaviour that the "Patient" mic mode in
      `/c/dev/live-ninja/web/static/js/realtime.mjs` currently works around in the client.
      DoD: a live session logs a `session.updated` echoing all four settings.
- [ ] **F4. Fix the usage gap.** `/c/dev/live-ninja/web/static/js/conversation.mjs:1022` records that
      `nova-bridge` never surfaced usage, so Nova sessions showed no cost at all. The new bridge
      **must** forward `response.done` usage from the first commit. Do not repeat this.
      DoD: a bridged session produces a non-zero session cost in the UI badge.
- [ ] **F5. Do not port the dead infrastructure.** `cmd/nova-bridge/`, `containers/nova-bridge/`, the
      ECS cluster, ALB, target group, listener, task definition, both security groups, `BatchVpc` and
      its 8 networking resources, the `NovaBridge` ECR repo, and the `/nova/*` CloudFront behaviour —
      roughly 700 lines of `template.yaml`. AWS-side removal is WS-K.
      DoD: `grep -c -i 'nova' /c/dev/live-ninja/infra/main.bicep` returns `0`.

---

### WS-G — IoT Hub and firmware

*Depends on: A6. Blocks: WS-J. The long pole — start early, it needs physical device access.*

- [ ] **G1. Provision IoT Hub and Device Provisioning Service** with X.509 attestation replacing
      fleet-provisioning-by-claim.
      DoD: `az iot hub show -g rg-liveninja-prod -n <hub> --query properties.state -o tsv` returns
      `Active`.
- [ ] **G2. Port the ingest paths.** `/c/dev/live-ninja/cmd/iot-ingest/` (70 lines) reads the raw MQTT
      body delivered by an IoT Rule action; it becomes an Event Hub-triggered Container Apps Job.
      `/c/dev/live-ninja/cmd/shadow-ingest/` (605 lines) moves from device shadows to device twins.
      **`/c/dev/live-ninja/cmd/iot-authorizer/` (606 lines) is deleted** — IoT Hub does X.509 natively,
      so the custom authorizer has no counterpart.
      DoD: `cd /c/dev/live-ninja && go test ./cmd/iot-ingest/ ./cmd/shadow-ingest/` passes, and
      `test -d /c/dev/live-ninja/cmd/iot-authorizer` returns non-zero.
- [ ] **G3. Firmware change.** `/c/dev/live-ninja/firmware/components/ln_iot/` swaps the AWS ATS
      endpoint (`ln_iot.h:99`) and fleet-provisioning-by-claim for the IoT Hub endpoint and DPS.
      `/c/dev/live-ninja/firmware/components/ln_realtime/ln_realtime.c:64` keeps its OpenAI direct URL
      and swaps the `nova-bridge` mode (`ln_rt_session.c:18`) for `azure-voice-live`.
      DoD: `cd /c/dev/live-ninja/firmware && idf.py build` exits 0.
- [ ] **G5. Migrate client live events to Azure Web PubSub** (locked decision 8). This is the
      milestone that makes deleting `cmd/iot-authorizer` safe, and G2 must not delete it until G5 has
      shipped. `cmd/iot-authorizer/main.go:1-2` states it is "the AWS IoT Core custom authorizer
      fronting MQTT over WebSockets for **the web and Android clients**", and
      `android/.../realtime/LiveEventsClient.kt:318` builds
      `wss://${c.endpoint}/mqtt?x-amz-customauthorizer-name=${c.authorizerName}`. Devices authenticate
      with X.509 and never touch it, so IoT Hub is not its replacement.
      Port three surfaces to Azure Web PubSub with a server-minted per-user token, keeping the
      per-user topic isolation the IoT policy provides today: `/c/dev/live-ninja/internal/sync/events.go`,
      `/c/dev/live-ninja/internal/webapp/iot_routes.go` (the `GET /api/v1/iot/credentials` route and
      its `endpoint`/`authorizerName`/`topicFilter` response shape), and the Android pair
      `LiveEventsClient.kt` / `MqttCodec.kt`. Keep the existing response field names as a deprecated
      alias per `contracts/README.md:27-30` so an installed build does not hard-fail.
      **Until the fielded Android population has updated, AWS IoT Core, the authorizer Lambda, and the
      credentials route must stay alive** — WS-K M2 is blocked on that, not only on the M5Stack.
      DoD: `cd /c/dev/live-ninja && go test ./internal/sync/ ./internal/webapp/ -run 'Events|Iot'`
      passes, and two browser sessions for the same user observe each other's presence and
      turn-taking lock through Web PubSub with `cmd/iot-authorizer` stopped.
- [ ] **G4. `[!]` BLOCKED BY DESIGN — provision the physical device.**
      `/c/dev/live-ninja/plan.md:1262` records an explicit prohibition on deleting or reprovisioning
      the existing M5Stack Thing or its certificate. Migrating it to IoT Hub necessarily reprovisions it.
      **Unblocked by:** explicit operator go-ahead naming the device, plus physical access to reflash it.
      Until then the device stays on AWS IoT Core and WS-K M2 must not delete the AWS IoT resources.
      DoD (once unblocked):
      `az iot hub device-identity show --hub-name <hub> -d <device-id> --query status -o tsv` returns
      `enabled`, and the device publishes telemetry that lands in Cosmos.

---

### WS-H — Web client

*Depends on: B2, B3. Blocks: WS-J.*

- [ ] **H1. Add the two client-direct Azure transports.**
      `/c/dev/live-ninja/web/static/js/realtime.mjs` already branches on the `mode` field returned by
      `GET /api/v1/realtime/session`. Add `mode: "azure-openai-direct"`. Because Azure's Realtime API
      is protocol-identical to OpenAI's, this reuses the entire existing WebRTC path — only the SDP
      POST host changes from `https://api.openai.com/v1/realtime/calls` to
      `$AZURE_OPENAI_ENDPOINT/openai/v1/realtime/calls`. **Do not fork the transport.**
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run 'ImportMap|Render'` passes, and a
      browser session pinned to `azure-openai-realtime-mini` completes a turn.
- [ ] **H2. Replace the Nova transport with the Voice Live transport.** Add `mode: "azure-voice-live"`,
      reusing the WSS/PCM16 skeleton the Nova path established. Keep `mode: "nova-bridge"` accepted
      and mapped to the new one, so a client holding an older cached bundle does not hard-fail.
      DoD: a browser session on `azure-voice-live` completes a turn and reports a non-zero cost.
- [ ] **H3. Guard the module graph.** A `conversation.mjs` change can silently kill the whole page for
      a client holding an older cached sibling module. Bump every fingerprint in the same deploy and
      verify against a primed cache, not just a hard reload.
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run ImportMap` passes, and both a
      hard-reload and a warm-cache reload render `/conversation`.
- [ ] **H4. Settings and Help.** Add the three engines to the settings picker and to the Help drawer in
      the same commit (`CLAUDE.md` rule).
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run 'TestHelpDrawer|Settings'` passes.

---

### WS-I — Android client

*Depends on: B2, B3. Blocks: WS-J.*

- [ ] **I1. Repoint the backend.** Every endpoint is a constant in
      `/c/dev/live-ninja/android/app/src/main/java/ninja/jeremy/liveninja/config/BackendConfig.kt`.
      Because `live.jeremy.ninja` is kept as the domain (see WS-J), `BASE_URL` does not change at all.
      Add `AZURE_REALTIME_CALLS_URL` beside the existing `OPENAI_REALTIME_CALLS_URL`.
      DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:assembleDebug` exits 0.
- [ ] **I2. Extend `WebRtcTransport.kt`** to take the Azure SDP host from the session bootstrap rather
      than a compile-time constant. Same protocol, so no new transport class.
      DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest` passes.
- [ ] **I3. Replace `NovaBridgeTransport.kt` with `VoiceLiveTransport.kt`.** Same WSS/PCM16 framing.
      DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest` passes.
- [ ] **I4. Update `MqttCodec.kt` and `LiveEventsClient.kt`** for IoT Hub. Gated behind WS-G; if G4
      stays blocked, this stays on the AWS IoT path and is marked `[!]`.
      DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest` passes.
- [ ] **I5. Instrumented verification on real hardware.** Two devices are attached: the Tab S9 FE
      (`R52XC06P9KJ`) and a Galaxy S9 sitting exactly on `minSdk 29`. Run on **both** — the S9 is the
      minSdk floor and has historically caught what the tablet did not. If a Compose test goes red,
      dump the semantics tree before theorising; the last such failure was a network race, not
      contamination.
      DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:connectedDebugAndroidTest` passes on both
      serials.
- [ ] **I6. Release build.** Ship through `.github/workflows/android-release.yml`, not a local build.
      **Blocked until WS-J M4 passes and `alexa-version` merges to `main`** (locked decision 9).
      `build-and-publish` is hard-gated at `.github/workflows/android-release.yml:112` on
      `github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main'`, so a run
      triggered from `alexa-version` skips that job and still reports `success` — the original DoD
      below could pass with no APK ever built. Do not work around the guard; the merge is the path.
      Also required before this ships: **I7**, or the APK the run publishes is unreachable.
      DoD: after the merge,
      `gh run view <id> --json jobs -q '.jobs[]|select(.name=="build-and-publish").conclusion'`
      returns `success` **and** `curl -fsS https://live.jeremy.ninja/v1/app/android/latest | jq -r .versionCode`
      is greater than the version this run started from.
- [ ] **I7. Repoint Android distribution off S3.** `.github/workflows/android-release.yml:437-446`
      publishes the APK, `android-latest.json`, and `android-assetlinks.json` into the AWS assets
      bucket with `aws s3 cp`, and `internal/webapp/android_distribution_routes.go:21-23` serves them
      by reading those S3 objects — including `/.well-known/assetlinks.json`, which is what makes the
      `https://live.jeremy.ninja/auth/lwa/app-return` App Link verify. After WS-J M3 repoints the
      hostname to Front Door, a stale `assetlinks.json` breaks the login handoff for every installed
      build and the in-app update channel goes dead. Move the upload to `azure/login@v2` plus
      `az storage blob upload` into the WS-E M2 assets container, and repoint the serving routes to
      Blob. This must land **before** WS-J M3, not after.
      DoD: `curl -fsS https://azure.live.jeremy.ninja/.well-known/assetlinks.json | jq -e '.[0].target.package_name=="ninja.jeremy.liveninja"'`
      and `curl -fsS https://azure.live.jeremy.ninja/v1/app/android/latest | jq -e .versionCode` both exit 0.

---

### WS-J — Data migration and cutover

*Depends on: WS-C, WS-D, WS-E, WS-H, WS-I.*

- [ ] **J1. Export and import.** Export the DynamoDB table to S3, transform to Cosmos documents, bulk
      import. Preserve TTL semantics — DynamoDB TTL is an absolute epoch-seconds attribute while
      Cosmos TTL is a relative seconds value, so this needs a per-item conversion, not a straight copy.
      DoD: item counts match per `pk` prefix. Write the comparison to
      `/c/dev/live-ninja/docs/migration-reconciliation.md`; zero mismatches required.
- [ ] **J2. Dual-run.** Run Azure against the imported data behind a preview hostname
      (`azure.live.jeremy.ninja`) while `live.jeremy.ninja` stays on AWS. Exercise every surface: web,
      Android, all six engines.
      DoD: a checklist in the Execution log with one line per engine per surface, each carrying a pass
      and a session id.
- [ ] **J3. Freeze, re-sync, cut DNS.** Take a write freeze, re-run the J1 delta, then repoint
      `live.jeremy.ninja` in Route 53 from the CloudFront A + AAAA ALIAS pair
      (`template.yaml:2741-2759`) to a `CNAME` at the Front Door endpoint. `live` is a subdomain and
      not the zone apex, so a plain `CNAME` is legal here.
      **Precondition 1: WS-D M5 shows `live.jeremy.ninja` already `Approved` on Front Door.**
      Certificate issuance must not happen inside the freeze window.
      **Precondition 2: authorization — GRANTED 2026-08-19 (locked decision 7).** Repointing
      `live.jeremy.ninja` is covered by the narrow Route 53 grant. No other AWS resource in account
      `759775734231` may be touched during the cutover.
      Set the replacement `CNAME` to a **60-second TTL at creation**, so a rollback propagates in
      about a minute. There is no TTL pre-drop step on the outgoing records and none is possible:
      `DnsRecordA` and `DnsRecordAAAA` (`template.yaml:2740-2759`) are Route 53 *aliases* — they carry
      `AliasTarget:` and no `TTL:` property, and an alias inherits its target's TTL rather than
      declaring one. Confirm what is actually being served before the freeze with
      `dig live.jeremy.ninja | grep -E '^live\.jeremy\.ninja'`.
      **Before cutting, disable the AWS pipeline:**
      `gh workflow disable deploy.yml --repo JeremyProffittOrg/live-ninja`. `deploy.yml` carries
      `workflow_dispatch:` alongside its `main` trigger, and both alias records are CloudFormation-owned,
      so any manual dispatch after the cutover silently re-creates the CloudFront aliases and reverses
      J3 with no error. Re-enabling it is a required step of any rollback.
      Because the domain does not change, the cutover needs no Android release, no firmware change,
      and no client certificate work — no client pins TLS (see Verified facts, DNS and TLS today).
      DoD: `dig +short live.jeremy.ninja` resolves to the Front Door endpoint and
      `curl -fsS https://live.jeremy.ninja/healthz` returns 200.
      **Rollback:** re-create the two ALIAS records from `template.yaml:2741-2759`. Recovery is
      bounded by the 60-second TTL and needs no Azure-side change.
- [ ] **J4. Soak.** 72 hours on Azure with the AWS stack still standing and re-pointable.
      DoD: zero unhandled errors in Log Analytics across the window, and the B6 cache-ratio telemetry
      stays above 80%.

---

### WS-K — AWS decommission

*Depends on: WS-J M4. **Every milestone here is a stop condition.** Nothing in this workstream runs
without explicit per-step operator approval at the time.*

- [ ] **K1. `[!]` Delete the compute and data stack.** Requires explicit go-ahead. Take a final
      DynamoDB export and an S3-to-Blob sync first, and verify both before anything is deleted.
- [ ] **K2. `[!]` Delete the IoT resources.** Additionally blocked by WS-G M4 — if the M5Stack is still
      on AWS IoT Core, this must not run at all.
- [ ] **K3. `[!]` Decide `internal/ghost`.** `/c/dev/live-ninja/internal/ghost/client.go` reaches
      ghost-cli by AWS IAM invoke and explicitly "cannot name its own principal" — ghost-cli pins it
      server-side from the `live-ninja` allowlist entry. Either keep one narrow AWS role for this call,
      or get ghost-cli to accept a different transport. **Do not close the AWS account until this is
      resolved**, or the voice code-update tool stops working.
- [ ] **K4. `[!]` Retire the AWS OIDC role and `deploy.yml`.** Last, and only once K1 through K3 are
      done. `deploy.yml` lives on `main`, so this depends on the WS-J M4 merge in locked decision 9.
      **Before this runs, disable rather than delete:** `deploy.yml` carries `workflow_dispatch:`
      alongside its `main` trigger and owns the CloudFront alias records through CloudFormation, so
      until it is gone a manual dispatch silently re-creates them and reverses WS-J M3.
      DoD: `gh workflow list --repo JeremyProffittOrg/live-ninja --all | grep -c deploy.yml` returns
      `0`, and `aws iam get-role --role-name gha-deploy` returns `NoSuchEntity`.

---

## Cost model at 60 minutes a day, per user

**60 minutes a day is a planning assumption, not an enforced ceiling** (locked decision 6). Nothing
blocks a user at 60 minutes; WS-A M8's budgets are the only backstop. Every figure below is what an
average day costs, not what a bad day is capped at.

Measured against Azure list prices read from the Azure pricing pages on 2026-08-18. Assumes 18
minutes/day of user speech, 24 minutes/day of assistant speech, Microsoft's published conversion of
10 tokens/second input and 20 tokens/second output audio, near-100% prompt-cache hits, and — the
term that was previously unstated — **an average of about 70 assistant turns per day over roughly 6
sessions, re-reading about 11,300 tokens of context per turn, giving ~30.8M cached input tokens per
month.** That cached term is roughly 26x the 1.19M raw audio tokens and is the dominant cost in every
row below; the figures cannot be reproduced from the audio assumptions alone.

**Cost scales linearly in turns per day.** At 140 turns/day the mini engine is roughly $40/month and
Gemini roughly $197/month. Assumed user count is **N = 1** (single-owner instance,
`internal/realtime/quota.go:56`).

```
azure-openai-realtime-mini   gpt-realtime-2.1-mini    $30.32/mo   <- default engine
azure-openai-realtime        gpt-realtime-2.1         $82.87/mo
azure-voice-live             Voice Live Pro native    $81.72/mo   PLACEHOLDER + bridge infra
openai-realtime              gpt-realtime (unchanged) $81.72/mo
gemini-flash-live            (unchanged, no caching) $104.60/mo
```

Two numbers govern the design:

0. **The `azure-voice-live` row above is a placeholder, not a Voice Live price.** $81.72 is the
   `gpt-realtime` figure copied across — it reproduces exactly from `internal/realtime/rates.go`'s
   `gpt-realtime` entry, which is why it matches the `openai-realtime` row to the cent. Voice Live Pro
   is priced separately and has not been sized. WS-B M4 must add a real entry.
1. **Cache collapse is an 11x event on the default engine and a 13x event on the full engine.** The
   multiplier is engine-dependent because the cached discount is: 33x on mini (10.00 vs 0.30), 80x on
   the full model (32.00 vs 0.40). With zero cache hits `azure-openai-realtime-mini` goes from $30.32
   to $329/month (10.9x) and `azure-openai-realtime` from $82.87 to $1,056/month (12.7x). The
   previously published $1,054 was $2 off its own arithmetic — both rows now use one cached-token
   volume. It produces no error and no other signal. WS-B M6 exists solely to make it visible. Never
   inject a timestamp, reorder the tool list, or rewrite the system prompt mid-session — all three
   invalidate the cached prefix.
2. **The 10-minute session cap is a cost control, not only a UX rule.** It holds context near 14,600
   tokens. Raising it grows the per-turn re-read superlinearly. `QUOTA_SESSION_CAP_SECONDS` stays at 600.

Standing Azure infrastructure, independent of usage. The earlier "$60 to $90 per month" figure named
only five services and is **a floor, not an estimate of the total** — the plan provisions at least
eight more that carry a standing or unavoidable charge. Line items, one per always-on resource:

```
PRICED IN THE ORIGINAL $60-$90 FLOOR
  Container Apps minimum replicas (web, jobs, voice-live-bridge)
  Front Door Standard base fee
  IoT Hub tier
  Log Analytics ingestion

NAMED BUT NOT ACTUALLY SIZEABLE YET
  Cosmos serverless RU + storage        [not yet sized - WS-C M3 measures it]
                                        ("serverless floor" was a misnomer: serverless
                                         Cosmos has no floor, it bills per consumed RU]

MISSING FROM THE ORIGINAL FIGURE ENTIRELY
  Azure Data Explorer cluster           [WS-E M5 - VM-backed, bills while it exists;
                                         Athena billed per query, so this is a new class]
  Event Hubs throughput units + Capture [WS-E M5 - Capture is an always-on add-on]
  Service Bus namespace base charge     [WS-E M3]
  Azure Container Registry + storage    [WS-E M6]
  Blob Storage + transactions + egress  [WS-E M2]
  Key Vault signing operations          [WS-E M1 - on every auth path]
  Front Door egress GB + request charges
  Azure OpenAI gpt-5.2 for the RCA analyzer
                                        [WS-D M3 - cap 10/day x 2000 output tokens
                                         = 600k output tokens/month on a frontier
                                         model, plus input context; in neither table]
  Azure OpenAI text-embedding-3-small   [WS-C M4]
```

**There is no total until WS-C M3 reports measured RU and WS-E M5 reports ingestion volume.** WS-A M8
sets budgets from the upper bound, not from this floor. Add a milestone-completion step to WS-A: once
the stack is live, run
`az costmanagement query --scope "/subscriptions/adc40fff-bab3-4bd2-b961-1832d0375052/resourceGroups/rg-liveninja-prod" --timeframe MonthToDate --type ActualCost --dataset-aggregation '{"c":{"name":"Cost","function":"Sum"}}' --dataset-grouping name=ServiceName type=Dimension -o table`
and record the per-service output verbatim in the Execution log. That replaces the estimate with a
measurement.

---

## Sequencing

Start these three on day one, in parallel:

- **WS-A** (foundation) — blocks every deploy, so it must never idle.
- **WS-B** (voice engines) — pure code, no infrastructure dependency, and it is the deliverable that
  was actually asked for. Ships first.
- **WS-G** (IoT and firmware) — the long pole, because it needs physical device access and is already
  partly blocked.

Then WS-C and WS-E in parallel once A2 lands. WS-D behind WS-C. WS-F behind B2. WS-H and WS-I behind
B3. WS-J only when D, E, H, and I are all green. WS-K last, and only with per-step approval.

Never let the run idle on a blocked item. If WS-G M4 blocks on physical device access, all of WS-A
through WS-F is still runnable.

---

## Execution log

Appended in place as work lands: commands run, commits, identifiers, and what each verification
actually returned. Written as it happens, not reconstructed at the end.

- **2026-08-18** — Branch `alexa-version` created from `main` @ `a1e30eb` and pushed.
  `git push -u origin alexa-version` returned `* [new branch] alexa-version -> alexa-version`.
  Confirmed no deploy was triggered: `/c/dev/live-ninja/.github/workflows/deploy.yml` is
  `on: push: branches: [main]` only.
- **2026-08-18** — Four migration decisions locked by the operator. See Locked decisions.
- **2026-08-18** — This plan written from a full read of the repository. No Azure resource created yet.
  **Next action: WS-A M1.**
- **2026-08-19** — DNS decision recorded and the plan corrected. Route 53 stays the authoritative
  zone for `jeremy.ninja`; no domain is registered in Azure and no Azure DNS zone is created. The
  application keeps the URL `https://live.jeremy.ninja` through and after cutover. Verified by
  command: `nslookup -type=NS jeremy.ninja 8.8.8.8` returned the four `awsdns` nameservers, and
  `nslookup -type=SOA live.jeremy.ninja 8.8.8.8` returned the `jeremy.ninja` SOA, proving `live` is a
  record in that zone and not a delegated zone. `grep -rniE
  'certificatePinner|pinning|network_security_config|sha256/'
  /c/dev/live-ninja/android/app/src/main` returned no matches, so the certificate issuer change is
  invisible to clients. Plan edits: service-mapping row reassigned from WS-A to WS-D and "Azure DNS"
  removed; a "DNS and TLS today" block added to Verified facts; D4 retitled and its definition of
  done repointed from `live.jeremy.ninja` (which still answers from CloudFront at that point in the
  sequence, so it would have passed against the AWS stack) to the Front Door default endpoint; a new
  D5 added covering the three Route 53 records and certificate validation; J3 given the D5
  precondition, the TTL pre-drop, and an explicit rollback; E1's definition of done corrected for the
  same production-hostname trap as D4.
- **2026-08-19** — Post-edit review of the DNS change caught three defects in it, all verified
  against the repo and all now corrected. (1) The J3 instruction to pre-drop the TTL on the
  `live.jeremy.ninja` records was **not executable**: `template.yaml:2740-2759` shows `DnsRecordA` and
  `DnsRecordAAAA` are Route 53 aliases carrying `AliasTarget:` and no `TTL:`, and an alias has no
  settable TTL. Replaced with a 60-second TTL on the incoming `CNAME`, which is where the rollback
  window actually comes from. (2) `deploy.yml` carries `workflow_dispatch:` and owns both alias
  records through CloudFormation, so a manual dispatch after cutover would silently restore the
  CloudFront aliases and reverse J3 with no error; J3 now disables the workflow before cutting and
  re-enables it as part of rollback. (3) **Authorization gap, unresolved and blocking:** every record
  D5 and J3 write lives in the `jeremy.ninja` hosted zone in AWS account `759775734231`.
  `## Standing authorizations` grants only read access there and lists modification as NOT granted.
  D5 and J3 are both marked `[!]` pending an explicit narrow grant from the operator. That grant is
  deliberately **not** written here — a standing authorization is the operator's to give, and
  self-granting one would fabricate consent that was never given.
- **2026-08-19** — Seven persona reviewers audited the plan against the repository on non-overlapping
  slices. Their claims were re-verified in the main thread before anything was written here; only
  command-confirmed items are recorded. Three entries in `## Verified facts` / `## Locked decisions`
  were found to be false or materially incomplete and are corrected above: (a) the data-layer sizing
  claim ("a 5-query problem, not a schema rewrite") is withdrawn — 17 files outside `internal/store`
  build DynamoDB requests directly, the `ddbAPI` seam is typed in DynamoDB SDK structs with 30
  `UpdateExpression` and 85 `ConditionExpression` uses, and it declares 7 methods not 8; (b) Locked
  decision 3's premise that X.509 replaces `cmd/iot-authorizer` is contradicted by
  `cmd/iot-authorizer/main.go:1-2` — that authorizer fronts MQTT for the web and Android clients, not
  for devices, so deleting it removes live events for every client and needs its own migration; (c) the
  Android "endpoints are centralised" fact is qualified — `callsUrl` is a compile-time constant never
  read from the session JSON, so an old build handed an `azure-*` mode POSTs an Azure credential to
  `api.openai.com` rather than failing closed. The decisions themselves are annotated, not reversed —
  reversing a user-confirmed locked decision is the operator's call.
- **2026-08-19** — Ordering defect in D5 found by the platform reviewer and corrected. As first
  written, D5 instructed the run to add a `_dnsauth` TXT record carrying "the Front Door
  managed-certificate validation token" before any Front Door custom domain existed. The token is a
  property of an existing custom domain — it is read back with
  `az afd custom-domain show --query validationProperties.validationToken` — so the step was
  unexecutable as sequenced, and D5's own definition of done then queried
  `az afd custom-domain show` for two objects nothing had created. D5 now opens with D5a, which
  creates both custom domains and attaches them to the route, and only then writes the records.
- **2026-08-19** — Fourth and last false entry in `## Verified facts` corrected, from the cost
  reviewer and re-verified in the main thread. "Raising to 60 min/day is an environment-variable
  change, not a code change" is withdrawn: the daily and monthly quota gates read counters that
  production never writes, so both caps are inert. Verified by
  `grep -rn 'AddDayUsage\|AddMonthUsage' --include=*.go .`, which returns the two definitions at
  `internal/store/usage.go:92` and `:102` plus a single caller at `usage.go:112` (`BumpDayMints`,
  passing `0, 0, 1`). Independently corroborated by `docs/launch-go-no-go-2026-07-26.md:41`, which
  already carries this as a **HOLD**. This is a pre-existing AWS defect that the migration inherits
  rather than causes, but it invalidates the premise of the cost-model section and of WS-B M5.
- **2026-08-19** — Five operator decisions locked (6 through 10 above) and the plan updated to match.
  Verbatim where given: "60 minutes should not be a hard cap" and "you'll need to update dns".
  Resulting edits: WS-B M5 split into M5a (write the counters, measurement only, an explicit test that
  exceeding the daily threshold returns 200 not 402) and M5 (advisory threshold plus soft warning);
  WS-A M8 rebudgeted to $100/$250/$500, resource-group-scoped, tag-filtered, with actual and
  forecasted notifications, and noted as the only spend backstop now that no hard cap exists; the
  narrow Route 53 grant added to `## Standing authorizations`, which cleared the `[!]` authorization
  blocks on WS-D M5 and WS-J M3; a new WS-G M5 porting client live events to Azure Web PubSub, with
  `cmd/iot-authorizer` deletion and WS-K M2 both blocked behind it; WS-I M6 marked blocked on the
  post-WS-J merge to `main` with a DoD that can actually fail, plus a new WS-I M7 moving Android
  distribution and `assetlinks.json` off S3 before the hostname cuts over; and WS-K M4 given a DoD
  and the `workflow_dispatch` hazard note.
- **2026-08-19** — Cost model corrected against its own arithmetic. The five published per-engine
  figures could not be reproduced from the stated assumptions: solving for the missing terms recovers
  ~30.8M cached input tokens/month (about 70 assistant turns/day re-reading ~11,300 tokens), which is
  ~26x the raw audio volume and the dominant cost in every row. That term is now stated, with a
  linear-in-turns sensitivity line. The "13x" collapse claim was neither engine's figure — it is 10.9x
  on mini and 12.7x on the full model, because the cached discount differs (33x vs 80x); the full
  engine's $1,054 was $2 off its own arithmetic and is now $1,056. The `azure-voice-live` row is
  marked PLACEHOLDER: $81.72 reproduces exactly from `rates.go`'s `gpt-realtime` entry, so it is that
  price copied across, not a Voice Live Pro price. The standing-infrastructure figure is relabelled a
  floor and expanded into line items — Azure Data Explorer, Event Hubs Capture, Service Bus, ACR,
  Blob, Key Vault operations, Front Door egress and requests, and RCA/embedding token spend were all
  absent. WS-B M4 gains the missing `gpt-realtime-mini` and Voice Live entries plus a test that a
  shipped engine can never fall through `RatesFor`'s silent default. WS-B M6's threshold moves from
  80% to 95%: at 80% the mini engine has already tripled its cost.
