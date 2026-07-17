# `X-LN-Client` / `X-LN-Server` Headers + `GET /v1/compat`

Capability-negotiation contract (NFR-08, Q-19, PRD §12.3 "Long-lived M5Stack devices outrun
API changes"). This is what lets a 10-year-old M5Stack firmware build and a same-day web
deploy both talk to `/v1` safely.

## `X-LN-Client` (request header, sent by every client on every request)

```
X-LN-Client: <surface>/<semver>+<build>
```

- `<surface>` — one of `web` | `android` | `m5stack`. Matches `telemetry.schema.json`'s
  `surface` enum (minus `backend`, which is server-only).
- `<semver>` — the client application's own semantic version, `MAJOR.MINOR.PATCH` (no `v`
  prefix). This is the **client app/firmware version**, not the `/v1` API path version.
- `<build>` — an opaque build identifier: a short git SHA (web/Android CI) or a firmware
  build string (M5Stack, matches `shadow.md`'s `reported.firmwareVersion` free-form part —
  e.g. `g1a2b3c4` or `20260717-1`).

Example:

```
X-LN-Client: m5stack/1.4.2+20260717-1
X-LN-Client: web/0.9.0+g1a2b3c4
X-LN-Client: android/2.1.0+r48
```

Parsing regex (reference, all server-side parsers must accept this and reject/ignore
malformed headers gracefully rather than erroring the whole request — a missing/malformed
header degrades to "assume oldest supported client" behavior, never a 5xx):

```
^(web|android|m5stack)/(\d+\.\d+\.\d+)\+([A-Za-z0-9._-]+)$
```

## `X-LN-Server` (response header, sent on every response)

```
X-LN-Server: <deployedSemver>+<gitSha>
```

- `<deployedSemver>` — the backend's own release version (bumped per deploy per whatever
  scheme the repo settles on; not tied to `/v1`'s path version, which never changes across
  the field lifetime described in NFR-08).
- `<gitSha>` — short SHA of the deployed commit, for support/debugging correlation with
  `gh run watch` deploy logs.

Example:

```
X-LN-Server: 1.12.0+9b7e156
```

## `GET /v1/compat` (Public route)

```
GET /v1/compat
X-LN-Client: m5stack/1.4.2+20260717-1
```

No auth required (it must be reachable by a device that cannot yet authenticate — e.g.
during onboarding, or a device whose credentials are being rotated/have expired). Response:

```jsonc
{
  "apiVersion": "v1",
  "minSupportedClientVersion": {
    "web": "0.5.0",
    "android": "1.0.0",
    "m5stack": "1.0.0"
  },
  "recommendedClientVersion": {
    "web": "0.9.0",
    "android": "2.1.0",
    "m5stack": "1.4.2"
  },
  "status": "supported",           // "supported" | "deprecated" | "unsupported"
  "message": null,                  // human-readable string when status != "supported", else null
  "serverTime": "2026-07-17T20:31:00Z"
}
```

| Field | Type | Notes |
|---|---|---|
| `apiVersion` | string | Always `"v1"` for the entire field lifetime per NFR-08 — present so a client can sanity-check it's talking to the API it expects, not so it can branch on it changing. |
| `minSupportedClientVersion` | object | Per-surface floor. A client below its floor gets `status: "unsupported"` and should show/log a "please update" state (plan.md M7 "below-min 'please update' states") rather than attempting normal operation. |
| `recommendedClientVersion` | object | Per-surface target; used for soft "update available" nudges — never blocking. |
| `status` | enum | Computed by the server from the **calling client's own** `X-LN-Client` header against the two version maps above: `unsupported` (below `minSupportedClientVersion` for its surface), `deprecated` (at or above min but below recommended by more than one MAJOR version), `supported` (otherwise). If `X-LN-Client` is missing/malformed, the server responds as if the client is at version `0.0.0` for its best-guessed surface (or `unsupported` outright if surface can't be inferred) — fail toward telling an old/broken client to update, never toward silently assuming it's fine. |
| `message` | string\|null | Present (non-null) whenever `status != "supported"`; human-readable, safe to render directly (e.g. M5Stack LVGL "Error/reconnect" screen, PRD §4.2 UX-L02, or a web/Android "please update" banner). |
| `serverTime` | string (ISO-8601 UTC) | Lets a client with no reliable RTC (M5Stack post-flash, pre-NTP) sanity-check/seed its clock — relevant because `shadow.md`'s version-reconciliation and this device's own cert rotation both reason about time. |

### Who calls this and when

- **M5Stack**: on every boot (Boot → Idle/Provisioning transition, PRD §5 state diagram)
  before attempting a realtime session, and periodically thereafter (e.g. once per 24h
  alongside its credential-rotation cycle, `deploy.md`/Auth §6) so a firmware left running
  for years still gets a timely "please update" or "unsupported, here's why" signal even
  though nobody is actively pushing OTA to it.
- **Web/Android**: on app foreground/page load, best-effort, to drive the soft update-nudge
  banner; never blocks the click-to-talk / core conversation path (FR-W03's "guaranteed
  click-to-talk fallback" takes precedence — a `compat` check failure or `deprecated` status
  must never prevent a basic voice turn from working).
- **Backend internal use**: the `authorizer` and `web` Lambdas do **not** gate requests on
  `/v1/compat` status themselves (that would make an old device's every single request pay
  an extra round trip's worth of logic) — `/v1/compat` is a client-pulled advisory endpoint,
  not a server-side request gate. A genuinely broken-beyond-repair old client is handled by
  normal API error responses (4xx) on the routes it actually calls, not by this endpoint.

### Versioning discipline

Per `contracts/README.md`: `minSupportedClientVersion` only ever moves forward (a version
once supported may later become unsupported as the fleet ages out ancient
firmware — this is the ONE field in this whole contracts directory that is explicitly
allowed to narrow support over time, since it describes fleet policy, not wire-format
compatibility). The `/v1` **path** and this endpoint's own **response shape**, by contrast,
follow the standard additive-only rule and never break.
