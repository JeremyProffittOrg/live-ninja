# Live Ninja — Contracts (WS-G freeze, M0)

This directory is the **single source of truth** for every integration seam that crosses a
workstream boundary (backend ↔ web, backend ↔ Android, backend ↔ M5Stack, backend ↔ IoT
shadow). Every client and server implementation across WS-B..F **must** conform to these
files exactly. If a workstream needs a field or behavior not covered here, it amends this
directory first (see "Change process" below) rather than inventing an ad hoc shape.

Frozen 2026-07-17 at milestone **M0** per plan.md task "Contract freeze (WS-G)". Read
alongside `PRD.md` (FR IDs, §7 Auth, §8 Data Model, "REST endpoint catalog") and `plan.md`
(milestone Definitions of Done).

## Index

| File | Covers | Primary FR IDs |
|---|---|---|
| [`settings.schema.json`](./settings.schema.json) | Canonical per-user settings document (`SETTINGS#v<n>`) | FR-S01..05, FR-VE-01..04 |
| [`shadow.md`](./shadow.md) | IoT named shadow (`config`) document shape + reconciliation | FR-S02, FR-M02/M05, Q-19 |
| [`wakeword-manifest.md`](./wakeword-manifest.md) | Per-platform wake-word model distribution manifest | FR-K03, FR-K04 |
| [`telemetry.schema.json`](./telemetry.schema.json) | Analytics/telemetry event envelope (no transcript content) | NFR-06, Crosscut §5 |
| [`headers.md`](./headers.md) | `X-LN-Client`/`X-LN-Server` negotiation + `GET /v1/compat` | NFR-08, Q-19 |
| [`metering.md`](./metering.md) | Quota gate: daily-minutes + monthly-token caps, token bucket, soft/hard cap responses | FR-B09, FR-V06, Q-16 |
| [`api.md`](./api.md) | Full `/v1` route inventory across all milestones, auth requirements | All FR-B/V/A/W/M/K/S + FR-DLV/MEM/TOP/VE |

## Versioning rules

1. **Path-versioned, additive-only within `/v1`.** The API is served under `/v1` for the
   entire field lifetime (10-year M5Stack horizon — see NFR-08, Q-19, PRD §12.3 "Long-lived
   M5Stack devices outrun API changes"). Within `/v1`:
   - New fields, new routes, new enum members may be **added** at any time.
   - Existing fields/routes/enum members are **never removed or repurposed**. If a field is
     truly obsolete, stop writing it and leave it documented as deprecated; do not delete it
     from the schema or reuse its name for something else.
   - A breaking change (removal, type change, semantic change of an existing field) requires
     a new path version (`/v2`) and is out of scope for any milestone through M12.
2. **Unknown-field preservation.** Every JSON document defined here (`settings`, shadow
   `config`, telemetry events) is `"additionalProperties": true` (or documented as such for
   non-JSON-Schema files). Every reader — server and every client (web, Android, M5Stack) —
   **must** round-trip fields it does not recognize: read the full document, mutate only the
   fields it understands, and write the rest back unchanged. This is what lets a 10-year-old
   M5Stack firmware and a same-day web deploy share one settings document without either
   corrupting the other's fields. Concretely:
   - Server-side `UpdateItem` for settings/shadow updates a whole-document attribute (or
     merges at the top level) — it never does a partial `PutItem` that would drop fields an
     older writer didn't send.
   - Firmware and app clients deserialize into a permissive structure (e.g. a generic
     map/JSON value alongside typed accessors for known fields) and re-serialize the same
     unknown keys they read.
3. **Enums grow, never shrink.** `wakeEngine`, `voiceEngine`, `theme`, `turnDetection`, etc.
   may gain new members in a later milestone (e.g. `nova-sonic` added at M12). A client that
   doesn't recognize a new enum value must fall back to its default for that field rather
   than erroring, and must preserve the unrecognized value on write-back (rule 2).
4. **`version` is the concurrency primitive, not a schema version.** The integer `version`
   field on the settings document (FR-S01) is optimistic-concurrency only (`ConditionExpression
   version = expected`, bumped on every write, higher-version-wins reconciliation across
   surfaces). It says nothing about the JSON Schema shape — schema evolution is governed by
   rules 1–3 above, independently of the data's `version` counter.
5. **FR ID reconciliation.** Where a contract file's field or route maps to a PRD FR ID, the
   ID is cited inline. Where plan.md milestone notes used a placeholder bracket tag (e.g.
   `[WAKE]`, `[SETTINGS]`) before FR IDs were finalized, that tag is now resolved to the
   concrete FR-xxx ID cited in this directory — plan.md task lines are not rewritten, but
   this directory is the canonical mapping.
6. **Change process.** Any workstream needing a new field/route: (a) add it here additively
   per rules 1–3, (b) note the milestone/FR ID that motivated it, (c) update `api.md`'s
   Change Log. Do not fork a private copy of a contract inside a workstream's own code — the
   files in this directory are imported/copied verbatim (schema validation in CI, per
   plan.md §6 "Cross-cutting gates").

## CI enforcement (per plan.md §6)

- `settings.schema.json` and `telemetry.schema.json` are validated in CI against every
  client fixture (web, Android, M5Stack) that produces or consumes those documents.
  `additionalProperties: true` is asserted to actually be permissive (a round-trip fixture
  with an unrecognized field must survive unchanged).
- `api.md`'s auth column is cross-checked against the `authorizer` Lambda's public-route
  allowlist (`/healthz`, `/static/*`, `/auth/*`, `/.well-known/*`) — any route marked
  **Public** here must appear in that allowlist, and vice versa.
