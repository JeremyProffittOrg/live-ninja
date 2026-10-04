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
Every route is a read; Live Ninja always calls Ghost with GET.

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
