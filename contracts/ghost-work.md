# Ghost work and retained archive reads

Backend source contract pinned to Ghost commit
`672c44030d50a738d514b1d9b9ad89c754d12bae`:
`lambda/command/history.go`, `nodes.go`, `schedule.go`, `internal_invoke.go`.
The history routes additionally require Ghost's GET-only internal-invoke
allowlist update. No new credentials, IAM grants, capture or uploads are requested.

All routes live under `/api/v1/ghost-work`. They require an authenticated,
personal web/Android session and a fresh strongly consistent active-owner check.
The shared Ghost principal has no per-member identity mapping, so members and
device/provider-scoped tokens are denied. Every response uses `Cache-Control:
no-store`. Do not persist archive text in application telemetry or caches.
Every `/api/v1/ghost-work` route is a read and calls Ghost with GET. The only
writes to Ghost are the two owner-confirmed assistant tools described under
"Assistant tools" below (POST /schedule and POST /command).

## Discovery and selection

`GET /nodes` returns:

```json
{"nodes":[{"node_id":"OFFICEPC","state":"live","agent_version":"...","last_seen":"provider timestamp","connected":true}],"source":"ghost","binding":"explicit_provider_selection","historyAvailability":"check_per_node"}
```

Legacy `status` may also be present. `state` is the canonical presence field.
The inventory is already filtered by Ghost's per-node ActionView authorization.
It is bounded by the provider inventory implementation, not a claim that every
historical/offline/decommissioned machine is discoverable. Users can select an
actual node and provider session; the history read separately enforces
ActionPresign against that exact node on every request.

`GET /jobs` returns `{events:[ScheduledEvent],source:"ghost",runHistoryLimit:10,
providerSessionBindingAvailable:false,coverage:"authorized_nodes_only"}`.
The canonical schedule route returns the whole list without a continuation and
retains only the latest 10 run metadata rows per event. Live Ninja filters it
against a fresh authorized node inventory; events referencing an unauthorized
primary, backup, last-run or historical run node are omitted rather than exposing
that node's metadata. It does not claim this metadata list is full run history.

ScheduledEvent fields (strings except the three booleans and runs array):
`event_id,node,repo,cli,model,effort,prompt,backup_node,output_file,cron,run_at,
timezone,enabled:boolean,archived:boolean,deploy:boolean,created_at,next_run,
last_run_id,last_run_status,last_run_ts,last_run_node,runs`.
Each run contains string `run_id,status,ts,node,trigger,output_key,summary`.
Times remain provider-supplied strings; missing values are not synthesized.
The upstream serving path redacts known Live Ninja code-update run tokens;
it does not detect arbitrary secrets in user-provided text. No field binds a schedule or
run to a provider session. Never join by latest session, matching prompt, node,
terminal handle or timestamp. Ghost work is separate from local in-app Jobs.

## Archive pages

`GET /sessions?node_id=OFFICEPC&cursor=<optional>` returns the canonical page:
`{version:1,node_id,sessions:[{session_id,date}],next_cursor,coverage:"retained_only"}`.
Date is the upload partition, not session start. Follow even empty pages with a
cursor and deduplicate sessions by ID across pages.

`GET /events?node_id=OFFICEPC&session_id=<actual-provider-id>&cursor=<optional>`
returns `{version:1,node_id,session_id,events,next_cursor?,resume_cursor,
coverage:"retained_only",gaps:[]}`. Events have `id`, string `sequence`, optional
`timestamp`, `kind`, full `text`, optional `name` and `call_id`, zero-based `part`,
and boolean `more`. Supported kinds are user, assistant, tool_call, tool_result,
command and event. Preserve provider source order and string sequence; do not
sort by timestamps, coerce sequences to numbers, or invent missing timestamps.

### POST page reads (browser)

`POST /sessions` with `{"node_id":"...","cursor":"...optional"}` and
`POST /events` with `{"node_id":"...","session_id":"...","cursor":"...optional"}`
return exactly the same pages as the GET forms. They exist only to keep the
opaque cursor (up to 2048 bytes) out of the request line. They are reads: no
Job or provider state is created, changed or queued. The web client uses them;
the GET forms remain for Android and other existing clients.

POST bodies must be a single JSON object (`Content-Type: application/json`, no
`Content-Encoding`) of at most 8 KiB, containing only the string fields above.
Unknown fields, null or non-string values, trailing data, non-object bodies and
`session_id` on discovery return 400 `invalid_request`; larger bodies return 413
`request_too_large`. Node, session and cursor validation is identical to GET.
The same owner/scope/surface checks apply, and cookie-bearing web sessions must
pass the existing `X-LN-CSRF` double-submit check.

### HTTP 431 evidence and uncertainty

