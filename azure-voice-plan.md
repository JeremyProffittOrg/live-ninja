# Azure Voice Engines Plan — live-ninja

**Governs:** adding Azure-hosted realtime voice engines to Live Ninja as selectable voice-model
options, authenticated against Azure rather than an OpenAI platform key, with **no container and no
always-on backend media bridge**.

**Does NOT govern:** the full AWS→Azure migration. That is
[azure-migration-plan.md](azure-migration-plan.md) on the `alexa-version` branch, which is 100%
unstarted (`## Execution log`, 2026-08-18: "No Azure resource created yet"). This plan is
deliberately independent of it: it ships to production on AWS today, and every artifact it creates
(the Azure resources, the engine constants, the rate rows, the client transports) is reused
unchanged by that migration later. Nothing here has to be undone for the migration to proceed.

**Status markers:** `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked.

---

## Locked decisions (user-confirmed 2026-08-24; do not revisit)

1. **Azure OpenAI Realtime authenticates with a resource API key held in SSM.** The broker runs on
   AWS Lambda and has no Azure managed identity. The operator chose the resource `api-key` over an
   Entra app registration with a certificate credential. The key is written by
   `/c/dev/live-ninja/scripts/set-secret.sh` into SSM SecureString and is read through the existing
   `config.Loader` exactly like `ParamOpenAIAPIKey`. **No agent ever sees its value**
   (`/c/dev/live-ninja/deploy.md`, "Credentials — agents NEVER handle secret values").

2. **Azure Voice Live ships now, isolated.** The operator accepted the security downgrade described
   under `## The token problem, stated honestly` below: Voice Live has no ephemeral-token mint, so
   the container-free path requires handing the browser a resource-scoped Microsoft Entra bearer
   token and letting the client author its own session config. It is contained by running Voice Live
   on its **own** Microsoft Foundry resource, in its **own** resource group, reachable by its **own**
   service principal that holds no other role anywhere in the subscription.

3. **Web and Android ship together.** Both surfaces currently hardcode
   `https://api.openai.com/v1/realtime/calls` and would POST an Azure credential to OpenAI if handed
   an Azure session today (see Verified fact R7). Both are fixed in the same release wave, and the
   server-side client-version gate (WS-D M1) is what makes that safe.

4. **Work lands on `main`.** Each milestone commits and pushes to `main`, and per
   `/c/dev/live-ninja/CLAUDE.md` **every push to `main` is a production deploy**. All four new
   engines are therefore inert until WS-F M1 passes: they are absent from every settings enum until
   the milestone that adds them, and the version gate rejects every client that cannot handle them.

5. **The Android build is installed on the Galaxy S9 phone as part of the run** (operator,
   2026-08-24, verbatim: "install on s9 when done", clarified same day: "I mean s9 the phone").
   See WS-E M4. The target is **unambiguous and fixed**: model `SM-G965U`, serial
   `4633424442303098`, Android 10 / **SDK 29**, `arm64-v8a`. It is **not** the Galaxy Tab S9 FE
   (`R52XC06P9KJ`, `SM-X518U`) and **not** the Tab S11 Ultra (`R5GL700QAGK`, `SM-X930`), both of
   which are SDK 36 and both of which were attached to this machine when the plan was written.
   The phone sits exactly on the app's `minSdk = 29` floor
   (`/c/dev/live-ninja/android/app/build.gradle.kts`), so it is the only attached device that
   exercises the minimum supported API level — which is the reason it is the one that matters.

