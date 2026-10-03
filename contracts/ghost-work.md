# Ghost work and retained archive reads

Backend source contract pinned to Ghost commit
`672c44030d50a738d514b1d9b9ad89c754d12bae`:
`lambda/command/history.go`, `nodes.go`, `schedule.go`, `internal_invoke.go`.
The history routes additionally require Ghost's GET-only internal-invoke
allowlist update. No new credentials, IAM grants, capture or uploads are requested.

All routes are GET-only under `/api/v1/ghost-work`. They require an authenticated,
personal web/Android session and a fresh strongly consistent active-owner check.
The shared Ghost principal has no per-member identity mapping, so members and
device/provider-scoped tokens are denied. Every response uses `Cache-Control:
no-store`. Do not persist archive text in application telemetry or caches.

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

The provider bounds each page to 256 fragments/192 KiB of text and each fragment
to 48 KiB. Live Ninja preserves visible text without another truncation. Follow
next_cursor through empty pages until absent. Keep resume_cursor at the current
end for polling. Refresh/reconnect must rescan from the beginning to recover
late uploads before the previous cursor, deduplicating by stable event ID.
Virtualize rendering, not retained client content. Pagination failure never
means history is complete.

Coverage is always retained_only; capture completeness is unknown. The existing
90-day provider lifecycle is deployed in Ghost revision `b0e4f0a0e68e2e8703a0e10ea2f8e0fd4cf69997`. Missing, expired, never-uploaded or
overwritten bytes cannot be reconstructed. Gaps and existing redactions remain
visible. Thinking, hidden prompts and arbitrary metadata are excluded upstream;
this text projection does not preserve images or attachments.

## Errors

Errors have the existing Live Ninja `{error:{code,message}}` envelope.
401/403 (`provider_access_denied` or local authentication/owner/scope denial)
requires clearing displayed provider content and stopping requests. 400 means
invalid node/session/cursor. 409 `history_rescan_required` means the object
changed: restart and deduplicate. 410 `history_expired` also requires rescan and
an explicit missing-history indication. 413 `history_object_too_large` and 422
`history_malformed` are incomplete-history failures. 429 retries later. 503
`provider_transport_unavailable` includes a Ghost deployment that has not added
GET /history/sessions and GET /history/events to internal invoke; 503
`provider_unavailable` is a retryable read failure. Neither is an empty archive.

Tests are fake-backed transport and HTTP authorization/contract tests. They do
not establish that Ghost's allowlist update is deployed or that the pinned
principal currently has ActionPresign on a particular machine.
