# Voice engines (FR-VE-01..04)

Live Ninja speaks to three realtime speech-to-speech backends. Which one a given
session uses is decided **per device** by a stored pin, resolved server-side at
session bootstrap. The engines have fundamentally different network shapes,
and that difference is the whole reason this document exists.

| Engine pin value      | Backend                         | Media path                         | Where audio is relayed |
|-----------------------|---------------------------------|------------------------------------|------------------------|
| `openai-realtime`     | OpenAI Realtime (`gpt-realtime`)| **Client-direct** WSS to OpenAI    | Nowhere — client ⇄ OpenAI |
| `openai-realtime-mini`| OpenAI Realtime mini (`gpt-realtime-mini`) | **Client-direct** WSS to OpenAI | Nowhere — client ⇄ OpenAI |
| `nova-sonic`          | Amazon Bedrock **Nova Sonic** (`amazon.nova-sonic-v1:0`, `us-east-1`) | **Backend-bridged** WSS to our Nova bridge | client ⇄ Nova bridge ⇄ Bedrock |
| `gemini-flash-live`   | Google **Gemini Live API** (`gemini-3.1-flash-live-preview`, native audio; M13) | **Client-direct** WSS to Google | Nowhere — client ⇄ Google |

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

and returns **one of three shapes**:

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

| Surface   | OpenAI-direct | Nova-bridge | Gemini-direct | Notes |
|-----------|:-------------:|:-----------:|:-------------:|-------|
| Web (`realtime.mjs`)        | ✅ | ✅ | ⚠️ per-surface until verified | Triple path: WebRTC/WSS to OpenAI, WSS to the bridge, or WSS to Google. |
| Android (`RealtimeTransport`)| ✅ | ✅ | ⚠️ per-surface until verified | Same triple path. |
| M5Stack Tab5 (`ln_realtime`) | ✅ | ❌ **out of scope** | ⚠️ **HIL-unverified** | Its historical Nova branch predates the required signed-config handshake; the surface is backlog-only. |

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

---

## References

- `archive/plan.md` → **M12 — Secondary Voice Engine (Nova Sonic)** (DoD + task list; Nova is disabled — see `backlog.md`).
- `archive/gemini-plan.md` → **M13 — Tertiary Voice Engine (Gemini Flash Live)** (protocol facts, mint recipe, DoD).
- PRD → **FR-VE-01..04**.
- `contracts/api.md` → `GET /v1/realtime/session`, `WSS /nova/session`.
- `contracts/settings.schema.json` → `#/properties/voiceEngine`.
- `internal/voiceengine` → common event schema + normalizers.
- `firmware/components/ln_realtime` → M5Stack dual-path client.
