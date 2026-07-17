# Wake-word Model Manifest — `GET /v1/wakeword/{id}/model`

Contract for per-platform, content-addressed wake-word model distribution (FR-K04) and its
place in the training pipeline (FR-K03). Referenced from `api.md` and from `shadow.md`'s
`wakeModel` shadow field.

## Endpoint

```
GET /v1/wakeword/{id}/model?platform={web|android|esp32}
Authorization: Bearer <session JWT>   (or __Host- cookie on web)
```

- `{id}` — the wake-word catalog/user-model ID (same value as `settings.schema.json`'s
  `wakeWord` field, and the `wwId` referenced by plan.md M6's training pipeline task).
- `platform` — required. One model asset can differ per platform (e.g. `.tflite`/`.onnx`
  for Android/web openWakeWord runtimes vs. a curated flashable WakeNet blob for `esp32`,
  FR-K05).
- Auth: **Session JWT required** — this is not a Public route (contrast with the read-only
  `wakeword catalog snapshot` served from S3/CloudFront per plan.md M6 "no live table read on
  public path", which is a separate, unauthenticated static JSON list of catalog entries and
  does not carry model bytes).

## Response schema

```jsonc
{
  "id": "hey-live-ninja",
  "platform": "esp32",
  "engine": "wakenet",              // one of settings.schema.json#/properties/wakeEngine enum: openwakeword | porcupine | wakenet
  "format": "wakenet-esp32-v9",      // engine+platform-specific asset format tag; see table below
  "url": "https://cdn.live.jeremy.ninja/wakewords/hey-live-ninja/esp32/model.bin?X-Amz-...",
  "sha256": "3f9a2c1e...",           // hex sha256 of the exact bytes at `url`; client MUST verify before hot-swap
  "sizeBytes": 184320,
  "expiresAt": "2026-07-17T21:00:00Z"
}
```

| Field | Type | Notes |
|---|---|---|
| `id` | string | Echoes the requested `{id}`. |
| `platform` | string enum `web`\|`android`\|`esp32` | Echoes the requested `platform` query param. |
| `engine` | string enum `openwakeword`\|`porcupine`\|`wakenet` | Determines which runtime on the client loads the asset; must match `settings.schema.json`'s `wakeEngine` enum exactly so a client can validate compatibility before attempting to load. |
| `format` | string | Free-form but stable per (engine, platform) pair — see format table below. Lets a client refuse to load an asset shape its runtime version doesn't understand, and request a different `platform` value or fall back to the default model instead of crashing (FR-K04 "unsupported format → default model + report"). |
| `url` | string (URL) | Presigned S3 URL (web/Android) or CloudFront-signed URL (any platform behind the CDN). Short-lived — see `expiresAt`. |
| `sha256` | string (64 lowercase hex chars) | SHA-256 of the object at `url`. Client fetches, hashes, compares **before** swapping in the new model; mismatch = reject, keep previous model, report via telemetry (`telemetry.schema.json` event `wakeword_model_verify_failed`). |
| `sizeBytes` | integer | Expected content length; used as a cheap pre-check before the (potentially slow, on ESP32) full hash. |
| `expiresAt` | string (ISO-8601 UTC) | `url` validity window. Re-request the manifest endpoint after expiry rather than caching the presigned URL — the manifest endpoint itself is cheap (DynamoDB GetItem, not Scan) and re-mintable at will. |

## Format tags by (engine, platform)

| engine | platform | `format` value | Asset |
|---|---|---|---|
| `openwakeword` | `web` | `oww-onnx-web-v1` | int8 `.onnx`, loaded in an AudioWorklet via WASM runtime (FR-W03). |
| `openwakeword` | `android` | `oww-tflite-android-v1` | `.tflite`, loaded by the Android `WakeWordEngine` (FR-N02). |
| `porcupine` | `android` | `ppn-android-v1` | Picovoice `.ppn`, requires the user's Porcupine AccessKey already configured (progressive disclosure field, UX-R05). |
| `wakenet` | `esp32` | `wakenet-esp32-v9` | Curated flashable WakeNet model from Espressif's ESP-SR set (FR-M02, FR-K05) — select-only on device, never trained on device. |
| `openwakeword` | `esp32` | `oww-esp-int8-v1` | oWW-ESP fallback used when the requested phrase has no curated WakeNet equivalent (FR-K05). |

New (engine, platform, format) combinations may be **added** in later milestones (additive,
per `contracts/README.md` rule 1/3); an existing tag's meaning is never changed.

## Client verification + hot-swap sequence (all platforms)

1. Client observes `wakeWord`/`wakeEngine` change (settings sync — web via WebSocket frame,
   Android via FCM data message, M5Stack via shadow `desired` delta per `shadow.md`).
2. Client calls `GET /v1/wakeword/{id}/model?platform=<self>`.
3. Client downloads `url`, computes SHA-256 over the full byte stream.
4. If computed hash == `sha256` field: atomically swap the active model (old model stays
   loaded/active until the new one is fully verified — no listening gap, no crash-prone
   half-loaded state); on M5Stack, this also updates the shadow's
   `reported.wakeModelSha256Applied` (see `shadow.md`).
5. If the hash mismatches, or `format` isn't understood by the running client version:
   **do not swap.** Keep the previous model active, emit a `wakeword_model_verify_failed`
   (hash mismatch) or `wakeword_model_unsupported_format` telemetry event
   (`telemetry.schema.json`), and fall back to the platform's shipped default model
   (`hey-live-ninja`) if no previous custom model was already active (FR-K04's "unsupported
   format → default model + report").

## Relationship to the training pipeline (FR-K03)

`POST /v1/wakewords` (see `api.md`) creates a new catalog/user-model entry and kicks off
the async training job (AWS Batch arm64 for openWakeWord; Picovoice Console API for
Porcupine — plan.md M6). While `status != "ready"`, `GET /v1/wakeword/{id}/model` returns
`404` (or `409` with a `{"status":"training"}` body — implementers pick one consistently;
recorded here once M6 finalizes it, additive change). Once training completes and the
model lands in the `live-ninja-wakewords-<acct>` S3 bucket with `status=ready` (and the
user is SES-notified per FR-K03), this endpoint starts returning the manifest above.
