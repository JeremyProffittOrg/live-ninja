> **MIGRATED — 2026-07-24.** This plan is archived. Its unfinished items were consolidated into
> [/plan.md](../plan.md) and any deliberately-deferred items into [/backlog.md](../backlog.md).
> Kept here as a historical record — do not edit; track live work in /plan.md.

# Gemini Flash Live — Third Voice Engine Integration Plan (M13)

> **Status:** LOCKED 2026-07-19 (question pass complete) — ready for autonomous execution · **Owner:** jeremy · **Repo:** `JeremyProffittOrg/live-ninja`
> **Parent plan:** [plan.md](plan.md) (conventions: §1.1 status markers, §1.2 model routing, §1.3 execution rules)
> **Engine value:** `gemini-flash-live` · **Bootstrap mode:** `gemini-direct` · **Model:** `gemini-3.1-flash-live-preview`
> **Research verified:** 2026-07-19 against live Google/OpenAI docs (see §3 sources)

Adds Google's **Gemini Live API with native audio** as a third voice engine, pinnable
per device exactly like the existing engines. Gemini supports **ephemeral tokens**
(Live-API-only, single-session, config-lockable), so this engine is **client-direct**
like OpenAI — audio never touches our AWS account, **no bridge, no new infra, zero
standing cost**. The integration layer is: one new mint branch in the realtime broker,
one new transport branch per client surface, additive enum/contract updates.

---

## 1. How to read this plan

- Status markers, model-routing letters (H/S/F/O), and execution rules are inherited
  verbatim from `plan.md` §1.1–1.3. `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked.
- This is a standalone milestone plan (**M13**) structured for autonomous execution by
  agentic teams after the question pass locks §2. Workstreams reuse the parent plan's
  map: **WS-C** (realtime backend) leads; **WS-D** (web), **WS-E** (Android),
  **WS-F** (firmware), **WS-G** (contracts/settings/tests) support.
- Verbose implementation notes accrue in §10 as work proceeds (RESUME STATE convention
  from plan.md §8).

### Workstream parallelism

WS-C Phase A (server) blocks everything else only at its **first two tasks** (engine
constants + broker mint). Once the broker returns the `gemini-direct` shape, WS-D,
WS-E, and WS-F proceed **fully in parallel** (each surface is an independent transport
branch). WS-G contract/schema edits land with Phase A; WS-G tests land last.

| Workstream | Scope here | Primary models | Blocked by |
|---|---|---|---|
| WS-C | Broker mint branch, ephemeral-token minter, config/SSM/IAM, rates | O (mint/auth design), F (minter), S (wiring) | — (starts immediately) |
| WS-D | Web `#connectGemini` transport, CSP, picker radio | F (transport), S (UI) | WS-C tasks 1–3 |
| WS-E | Android `GeminiLiveTransport`, DI, picker radio | F (transport), S (UI) | WS-C tasks 1–3 |
| WS-F | Firmware `LN_RT_ENGINE_GEMINI_DIRECT` branch, resampler gate | F (audio/protocol), S (parse) | WS-C tasks 1–3 |
| WS-G | Schema/contract/docs, settings validation, cross-engine tests | S (tests), H (docs/enums) | WS-C task 1 (enums), all (final tests) |

---

## 2. Locked decisions (question pass completed 2026-07-19, owner-answered)

