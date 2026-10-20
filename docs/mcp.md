# Live Ninja MCP

Endpoint: https://live.jeremy.ninja/mcp

Use a remote MCP connection with OAuth/browser sign-in. The authorization server
supports dynamic public-client registration and PKCE S256. Sign in with the
existing Live Ninja Login with Amazon owner account, then review the client name,
return address, and requested permissions. A browser opening /mcp directly without
a bearer credential receives an authentication challenge; it is not an app page.

## Capabilities

- call_message: read a supplied plain-text message to primary or secondary, the
  owner's privately configured cell aliases. Arbitrary phone numbers are rejected.
  Set `live: true` to deliver the message as a summary in an Azure live conversation.
  Optionally include `payload`, a JSON-encoded object string of at most 12,000 UTF-8
  bytes. The live session can use that data to answer follow-up questions, along
  with the owner's saved profile, memory, rules and guides. Payload text is data,
  not permission to perform actions. On that call the owner can ask for account
  actions (memory, notes, timers, reminders, email, Jobs and rules); job and rule
  changes apply after the owner confirms them aloud. The call cannot place calls,
  send texts or control devices. Omitting `live` keeps the existing spoken playback behavior.
- phone_message_status: inspect a returned requestId, provider/playback state, the
  exact intended script, and an available private recording link.
- memory_search, entity_get, recall_note, memory_write, plan_upsert, forget,
  remember_note: existing owner-scoped memory operations.
- job_list, job_status and job proposal tools: existing Jobs behavior. Create,
  start, pause, resume, cancel, retry and note operations return proposals for
  review in https://live.jeremy.ninja/jobs. They do not start a general AI worker.
- ghost_nodes, ghost_sessions, ghost_session_summary, ghost_run_status
  (`ghost:read`): the owner's ghost coding-agent fleet. ghost_nodes lists
  computers with online state, agent version and the sessions active in the
  last 30 minutes; ghost_sessions pages the retained sessions of one computer
  or the whole fleet (follow `nextCursor`) and lists scheduled runs beside, never
  joined to, sessions; ghost_session_summary returns a deterministic EXTRACTIVE
  summary (no model call) of one session or of the sessions active in a window,
  bounded at 200 events, 48 KB and 4 history pages per session and 10 sessions
  per window, with `partial: true` and `partialReasons` when a bound is hit;
  ghost_run_status reads a run back by eventId+runId, by requestId, or the
  latest. Coverage is retained uploads only; an empty list never proves nothing
  ran.
- ghost_run_start and ghost_session_command (`ghost:manage`): start a coding run
  on one computer and repository, or send NUDGE (text), ANSWER (reply to a
  waiting prompt), PAUSE or RESUME to one session on one computer. KILL is not
  offered. Without `confirm` both return `confirmation_required` with the exact
  proposal in `error.details.proposed` and a server-issued
  `error.details.proposalId`; the client reads the proposal back and, only after
  the owner agrees, calls again with the SAME arguments, `confirm: true`, that
  `proposalId` and a NEW `requestId`. The proposal is bound to the signed-in
  owner and to the exact arguments, lasts 15 minutes and is consumed on use:
  `confirm: true` without a `proposalId` is refused (`invalid_args`), changed
  arguments are refused (`invalid_args`), and an unknown, expired or spent
  `proposalId` is refused (`not_found`); none of those launches or sends
  anything, on any surface. A connector therefore cannot assert confirmation on
  its own — the `confirm` flag passes through untouched, but it only works with
  the proposal the server issued. Success is `launched` (run) or `accepted`
  (command): ghost queued the work to the computer, which applies it on its own
  time. Neither result means the run finished or the session acted; use
  ghost_run_status or ghost_session_summary. `ghost_session_command` depends on
  a ghost-cli change (POST /command on its internal-invoke allowlist) that ships
  separately; until it has, the tool returns `not_configured` with "session
  commands are not enabled on the fleet service yet" and sends nothing.
- code_update_repos, code_update_start, code_update_status (`ghost:manage`): the
  existing code-update tools, now granted with the same permission as the ghost
  manage tools. code_update_start still returns a proposal and starts nothing.

Phone messages return promptly after provider acceptance and continue separately
from the conversation. This does not add general independent AI-task execution.
SMS delivery is not available through this MCP endpoint.
A queued or completed call is not proof that a person heard the message. Only
the signed callback after the spoken script marks playbackCompleted. The script
transcript is the intended text, not speech recognition of the listener.

Mutating MCP tools require a stable requestId. Reuse that ID when checking an
uncertain request; do not generate another ID to force a redial. The shared tool
registry still validates arguments, rechecks the active account, applies owner
permissions and writes its existing audit record.

