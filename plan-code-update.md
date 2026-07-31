# Plan — Voice-driven code updates (live-ninja → ghost-cli)

**Goal.** Say *"update an application"* to Live Ninja, name the app, describe the change, and
have a Claude (or Codex) session open on that repo on `officepc` and start working — with the
instructions rewritten by Opus first, and progress + results arriving by email.

**Status (2026-07-30):** ghost-cli side **shipped and deployed** (commit `9b7c369`, Deploy ghost-cli:
completed success). live-ninja side **implemented, tests green, awaiting deploy**. Spans two repos:
`c:\dev\live-ninja` (primary) and `c:\dev\ghost-cli`.

### What shipped where

| Workstream | State | Landed in |
|---|---|---|
| WS-A1 `run_now` on `POST /schedule` | done, deployed | `lambda/command/schedule.go`, `schedule_run.go` (`fireRun`, `refuseIfRunInFlight`), `schedule_validate.go`, `schedule_runnow_test.go` |
| WS-A2 internal-invoke path | done, deployed | `lambda/command/internal_invoke.go` + `cmd/bootstrap/main.go`, `internal_invoke_test.go` |
| WS-A3 ghost-cli template | done, deployed | `INTERNAL_INVOKE_PRINCIPAL: live-ninja`. **No** `Lambda::Permission` — same-account invoke needs only the caller's identity policy, and a resource policy would have named a role live-ninja's stack had not created yet. |
| WS-B1 ghost client + matcher | done | `internal/ghost/{client,match}.go` + tests |
| WS-B2 voice tools | done | `internal/tools/codeupdate.go`, registered in `definitions()`, persona clause in `internal/realtime/personas.go` |
| WS-C dispatch worker | done | `internal/codeupdate/{codeupdate,prompt,store,dispatch}.go`, `cmd/codeupdate-dispatch` |
| WS-D progress ingest | done | `internal/webapp/codeupdate_routes.go` (public `/v1/code-update/progress`) |
| WS-E infra + docs | done, awaiting deploy | `template.yaml` (queue/DLQ/worker/log group/IAM/param), `Makefile`, `.github/workflows/deploy.yml`, `contracts/api.md`, `docs/system-map.md` |

### Adversarial review (2026-07-30, 6 lenses x refutation pass)

34 findings raised, **16 refuted**, **18 confirmed and fixed** — except two, recorded as
follow-ups below. The two criticals were both real and both would have hurt:

1. **The deploy gate could be silently deleted.** `fit()` truncated the ASSEMBLED prompt, which
   put the body first and the operating rules last, so truncation ate the tail. ghost-cli's own
   preprocessor bounds its rewrite at 16384 runes, so any rewrite that ran long arrived already at
   the ceiling and deterministically pushed the gate off the end. What survived was "Follow this
   repository's own CLAUDE.md" — whose first line, in these repos, is that pushing to `main` IS the
   production deploy. **A `deploy:false` voice command would have instructed the push it exists to
   forbid, and the confirmation email would still have read "Deploy: NO".** Fixed by reserving the
   fixed-size rules out of the budget and truncating the body; the test now asserts the gate, the
   token and the progress block all survive at nine body lengths across the danger band. My original
   test only checked the rune count and the trailing directive, which is exactly why it passed.
2. **A 264-char Lambda `Description`** (cap: 256) would have failed `CreateFunction` and rolled back
   the entire changeset — nothing in the release would have shipped.

Also fixed: the progress route was missing from the API Gateway authorizer's public allowlist (every
post would have 403'd before reaching the handler); the internal-invoke allowlist was resource-only,
so `/schedule` exposed PUT and DELETE; `GET /schedule` had no principal check at all; a redelivered
queue message re-minted the run token, revoking the live session's credential and resetting its
email cap; the `limit` param was inert (handler read `float64`, the router hands `int`); the TTL was
never enforced in code, so an expired token still authenticated; and the persona splice produced a
doubled em dash.

### End-to-end verification against production (2026-07-30)

Both stacks deployed green. Verified live, in order:

