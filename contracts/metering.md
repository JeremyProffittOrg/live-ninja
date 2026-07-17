# Metering & Quota Gate Contract

Governs every call to `GET /v1/realtime/session` (the ephemeral-token/session-bootstrap
mint route — see `api.md`) across all three surfaces. Implements FR-B09, FR-V06, FR-V08,
and PRD Q-16's baked-in defaults. **Enforcement is always pre-spend**: the gate is checked
and updated atomically *before* the broker mints an OpenAI ephemeral token or a Nova Sonic
bridge URL — never after, so a rejected mint costs nothing.

## The three independent limits

| Limit | Default | Enforced by | What happens at the cap |
|---|---|---|---|
| **Token bucket (mint rate)** | 1 token refilled per 5s, burst capacity 3 | Atomic `UpdateItem` on a per-user bucket counter with a `ConditionExpression` (never a read-then-write race) | `429` — this is an abuse/thundering-herd guard, not a spend cap; the user's own reconnect storms (flaky WiFi, app restart loop) are what this catches. |
| **Daily realtime-minutes cap** | ~30 minutes of realtime audio per user per UTC day | `USAGE#<userId>/LOG#<yyyymmdd>#<ulid>` items (FR-V06's atomic `UpdateItem ADD` from `response.done`), summed and checked at mint time against the day's running total | `402` once exhausted; soft warning header (below) at 80%. |
| **Monthly token/cost ceiling** | ~$15/month per user, expressed internally as a token ceiling derived from current OpenAI pricing | Monthly rollup counter (`ROLLUP#<yyyymm>`, `usage-rollup` hourly job, FR-B07), checked at mint time | `402` once exhausted; soft warning header at 80%. |

A fourth, session-level (not quota) control: **10-minute hard session duration cap**
(FR-V08) auto-closes any single realtime session regardless of remaining quota — the
broker binds this into the OpenAI session config at mint (not a separate check here), and
an auto-closed session is not treated as abandoned/billed-in-error; usage up to the close
point is metered normally.

## Token-bucket mechanics (rate limiter on the mint endpoint itself)

- Per-user bucket: capacity 3, refill 1 token / 5s, stored as an integer count + last-refill
  timestamp on the user's row (or a dedicated `USER#<userId>` / `BUCKET#realtime` item).
- On each `GET /v1/realtime/session`: compute tokens-available = min(capacity, stored +
  floor((now - lastRefill) / 5s)); if >= 1, atomically decrement via a conditional
  `UpdateItem` (`ConditionExpression: tokens >= :one`) and proceed; if 0, reject with `429`
  immediately — no queuing, no delay-then-retry server-side. The client backs off and
  retries (see Retry-After below).
- This check runs **before** and independently of the daily/monthly checks — a request can
  fail the rate limit even with plenty of quota left (and vice versa).

## Soft-cap warning (80% threshold, non-blocking)

When a mint **succeeds** (not rejected by any of the three limits) but the user's running
daily-minutes or monthly-token usage is **≥ 80%** of its cap, the response carries:

```
X-LN-Quota-Warning: daily_minutes=83%
```

or, if both are over threshold, a comma-separated list:

```
X-LN-Quota-Warning: daily_minutes=83%,monthly_tokens=91%
```

- Format: comma-separated `<kind>=<percentUsed>%` pairs, `kind` ∈ `daily_minutes` |
  `monthly_tokens` (matches `telemetry.schema.json`'s `quota_warning` event `kind` enum).
  `percentUsed` is an integer, floor-rounded.
- The header is **absent entirely** when neither limit is at/above 80% — clients must not
  infer "under 80%" from header absence on error responses (see below); it is only ever
  emitted alongside a `2xx` mint success.
- Every mint that carries this header also fires a `quota_warning` telemetry event
  (`telemetry.schema.json`) with `attrs: {kind, pctUsed}`.
- Clients (web/Android UI, M5Stack ambient indicator) should surface this as a
  non-blocking, dismissible notice — never interrupt an in-progress conversation to show it.

## Hard-cap error response (`402 Payment Required`)

Returned when the daily-minutes **or** monthly-token ceiling is already exhausted at mint
time (checked pre-spend, so the request never reaches OpenAI/the broker's SSM read).

```jsonc
// HTTP/1.1 402 Payment Required
{
  "error": "quota_exceeded",
  "kind": "daily_minutes",          // "daily_minutes" | "monthly_tokens"
  "message": "Daily realtime-audio limit reached. Resets at 2026-07-18T00:00:00Z.",
  "used": 30.4,                      // same unit as `limit`
  "limit": 30,                       // minutes for daily_minutes; token count for monthly_tokens
  "resetAt": "2026-07-18T00:00:00Z"  // ISO-8601 UTC — next UTC midnight (daily) or next billing-month start (monthly)
}
```

If **both** caps are simultaneously exhausted, `kind` reports whichever cap was hit first in
check order (daily before monthly), and the client should treat a `402` as "quota
exhausted" generically rather than branching hard on `kind` for anything beyond display
copy — the important behavior (stop trying to mint, show a wait-until message, `resetAt`)
is identical either way.

## Rate-limit error response (`429 Too Many Requests`)

Returned when the token bucket has 0 tokens available, independent of quota state.

```jsonc
// HTTP/1.1 429 Too Many Requests
// Retry-After: 5
{
  "error": "rate_limited",
  "message": "Too many session requests in a short period. Retry shortly.",
  "retryAfterSeconds": 5
}
```

- `Retry-After` header is set alongside the body (standard HTTP semantics) so generic HTTP
  clients/CDNs also do the right thing without JSON parsing.
- `retryAfterSeconds` in the body mirrors the header for clients that only inspect JSON
  (embedded firmware HTTP stacks that don't expose response headers easily).
- Clients must back off **at least** `retryAfterSeconds` before their next mint attempt;
  repeated immediate retries against a `429` are themselves flagged by the ops alarms
  (NFR-06) as an anomalous pattern.

## Distinguishing `402` vs `429` (client behavior)

| Response | Meaning | Client behavior |
|---|---|---|
| `429` | Transient — you're minting too fast. | Back off `retryAfterSeconds`, then retry normally; no user-facing "you're out" messaging needed for a single isolated 429 (only surface a message if it recurs). |
| `402` | Not transient within the current window — quota is genuinely exhausted until `resetAt`. | Show the wait-until message from `error.message`/`resetAt`; do not silently retry-loop; the mic/click-to-talk control should reflect a disabled/"quota reached" state until `resetAt` passes. |

## Idempotency / consistency notes

- All counter updates (token bucket, daily-minutes accrual, monthly rollup checks) use
  DynamoDB conditional `UpdateItem`/`UpdateItem ADD` — never read-modify-write from
  application code — so concurrent mint attempts from the same user (e.g. web + Android +
  M5Stack all waking at once) cannot race past the cap (FR-B09, FR-B03 "no Scan").
- `usage-rollup` (FR-B07, hourly EventBridge) recomputes the monthly rollup from the day's
  `USAGE#`/`LOG#` items via `Query` (never `Scan`) and is the source of truth the mint-time
  check reads for `monthly_tokens`; the daily-minutes check reads the same-day `LOG#` items
  directly (finer-grained, doesn't wait for the hourly rollup) so a same-day cap is
  enforced promptly rather than up to an hour late.
