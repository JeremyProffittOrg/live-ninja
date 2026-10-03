# Jobs history, trusted voice review and Android continuity

This increment builds on production revision `533ecaaca1cd2f7f03abb04635683482cf33b4b0`. It adds to the existing Go/Fiber/DynamoDB and native Compose application; it does not introduce another application or a Ghost API substitute.

## User-visible behavior

- Jobs has a compact list and larger selected-job workspace, responsive light/dark styling, a paginated activity timeline, full retained output, older/latest navigation, and search explicitly limited to loaded history.
- New immutable history entries are stored outside the bounded job aggregate. The boundary is explicit: events begin with this update; existing retained run receipts keep their prior limit. No missing historical conversations are reconstructed or invented.
- Users can save notes to a job. A note is durable context, not an external agent instruction. It does not launch, steer or alter execution instructions.
- Nine voice tools list/status local reminder/review jobs or propose create/start/pause/resume/cancel/retry/note actions. Mutating tools never execute directly. Web and native review show a frozen action, then submit through the authenticated application after a human click. Account/session changes invalidate review and voice context. Jobs tools require the fixed client capability `jobs-review-v1`; older clients must update/reload. OpenAI, Gemini and supported client-executed voice paths receive filtered manifests. Nova Jobs tools remain unavailable because its server-side executor does not provide this trusted review binding.
- Android checks for updates hourly while foregrounded, uses bounded retry backoff, and retains its existing periodic background checks. Official-source metadata, APK size/hash/package/version and installed signing identity must match. Android confirmation remains mandatory. Interrupted confirmation is recoverable through a deliberate retry.
- Android's optional permission guide explains microphone, camera, notifications and approximate location, remembers it was offered, permits skipping, and can be reopened from Settings. It does not batch-launch Android permission dialogs or special settings at startup.

## Storage and API

`internal/jobs/history.go` and `history_store.go` define owner-scoped append-only events. DynamoDB stores `USER#<uid>/JOBEVENT#<jobId>#<sequence>` alongside existing records. Job version CAS and history writes commit together. New history has no latest-N truncation or TTL; export/deletion must cover the same user partition. Cost grows with event count and retained full snapshots, so future retention controls require an explicit product decision rather than silent deletion.

`GET /api/v1/jobs/:id/history?limit=30&cursor=<older>` returns ascending entries within the page. `after=<newer>` fills newer gaps, with `hasMore` declaring backlog. Cursors are scoped to owner/job/filter. `POST /api/v1/jobs/:id/commands` accepts only a note with expected version and idempotency request ID. Unsupported external steering remains unavailable.

`internal/tools/jobs.go` contains the voice routes; `internal/webapp/api_routes.go` wires the real service and scheduling capability into the existing registry. `web/static/js/job-timeline.mjs` and native `ui/jobs/JobHistory.kt` preserve full text and pagination state. Client mutation retries retain exact request identity while an outcome is unknown.

## Security and recovery

All routes retain existing authentication, owner fencing, CSRF and revocation checks. New voice mutations require trusted interface review. Browser proposals carry private in-memory session bindings; those are not serialized into model output. Native calls carry original session identity and a bearer that cannot be silently replayed under another account. No new infrastructure permissions or physical-device installation is part of this increment.

Independent review found and required corrections for: full run output hidden by progress text; stale list/mutation responses; text typed during an in-flight note being erased; light/dark badge contrast; FileStore pointer aliasing; cancellation history lost to aggregate compaction; revoked-account cleanup appending keys during purge; old voice sessions crossing account boundaries; and background Android install confirmation becoming stranded. Final verification evidence and disposition are recorded in the task workspace review reports and release receipt.

## Explicit boundaries

An owner-only Ghost work explorer implements the pinned provider history contract at ghost-cli `672c44030d50a738d514b1d9b9ad89c754d12bae`. It preserves all retained text fragments and lexical string sequences, exposes gaps and the current 30-day lifecycle, and rescans to find late uploads. It requires the upstream GET-only internal allowlist patch and authorized node reads; until those are verified, live integration is not complete. Scheduled work has only ten recent run metadata rows and no authoritative provider-session binding, so users select the actual provider session separately. No job-to-session link is guessed. External coding execution controls remain blocked. The email worker migration and experimental schedule preview remain excluded.

The style reference is verified Lock-X Creator (`tmat-creator`); the user's `tmat-online` alias remains unconfirmed. Screenshots are actual local QA screens, with test content identified as such.

Android version is prepared as 0.3.14 / code 20. Source compilation is not APK publication. A signed release requires the existing main-only release workflow; Android still asks the device user to confirm installation. Signing-key migration is intentionally not automatic.

## Rollout and rollback

Deploy backend and web through the existing main workflow before publishing the dependent Android APK. Verify the exact server version and CI results. Jobs worker scheduling remains governed by its existing setting. Roll back application code if necessary while preserving history rows. Downgrading the local FileStore to an old binary requires a backup/export because its new snapshot format retains separate history. Reverting Android version code does not downgrade installed Android packages; publish a corrected higher version instead.

## Release validation

The frozen Android source passed 491 JVM tests and built both debug APKs. A private offline emulator passed 15 Compose screen tests (10 Jobs, 5 Ghost), using explicitly labeled fixtures. No physical device was changed. Web verification passed 87 unit tests, 36 local Jobs/Approvals browser checks and 16 Ghost browser checks across desktop/mobile. Ghost checks included light/dark accessibility with zero automated violations. Two initial history browser failures were a test locator matching both local and hidden Ghost regions; the locator was scoped to local Jobs and both cases passed. This does not establish live provider availability or actual Android system-install confirmation.

Independent reviews covered backend storage/purge consistency, upstream contract fidelity, original-account privacy, client compatibility, native permission/update recovery and desktop/mobile presentation. The final repository-wide Go tests/vet and production workflow receipts accompany the release evidence in the task workspace.
