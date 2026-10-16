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
- phone_message_status: inspect a returned requestId, provider/playback state, the
  exact intended script, and an available private recording link.
- memory_search, entity_get, recall_note, memory_write, plan_upsert, forget,
  remember_note: existing owner-scoped memory operations.
- job_list, job_status and job proposal tools: existing Jobs behavior. Create,
  start, pause, resume, cancel, retry and note operations return proposals for
  review in https://live.jeremy.ninja/jobs. They do not start a general AI worker.

Phone messages return promptly after provider acceptance and continue separately
from the conversation. This does not add general independent AI-task execution.
A queued or completed call is not proof that a person heard the message. Only
the signed callback after the spoken script marks playbackCompleted. The script
transcript is the intended text, not speech recognition of the listener.

Mutating MCP tools require a stable requestId. Reuse that ID when checking an
uncertain request; do not generate another ID to force a redial. The shared tool
registry still validates arguments, rechecks the active account, applies owner
permissions and writes its existing audit record.

## Permissions and connection lifecycle

Permissions are phone:send, phone:read, jobs:read, jobs:manage, memory:read and
memory:write. Tools are listed only for granted scopes, and calls enforce the
same scope again. MCP credentials cannot access ordinary app API routes.
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
most 90 seconds; there is no automatic redial. Each message reserves $0.30 under
the $20 monthly allocation. Additional daily spending needs explicit owner
permission; the MCP tools cannot authorize it themselves. The allocation includes
the existing fixed reserve and a month-scoped floor carried from hotel trials.

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