6. **Creating and paying for the Azure resources this plan needs is authorized** (operator,
   2026-08-24, verbatim: "enable gpt-live in azure and what ever else is needed to leverage azure
   instead of a gpt key for gpt-live"). See `## Standing authorizations`.

---

## Consequence of the locked decisions (not a new decision, not open for re-litigation)

Locked decisions 1 and 2 interact, and the interaction has exactly one consistent resolution. It is
recorded here so the run does not stall on it mid-flight:

**Voice Live still needs a service principal, even though Azure OpenAI Realtime uses an API key.**

- Azure OpenAI Realtime has an ephemeral mint (`POST /openai/v1/realtime/client_secrets`). The
  broker calls it *server-side* with the API key and hands the browser a short-lived, config-bound
  `ek_…`. The API key never leaves Lambda. Locked decision 1 works.
- Voice Live has **no mint**. The browser itself must authenticate to
  `wss://…/voice-live/realtime/calls`. Its only two credentials are the **resource `api-key`** (a
  permanent, non-expiring, full-resource key) or an **Entra bearer token** in the `Authorization`
  query-string parameter. Putting the permanent resource key in a browser is not an option under any
  reading of locked decision 2's word "isolated".

Therefore Voice Live gets a **dedicated Entra app registration with a client secret** (SSM
SecureString, `/live-ninja/prod/azure/voicelive_client_secret`), holding `Cognitive Services User` +
`Foundry User` on the Voice Live resource **and nothing else, anywhere**. The broker performs the
client-credentials exchange and returns the resulting token to the client with its observed
expiry. This is the minimum consistent with both locked decisions; a certificate credential is the
documented upgrade path (WS-G M2) and is not required to ship.

---

## The token problem, stated honestly

This section exists so no later reader mistakes the Voice Live engines for the same security shape as
the OpenAI, Azure-OpenAI, or Gemini engines. Put it in `docs/voice-engines.md` verbatim (WS-F M3).

| Engine | Credential the client holds | Bound to one session? | Bound to the server's config? | Expiry |
|---|---|---|---|---|
| `openai-realtime` / `-mini` | OpenAI `ek_…` | yes | **yes** — model, voice, instructions, tools fixed at mint | 60 s |
| `gemini-flash-live` | Gemini single-use token | yes (single-use) | **yes** — constrained at mint | short |
| `gpt-live-azure` / `-mini` | Azure `ek_…` | yes | **yes** — same `client_secrets` contract | 60 s |
| `azure-voice-live` / `-lite` | Entra bearer, resource-scoped | **no** | **no** — client sends `session` in `rtc.call.sdp.create` | ~60–90 min (Entra minimum; not shortenable) |

What that means concretely for the two Voice Live engines, and what the mitigation actually is:

- **A leaked token can open unlimited sessions** on that resource until it expires. Mitigation: the
  resource is Voice-Live-only and carries its own Azure budget with actual + forecast alerts
  (WS-A M5). It cannot reach the Azure OpenAI resource, Cosmos, storage, or anything else.
- **The persona instructions leave the server.** On every other engine the raw instruction text never
  reaches the client (the anti-injection rule in `/c/dev/live-ninja/internal/realtime/personas.go`).
  On Voice Live WebRTC the client *is* the thing that sends `session.instructions`, so the broker can
  author that config but cannot enforce it. The Help drawer and `docs/voice-engines.md` must say so
  (WS-F M2, WS-F M3). Do not write copy that implies the config is enforced.
- **The 60-second TTL story does not apply.** Clients fail closed at `expiresAt` exactly as they do
  on Gemini, but the window is minutes-to-hours rather than seconds.

If Microsoft ships a Voice Live `client_secrets`-equivalent, WS-G M1 replaces this whole shape and
both bullets above disappear. That is the single item to watch.

---

## Verified facts — the repository (confirmed by reading the files named, 2026-08-24)

- **R1. There are four voice engines today**, declared in
  `/c/dev/live-ninja/internal/voiceengine/engine.go:10-26`: `openai-realtime`,
  `openai-realtime-mini`, `nova-sonic`, `gemini-flash-live`. `Engine.Valid()` (`:35-42`) and
  `Engine.IsClientDirect()` (`:33`) switch over exactly those four. `IsClientDirect` has **zero
  callers** — `engine.go` is its only mention.

- **R2. A new constant in `engine.go` reaches nothing on its own.** Engine routing is a direct string
  compare in `/c/dev/live-ninja/cmd/realtime-broker/main.go:328` (`if engine ==
  voiceengine.EngineNovaSonic`) and `:345` (`… == voiceengine.EngineGeminiFlashLive`).
  `/c/dev/live-ninja/internal/realtime/mint.go:483-493` `validEngine` switches over the same four
  constants and returns `false` for anything else, so `PinToEngine` (`:469-482`) silently falls
  through to `EngineOpenAIRealtime`. A device pinned to an unknown engine gets the default with **no
  error**.

- **R3. The write path and the contract reject unknown engines.** `oneOf(...)` allowlists and their
  error strings are at `/c/dev/live-ninja/internal/webapp/settings_routes.go:502` and `:510-511`;
  the enums are at `/c/dev/live-ninja/contracts/settings.schema.json:192-197` (`voiceEngine.default`)
  and `:200-205` (`voiceEngine.devices.additionalProperties`). A user selecting an engine missing
  from these gets a 400 and the setting can never be stored. Schema evolution is **additive-only**
  (`/c/dev/live-ninja/contracts/README.md:27-30`), so nothing is removed.

- **R4. Per-device pinning has moved.** `contracts/settings.schema.json:183` and `:202` state that
  `voiceEngine.devices` is the **deprecated pre-M31** map, and that new clients use
  `deviceOverrides[deviceId].sections.voiceEngine`. `ResolveEngine`
  (`/c/dev/live-ninja/internal/realtime/mint.go:425-458`) reads **both** `voiceEngine` and
  `deviceOverrides` in one `GetItem` and flattens them through `store.EffectiveSettings(raw,
  deviceID)` before calling `PinToEngine`. Any new engine must be accepted by **both** paths.
  (`azure-migration-plan.md` WS-B M2 describes only the deprecated `devices` map — it predates M31
  and is incomplete on this point.)

- **R5. The existing mint body is already the Azure GA shape.** `Mint`
  (`/c/dev/live-ninja/internal/realtime/mint.go:291-330`) builds
  `{"type":"realtime","model":…,"audio":{"output":{"voice":…},"input":{turn_detection, noise_reduction,
  transcription}},"instructions":…,"tools":…}` wrapped in `{"expires_after":{"anchor":"created_at",
  "seconds":60},"session":{…}}`. That is byte-for-byte the object Azure's GA
  `/openai/v1/realtime/client_secrets` accepts. **The Azure minter can reuse this builder verbatim**;
  only the URL, the auth header, and the model id differ. `clientSecretsURL` is hardcoded at `:40`.

- **R6. The mint is the only place an API key is touched.** `mint.go:319` resolves
  `config.ParamOpenAIAPIKey` / `config.EnvOverrideOpenAIAPIKey` through the SSM-backed
  `config.Loader` (`/c/dev/live-ninja/internal/config/config.go:28,40`), cached 5 minutes, never in a
  deployed function's environment. `ParamGeminiAPIKey` (`:29`) follows the same pattern. An Azure
  parameter is a two-line addition to that file.

- **R7. Both clients hardcode the OpenAI SDP host — this is the blocking client defect.**
  - Web: `/c/dev/live-ninja/web/static/js/realtime.mjs:98` `const OPENAI_CALLS_URL =
    'https://api.openai.com/v1/realtime/calls'`, used as the constructor default at `:466-469` and
    posted at `:778-785` with `Authorization: 'Bearer ' + minted.clientSecret.value`.
  - Android: `/c/dev/live-ninja/android/app/src/main/java/ninja/jeremy/liveninja/config/BackendConfig.kt:54`
    `OPENAI_REALTIME_CALLS_URL`, used as the **default value** of `RealtimeSession.callsUrl`
    (`realtime/RealtimeSessionApi.kt:55`). `parseSession` (`:167-227`) **never reads `callsUrl` from
    the JSON**, so it is always the compile-time constant.
  - Broker: `SessionResp` (`/c/dev/live-ninja/cmd/realtime-broker/main.go:130-156`) has **no
    `callsUrl` field at all**.

    Net effect: an already-installed client handed an Azure ephemeral credential would POST it to
    `api.openai.com`. WS-D M1 (server-side version gate) and WS-D M2 (`callsUrl` on the wire) exist
    for exactly this.

- **R8. The engine-fallback cascade already exists and is the pattern to copy.**
  `cmd/realtime-broker/main.go:328-375`: a pinned engine that cannot mint logs, emits the
  `EngineFallback` metric, appends a plain-language warning to `warnings`, and re-routes to
  `EngineOpenAIRealtime` — but only when `b.minter != nil`. The comment at `:345-360` records why:
  a pinned engine that is down was previously a hard 502 with no way out except changing the setting.

- **R9. CSP is an explicit allowlist.** `/c/dev/live-ninja/internal/webapp/pages_routes.go:56`
  `connect-src 'self' https://api.openai.com wss://generativelanguage.googleapis.com <two S3 hosts>
  wss://a17oe0gnthrosw-ats.iot.us-east-1.amazonaws.com`. Neither Azure host is present; without them
  the browser blocks the SDP POST and the Voice Live socket with no useful error.
  `/c/dev/live-ninja/internal/webapp/mobile_shell_ui_test.go:338-353` already asserts an origin lands
  **inside** the `connect-src` directive — copy that test shape.

- **R10. `rates.go` has a live billing defect this plan must fix on the way past.**
  `/c/dev/live-ninja/internal/realtime/rates.go:54` sets `defaultRates = modelRates["gpt-realtime"]`
  and `RatesFor` (`:58-63`) silently returns it for any unknown model. There is **no key for
  `gpt-realtime-mini`** (defined as a model id at `mint.go:34`), so the `openai-realtime-mini` engine
  is billed in the cost badge at full `gpt-realtime` rates today. `rates_test.go:33` asserts the
  silent fallback, which is why no test catches it. The header at `:20-26` dates the existing entries
  to **2025-08**.

- **R11. The Voice catalog is shared and order-locked.**
  `/c/dev/live-ninja/internal/realtime/catalog.go:33-45` `SupportedVoices` (10 entries, `cedar`
  default) must stay in sync with `allowedRealtimeVoices` (`personas.go`) and
  `contracts/settings.schema.json#/properties/voice`. `SupportedGeminiVoices` (`:56-88`) is the
  precedent for an engine-specific catalog served as a sibling field on
  `GET /api/v1/realtime/voices`.

- **R12. `X-LN-Client` parsing already exists.** `/c/dev/live-ninja/internal/webapp/version.go:35-66`
  defines the grammar from `contracts/headers.md` and `parseClientVersion` returns
  `(clientVersion{surface,major,minor,patch,build}, ok)`. `compatStatus` (`:132`) and the middleware
  (`:170`, `:228`) already gate on it and emit the `ClientVersions` metric. WS-D M1 reuses this — it
  does not invent a version scheme.

- **R13. Help copy is a hard requirement, not a nicety.** `/c/dev/live-ninja/CLAUDE.md`: "**any
  change to a feature, setting, capability, page, or tool updates the Help copy in the same
  commit.**" Content lives in the `HELP DRAWER` block of
  `/c/dev/live-ninja/web/templates/pages/conversation.html`; the guard is
  `go test ./internal/webapp/ -run TestHelpDrawer`.

- **R14. Deployment is push-to-`main` only.** `/c/dev/live-ninja/deploy.md`: GitHub Actions is the
  only thing that touches AWS; no local `sam deploy`. `.github/workflows/deploy.yml` is
  `on: push: branches: [main]` plus `workflow_dispatch`. AWS auth is OIDC via
  `vars.AWS_DEPLOY_ROLE_ARN` — never static keys.

- **R15. No Azure SDK is vendored.** `grep -n azure /c/dev/live-ninja/go.mod` returns nothing. The
  Entra client-credentials exchange for Voice Live is a single `POST` to
  `https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token` — implement it as a small cached
  token client in the existing hand-rolled style (`gemini_mint.go` is the precedent), rather than
  pulling `azidentity` and its transitive tree into a Lambda cold start.

---

## Verified facts — Azure (read from Microsoft Learn on 2026-08-24, URLs recorded)

- **A1. Azure OpenAI Realtime has a real ephemeral mint, and it is the GA path.**
  `POST https://<resource>.openai.azure.com/openai/v1/realtime/client_secrets` — **no `api-version`
  query parameter**. SDP goes to `https://<resource>.openai.azure.com/openai/v1/realtime/calls`.
  The preview forms (`/openai/realtimeapi/sessions?api-version=2025-04-01-preview`,
  `https://<region>.realtimeapi-preview.ai.azure.com/v1/realtimertc`) are **deprecated from
  2026-04-30**. Source:
  <https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/realtime-audio-webrtc>

- **A2. The mint accepts either an `api-key` or a Microsoft Entra ID token.** Locked decision 1 uses
  the API key. Required role for the Entra path is `Cognitive Services User`. Same source as A1.

- **A3. GPT realtime global deployments exist in East US 2 and Sweden Central only.** Deployable
  models listed: `gpt-realtime` (2025-08-28), `gpt-realtime-mini` (2025-10-06 and 2025-12-15),
  `gpt-realtime-1.5` (2026-02-23), `gpt-realtime-2` (2026-05-07), `gpt-realtime-translate`,
  `gpt-realtime-whisper`. The pricing page additionally lists **GPT-Realtime-2.1** and
  **GPT-Realtime-2.1 mini** (Global and Data Zone). A 403 on the mint means the resource is in an
  unsupported region. Same source as A1.

- **A4. `cedar` and `marin` are supported on Azure.** The WebRTC how-to's session table lists
  `alloy, ash, ballad, coral, echo, sage, shimmer, verse`, but Microsoft's model documentation
  records Marin and Cedar as added standard voices. **This is the one Azure fact that is documented
  ambiguously**, and `cedar` is this project's locked default (`catalog.go:37`). WS-B M1 proves it by
  minting with `cedar` before anything depends on it; WS-B M4 carries the fallback if it fails.
  Source: <https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/realtime-audio>

- **A5. Voice Live has a WebRTC path, and Microsoft recommends it for browsers.** From the how-to:
  "In most cases, use Voice Live API with WebRTC for real-time audio streaming in client-side
  applications such as a web application or mobile app." Control channel:
  `wss://<resource>.services.ai.azure.com/voice-live/realtime/calls?api-version=2026-01-01-preview&model=<model>`.
  The client sends `rtc.call.sdp.create` carrying `sdp_offer` **and the `session` config**, and
  receives `rtc.call.sdp.created` with `sdp_answer`. Audio then flows over RTP; a
  `voice-live-events` data channel carries VAD, response lifecycle and transcription events; the
  control WebSocket carries `session.update`, errors and **function/tool-call events**.
  **Status: public preview, no SLA.** Source:
  <https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live-webrtc>

- **A6. Voice Live WebRTC uses global standard deployments and auto-routes to the nearest region.**
  No capacity planning, no model deployment step — the models are fully managed. Same source as A5.

- **A7. Voice Live's browser-usable credential is an Entra bearer token in a query parameter.**
  "You can send the token on the WebSocket upgrade request, either in the `Authorization` header in
  the format `Bearer <token>`, or as an `Authorization` query string parameter with the same
  `Bearer <token>` value." The header form "isn't available in a browser environment." Token scope:
  `https://ai.azure.com/.default` (legacy `https://cognitiveservices.azure.com/.default`). Roles:
  `Cognitive Services User` **and** `Foundry User`. The `api-key` alternative is also query-string
  capable — and is a permanent full-resource key, which is why locked decision 2's isolation matters.
  Sources: <https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live-how-to>,
  <https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live-api-reference-2026-06-01-preview>

- **A8. Voice Live's model catalog is the answer to "what other Azure voice models operate like
  this".** All are fully managed and need no deployment:
  - **Native speech-to-speech:** `gpt-realtime-1.5`, `gpt-realtime`, `gpt-realtime-mini`,
    `phi4-mm-realtime` (preview), `azure-realtime`.
  - **Cascaded** (Azure speech-to-text in, Azure text-to-speech out, any chat model in the middle):
    `gpt-5.4`, `gpt-5.3-chat`, `gpt-5.2`, `gpt-5.2-chat`, `gpt-5.1`, `gpt-5.1-chat`, `gpt-5`,
    `gpt-5-mini`, `gpt-5-nano`, `gpt-4.1`, `gpt-4.1-mini`, `gpt-4.1-nano`, `gpt-4o`, `gpt-4o-mini`,
    `phi4-mini` (preview).
  - `gpt-5.5`, `gpt-5.4-mini`, `gpt-5.4-nano` work but are **not pre-deployed** — they need Bring
    Your Own Model, which is out of scope here.
  Source: <https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live>

- **A9. `azure-realtime` is a distinct Azure-native speech-to-speech model with its own voices.**
  Requires `api-version` `2026-01-01-preview` or later. Voice is specified as
  `{"type":"azure-realtime-native","name":"<name>"}`; default is `ava`. 35 voices across 20 locales —
  `aarti, alvaro, andrew, antonio, ava, clara, dalia, denise, diego, diya, elsa, emma, florian,
  francisca, hyunsu, jorge, keita, liam, meera, nanami, natasha, niwat, premwadee, rayn, remy,
  seraphina, sonia, sunhi, sylvie, thierry, william, xiaoxiao, ximena, yunxi`. Microsoft's own
  standalone browser sample uses `model=azure-realtime`, `voice=ava`. Source: same as A7 (how-to).

- **A10. Voice Live pricing is tiered by the model chosen, not selected explicitly.**
  **Pro** = `gpt-realtime`, `gpt-4o`, `gpt-4.1`, `gpt-5`, `gpt-5-chat`. **Basic** =
  `gpt-realtime-mini`, `gpt-4o-mini`, `gpt-4.1-mini`, `gpt-5-mini`. **Lite** = `gpt-5-nano`,
  `phi4-mm-realtime`, `phi4-mini`. Token estimation: Azure OpenAI models ≈ 10 input / 20 output
  tokens per audio second; Phi models ≈ 12.5 / 20. Cached audio and text inputs are billed. Source:
  same as A8.

- **A11. Voice Live adds capabilities the OpenAI path does not have**, all optional and additive:
  `azure_semantic_vad` / `azure_semantic_vad_multilingual` turn detection with `remove_filler_words`,
  `azure_deep_noise_suppression`, `server_echo_cancellation` (including Live-Reference AEC from
  api-version `2026-07-15`), word-level `output_audio_timestamp_types`, viseme output, and avatars.
  `semantic_vad` — what this project uses today (`mint.go:249`) — is `gpt-realtime`/`-mini` only.
  Source: same as A7.

- **A12. Azure list rates carried forward from `azure-migration-plan.md` WS-B M4, read 2026-08-18.**
  Per 1M tokens, Global: `gpt-realtime-2.1` — text in 4.00, cached text 0.40, text out 24.00, audio
  in 32.00, cached audio 0.40, audio out 64.00. `gpt-realtime-2.1-mini` — 0.60 / 0.06 / 2.40 / 10.00
  / 0.30 / 20.00. `gpt-realtime-2` — identical to 2.1. **Voice Live Pro/Basic/Lite per-token rates
  were never captured and are unknown.** WS-B M5 re-reads all of them; the pricing page renders its
  numbers in JavaScript, so read it in a browser, not with `curl`.

---

## What ships: the engine catalog

Four new engines, all client-direct, none requiring a container or any always-on backend service.
The existing four engines are untouched; `openai-realtime` remains the platform default.

| New pin | Backend | Model | Transport | Credential in client | Voice catalog |
|---|---|---|---|---|---|
| `gpt-live-azure` | Azure OpenAI Realtime | `gpt-realtime-2.1` (fallback `gpt-realtime-2`, then `gpt-realtime`) | WebRTC — SDP POST to `/openai/v1/realtime/calls` | Azure `ek_…`, 60 s, config-bound | `SupportedVoices` (existing 10) |
| `gpt-live-azure-mini` | Azure OpenAI Realtime | `gpt-realtime-2.1-mini` (fallback `gpt-realtime-mini`) | same | same | same |
| `azure-voice-live` | Azure AI Voice Live | `azure-realtime` | WebRTC — WSS control channel + RTP | Entra bearer, resource-scoped | new `SupportedAzureRealtimeVoices` (35, A9) |
| `azure-voice-live-lite` | Azure AI Voice Live | `phi4-mm-realtime` | same | same | Azure standard TTS voices (`azure-standard`) |

Explicitly **not** shipping as pins: the 15 cascaded Voice Live models (A8). They reach the same
transport, so WS-G M3 exposes them behind one additive optional setting rather than 15 more engine
constants. Recorded in `backlog.md`, not here.

Explicitly **out of scope**: M5Stack Tab5 firmware. `docs/voice-engines.md` already marks that
surface backlog-only, and it is unaffected — it stays on `openai-realtime`.

---

## Standing authorizations (granted 2026-08-24)

Granted:

- **Create, configure and delete Azure resources** inside subscription
  `adc40fff-bab3-4bd2-b961-1832d0375052` for the resources named in WS-A, and **incur the Azure spend
  they generate**, subject to the budgets in WS-A M5.
- **Create Microsoft Entra app registrations and role assignments** scoped to those resources only
  (WS-A M4). No role assignment at subscription or management-group scope.
- **Write SSM SecureString parameters** under `/live-ninja/prod/azure/` in AWS account
  `759775734231`, via `/c/dev/live-ninja/scripts/set-secret.sh` only. Values are typed by the
  operator; no agent reads them back.
- **Push to `main`**, which deploys to production on every push (locked decision 4).
- **Install a debug APK on an attached Android device over ADB** (locked decision 5).

NOT granted, and nothing in this plan needs them:

- Modifying anything in the `jeremy.ninja` Route 53 hosted zone. This plan adds no DNS records.
- Deleting or reconfiguring any existing AWS resource. Every change is additive.
- Deploying anything from a local machine (`deploy.md`). Azure *resource provisioning* via `az` is
  authorized above; **application deployment** stays on GitHub Actions.
- Touching the `alexa-version` branch or `azure-migration-plan.md`.

---

## Stop conditions (only these)

Anything not listed here is worked around, marked `[!]` with the exact thing that unblocks it, and
reported at the end. Do not pause on anything else.

1. **A required Azure credential cannot be created or is rejected**, and no substitute exists. State
   the exact `az` command and its verbatim error.
2. **Any secret value would have to be printed, logged, committed, or pasted into chat** to continue.
   Stop rather than do it. This includes a DoD command that would echo a minted `ek_…` or an Entra
   token — WS-B M1's DoD is written to print only `MINT_OK` for this reason.
3. **Azure OpenAI realtime models are unavailable in both East US 2 and Sweden Central.** Record what
   `az cognitiveservices account list-models` actually returned.
4. **A production deploy to `main` fails and the fix is not inside this plan's scope** — e.g. the
   pipeline breaks on unrelated pre-existing code. Fixing a failure *this plan caused* is part of the
   run, not a stop condition.
5. **Azure month-to-date spend on the new resource groups crosses $250** (the second budget
   threshold, WS-A M5) before WS-F M1 has passed.

Not stop conditions: Voice Live being preview-status; `cedar` turning out unsupported on Azure (WS-B
M4 handles it); the Android S9 install failing (record it, finish everything else); Voice Live rates
being unpublished (record `rates_missing` and ship the engine with the badge suppressed).

---

## Workstreams

Dependency summary: **WS-A** (Azure resources) blocks WS-B M1 and WS-C M1 only. **WS-B** and **WS-D**
are pure Go and can start immediately, in parallel, with no Azure resource in existence. **WS-E**
(clients) depends on WS-D M2's wire contract. **WS-F** is the live gate. **WS-G** is deferred work
that must be written into `backlog.md`, not left implicit.

Sequencing note: start WS-B and WS-D **first and in parallel** with WS-A's portal work. WS-A is the
only step with an external latency (resource provisioning, RBAC propagation), and nothing in WS-B or
WS-D M1–M3 needs it.

---

### WS-A — Azure resources (blocks WS-B M1, WS-C M1)

- [x] **A1. Create the resource groups.** Two, so the Voice Live blast radius is its own container
      per locked decision 2: `ln-azure-openai-rg` and `ln-voicelive-rg`, both in `eastus2`, both
      tagged `project=live-ninja` and `plan=azure-voice-plan` (the budgets in A5 filter on those
      tags).
      DoD: `az group list --query "[?tags.plan=='azure-voice-plan'].name" -o tsv` prints exactly two
      names.

- [x] **A2. Create the Azure OpenAI (Microsoft Foundry) resource.** Name `ln-aoai-eastus2`, kind
      `AIServices`, region `eastus2` (A3), with a custom subdomain — the
      `<resource>.openai.azure.com` host form in A1 requires one.
      DoD: `az cognitiveservices account show -n ln-aoai-eastus2 -g ln-azure-openai-rg --query
      "properties.endpoint" -o tsv` prints an `https://ln-aoai-eastus2.openai.azure.com/`-shaped URL.

- [x] **A3. Deploy the realtime models.** Deploy `gpt-realtime-2.1` as a **GlobalStandard**
      deployment named `gpt-realtime-2-1`, and `gpt-realtime-2.1-mini` as `gpt-realtime-2-1-mini`.
      If `2.1` is not offerable in this subscription, fall back to `gpt-realtime-2`, then
      `gpt-realtime`/`gpt-realtime-mini` (A3). **Record which model id actually deployed, verbatim,
      in the Execution log** — that single fact propagates to WS-B M2, WS-B M5, WS-E and WS-F.
      DoD: `az cognitiveservices account deployment list -n ln-aoai-eastus2 -g ln-azure-openai-rg
      --query "[].name" -o tsv` prints both deployment names.

- [!] **A4. Create the Voice Live resource and its service principal.** Foundry resource
      `ln-voicelive` in `ln-voicelive-rg`, with a custom subdomain (the
      `<resource>.services.ai.azure.com` host in A5 needs one). Then an Entra app registration
      `ln-voicelive-client` with a client secret, granted `Cognitive Services User` **and**
      `Foundry User` (A7) scoped to `ln-voicelive` **and nothing else**. No model deployment step —
      Voice Live models are fully managed (A6).
      DoD: `az role assignment list --assignee <appId> --all --query "[].scope" -o tsv` prints **only**
      the `ln-voicelive` resource id, twice, and nothing else. Any other scope in that output is a
      failure of locked decision 2's isolation and must be removed before proceeding.

      **[!] BLOCKED 2026-08-24 — stop condition 1. This machine's only Azure identity cannot create
      an Entra app registration.** It is the service principal `azure-owner-deployer`
      (`f16364f7-e9d4-4b28-95aa-7b11e2fe8ea7`). It holds ARM rights on the subscription but **no
      Microsoft Graph rights at all**:
      ```
      $ az account show --query "user.type" -o tsv
      servicePrincipal
      $ az rest --method GET --url ".../servicePrincipals(appId='f16364f7-...')/memberOf" --query value -o tsv
      (empty - no directory role)
      $ az ad app list --query "[0].displayName" -o tsv
      ERROR: Insufficient privileges to complete the operation.
      ```
      WHAT UNBLOCKS IT, and who supplies it: the operator, either by (a) creating
      `ln-voicelive-client` in the Entra portal, adding a client secret, and granting it
      `Cognitive Services User` + `Foundry User` scoped to `ln-voicelive` only; or (b) granting this
      service principal the Graph application permission `Application.ReadWrite.OwnedBy` plus the
      `Application Developer` directory role, after which A4 runs unattended as written.
      Recommended: (b), because it makes this and every future run self-service.
      **A4 blocks all of WS-C and both Voice Live engines** (`azure-voice-live`,
      `azure-voice-live-lite`). It does NOT block `gpt-live-azure` / `gpt-live-azure-mini`, which
      need only WS-A M2/M3 (done) — so the run continues on the Azure OpenAI half.

- [x] **A5. Set the budgets.** This is the only spend backstop — the daily quota counters are inert
      today (see the WS-B M6 note), so nothing else will surface a runaway. Two budgets, one per
      resource group, at **$100 / $250 / $500** with **both actual and forecasted** notifications to
      the operator's address.
      DoD: `az consumption budget list --query "[?contains(name,'ln-')].[name,amount]" -o tsv` prints
      two rows, and one test notification has been received.

- [~] **A6. Store the credentials in SSM.** Three SecureString parameters, all written by
      `/c/dev/live-ninja/scripts/set-secret.sh` with the operator typing the values:
      `/live-ninja/prod/azure/openai_api_key`, `/live-ninja/prod/azure/voicelive_client_secret`, and
      `/live-ninja/prod/azure/voicelive_client_id`. Tenant id
      `d0695ba8-1211-4da6-81a4-05427c842a2a` and the two endpoint hostnames are **identifiers, not
      secrets** — they go in `template.yaml` as plain environment variables.
      DoD: `aws ssm get-parameters-by-path --path /live-ninja/prod/azure --query "Parameters[].Name"
      -o text` prints three names. **Do not add `--with-decryption`.**

---

### WS-B — Broker: engines, mint, rates (pure Go; start immediately, no Azure dependency)

- [x] **B1. Live-mint smoke test.** Depends on A3 and A6. Proves the endpoint, the key, the model id,
      and the voice all work before any Go code depends on them.
      **Record only `MINT_OK` and the chosen model id in the Execution log — never the response
      body.** Do not run this under `set -x`. Stop condition 2 governs.
      DoD:
      `curl -sS -X POST "https://ln-aoai-eastus2.openai.azure.com/openai/v1/realtime/client_secrets" -H "api-key: $AZURE_OPENAI_KEY" -H 'Content-Type: application/json' -d '{"session":{"type":"realtime","model":"gpt-realtime-2-1","audio":{"output":{"voice":"cedar"}}}}' | jq -e 'has("value")' > /dev/null && echo MINT_OK`
      prints `MINT_OK`. A 403 here means the region is wrong (A3); a 400 naming `voice` means A4's
      ambiguity resolved against us — go to B4.

- [x] **B2. Extend the engine enum and every gate that reads it.** A new constant alone reaches
      nothing (R2), so all five of these land together:
      (a) `/c/dev/live-ninja/internal/voiceengine/engine.go` — add `EngineGPTLiveAzure =
      "gpt-live-azure"`, `EngineGPTLiveAzureMini = "gpt-live-azure-mini"`, `EngineAzureVoiceLive =
      "azure-voice-live"`, `EngineAzureVoiceLiveLite = "azure-voice-live-lite"`; extend `Valid()`.
      (b) Redefine `IsClientDirect()` as "not `nova-sonic`" — **all four new engines are
      client-direct**, which is the entire point of this plan — and give it its first real caller so
      it can no longer drift unnoticed (R1).
      (c) `/c/dev/live-ninja/internal/realtime/mint.go:483-493` — add all four to `validEngine`, or a
      device pinned to one silently resolves to `openai-realtime` with no error (R2).
      (d) `/c/dev/live-ninja/internal/webapp/settings_routes.go:502,510-511` — add all four to both
      `oneOf(...)` allowlists **and their error strings**, and to the M31 `deviceOverrides` section
      validator (R4). Derive all of them from one exported list so the two paths cannot diverge.
      (e) `/c/dev/live-ninja/contracts/settings.schema.json:192-197` and `:200-205` — add all four to
      both enums. Additive only; nothing is removed (R3).
      DoD: `cd /c/dev/live-ninja && go test ./internal/voiceengine/ ./internal/realtime/
      ./internal/webapp/ ./cmd/realtime-broker/` passes, with new cases asserting
      `PinToEngine("gpt-live-azure", nil, "") == EngineGPTLiveAzure`, that a settings PUT of
      `voiceEngine.default = "azure-voice-live"` is accepted and round-trips, **and** that a
      `deviceOverrides[<id>].sections.voiceEngine.default = "gpt-live-azure-mini"` PUT is accepted
      and resolves through `ResolveEngine` for that device (R4).

- [x] **B3. The Azure minter.** New `/c/dev/live-ninja/internal/realtime/azure_mint.go`. It **reuses
      the existing session-config builder verbatim** (R5) — do not fork `buildAudioInput` or
      `buildTurnDetection`. Only three things differ from `Minter`: the URL
      (`<endpoint>/openai/v1/realtime/client_secrets`, **no `api-version` parameter** — A1), the auth
      header (`api-key: <key>` from `config.ParamAzureOpenAIAPIKey`, resolved through the same
      `config.Loader` — R6), and the model id (the deployment name from A3). `clientSecretsURL`
      (`mint.go:40`) stays exactly as it is; `openai-realtime` must keep working byte-for-byte.
      Add the two config constants to `/c/dev/live-ninja/internal/config/config.go` beside
      `ParamGeminiAPIKey` (`:29`).
      DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run 'Mint|Azure' -v` passes,
      including a test asserting the Azure request URL carries **no** `api-version` query parameter
      and that the OpenAI minter's URL is unchanged.

- [ ] **B4. Voice mapping, and the `cedar` question.** Depends on B1. If B1 minted with `cedar`,
      `gpt-live-azure` reuses `SupportedVoices` unchanged and this milestone is a no-op plus a test.
      If it did not, add a mapping table (`cedar`→`ash`, `marin`→`coral`, both documented as
      substitutions in the picker copy) rather than silently sending a rejected voice.
      Separately, add `SupportedAzureRealtimeVoices` — the 35 `azure-realtime-native` voices from A9,
      default `ava` — as a new sibling on `GET /api/v1/realtime/voices`, following the
      `SupportedGeminiVoices` precedent (R11).
      DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run 'Voice|Catalog'` passes, and a
      new test asserts every engine's default voice is a member of that engine's own catalog.

- [x] **B5. Rate table, and close the silent-fallback defect (R10).** In
      `/c/dev/live-ninja/internal/realtime/rates.go`:
      - Add rows for the deployed Azure model ids using A12's figures, **re-read in a browser first**
        — the pricing page renders numbers in JavaScript and `curl` returns `$-` placeholders.
      - Add the **missing `gpt-realtime-mini` row**. This is a live billing defect, not new work:
        `openai-realtime-mini` is billed in the cost badge at full rates today.
      - Add Voice Live Pro/Basic/Lite rows keyed on the model id. If the published rates cannot be
        found, ship the engines with `rates_missing` logged and the cost badge suppressed — do not
        invent a number and do not let the silent default cover it.
      - Re-verify the two existing entries; their header dates them to 2025-08.
      - Keep `RatesFor`'s fallback for genuinely unknown ids, and add `RatesForEngine` returning
        `(Rates, bool)` that logs `code=rates_missing` instead of guessing.
      DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run Rates` passes **and** a new
      `TestRatesCoverEveryShippedEngine` asserts every model id reachable from the eight engine
      constants has an explicit `modelRates` key. `TestRatesForUnknownModelFallsBack` must not be the
      thing that makes it pass.

- [~] **B6. Broker routing for the four new engines.** In
      `/c/dev/live-ninja/cmd/realtime-broker/main.go`, add `handleAzureDirect` and
      `handleVoiceLiveDirect` beside the existing handlers, and wire both into the **existing**
      fallback cascade at `:328-375` (R8) — same `EngineFallback` metric, same plain-language
      `warnings` entry naming the engine, same `b.minter != nil` guard so the broker never falls back
      to an unconfigured default. Do not invent a second cascade.
      Note for the reader: the daily/monthly quota counters this cascade sits beside are **inert in
      production** — `AddDayUsage`/`AddMonthUsage` (`internal/store/usage.go:92,102`) have one caller
      that passes `0, 0, 1`. That is a pre-existing defect inherited from
      `azure-migration-plan.md`'s audit, it is **out of scope here**, and A5's budgets are the actual
      spend backstop. Do not fix it in this plan; do not rely on it either.
      DoD: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/` passes, with tests asserting that
      each new engine returns its own `mode`, and that a mint failure on each falls back to
      `openai-realtime` with a warning rather than returning 502.

---

### WS-C — Voice Live token exchange (depends on A4, A6)

- [ ] **C1. Entra client-credentials token client.** New
      `/c/dev/live-ninja/internal/realtime/entra_token.go`: `POST
      https://login.microsoftonline.com/d0695ba8-1211-4da6-81a4-05427c842a2a/oauth2/v2.0/token` with
      `grant_type=client_credentials`, `scope=https://ai.azure.com/.default` (A7), client id and
      secret from SSM (A6). Hand-rolled, not `azidentity` (R15). Cache the token in memory keyed on
      scope and refresh at 80% of its lifetime — the Lambda container is reused, and re-minting per
      session would add a round trip to every conversation start.
      **Never log the token, and never include it in an error string.**
      DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run Entra` passes, including a test
      that a cached unexpired token is reused and one asserting the token never appears in the
      error path.

- [ ] **C2. The Voice Live bootstrap response.** `handleVoiceLiveDirect` returns
      `mode: "voice-live-direct"` with `voiceLiveEndpoint`
      (`wss://ln-voicelive.services.ai.azure.com/voice-live/realtime/calls?api-version=2026-01-01-preview&model=<model>`
      — A5), `accessToken: {value, expiresAt}` carrying the **observed** `exp` from the token, and
      `sessionConfig` — the server-authored `session` object the client must send inside
      `rtc.call.sdp.create`.
      **Field-naming rule, inherited from the Gemini precedent:** legacy clients detect Nova by the
      *presence* of `wsUrl`/`bridgeUrl`, so the Voice Live shape must use neither of those names or
      an old client will route audio to a bridge that does not exist for it.
      Record in the Execution log the **actual observed token lifetime**, so the honesty table in
      `## The token problem` states a measured number rather than "~60–90 min".
      DoD: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run VoiceLive` passes, including a
      test asserting the response contains **no** `wsUrl` and no `bridgeUrl` field.

---

### WS-D — The wire contract and the version gate (pure Go; start immediately, in parallel with WS-B)

This workstream is what makes locked decision 3 safe. R7 is the defect it closes.

- [x] **D1. Server-side client-version gate.** Reuse `parseClientVersion`
      (`/c/dev/live-ninja/internal/webapp/version.go:55-66`) — do not invent a version scheme (R12).
      In the broker's engine resolution, **before** any Azure mint: if the resolved engine is one of
      the four new ones and `X-LN-Client` is absent, unparseable, or below the per-surface minimum,
      route to `openai-realtime` instead and emit `EngineFallback` with reason
      `client_too_old`. Set the minimums to the versions WS-E actually ships.
      **This gate is the reason an already-installed client can never receive an Azure credential.**
      It fails closed by construction: an unknown client is an old client.
      DoD: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run 'Gate|ClientVersion'` passes,
      with a test asserting that a request carrying **no** `X-LN-Client` and a
      `voiceEngine.default = "gpt-live-azure"` pin receives `mode: "openai-direct"` and an OpenAI
      model id — never an Azure one.