## Permissions and connection lifecycle

Permissions are phone:send, phone:read, jobs:read, jobs:manage, memory:read,
memory:write, ghost:read and ghost:manage. Tools are listed only for granted
scopes, and calls enforce the same scope again. A connection granted before
ghost:read and ghost:manage existed carries neither: it keeps listing exactly
its earlier tools and keeps working, and the ghost tools appear only after the
owner disconnects and connects again, consenting to the new permissions. MCP credentials cannot access ordinary app API routes.
The server implements stateless Streamable HTTP with JSON responses, not an
optional server-initiated SSE stream.

Access credentials expire after 15 minutes; rotating refresh credentials have a
30-day grant lifetime. Reusing a spent refresh credential revokes that grant.
Disconnect through the client, or use Live Ninja's Log out everywhere to revoke
access through the existing account kill switch. A new connection requires
browser consent again.

Discovery:
- https://live.jeremy.ninja/.well-known/oauth-protected-resource/mcp
- https://live.jeremy.ninja/.well-known/oauth-authorization-server

## Owner phone configuration and costs

Runtime configuration is private in the phone recorder DynamoDB table, never in
this repository: CONFIG/OWNER_MESSAGES binds the existing owner UUID to primary
and secondary destinations and the established Twilio caller number. The main app
invokes only the named recorder Lambda through IAM. Public callback routes verify
Twilio signatures, account, call and request bindings before updating state.

Phone sends share a durable voice lock with the hotel runner. Unknown creation
outcomes retain their lock and request record for reconciliation. Calls last at
most 90 seconds; there is no automatic redial. Each message reserves $0.30.
Outgoing calls use a standing $10 daily allowance (owner-local day) that needs no
approval, and every call this month, incoming and outgoing, shares a $100 cap.
At either limit admission refuses with `daily_allowance_used` or
`monthly_cap_reached`. The month total includes a fixed $8 infrastructure floor.
When a call settles with a measured provider cost (plus recorded model usage for
live calls), that cost replaces its reservation in the day and month totals; an
unknown cost keeps the reservation.

Live message calls instead reserve min($10, remaining day allowance, remaining
month allowance) and last at most ten minutes; the part above the $0.459 telephone
reserve is the model cost cap, and the bridge says goodbye and ends the call before
it is reached. They use
the outgoing allowance; the separate $10/day incoming allowance does not fund
outgoing delivery. Say “hang up” to end a live call, or leave it idle for two
minutes. Continuous speech pauses the inactivity timer. A stable requestId binds
the destination, summary, live option and payload; changing those fields requires
a new explicit delivery request. Provider acceptance does not prove the owner
answered or heard the summary. No automatic retries place another call.

Owner-message recordings remain private in S3 with the existing 180-day lifecycle.
Download links last seven days from their first issuance and exchange for
short-lived S3 URLs. Caller identifiers and secrets must not appear in source or
logs.

## Verification boundaries

Unit tests cover protocol negotiation, tool filtering, scope denial, origin checks,
real shared-registry memory persistence/idempotency, owner phone contracts, OAuth
consent/PKCE/resource binding, transactional token rotation and revocation.
Deployment probes must confirm public metadata and rejection of unauthorized
requests. Those checks do not establish an actual Claude or ChatGPT browser
connection; the owner must complete its sign-in and tool test.

Sources:
- https://modelcontextprotocol.io/specification/2025-11-25/basic/transports
- https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization
- https://modelcontextprotocol.io/specification/2025-11-25/server/tools
- https://developers.openai.com/plugins/build/auth

## Owner web phone controls

The signed-in owner can read `GET /api/v1/phone/budget`: today's outgoing use of
the $10 allowance and this month's use of the $100 cap. The day comes from the
configured timezone (America/New_York by default). The former
`POST /api/v1/phone/budget/grant` approval was retired on 2026-10-19 and returns
410. MCP and model tools cannot change spending. The dated hotel runner retains
its stricter $20 cap.

History exposes `GET /api/v1/phone/recordings?source=messages&limit=25` with a
bounded cursor, and `GET /api/v1/phone/recordings/{CallSid}/media` with source and
disposition (`inline` or `attachment`). Every request rechecks the active owner.
Legacy trial sources require a private explicit owner binding. Media links last
five minutes, support byte ranges, and never expose Twilio credentials. Revocation
prevents new links; issued S3 links remain usable until their short expiry.
Recordings retain the existing 180-day lifecycle. Native Android downloads, Jobs
links, and cross-tab microphone coordination are not yet verified. End conversations
in other tabs before playing recordings.