| # | Check | Result |
|---|---|---|
| 1 | Internal invoke reaches ghost-cli | `GET /launch/repos` → 200, **223 repos**, most-recently-pushed first |
| 2 | Method scoping | `PUT /schedule` → `InternalInvokeError: PUT /schedule is not reachable by internal invoke` |
| 3 | Authz gate BEFORE seeding | 403, log reason `deny: principal not in allowlist` — exactly as predicted |
| 4 | Authz gate AFTER seeding | 403 with reason `deny: target node not in principal ACL` — the principal is now recognized, `operator` grants launch, and the node ACL is doing the refusing |
| 5 | run_now semantics | event stored `enabled=false`, `cron=""`, `next_run=""`; run row `RUNNING`, `trigger=run_now` |
| 6 | Launched prompt | contains `DO NOT PUSH` (gate closed), the progress `curl`, a live `cu_` token, and ends with the exact output directive |
| 7 | Launch confirmation email | SES `code-update-started` sent |
| 8 | Progress endpoint (live HTTPS) | valid token → `200 {"accepted":true,"remaining":7}` |
| 9 | Progress endpoint, wrong secret / no credential | `401 {"error":"unauthorized"}` — **identical bodies**, and a 401 rather than a 403 proves the API Gateway authorizer allowlist fix landed |
| 10 | Progress email | SES `code-update-progress` sent |

**Found only by running it:** the node's IoT Thing name is `OFFICEPC`, uppercase. `DefaultNode` was
`officepc`. ghost-cli's node ACL compares exactly with no case folding, and the name is interpolated
into `cockpit/nodes/<name>/cmd` — so the launch was refused as a permissions error, and with a
wildcard principal it would instead have published to a topic nothing subscribes to and left the run
wedged `RUNNING` until the 2 h grace. Fixed and pinned by a test. No amount of code reading would
have surfaced this.

**Not verified here:** the coding session finishing on the node and ghost-cli's completion summary
email. That leg is ghost-cli's pre-existing `lambda/summary` path, unchanged by this work, and needs
the node online with the agent running.

### Follow-ups NOT fixed here (pre-existing ghost-cli gaps, now reachable)

- **`POST /schedule/preprocess` is authorized but never audited.** It is a write gated on
  `ActionLaunch` and a billable Opus spend, yet `SchedulePromptHandler` has no audit sink — so one of
  the five internally-reachable routes writes no hash-chain entry. Fixing it means threading an
  `authz.AuditSink` into that handler, which is a ghost-cli design change beyond this integration.
- **The run token is readable from the stored event prompt.** `GET /schedule` returns each event's
  full prompt, and the prompt is where the plaintext token lives — so any principal that can read the
  schedule can read a live run's token. Impact is bounded (the token authorizes exactly one thing:
  emailing the OWNER about that run, capped at 8 posts, expiring in 24 h), but the cleaner shape is
  to redact `cu_` tokens from the prompt in ghost-cli's GET response.
- **`GET /launch/repos` performs no `Authorize` call.** It checks only that a principal is non-empty,
  which on the internal-invoke path is a tautology. Impact here is nil (listing repos is exactly what
  this integration is for), but a viewer-scoped principal could enumerate every repo the GitHub App
  can reach.

### Defects the tests caught during implementation

- **Double output directive.** ghost-cli's preprocessor *always* appends `outputDirective` to its
  rewrite, so appending blindly emitted it twice and stranded the deploy gate and progress block
  after the sentence meant to be last. `BuildPrompt` now strips any existing directive first.
- **Near-miss `owner/name` returned no candidates.** Ranking only against the repo's *name* half meant
  `JeremyProffittOrg/ghost` matched nothing — precisely when the owner most needs a choice offered.
  `Rank` now also scores the query's name half.
- **Missing `DOMAIN_NAME` on the worker** would have silently omitted the progress block from every
  prompt: no progress emails, nothing reading as broken. Now set, and the worker warns at startup if
  it is ever absent.
- **`docs/system-map.md` was at 7980/8000 chars**, so any new subsystem would have truncated the map
  mid-sentence inside every RCA prompt. Budget deliberately raised to 9200 with the reasoning recorded
  at the constant.

---

## 0. What already exists (verified in code, 2026-07-30)

Most of the hard machinery is already built in ghost-cli. This plan is mostly wiring.