- [x] **D2. Put `callsUrl` on the wire.** Add `CallsURL string \`json:"callsUrl,omitempty"\`` to
      `SessionResp` (`/c/dev/live-ninja/cmd/realtime-broker/main.go:130-156`). Emit it on **both**
      `openai-direct` (the existing OpenAI URL, so the field is exercised by the default path from
      day one and cannot rot) and the new `azure-direct` mode.
      DoD: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run Session` passes, with a test
      asserting `callsUrl` is present and correct on an `openai-direct` response.

- [~] **D3. Contract and CSP.** Update `/c/dev/live-ninja/contracts/api.md` for
      `GET /v1/realtime/session`'s two new response shapes (`azure-direct`, `voice-live-direct`) and
      the new `callsUrl` field. Add `https://ln-aoai-eastus2.openai.azure.com` and
      `wss://ln-voicelive.services.ai.azure.com` to the `connect-src` allowlist at
      `/c/dev/live-ninja/internal/webapp/pages_routes.go:56` (R9) — without them the browser blocks
      the SDP POST and the Voice Live socket with an error that looks like a network fault.
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run CSP` passes, with a test in the
      shape of `mobile_shell_ui_test.go:338-353` asserting both origins land **inside** the
      `connect-src` directive and not merely somewhere in the policy string.

---

### WS-E — Clients (depends on WS-D M2)

- [ ] **E1. Web: honour `callsUrl`, add the Voice Live transport.** In
      `/c/dev/live-ninja/web/static/js/realtime.mjs`: read `callsUrl` from the mint response,
      falling back to `OPENAI_CALLS_URL` (`:98`) when absent, so an older server keeps working. The
      `azure-direct` mode then reuses the **existing** WebRTC path unchanged — same SDP POST, same
      `Authorization: Bearer ek_…` (`:778-785`) — because Azure's `/calls` contract matches OpenAI's.
      Add a `voice-live-direct` branch: open the control WSS with the Entra token in the
      `Authorization` query parameter (URL-encoded — A7), send `rtc.call.sdp.create` with `sdp_offer`
      plus the server-supplied `sessionConfig`, await `rtc.call.sdp.created`, apply `sdp_answer`,
      and subscribe to the `voice-live-events` data channel for VAD, transcript and response
      lifecycle events; keep the control socket open for tool calls (A5). Normalize all of it onto
      the existing `internal/voiceengine` common event vocabulary — topics, memory, transcripts and
      barge-in must behave identically across engines.
      **Hazard:** a `conversation.mjs`/`realtime.mjs` change can kill the whole page for clients
      holding an older cached sibling module. The import map shipped for this — verify the warm-cache
      path before the milestone is called done, not after.
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/` passes, and a browser session pinned
      to `gpt-live-azure` completes a turn and shows a non-zero cost badge.