| # | Decision | **LOCKED** |
|---|---|---|
| D1 | Surfaces in scope | **All three** (web + Android + firmware); firmware may ship HIL-unverified like Nova did |
| D2 | Model | **`gemini-3.1-flash-live-preview`**; `GEMINI_LIVE_MODEL` env var allows swap (e.g. to `gemini-2.5-flash-native-audio-preview-12-2025`) without code change |
| D3 | API surface | **Gemini API / AI Studio key** (Preview status accepted; simple API key + ephemeral tokens; no Vertex) |
| D4 | Voice | **Per-engine voice picker ships in this milestone.** Catalog = the full prebuilt-HD voice set, **runtime-validated in the Phase 0 spike** (Google doesn't publish the exact Live-valid subset; only spike-accepted names ship). Default Gemini voice: `Kore`. |
| D4b | Persona voices | **Per-persona Gemini mapping** — every persona in `personas.go` gets a hand-curated nearest-match `geminiVoice` field (its OpenAI voice suggestion is meaningless on Gemini). Resolution on Gemini mirrors OpenAI: user's Gemini-voice setting ?? persona `geminiVoice` ?? `Kore`. |
| D5 | Mint fallback | **Ladder confirmed:** Go genai SDK `AuthTokens.Create` → raw REST v1alpha → pause-and-ask (Phase 0 gate) |
| D6 | Picker exposure | **Enabled immediately** on web + Android (unlike Nova's commented-out option) |

---

## 3. Architecture snapshot

### 3.1 Where Gemini sits among the engines

| Engine pin | Backend | Media path | AWS in audio path? |
|---|---|---|---|
| `openai-realtime` / `-mini` | OpenAI Realtime (`gpt-realtime`) | Client-direct **WebRTC** (web/Android) / **WSS** (firmware) | No |
| `nova-sonic` | Bedrock Nova Sonic | **Backend-bridged** WSS via Fargate bridge | Yes (bridge) |
| **`gemini-flash-live`** | **Gemini Live API (native audio)** | **Client-direct WSS** to `generativelanguage.googleapis.com` | **No** |

Gemini is a **structural hybrid** of the two existing paths: WSS + PCM16 framing like
the Nova transport, but client-direct with its own auth + client-sent session `setup`
like OpenAI. On web/Android the implementation forks the **Nova WSS transport
skeleton** (binary-PCM capture/playback plumbing, ready-gating) and swaps auth + event
translation. On firmware, the OpenAI path is already WSS, so Gemini is a third
`ws_open` branch.

**Audio rates match the platform exactly:** pcm16 **16 kHz mono uplink**, **24 kHz mono
downlink** — the same rates every client already runs (`audio/pcm;rate=16000` in,
24 kHz raw out). One firmware exception in §3.5.

### 3.2 Gemini Live protocol facts (verified 2026-07-19)

- **Endpoint:** `wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent`
  — auth via `?access_token=<ephemeral-token>` query param (browser-safe; no headers or
  subprotocol needed). [ai.google.dev/gemini-api/docs/live-api/get-started-websocket]
- **Ephemeral tokens:** minted server-side against **v1alpha** (`auth_tokens.create`);
  `uses: 1` (single session), `expire_time` default 30 min (message window),
  `new_session_expire_time` default 1 min (session-start window);
  `liveConnectConstraints` locks model + session config at mint time.
  [ai.google.dev/gemini-api/docs/ephemeral-tokens]
- **Handshake:** client sends `{"setup": {model, responseModalities:["AUDIO"], systemInstruction,
  tools, speechConfig, realtimeInputConfig, sessionResumption, contextWindowCompression,
  inputAudioTranscription:{}, outputAudioTranscription:{}}}` → server replies `setupComplete`.
- **Uplink audio:** `{"realtimeInput": {"audio": {"data": "<base64 pcm16>", "mimeType": "audio/pcm;rate=16000"}}}`.
- **Downlink audio:** `serverContent.modelTurn.parts[].inlineData` (base64 pcm16 @ 24 kHz).
- **Transcripts:** `serverContent.inputTranscription` / `.outputTranscription` deltas
  (enabled by the empty transcription objects in setup).
- **Barge-in:** automatic VAD on by default; server sends `serverContent.interrupted: true`
  → client flushes local playback. Pending tool calls get `toolCallCancellation`.
  There is **no client→server cancel primitive** in auto-VAD mode; the client `bargeIn()`
  maps to local playback flush + VAD interruption.
- **Tools:** server `toolCall.functionCalls[]{id,name,args}` → client executes (same
  `POST /api/v1/tools/invoke`) → client `toolResponse.functionResponses[]{id,name,response}`.
  Manual only (no auto-execution). `behavior: "NON_BLOCKING"` available for async.
- **Session lifecycle:** connection lifetime **~10 min** (server sends `goAway` with
  `timeLeft` first); audio session cap **15 min** unless `contextWindowCompression`
  (sliding window) is set — then effectively unlimited. `sessionResumption` handles
  (valid 2 h) let a client reconnect the *same* session across the 10-min recycles,
  and a `uses:1` token permits those resumption reconnects within its `expireTime`.
  Past 30 min the client re-fetches `GET /api/v1/realtime/session` (fresh token) and
  resumes with the stored handle — same pattern as Nova's per-reconnect re-mint.
- **Usage/cost:** `usageMetadata` on server messages carries token counts by modality.
- **Voices:** `speechConfig.voiceConfig.prebuiltVoiceConfig.voiceName` (e.g. Kore, Puck,
  Charon); native-audio models auto-detect language.

### 3.3 Pricing (per 1M tokens, verified 2026-07-19; audio ≈ 25 tokens/sec)

| Model | Text in | Audio in | Text out | Audio out |
|---|---|---|---|---|
| `gemini-3.1-flash-live-preview` | $0.75 | $3.00 (≈$0.005/min) | $4.50 | $12.00 (≈$0.018/min) |
| `gpt-realtime-2.1` (current OpenAI) | $4.00 | $32.00 | $24.00 | $64.00 |
| `gpt-realtime-2.1-mini` | $0.60 | $10.00 | $2.40 | $20.00 |

Gemini audio is ~**10× cheaper than gpt-realtime** and **2–3× cheaper than mini** —
*with zero standing infra* (unlike Nova's always-on Fargate+ALB). Caveat: OpenAI has
audio-input caching; Gemini Live has none, which narrows the gap on long sessions.
This is the picker copy angle: **"cheapest engine, no infrastructure cost, preview-status model."**

### 3.4 The `gemini-direct` bootstrap shape (new, third response)

```jsonc
// GET /api/v1/realtime/session — engine resolved to gemini-flash-live
{
  "mode": "gemini-direct",
  "engine": "gemini-flash-live",
  "model": "gemini-3.1-flash-live-preview",
  "geminiEndpoint": "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
  "accessToken": {
    "value": "auth_tokens/…",
    "expiresAt": "2026-07-19T12:30:00Z",        // token expire_time (~30 min)
    "newSessionExpiresAt": "2026-07-19T12:01:00Z" // session-start window (~1 min)
  },
  "sessionConfig": { /* the exact `setup` frame body the client must send */ },
  "toolManifest": [ /* same registry as other engines */ ],
  "rates": { /* from internal/realtime/rates.go, keyed by the Gemini model */ },
  "sessionId": "…",
  "quotaWarning": null
}
```

> **⚠ Field-naming is load-bearing:** clients (and pre-M12 firmware especially,
> `ln_rt_session.c:109-120`) detect Nova by the **presence** of `wsUrl`/`bridgeUrl`
> fields, not just `mode`. The Gemini shape must therefore **never** use a field named
> `wsUrl`/`ws_url`/`bridgeUrl`/`bridge_url` — hence `geminiEndpoint` + `accessToken`.
> A legacy client that doesn't know `gemini-direct` then fails closed (unknown shape)
> instead of silently misrouting into its Nova branch. Engines are only pinned
> per-device by the owner, so no un-updated device should ever receive this shape
> anyway — the naming is defense in depth.

The broker mints the token with `liveConnectConstraints` (locking model + config) AND
returns `sessionConfig` for the client to send as its `setup` frame. A known Google-side
bug intermittently ignores `systemInstruction` supplied only via constraints — sending
it in the client `setup` too is the documented-workaround belt-and-suspenders.

### 3.5 Per-surface transport summary

| Surface | Fork base | Auth | Uplink framing | Key gotcha |
|---|---|---|---|---|
| Web | Nova WSS skeleton (`#connectNovaBridge`) | `?access_token=` in URL (browsers can't set WS headers) | JSON+base64 (NOT raw binary like Nova) | CSP `connect-src` must add the Google host |
| Android | `NovaBridgeTransport` (OkHttp) | `?access_token=` (keep parity with web) | JSON+base64 | New Hilt binding + coordinator branch |
| Firmware | OpenAI WSS branch (`ws_open`) | `?access_token=` in URL, no headers | JSON+base64 (same `send_audio_slice` b64 path) | **Bypass the 16k→24k uplink resampler** — Gemini wants 16 kHz; `r32_process` is currently unconditional (`ln_realtime.c:275`) |

Firmware TLS: both HTTP + WSS already use the global ESP-IDF cert bundle
(`esp_crt_bundle_attach`); Google's GTS roots are in the full bundle — no per-host CA
work, just verify the build ships the full bundle (`CONFIG_MBEDTLS_CERTIFICATE_BUNDLE`).

---

## 4. M13 — Tertiary Voice Engine (Gemini Flash Live)  `[~]`  (code-complete + deployed; E1/E2 live-audio verification = owner handoff)

> **Client-direct — no new infra.** Requires only a Gemini API key (AI Studio) set as
> a secret. No Fargate, no ALB, no new Lambda. Model is **Preview** on the Gemini API;
> the engine is opt-in per device, so preview instability risk is scoped to pinned
> devices. All Gemini output audio is watermarked by Google (SynthID) — cosmetic, no
> integration impact.

**Definition of Done:** a device can be pinned to `gemini-flash-live`; a pinned device's
session bootstrap returns the `gemini-direct` shape and the client connects **directly**
to Gemini Live over WSS with an ephemeral token (API key never leaves the broker);
audio, transcripts, tools, topics, memory, barge-in, and cost tracking behave
identically to the other engines; sessions survive the 10-min connection recycle via
resumption handles; OpenAI- and Nova-pinned devices are byte-for-byte unchanged.
_(extends FR-VE-01..04)_

### Phase 0 — Mint spike (gate; nothing else merges until this passes)  `[x]`

- `[x]` **F** — **T0: prove ephemeral-token minting from Go.** Try in order:
  (1) `google.golang.org/genai` Go SDK `AuthTokens.Create` against v1alpha;
  (2) raw REST `POST /v1alpha/auth_tokens` (officially undocumented; community reports
  `ACCESS_TOKEN_TYPE_UNSUPPORTED` friction — capture exact request shape from the JS SDK's
  wire traffic if needed);
  then hand-verify the minted token opens a live WSS session (scripted smoke: setup →
  one audio turn → transcript back). **If both fail → `[!]` pause and ask** (genuine
  blocker; options then: pin to JS-SDK-shaped REST replay, or a Vertex pivot).
  _Record the working mint path + exact request/response in §10 before Phase A starts._
- `[x]` **S** — **T1: voice-catalog validation (D4).** With a working token, probe the
  full prebuilt-HD voice name set (Zephyr, Puck, Charon, Kore, Fenrir, Leda, Orus,
  Aoede, Callirrhoe, Autonoe, Enceladus, Iapetus, Umbriel, Algieba, Despina, Erinome,
  Algenib, Rasalgethi, Laomedeia, Achernar, Alnilam, Schedar, Gacrux, Pulcherrima,
  Achird, Zubenelgenubi, Vindemiatrix, Sadachbia, Sadaltager, Sulafat) against
  `gemini-3.1-flash-live-preview` — a setup frame per name; accepted = setupComplete +
  an audio turn. The accepted list **is** the shipped Gemini voice catalog. Record the
  validated list in §10; it feeds A7's `SupportedGeminiVoices`.

### Phase A — Server (WS-C + WS-G contracts)  `[x]`

Ordered tasks:
- `[x]` **H** — **A1: engine constants.** `internal/voiceengine/engine.go`: add
  `EngineGeminiFlashLive Engine = "gemini-flash-live"`; include in `Valid()`
  (engine.go:30-37). Verify `IsClientDirect()` (`e != EngineNovaSonic`, engine.go:27)
  correctly returns true. Add to `internal/realtime/mint.go` `validEngine`
  (mint.go:793-802) so the pin resolves instead of falling through to default.
- `[x]` **S** — **A2: config plumbing.** `internal/config/config.go`: add
  `ParamGeminiAPIKey = "/live-ninja/prod/gemini/api_key"` + `EnvOverrideGeminiAPIKey =
  "GEMINI_API_KEY"` (config.go:25-41 pattern). `template.yaml` RealtimeBrokerFunction:
  add `GEMINI_LIVE_MODEL` env var (default `gemini-3.1-flash-live-preview`) and add the
  Gemini SSM ARN to the `ssm:GetParameter` policy (new Sid beside `OpenAiKeyParam`,
  template.yaml:499-504; existing KMS-via-SSM condition already covers decryption).
  Extend the deploy workflow's GitHub-secret→SSM sync with `GEMINI_API_KEY` (mirror the
  OpenAI key step). **Owner action:** run `./scripts/set-secret.sh GEMINI_API_KEY`
  (agents never see the value).
- `[x]` **F** — **A3: Gemini minter.** New `internal/realtime/gemini_mint.go`:
  `GeminiMinter.Mint(ctx, sessionID, cfg)` using the Phase-0-proven path. Mints
  `uses:1`, `expire_time=+30m`, `new_session_expire_time=+1m`, `liveConnectConstraints`
  = model + full session config (responseModalities AUDIO, voice from the D4b
  resolution chain (user Gemini-voice setting ?? persona `geminiVoice` ?? `Kore`),
  system instructions from the same persona/guide resolution the OpenAI path uses
  (main.go:321-329), tool declarations translated from the tool manifest,
  `sessionResumption:{}`, `contextWindowCompression:{slidingWindow:{}}`,
  input/output transcription on, VAD defaults). Returns token + the `sessionConfig`
  echo for the client `setup` frame.
- `[x]` **S** — **A4: broker branch.** `cmd/realtime-broker/main.go` `handleMint`:
  add `if engine == voiceengine.EngineGeminiFlashLive { return b.handleGeminiDirect(...) }`
  beside the Nova branch (main.go:304-306). New handler mirrors `handleNovaBridge`
  (main.go:385-431) but calls the GeminiMinter; returns the §3.4 shape (extend
  `Response` struct, main.go:111-165, with `GeminiEndpoint`/`AccessToken` fields —
  **never** reusing `WSURL`). Quota gate + persona/voice/guides resolution reused as-is.
  Wire in `main()`: `GEMINI_LIVE_MODEL` env, minter construction (main.go:660-694).
- `[x]` **S** — **A5: web-tier passthrough + rates.** `internal/webapp/api_routes.go`
  `handleRealtimeSession` (:466-499): pass the `gemini-direct` mode through (currently
  branches nova-bridge vs default) and attach `"rates": realtime.RatesFor(model)`.
  `internal/realtime/rates.go`: add `gemini-3.1-flash-live-preview` to `modelRates`
  (text in 0.75 / audio in 3.00 / text out 4.50 / audio out 12.00 per 1M) so the cost
  badge doesn't silently fall back to gpt-realtime pricing (rates.go:28-42).
- `[x]` **H** — **A6: contracts + settings validation.**
  `contracts/settings.schema.json`: add `gemini-flash-live` to **both** `voiceEngine`
  enums (default :170-175, devices :180-187; additive-only per contracts/README rule 3).
  `internal/webapp/settings_routes.go` `validateAndNormalizeSettings`: add to both
  `oneOf` lists (:377-379, :384-388). `contracts/api.md`: document the third shape on
  the `GET /v1/realtime/session` row (:53). Update `docs/voice-engines.md` to a
  three-engine table (client support matrix row starts ⚠ per-surface until verified).
- `[x]` **S** — **A7: Gemini voice catalog + persona mapping (D4/D4b).**
  `internal/realtime/catalog.go`: add `SupportedGeminiVoices` (the T1-validated list,
  same `VoiceInfo` shape) and serve it — extend `GET /api/v1/realtime/voices` with an
  engine-keyed shape (additive: keep the existing array for legacy, add a
  `geminiVoices` sibling). `internal/realtime/personas.go`: add a `geminiVoice` field
  to every persona entry with a hand-curated nearest-match (curation heuristic: match
  gender-register + energy of the OpenAI suggestion; record the mapping table in §10).
  Settings: new additive `geminiVoice` property in `contracts/settings.schema.json` +
  validation against the shipped catalog in `settings_routes.go` (mirror the existing
  OpenAI voice validation). Broker resolution: Gemini-mode sessions resolve
  user `geminiVoice` ?? persona `geminiVoice` ?? `Kore` (a `ResolveSessionVoice`
  sibling or an engine parameter — follow existing code shape). _(feeds A3's config;
  A3 may land with `Kore` hardcoded first and pick up the resolver when A7 merges,
  both within Phase A — no cross-phase stub.)_
- `[x]` **S** — **A8: server tests.** Broker unit tests: pin→`gemini-direct` shape,
  no `wsUrl`-named fields in the JSON (regression-guard the §3.4 naming rule),
  OpenAI/Nova responses unchanged; settings PUT accepts the new enum value and a
  valid/invalid `geminiVoice`; mint-branch test with a fake minter; voice-resolution
  chain test (setting ?? persona ?? Kore).

### Phase B — Web (WS-D; parallel with C/D after A4)  `[x]`

- `[x]` **H** — **B1: CSP.** `internal/webapp/pages_routes.go:47`: add
  `wss://generativelanguage.googleapis.com` to `connect-src`; update the assertion in
  `internal/webapp/render_test.go:129`.
- `[x]` **F** — **B2: `#connectGemini` transport.** `web/static/js/realtime.mjs`:
  extend `mintOnce` shape validation (:182-211) for `gemini-direct`
  (require `geminiEndpoint` + `accessToken.value` + `sessionConfig`); add the mode
  dispatch branch (:536-542). New `#connectGemini` forked from `#connectNovaBridge`
  (:726-786): URL = `geminiEndpoint + '?access_token=' + token`; on open send the
  `setup` frame from `sessionConfig`; ready-gate on `setupComplete` (replacing Nova's
  `session.start` gate). Reuse the existing 16k capture / 24k playback PCM plumbing
  (NOVA_DEFAULT_*_RATE constants :103-106, `#startNovaCapture` :991, `#novaEnqueueAudio`
  :934) — but frame uplink as JSON `realtimeInput.audio` base64 (not raw binary), and
  decode downlink from `serverContent.modelTurn.parts[].inlineData`.
- `[x]` **F** — **B3: event translation + lifecycle.** Map Gemini events to the same
  CustomEvents the other paths emit: `inputTranscription`/`outputTranscription` →
  userdelta/userfinal/assistantdelta/assistantfinal (parity with `#onNovaMessage`
  :898-913); `interrupted` → local playback flush (the existing `bargeIn()` flush path,
  minus `response.cancel` which has no Gemini equivalent); `toolCall` → existing tool
  router flow → `toolResponse`; `usageMetadata` → the cost estimator (same fields the
  OpenAI path feeds); `goAway`/10-min recycle → store `sessionResumptionUpdate` handles,
  reconnect with handle, re-fetch a fresh session (new token) past `expiresAt` —
  mirroring the Nova reconnect-re-mints pattern. Transcript turns POST to
  `/api/v1/transcript` with `engine: "gemini-flash-live"`.
- `[x]` **S** — **B4: picker radio + Gemini voice picker.**
  `web/templates/pages/conversation.html` (:329-353, inside the `voice-engine-section`
  markers): add the `gemini-flash-live` radio with the §3.3 cost note ("cheapest, no
  infra, preview model"). Wiring in `settings.mjs` (:1116-1132) is value-generic —
  verify only, plus `renderField` seed. **Voice picker (D4):** add a Gemini-voice
  select fed by the `geminiVoices` catalog from `/api/v1/realtime/voices`, shown
  when the engine selection is `gemini-flash-live` (existing voice picker stays
  OpenAI-scoped); writes the `geminiVoice` settings key.

### Phase C — Android (WS-E; parallel with B/D)  `[x]`

- `[x]` **S** — **C1: bootstrap parsing + DI.** `RealtimeSessionApi.kt`: add
  `MODE_GEMINI_DIRECT = "gemini-direct"` (:44-45) + parse `geminiEndpoint`/`accessToken`/
  `sessionConfig` fields (:97-113). `RealtimeModule.kt`: new `@GeminiTransport` qualifier
  bound to `GeminiLiveTransport` (:31-37).
- `[x]` **F** — **C2: `GeminiLiveTransport`.** Fork `NovaBridgeTransport.kt` (OkHttp
  WSS, 16k AudioRecord / 24k AudioTrack plumbing, :118-169, :360-418): auth via
  `?access_token=` query param (keep URL-auth parity with web even though OkHttp could
  set headers); send `setup` on open, ready-gate on `setupComplete`; JSON+base64 uplink
  framing; event translation + goAway/resumption lifecycle identical in design to B3.
- `[x]` **S** — **C3: coordinator + picker.** `RealtimeSessionCoordinator.kt` (:80-86):
  third branch selecting the Gemini transport (`connect(token, endpoint)` is already
  engine-agnostic). `SettingsScreen.kt` (:481-505): add the radio option (+
  `settings_engine_gemini_*` string resources); `SettingsStore` path is value-generic.
  **Voice picker (D4):** Gemini-voice list (from the voices endpoint) shown when the
  engine selection is `gemini-flash-live`; writes the `geminiVoice` settings key via
  `SettingsStore` (same preserve-the-rest spread as `setVoiceEngineDefault`).

### Phase D — Firmware (WS-F; parallel with B/C; may ship HIL-unverified per D1)  `[x]`  (HIL-unverified)

- `[x]` **S** — **D1: enum + bootstrap parse.** `ln_rt_internal.h`: add
  `LN_RT_ENGINE_GEMINI_DIRECT` (:24-27); extend `ln_rt_session_info_t` (:30-39) —
  check `ws_url[640]`/`token[512]` capacities against the Gemini endpoint+token lengths.
  `ln_rt_session.c` `parse_session_body` (:94-193): branch on `mode=="gemini-direct"`
  **before** the Nova `wsUrl`-presence heuristic; fill endpoint + access token + stash
  the `sessionConfig` setup frame (respect the PSRAM buffer discipline —
  ln_realtime.c:21-24).
- `[x]` **F** — **D2: `ws_open` branch + setup frame.** `ln_realtime.c` `ws_open`
  (:532-566): third branch — URL = endpoint + `?access_token=` (no headers, like Nova);
  verify `s_ws_url[1280]` headroom (:103). Send the `setup` frame in
  `WEBSOCKET_EVENT_CONNECTED` (:480-496) gated on mode (where the OpenAI
  `session.update` used to live); gate readiness on `setupComplete`.
- `[x]` **F** — **D3: audio path + resampler gate.** **Gate `r32_process` off for
  Gemini** in `ln_realtime_send_audio` (:263-283 — the 16k→24k uplink resample at :275
  is currently unconditional; Gemini takes the AFE's native 16 kHz directly, mimeType
  `audio/pcm;rate=16000`). Downlink stays 24 kHz — existing decode/playback unchanged.
  Map events in `handle_msg`: transcription deltas, `interrupted` → existing barge-in
  flush, `goAway` → reconnect (the existing reconnect already re-fetches a fresh
  session, which re-mints the token — resumption handle carriage added on top).
  Tool calls: same `POST /api/v1/tools/invoke` flow as the other modes.
- `[x]` **H** — **D4: TLS sanity.** Confirm the build ships the full ESP-IDF cert
  bundle (GTS roots for `generativelanguage.googleapis.com`); no per-host pinning
  exists (`esp_crt_bundle_attach` global — ln_realtime.c:560, ln_rt_session.c:226).

### Phase E — Verification (WS-G; after B/C/D)  `[~]`  (automated done; live-audio items = owner handoff)

- `[!]` **S** — **E1: cross-engine parity test.** Pin one device to
  `gemini-flash-live`, one to `openai-realtime`: transcripts land in the same sink with
  correct `engine` tags; tools invoke identically; topics/memory extraction runs;
  conversation cost populates from Gemini `usageMetadata` at Gemini rates; barge-in
  interrupts playback on both; a persona switch audibly changes the Gemini voice per
  the D4b mapping and a user `geminiVoice` setting overrides it.
- `[!]` **S** — **E2: lifecycle test.** A >10-min web session survives the goAway
  recycle via resumption handle; a >30-min session re-fetches a fresh token and
  resumes; quota gate still fires pre-mint.
- `[x]` **H** — **E3: regression.** OpenAI-pinned and (disabled) Nova paths
  byte-identical: broker tests, `render_test.go` CSP, settings round-trip, firmware
  OpenAI smoke.
- `[x]` **H** — **E4: docs close-out.** `docs/voice-engines.md` three-engine final,
  plan.md cross-reference (M13 row in roadmap + this file), §10 notes complete.

---

## 5. Deploy & secrets

- **No new infra.** Changes ride the existing pipeline: push to `main` → GitHub Actions
  (OIDC) → sam build/deploy. No Fargate/ALB/Lambda additions; broker + web-tier Lambdas
  redeploy with new env/IAM only.
- **One new secret:** `GEMINI_API_KEY` (GitHub secret via `./scripts/set-secret.sh
  GEMINI_API_KEY` — or `scripts\set-secret.bat GEMINI_API_KEY` from cmd/PowerShell —
  owner-typed; deploy workflow syncs → SSM SecureString
  `/live-ninja/prod/gemini/api_key`; broker gets the sole `ssm:GetParameter`). No
  Secrets Manager. Agents never see the value.
  - **Where the key comes from:** Google AI Studio → <https://aistudio.google.com/apikey>
    (create under a Google Cloud project). The **Google AI Pro consumer subscription
    does NOT cover API-key usage** (verified 2026-04-20 announcement: subscriber perks
    apply only inside AI Studio; API keys bill pay-per-request separately). The API
    free tier (no card) exists but is rate-limited and its Live-API coverage is
    unconfirmed — enable billing on the project (Tier 1) for real sessions; usage
    bills at the §3.3 rates.
- **One new variable-ish env:** `GEMINI_LIVE_MODEL` in template.yaml (default
  `gemini-3.1-flash-live-preview`) — model swaps (e.g. to 2.5, or future GA id) without
  code changes.
- **Cost tags:** unchanged (stack-level, already applied).

## 6. Risks & mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| Go-side ephemeral mint unsupported/undocumented (SDK gap, `ACCESS_TOKEN_TYPE_UNSUPPORTED`) | Medium | Phase 0 spike is a hard gate with an ordered fallback ladder; pause-and-ask is the designed terminal state, not a stub |
| Preview-model instability (a 1011-mid-turn incident hit the 2.5 preview in May 2026) | Medium | Opt-in per device; `GEMINI_LIVE_MODEL` env flips models without redeploy; OpenAI default untouched |
| `systemInstruction` ignored under constrained tokens (known Google bug) | Medium | Send instructions in both `liveConnectConstraints` and the client `setup` frame; E1 asserts persona adherence |
| Legacy clients misparse the third shape as Nova | Low | No `wsUrl`-family field names (§3.4) + A7 regression test; pins are owner-set per updated device |
| 10-min recycle audible mid-conversation | Medium | Resumption handle + pre-emptive reconnect on `goAway.timeLeft`; E2 exercises it |
| Firmware RX buffer pressure from large JSON frames | Low | Existing 32KB→256KB growable RX buffer; D3 verifies against real downlink frame sizes |
| Gemini pricing/model churn | Low | rates.go keyed by model id; env-var model switch; §3.3 re-verify before UI copy quotes numbers |

## 7. Question pass — CLOSED 2026-07-19

Two rounds asked and answered by the owner; all resolutions locked in §2 (D1–D6).
Round 1: scope / model / API surface / voice-handling. Round 2 (surfaced by the
voice-picker answer): voice-catalog source (full HD set, spike-validated), persona
voice interplay (per-persona Gemini mapping), and confirmation of D5/D6 defaults.
No open questions remain; execution may start at Phase 0.

## 8. Sources (verified 2026-07-19)

- Live API: ai.google.dev/gemini-api/docs/live · /live-api/get-started-websocket · /live-api/capabilities · /live-api/session-management · /live-tools
- Ephemeral tokens: ai.google.dev/gemini-api/docs/ephemeral-tokens
- Model: ai.google.dev/gemini-api/docs/models/gemini-3.1-flash-live-preview · blog.google (3.1 Flash Live announce, 2026-03-26)
- Pricing: ai.google.dev/gemini-api/docs/pricing · developers.openai.com/api/docs/pricing
- Repo maps: broker `cmd/realtime-broker/main.go` · `internal/realtime/mint.go` · `internal/voiceengine/` · web `realtime.mjs` · Android `realtime/` · firmware `ln_realtime/` (line refs inline above, surveyed at HEAD b544db3)

## 9. Execution rules (inherited)

Autonomous straight-through run once §2 locks; pause only on a genuine un-pre-decided
blocker (Phase 0 terminal failure is the one designed pause). Verbose notes in §10 as
work proceeds. Every push to `main` is a prod deploy — Phase A merges only after its
tests pass locally; client phases merge per-surface when their smoke passes.

## 10. Implementation Notes (append-only; RESUME STATE convention per plan.md §8)

> **RESUME STATE — M13 CODE-COMPLETE + DEPLOYED 2026-07-19.** Phases 0/A/B/C/D done;
> E3/E4 done; **E1/E2 (live audio parity + lifecycle) are the OWNER HANDOFF** — see the
> Phase E notes below. Commits: 8f32d1b (server), 6efa476 (web), 35b89aa (Android,
> compile-unverified — no JDK/Android CI; owner's next Studio build is the gate),
> 968d373 (firmware, HIL-unverified per D1). All deploys green; GEMINI_API_KEY synced
> to SSM (verified present). `go test ./...` green; JS syntax-checked.**

### Phase 0 results (2026-07-19, spike run locally against live Gemini API)

**T0 — mint path PROVEN (D5 ladder rung 1, SDK):** `google.golang.org/genai` v1.64.0
`client.AuthTokens.Create` works. **Three protocol corrections vs §3.2/§3.4 discovered
and verified live (the §3.2 endpoint is for API-key auth only):**

1. **Mint client MUST be constructed with `HTTPOptions: genai.HTTPOptions{APIVersion: "v1alpha"}`**
   (mirrors the JS SDK's documented `httpOptions:{apiVersion:'v1alpha'}` requirement).
2. **Ephemeral tokens are only honored by a DIFFERENT WSS method:**
   `wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContentConstrained?access_token=<url-escaped token>`
   — NOT the v1beta `BidiGenerateContent` endpoint in §3.2 (that one closes with
   "Method doesn't allow unregistered callers" for token auth; it only accepts `?key=<api-key>`).
   Verified against the JS SDK source (`@google/genai` 2.12.0 `live.connect`): when
   `apiKey.startsWith('auth_tokens/')` it switches method to `BidiGenerateContentConstrained`
   and param name to `access_token`. **The `geminiEndpoint` field in the §3.4 bootstrap
   shape must carry the Constrained URL.** Token name must be URL-escaped in the query.
3. **Raw WSS setup frame nesting:** `responseModalities` and `speechConfig` live under
   `setup.generationConfig.{…}`, not at the top level of `setup` (the SDK's
   `LiveConnectConfig` flattens them; the wire protocol does not). `systemInstruction`,
   `inputAudioTranscription`, `outputAudioTranscription`, `sessionResumption` are
   top-level `setup` fields.

Smoke result (minted token, constrained endpoint, full setup frame sent client-side):
`setupComplete` ✔ → text turn → **31,682 bytes audio `audio/pcm;rate=24000`**, output
transcription "Hello.", `sessionResumptionUpdate` handle received ✔, `usageMetadata`
present (391 total tokens) ✔. Mint config used: `uses:1`, `expireTime:+30m`,
`newSessionExpireTime:+2m`, `liveConnectConstraints{model, config{AUDIO, Kore voice,
systemInstruction, sessionResumption, in/out transcription}}`.

REST fallback (rung 2) not needed — never exercised. Production minter (A3) uses the
genai SDK (adds `google.golang.org/genai` to go.mod).

**T1 — voice catalog: ALL 30 prebuilt-HD voices ACCEPTED** against
`gemini-3.1-flash-live-preview` (setupComplete + real audio synthesis each): Zephyr,
Puck, Charon, Kore, Fenrir, Leda, Orus, Aoede, Callirrhoe, Autonoe, Enceladus, Iapetus,
Umbriel, Algieba, Despina, Erinome, Algenib, Rasalgethi, Laomedeia, Achernar, Alnilam,
Schedar, Gacrux, Pulcherrima, Achird, Zubenelgenubi, Vindemiatrix, Sadachbia,
Sadaltager, Sulafat. This full list ships as `SupportedGeminiVoices` (A7).

Spike source: session scratchpad `gemini-spike/main.go` (not committed; recipe fully
captured above). API key was provided via a local drop file, consumed into process env
only, and deleted after the run.

### Phase A notes (2026-07-19)

- **A1–A8 complete; `go test ./...` fully green (17 packages).**
- `google.golang.org/genai` v1.64.0 added to go.mod (the Phase-0-proven mint path).
- `internal/realtime/gemini_mint.go`: `GeminiMinter` (loader-backed key, v1alpha
  client, `uses:1`/+30m/+2m token, constraints = full session config, SessionConfig
  echo in RAW wire shape). `GeminiLiveEndpoint` const carries the **Constrained**
  endpoint. Test seam: injectable `create` func.
- Broker: `handleGeminiDirect` mirrors `handleNovaBridge` (gate → voice/guides →
  mint → bookkeeping → §3.4 shape). Errors: `gemini_unavailable` / `mint_failed`
  (502, fallback-cascade compatible).
- Voice identity: `Persona.GeminiVoice` (D4b hand-curated map below),
  `ResolveGeminiVoiceChain` (setting ?? persona ?? Kore),
  `ResolveSessionGeminiVoice` (one GetItem: geminiVoice + voiceAccent +
  personaPrefs; accents reuse the OpenAI directive path).
- **D4b persona → Gemini voice map:** default→Achird · valley-girl→Leda ·
  logic-officer→Schedar · deputy-chief→Puck · noir-detective→Algenib ·
  bard→Enceladus · zen-monk→Vindemiatrix · drill-sergeant→Alnilam ·
  play-by-play→Laomedeia · butler→Iapetus · surfer→Zubenelgenubi ·
  worried-grandma→Gacrux · pirate-captain→Algenib · sommelier→Algieba ·
  heh-heh-duo→Zubenelgenubi · swamp-master→Enceladus · cool-intensity→Fenrir.
  (Custom/stored personas have no mapping → chain bottoms at Kore.)
- `SupportedGeminiVoices` = all 30 spike-validated voices; served as additive
  `geminiVoices` on GET /api/v1/realtime/voices. New settings key `geminiVoice`
  (lenient, absent→""), schema + validation + tests updated; both voiceEngine
  enums now include `gemini-flash-live`.
- rates.go: gemini model keyed at §3.3 prices; cached rates set EQUAL to uncached
  (Gemini has no input caching) so cache-shaped usage can't underprice the badge.
- template.yaml: `GeminiLiveModel` param → `GEMINI_LIVE_MODEL` env; `GeminiKeyParam`
  IAM Sid; deploy.yml syncs `GEMINI_API_KEY` secret → SSM (secret already set by
  owner 2026-07-19).
- Tests added: broker shape test incl. **no-wsUrl-family regression guard**, voice
  resolution table, unavailable/mint-failure, nova-untouched; minter constraint/
  setup-shape tests; tool-declaration mirror; persona-mapping totality; catalog
  pin (30 + Kore default); rates; settings validation cases.

### Phase B/C/D notes (2026-07-19)

- **Web (6efa476):** `#connectGemini` forked from the Nova WSS skeleton; binary-frame
  JSON handled via TextDecoder (Google sends JSON in binary frames); per-utterance
  synthetic itemIds (`g-user-N`/`g-asst-N`); goAway reconnects defer to the turn
  boundary while speaking (accepted edge: a close before turnComplete degrades to the
  normal connectionlost path); usage mapped into the OpenAI-shaped payload the cost
  badge prices (per-turn latest-wins — WATCH in E1: if Google ever streams cumulative
  counts the badge overcounts). Transcript engine tag: gemini sessions tag
  `gemini-flash-live`; OpenAI/Nova keep their model-id tags byte-for-byte (coordinator
  corrected the subagent's broader change). `commitTurn()` is a no-op (auto-VAD owns
  turn boundaries).
- **Android (35b89aa):** `GeminiLiveTransport` (~820 lines) forked from
  NovaBridgeTransport; new no-op `prime(session)` transport seam carries the setup
  frame without downcasts; parse extracted into pure `parseSession()` + regression
  tests. **Compile-unverified** (no JDK/Android SDK on the build machine; repo has no
  Android CI) — statically cross-checked against the broker contract.
- **Firmware (968d373, HIL-unverified):** implemented against the post-rebase
  decoupled-uplink refactor (upstream moved under us mid-flight; re-based cleanly).
  Gemini uplink bypasses `r32_process` (AFE-native 16 kHz). Binary WS frames parsed as
  JSON on the Gemini engine only. **Plan correction discovered:** firmware has NO
  tool-invoke plumbing on ANY engine (OpenAI function_calls are intentionally ignored
  on-device) — §4-D3's "same flow as the other modes" did not exist. Decision: every
  Gemini functionCall is answered immediately with a structured
  `{"error":"tool execution is not available on this device"}` toolResponse so turns
  never stall (strictly better than the OpenAI path's silent ignore); full on-device
  invocation is a **backlog item**. TLS: full IDF cert bundle already ships — no change.
- **Coordination event:** owner pushed 8 commits mid-execution (Tab5 SDIO/mbedTLS/
  uplink refactor + Android LWA broker auth); rebased before any agent had written,
  firmware agent re-read the refactored files from scratch.

### Phase E status (2026-07-19)

- **E3 (regression) `[x]`:** full `go test ./...` green post-everything (0 failures),
  `go vet` clean, `node --check` clean on all touched .mjs, render_test CSP updated,
  broker shape tests include the no-wsUrl-family guard, settings round-trip covered.
  OpenAI/Nova code paths verified byte-identical by review + tests.
- **E4 (docs) `[x]`:** docs/voice-engines.md three-engine, contracts/api.md third
  shape, plan.md M13 rows, this file.
- **E1/E2 `[!]` OWNER HANDOFF (live audio required — automation profile mic is
  hard-blocked, same constraint as M12's Nova verification):**
  1. Settings → Voice engine → pin a device (or the default) to **Gemini Flash Live**;
     optionally pick a Gemini voice (else persona mapping ?? Kore).
  2. Speak a few turns: expect audio replies, live captions, transcript in History
     tagged `gemini-flash-live`, cost column priced at Gemini rates.
  3. Ask it to use a tool (e.g. weather) — web/Android should execute it; the Tab5
     should say tool execution isn't available on-device.
  4. Barge-in mid-reply — playback should cut instantly.
  5. Keep one session past ~10 min (goAway recycle should be seamless) and past
     ~30 min (token re-mint + resume should be seamless).
  6. Switch personas and confirm the Gemini voice changes per the D4b mapping.

_(notes accrue here per task as execution proceeds)_
