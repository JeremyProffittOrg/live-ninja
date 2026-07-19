# Live Ninja — QA Report

Read-only QA pass across 8 surfaces. Every surface returned **pass-with-notes**: no blockers, two majors (both contract/IaC accuracy, not runtime breaks), the rest minor/polish. This report drives the fix pass.

## Summary

| # | Surface | Verdict | Blockers | Majors | Minors | Polish |
|---|---------|---------|:-------:|:------:|:------:|:------:|
| 1 | Auth & sessions | pass-with-notes | 0 | 0 | 2 | 1 |
| 2 | Realtime voice backend | pass-with-notes | 0 | 0 | 1 | 1 |
| 3 | Personas platform | pass-with-notes | 0 | 0 | 1 | 0 |
| 4 | Settings + sync | pass-with-notes | 0 | 1 | 0 | 1 |
| 5 | Memory + guides | pass-with-notes | 0 | 0 | 1 | 0 |
| 6 | History + topics + files | pass-with-notes | 0 | 0 | 1 | 1 |
| 7 | Infra, cost, deploy, IaC | pass-with-notes | 0 | 1 | 1 | 3 |
| 8 | Frontend UX & a11y | pass-with-notes | 0 | 0 | 0 | 3 |
| | **Totals** | | **0** | **2** | **7** | **10** |

---

## 1. Auth & sessions

LWA BFF, KMS ES256 JWT, rotating refresh + reuse detection, authorizer, device 10-yr pairing, RFC 8628 user-code, logout/logout-all, allowlist + owner binding.

### Checks run
- JWKS serves live: `GET /.well-known/jwks.json` → 200, single valid EC P-256 ES256 key (kid `36b63221-…`), `Cache-Control public max-age=3600`. VerifyJWT rejects `alg!=ES256`/`alg:none`, verifies ES256 over signing input, checks `iss`/`aud`/`exp`(+60s skew)/`iat`-not-future, validates JWK on-curve via `crypto/ecdh`.
- Authorizer deny-by-default is LIVE: `/v1/account` and `/v1/settings` → 403 with no token and with a malformed bearer; public routes open (`/healthz` 200, `/auth/lwa/login` 302). DefaultAuthorizer (simple response, payload 2.0), no identity sources, no result caching → internal allowlist is the single source of truth, runs every request.
- Authorizer public-path list reconciled against `contracts/api.md` + actual Fiber route registration; exact set + prefixes (`/static/`, `/auth/`, `/.well-known/`). `logout`/`logout-all` deliberately JWT-gated; no protected `/v1/*` inadvertently public.
- `tokensValidAfter` kill-switch verified in authorizer, defense-in-depth in webapp `ExtractAuthContext`, and in refresh handler. `TestHandlerTokensValidAfterKillSwitch` passes.
- Rotating refresh + reuse family-revoke verified in code and live: conditional `TransactWriteItems` rotate-exactly-once; `presented==prevHash` → `RevokeFamily` + `ErrRefreshReuse`; lost-race path re-reads with `ConsistentRead` then family-revokes; security-alert email on reuse. Tests pass; prod shows live rotation.
- Device 10-yr flow + RFC 8628 pairing verified end to end (code, firmware, prod): PKCE S256, 8-char rejection-sampled user code, Authorize-gate-before-user-code, constant-time match, 5-attempt lockout, CAS state transitions, 10-yr `DeviceWindow`. Prod device session familyId matches DEVICE record — lineage consistent.
- CSRF exemption for `/auth/device/pair/confirm` verified LIVE (carries its own one-shot PAIRCONFIRM double-submit, constant-time).
- OAuth login-CSRF/fixation guard verified: `__Host-ln_oauth` state cookie, constant-time compare, one-shot consume (DeleteItem ALL_OLD). LWA two-check Validate (tokeninfo `aud==client_id` + profile + user_id agreement).
- Owner binding + allowlist verified in code and prod: `attribute_not_exists` singleton, owner-or-allowlist, 2× GetItem, no Scan.
- Live contract behaviors match `api.md`: poll unknown nonce → 404 expired; refresh w/o token → 401; logout w/o credential → 200 idempotent.
- No Scan on any serving path (sessions/devices/allowlist). `go test ./internal/auth/... ./cmd/authorizer/... ./internal/webapp/... ./internal/store/...` PASS.

### Findings
- **[minor]** `contracts/api.md` device-pairing routes are stale/inconsistent with the shipped implementation. Doc lists `GET /auth/device/pair/poll?nonce=`, a separate `GET /auth/lwa/device/callback`, and `GET /auth/device/pair/claim`; the real system uses `POST /auth/device/pair/poll {nonce,codeVerifier}` (claim folded in), reuses `/auth/lwa/callback` (device_nonce in OAUTHSTATE), and `/auth/device/claim` as the browser entry leg. Firmware, Android, backend all agree on the real POST shapes — no functional break, but the canonical contract doc and the `internal/auth/device.go` header comment would mislead a third-party implementer.
  *File:* `contracts/api.md` (Auth & Identity table) + `internal/auth/device.go` (header, ~lines 9-19)
  *Repro:* compare `api.md` rows against `RegisterAuthRoutes` (`internal/webapp/auth_routes.go` 69-76) and `firmware/components/ln_net/src/ln_pairing.c` 108-149.