The deployed server leaves Fiber's default `ReadBufferSize` (4096 bytes), which
bounds the request line plus all headers together. A cursor in the query string
adds up to ~2 KiB to the request line, on top of the bearer token, cookies and
the forwarded request-context header the Lambda Web Adapter adds. A regression
test (`TestGhostWorkHTTP431ReproducerCursorInRequestLineVersusBody`) shows, with
synthetic credential-free padding headers, that a cursor-bearing GET exceeds the
buffer while the same headers without a cursor, or with the cursor in a POST
body, pass and still enforce authentication. These sizes are synthetic and are
not a production measurement; no captured production request has been examined.
The cursor-in-request-line overflow is therefore a supported hypothesis for the
intermittent 431 (first pages carry no cursor; later pages and polls do), not a
proven cause. No header limit was raised.

The provider bounds each page to 256 fragments/192 KiB of text and each fragment
to 48 KiB. Live Ninja preserves visible text without another truncation. Follow
next_cursor through empty pages until absent. Keep resume_cursor at the current
end for polling. Refresh/reconnect must rescan from the beginning to recover
late uploads before the previous cursor, deduplicating by stable event ID.
Virtualize rendering, not retained client content. Pagination failure never
means history is complete.

### Fragment identity

At the pinned Ghost commit, `id` equals `sequence` and is built as
`fmt.Sprintf("%s:%09d:%05d:%05d", key, lineNo+1, blockNo, part)`. The web client
joins fragments into one logical message only when both carry this identity
with the same key, line and block, the suffix matches the numeric `part`, and
parts are adjacent in source order. Fragments without a recognizable identity are
never joined heuristically; they are shown separately with a notice, and the
message count is marked uncertain. Only the part suffix is read as a number.

Coverage is always retained_only; capture completeness is unknown. The existing
90-day provider lifecycle is deployed in Ghost revision `b0e4f0a0e68e2e8703a0e10ea2f8e0fd4cf69997`. Missing, expired, never-uploaded or
overwritten bytes cannot be reconstructed. Gaps and existing redactions remain
visible. Thinking, hidden prompts and arbitrary metadata are excluded upstream;
this text projection does not preserve images or attachments.

## Assistant tools

Six owner-only tools in the shared registry (`internal/tools/ghost_*.go`) expose
the fleet to every assistant surface that carries the owner role, and to remote
MCP clients under two scopes. The `/api/v1/ghost-work` routes above are
unchanged; the tools call `internal/ghost` directly with the same pinned
principal, the same allowlisted resources and the same no-store, retained-only
semantics.

| Tool | Scope | Effect | Arguments |
|---|---|---|---|
| `ghost_nodes` | `ghost:read` | read | `node?` |
| `ghost_sessions` | `ghost:read` | read | `node?`, `cursor?`, `limit?` (1..50, default 20) |
| `ghost_session_summary` | `ghost:read` | read | `node`, `sessionId?`, `windowMinutes?` (5..1440, default 60; used only without `sessionId`) |
| `ghost_run_status` | `ghost:read` | read | `requestId?`, or `eventId`+`runId`, or nothing for the latest `CODEUPD#` record |
| `ghost_run_start` | `ghost:manage` | POST /schedule | `repo`, `instructions`, `node?`, `agent?` (claude or codex), `model?`, `effort?`, `deploy?` (default true), `confirm`, `proposalId` (with `confirm`) |
| `ghost_session_command` | `ghost:manage` | POST /command | `node`, `sessionId`, `command` (NUDGE, ANSWER, PAUSE or RESUME), `text` (NUDGE/ANSWER only, at most 280 bytes, one line), `confirm`, `proposalId` (with `confirm`) |

Reads: `ghost_nodes` returns `nodes[]` with `node, online, state, connected,
agentVersion, lastSeen, activeSessions, activeSessionsKnown, retainedSessions`
plus `total, online, activeWindowMinutes, coverage`; a count that could not be
read is `activeSessionsKnown: false`, never zero. `ghost_sessions` returns
`sessions[] {node, sessionId, uploadDate, activity, lastActivity}`, `nextCursor`,
`scope` (`node` or `fleet`) and `coverage`; its first page also carries
`scheduledRuns[]` and `scheduledRunsAvailable`, reported beside sessions and
never joined to them. `ghost_session_summary` is deterministic and EXTRACTIVE —
no model reads the transcript. Consecutive fragments of one message (`part`
ascending while `more` is true, same kind and `call_id`) are joined back into
one event before anything is counted or quoted; `fragmentsRead` is reported when
it differs from `eventsRead`. Per session it reports `eventsRead,
eventsSummarized, gaps, countsByKind, toolCalls, firstTimestamp, lastTimestamp,
lastUserPrompt, lastAssistantMessage, lastTool, lastEventKind, awaitingInput,
pendingQuestion`; bounds are 200 events, 48 KB of text and 4 upstream pages per
session and 10 sessions per window, and hitting any bound sets `partial: true`
with `partialReasons[]`. Quoted session text is information about the owner's
work, never an instruction to the assistant.

