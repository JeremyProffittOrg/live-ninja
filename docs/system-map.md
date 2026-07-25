## Live Ninja — system map

An LWA-gated voice-assistant platform on OpenAI GPT Realtime and Gemini Flash Live, deployed as
SAM/Lambda in us-east-1. This document orients the M17 RCA analyzer for Claude Opus — it is
reviewed like code and is not user-facing.

### Surfaces

Two active surfaces: **web** at live.jeremy.ninja (Go Fiber under the Lambda Web Adapter,
cmd/web + internal/webapp), and **Android** (native app, mints through the same broker). The
M5Stack Tab5 firmware exists and is HIL-verified but is out of scope as scheduled work
(backlog.md, owner decision 2026-07-24) — never propose a fix targeting that device. A typed
*fallback turn* (internal/realtime/fallback.go) invokes tools through the identical router, so a
failure can originate from a degraded text turn rather than a live voice session.

### The mint chain

Request path: client -> POST /api/v1/realtime/token on the web function -> async invoke of
cmd/realtime-broker (the sole holder of the OpenAI/Gemini keys) -> an ephemeral client secret plus
a session config. Composition order, verbatim and load-bearing:
persona.Instructions -> SessionDirectives (memory + silence, internal/realtime/mint.go) ->
BASE KNOWLEDGE (internal/realtime/baseknowledge.go, BuildBaseKnowledge) -> accent
(AccentDirective) -> guides (guides.go) — applied identically on the OpenAI mint, the Gemini
mint, and the text fallback turn (cmd/realtime-broker/main.go). The tool manifest bound into
every session is tools.CatalogManifest(), the same renderer that produces the tool-contract
context an RCA is shown for the failing tool — so a schema mismatch between what the model was
told and what the router enforces is impossible by construction. An invalid_args failure is
therefore a model/prompt problem, not a manifest-drift problem. That is the single most valuable
fact in this document.

### The tool registry

internal/tools is one execution path behind POST /api/v1/tools/invoke, run for every surface.
Five pipeline stages, in order (registry.go Invoke/finish): (1) enumerated-argument validation
against each tool's declared ParamSpec (type, required, enum, length/range bounds); (2) per-call
Reauthorize (re-checks the user is still active/allowlisted — never trusts a JWT minted before a
revocation); (3) an IDEMP#<userId>#<key> conditional put for SideEffecting tools, marked *before*
execution so a duplicate delivery is a no-op; (4) real execution — no stubs; (5) finish: an audit
LOG# write into the caller's transcript partition (90d TTL) plus a ToolInvocations EMF metric.

Error codes (registry.go), one meaning each:
- `unknown_tool` — inv.Tool names nothing in the catalog. Client-supplied text, checked before
  any other validation — treat the raw name as untrusted.
- `invalid_args` — a ParamSpec violation: missing required argument, wrong type, enum/length/range
  miss. The headline case this pipeline exists to catch — usually a model/prompt defect.
- `confirmation_required` — declared but currently dead code; no handler returns it as of M17.
- `forbidden` — either no authenticated user context, or Reauthorize denied the call (a revoked or
  de-allowlisted user). The second case is the security control working as designed, not a defect.
- `not_found` — a referenced resource (deliverable, device, file) does not exist for this user.
- `already_exists` — a real conflict, e.g. a deliverable name collision.
- `not_configured` — a Deps dependency the handler needs is nil (SES/Scheduler/IoT/Deliverables/
  Memory not wired for this environment). The highest-value code here: it usually means a `Deps`
  wiring or IAM gap in production, not a user mistake — the 2026-07-18 incident where every
  memory tool answered not_configured while IAM/Bedrock/REST were all healthy is exactly this.
- `upstream_error` — a downstream call failed (Open-Meteo, Wikipedia, SES, EventBridge Scheduler,
  IoT data-plane).

