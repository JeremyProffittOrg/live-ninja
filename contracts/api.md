# `/v1` API Route Inventory (all milestones)

> **Deployed path prefix (2026-07-19):** every `/v1/...` row below is actually served under
> **`/api/v1/...`** (`internal/webapp/api_routes.go` mounts the group at `/api/v1`); the
> unversioned `/auth/*`, `/.well-known/*`, `/healthz` routes are exactly as written. The bare
> `/v1/...` spelling 404s in production. This bit the M5Stack firmware (the one consumer that
> followed this doc literally — its mint calls 404'd, masked for months behind the version
> gate's 426 which fired before routing). Client implementers: use `/api/v1/...`.

Consolidated, canonical route inventory across M0–M12, reconciling the PRD §5 "REST endpoint
catalog" with the concrete route names used in plan.md's per-milestone task lists (which
sometimes used shorthand like `GET auth login` or `/api/v1/auth/lwa/exchange`). Where a
milestone task description and the PRD catalog implied different paths, this file is the
**resolved, canonical name** — implementers follow this file, not the shorthand in plan.md
prose (see "Reconciliation notes" at the end).

## Auth column key

| Value | Meaning |
|---|---|
| **Public** | The HTTP API Lambda `authorizer` passes the request through without validating a session JWT (matches the M0 authorizer allowlist: `/healthz`, `/static/*`, `/auth/*`, `/.well-known/*`, plus the two routes below explicitly carved out for pre-auth device/updater use). This does **not** mean "no authentication logic at all" — several of these routes validate a refresh cookie, an OAuth `state`, or a single-use pairing nonce inside the handler itself; "Public" describes the authorizer layer only. |
| **Session JWT** | Requires a valid first-party ES256 access JWT — `Bearer` header (Android, M5Stack) or `__Host-` HttpOnly cookie + in-memory JWT (web). Rejected by the `authorizer` before reaching the handler if invalid/expired/`iat < tokensValidAfter`. |

Every **Session JWT** route additionally re-derives `userId` (and `deviceId` where
relevant) from the verified claims — never from a client-supplied body/query field — per
NFR-02/FR-A02's anti-confused-deputy posture.

---