- **[minor]** `RotateRefresh`'s early `ErrInvalidRefresh` return lacks the `ConsistentRead` re-read its sibling (transaction-canceled) path has. A stale GSI1 read immediately after a rotation can spuriously reject a legitimately-newer token (401 + forced re-auth) instead of rotating. Fails closed, self-heals via re-login; effectively unreachable at the 24h device cadence, low-probability for rapid web refreshes.
  *File:* `internal/store/sessions.go:156-164`
  *Repro:* rotate A→B (base table `refreshHash=B,prevHash=A`); GSI1 still returns pre-rotation snapshot; client presents B: `B!=RefreshHash(A)` and `B!=PrevHash(Z)` → early return with no `getSessionConsistent` re-read. Contrast lines 197-210 which do re-read.
- **[polish]** Strict reuse posture means a benign multi-tab web refresh race (two tabs submit the same pre-rotation token from the shared `__Host-ln_rt` cookie) trips a full family revoke + security-alert email. This is the intended design per plan.md M1, not a defect, but aggressive UX worth confirming acceptable for real multi-tab usage.
  *File:* `internal/store/sessions.go:128-222`; `internal/webapp/auth_routes.go:349-358`

### Owner-verify needed
- Full authenticated LWA web sign-in end-to-end (real Amazon login → callback → `__Host-ln_rt` cookie → `/conversation`).
- Real M5Stack device pairing on hardware (on-screen user code → confirm page → PKCE claim → 10-yr refresh; `ProvisionIoT` IoT identity leg still an empty hook, not yet exercised).
- Security emails actually delivered (new-sign-in alert, refresh-reuse alert) to owner inbox — enqueue verified, delivery not.
- Android Custom-Tabs PKCE exchange (`POST /auth/lwa/exchange`) round-trip on real device/emulator.

---

## 2. Realtime voice backend (`internal/realtime` + `cmd/realtime-broker`)