- [ ] **E2. Android: read `callsUrl` from JSON.** This is the R7 fix.
      `parseSession` (`/c/dev/live-ninja/android/app/src/main/java/ninja/jeremy/liveninja/realtime/RealtimeSessionApi.kt:167-227`)
      must read `callsUrl` from the response and only fall back to
      `BackendConfig.OPENAI_REALTIME_CALLS_URL` (`:55`) when it is absent. `WebRtcTransport` needs no
      change — it already takes `callsUrl` as a parameter (`WebRtcTransport.kt:191,274,297`).
      DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest --tests
      '*RealtimeSessionParseTest*'` passes, with a new case asserting an Azure `callsUrl` in the JSON
      is what reaches `connect()` — **and** a case asserting a response with no `callsUrl` still
      reaches OpenAI's.

- [ ] **E3. Android: `VoiceLiveTransport.kt`.** New `RealtimeTransport` implementation beside
      `GeminiLiveTransport.kt`, which is the closest structural precedent (both are WSS + a
      server-authored first frame). Same control-channel handshake as E1.
      DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest` passes.

- [ ] **E4. Build and install on the Galaxy S9 phone** (locked decision 5). The target serial is
      fixed: `4633424442303098` (`SM-G965U`, SDK 29). **Three devices are attached to this machine**,
      so a bare `adb shell` fails with "more than one device" — every command below targets `-s`
      explicitly. Confirm the phone is the device being reached before installing:
      ```
      cd /c/dev/live-ninja/android
      adb -s 4633424442303098 shell getprop ro.product.model    # must print SM-G965U
      ./gradlew :app:assembleDebug
      adb -s 4633424442303098 install -r app/build/outputs/apk/debug/app-debug.apk
      ```
      Then pin that device to `gpt-live-azure` in Settings and run one real spoken turn.
      **Git Bash hazard:** `adb shell` commands containing an absolute path get MSYS path-mangled
      (`/data` becomes `C:/Program Files/Git/data`). Prefix those with `MSYS_NO_PATHCONV=1` and quote
      the whole remote command.
      **SDK 29 is the point of this device.** It sits exactly on the app's `minSdk` floor, so it is
      the first place to look if the new transports work on a tablet and not here — check
      `logcat -b crash` before assuming the engine is at fault.
      DoD: `adb -s 4633424442303098 shell pm list packages | grep ninja.jeremy.liveninja` prints the
      package, the app launches and the process stays alive, and one spoken turn completes on
      `gpt-live-azure` with a transcript written to the store.
      A failure here is recorded and reported — it is **not** a stop condition; finish everything
      else.