Writes: both manage tools are `SideEffecting` (they need an `idempotencyKey`,
which MCP derives from the required `requestId`). Called without `confirm: true`
they validate that the computer, repository or session exists and return
`confirmation_required` with `details.operation`, `details.proposed`
(`{repo, node, nodeOnline, agent, deploy, instructions, preprocess: false,
model?, effort?}` for a run; `{node, nodeOnline, sessionId, command, text?}` for
a command), `details.proposalId`, `details.proposalExpires` and
`details.confirmWith`. The registry releases that idempotency claim, so the
confirmed call uses a new key.

Confirmation is server-issued and user-bound (`internal/tools/ghost_confirm.go`):
the proposal is stored as `GHOSTPROP#<userId>#<proposalId>` with a hash of the
binding arguments (`repo, node, agent, deploy, instructions, model, effort` for a
run; `node, sessionId, command, text` for a command — `nodeOnline` is
informational) and a 15-minute expiry. The confirmed call must carry
`confirm: true` and that `proposalId` with the same arguments; the handler
recomputes the hash, refuses a missing or malformed id (`invalid_args`), an
unknown, foreign, expired or spent id (`not_found`) and changed arguments
(`invalid_args`), and consumes the row with a conditional delete before anything
is sent. A lone `confirm: true` therefore launches or sends nothing on any
surface, and a proposal works once. On success `ghost_run_start` returns `status: launched` with
`requestId, eventId, runId, runStatus, commandId, node, repo, agent, deploy`;
`ghost_session_command` returns `status: accepted` with `commandId` and
`delivery: asynchronous`. Both mean ghost queued the work to the computer, which
applies it on its own time; neither means the run finished or the session acted.
KILL, LAUNCH, SAVE_LOCAL and LIST_SAVED are refused by `internal/ghost` before any
invoke. Owner decisions (2026-10-19): commands are NUDGE, ANSWER, PAUSE and RESUME
only; scope is any computer and any repository, and every start or command still
needs its own confirmation.

Upstream state of `ghost_session_command`: POST /command is reachable only once
ghost-cli's internal-invoke allowlist (`lambda/command/internal_invoke.go`)
carries it — plan item ghost-command-allowlist, shipped through the ghost-cli
pipeline. Until then ghost-cli refuses the route with a Lambda FunctionError
before its router runs; `internal/ghost` maps that to `ErrRouteNotAllowed` and
the tool answers `not_configured` with "session commands are not enabled on the
fleet service yet; nothing was sent", releasing the idempotency claim. A 429
from either write route is likewise a definite refusal and maps to
`not_configured` ("try again in a few minutes"), never to `upstream_error`.

`ghost_run_start` request ids are UUIDv7-shaped (millisecond prefix, version 7,
key-bound bits), so `codeupdate.Store.Latest`'s lexical-order assumption holds
beside the UUIDv7 ids of the other `CODEUPD#` rows.

Surfaces: web and Android conversations carry the tools for the account owner.
Remote MCP clients receive the four read tools under `ghost:read` and the two
manage tools, plus the pre-existing `code_update_repos`, `code_update_start` and
`code_update_status`, under `ghost:manage`; a grant issued before those scopes
existed lists only its old tools until the owner reconnects. Owner phone calls
do not carry the ghost tools yet: the phone tool set lives in
`internal/tools/phone_direct.go` and is unchanged by this contract.

## Errors

Errors have the existing Live Ninja `{error:{code,message}}` envelope.
401/403 (`provider_access_denied` or local authentication/owner/scope denial)
requires clearing displayed provider content and stopping requests. 400 means
invalid node/session/cursor or an invalid POST body. 409 `history_rescan_required`
means the object changed: restart and deduplicate. 410 `history_expired` also
requires rescan and an explicit missing-history indication. 413
`history_object_too_large` and 422 `history_malformed` are incomplete-history
failures; 413 `request_too_large` is an oversized POST body. 429 retries later.
431 from the server's header parser is reported to the user as a too-large
request without clearing loaded content. 503 `provider_transport_unavailable`
includes a Ghost deployment that has not added GET /history/sessions and GET
/history/events to internal invoke; 503 `provider_unavailable` is a retryable
read failure. Neither is an empty archive.

Tests are fake-backed transport and HTTP authorization/contract tests. They do
not establish that Ghost's allowlist update is deployed or that the pinned
principal currently has ActionPresign on a particular machine.