### Checks run
- `go test -count=1 ./internal/realtime/... ./cmd/realtime-broker/...` → PASS (0.67s / 0.50s). accent/voiceprefs/quota/fallback_tools/mint/personas tests green.
- Broker mint is config-bound: model/voice/instructions/tools/turn_detection fixed server-side, 60s TTL, persona resolved server-side (client sends ID only). Verified in code + prod logs.
- Quota gate strictly pre-spend, ordered per `metering.md`: suspension → token bucket (CAS UpdateItem, no read-then-write race) → daily-seconds → monthly-tokens → hourly-burn auto-suspend → concurrency, all before any OpenAI/SSM touch. Prod log shows a real 429 rejection.
- Concurrent-session slots: `RecordMint` writes slot post-spend (mint never self-counts); `CheckSession` skips `checkConcurrent` at redemption (documented prod fix); release via `store.ReleaseSessionSlot` on Final transcript flush (`api_routes.go:736`) with exp-expiry + TTL backstop.
- Persona resolution server-side + anti-injection intact: bare IDs from clients, refs composed server-side, `resolveStoredPersonaRef` live GetItem re-check, `ComposeCustomInstructions` frames user style as non-authoritative. No Scan.
- Tool router re-authz + idempotency: `Reauthorize` mandatory and per-call; side-effecting tools require `idempotencyKey` + mark-before-execute conditional PutItem; identity from verified claims; unexpected args rejected; `send_email` external send gated by owner allow-list (not model's `confirmExternal`).
- Fallback cascade (STT/LLM/TTS + tool-capable turn) all proxied through the broker (sole key holder); 2× retry w/ backoff on transient 429/5xx, non-429 4xx not retried; `TurnWithTools` returns tool_calls verbatim. Prod log shows fallback-turn ok.
- No Scan on any serving path.

### Findings
- **[minor]** The model-facing tool manifest bound into every realtime session (`internal/realtime/mint.go` `toolManifest`) is a hand-maintained duplicate of the enforcement schema (`internal/tools` ParamSpecs) with NO cross-check test — and they have already drifted. `realtime` never imports `internal/tools`; `fallback_tools_test` only checks intra-package consistency.
  *File:* `internal/realtime/mint.go`
  *Repro:* `set_timer` — manifest advertises `seconds` required, bounds 1..86400; router (`internal/tools/scheduler.go`) enforces min 5 / max ~1yr, treats seconds/inSeconds as optional-either → model-sent `seconds=1..4` rejected `invalid_args` though manifest said valid. `set_reminder` — manifest advertises `at`+`message` both required and only those two; router makes `at` optional and also accepts `inSeconds`/`seconds` offsets the model is never told about (lost capability). Common path works; bounds/required/param-set contracts differ and nothing guards future drift.
- **[polish]** Stale doc comments describe the accent chain as `personaPrefs ?? top-level voiceAccent`, omitting the persona suggested-accent step the code/tests implement (`pref ?? suggested ?? top`). Documentation-only; behavior correct and tested.
  *File:* `internal/realtime/voiceprefs.go` (lines 11-12, 88-93); `cmd/realtime-broker/main.go` handleMint comment (line 313).

### Owner-verify needed
- Real voice session end-to-end (no mic here): mint → WebRTC connect to OpenAI → spoken turn → tool call round-trip through `POST /api/v1/tools/invoke`.
- Authenticated browser mint+connect: confirm resolved voice/accent actually applied to audio heard.
- Audible accent rendering: a suggested-accent persona (Noir Detective → new-york) actually speaks with the directive-driven accent.

---

## 3. Personas platform

Built-in registry + user CRUD + share-on-platform + editor wiring + mint resolution.

### Checks run
- Built-in registry: 17 personas (default + 16 styled). Every suggested voice is in the 10-voice set. Josh Lyman (deputy-chief) voice = `ash`, NOT `marin` (`personas.go:136`). 5 newer character personas all present with valid voices/accents.
- Accents used by built-ins (noir=new-york, bard/butler=british, sommelier=french) all in `SupportedAccents`; `TestAccentCatalogAndDirectivesInSync` enforces a mint directive per non-none accent. PASS.
- Every styled persona embeds the operational core; default instructions == `coreInstructions` byte-for-byte (`TestDefaultPersonaUnchanged`). Anti-injection composition framing present.
- User CRUD: cap 100 via count-before-create; validation (name≤80/desc≤200/instr≤4000/voice must be realtime voice); conditional PutItem prevents overwrite + ghost update; built-in IDs guarded from create/update/delete/share (403).
- Share-on-platform: CATALOG mirror write-through (share/unshare/edit-while-shared), attributed, server-side copy only, instruction text never returned for builtin/shared. Tests pass.
- Resolution order builtin → own → shared with mint-time live recheck; GetItem key lookups only (no Scan); deleted own + unshared mirror both fall back to default at mint.
- Anti-injection: `qualifyPersonaRef` rejects any client ID containing `:` or `#` → default; wired into BOTH mint paths (`api_routes.go:441` + `:810`).
- No Scan; `node --check` on `personaeditor.mjs`/`personas.mjs`; go build/vet clean. Editor element IDs all resolve; end-to-end editor wiring verified. Prod: 0 shared mirrors, 0 owner custom personas (clean empty state).

### Findings
- **[minor]** `TestBuiltinPersonaSeedSet` samples only 11 of 17 built-ins and does not pin the voice-per-persona map, so a regression reverting Josh Lyman to `marin` — or a wrong voice on any of the 6 unsampled personas (sommelier, heh-heh-duo, swamp-master, cool-intensity, pirate-captain, butler) — would not be caught. Current code is correct; coverage gap, not a defect.
  *File:* `internal/realtime/personas_test.go`
  *Repro:* change deputy-chief voice to `marin`, run `go test ./internal/realtime/ -run TestBuiltinPersonaSeedSet` — still passes (only checks membership in `allowedRealtimeVoices`, not exact voice per persona).

### Owner-verify needed
- Authenticated `GET /personas` renders the grouped library table (only 302 observable unauthenticated).
- Full authenticated editor round-trip: create custom persona, edit voice/accent via conversation-page editor, share, copy a shared one, confirm `personachanged` refresh + mid-session pending banner.
- Actual voice/accent output per persona in a live session (Josh Lyman audibly `ash`, noir carries New York accent).

---

## 4. Settings + sync

### Checks run
- `contracts/settings.schema.json` parses as valid draft-2020-12 JSON.
- Go tests pass uncached (store defaults/migration/optimistic-concurrency, webapp validate+normalize/render/personaPrefs cap/legacy themeStyle migration, realtime voice/accent chains). `node --check` on settings/conversation/realtime/personaeditor/theme JS.
- Optimistic-concurrency 409: `attribute_not_exists(pk) OR version = :expected` → `ErrVersionConflict` → 409; `settings.mjs reconcile409()` re-GETs, remote-wins touched fields, re-applies unrelated edits, retries once. Verified in code + test.
- personaPrefs migration: seeds `personaPrefs[currentPersona]` from top-level voice/accent once (key-presence guard prevents re-seed). Prune-oldest cap 200 server-side.
- micEagerness/turn-detection/noise_reduction mapping byte-consistent between server mint and client `updateAudioInput`. Cross-checked vs `mint_test.go`.
- Cross-tab localStorage ping writes `ln.settings.version`; storage listener re-GETs and applies delta; writer never self-fires; storage-blocked degrades gracefully.
- Appearance zone split works in code + data across store/HTTP/SSR/pickers; legacy single `themeStyle` migrates to `liveStyle` on read AND write. Prod owner item confirms two-zone shape.
- Prod endpoint auth fail-closed (302/403). Forward-compat: `additionalProperties:true` at every level; unknown fields round-trip.

### Findings
- **[major]** `contracts/settings.schema.json` is stale for the shipped appearance zone split: it still documents only the pre-split single `themeStyle` + `accentColor`, but the entire live implementation (store default, HTTP validation, webapp, `theme.js`) and prod data use two-zone `appStyle` + `liveStyle` + `accentColor`. The schema is declared "the ONLY settings document across all three surfaces (web, Android, M5Stack)" relied on by "firmware written years apart" — an Android/M5Stack implementer building appearance from the schema codes against the removed `themeStyle` and never learns about appStyle/liveStyle. Runtime unbroken only because `additionalProperties:true` silently accepts undocumented fields. Contract-accuracy defect, not a functional break.
  *File:* `contracts/settings.schema.json:230-250`
  *Repro:* `grep -rn 'themeStyle\|appStyle\|liveStyle' contracts/` — schema is the sole remaining reference to `themeStyle`; every other layer + prod DynamoDB uses appStyle+liveStyle.
- **[polish]** Schema `theme` default is `system` (line 149) while store `DefaultSettings` returns `light` (owner-locked). Deliberate/documented in code, forward-compat tolerates, but the canonical schema default no longer matches the served default.
  *File:* `contracts/settings.schema.json:149`

### Owner-verify needed
- Authed browser autosave + 409 reconcile: change a control, confirm "All changes saved"; concurrent edit from a second device triggers the remote-wins-refresh toast.
- Cross-tab live-session apply: change Mic pickup / turn detection in tab B, confirm tab A applies mid-session via `session.update` with the "Listening settings updated" notice.
- Mid-session mic-eagerness chip audibly changes end-of-turn behavior (needs mic).
- Per-persona voice memory end-to-end: distinct voices for two personas, switch, confirm each speaks its saved voice and `personaPrefs` persists in DynamoDB.

---

## 5. Memory + guides

Entity/EMB store, Titan embedder, `memory_*` tools, mint directive, guide injection, `/memory` page.

### Checks run
- `buildAPIToolsRegistry` wires `Deps.Memory` (`api_routes.go:541-543,571-585`) — the exact fix for the "memory failed" not_configured incident. CONFIRMED.
- All 5 memory tools registered (`registry.go:318-322`: memory_search/memory_write/entity_get/plan_upsert/forget); every handler guards `deps.Memory==nil` → `CodeNotConfigured`.
- PROD: `/live-ninja/lambda/web` shows `memory_write=not_configured` at 13:00/16:33/17:10 today (the incident), then `memory_write ok` from 21:18 and `memory_search ok` at 22:30 today by owner — fix deployed today, works in live sessions. 0 "embedder unavailable" warnings in 7d.
- Live Bedrock invoke of `amazon.titan-embed-text-v2:0` (us-east-1) → 512-dim normalized vector (matches `EmbedDim=512`).
- IAM: `bedrock:InvokeModel` scoped to the single titan-embed-text-v2:0 ARN inside WebFunction role (`template.yaml 362-367`).
- Owner data: ENT#place home + work addresses both `embedded=true` with matching EMB# items (model titan-embed-text-v2:0), via Query (no Scan).
- `memoryUsageDirective` unconditionally appended to every mint (`mint.go:620`). Guide injection wired end-to-end via broker `LoadEnabledGuides` (single-partition Query) → suffix.
- `/memory` page registered (`pages_routes.go:192`), 302 on prod; `memory.mjs` `node --check` ok. No Scan on serving paths. `go test ./internal/memory ./internal/tools ./internal/store ./internal/webapp` pass.

### Findings
- **[minor]** The tool schema the model sees (`mint.go` hardcoded `toolManifest`) and the schema actually enforced (`tools` package Definitions) are two independent hand-maintained sources with no cross-check test; `tools.Registry.Manifest()` is referenced only in tests (effectively dead in prod). They currently match for all memory tools, so no functional bug, but a future edit to `internal/tools/memory.go` params (rename/new required field) would silently diverge from what the model is told. (Same root class as finding 2's realtime manifest drift.)
  *File:* `internal/realtime/mint.go:99` (toolManifest) vs `internal/tools/memory.go` / `registry.go:347` (Manifest)
  *Repro:* change a memory tool param name/required-set in `internal/tools/memory.go` and rebuild — no test fails, mint.go still advertises the old schema.

### Owner-verify needed
- A real voice session confirming the model actually CALLS `memory_search` on "what is my home address" (prod logs already show it succeeding today, but fresh end-to-end voice confirmation is owner-only).
- Confirm the fix commit is the currently-deployed WebFunction version (inferred from prod log timeline).

---

## 6. History + topics + files

Transcript sink capture, CONVSESS# dedup, topics-extract, `/history` filters, tool-calls in history, topic delete, file tools, deliverables.

### Checks run
- Transcript user-turn capture VERIFIED end-to-end in prod: owner LOG# rows for recent sessions alternate user/assistant cleanly in seq order (11-turn session has all 5 user turns, seq 1-11). The just-fixed "user turns missing" bug is genuinely fixed — capture centralized in `conversation.mjs attachTranscriptRendering` (single feeder of `sink.addTurn`) covering delta accumulation, authoritative finals, transcription-failure fallback, onBeforeFinal drain, with double-log guards.
- CONV titles in prod populated from first user utterance — confirms `conversationTitle()` sees real user turns.
- CONVSESS# dedup VERIFIED: all 18 owner CONV# rows — every sessionId appears exactly once, zero duplicates. `ClaimConversationSession` pins one canonical ts via conditional PutItem + consistent-read fallback; client latches `finalSent`. (4 CONVSESS markers vs 18 CONV rows is consistent — dedup fix is today's commit `3e49d4f`, older sessions predate it.)
- topics-extract tests pass (idempotency/concurrent-claim/second-final convergence). TREF conditional put, convCount bumped only on new ref, CONV written last. Prod CONVs topic-tagged.
- `/history` filters Query-only, NO Scan: `ListConversations` (sk BETWEEN, device via FilterExpression), `ListSessionTurns`, `ListTopics`, `ListDevices` (GSI2). `TestListConversationsUsesOnlyQueryAndKeyLookups` asserts it.
- History filter controls are populated lookups (device `<select>` from `/api/v1/devices`, dates `type=date`, topics checkbox chips from `/api/v1/topics`).
- Tool-calls in history: single top-of-page Show-tool-calls toggle persisted to localStorage; tool cards have focusable Details disclosure + hover/focus reveal; server projects role=tool audit rows; merged transcript stable-sorted by ts.
- topic delete: `DELETE /api/v1/topics/:id` → `DeleteTopic` removes topic + TREF refs, leaves conversations. Tests pass.
- File tools: `file_list`/`file_read`/`file_create` registered + in manifest; atomic no-overwrite via DELIVNAME# conditional claim; `SafeName` blocks traversal; no delete/overwrite tool exists (`TestNoFileDeleteOrOverwriteToolExists`). 19 file+deliverable tests pass.
- `node --check` on 5 changed JS; go tests pass across topics-extract/store/deliv/tools/webapp.

### Findings
- **[minor]** Device/topic filter + pagination can hide matching conversations: DynamoDB applies `Limit` before the `deviceId` `FilterExpression` in `ListConversations`, so a page can return zero items while `LastEvaluatedKey` (nextCursor) is still set. `history.mjs renderConversations()` treats an empty page as terminal empty-state and hides "Load more" (L382-399), so filtered conversations beyond the first Limit window become unreachable. Latent — owner currently has 18 CONV rows (< 25 page size); triggers once history exceeds a page and a device (or topic — same class via deleted-CONV skips) has no conversations among the newest page. No DB data loss, UI reachability only.
  *File:* `internal/store/topics.go:664` (FilterExpression+Limit) / `web/static/js/history.mjs:398` (empty-page hides Load-more)
  *Repro:* create >25 conversations where the 25 newest are all `web` and an older one is `android`; filter by device=android → page 1 returns 0 items + a nextCursor; client shows "No conversations match" and hides Load-more; the android conversation is unreachable.
- **[polish]** CONVSESS# idempotency markers written with no TTL/expiresAt (`ClaimConversationSession` PutItem sets only pk/sk/sessionId/ts/claimedAt), so they accumulate permanently — one per session — even after the 30-day LOG# rows they guard expire. Negligible at single-user scale but unbounded over years; the sibling ACTIVEUSER marker carries a 48h TTL.
  *File:* `internal/store/topics.go:419`

### Owner-verify needed
- Live voice round-trip capture on Android and M5Stack/device surfaces (web path verified via prod rows; non-web mic/voice not exercisable here).
- Authenticated `/history` browser rendering: tool-call Details disclosure, hover/focus reveal, tooltip fallback, top toggle persistence across reloads.
- Whether the device/topic filter pagination edge case (finding above) is acceptable to defer until history exceeds one page, or fix now (server-side fill-the-page loop, or client keeping Load-more visible while a cursor exists).

---

## 7. Infra, cost, deploy, IaC

`template.yaml`, `deploy.yml`, `samconfig.toml`.

### Checks run
- `go build ./...` clean; `go vet ./...` clean; `go test ./...` all packages PASS. `sam validate --lint` → "valid SAM Template".
- arm64 everywhere: Globals `Architectures:[arm64]`, WakewordTrainJobDefinition + NovaBridgeTaskDefinition `RuntimePlatform ARM64`.
- 6 cost tags in `samconfig.toml` at stack level: Project=live-ninja CostCenter=voice-ai Environment=prod ManagedBy=sam DeployedVia=github-actions Owner=jeremy.
- DynamoDB (prod): PITR ENABLED, TTL ENABLED on `ttl`, GSI1+GSI2 present. No Scan anywhere: repo-wide grep = 0; store `ddbAPI` interface does not declare Scan (compile-time guarantee asserted by `topics_test.go`).
- Broker KMS: `JWT_KMS_KEY_ID` env + `kms:Sign`/`kms:GetPublicKey` scoped to `JwtKey.Arn`.
- `deploy.yml` auth: OIDC only (`role-to-assume vars.AWS_DEPLOY_ROLE_ARN`), zero static keys; secrets flow GH secrets → SSM SecureString, never in template/source.
- Nova gated off: `NovaBridgeEnable=false` hardcoded in deploy step; single `NovaBridgeReady` condition on every Nova resource. No orphan Nova infra in prod (no ECR repo / ECS cluster / ALB).
- Budgets in prod: `live-ninja-monthly-20/50/100` present, tag-filtered `user:Project$live-ninja`, 100% ACTUAL alert.
- Log retention: all 9 `/live-ninja/lambda/*` groups `RetentionInDays=5` (owner debug request), stack-managed.
- Least-priv IAM: TableCrud has no `dynamodb:Scan`; Bedrock scoped to single model ids; `Resource:*` only on actions with no resource-level support (documented). Stack `live-ninja`: UPDATE_COMPLETE.
- golangci-lint: 29 quality issues (15 errcheck, 3 gofmt, 11 staticcheck) — NOT run by CI (deploy.yml runs only `go vet` + `go test`), non-blocking for deploy.

### Findings
- **[major]** `NovaBridgeImageReady` parameter is dead — declared and passed by the deploy workflow but referenced in NO CloudFormation Condition, so the documented two-phase Nova bring-up (skip the ECS TaskDefinition until the image exists) is not actually implemented. Both `NovaBridgeRepo` and `NovaBridgeTaskDefinition` are gated only on `NovaBridgeReady`, so the first time Nova is enabled the repo and TaskDefinition are created in the same changeset with no image present → `AWS::EarlyValidation` ResourceExistenceCheck on the container image fails changeset creation — exactly the failure the template + deploy.yml comments claim to prevent. Dormant today (Nova off, prod reconciled), but the documented enable path is broken.
  *File:* `template.yaml:58-70,104` / `deploy.yml:340-358,379`
  *Repro:* template's only condition is `NovaBridgeReady: !Equals [!Ref NovaBridgeEnable, "true"]` (line 104); `NovaBridgeImageReady` never appears in a `!Ref` outside comments; `deploy.yml:379` passes it but it gates nothing.
- **[minor]** Stale template comment claims the Nova ECR repo is unconditional, but `NovaBridgeRepo` carries `Condition: NovaBridgeReady`. Combined with `DeletionPolicy: Retain`, an enable-then-disable cycle would leave a retained-but-orphaned repo. Harmless now (repo never created).
  *File:* `template.yaml:56-57` vs `1564-1566`
- **[polish]** Orphan comment block in the deploy job describes a removed nova-roll step (actual roll logic lives in the build-nova-container job, lines 218-233).
  *File:* `deploy.yml:395-402`
- **[polish]** Template comments say "30-day retention" for Lambda log groups but actual `RetentionInDays` is 5 (owner debug request). Doc staleness.
  *File:* `template.yaml:139-143,807-820`
- **[polish]** Code is not gofmt-clean and has errcheck/staticcheck debt; CI does not catch it (pipeline runs only `go vet` + `go test`). `golangci-lint run ./...` reports 29 issues incl. 3 gofmt violations.
  *File:* `internal/realtime/catalog.go:1` (+2 more gofmt), plus 15 errcheck / 11 staticcheck

### Owner-verify needed
- First-time enable of Nova (`NovaBridgeEnable=true`) — the latent EarlyValidation image-existence failure (major finding) should be confirmed/fixed before anyone flips Nova on.
- Whether Project/CostCenter tags were activated as Cost Allocation Tags in the Billing console (required for the tag-filtered budgets to match spend) — not verifiable via API.

---

## 8. Frontend UX & a11y

Conversation shell v2, all pages, PWA, CSP, theming.

### Checks run
- Conversation shell v2 wired end-to-end: header removed (conversation.html has no nav partial; the 5 other pages include it), rail command center, mic-sens chips (aria-pressed toggles bound to `settings.micEagerness` + live `session.updateAudioInput`), persona select+edit, New-conversation/cost-badge/state-pill, History/Memory rail links — all IDs resolve.
- Docked settings drawer uses native `<dialog>.showModal` (focus trap + Escape), close button, scrim-click close via `e.target===dialog`.
- Viewport-fixed scroll: `.conv-app` `100dvh overflow:hidden`; `.conv-main__scroll overflow-y:auto min-height:0`; rail scrolls independently.
- All pages: `/` 200 public; `/conversation /settings /history /memory /personas /downloads` all 302 to `/` unauthenticated. Go render tests pass.
- Other pages per house UI rules: real `<table>` with `<th scope=col>`, `<select>` for enumerable filters, date inputs, skeleton + role=status + aria-live states, `<dialog>` editors with aria-labelledby.
- PWA SW: network-first for HTML/navigations (cache-fallback only offline, skips redirected), stale-while-revalidate for `/static/*`, bypasses `/api /auth /.well-known /healthz /sw.js` + cross-origin + non-GET, offline 503 page, skipWaiting in install, clients.claim + old-cache deletion in activate, SW_VERSION busting. Served prod 200 no-cache+ETag.
- Manifest valid: standalone, start_url/scope `/`, theme+bg color, categories, 4 icons resolve 200 with correct content-type + actual PNG dimensions matching declarations.
- CSP deployed matches code exactly: `default-src self; script-src self wasm-unsafe-eval` (no inline, test-enforced); `connect-src self + api.openai.com + wakeword S3 bucket`; `frame-ancestors none; base-uri self; form-action self` (wakeword-bucket origin documented/intentional).
- Theming AA both themes: HAL9000 muted `#c6cdd8` on `#050507` = ~12.7:1; HAL/terminal zones pin text/ink tokens to dark values; light-theme tokens AA-annotated.
- `node --check` on all 19 JS modules + sw.js. Prod HTML `Cache-Control: no-cache` (no stale-HTML risk); hashed assets `public,max-age=31536000,immutable`; theme.js external file (CSP-compliant no-flash boot).

### Findings
- **[polish]** referrer-policy header mismatch: `SecurityHeaders()` sets `same-origin` but prod returns `strict-origin-when-cross-origin` (a CloudFront/edge response-headers-policy override). Both safe; app-level Set silently overridden — worth reconciling so code reflects what ships.
  *File:* `internal/webapp/pages_routes.go:347`
  *Repro:* `curl -sI https://live.jeremy.ninja/ | grep -i referrer` → `strict-origin-when-cross-origin`.
- **[polish]** On the conversation page, Settings/Personas/Downloads are reachable ONLY through the native `<dialog>` settings drawer (rail links only History/Memory; appbar removed). The open handler is guarded by `typeof settingsDrawer.showModal === 'function'`, so a browser lacking `<dialog>` support makes the gear button inert and those three pages unreachable from `/conversation`. Low real-world risk (dialog supported in all evergreen browsers since 2022); no non-dialog fallback nav.
  *File:* `web/static/js/conversation.mjs:1123`
- **[polish]** A user-chosen `accentColor` is applied via `theme.js setAccent()` to `--ln-teal/--ln-cyan/--ln-accent`, and `--ln-base-accent-ink` resolves to `var(--ln-teal)` — so a custom accent also becomes accent TEXT color (links/accent ink). A low-luminance custom accent on a dark zone could drop accent-ink text below AA. Only affects owner self-customization; all four shipped presets are AA. Consider deriving accent-ink independently or contrast-clamping custom accents.
  *File:* `web/static/js/theme.js:43`
  *Repro:* set custom accent `#14306e` on the dark HAL zone → accent-ink links render near-invisible on `#050507`.

### Owner-verify needed
- Barge-in / wake-word detection / live voice turn behavior (needs a real mic).
- Authenticated page runtime in a live browser: drawer focus-trap/Escape, mic-sens chips live-applying, persona `<select>` populating from `GET /api/v1/personas`, transcript streaming into `#transcript`, cost badge on session start.
- Visual AA contrast of the custom-accent text case (polish finding 3) in an authed session with a low-contrast accent.
- PWA install + offline behavior on a device (install prompt / add-to-homescreen / real offline navigation fallback).

---

## Blockers & Majors (ranked)

**Blockers:** none.

**Majors (2):**

1. **[major · Infra] Broken Nova enable path — `NovaBridgeImageReady` parameter is dead.** The documented two-phase bring-up is not implemented: repo + ECS TaskDefinition are both gated only on `NovaBridgeReady`, so the first `NovaBridgeEnable=true` creates the TaskDefinition with no image present → EarlyValidation image-existence failure blocks changeset creation. Dormant today (Nova deliberately off), but the enable path will fail exactly as its own comments warn. Highest-ranked because it is a latent deploy-time failure of a documented operation, not just doc drift.
   *File:* `template.yaml:58-70,104` / `deploy.yml:340-358,379`
   *Fix direction:* add a real `NovaBridgeImageReady` Condition and gate `NovaBridgeTaskDefinition` (and any image-consuming resource) on it, keeping the repo creatable first — or collapse to a documented manual two-step enable.

2. **[major · Settings] `contracts/settings.schema.json` stale for the appearance zone split.** Schema still documents only pre-split `themeStyle`; every runtime layer + prod data use two-zone `appStyle`+`liveStyle`+`accentColor`. The schema is the declared canonical cross-surface contract for firmware "written years apart"; an Android/M5Stack implementer would code against a removed field. Runtime unbroken only via `additionalProperties:true`. Contract-accuracy defect.
   *File:* `contracts/settings.schema.json:230-250`
   *Fix direction:* replace the `appearance` block's `themeStyle` with `appStyle` + `liveStyle` (enum values matching store), keep `accentColor`, and reconcile the `theme` default (`system` → `light`) polish item in the same edit.

**Cross-cutting minor worth batching:** the tool-manifest drift appears twice (findings 2 and 5) — `internal/realtime/mint.go toolManifest` is a hand-maintained duplicate of `internal/tools` enforcement schemas with no cross-check test, and has already drifted for `set_timer`/`set_reminder`. A single cross-check test (assert the realtime manifest against `tools.Registry.Manifest()`) would close both.

---

## Human / mic / device verification checklist

Aggregated `ownerVerifyNeeded` across all surfaces — items QA could not exercise (no authed browser, no mic, no hardware, no billing-console/API-invisible state).

### Authenticated browser flows
- [ ] Full LWA web sign-in end-to-end: real Amazon login → `/auth/lwa/callback` → `__Host-ln_rt` cookie → reach `/conversation`. (Surface 1)
- [ ] Android Custom-Tabs PKCE exchange (`POST /auth/lwa/exchange`) on a real device/emulator. (Surface 1)
- [ ] `GET /personas` returns 200 and renders the grouped library (builtin/mine/shared) when authed. (Surface 3)
- [ ] Full editor round-trip: create custom persona, edit voice/accent, share, copy a shared one, confirm `personachanged` refresh + mid-session pending banner. (Surfaces 3, 4)
- [ ] Settings autosave + 409 reconcile: change a control ("All changes saved"), then a concurrent second-device edit triggers the remote-wins-refresh toast. (Surface 4)
- [ ] Authenticated `/history` rendering: tool-call Details disclosure, hover/focus reveal, tooltip fallback, top toggle persists across reloads. (Surface 6)
- [ ] Conversation page authed runtime: drawer focus-trap/Escape, mic-sens chips live-apply, persona `<select>` populates from `GET /api/v1/personas`, transcript streams into `#transcript`, cost badge on session start. (Surface 8)

### Live voice / microphone
- [ ] Real voice session: mint → WebRTC connect to OpenAI → spoken turn → tool call round-trip via `POST /api/v1/tools/invoke`. (Surface 2)
- [ ] Resolved voice/accent actually applied to the audio heard; suggested-accent persona (Noir Detective → new-york) audibly speaks the accent; Josh Lyman audibly `ash`. (Surfaces 2, 3)
- [ ] Model actually CALLS `memory_search` when asked "what is my home address" in a live session. (Surface 5)
- [ ] Cross-tab live-session apply: change Mic pickup / turn detection in tab B → tab A applies mid-session via `session.update` with the "Listening settings updated" notice. (Surface 4)
- [ ] Mid-session mic-eagerness chip audibly changes end-of-turn behavior on the live session. (Surface 4)
- [ ] Per-persona voice memory: distinct voices for two personas, switch, confirm each speaks its saved voice and `personaPrefs` persists in DynamoDB. (Surface 4)
- [ ] Barge-in / wake-word detection / live voice turn behavior in a browser with a working mic. (Surface 8)

### Device / hardware
- [ ] Real M5Stack pairing on hardware: on-screen user-code display → typed into confirm page → PKCE claim yields 10-yr refresh. `ProvisionIoT` IoT identity leg (Thing/cert) still an empty hook — not yet exercised end to end. (Surface 1)
- [ ] Live voice round-trip capture on Android and M5Stack surfaces — confirm their transcript sink feeds user turns identically to web. (Surface 6)
- [ ] PWA install + offline: install prompt / add-to-homescreen / real offline navigation fallback on a device. (Surface 8)

### Delivery / infra / out-of-band
- [ ] Security emails actually delivered to owner inbox: new-sign-in alert + refresh-reuse alert (enqueue verified, SES delivery not). (Surface 1)
- [ ] Confirm the memory-fix commit is the currently-deployed WebFunction version. (Surface 5)
- [ ] First-time Nova enable (`NovaBridgeEnable=true`): confirm/fix the latent EarlyValidation image-existence failure BEFORE flipping Nova on. (Surface 7)
- [ ] Confirm Project/CostCenter tags were activated as Cost Allocation Tags in the Billing console (required for tag-filtered budgets to match spend). (Surface 7)

### Deferral decisions (owner call)
- [ ] History device/topic filter pagination edge case (Surface 6 minor): acceptable to defer until history exceeds one page, or fix now (server-side fill-the-page loop, or client keeps Load-more visible while a cursor exists)?
- [ ] Strict refresh-reuse posture (Surface 1 polish): confirm the full family-revoke + security-alert on a benign multi-tab web refresh race is acceptable UX for real multi-tab usage.
- [ ] Custom-accent-as-text-color AA risk (Surface 8 polish): confirm acceptable or add independent accent-ink derivation / contrast-clamp.