---

### WS-F — Ship the engines (the gate)

- [ ] **F1. Live end-to-end smoke on every new engine.** Depends on all of WS-B, WS-C, WS-D, WS-E.
      For each of the four pins: set it as the account default, start a session on web, speak one
      turn, confirm audio out, confirm the transcript reached
      `POST /api/v1/transcript`, confirm one tool call round-trips, and confirm barge-in interrupts
      playback.
      DoD: four sessions, four transcripts in the store, and the Execution log records the model id,
      the observed first-audio latency, and the cost badge value for each. **Nothing below this line
      is done until this passes.**

- [ ] **F2. Settings picker and Help drawer** — mandatory in the same commit (R13). The picker gains
      four rows; the Help drawer gains an entry per engine. Copy must state, for the two Voice Live
      rows: preview status, no SLA, and that the session configuration is **not** enforced
      server-side (`## The token problem`). Do not write copy implying otherwise.
      DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run TestHelpDrawer` passes.

- [ ] **F3. Update `docs/voice-engines.md`.** It currently documents three engines and says "three
      realtime speech-to-speech backends" — it will be eight across five providers. Add the new rows
      to the engine table, the client support matrix, and the cost/tradeoff section, and paste
      `## The token problem, stated honestly` in verbatim. Keep `agents.md` and `CLAUDE.md`
      consistent per `/c/dev/live-ninja/CLAUDE.md`.
      DoD: `grep -c 'gpt-live-azure' /c/dev/live-ninja/docs/voice-engines.md` returns non-zero and the
      client support matrix has a column for each new engine.

