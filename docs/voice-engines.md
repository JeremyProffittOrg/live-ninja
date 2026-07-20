# Voice engines (FR-VE-01..04)

Live Ninja speaks to three realtime speech-to-speech backends. Which one a given
session uses is decided **per device** by a stored pin, resolved server-side at
session bootstrap. The engines have fundamentally different network shapes,
and that difference is the whole reason this document exists.

| Engine pin value      | Backend                         | Media path                         | Where audio is relayed |
|-----------------------|---------------------------------|------------------------------------|------------------------|
| `openai-realtime`     | OpenAI Realtime (`gpt-realtime`)| **Client-direct** WSS to OpenAI    | Nowhere — client ⇄ OpenAI |
| `openai-realtime-mini`| OpenAI Realtime mini            | **Client-direct** WSS to OpenAI    | Nowhere — client ⇄ OpenAI |
| `nova-sonic`          | Amazon Bedrock **Nova Sonic** (`amazon.nova-sonic-v1:0`, `us-east-1`) | **Backend-bridged** WSS to our Nova bridge | client ⇄ Nova bridge ⇄ Bedrock |
| `gemini-flash-live`   | Google **Gemini Live API** (`gemini-3.1-flash-live-preview`, native audio; M13) | **Client-direct** WSS to Google | Nowhere — client ⇄ Google |

`openai-realtime` is the platform default (`settings.schema.json#/properties/voiceEngine/default`).

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
is a small **ECS Fargate service (arm64, scale-to-1)** running a Go WSS server,
fronted by an ALB with the ACM cert on **`nova.live.jeremy.ninja`** (Route 53).
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
  "wsUrl": "wss://nova.live.jeremy.ninja/…?token=…",  // single-use token in the URL
  "token": "…",                                        // optional; usually already in wsUrl
  "sessionId": "…"
}
```

**Gemini-direct** (M13) — the client opens a WSS straight to Google's Live API:

```jsonc
{
  "mode": "gemini-direct",
  "engine": "gemini-flash-live",
  "model": "gemini-3.1-flash-live-preview",
  "geminiEndpoint": "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContentConstrained",
  "accessToken": { "value": "auth_tokens/…", "expiresAt": "…", "newSessionExpiresAt": "…" },
  "sessionConfig": { /* the exact `setup` frame body the client sends on open */ },
  "voice": "Kore",
  "sessionId": "…",
  "rates": { /* Gemini per-1M-token rates for the cost badge */ }
}
```

The token is **single-use** (session resumption reconnects don't count as a
use), constrained at mint to the exact model/voice/instructions/tools, and
carried as `?access_token=<url-escaped token>` on the WSS URL (browsers can't
set upgrade headers). Past `expiresAt` (~30 min) the client re-fetches this
route for a fresh token and resumes via its stored resumption handle. The
field names are deliberately outside the `wsUrl`/`bridgeUrl` family: legacy
clients detect Nova by field *presence*, so the Gemini shape must never trip
that heuristic.

The bridge token is **single-use and scoped to that one `sessionId`**. WebSocket
upgrade requests can't reliably carry a `Bearer` header across every client
stack (browsers especially), so the token rides in the URL query string rather
than a header (`contracts/api.md`, `/v1/realtime/bridge/{sessionId}`). The bridge
verifies it (and the underlying first-party JWT) and runs the quota gate before
it ever opens the Bedrock stream.

Clients branch on `mode`. Presence of `wsUrl` (or a `bridgeUrl`) is treated as
Nova even if `mode` is absent, so the exact broker spelling can be finalized
without breaking already-shipped clients.

---

## One event vocabulary across both engines (FR-VE-01)

Topics, memory, tools, transcripts, and barge-in must behave **identically** no
matter which engine answered. That is achieved by normalizing both engines onto
a **common event schema** (`internal/voiceengine`):

```
session.start | audio.in | audio.out | transcript |
tool.call | tool.result | turn.start | turn.end | error
```

- The bridge maps **Nova Sonic** events (tool-use, VAD/barge-in, transcript
  turns) onto this schema.
- The web/Android/broker code maps **OpenAI Realtime** events onto the same
  schema.
- Transcript turns from either engine are POSTed to the **same** transcript sink
  (`POST /api/v1/transcript`); function calls from either engine go to the
  **same** tool router (`POST /api/v1/tools/invoke`).

On the wire toward the *client*, the Nova bridge deliberately speaks the **same
pcm16 event framing** the clients already use for OpenAI (uplink
`input_audio_buffer.append`, downlink `response.output_audio.delta`, transcript
deltas, `response.cancel` for barge-in). That is what lets each client add Nova
support as a **transport switch only** — a different URL and auth — with no
change to audio encode/decode, playback, or barge-in logic.

Audio format is pcm16 both ways: **16 kHz mono uplink**, **24 kHz mono downlink**
(the bridge conforms Nova's stream to the same rates the OpenAI path uses).

---

## Client support matrix

| Surface   | OpenAI-direct | Nova-bridge | Gemini-direct | Notes |
|-----------|:-------------:|:-----------:|:-------------:|-------|
| Web (`realtime.mjs`)        | ✅ | ✅ | ⚠️ per-surface until verified | Triple path: WebRTC/WSS to OpenAI, WSS to the bridge, or WSS to Google. |
| Android (`RealtimeTransport`)| ✅ | ✅ | ⚠️ per-surface until verified | Same triple path. |
| M5Stack Tab5 (`ln_realtime`) | ✅ | ⚠️ **HIL-unverified** | ⚠️ **HIL-unverified** | Nova and Gemini branches implemented; not yet validated on hardware. |

### M5Stack firmware (`firmware/components/ln_realtime`)

The Tab5 firmware already ran an OpenAI-direct WSS client. M12 adds a
**nova-bridge branch** guarded by the `mode` field of the session-bootstrap
response:

- `ln_rt_session.c` parses **both** response shapes. OpenAI-direct resolves the
  ephemeral token + model as before; nova-bridge resolves `wsUrl` (+ optional
  `token`) and sets the session's engine mode. A `nova-bridge` response is no
  longer rejected (it was, pre-M12).
- `ln_realtime.c` (`ws_open`) branches on the mode:
  - **OpenAI-direct:** `wss://api.openai.com/v1/realtime?model=…` with
    `Authorization: Bearer ek_…`, and it sends `session.update` (pcm16 in/out)
    on connect — unchanged.
  - **Nova-bridge:** connects to `wsUrl` verbatim (token already in the URL; a
    separately-returned token is appended as a query param if not present), with
    **no** `Authorization` header and **no** OpenAI `session.update` — the bridge
    fixes pcm16 and owns session config server-side.