| Capability | Where | Notes |
|---|---|---|
| List every reachable GitHub repo | `lambda/command/launch_repos.go` → `GET /launch/repos` | GitHub App (`/installation/repositories`) or PAT (`/user/repos`), 5 pages × 100, non-archived first then most-recently-pushed. Returns `{repos:[{repo,commit_sha}]}`. |
| Opus prompt rewrite | `lambda/command/schedule_prompt.go` → `POST /schedule/preprocess` | Model `global.anthropic.claude-opus-4-6-v1`. **Async**: 202 + `job_id`, poll `GET /schedule/preprocess-status`. Worker ceiling 600 s (the function's own timeout). Quota 10 jobs / 10 min, gated on `authz.ActionLaunch` for the target node. |
| Launch a coding session on a node | `lambda/command/schedule.go` + `schedule_run.go` | `POST /schedule` stores the event; `POST /schedule/run` dispatches a LAUNCH envelope carrying `run_id` + `output_file`, writes a `RUNNING` run row, TTL 10 min. |
| Output capture + **summary email** | agent `scheduled_complete` → `lambda/summary/handler.go` | Only armed when the LAUNCH carries `run_id` + `output_file` (i.e. the schedule path). Pulls the output file from S3, Bedrock-summarizes it (`claude-sonnet-4-5`), emails **proffitt.jeremy@gmail.com** from **ghost@jeremy.ninja**. Already deployed. |
| `claude` / `codex` selection | `lambda/command/agenttypes.go`, `agent/internal/agentprofile/profile.go` | Closed set `{claude, codex, grok, opencode, antigravity}`. Codex has no session-name flag — the profile already handles that. |
| Capability matrix + hash-chained audit | `lambda/authz/` | `operator` ⊇ `{approve, deny, nudge, answer, kill, launch, pause, resume}`. Allowlist is signed and loaded from SSM; unseeded ⇒ deny-all. |
| Run history / re-run in the cockpit | `GET /schedule`, `GET /schedule/run-output` | Event carries `runs[]` with `status`, `output_key`, `summary`. |

**Gaps this plan closes:**

1. `POST /command` LAUNCH cannot carry `run_id`/`output_file` (`lambda/command/params.go`), so an
   ad-hoc launch gets **no capture and no email**. And schedule validation demands `cron` *or* a
   future `run_at` — there is no "run this once, right now" primitive.
2. live-ninja has no client for ghost-cli and no tools for this at all.
3. The node has **no email path**. `agent/cmd/ghost` has `auth|node|run|schedule|logs|self-update`
   — no `notify`. Mid-run progress email needs new plumbing.
4. live-ninja's web Lambda timeout is **30 s** (`template.yaml` Globals), so a tool handler can
   never block on the Opus rewrite.

---

## 1. Locked decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Add `run_now` to `POST /schedule`** in ghost-cli. When set, `cron` and `run_at` are both omitted, the event stores `enabled=false`, and the same dispatch path `POST /schedule/run` uses fires immediately. | Gives run-now honest semantics *and* keeps the output-capture + summary-email path. Leaves a durable, re-runnable "Update &lt;app&gt;" event with run history in the cockpit. |
| D2 | **live-ninja reaches ghost-cli by direct `lambda:InvokeFunction`**, not HTTPS. | Owner's call. Mitigated by D3 — the capability matrix and audit chain still run, because every handler reads its principal from `RequestContext.Authorizer` and the command Lambda already accepts non-APIGW events. |
| D3 | The internal-invoke path **pins the principal server-side** to `INTERNAL_INVOKE_PRINCIPAL` (env), never from the event, and serves a **closed resource allowlist**. | Otherwise live-ninja could assert `fleet-admin` and reach `/keys` or `/rollout`. |
| D4 | **Async finalization in a live-ninja SQS worker** (`cmd/codeupdate-dispatch`), mirroring `cmd/rca-analyzer`. | The Opus rewrite takes 30–90 s; the voice tool must return in well under 30 s, and the update must still land if the conversation ends. |
| D5 | **Three email legs:** launch confirmation (live-ninja), mid-run progress (agent → live-ninja ingest), completion summary (ghost-cli, already built). | Owner's selection. |
| D6 | Mid-run progress uses a **new public `POST /v1/code-update/progress`** on live-ninja, authenticated by a **per-run bearer token** minted by the worker and embedded in the prompt. | The node has no credential for anything else. Token is per-run, hashed at rest, TTL-expired, post-count-capped. |
| D7 | Defaults: node `officepc`, CLI `claude`, Opus preprocessing **on**, output file `update-report.md`. Codex selectable by voice. | The ask. |

---

## 2. End-to-end sequence

```
Voice: "update an application"
  └─ model calls code_update_repos()                    → top 20 repos (cached 5 min)
Voice: "live ninja"        (or something fuzzier)
  └─ model matches locally; ambiguous → code_update_repos(query:"…") for discovery
Voice: "<what to change>"  → model reads it back for confirmation
  └─ model calls code_update_start(repo, instructions, agent, preprocess, confirm:true)
       ├─ tool validates + enqueues to CodeUpdateQueue           (<1 s, returns immediately)
       └─ speaks: "Queued. I'll email you when it starts."

CodeUpdateDispatchFunction (SQS, 300 s):
  1. mint run token → CODEUPD#<requestId> row (SHA-256 of secret, TTL 24 h)
  2. invoke ghost-cli-command {internal_task:"api", resource:"/schedule/preprocess"}  → job_id
  3. poll  /schedule/preprocess-status every 5 s, ≤ 240 s                             → rewritten
  4. append (NOT via the model) the progress-email block + token + output directive
  5. invoke /schedule  {run_now:true, enabled:false, node, repo, cli, model, prompt, output_file}
       → event_id + run_id      (ghost-cli authorizes, audits, publishes LAUNCH to IoT)
  6. persist run_id on the row; EmailQueue → SES "Update started on <repo>"

officepc agent → launches `claude`/`codex` in the cloned repo with the prompt
  ├─ agent curls  POST https://live.jeremy.ninja/v1/code-update/progress   at milestones
  │     → live-ninja verifies token → EmailQueue → SES "Progress: <repo>"
  └─ writes update-report.md → scheduled_complete → lambda/summary → SES summary email

Voice at any time: "how's that update going?"
  └─ code_update_status(requestId?) → queued|preprocessing|launched|failed + run status
```

---

## WS-A — ghost-cli changes

### A1. `run_now` on `POST /schedule`  *(files: `lambda/command/schedule.go`, `schedule_validate.go`, `schedule_run.go`)*

- Add `RunNow bool \`json:"run_now"\`` to `scheduleRequest`.
- `validateScheduleRequest`: when `RunNow` is true, **require** `cron == "" && run_at == ""` and
  reject `archived`. When false, today's rule is unchanged (exactly one of cron/run_at).
- `handleUpsert`: with `run_now`, store `enabled=false`, `next_run=""`, then fire.
- **Refactor** `schedule_run.go`: extract the post-authorization body of `HandleRun` into
  `func (h *ScheduleHandler) fireRun(ctx, ev, corrID, principal string) (runID string, err error)` —
  mint `run_id` → audit-allow write → `RUNNING` run row → publish LAUNCH → mark `FAILED` on publish
  error → update `last_run_*`. `HandleRun` and `handleUpsert(run_now)` both call it. Audit-then-act
  ordering is preserved: no LAUNCH is ever issued unaudited.
- Upsert response gains `run_id` (and returns 202 instead of 200) when `run_now` fired.

**Tests:** `schedule_test.go` — run_now + cron rejected; run_now + run_at rejected; run_now stores
`enabled=false`; run_now publishes exactly one LAUNCH with `run_id` + `output_file`; audit write
failure ⇒ 500 with no publish and no run row; non-run_now path byte-identical to today.

### A2. Internal-invoke envelope  *(files: `lambda/command/internal_invoke.go` (new), `lambda/command/cmd/bootstrap/main.go`)*

Recognized in `lambda.Start` **before** the APIGW unmarshal, alongside the existing
`DecodePreprocessJobEvent` branch:

```json
{ "internal_task": "api", "resource": "/schedule", "method": "POST",
  "body": "{…}", "query": {"job_id":"…"}, "correlation_id": "…" }
```

Rules — all fail-closed:

- Only when `internal_task == "api"`. An APIGW request cannot unmarshal into this shape with a
  non-empty `internal_task`, so the three event kinds stay disjoint (same argument the preprocess
  worker already relies on).
- **Principal is never read from the event.** It is `os.Getenv("INTERNAL_INVOKE_PRINCIPAL")`.
  Empty ⇒ the whole path is disabled and returns an error.
- `resource` must be in a closed allowlist: `/launch/repos`, `/schedule`, `/schedule/preprocess`,
  `/schedule/preprocess-status`, `/nodes`. **Not** `/command`, `/keys`, `/rollout`,
  `/config-patch`, `/kill-all`, `/settings`.
- Builds `events.APIGatewayProxyRequest{Resource, HTTPMethod, Body, QueryStringParameters,
  RequestContext.Authorizer: {"principal": pinned, "correlation_id": …}}` — **no scope keys**, so
  `authz.WithRequestScope` treats it as unscoped and the allowlist entry resolves unchanged.
- Returns the `APIGatewayProxyResponse` as the invoke payload (status + body), so the caller sees
  the same status codes an HTTP client would.
- Logs `internal_invoke` with resource + correlation id. Never logs the body (prompts).

**Tests:** `internal_invoke_test.go` — event-shape disjointness vs. APIGW and preprocess events;
principal from the event is ignored; unset env disables; every non-allowlisted resource rejected;
allowlisted resource reaches the right handler with the pinned principal.

### A3. Template + allowlist  *(files: `template.yaml`, owner action)*

- `CommandFunction` env: `INTERNAL_INVOKE_PRINCIPAL: live-ninja`.
- `AWS::Lambda::Permission` (or a resource policy statement) allowing
  `live-ninja-codeupdate-dispatch`'s role to `lambda:InvokeFunction` on `ghost-cli-command`.
  Prefer principal-scoped to the exact role ARN, not the whole account.
- **Owner step:** add `live-ninja` to the signed launch allowlist as
  `{role: "operator", nodes: ["officepc"]}` and re-seed via `scripts/sign_launch_allowlist.py` +
  `scripts/reseed-launch-allowlist.sh`. Until this lands every internal invoke denies with
  `ReasonUnknownPrincipal` — which is the correct failure.

### A4. Release

Server-side only — no agent binary change, so **no node roll and no `vX.Y.Z` tag needed** for
WS-A. (If WS-D2's alternative `ghost notify` path is ever revisited, that *would* need a release.)

---

## WS-B — live-ninja: ghost client + voice tools

### B1. `internal/ghost` (new package)

```go
type Client struct { lambda LambdaInvokeAPI; fn string; log *slog.Logger }

func (c *Client) ListRepos(ctx) ([]Repo, error)                    // GET  /launch/repos
func (c *Client) Preprocess(ctx, PreprocessRequest) (jobID string, error)  // POST /schedule/preprocess
func (c *Client) PreprocessStatus(ctx, jobID) (PreprocessStatus, error)    // GET  /schedule/preprocess-status
func (c *Client) CreateAndRun(ctx, LaunchRequest) (eventID, runID string, error) // POST /schedule (run_now)
func (c *Client) Schedule(ctx, eventID) (Event, error)             // GET  /schedule
func (c *Client) Nodes(ctx) ([]Node, error)                        // GET  /nodes
```

- Marshals the WS-A2 envelope, invokes `GHOST_COMMAND_FUNCTION_ARN`, unmarshals the proxy response,
  maps status → typed errors (`ErrNotAuthorized`, `ErrUpstream`, `ErrQuotaExceeded`).
- **Never logs prompts or the run token.**
- 5-minute in-container cache for `ListRepos` (a warm container serves the whole conversation).

**Repo matching** (`internal/ghost/match.go`): normalize both sides (lowercase, strip everything
outside `[a-z0-9]`) on the **name** half of `owner/name`. Rank exact → prefix → substring →
token-overlap. Return all candidates above the floor so the tool can hand the model a
disambiguation list rather than guessing. `"live ninja"` → `JeremyProffittOrg/live-ninja`.

### B2. Tools  *(new file `internal/tools/codeupdate.go`, registered in `definitions()`)*

**`code_update_repos`** — read-only, not side-effecting.
| Param | Type | Notes |
|---|---|---|
| `query` | string, ≤100 | Optional. Absent ⇒ the 20 most-recently-pushed. Present ⇒ ranked match across the full list (**this is the "discovery" leg**). |
| `limit` | integer, 1–50 | Default 20. |

Returns `{repos:[{repo,name,owner}], total, truncated}`.

**`code_update_start`** — `SideEffecting: true` (⇒ idempotency key + IDEMP# guard + audit LOG#).
| Param | Type | Notes |
|---|---|---|
| `repo` | string, required | `owner/name`, validated against the same `repoRe` shape ghost-cli enforces. |
| `instructions` | string, required, 10–8000 | What the owner said. |
| `agent` | enum `claude|codex` | Default `claude`. |
| `node` | string | Default `officepc`. |
| `preprocess` | boolean | **Default true.** False only when the user says not to. |
| `model` / `effort` | string | Optional passthrough. |
| `confirm` | boolean, required | Must be `true`; otherwise `confirmation_required` telling the model to read the repo + instructions back first. |

Handler: validate → resolve repo against the cached list (unknown repo ⇒ `not_found` with the
nearest candidates) → mint `requestId` (UUIDv7) → `SQS.SendMessage` to `CODE_UPDATE_QUEUE_URL` →
return `{status:"queued", requestId, repo, node, agent, preprocess}` in well under a second.

**`code_update_status`** — read-only. `requestId` optional (absent ⇒ most recent for the user).
Reads the `CODEUPD#` row; if it carries a `run_id`, enriches from `GET /schedule` with the run's
`status` / `summary`. Returns `queued | preprocessing | launching | launched | failed` plus
`runStatus`.

### B3. Persona instructions  *(`internal/realtime/personas.go`, `mint.go`)*

`persona_tool_coverage_test.go` fails the build if a manifest tool is not named in
`coreInstructions` (ceiling: 2 unmentioned). **All three tools must be named there.** Add a clause
covering: start from `code_update_repos`; confirm the repo *and* the instructions out loud before
calling `code_update_start`; `preprocess` stays true unless the user says not to; after starting,
say updates will arrive by email and offer `code_update_status`.

### B4. Docs / contracts

- `contracts/api.md`: the three tools in the tool table, the new public
  `POST /v1/code-update/progress` route, and a changelog row.
- `docs/system-map.md`: the tools + the new worker in the component list.

---

## WS-C — live-ninja: the dispatch worker

### C1. Infrastructure  *(`template.yaml`)*

- `CodeUpdateQueue` (VisibilityTimeout **360**) + `CodeUpdateDeadLetterQueue` (maxReceiveCount 3),
  modelled on `RcaQueue`.
- `CodeUpdateDispatchFunction`: arm64, `Timeout: 300`, `MemorySize: 512`, SQS event source
  `BatchSize: 1`, dedicated log group **`RetentionInDays: 7`**.
- Env: `GHOST_COMMAND_FUNCTION_ARN`, `TABLE_NAME`, `EMAIL_QUEUE_URL`, `OWNER_EMAIL`, `DOMAIN_NAME`,
  `CODE_UPDATE_DEFAULT_NODE=officepc`, `CODE_UPDATE_OUTPUT_FILE=update-report.md`,
  `PREPROCESS_POLL_TIMEOUT_SECONDS=240`.
- IAM: `lambda:InvokeFunction` on `ghost-cli-command` (exact ARN), `sqs:SendMessage` on
  `EmailQueue`, DynamoDB CRUD on the table, SQS consume on its own queue.
- `WebFunction` env gains `CODE_UPDATE_QUEUE_URL`; its policy gains `sqs:SendMessage` on it.
- New template `Parameter GhostCommandFunctionArn`, default
  `arn:aws:lambda:us-east-1:759775734231:function:ghost-cli-command` (the FunctionName is pinned in
  ghost-cli's template, so the ARN is deterministic — no cross-stack import coupling). Overridable
  from repo variable `GHOST_COMMAND_FUNCTION_ARN` in `.github/workflows/deploy.yml`.

### C2. `cmd/codeupdate-dispatch/main.go` + `internal/codeupdate/`

Per SQS message:

1. **Mint the run token.** `cu_<requestId>_<64-hex>`. Store `CODEUPD#<requestId>` with
   `sha256(secret)`, `repo`, `node`, `agent`, `userId`, `status`, `postCount:0`, TTL now+24 h.
   Plaintext exists only in memory and in the prompt. (Same discipline as ghost-cli's `gk_` keys.)
2. **Compose the base prompt** (C3).
3. **Preprocess** when enabled: `Preprocess()` → poll `PreprocessStatus` every 5 s to 240 s.
   - `DONE` → use the rewritten prompt.
   - `FAILED` / timeout → **launch with the owner's original instructions and say so explicitly in
     the confirmation email** (`"Opus rewrite unavailable — launched with your original wording"`).
     Not a silent fallback; also recorded on the row. *(Flagged in §7 if you'd rather it hard-fail.)*
   - Send **only the owner's instructions** to Opus. The run token and the progress/output
     directives are appended by us afterwards, so a rewrite can never mangle or leak them.
4. **Launch.** `CreateAndRun` with `run_now:true`, `enabled:false`, `output_file=update-report.md`,
   `event_id = "voice-update-" + slug(repo)` (stable ⇒ repeat updates to the same repo reuse one
   cockpit event and accumulate run history).
5. Persist `event_id` / `run_id` / `status=launched`.
6. **Confirmation email** via `EmailQueue` (template `code-update-started`): repo, node, CLI, model,
   run id, whether Opus rewrote it, and the **final prompt verbatim** — the record of what a voice
   command actually authorized.
7. Any failure ⇒ `status=failed` + reason on the row, failure email, message returned to SQS.
   3 attempts, then DLQ.

### C3. Prompt template  *(`internal/codeupdate/prompt.go`)*

Assembled deterministically:

```
Repository: <owner/name>   (already cloned; you are in the working copy)

<owner's instructions, or the Opus rewrite of them>

Follow this repository's own CLAUDE.md / agents.md conventions for branching,
verification, committing and deploying. If they are absent, do not push.

Report progress by email at three points — when you have finished reading the
code and have a plan, at the halfway mark, and when you are done or blocked:

  curl -sS -X POST https://live.jeremy.ninja/v1/code-update/progress \
    -H "Authorization: Bearer <run token>" \
    -H "Content-Type: application/json" \
    -d '{"status":"working","summary":"<one paragraph, plain text>"}'

status is one of: planning | working | blocked | done.
Do not send more than 8 of these. Never include the token in any file, commit,
log line or report.

Output your detailed actions and findings to update-report.md.
```

The last line is byte-identical to ghost-cli's `outputDirective("update-report.md")`, which is what
arms the capture → summary-email path.

> **Note the deploy consequence.** For `live-ninja` and `ghost-cli`, "follow this repo's CLAUDE.md"
> means *commit and push to main*, and **push to main is a production deploy**. A voice command can
> therefore ship to production. See §7 — say the word and I'll default the prompt to
> "commit locally, do not push" with an explicit "and deploy it" voice opt-in.

---

## WS-D — live-ninja: progress ingest

### D1. `POST /v1/code-update/progress`  *(new `internal/webapp/codeupdate_routes.go`)*

Mounted on `app` **outside** the `/api/v1` auth group — same pattern as
`RegisterAndroidDistributionRoutes` (which is exactly why the path is `/v1/...`, not `/api/v1/...`:
Fiber's group middleware covers the whole `/api/v1` prefix).

- `Authorization: Bearer cu_<requestId>_<secret>`. Shape-check → parse `requestId` → load
  `CODEUPD#<requestId>` → `subtle.ConstantTimeCompare(sha256(secret), stored)`. Any mismatch,
  missing row, expired TTL, or terminal status ⇒ **401**, no detail.
- Body `{status: planning|working|blocked|done, summary: string ≤4000}`.
- **Caps:** conditional-update increment of `postCount`, max 8 per run (429 past that); `summary`
  truncated at 4000 chars; row TTL 24 h.
- Enqueues onto `EmailQueue` (template `code-update-progress`, subject
  `"[<repo>] <status> — voice update"`), always to `OWNER_EMAIL` — the recipient is **never**
  caller-controlled.
- Returns `{accepted:true, remaining:N}`.

### D2. Tests

`codeupdate_routes_test.go`: valid token accepted once; wrong secret 401; unknown requestId 401;
expired row 401; 9th post 429; oversized summary truncated not rejected; recipient always
`OWNER_EMAIL` regardless of body; token never appears in any log line.

---

## WS-E — verification

**Unit / integration (CI, per repo):**
- ghost-cli: `go test ./lambda/command/... ./lambda/authz/...` — WS-A1 + A2 suites above.
- live-ninja: `go test ./internal/tools/... ./internal/ghost/... ./internal/codeupdate/... ./internal/webapp/... ./internal/realtime/...`
  (`persona_tool_coverage_test.go` and `tool_manifest_test.go` are the ones that catch a
  half-registered tool).

**End-to-end, in order — this is the runbook:**

1. Deploy ghost-cli (push to `main`, watch the run). Seed the `live-ninja` allowlist entry (A3).
2. Deploy live-ninja (push to `main`, watch the run).
3. `aws lambda invoke` `live-ninja-codeupdate-dispatch` with a synthetic SQS record for a
   throwaway repo, `preprocess:false` → expect a LAUNCH on `officepc` within seconds, a session
   window on the node, and a confirmation email.
4. Repeat with `preprocess:true` → confirm the Opus rewrite lands within ~90 s and the email shows
   the rewritten prompt.
5. `curl` the progress endpoint with the minted token → expect a progress email; call it 9 times →
   expect a 429 on the last.
6. Let the run finish → expect ghost-cli's Bedrock summary email.
7. **Voice**: "update an application" → "live ninja" → a small real change → confirm → verify the
   session opens on `officepc` and all three emails arrive.
8. Negative: remove the `live-ninja` allowlist entry and re-run → expect a clean denial, a failure
   email, and **no** LAUNCH.

**Delivery:** commit and push to `main` in each repo (that is the deploy trigger for both); watch
each Actions run to a terminal result and report a one-line summary.

---

## 6. Owner steps (only you can do these)

1. Add `live-ninja` → `{role: operator, nodes: [officepc]}` to the signed launch allowlist and
   re-seed it (`scripts/sign_launch_allowlist.py`, `scripts/reseed-launch-allowlist.sh`).
2. Confirm `officepc` is enrolled and online (`GET /nodes`) before the E2E run.
3. No secret is created by this plan — D2 removed the API key entirely, and the run token is minted
   and stored by the worker. Nothing needs `scripts/set-secret.sh`.

---

## 7. Resolved (2026-07-30)

1. **Push-to-main from a voice command → commit locally, explicit voice opt-in to deploy.**
   `code_update_start` gains a `deploy` boolean (default **false**). False ⇒ the prompt says
   "commit your work, do not push". True ⇒ the prompt says to push to `main` per the repo's own
   conventions, and both the confirmation email and the persona read-back name the deploy
   explicitly. The persona sets `deploy:true` only when the owner actually says something like
   "and deploy it" / "and ship it".
   *(The owner's answer here — "commit and push continuously until this plan is complete without
   permissions" — is read as direction for **implementing this plan**: work autonomously, commit
   and push each finished unit without pausing for approval. It is not read as a change to the
   generated prompt's deploy gate, since "this plan" is this document. Correct me if that is
   backwards and I'll flip the default to `deploy:true`.)*
2. **Opus-rewrite failure → launch with the original wording and say so in the email.**
3. **Repo scope → anything the launcher credential reaches.** The signed allowlist already bounds
   the node to `officepc`; discovery across the full list was an explicit requirement.

---

## 8. Risks

| Risk | Mitigation |
|---|---|
| Internal invoke lets live-ninja assert a privileged principal | Principal pinned from env, resource allowlist, `operator`-only entry scoped to `officepc` (A2/A3). |
| Run token leaks via the report file or a commit | Prompt forbids it; token is per-run, 24 h TTL, 8-post cap, and grants nothing but "email the owner". |
| Prompt injection through a repo's own content steering the coding agent | Out of scope here — it is the same exposure any ghost-cli launch has. Bounded by the target repo being owner-chosen by voice, and by the confirmation email recording exactly what was sent. |
| Voice mishears the repo name | `code_update_start` requires `confirm:true` and the persona clause requires reading repo + instructions back; ambiguous matches return candidates instead of guessing. |
| Preprocess quota (10 / 10 min) exhausted | Typed `ErrQuotaExceeded` → failure email naming the quota, message to DLQ rather than a silent retry storm. |
| Existing live-ninja log groups are at 30-day retention, house rule says 7 | New groups here are 7. Reconciling the existing ones is a separate follow-up — not folded into this change silently. |
```
