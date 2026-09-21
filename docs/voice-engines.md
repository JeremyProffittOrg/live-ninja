# Voice engines (FR-VE-01..04)

Live Ninja speaks through eight realtime speech-to-speech engines across five
providers (OpenAI, Amazon, Google, Azure OpenAI, Azure AI Voice Live). Which
one a given session uses is decided **per device** by a stored pin, resolved
server-side at session bootstrap. The engines have fundamentally different
network shapes, and that difference is the whole reason this document exists.

| Engine pin value | Backend | Media path | Where audio is relayed |
|------------------|---------|------------|------------------------|
| `openai-realtime` | OpenAI Realtime (`gpt-realtime`) | **Client-direct** WebRTC to OpenAI | Nowhere — client ⇄ OpenAI |
| `openai-realtime-mini` | OpenAI Realtime mini (`gpt-realtime-mini`) | **Client-direct** WebRTC to OpenAI | Nowhere — client ⇄ OpenAI |
| `nova-sonic` | Amazon Bedrock **Nova Sonic** (`amazon.nova-sonic-v1:0`, `us-east-1`) | **Backend-bridged** WSS to our Nova bridge | client ⇄ Nova bridge ⇄ Bedrock |
| `gemini-flash-live` | Google **Gemini Live API** (`gemini-3.1-flash-live-preview`, native audio) | **Client-direct** WSS to Google | Nowhere — client ⇄ Google |
| `gpt-live-azure` | Azure OpenAI Realtime, deployment `gpt-realtime-2-1` | **Client-direct** WebRTC to Azure | Nowhere — client ⇄ Azure |
| `gpt-live-azure-mini` | Azure OpenAI Realtime, deployment `gpt-realtime-2-1-mini` | **Client-direct** WebRTC to Azure | Nowhere — client ⇄ Azure |
| `azure-voice-live` | Azure AI Voice Live, public preview, no SLA. Model is `AZURE_VOICELIVE_MODEL` (default `gpt-4o-mini-realtime-preview`) | **Client-direct** WSS to Azure | Nowhere — client ⇄ Azure |
| `azure-voice-live-lite` | Same Voice Live credential and the same model env as `azure-voice-live` today. Preview, no SLA | **Client-direct** WSS to Azure | Nowhere — client ⇄ Azure |