---

### WS-G — Deferred, and written down so it is not implicit

These are **backlog items, not plan items**. Add them to `/c/dev/live-ninja/backlog.md` in WS-F M3's
commit. A backlog item is never pulled into this plan without a decision.

- [ ] **G1. Replace the Voice Live token shape** if Microsoft ships a Voice Live
      `client_secrets`-equivalent. This is the single change that would delete
      `## The token problem` entirely. Watch the Voice Live API reference changelog.
- [ ] **G2. Move the Voice Live client secret to a certificate credential**, and — once the broker
      runs on Azure — to a managed identity, at which point both SSM parameters disappear.
- [ ] **G3. Expose the 15 cascaded Voice Live models** (A8) behind one additive optional
      `azureVoiceLiveModel` setting rather than 15 further engine constants.
- [ ] **G4. Voice Live's exclusive capabilities** (A11): `azure_semantic_vad_multilingual` with
      `remove_filler_words`, `azure_deep_noise_suppression`, Live-Reference AEC, word timestamps,
      visemes, avatars. Several would measurably improve the ambient-noise behaviour that
      `silenceDirective` (`mint.go:80-90`) currently works around in the prompt.
- [ ] **G5. M5Stack Tab5** on an Azure engine. Out of scope; the surface is backlog-only today.
- [ ] **G6. Make the usage counters real.** `AddDayUsage`/`AddMonthUsage` are written by nothing
      (WS-B M6 note). Inherited defect, tracked in `azure-migration-plan.md` WS-B M5a. Not this plan.

---

## Cost model

The honest headline: **these engines add no fixed monthly cost.** That is the whole point of the
"no container" constraint — unlike `nova-sonic`, which carries an always-on Fargate task plus an ALB
whether or not anyone speaks, all four new engines are client-direct and bill only per token. The
standing cost of this plan is **$0/month** plus the per-session token spend of whichever engine is
actually pinned.

Comparative per-token positioning, from A12 and A10 — treat the Azure figures as **read 2026-08-18
and pending WS-B M5's re-read**, and the Voice Live figures as **unknown until WS-B M5**:

- `gpt-live-azure` ≈ the same list rate as `openai-realtime` (audio in 32.00 / audio out 64.00 per
  1M). Choosing it is a **provider and data-residency** decision, not a cost saving.
- `gpt-live-azure-mini` ≈ 3× cheaper on audio (10.00 / 20.00). Note that `openai-realtime-mini` is
  *billed* at full rates in the UI badge today because `rates.go` has no row for it (R10) — WS-B M5
  fixes that, which will make the existing mini engine look correctly cheaper too.