- Uplink (`input_audio_buffer.append`), downlink audio decode, transcript
  events, and local/VAD barge-in (`response.cancel`) are **shared** across both
  paths — same pcm16 framing.
- Reconnect re-fetches a **fresh** session each attempt, which correctly re-mints
  the single-use bridge token per reconnect.

> **HIL status:** the firmware Nova path is written and reviewed but has **not**
> been exercised against a live bridge on real Tab5 hardware. It is marked
> HIL-unverified until the PC↔Tab5 bidirectional smoke test covers a Nova-pinned
> device. The OpenAI-direct firmware path is unchanged and unaffected.

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
the `nova-bridge` response and connects to `nova.live.jeremy.ninja`.

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

- `plan.md` → **M12 — Secondary Voice Engine (Nova Sonic)** (DoD + task list).
- `gemini-plan.md` → **M13 — Tertiary Voice Engine (Gemini Flash Live)** (protocol facts, mint recipe, DoD).
- PRD → **FR-VE-01..04**.
- `contracts/api.md` → `GET /v1/realtime/session`, `WSS /v1/realtime/bridge/{sessionId}`.
- `contracts/settings.schema.json` → `#/properties/voiceEngine`.
- `internal/voiceengine` → common event schema + normalizers.
- `firmware/components/ln_realtime` → M5Stack dual-path client.
</content>
</invoke>