`openai-realtime` is the platform default. Choosing `gpt-live-azure` is a
provider and data-residency decision, not a cost saving: its audio rates match
`gpt-realtime`. The two Voice Live rows are preview, have no SLA, and their
session configuration is **not** enforced server-side. See
[The token problem, stated honestly](#the-token-problem-stated-honestly).

`openai-realtime` is the platform default (`settings.schema.json#/properties/voiceEngine/default`).
The mini pin uses its own fixed minter rather than inheriting `OPENAI_REALTIME_MODEL`, so its
response and ledger cannot claim mini while silently requesting the full model. OpenAI currently
marks the [`gpt-realtime-mini` alias as deprecated](https://developers.openai.com/api/docs/models/gpt-realtime-mini);
the project preserves its explicit PRD target here, with a future model migration kept separate
from this routing correction.

---

## Why Nova needs a bridge (and OpenAI does not)

OpenAI Realtime hands out a short-lived, config-bound **ephemeral token**. Every
client — web, Android, and the M5Stack Tab5 — opens a WebSocket *directly* to
`wss://api.openai.com/v1/realtime` with that token and streams pcm16 both ways.
No AWS compute ever sits in the audio path. This is cheap (no media egress, no
always-on service) and low-latency, and it is unchanged by M12.

Bedrock Nova Sonic is different by construction. Its bidirectional streaming API
(`InvokeModelWithBidirectionalStream`) is an **HTTP/2 stream signed with SigV4**
and a **server-held session** — there is no client-mintable ephemeral credential,
and you cannot hand a browser or an ESP32 the SigV4 signing keys. So a Nova
session **must** be terminated by our own backend, which holds:

1. the **client-facing WSS** (audio to/from the device), and
2. the **Bedrock bidirectional stream** (audio to/from Nova Sonic),

and pumps audio between them for the life of the turn. That backend is the
**Nova bridge**.

### The bridge is a small Fargate service, not a Lambda

A Lambda behind an API Gateway WebSocket API is request/response *per frame* — it
cannot hold an open HTTP/2 stream to Bedrock for the duration of a session.
Bedrock's bidirectional streaming needs a long-lived held socket. So the bridge
is a small **ECS Fargate service (arm64, scale-to-1)** running a Go WSS server.
Its ALB is reached through CloudFront's same-origin `/nova/*` behavior, so
clients connect to **`wss://live.jeremy.ninja/nova/session`** and no separate
public bridge hostname is required.
It is deliberately the only place in the whole product where AWS is in the audio
media path. See `template.yaml` for the task definition / service / listener and
the bridge source for the audio pump.

---

## Session bootstrap: how a client learns which path to take (FR-VE-03)

Every surface calls the same route to start a session:

```
GET /api/v1/realtime/session      (Authorization: session JWT; X-LN-Client header)
```

The realtime broker resolves the engine for this session as:

```
engine = voiceEngine.devices[deviceId]  ??  voiceEngine.default
```

and returns **one of five shapes**:

**OpenAI-direct** (default) — the client opens a WSS straight to OpenAI:

```jsonc
{
  "mode": "openai-direct",           // may be omitted on legacy responses
  "clientSecret": { "value": "ek_…", "expiresAt": "2026-07-18T12:00:00Z" },
  "model": "gpt-realtime",
  "voice": "cedar",
  "sessionId": "…"
}
```

**Nova-bridge** — the client opens a WSS to our bridge instead:

```jsonc
{
  "mode": "nova-bridge",
  "wsUrl": "wss://live.jeremy.ninja/nova/session?token=…", // single-use token in the URL
  "token": "…",                                        // optional; usually already in wsUrl
  "sessionConfig": { /* server-resolved prompt + server-executable tools */ },
  "sessionId": "…"
}
```

The client sends `{"type":"session.start","config":<sessionConfig>}` as the
first WebSocket frame and does not start microphone capture until the bridge
acknowledges with `{"type":"session.start"}`. The bridge token carries a signed
SHA-256 digest of that config; the bridge canonicalizes and verifies the first
frame before opening Bedrock, so the relay client cannot change persona,
instructions, or tool policy. Because Nova executes tool calls in the bridge,
the config deliberately excludes every device-local tool.

**Gemini-direct** (M13) — the client opens a WSS straight to Google's Live API:

```jsonc
{
  "mode": "gemini-direct",
  "engine": "gemini-flash-live",
  "model": "gemini-3.1-flash-live-preview",
  "geminiEndpoint": "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained",
  "accessToken": { "value": "auth_tokens/…", "expiresAt": "…", "newSessionExpiresAt": "…" },
  "sessionConfig": { /* the exact `setup` frame body the client sends on open */ },
  "voice": "Kore",
  "sessionId": "…",
  "rates": { /* Gemini per-1M-token rates for the cost badge */ }
}
```

The token is **single-use** (session-resumption reconnects don't count as a
use), constrained at mint to the exact model/voice/instructions/tools while
leaving only `sessionResumption` client-controlled, and carried as
`?access_token=<url-escaped token>` on the WSS URL (browsers can't set upgrade
headers). While the original token is valid, clients can replace a dropped or
`goAway` socket with the latest server-issued resumption handle. At
`expiresAt`, clients fail closed: the ordinary mint route creates a new broker
`sessionId`, ledger marker, and concurrency slot, so it cannot safely continue
the old logical session. Long-lived continuation needs an authenticated
same-session renewal contract before it can be enabled. The field names are
deliberately outside the `wsUrl`/`bridgeUrl` family: legacy clients detect Nova
by field *presence*, so the Gemini shape must never trip that heuristic.

**Azure-direct** (`gpt-live-azure` and `gpt-live-azure-mini`) — the same WebRTC
path as OpenAI, with `callsUrl` pointing at the Azure `/calls` host:

```jsonc
{
  "mode": "azure-direct",
  "callsUrl": "https://ln-aoai-eastus2.openai.azure.com/openai/v1/realtime/calls",
  "clientSecret": { "value": "ek_…", "expiresAt": "…" },
  "model": "gpt-realtime-2-1",
  "voice": "cedar",
  "sessionId": "…"
}
```

`cedar` is accepted on that Azure deployment, so the OpenAI voice catalog is
reused. The client posts SDP to `callsUrl` with `Authorization: Bearer ek_…`.

**Voice-live-direct** (`azure-voice-live` and `azure-voice-live-lite`) — the
client opens a control WSS with an Entra bearer token and sends the
server-authored `sessionConfig` inside `rtc.call.sdp.create`. The field names
are not `wsUrl` or `bridgeUrl`:

```jsonc
{
  "mode": "voice-live-direct",
  "engine": "azure-voice-live",
  "model": "gpt-4o-mini-realtime-preview",
  "voiceLiveEndpoint": "wss://ln-voicelive.services.ai.azure.com/voice-live/realtime/calls?api-version=2026-01-01-preview&model=gpt-4o-mini-realtime-preview",
  "accessToken": { "value": "…", "expiresAt": "…" },
  "sessionConfig": { /* the session object the client must send; not enforced */ },
  "voice": "cedar",
  "sessionId": "…"
}
```

The Voice Live credential is a resource-scoped Entra token. It is not bound to
one session and it is not bound to the server's config. The cost badge is
suppressed for these two models because no published rate row exists
(`rates_missing`). Do not invent a number.

The bridge token is **single-use, scoped to that one `sessionId`, and bound to
the exact server-generated session config**. WebSocket
upgrade requests can't reliably carry a `Bearer` header across every client
stack (browsers especially), so the token rides in the URL query string rather
than a header (`contracts/api.md`, `/nova/session`). The bridge
verifies it (and the underlying first-party JWT), validates the live session
slot, and atomically marks that slot redeemed before it ever opens the Bedrock
stream. A racing or replayed connection is rejected; reconnect first fetches a
fresh session.

Clients branch on `mode`. Presence of `wsUrl` (or a `bridgeUrl`) is treated as
Nova even if `mode` is absent, so the exact broker spelling can be finalized
without breaking already-shipped clients.

---

## One event vocabulary across all engines (FR-VE-01)

Topics, memory, tools, transcripts, and barge-in must behave **identically** no
matter which engine answered. That is achieved by normalizing every engine onto
a **common event schema** (`internal/voiceengine`):

```
session.start | audio.in | audio.out | user.text | transcript |
tool.call | tool.result | turn.start | turn.end | error
```

- The bridge maps **Nova Sonic** events (tool-use, VAD/barge-in, transcript
  turns) onto this schema.
- The web/Android/broker code maps **OpenAI Realtime** events onto the same
  schema.
- Transcript turns from every engine are POSTed to the **same** transcript sink
  (`POST /api/v1/transcript`); function calls from either engine go to the
  **same** tool router (`POST /api/v1/tools/invoke`).

On the Nova client wire, audio and control are deliberately separated:

- binary WebSocket frames carry raw little-endian mono PCM16 in both directions
  (microphone uplink and assistant downlink);
- JSON text frames carry `session.start`, transcript/turn lifecycle, and
  errors. The shared `user.text` operation returns a typed unsupported error
  on Nova Sonic v1 because v1 permits TEXT only as pre-audio history, not as
  an interactive mid-stream turn;
- Nova server VAD detects barge-in from the continuous microphone stream; an
  interrupted assistant `turn.end` makes web/Android clear queued playback
  immediately, while the bridge drops trailing audio until the next completion;
- Nova's manifest contains server-executable tools only. The bridge executes
  those calls exactly once and returns results to Nova without redispatching
  them to the client.

Audio format is pcm16 both ways: **16 kHz mono uplink**, **24 kHz mono downlink**
(the bridge adapter converts raw binary frames to/from Nova's base64 event
payloads).

---

## Client support matrix

| Surface | openai-realtime | openai-realtime-mini | nova-sonic | gemini-flash-live | gpt-live-azure | gpt-live-azure-mini | azure-voice-live | azure-voice-live-lite | Notes |
|---------|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|-------|
| Web (`realtime.mjs`) | yes | yes | yes | yes | yes | yes | yes | yes | WebRTC for the OpenAI and Azure OpenAI pins. WSS for Gemini and Voice Live. WSS to the bridge for Nova. |
| Android (`RealtimeTransport`) | yes | yes | yes | yes | yes | yes | yes | yes | Same paths. A spoken turn on the Galaxy S9 is still unverified. |
| M5Stack Tab5 (`ln_realtime`) | yes | no | no | no | no | no | no | no | OpenAI-direct only. Azure engines are out of scope on this surface. |

Both Voice Live pins share one broker model (`AZURE_VOICELIVE_MODEL`). The rate table names `azure-realtime` and `phi4-mm-realtime` as `rates_missing`. The broker does not select `phi4-mm-realtime` for the lite pin yet.

### M5Stack firmware (`firmware/components/ln_realtime`)

The Tab5 firmware already runs an OpenAI-direct WSS client. Its historical M12
**nova-bridge branch** is guarded by the `mode` field of the session-bootstrap
response, but it predates the required signed-config bootstrap:

- `ln_rt_session.c` parses the Nova URL/token but does **not** yet retain the
  returned `sessionConfig`.
- `ln_realtime.c` (`ws_open`) branches on the mode:
  - **OpenAI-direct:** `wss://api.openai.com/v1/realtime?model=…` with
    `Authorization: Bearer ek_…`, and it sends `session.update` (pcm16 in/out)
    on connect — unchanged.
  - **Nova-bridge:** can connect to `wsUrl`, but does not send the mandatory
    first `session.start` config frame and therefore must not be enabled.
- Its legacy JSON/base64 audio and control mapping also predates the bridge's
  current raw-binary PCM contract and would need to be updated with the signed
  config handshake.
- Reconnect re-fetches a **fresh** session each attempt, which correctly re-mints
  the single-use bridge token per reconnect.

> **Scope/status:** Tab5 is outside the active plan. Before Nova can be enabled
> there, firmware must parse `sessionConfig`, send it as the first frame, await
> the bridge ACK, and then pass a real hardware smoke. OpenAI-direct is
> unchanged and unaffected.

---

## How to pin a device to Nova (FR-VE-04)

The per-device engine picker lives in **Settings** on web and Android (a
segmented control / list: *OpenAI Realtime · OpenAI Realtime Mini · Nova Sonic ·
Gemini Flash Live*,
with the cost/tradeoff note below). Picking an engine for a device writes:

```jsonc
// settings document, voiceEngine block
{
  "voiceEngine": {
    "default": "openai-realtime",
    "devices": {
      "DEVICE#<deviceId>": "nova-sonic"   // this one device now routes to the bridge
    }
  }
}
```

Only the pinned device changes; every other device keeps falling back to
`default` and stays client-direct. The next session that device bootstraps gets
the `nova-bridge` response and connects to the same-origin
`wss://live.jeremy.ninja/nova/session` endpoint.

`deviceId` keys are the caller's own `DEVICE#<id>` ids (from `GET /v1/devices`,
which backs the picker). An absent key ⇒ `default`.

---

## Cost / tradeoff note (surface this in the picker)

The pin exists so an individual device can trade latency/quality for cost, or
route to a different provider — but Nova is **not free of infrastructure cost**,
and that is the honest tradeoff to weigh:

- **OpenAI-direct (`openai-realtime` / `-mini`):** audio never touches our AWS
  account — **zero backend media cost**, lowest hop count, lowest latency. You
  pay OpenAI's per-minute realtime rate. `-mini` is the cheaper OpenAI tier for a
  quality tradeoff. This is the default for a reason.
- **Gemini-direct (`gemini-flash-live`):** audio never touches our AWS account
  either — same zero-infra shape as OpenAI-direct, at roughly **10× cheaper
  audio rates than `gpt-realtime`** (and 2–3× cheaper than `-mini`) as of
  2026-07. Caveats: the model is **Preview** status (opt-in per device for a
  reason), and Gemini Live has no audio-input caching, which narrows the gap on
  long sessions. Picker copy angle: *cheapest engine, no infrastructure cost,
  preview-status model.*
- **Nova-bridge (`nova-sonic`):** you pay **Bedrock Nova Sonic** per-token
  speech pricing **plus** the always-on cost of the Nova bridge — one tiny
  arm64 Fargate task kept at scale-to-1, its ALB, and cross-service audio egress.
  Even when Nova's per-minute model rate undercuts OpenAI, the standing Fargate +
  ALB baseline means Nova only wins on total cost at **sustained** usage on the
  pinned device; for an occasionally-used device the always-on baseline can make
  it *more* expensive overall. It also adds one network hop (device → bridge →
  Bedrock) versus the direct path.

> Confirm current Bedrock Nova Sonic and OpenAI Realtime published rates before
> quoting hard numbers in the UI — provider pricing moves. The **architectural**
> cost difference above (zero-media-cost direct vs. always-on-bridge Nova) is the
> stable, decision-relevant point to show users.

**Rule of thumb for the picker copy:** keep high-traffic, always-listening
devices on the default OpenAI path unless you specifically want Nova's voice or
provider; reserve `nova-sonic` for devices where you deliberately want Bedrock in
the loop and the usage is steady enough to amortize the bridge.

- **`gpt-live-azure`:** same list audio rates as `gpt-realtime` (32.00 in / 64.00 out per 1M). No standing infrastructure. The deployment name on the wire is `gpt-realtime-2-1`.
- **`gpt-live-azure-mini`:** audio 10.00 in / 20.00 out per 1M. Deployment name `gpt-realtime-2-1-mini`.
- **`azure-voice-live` and `azure-voice-live-lite`:** no fixed monthly cost. No published per-token rate is stored, so the cost badge is suppressed. Preview, no SLA. The session config is not enforced server-side.

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

## References

- `archive/plan.md` → **M12 — Secondary Voice Engine (Nova Sonic)** (DoD + task list; Nova is disabled — see `backlog.md`).
- `archive/gemini-plan.md` → **M13 — Tertiary Voice Engine (Gemini Flash Live)** (protocol facts, mint recipe, DoD).
- PRD → **FR-VE-01..04**.
- `contracts/api.md` → `GET /v1/realtime/session`, `WSS /nova/session`.
- `contracts/settings.schema.json` → `#/properties/voiceEngine`.
- `internal/voiceengine` → common event schema + normalizers.
- `firmware/components/ln_realtime` → M5Stack dual-path client.