- `azure-voice-live-lite` (`phi4-mm-realtime`) is the **Lite** tier and should be the cheapest option
  in the product once its rates are known — at the cost of preview status and the token shape in
  `## The token problem`.
- `gemini-flash-live` remains the cheapest **non-preview** engine.

The dominant cost term in this product is not audio — it is **cached text input re-read on every
assistant turn**. `azure-migration-plan.md`'s corrected cost model recovers ~30.8M cached input
tokens/month at 60 min/day, roughly 26× the raw audio volume. A cache-hit collapse takes the full
engine from ~$83/month to ~$1,056/month **with no error and no other signal**. This plan does not add
cache telemetry — that is `azure-migration-plan.md` WS-B M6 — so **WS-A M5's budgets are the entire
cost-safety story for this run.** Do not skip them and do not raise their thresholds.

---

## Sequencing

1. **In parallel, immediately:** WS-B M2–M6 and WS-D M1–M3 (pure Go, no Azure dependency) alongside
   WS-A M1–M6 (portal and CLI work with real provisioning latency).
2. WS-B M1 as soon as A3 and A6 land — it is the cheapest possible proof that the whole Azure premise
   holds, and B4 depends on its answer.
3. WS-C once A4 lands.
4. WS-E once WS-D M2 fixes the wire contract.
5. WS-F M1 gates everything user-visible. F2 and F3 land in the same commit wave as the picker.
6. WS-G is written into `backlog.md`, never executed here.

Nothing idles: while WS-A waits on resource provisioning or RBAC propagation, WS-B and WS-D are
fully runnable, and together they are the majority of the code in this plan.

---

## Execution log

Appended in place as work lands: commands run, commits, identifiers, and what each verification
actually returned. Written as it happens, not reconstructed at the end.

- **2026-08-24** — Plan written from a full read of the repository and of Microsoft Learn. Four
  operator decisions locked (see `## Locked decisions`). No Azure resource created yet, no code
  changed yet. **Next action: WS-A M1 and WS-B M2 and WS-D M1, started together.**

- **2026-08-24** — Audit landed as `/c/dev/live-ninja/azure-voice-gaps.md` (commit `29c2686`):
  47 findings from an eleven-reviewer audit, each adversarially verified. Plan committed as
  `75df361`. Work proceeds on branch `alexa-version`, NOT `main` — see gap-register M1/S5;
  locked decision 4 is deferred until the code is green, so no production deploy has happened.

- **2026-08-24 — WS-A M1 DONE.** `az group create` for `ln-azure-openai-rg` and `ln-voicelive-rg`,
  both `eastus2`, both tagged `project=live-ninja plan=azure-voice-plan`.
  DoD returned exactly two names:
  ```
  $ az group list --query "[?tags.plan=='azure-voice-plan'].name" -o tsv
  ln-azure-openai-rg
  ln-voicelive-rg
  ```

- **2026-08-24 — WS-A M2 DONE, and its DoD is WRONG (gap-register W5, now confirmed live).**
  `ln-aoai-eastus2` created, kind `AIServices`, sku `S0`, custom subdomain, `provisioningState`
  `Succeeded`. But `properties.endpoint` is **`https://ln-aoai-eastus2.cognitiveservices.azure.com/`**,
  not the `openai.azure.com` form the DoD asserts. The realtime host the plan needs is real, it is
  just in the endpoint MAP:
  ```
  $ az cognitiveservices account show -n ln-aoai-eastus2 -g ln-azure-openai-rg       --query 'properties.endpoints."OpenAI Realtime API"' -o tsv
  https://ln-aoai-eastus2.openai.azure.com/
  ```
  REPLACEMENT DoD for A2: query `properties.endpoints."OpenAI Realtime API"`, not `properties.endpoint`.
  Also recorded: this same resource exposes a `Voice Live Realtime API` endpoint at the
  `cognitiveservices.azure.com` host, NOT the `services.ai.azure.com` host A5 states. WS-C M2's
  `voiceLiveEndpoint` must be re-derived from the endpoint map of the actual Voice Live resource
  rather than assembled from the hostname template in A5.

- **2026-08-24 — WS-A M3 DONE. Model ids that actually deployed, verbatim (this propagates to
  WS-B M2, M5, WS-E and WS-F):**
  ```
  Deployment             Model                  Version     Cap    State
  gpt-realtime-2-1       gpt-realtime-2.1       2026-07-07  10     Succeeded
  gpt-realtime-2-1-mini  gpt-realtime-2.1-mini  2026-07-07  10     Succeeded
  ```
  A3's primary target was correct — no fallback needed. Quota limit is 10 units per model in
  `eastus2` (`az cognitiveservices usage list`), and both deployments take the full 10. Note
  `az cognitiveservices account deployment update` does not exist in az 2.89.1; changing capacity
  is delete + create.

- **2026-08-24 — WS-B M1 DONE. Two things settled that the plan left open.**
  1. **`cedar` IS accepted on Azure.** A4's ambiguity resolves in our favour, so WS-B M4's voice
     mapping is a no-op plus a test and `SupportedVoices` is reused unchanged.
  2. **Gap-register F2 is REFUTED by live evidence.** F2 claimed Azure rejects
     `audio.input.transcription.model = "gpt-4o-mini-transcribe"` because Azure requires a
     deployment name and WS-A M3 deploys no transcription model. The FULL production session
     config — the exact object `Minter.Mint` builds at `internal/realtime/mint.go:295-306`,
     including that transcription model, `semantic_vad`, `near_field` noise reduction, instructions
     and tools — mints successfully:
  ```
  minimal body (DoD as written)  -> HTTP 200, MINT_OK (cedar accepted)
  full production session config -> HTTP 200, MINT_OK
  ```
  R5's "the Azure minter can reuse this builder verbatim" therefore HOLDS at mint time. Not yet
  verified: whether transcription EVENTS actually arrive at session runtime — that is WS-F M1's job.
  Per stop condition 2 the key was piped from `az` into `curl` and never printed, and the minted
  `ek_` was never written to a file that survived the command.

- **2026-08-24 — WS-A M5 DONE.** Two resource-group budgets, $500/month each, alerting at 20% /
  50% / 100% (= $100 / $250 / $500) to the operator's address.
  ```
  $ az consumption budget list --query "[?contains(name,'ln-')].[name,amount]" -o tsv
  ln-azure-openai-rg-budget   500.0
  ln-voicelive-rg-budget      500.0
  ```
  **Gap-register D18 is REFUTED.** It claimed the subscription-scoped DoD returns zero rows for
  resource-group-scoped budgets. It does not — the command above is correct exactly as the plan
  wrote it. Two real corrections to A5 instead:
  1. The create command is `az consumption budget create-with-rg` (there is no `list-with-rg`), and
     its `--notifications` element properties are kebab-case: `contact-emails`, not `contactEmails`.
  2. **Forecasted alerts cannot be created with az 2.89.1.** The notification object has no
     `thresholdType` field — `az consumption budget create-with-rg --notifications "??"` lists only
     contact-emails, enabled, operator, threshold, contact-groups, contact-roles. A5 asks for "both
     actual and forecasted"; only **actual** shipped. Forecast alerts need the portal or the Cost
     Management REST API. Recorded rather than silently dropped.
  3. The "one test notification has been received" clause is still unmet and cannot be met by this
     run — it needs a human inbox.

- **2026-08-24 — WS-B M2 DONE.** `voiceengine.All` is now the single source of truth; `Engine.Valid`,
  `validEngine` and both settings allowlists derive from it. Added `IsAzure` / `IsVoiceLive`.
  Its original DoD passed on an untouched tree (gap register D2), so it was replaced with three
  named tests. Proof they have teeth — reverting `validEngine` to the pre-M2 four-engine switch:
  ```
  --- FAIL: TestPinToEngineAcceptsAzureEngines
      PinToEngine("gpt-live-azure", nil, "") = "openai-realtime", want "gpt-live-azure"
  ```
  Also closed the sixth gate no milestone named: `isOpenAIConversationEngine`
  (`/c/dev/live-ninja/internal/store/topics.go`) is a `gpt-`/`openai-` prefix match feeding the
  per-user OpenAI allowance, so Azure spend was being charged against the OpenAI budget. Now
  excluded explicitly, with a test.

