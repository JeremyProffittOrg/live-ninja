> Release selection (2026-10-03): this main-branch release contains Jobs, native Jobs, trusted rule/coding review, and memory-result accuracy changes. The email-consumer reliability migration is excluded because it requires a pause-and-drain cutover absent from the deployment workflow. Existing email behavior is retained. Earlier email verification below describes local prototype evidence only. Unverified schedule-preview continuation is also excluded. See [RELEASE-2026-10-03.md](RELEASE-2026-10-03.md).

# Native Jobs and trusted approvals - local continuation

Repository `C:\dev\live-ninja`; branch `codex/jobs-r1-20261003`; base `f5b57a2c9970b00c509a0f59a9c5d7ce51204ef4`. Changes remain uncommitted and undeployed. This adds to Jobs R1; it does not implement the entire expansion roadmap.

## What changed

Android now has a fifth, native Jobs navigation tab. Kotlin DTOs and Retrofit methods call the existing Jobs API through `ui/jobs/JobsRepository.kt`; `JobsViewModel.kt` owns state and `ui/screens/JobsScreen.kt` renders Compose. Users can list/create/edit jobs, choose manual/once/daily/weekdays/weekly schedules and IANA zones, pause/resume/cancel, run reminders, inspect retained history, review an immutable instruction snapshot, and explicitly retry eligible runs. Date/time controls are native. Provider limitations remain visible.

Commands preserve request IDs for explicit retry after an uncertain response and use expected versions. Stale drafts survive conflicts. Reads that predate mutations cannot overwrite newer state; pagination and refresh are serialized. This is in-memory retry state, not a durable process-death outbox. Reminder completion is an in-app receipt, not delivery through email, push or an Android alarm. The cloud worker remains disabled by default.

Conversation rule/coding proposals now expose a Review proposal button even when ordinary tool-call details are hidden. A model cannot approve itself. The shared `web/static/js/approval-review.mjs` renders exact snapshots using text nodes and explicit human buttons. `/approvals` adds owner coding preparation and paginated receipt history. Native Android gets Jobs review UX; rule/coding review dialogs are web surfaces in this slice.

## Rule approval contract

`POST /api/v1/rule-reviews` accepts `operation` (save/delete), a UUID `requestId`, `reviewedAt`, `expectedVersion`, `expectedUpdatedAt`, the existing `expectedRule` snapshot when applicable, and `proposed`. Creation uses version zero. Updates/deletes bind description/body/enabled as well as version/timestamp, preventing changed content from passing an ABA comparison. Save preserves disabled state unless explicitly changed.

Review timestamps must be within 15 minutes and no more than one minute in the future. A DynamoDB transaction combines an exact rule CAS, fresh active-profile/allowlist fence and one-use review receipt. The receipt is keyed by user and hashed request UUID, with normalized payload hash and 30-day TTL. Replays, expired decisions and stale snapshots are rejected; browser mutations never auto-retry. Audit data includes rule name and hashes, but not body/description. Existing direct Memory CRUD remains separate.

## Coding approval contract and deliberate execution boundary

`internal/codeapproval` owns immutable intents, a DynamoDB store and a local FileStore. Routes are GET `/api/v1/code-approvals/options`, POST `/prepare`, GET `/`, GET `/:id`, and POST `/:id/approve`. Preparation binds the signed-in owner, repository/node catalog selection and exact instructions/options. Deployment is disabled. Intents expire after 15 minutes; retained visibility is 30 days.

Approval requires `expectedVersion`, `expectedActionHash` and a request ID (the UI generates a UUID; the API accepts 8-128 letters, digits, underscores or hyphens). A persisted random generation nonce participates in the action hash, so recreating a deterministic prepare ID after expiry cannot validate an old approval. The store atomically checks active ownership and the exact action/version. Successful approval returns `approved_blocked`, execution `not_started`, and requires fresh approval before future execution. There is no queue or launch interface in this service. Legacy Ghost dispatch lacks reliable ambiguous-outcome recovery and execution isolation; recording approval does not repair or bypass those dependencies.

Trusted web/Android surface checks, fresh authorization, user partition boundaries, CSRF and scoped-token rejection apply to mutations. JSON decoding rejects unknown fields/trailing data before side effects; regression tests also correct the prior Jobs decoder's response-versus-error ambiguity.

DynamoDB TTL is eventual. Local FileStore enforces expiry for visibility/use but does not physically prune expired bytes. Neither mechanism promises immediate erasure. Ordinary conversation/audit retention still applies.

## Local preview and operational limits

`cmd/jobs-preview` continues to serve real Jobs at loopback only. `/approvals` uses an explicitly unverified fixture catalog (`preview/example`, `PREVIEW_ONLY`), saved in `<jobs-data-path>.approvals`; it never contacts a real fleet. Rule browser tests use declared API mocks because the preview does not connect production rules. Jobs/coding browser tests exercise the local service. Files inherit Windows ACLs; keep their parent directory private.

The native app is not connected to this loopback preview by default. Production endpoints have not been deployed. Shipping the APK before an authorized backend rollout would expose unavailable Jobs APIs. No physical tablet was installed, signed in or modified. Native instrumentation runs only controlled state on a fresh offline emulator; Retrofit serialization tests use an in-process fake transport. No real Android login/backend integration is claimed.

## Verification and independent review

Final counts, command logs, APK hashes and source manifests are in the task-workspace continuation delivery receipt. Coverage includes native DTO/repository/ViewModel regressions, Compose screen interaction, actual loopback Jobs and coding approval browser cases, rule replay/ABA transaction tests, strict decoding, authorization checks, XSS escaping and accessibility smoke checks. Test doubles do not establish live DynamoDB IAM correctness or complete accessibility certification.

Independent security and architecture reviewers recorded findings in `contributions/14-r1-native-approval-security.md` and `15-r1-native-approval-architecture.md`. Resolved findings include one-use rule decisions, coding action-generation binding, stale native reads, native pagination/read overlap, and truncated approval history. Reviewers inspected sources/tests; centralized execution logs provide runtime evidence.

## Rollout and rollback

Before deployment: review the full working-tree diff, test isolated DynamoDB transactions/IAM and account revocation/purge races, decide audit/retention operations, and authorize a backend change set. Deploy compatible backend APIs before enabling native navigation in a distributed app. Keep scheduled cloud work off until the first-slice corruption recovery, scale and operations gates close. Preserve email receipt migration/drain restrictions preserved in the local expansion package.

Rollback may remove navigation and new routes while preserving durable jobs/intents/receipts for reconciliation. Do not revive proposal-driven automatic coding/rule writes or the old email marker-skip consumer. Do not reinterpret an `approved_blocked` receipt as future permission to launch. Real external actions need provider consent, recoverable dispatch, execution isolation and a fresh exact-action approval.