## Auth & Identity (M1 — FR-A01..08)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET | `/auth/lwa/login` | Start LWA Authorization Code + PKCE for the web surface; sets OAuth `state`+verifier (TTL 10 min), 302s to LWA. | Public |
| GET | `/auth/lwa/callback` | LWA redirects here with `code`+`state`; backend exchanges code server-side, validates `aud`/`tokeninfo`, upserts user, sets `__Host-` refresh cookie + returns access JWT. **Also the device browser leg:** when the consumed `OAUTHSTATE` row carries a `DeviceNonce` (put there by `/auth/lwa/login?device_nonce=…`), this same handler instead runs the Authorize gate and serves the SSR user-code confirm page — it does NOT open a web session or bind the device (bind happens at `/auth/device/pair/confirm`). There is no separate `/auth/lwa/device/callback` route. | Public |
| POST | `/auth/lwa/exchange` | Android's Custom-Tabs+PKCE code exchange: `{code, code_verifier}` → access JWT + 30-day refresh. **Canonicalized here** (PRD/plan.md prose used `/api/v1/auth/lwa/exchange`; moved under unversioned `/auth/*` to match the authorizer's public-route prefix and sit alongside `/auth/lwa/login`+`callback` — see Reconciliation notes). | Public |
| POST | `/auth/device/pair/start` | M5Stack registers a `PAIR#<nonce>` + device-generated PKCE challenge before it has any credentials (FR-A06). Single-use, 600s TTL (PRD threat table "Pairing hijack"). Response additionally carries `userCode`: an RFC 8628-style anti-phishing code (8 chars from the `BCDFGHJKLMNPQRSTVWXZ` alphabet, formatted `XXXX-XXXX`) the device MUST display on its screen — the human types it into the browser confirm page before the bind can finalize. | Public |
| POST | `/auth/device/pair/poll` | The single device-facing poll (JSON body, NOT query params). Sends `{nonce}` while waiting; once the pairing is `bound` the device sends `{nonce, codeVerifier}` and the claim is **folded into this same call** — the handler presents the PKCE verifier, and on success returns `{"status":"bound", deviceId, sessionId, refreshToken, refreshExpiresAt, accessToken, expiresAt, thingName, certArn}` (the device's 10-year refresh credential + provisioning claim, once). While waiting: `{"status":"pending", pollIntervalSeconds}`. Terminal states: `410 {"status":"failed","reason":"user_code_attempts_exceeded","message":…}` after 5 wrong user-code entries in the browser (pairing invalidated — restart to get a fresh nonce + code), `410 already_claimed`, `403 verifier_mismatch` (PKCE), `404 {"status":"expired"}`. There is no separate `pair/claim` route — the claim is this call. | Public |
| GET | `/auth/device/claim` | Browser entry leg (the `claimUrl` the device shows as a QR/link): validates `?nonce=` is still claimable, then 302s into `/auth/lwa/login?device_nonce=<nonce>` to start the LWA browser flow. Returns `410` (`failed`) / `404` (expired) / `409` (already paired) for a non-claimable nonce. | Public |
| POST | `/auth/device/pair/confirm` | Confirm-page form target: `{token, user_code}` (form-encoded). **CSRF-exempt** (a bare device browser has no `__Host-ln_csrf` cookie/`X-LN-CSRF` header to echo; it carries its own stronger one-shot `PAIRCONFIRM` token instead). Token must match the HttpOnly `__Host-ln_pair` cookie (constant-time) and resolve to a live `PAIRCONFIRM` row (hidden form field + cookie, 10-min TTL, carrying the LWA-verified identity); `user_code` must constant-time match the code stored on the `PAIR#` row (case-insensitive, dash/space-insensitive). Only then does the backend bind the device: create the IoT Thing + scoped policy hook, the `DEVICE#` record, and the 10-year refresh family identity. Wrong code → inline error with remaining attempts (input preserved); 5th wrong entry → pairing invalidated (`PAIR` status `failed`, terminal, `410`). The code is REQUIRED — no bind path skips it. | Public |
| POST | `/auth/refresh` | Rotate the refresh token (web cookie or Android/M5Stack bearer refresh), issue a new 15-min access JWT; reuse of an already-rotated token revokes the whole `familyId` + fires a security alert. | Public (validates the refresh token/cookie itself — not a JWT-gated route since the access JWT has by definition expired when this is called) |
| POST | `/auth/logout` | Delete the caller's session row; refresh dies immediately, outstanding JWT dies within its ≤15 min natural expiry. Idempotent no-op if already logged out. | Public (acts on whatever refresh cookie/token is presented; no-op without one) |
| GET | `/.well-known/jwks.json` | JWKS for JWT verification (from `kms:GetPublicKey`, cached 24h) — consumed by the `authorizer` and by any future third-party verifier. | Public |
| GET | `/.well-known/assetlinks.json` | Android App Links / Digital Asset Links verification for the LWA Custom-Tabs redirect and the assistant role intent. | Public |

## Account & Devices (M1, M7 — FR-A07, FR-S05)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET | `/v1/account` | Account profile, active sessions, connected devices. | Session JWT |
| DELETE | `/v1/account` | Right-to-delete: partition-scoped `Query`+`BatchWriteItem` purge (no Scan) across DynamoDB, S3 prefix empty, IoT thing/cert delete, LWA refresh revoke, SES confirmation. | Session JWT |
| POST | `/v1/account/logout-everywhere` | Bump `tokensValidAfter`; every outstanding JWT across every surface is rejected within the authorizer's 60s cache window; every refresh row deleted. | Session JWT |
| GET | `/v1/devices` | List the caller's registered devices (Android installs, M5Stack units) — backs the per-device `voiceEngine` pin picker in Settings (`settings.schema.json#/properties/voiceEngine/devices`). | Session JWT |
| DELETE | `/v1/devices/{id}` | Revoke one device: detach its IoT cert, revoke its refresh family. | Session JWT |

## Realtime Voice & Tools (M2 — FR-B02, FR-V01..08, FR-B10)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET | `/v1/realtime/session` | Session bootstrap. Resolves the calling device's `voiceEngine` (`settings.schema.json`) and returns ONE OF: a short-lived OpenAI Realtime ephemeral token (config/persona/guides bound server-side), a Nova Sonic bridge WebSocket URL (M12), or a Gemini Live `gemini-direct` shape (M13: `geminiEndpoint` + single-use constrained `accessToken` + the `sessionConfig` setup frame — never `wsUrl`-family field names; see `docs/voice-engines.md`). Used by web, Android, **and the M5Stack** — identical route, no surface-specific variant. Gated by the metering/quota gate (`metering.md`) — pre-spend check runs before any mint. | Session JWT |
| POST | `/v1/tools/invoke` | Server-side tool execution, re-authorized per call against the calling user; carries idempotency keys for side-effecting tools; the M5Stack routes its `function_call`s here over plain HTTPS (never through IoT). | Session JWT |
| WSS | `/v1/realtime/bridge/{sessionId}` | Nova Sonic backend media bridge (M12 only) — bidirectional audio for a device explicitly pinned to `nova-sonic` (`settings.schema.json#/properties/voiceEngine`). The **only** route where AWS sits in the audio media path; absent/unused for any device on `openai-realtime`/`openai-realtime-mini`. `sessionId` is the value returned by the preceding `GET /v1/realtime/session` call. Auth via a short-lived token embedded in the bridge URL returned by that call (WebSocket upgrade requests can't reliably carry a Bearer header on every client stack) — the token is single-use and scoped to that one `sessionId`. | Session JWT (bootstrapped via the realtime/session response, not a fresh header on the WS handshake) |

## Settings (M6 — FR-S01..05)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET | `/v1/settings` | Fetch the caller's canonical settings document (`settings.schema.json`). | Session JWT |
| PUT | `/v1/settings` | Update settings; body includes the expected `version` — `ConditionExpression version = expected`; mismatch → `409` (client re-reads/re-applies, per `contracts/README.md` rule 4). Also fans out via WebSocket (web), FCM data message (Android), and IoT shadow `desired` (M5Stack — see `shadow.md`). | Session JWT |
| GET | `/v1/settings?since={version}` | Reconciliation fetch: "give me the doc only if newer than `{version}`" — lets a client that just pushed a local change confirm whether a concurrent write from another surface won. | Session JWT |
| WSS | `/v1/ws` | Persistent control-plane WebSocket for the web client. Carries `settings.updated` frames (FR-S02) and other server-push control notices (e.g. a device coming online). Not used for realtime audio — that's the direct-to-OpenAI WebRTC path. | Session JWT |

## Wake-word (M6 — FR-K01..06)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET | `/static/wakewords/catalog.json` | Shared wake-word catalog snapshot (built-in phrases + platform coverage), served as a static CloudFront-cached asset regenerated on catalog change — **deliberately not a live DynamoDB-backed route** (plan.md M6: "no live table read on public path"). Populates the combobox in `settings.schema.json#/properties/wakeWord`. | Public (`/static/*`) |
| POST | `/v1/wakewords` | Create a custom wake-word training job: `{phrase, engine}`. Validates length/phonemes/profanity/collision; enforces job concurrency ≤2, 20-min timeout, ≤3/day/user (FR-K03). | Session JWT |
| GET | `/v1/wakewords/{id}` | Poll training-job/catalog-entry status (`pending`\|`training`\|`ready`\|`failed`). | Session JWT |
| GET | `/v1/wakeword/{id}/model?platform={web\|android\|esp32}` | Content-addressed model manifest for hot-swap — full schema in `wakeword-manifest.md` (FR-K04). | Session JWT |

## Uploads (FR-B04)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| POST | `/v1/uploads` | Presigned S3 `PUT` with a pinned `Content-Type`/`Content-Length` allowlist and size cap; object key namespaced under the caller's `userId`. | Session JWT |

## Deliverables Store (M9 — FR-DLV-01..06)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| POST | `/v1/deliverables` | Create a deliverable (PDF/MD/CSV/JSON/ICS/image/artifact) → S3 object under `{userId}/{deliverableId}/{filename}` + DynamoDB index item. | Session JWT |
| POST | `/v1/deliverables/zip` | Bundle N existing deliverables into one ZIP, itself stored as a new deliverable. | Session JWT |
| POST | `/v1/deliverables/{id}/deliver` | Mint a short-lived presigned URL and surface/deliver the item; optionally emails it via SES. | Session JWT |
| GET | `/v1/deliverables` | List the caller's deliverables — `Query` on `PK=USER#{userId}, SK begins_with DELIV#`, never `Scan` (FR-DLV-04). Backs the web Download Center and Android Files tab identically. | Session JWT |
| GET | `/v1/deliverables/{id}/download` | Authorized download; key prefix must equal caller `userId`. | Session JWT |

## Memory Layer & Guide Entities (M10 — FR-MEM-01..09)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| POST | `/v1/memory/search` | Semantic recall over S3 Vectors: `{query, entityTypes?}` → ranked hits (`memory.search` tool). | Session JWT |
| POST | `/v1/memory` | Write a typed memory item (`memory.write` tool); `memoryType` ∈ `working`\|`episodic`\|`semantic`\|`procedural`. | Session JWT |
| GET | `/v1/memory` | List the caller's memory items for the memory browser (FR-MEM-05); `Query`-only, paginated. | Session JWT |
| DELETE | `/v1/memory/{id}` | "Forget" — removes the item from DynamoDB **and** the S3 Vectors index (both stores, FR-MEM-05). | Session JWT |
| GET | `/v1/entities/{id}` | Fetch one entity (person/place/information) — `entity.get` tool. | Session JWT |
| GET | `/v1/entities?type={entityType}` | List entities of one type via `GSI2` (`ETYPE#<userId>#<entityType>`) for the memory browser. | Session JWT |
| POST | `/v1/plans` | Create/update a plan and its tasks (`plan.upsert` tool). | Session JWT |
| GET | `/v1/plans` | List the caller's plans. | Session JWT |
| GET | `/v1/plans/{id}` | Fetch one plan with its tasks. | Session JWT |
| GET | `/v1/guides` | List the caller's Guide Entities. | Session JWT |
| PUT | `/v1/guides/{id}` | Create/edit/enable/prioritize a guide (versioned; body includes `enabled`, `priority`, `body`, optional sourcing directives per FR-MEM-08); syncs to devices via the same settings-fan-out transport as `PUT /v1/settings`. | Session JWT |

## Conversation Topics & Filterable History (M11 — FR-TOP-01..07)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET | `/v1/conversations` | List/filter conversation history by `topic`, `device`, `from`, `to`, `turnsOver` (strictly-greater-than turn-count facet — the UI's "long conversations" filter) — `Query` against the most selective GSI (`GSI3` by-topic or `GSI4` by-device) + `FilterExpression` for remaining facets, never `Scan` (FR-TOP-04). Rows carry `costUsd`/`costTextTokens`/`costAudioTokens` when the session's client-estimated cost was persisted. | Session JWT |
| GET | `/v1/conversations/{id}` | Fetch one conversation (transcript S3 pointer, summary, topic IDs). Always also returns `rawTurns`: every stored `LOG#` row verbatim (incl. the `role=system` session marker and tool audit lines, with `sk`/`seq`/`ts`/`engine`/`surface`) for the history detail "Raw transcript" view. (Deliberately not query-param-gated: the `%23` in ConvID paths makes the prod edge chain drop query strings.) | Session JWT |
| GET | `/v1/costs` | Month-to-date cost summary: `{monthStart, totalUsd, conversations, costed}` — the sum of persisted per-session cost estimates over the current UTC month's `CONV#` range (single-partition `Query`). Shown in the conversation page's Menu drawer. | Session JWT |
| GET | `/v1/topics` | List the caller's topic taxonomy. | Session JWT |
| POST | `/v1/topics` | Create a topic. | Session JWT |
| PATCH | `/v1/topics/{id}` | Rename / merge (via `mergedInto`) / split / color / archive a topic; tags reference the stable `topicId` so this never requires re-tagging conversations (FR-TOP-02). | Session JWT |

> `tag_topics(topics[])` (FR-TOP-06, optional in-band provisional tagging) is a realtime
> function call, not a REST route — it flows through `POST /v1/tools/invoke` like every
> other tool call (FR-V04); the post-session extractor remains the canonical tagger.

## Android update channel (M8)

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET | `/v1/app/android/latest` | In-app updater check: latest signed-APK version/URL for the sideload/internal-testing distribution channel (independent of the Google Play listing). Must be reachable pre-auth (a stale/broken install may not have a valid session). | Public |

## Liveness & Compatibility

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET | `/healthz` | Liveness/readiness probe (CloudFront → HTTP API → Fiber). | Public |
| GET | `/v1/compat` | Capability negotiation for long-lived clients (10-year M5Stack horizon) — full contract in `headers.md`. | Public |

---

## Reconciliation notes (plan.md prose → this canonical inventory)

- **Android LWA exchange**: plan.md M1 wrote `POST /api/v1/auth/lwa/exchange`; the shared
  spec's route convention is `/v1/*` for the resource API with `/auth/*` reserved
  (per the M0 authorizer's public allowlist) for pre-session identity bootstrap. Canonical
  path is **`POST /auth/lwa/exchange`** (no `/v1`, no `/api` prefix) — it sits alongside
  `/auth/lwa/login`/`callback`, matches the authorizer's `/auth/*` public prefix exactly, and
  is consistent with `/auth/refresh` and `/auth/logout` below it. Any WS-B/WS-E code written
  against the old shorthand should be updated to this path; this is the only renamed route
  in the freeze.
- **Web realtime session bootstrap**: plan.md M3 wrote `POST /api/realtime/session` for the
  web client specifically; the PRD catalog and this file use **`GET /v1/realtime/session`**
  uniformly across all three surfaces (it's a read/bootstrap, not a mutation — no request
  body is required, persona/device resolve from the verified JWT claims). WS-D's
  `realtime.mjs` should call the canonical path.
- **`GET auth login` / `GET auth callback`** (PRD §7 sequence-diagram shorthand) resolve to
  **`GET /auth/lwa/login`** / **`GET /auth/lwa/callback`** above.
- **Device pairing "devices/pair" bind** (plan.md M1 shorthand) is not a client-called REST
  route at all — it's the backend's own internal action, now taken inside the
  `POST /auth/device/pair/confirm` handler once the human has entered the device's RFC 8628
  user code (the shared `GET /auth/lwa/callback` handler, on its device leg, only authorizes
  and serves the confirm page). No separate bind route — and no `/auth/lwa/device/callback`
  route — needed; documented here so no workstream builds a phantom endpoint for it.

## Change log

| Date | Change | Motivated by |
|---|---|---|
| 2026-07-17 | Initial freeze at M0. Full inventory compiled from PRD §5 catalog + all milestone task lists (M1–M12); three route names canonicalized per "Reconciliation notes" above. | WS-G M0 contract-freeze task |
| 2026-07-18 | RFC 8628 user-code binding added to device pairing: `pair/start` responses gain `userCode` (`XXXX-XXXX`, alphabet `BCDFGHJKLMNPQRSTVWXZ`, displayed on the device); new `POST /auth/device/pair/confirm` route (browser confirm leg — constant-time code match, 5-attempt budget); `pair/poll` gains the terminal `failed` status (`reason: user_code_attempts_exceeded`) and folds the former `pair/claim` step into itself; the shared `lwa/callback` (device leg) now serves the confirm page instead of binding directly. No back-compat shim — the code is required (no device had onboarded). | Pairing anti-phishing gap: an allowlisted attacker could phish a victim into binding a 10-yr device credential to the victim's account |
