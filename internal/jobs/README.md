# Jobs core

This module provides user-owned in-app reminders and human review checkpoints.
It does not call an AI model, email provider, Ghost, browser, desktop, or device.
A reminder succeeds when its durable inbox receipt is committed. Review approval
only acknowledges the exact displayed checkpoint; it grants no external action.

## Consistency and permission boundary

Each job is one bounded aggregate at `USER#<uid>/JOB#<id>`, including its retained
runs and command receipts. Job/run/next-occurrence changes commit under one CAS.
The production adapter transaction also checks current active PROFILE status and
role. Members require an existing normalized `CONFIG/ALLOW#<identity>` grant;
the exact profile identity and selected grant are fenced in that transaction.
Revoked jobs are conditionally paused, with inverse grant checks and existing
job version preventing purge recreation or a regrant race.

Request IDs deterministically identify job creation; mutations deduplicate the
last 100 requests and also require the expected job version. Changed content or
schedule cancels pending runs and increments the execution generation. Approval
requires that generation, exact job version, owner-scoped run ID, and unexpired
24-hour checkpoint. Read paths persist review expiry with CAS, so manual review
retry remains usable when the scheduled worker is disabled. Retry supports only
failed/cancelled local runs, at most three attempts, one successor per retained
predecessor, and cannot resurrect old content after an edit or job cancellation.

## Scheduling and local execution

Manual `RunNow` and `RetryRun` atomically commit the actual local outcome. Cloud
scheduling is separately gated by `JOBS_WORKER_ENABLED`; the worker must be
explicitly enabled and deployed before scheduled behavior can be claimed.
The worker queries GSI2 partition `JOBS_DUE`, with fixed-width UTC timestamps.
It never scans. Every index result is reread consistently before mutation.
Duplicate workers are safe; no enqueue acknowledgment is reported as completion.

Schedules support one-time, daily, weekdays and weekly in a saved IANA zone.
An empty one-time instant is manual-only. Spring DST gaps skip that date; fall
overlaps use the first occurrence only. Missed recurring instances coalesce to
one overdue receipt, then advance beyond the current time. Resume does not
backfill a missed one-time job. The bundled Go timezone data is used on Windows
and the Lambda runtime. Schedule previews use `NextOccurrences`.

Retain **up to 50 runs**, further reduced as needed to keep the encoded aggregate
below 250KB. Terminal history is compacted first; unresolved reviews are never
silently evicted. Capacity saturation reports an error and backs scheduled jobs
off for 15 minutes, preserving the due occurrence and allowing other jobs to run.
Input limits are 160 title bytes and 2000 instruction bytes. This bounded run
cache is separate from the append-only history described below.

## Durable history, notes and voice proposals

New job/state/run events and user notes are immutable items at
`USER#<uid>/JOBEVENT#<jobId>#<20-digit sequence>`. Each entry is committed in the
same DynamoDB transaction as the job version change and profile/grant checks.
There is no latest-N truncation or TTL on these entries. Full run receipts,
including their output, remain in the ledger after the aggregate cache compacts.
Storage cost grows with retained activity; the API limits individual pages,
not lifetime history. Existing account export and purge query the full USER
partition and therefore include these items. The active-profile transaction
fence prevents a command or event from recreating data during/after purge.
Revoked-account worker suspension only updates the already-existing job; it
creates no history keys that could escape an account purge's captured key list.

`GET /api/v1/jobs/:id/history` returns the newest page in chronological order.
Use `olderCursor` with `cursor` for older pages, or `newerCursor` with `after`
for incremental reconnection. Cursors bind the user, job and optional note
filter. `hasMore` identifies further pages in the requested direction. No
legacy history is invented: the response states the recording boundary, and
the existing `/runs` endpoint still exposes its bounded legacy cache.

`POST /api/v1/jobs/:id/commands` accepts only a `note`, its text (1–2000 UTF-8
bytes), optional retained run ID, expected job version and stable request ID.
The verified user is the actor. A note saves context; it neither changes
execution instructions nor steers/starts a worker. Its response explicitly
states `executionChanged:false`. The recent 100 request receipts replay the
original command response even after subsequent changes; older stale versions
still fail rather than silently repeating a write. Unsupported commands fail
with 501. A separate owner-scoped `ConversationReader` supports provider
pagination without truncation, but no Ghost conversation adapter is installed:
the `/conversation` endpoint returns 501 until an actual provider is configured.

Voice `job_list` and `job_status` read saved jobs and history. The latter accepts
separate `cursor` (history) and `runCursor` (bounded legacy run cache) values.
Create, start,
pause, resume, cancel, retry and note tools only return
`confirmation_required` proposals containing the exact saved version and,
for retry, the original run snapshot. The signed-in UI must obtain a human
confirmation and use the existing versioned REST mutations. Model-supplied
confirmation, account IDs and arbitrary commands are not accepted. Tool audit
and per-invocation account reauthorization remain the shared registry's
responsibility. No external execution integration is claimed by these tools.

`FileStore` is for the local preview. It holds an OS-exclusive lock, writes and
syncs a temporary file, then atomically replaces the snapshot. A crashed process
releases the lock automatically. Unix files are mode0600; Windows inherits the
directory's ACL. It supplies no production identity system: the preview host
must bind to loopback and choose a fixed local user. Call `Close` on shutdown.
It atomically saves jobs and history in a version-2 snapshot and can read older
aggregate-only files. The first successful write upgrades the local format;
keep a backup before testing a rollback to an older binary. DynamoDB additions
are separate keys that older binaries ignore, so rollback preserves stored
events but stops recording new ones. FileStore rewrites its complete snapshot
and is intended for personal previews, not a high-throughput unbounded service.

## Recovery and deployment gates

Malformed job payloads are conditionally removed from the due index and marked
with a quarantine reason/time. Their original bytes remain unchanged for operator
investigation. A concurrent repair or purge defeats the quarantine condition;
transient storage errors are retried rather than quarantined. Malformed index
identities are skipped while following pagination, including empty pages.

There is a hard ten-page per-tick recovery budget. More than that many malformed
index pages requires operator repair: no durable cross-invocation recovery cursor
is implemented. Broad-corruption fairness/alerting and a live AWS integration
exercise remain explicit gates before enabling the default-disabled scheduler.
No provider deployment, cloud mutation, or integration runtime validation is
established by these local tests.

IAM must allow table GetItem, Query, PutItem, ConditionCheckItem,
TransactWriteItems, and UpdateItem (quarantine only), plus Query on GSI2. The
worker reads current user/grant records, not event-supplied account identity.
Scheduling throughput is limited to 100 job aggregates per tick; backlog drains
on subsequent invocations. Multi-user scale still requires explicit admission
and throughput design. History reads use bounded Query pages and strong reads;
no new GSI or Scan is required.

Validation: `go test ./internal/jobs ./cmd/jobs-worker` and `go vet` cover real
file persistence/reopen/OS locking, duplicate requests/workers, CAS cancellation
races, cross-user isolation, expiry without a worker, DST, byte compaction,
capacity backoff, transaction grant/revocation/purge fences, malformed pagination,
quarantine repair/purge races, and the worker-disabled gate. Dynamo tests use
request-shape fakes and cancellation errors; they make no live AWS calls.
