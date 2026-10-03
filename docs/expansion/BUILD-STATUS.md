> Release selection (2026-10-03): this main-branch release contains Jobs, native Jobs, trusted rule/coding review, and memory-result accuracy changes. The email-consumer reliability migration is excluded because it requires a pause-and-drain cutover absent from the deployment workflow. Existing email behavior is retained. Earlier email verification below describes local prototype evidence only. Unverified schedule-preview continuation is also excluded. See [RELEASE-2026-10-03.md](RELEASE-2026-10-03.md).

# Live Ninja expansion — implementation handoff

Repository: `C:\dev\live-ninja`
Branch: `codex/jobs-r1-20261003`
Base revision: `f5b57a2c9970b00c509a0f59a9c5d7ce51204ef4`
Status: local working-tree implementation; no commit, push, release or deployment.

This document preserves first-slice validation. The current native Android and trusted approval continuation is documented in [NATIVE-AND-APPROVALS.md](NATIVE-AND-APPROVALS.md); its checks supersede the native-build limitation below.

## Delivered

The real Jobs page and API provide durable reminder receipts and human review checkpoints, with creation/editing, manual runs, once/daily/weekday/weekly schedules, pause/resume/cancel, review acknowledgement, bounded retry, run history, optimistic version checks and request deduplication. This is an additive first slice of the approved expansion, not full feature parity. Backend, UI and worker reuse the existing Go/Fiber, embedded web assets, identity middleware, DynamoDB table/GSI and SAM architecture.

A standalone loopback runner uses the same Jobs service/API/UI with durable local storage and an actual timer. It does not require provider credentials. The production scheduler is source-wired and defaults off. Human review acknowledges a frozen instruction snapshot; it does not execute external work. A completed reminder means a saved in-app receipt, not email delivery.

Related fixes make rule and coding model writes proposal-only, preserve exact proposal fields without treating routine review requests as RCA failures, constrain coding discovery/status to the owner, recheck callback ownership, make memory deletion results truthful about incomplete cleanup, The separately tested email reliability migration is excluded from this release pending a controlled pause-and-drain cutover.

Read [JOBS-R1.md](JOBS-R1.md), [core contract](../../internal/jobs/README.md).

## Independent review and dispositions

The same builders did not approve their own work. Separate architecture/QA and security reviewers inspected concrete sources and reported findings while implementation proceeded. Their checkpoint reports retain the original findings; follow-ups record the fixes and limitations.

| Finding | Disposition |
|---|---|
| Accidental nested SAM parameter and missing transaction permissions | Corrected; local SAM lint validates the final source |
| Large escaped run text exceeding aggregate capacity | Byte-aware terminal-history compaction, preserved pending reviews and durable capacity backoff |
| Expired manual review stuck when worker is disabled | CAS-safe lazy expiry; retry works without background worker |
| Removed member still scheduled | Strong identity reads plus transaction-fenced role, exact identity and current allowlist grant |
| Corrupt due rows block healthy work | Conditional quarantine retains corrupt payload; bounded pagination skips malformed index identities; healthy partial work survives the page-budget error |
| Broad corruption beyond ten malformed pages | Explicit unresolved cloud-enablement gate; no durable recovery cursor/alerting claimed |
| Polling loses reading/focus/history pages | Unchanged renders skipped; expanded details, keyboard focus and loaded history extent retained |
| Only local-store tests initially | Added DynamoDB request-shape, grant/purge/quarantine and worker-gate tests with fakes |
| Model can confirm its own coding launch | Closed: new coding requests are proposals only, with no Ghost, record or queue side effect |
| Routine proposal arguments sent through client RCA | Server accepts-and-ignores confirmation requests before diagnostic construction; client also filters them |
| Legacy callback revocation race | Still documented: active-owner recheck is not an atomic revoke-and-enqueue fence |

Final reviewers found no remaining High defect for ordinary valid-input local reminder/review use and no unresolved blocker in the reviewed security corrections. This is not approval to deploy or a security certification.

Review records are retained in the task workspace under `contributions/11-build-security-review.md`, `12-build-architecture-review.md` and `13-build-security-followup.md`.

## Validation

- Full `go test ./...` passed, including 32 packages with tests.
- `go test -race ./internal/jobs ./internal/emaildelivery ./cmd/email-dispatch ./cmd/jobs-worker` passed.
- All 40 local JavaScript unit tests passed, including exact review proposal forwarding and exclusion of unrelated diagnostic details.
- Final `go vet ./...`, Linux arm64 web/worker builds, and `sam validate --lint --region us-east-1` passed.
- All 24 Jobs browser cases passed across desktop/mobile Chromium. The suite exercises the real loopback backend in desktop and mobile Chromium: persistence/actions, stale writes, lost-response deduplication, schedule types/timezones, actual scheduled Tick, cancellation/retry, pagination/focus retention, accessibility checks, escaped content and local access guards.
- Desktop/mobile/editor screenshots were visually inspected. Automated accessibility checks do not establish complete accessibility certification.

Final command receipts, browser counts, build results, local runner paths and content hashes are recorded in the task workspace delivery manifest. No live DynamoDB/SES/CloudFormation integration or native Android build is claimed by these checks.

## Remaining scope and rollout gates

External email/calendar/files/chat jobs, browser/desktop execution, autonomous coding, generated artifacts/images, event triggers, parallel specialists, cross-channel notifications, memory provenance/suppression, indefinite audit retention and full account recovery are later work. Existing features outside this slice do not automatically become durable Jobs providers.

Before cloud scheduling: isolated live DynamoDB transaction/IAM tests, broad-corruption recovery/alerting, scale/admission limits, operational ownership and an authorized change set. Scheduling is capped at 100 aggregates per tick. Stored history is at most 50 runs and 100 recent command receipts, reduced under byte pressure. The local store rewrites its file and is suitable for local use, not a claim of production scale.

Before email rollout: pause/drain the event source and avoid mixed legacy/new consumers. Unknown outcomes require reconciliation; SES accepted does not mean delivered. Do not roll back to the old marker-skip consumer. Preserve receipts during rollback.

Before coding launch: recoverable dispatch, fresh execution authorization and a real execution sandbox. The continuation supplies action-bound approval receipts only. The current proposal-only gate deliberately disables new voice-driven launches while preserving repository discovery and existing-run status.

Original untracked user files were preserved: `2026-10-01-Agent-Fleet-Dynamic-Rules-System-with-Location-Context.txt`, `bash.exe.stackdump`, and `update-report.md.prev`. No unrelated repository or process was changed.
