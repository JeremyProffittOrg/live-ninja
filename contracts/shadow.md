# IoT Named Shadow — `config` (M5Stack)

Contract for the AWS IoT Device Shadow named shadow `config`, used to sync settings to the
M5Stack Tab5 (FR-S02, FR-M02, FR-M05, Q-19). This is the **transport**, not a second schema:
the shadow's `desired`/`reported` document bodies are the same `settings.schema.json` shape
(subset relevant to firmware), so a device and the backend agree on field meaning without a
translation layer. IoT Core here carries **control/telemetry only — never audio** (Q-19,
NFR-06, PRD §5).

## Shadow topics (per Thing, `liveninja/<thingName>` prefix per the IoT policy in the shared
spec)

| Topic | Direction | Purpose |
|---|---|---|
| `$aws/things/<thingName>/shadow/name/config/update` | Backend → IoT | Backend publishes a `desired` delta after a settings write (fan-out, PRD §5 "Wake-word config propagates"). |
| `$aws/things/<thingName>/shadow/name/config/update/delta` | IoT → Device | Device subscribes; receives only the fields that differ from its last `reported` state. |
| `$aws/things/<thingName>/shadow/name/config/update` (device-published) | Device → IoT | Device publishes its own `reported` state (on boot, after applying a delta, and on heartbeat). |
| `$aws/things/<thingName>/shadow/name/config/update/accepted` | IoT → Backend (IoT Rule) | `shadow-ingest` Lambda subscribes via an IoT Rule and reconciles `reported` back into DynamoDB `SETTINGS#v<n>` (plan.md M6 task). |
| `$aws/things/<thingName>/shadow/name/config/update/rejected` | IoT → Device/Backend | Malformed delta or version conflict; device logs + reports current state unchanged; backend alerts if repeated. |
| `$aws/things/<thingName>/shadow/name/config/get` / `.../get/accepted` | Device → IoT → Device | Device requests full current shadow on boot/reconnect (Boot → Idle/Provisioning transition, PRD §5 firmware state diagram). |

## Document shape

The shadow document nests everything under `state.desired` / `state.reported` per the
standard AWS IoT Shadow envelope; the payload **inside** each is the M5Stack-relevant subset
of `settings.schema.json`, plus two shadow-only bookkeeping fields (`settingsVersion`,
`deviceReportedAt`) that never appear in the DynamoDB settings item itself.

```jsonc
{
  "state": {
    "desired": {
      "settingsVersion": 42,          // mirrors settings.schema.json `version` at publish time
      "wakeWord": "hey-live-ninja",    // settings.schema.json#/properties/wakeWord
      "wakeEngine": "wakenet",         // settings.schema.json#/properties/wakeEngine — M5Stack is always "wakenet" or an oWW-ESP fallback id, never "openwakeword"/"porcupine"
      "sensitivity": 0.5,              // settings.schema.json#/properties/sensitivity
      "voice": "cedar",                // settings.schema.json#/properties/voice
      "turnDetection": "semantic_vad", // settings.schema.json#/properties/turnDetection
      "voiceEngine": "openai-realtime",// resolved value for THIS device: settings.schema.json#/properties/voiceEngine/devices/<thisDeviceId> ?? .../default
      "wakeModel": {
        "url": null,                  // filled in by the device from wakeword-manifest.md when wakeWord/wakeEngine changes; shadow only ever carries the wakeWord ID, never the asset
        "sha256": null
      }
    },
    "reported": {
      "settingsVersion": 42,
      "wakeWord": "hey-live-ninja",
      "wakeEngine": "wakenet",
      "sensitivity": 0.5,
      "voice": "cedar",
      "turnDetection": "semantic_vad",
      "voiceEngine": "openai-realtime",
      "wakeModelSha256Applied": "…",
      "firmwareVersion": "1.4.2",
      "deviceReportedAt": "2026-07-17T20:31:00Z"
    }
  }
}
```

Notes:
- Only fields the device firmware actually understands are ever placed in `desired` by the
  backend for that Thing (the backend knows the device's `X-LN-Client` build — see
  `headers.md` — and trims accordingly); this keeps `desired` deltas small and avoids
  spamming a 10-year-old firmware build with fields it can't act on. This is a **transport
  optimization**, not a schema restriction — the canonical settings document in DynamoDB
  always carries the full `settings.schema.json` shape regardless of what any one device
  understands.
- `persona.systemInstructions` and `micDeviceId` are **not** shadowed — they are not
  meaningful to M5Stack firmware (persona resolves server-side at session mint; there is no
  selectable mic device on a fixed-hardware terminal).
- `privacy.*` (storeAudio/storeTranscripts/retentionDays) IS shadowed read-only for
  on-screen disclosure (the M5Stack "always show a persistent listening indicator" UX
  requirement) but the device never writes it back — only web/Android settings pages write
  privacy fields.

## Reconciliation rule: higher `version` wins

This is the same optimistic-concurrency mechanism as `settings.schema.json`'s `version`
field, just carried over IoT instead of HTTP (FR-S02, plan.md M6 DoD "higher-version-wins
reconciliation").

1. Every settings write (from any surface) increments `SETTINGS#v<n>`'s `version` in
   DynamoDB and re-publishes the full relevant subset to `desired` with the new
   `settingsVersion`.
2. The device applies a `desired` delta **only if** `desired.settingsVersion >
   reported.settingsVersion` it currently holds. If the device's local clock or a network
   partition ever causes it to see a `settingsVersion` <= what it already reported, it
   ignores the delta and re-publishes its own current `reported` unchanged (self-healing —
   the backend will see the mismatch on `update/accepted` and re-publish `desired` once
   more with the correct, current version).
3. After successfully applying a delta, the device publishes `reported` with the new
   `settingsVersion` and any resulting fields (e.g. `wakeModelSha256Applied` once the new
   wake model is hot-swapped and verified — see `wakeword-manifest.md`).
4. `shadow-ingest` Lambda (subscribed to `update/accepted`) writes the device's `reported`
   state back toward DynamoDB **only as a device-liveness/telemetry confirmation** — it must
   never downgrade the canonical `SETTINGS#v<n>` item's `version`; if `reported.settingsVersion`
   is lower than the current canonical version (the device hasn't caught up yet), the
   ingest Lambda takes no settings-side action beyond updating `DEVICE#<id>` `lastSeen`/
   `reportedVersion` telemetry attributes (`PutItem`, never `Scan`, per FR-B06).
5. Conflicting concurrent writers (web edits settings while M5Stack is offline) are resolved
   the same way any two writers are: DynamoDB's `version` is the single source of truth:
   whichever write lands last with the correct `ConditionExpression version = expected` wins
   and becomes the new `desired`; the loser gets HTTP 409 (web/Android path) or is simply
   superseded on next `desired` publish (device path, which is not request/response so there
   is no 409 — the device just eventually converges to the latest `desired`).

## Failure / offline handling

- A device that is offline when `desired` is published receives it on next connect via the
  shadow's persistent nature (AWS IoT retains `desired` regardless of connection state) — no
  special retry logic needed backend-side.
- `update/rejected` (malformed payload, e.g. a version regression attempt) is logged by the
  device to its local telemetry buffer and surfaced on next MQTT heartbeat publish
  (`liveninja/<thing>/telemetry`) so `iot-ingest` can flag it for the ops alarm (NFR-06).
- Provisioning-time bootstrap: on first bind (PRD §7 M5Stack pairing sequence), the backend
  seeds `desired` with the user's current full settings document immediately after IoT Thing
  creation, so the device's very first `get` returns a complete config rather than relying
  on an initial delta.
