> Release selection (2026-10-03): this main-branch release contains Jobs, native Jobs, trusted rule/coding review, and memory-result accuracy changes. The email-consumer reliability migration is excluded because it requires a pause-and-drain cutover absent from the deployment workflow. Existing email behavior is retained. Earlier email verification below describes local prototype evidence only. Unverified schedule-preview continuation is also excluded. See [RELEASE-2026-10-03.md](RELEASE-2026-10-03.md).

# Jobs R1 — local implementation

Built in `C:\dev\live-ninja` on `codex/jobs-r1-20261003`, based on `f5b57a2c9970b00c509a0f59a9c5d7ce51204ef4`. This is an additive first release of the expansion plan, not completion of the full roadmap. No deployment, release, push, provider grant or production data change was performed.

## Available behavior

The `/jobs` page uses the existing embedded templates, asset pipeline, identity and API middleware. It supports creating, editing, pausing, resuming and cancelling jobs; manual runs; human review acknowledgements; bounded run history; version conflicts; and idempotent command retries. Two execution kinds are implemented:

- **Reminder:** creates a durable in-app receipt. It does not send email, push notifications or an OS alarm.
- **Review:** freezes the title/instructions in a run for a signed-in human to acknowledge. Acknowledgement is not authorization for an external action. Editing or cancelling invalidates outstanding review intent. Review intent expires after 24 hours.

Manual runs commit their result synchronously with the command receipt. Once, daily, weekdays and weekly schedules use saved IANA zones. A DST gap skips that date; an overlap fires only at the first occurrence. Missed recurrence is coalesced, not replayed as a notification burst. Cloud scheduling is disabled by default through `JobsSchedulingEnabled=false`.

External email/calendar/file/chat actions, browser/desktop execution, autonomous coding, document/image generation, cross-channel notifications, multi-agent jobs are not implemented by this slice. A native Android Jobs tab now uses this same contract; see [NATIVE-AND-APPROVALS.md](NATIVE-AND-APPROVALS.md). The interface marks unavailable providers explicitly. Existing voice/conversation features continue separately; no model Jobs tool is registered.

## Local runner

From the repository:

```powershell
go run ./cmd/jobs-preview -listen 127.0.0.1:8793 -data C:\private\live-ninja-jobs\jobs.json
```

Choose an existing private local directory. Open `http://127.0.0.1:8793/jobs`. This executable serves the real Jobs API/UI with a single local identity and durable FileStore; it is not a preview facade or the production auth server. It permits literal loopback binding only, checks Host and Origin, issues a process-local bearer token, and makes no cloud execution calls. Other app services are not connected. The local ticker runs while the process is alive. Stop it with Ctrl+C and restart using the same data path to retain jobs. The store takes an exclusive OS lock and uses atomic file replacement. Windows ACLs are inherited; it does not configure private ACLs for you. The JSON file contains job text and should not be committed.

## API and storage

All production paths are under `/api/v1/jobs`, use verified identity, reject scoped session credentials, enforce fresh account authorization and preserve CSRF protection. Mutations require a signed-in web or Android surface. IDs supplied by clients never select a different user partition.

| Method/path | Behavior |
|---|---|
| GET / | Paginated jobs and honest capability metadata |
| POST / | Create with `requestId` |
| GET /:id | Read owned job |
| PATCH /:id | Replace input with `expectedVersion` and `requestId` |
| POST /:id/pause, resume, cancel | Versioned lifecycle command |
| GET /:id/runs | Retained history |
| POST /:id/run | Manual execution of the two in-app kinds |
| POST /:id/runs/:runId/approve, retry, cancel | Versioned run action |

Input is `{title,instructions,kind,schedule}`. Title is limited to 160 UTF-8 bytes and instructions to 2000. Schedule is `{kind:"once"}` for manual, `{kind:"once",at:"RFC3339 with offset"}` for once, or `{kind:"daily"|"weekdays"|"weekly",timezone:"America/New_York",time:"09:00",weekday:1}`. Sunday is 0. Use the exact version returned by the latest response; stale changes return409. Requests are strict JSON with a 16KiB limit. Request IDs have 8–128 bytes and must be reused only with the same operation and payload after an uncertain response.

`internal/jobs` owns lifecycle, scheduling and storage. Each DynamoDB aggregate is `USER#<user>/JOB#<id>`, so existing user-partition export/purge can discover it. CAS and transactional identity fences protect writes. The existing GSI2 indexes due jobs; no serving Scan is added. Histories retain up to 50 runs and up to 100 command receipts, subject to aggregate byte limits. Idempotency is bounded retention, not an eternal exactly-once guarantee. Run text is intentionally snapshotted for meaningful review; do not log it.

`cmd/jobs-worker` is a gated arm64 Lambda with a one-minute schedule. `Makefile` includes its build. SAM contains the worker, log group, narrow table/index permissions and default-off parameter. This code has not been tested against a live deployed DynamoDB table or EventBridge schedule.

## Related safety changes

- Rule save/delete model tools produce structured proposals in `error.details`; the trusted Review proposal dialog or signed-in Memory interface performs the actual user edit. `confirm=true` from a model cannot persist a rule. Proposal text is kept out of generic diagnostic messages.
- Coding tools are owner-only. New model-requested launches return proposals only; neither conversational confirmation nor a deploy flag launches Ghost. A trusted action-bound approval flow now records a receipt, but execution remains blocked pending dispatcher recovery and sandbox work. Repository discovery and existing run status remain available. Callback handlers recheck active owner status, but their legacy profile read and notification enqueue are not an atomic revocation fence. Existing Ghost execution is not an OS/network sandbox. Routine confirmation proposals are excluded from RCA queues; ordinary user invocation audit and conversation retention still apply.
- Email uses durable pending/claimed/sending/accepted/unknown receipts and a single SES SDK attempt. Ambiguous outcomes never blindly resend. Explicit rejections can retry. SQS partial batch reporting is enabled in source. SES acceptance is not inbox delivery. This email change is preserved only in the local expansion package and is not part of this main release.
- Memory deletion now says whether an entity or learned record was removed and reports partial cleanup. It does not claim source erasure, suppression of re-derivation or removal from an already-open conversation.

## Before any future deployment

Review the diff and independent findings, run the documented checks, inspect a SAM change set, and obtain deployment authorization. Keep scheduling disabled until worker authorization, capacity, lifecycle and real DynamoDB integration tests pass in an isolated environment. Validate account removal/purge races and clock/DST behavior there. Email migration must pause/drain the event source and avoid mixed old/new consumers; rollback cannot return to the old marker-skip behavior. Preserve receipts during rollback. No release should promise external job execution or complete memory erasure.

Next work should prioritize provider capability/consent records and one real read/draft connector. Follow with delivery/reconciliation UI, suppression/provenance memory controls, sandboxed coding/browser execution. Native Android Jobs is implemented locally and needs an authorized backend/app rollout. Each needs its own adversarial acceptance tests and explicit rollout decision.