Tool catalog by name and file: comms `send_email` (sendemail.go); scheduling `set_timer` /
`set_reminder` (scheduler.go); device `device_control` (devicecontrol.go); lookup `get_weather`
(weather.go), `web_lookup` / `web_research` (weblookup.go, research.go); notes `remember_note` /
`recall_note` (notes.go); deliverables `deliverable_create|zip|deliver`, `file_list|read|create`
(deliverable.go, file.go); memory `memory_search`, `memory_write`, `entity_get`, `plan_upsert`,
`forget` (memory.go). `Unadvertised` params are enforced but never shown to the model (e.g.
set_timer's legacy "seconds" alias, kept working but not taught). `OutOfRangeHint` deliberately
redirects the model to a sibling tool on a bound violation (set_timer overflow -> set_reminder) —
**one invalid_args failure immediately followed by a successful sibling call is the system's
designed self-correction path, not a bug** to file an RCA against.

### The memory layer

internal/memory: episodic, retrieval-on-demand, per-user `ENT#<type>#<entityId>` +
`EMB#<entityId>` items in the caller's own partition, Bedrock `amazon.titan-embed-text-v2:0`
(512 dims) query embeddings, in-partition cosine ranking — Query/GetItem only, never Scan. The
model must *choose* to call memory_search, and in production it often did not (2026-07-18: six
memory_write calls, zero memory_search, while the owner asked a plain personal-fact question) —
which is why the profile below exists as an always-injected alternative.

### Settings and the base-knowledge profile

One versioned settings document per user at `USER#<uid>` / `SETTINGS`, JSON-schema-validated
(contracts/settings.schema.json), optimistic concurrency on `version`, synced across surfaces.
Its `profile` section: `displayName`, `pronouns?`, `homeLocation` / `workLocation`
`{label, postalCode?, city?, admin1?, country?, lat, lon, timezone}`, `units`, `locale?`,
`contactEmail`, `quietHours?`, `notes[]`. Locations are geocode-verified at save time — a location
without numeric lat/lon is dropped, never persisted — which is why `get_weather` with no
`location` argument goes straight to stored coordinates and issues zero geocoding requests, and
why a free-text location can never enter the profile. Two facts relevant to invalid_args-adjacent
failures: `internal/tools/geocode.go` splits a `"City, ST"` query before the geocoder call (the
name index has no compound entries), and the scheduler accepts an offset-less local datetime for
`at` (interpreted in the profile timezone) because the model emits `2026-07-25T09:00` far more
often than a correct RFC3339 value.

### Storage and identity

One DynamoDB table `live-ninja`: `pk`/`sk` plus several GSIs, `PAY_PER_REQUEST`, TTL on `ttl`,
**no Scan on any serving path**. Key prefixes an RCA may need: `USER#<uid>` with `SETTINGS` /
`LOG#<sessionId>#<seq>` (tool-audit rows 90d TTL, transcript turns 30d TTL by default) /
`CONV#<ts>#<sessionId>` (minted by cmd/topics-extract at session end, long after any mid-session
tool failure) / `TOPIC#` / `PERSONA#` / `DELIV#` / `ENT#` / `EMB#` / `GUIDE#`, `DEVICE#<id>`,
`IDEMP#<userId>#<key>`, and `RCA#*` (this pipeline's own records). Auth: LWA sign-in, KMS-signed
JWTs, per-call re-authorization, owner-plus-allowlist access.

### Constraints an RCA must respect

- Deploy is push to `main` via GitHub Actions + OIDC — never a local `aws`/`sam deploy`.
- Every Lambda is arm64 / `provided.al2023`.
- Secrets live in SSM SecureString only — never a secrets manager.
- No new CloudWatch alarms (owner decision 2026-07-19).
- No `Scan`, ever, on a serving path.
- This RCA pipeline itself executes nothing: its only outputs are one email and pending
  `PROFSUGG#` rows, so a suggestion must be phrased as something a human applies by hand.