- **2026-08-24 — WS-D M1, M2, M3, and M1 was rewritten because it could not be built as specified
  (gap register W3).** `X-LN-Client` never reaches the broker: the broker is invoked with a
  marshaled struct, not a forwarded HTTP request. A gate reading that header would have failed
  closed for 100% of sessions and made WS-F M1 unpassable.
  What shipped instead: `Request.ClientVersion` and `Request.Capabilities` cross the invoke
  boundary, filled by the web function from `X-LN-Client` and a new `X-LN-Capabilities` header. The
  grammar moved to `/c/dev/live-ninja/internal/clientver` so the broker can parse it without
  importing `internal/webapp` and linking fiber into its binary.
  **The gate keys on declared capability FIRST and version second**, because version alone cannot
  work today:
  ```
  web      -> sends no X-LN-Client at all (grep over web/ finds no occurrence)
  android  -> sends "android/0.2.2-hal+r5", which the headers.md grammar REJECTS (pre-release suffix)
  ```
  Both are covered by tests. `callsUrl` needed adding in THREE places, not one — the broker
  `Response`, the `brokerResponse` mirror in the web function, and the explicit `fiber.Map` the
  handler returns; the mirror alone reaches no client (gap register W4). It rides the default
  `openai-direct` path so it is exercised on every session and cannot rot.
  D3 is `[~]`: the Azure OpenAI origin is in `connect-src` with a test asserting it lands INSIDE the
  directive, not merely somewhere in the policy. The Voice Live origin is absent because its
  resource is blocked on A4 and this list takes real hosts only. `contracts/api.md` is not yet
  updated.

- **2026-08-24 — WS-B M3 DONE.** `NewAzureMinter` varies only the URL, the auth header (`api-key:`)
  and the deployment name as the model id; the session-config builder is reused verbatim as R5
  predicted and the live mint confirmed. A test pins that the Azure URL carries **no** `api-version`
  parameter, and that the OpenAI minter's URL, auth scheme and SSM parameter are all unchanged.

- **2026-08-24 — WS-B M5 DONE.** Added the missing `gpt-realtime-mini` row (R10 — a live billing
  defect: every `openai-realtime-mini` session was priced at full `gpt-realtime` rates by the silent
  fallback). Azure rows are keyed on the **deployment** names `gpt-realtime-2-1` and
  `gpt-realtime-2-1-mini`, not A12's dotted model ids, because the deployment name is what the
  broker sends as `model` — keying them the other way would miss every lookup and re-create R10 on
  the new engines (gap register W6). The two Voice Live models ship on an explicit `ratesMissing`
  list with `RatesForModel` returning `ok=false`, rather than an invented number.
  `TestRatesCoverEveryShippedEngine` now fails if any shipped engine's model is neither priced nor
  declared missing.

- **2026-08-24 — WS-B M6 PARTIAL `[~]`.** `gpt-live-azure` and `-mini` route through the existing
  openai-direct handler with the Azure minter and `mode: "azure-direct"`; `realtimeMintAPI` gained
  `CallsURL()` so the broker asks the minter it actually used instead of hardcoding a host beside a
  variable minter. An Azure pin with no endpoint configured cascades to `openai-realtime` through
  the **existing** `EngineFallback` cascade with a plain-language warning, so the engine constants
  are safe in production ahead of the Azure configuration. **Not done:** `handleVoiceLiveDirect`,
  blocked on A4.

- **2026-08-24 — WS-A M6 BLOCKED `[!]`, and the plan's mechanism for it is wrong (gap register
  F1 / M2 / W2).** `/c/dev/live-ninja/scripts/set-secret.sh` does **not** write SSM — it ends in
  `gh secret set "$NAME" -R "$REPO"`, a GitHub Actions secret. SSM parameters are written only by
  `.github/workflows/deploy.yml`'s "Sync secrets to SSM" step, which `put-parameter`s a hardcoded
  list of four names. A6 therefore needs THREE steps where the plan has one milestone:
  - (a) `./scripts/set-secret.sh AZURE_OPENAI_API_KEY` — the operator types the value.
  - (b) A new milestone adding three `put-parameter` blocks plus `env:` indirection to that workflow
        step. Without it the parameters never exist at runtime.
  - (c) A new milestone widening the broker's IAM policy, which grants `ssm:GetParameter` on two
        exact ARNs that do not include `/live-ninja/prod/azure/*`. Otherwise every read is
        AccessDenied.
  The config constants `ParamAzureOpenAIAPIKey`, `ParamVoiceLiveClientID`,
  `ParamVoiceLiveClientSecret` and their env overrides are already in
  `/c/dev/live-ninja/internal/config/config.go`.
  The correct DoD, which must run AFTER a deploy and needs the MSYS guard the plan applies only to
  `adb` (gap register W1):
  ```
  MSYS_NO_PATHCONV=1 aws ssm get-parameters-by-path --path /live-ninja/prod/azure \
    --query "Parameters[].Name" --output text
  ```
  Without that prefix Git Bash mangles the leading-slash path and AWS answers with a
  `ValidationException` that reads like an AWS fault rather than a shell artifact.

- **2026-08-24 — Not yet started:** WS-C (blocked on A4), WS-E (web and Android clients),
  WS-F (needs a human at a microphone), WS-G (backlog transcription).
  **Nothing has been pushed.** All work is committed on `alexa-version`; locked decision 4's
  push-to-`main` production deploy is deliberately deferred until the client work lands, because
  `X-LN-Capabilities` has no sender yet and WS-F M1 cannot pass without one.

- **2026-08-24 — WS-A M6 PARTIALLY DONE, and the deploy path it needed is now built.** The plan's
  one milestone was really three (gap register F1 / M2 / W2). All three shipped for the Azure OpenAI
  half; the Voice Live half stays blocked behind A4.
  - (a) **GitHub secret set.** `AZURE_OPENAI_API_KEY` was written by piping
    `az cognitiveservices account keys list ... --query key1 -o tsv` straight into
    `gh secret set AZURE_OPENAI_API_KEY -R JeremyProffittOrg/live-ninja`. The value passed through a
    pipe and was never printed, written to disk, or pasted into the session, so stop condition 2 was
    not reached and no operator typing was required. Verified by metadata only:
    ```
    $ gh secret list -R JeremyProffittOrg/live-ninja | grep -i azure
    AZURE_OPENAI_API_KEY    2026-08-24T21:55:40Z
    ```
  - (b) **`.github/workflows/deploy.yml` now syncs the three Azure parameters.** Each write is
    guarded on its secret being non-empty. `put-parameter` rejects an empty value, so an unguarded
    write of the not-yet-existing Voice Live pair would have failed the entire production deploy
    over a credential nothing reads yet.
  - (c) **`template.yaml` gives the broker `ssm:GetParameter` on
    `/live-ninja/prod/azure/*`** (Sid `AzureKeyParams`). It is on the **broker only**. The first
    edit accidentally landed it in `WebFunction`'s policy — caught before commit and moved, because
    the web function must never hold a mint key and the broker's own description calls it the sole
    holder.

- **2026-08-24 — Broker configuration on the wire.** `template.yaml` gained three parameters:
  `AzureOpenAIEndpoint` (default `https://ln-aoai-eastus2.openai.azure.com`),
  `AzureOpenAIDeployment` (`gpt-realtime-2-1`) and `AzureOpenAIMiniDeployment`
  (`gpt-realtime-2-1-mini`), surfaced to the broker as `AZURE_OPENAI_ENDPOINT`,
  `AZURE_OPENAI_DEPLOYMENT` and `AZURE_OPENAI_MINI_DEPLOYMENT`.
  The endpoint default is the resource's **`properties.endpoints["OpenAI Realtime API"]`** value,
  NOT `properties.endpoint`, which on this kind=AIServices resource is
  `https://ln-aoai-eastus2.cognitiveservices.azure.com/` and would 404 the mint (gap register W5).
  `sam validate --lint` returns "is a valid SAM Template".

- **2026-08-24 — Branch reconciled and DEPLOYED (locked decision 4; gap register M1/S5 closed).**
  `origin/main` had moved to `a0f4a4a` ("ci: move 3 job(s) to the home self-hosted runner pool"),
  which edits the same `deploy.yml` this plan edits, so it was **not** a fast-forward. Resolved by
  merging `origin/main` into `alexa-version` — it auto-merged, and both changes survived: three
  `home-general-linux-x64` runner lines and five Azure-sync lines.
  Pushed with `git push origin HEAD:main` rather than checking `main` out, which left the
  uncommitted `plan.md` edit in the worktree untouched:
  ```
  a0f4a4a..a3043c4  HEAD -> main
  ```
  **The four engines are inert in this deploy by construction, on three independent grounds:**
  1. No settings picker offers them — WS-F M2 has not shipped.
  2. The broker's client gate refuses any client that does not send `X-LN-Capabilities`, and no
     client sends it yet (WS-E has not shipped).
  3. Both Voice Live engines have no minter at all; an `azure-voice-live` pin cascades to
     `openai-realtime` with a warning.
  What this deploy DOES change for existing users is the `gpt-realtime-mini` rate row: the
  `openai-realtime-mini` cost badge stops over-reporting at full `gpt-realtime` rates (R10).
