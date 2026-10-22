# Alexa text chat and website device settings

Status: implementation in progress, authorized 2026-10-22. Created 2026-10-22.

## Outcome and scope

Build a private Alexa Skills Kit custom skill that sends recognized text to Live Ninja and speaks Live Ninja's text replies. Follow-up questions use the same conversation context. Each Echo used with the skill appears as an Alexa device on the Live Ninja website. The owner can configure its profile, persona, and transcript privacy with the existing device settings controls.

The user requested this file by name. `C:\dev\live-ninja\alexa-plan.md` is the feature plan. Preserve the existing `plan.md`, `backlog.md`, and unrelated work during planning. When implementation starts, add a pointer from the execution checkout's `plan.md`; keep the detailed Alexa status here rather than duplicating it.

Non-goals: telephone integration, Alexa+ partner/MCP integration, raw microphone streaming, replacement of Alexa's voice, public Skill Store publication, mobile changes, new search providers, general autonomous job infrastructure, or changes to existing phone/browser/Android permissions and model choices. Do not implement from this planning request alone.

Planning changes: this file only. Planning proof: inspect the referenced code and platform contracts, verify every milestone has a command and an objective pass condition, check dependencies and file scope, and run a whitespace/content check.

## Locked decisions (user-confirmed; do not revisit)

- 2026-10-22 — "implement alexa-plan.md". This starts the implementation and supersedes the planning-only stop condition. The private development delivery, existing-identity configuration and initial bounded acceptance trial in this plan are in scope. Keep the initial model trial within $1, with no paid tool actions.
- 2026-10-22 — "Yes, test when ready" in response to: "The plan ends with a five-minute test on a real Echo. Will you be available to link the private skill in the Alexa app and test an Echo after the build is ready? I'll continue implementation now."

- 2026-10-22 — "create an alexa-plan.md to impelment an alexa skill that uses live-ninja as text chat interface, and add an alexa device type so we can configure it on the website"
- 2026-10-22 — "can't live ninja also do an interactrive chat, could we use this approach for alexa?" The later planning request selects the text-chat approach.
- 2026-10-19 — Repository instruction: "Do not create or start EC2 instances, including through scripts, IaC, CI/CD, Auto Scaling, recovery or rollback." Any exception needs a new decision for the exact resource.
- 2026-08-16 — Standing deployment authorization covers deployment and bounded delivery repair. It does not convert this request for a plan into an implementation start.

## Verified facts

Read on 2026-10-22. Repository sources were fetched from remote `main`, including snapshot `932ca072198698a706cac416ff8e44209f044acc`. The working directory `C:\dev\live-ninja` is an older branch with unrelated changes. Do not use that directory's old runtime files as the implementation baseline.

- `web/static/js/conversation.mjs`, `sendTyped`: typed messages go to a connected live session, otherwise to `POST /api/v1/fallback/turn`.
- `internal/webapp/api_routes.go`, `handleFallbackTurn`: the HTTP path starts a fresh message list for each request and permits five model/tool iterations. It uses the shared registry. It does not supply persistent multi-turn chat by itself.
- `internal/realtime/fallback.go` and `fallback_tools.go`: the current text model is `gpt-4o-mini`; the HTTP client allows 30 seconds per attempt and two transient retries. The broker holds the model credential; the web function executes tools.
- `cmd/realtime-broker/main.go`: fallback composition includes the stored profile, remembered facts, and the rule index for tool-capable turns. Phone composition in `phone_session.go` additionally provides a useful reference for guides and device-scoped persona handling. Reuse actual helpers, not older system-map claims.
- `internal/store/types.go` and `devices.go`: device rows already have `surface`, name, capabilities, status, and owner. Client registration currently accepts web/Android. Phone settings use a separate server-managed target with ownership and revocation guards.
- `internal/store/settings.go`: `GetEffectiveSettings` overlays sparse device overrides on account defaults. Existing supported section names include `aboutYou`, `persona`, and `privacy`. The persona section also carries voice/accent fields; these must not be presented as controls of Alexa's spoken voice.
- `internal/webapp/settings_scope_routes.go` and `web/static/js/device-settings.mjs`: both contain phone-specific handling for unsupported sections. Adding Alexa must preserve apply-to-all behavior for the other devices.
- `internal/mcpauth/service.go` and `internal/webapp/mcp_auth.go`: existing OAuth serves public MCP clients, requires PKCE S256, rejects client secrets/Authorization headers at token exchange, and binds credentials to MCP. Access tokens last 15 minutes, grants 30 days, and dynamic client registrations 90 days. ASK linking cannot simply reuse that configuration unchanged.
- Existing test homes are `internal/store/device_settings_test.go`, `internal/webapp/device_settings_routes_test.go`, `internal/webapp/fallback_turn_test.go`, `internal/realtime/fallback_tools_test.go`, `internal/mcpauth/service_test.go`, and `tests/web/specs/device-settings.spec.mjs`.
- `Makefile`, `template.yaml`, and `.github/workflows/deploy.yml` build Go arm64 Lambda functions and deploy from `main` with GitHub OIDC. Existing Go Lambda, AWS SDK, UUID, Go test, Python, and Playwright tooling cover this work without a new framework.
- ASK permits repeated intent/response turns, but allows approximately eight seconds for each full response. Progressive speech does not extend that deadline. Free-form `AMAZON.SearchQuery` requires an interaction model and is not an unrestricted transcript feed. See platform sources below.
- The existing page registrar did not establish a public privacy-policy page. Verify all current routes before adding one; do not give Amazon an invented policy URL.

## Implementation defaults and unresolved access

These are engineering defaults, not additional user-confirmed decisions:

- Distribution: private development skill, owner account only, `en-US`, invocation name `live ninja` if Amazon accepts it. Public certification and additional users/locales are separate work.
- Devices: one entry per physical Echo, registered after its first authenticated request. Use existing account defaults until the owner saves an override. Default display name `Alexa`; the website permits a room name such as `Kitchen Alexa`. Do not add a second group-settings database.
- Conversation: a new launch starts a new Alexa conversation. An explicit `resume` can recover an unexpired conversation on the same owned Echo. It never joins an active browser or phone session automatically.
- Voice: Alexa performs recognition and speech synthesis. Live Ninja selects the reply content and persona style. No Live Ninja audio model or telephone service runs for this surface.
- Text model candidate: `gpt-6-luna`, standard processing, `reasoning_effort=none` when using the existing Chat Completions tool format. The official model page supports that combination. Validate availability and response time before accepting it. Configure this only for Alexa; preserve existing fallback and voice model selection. `gpt-4o-mini` is deprecated with a published 2027-01-14 shutdown, so do not make it the new skill's permanent default.
- Bounds: adapter response budget 6.5 seconds; normal-answer target p95 below 6 seconds; worker deadline 30 seconds; at most five tool/model rounds and 512 output tokens per model response. Keep spoken chunks at most 600 characters and offer `more`. These are application choices, not claims about Amazon limits.
- Keep at most 12 recent conversational messages and 24 KiB of conversational text per Alexa session, excluding the separately bounded server prompt/tool manifest. Preserve tool-call/result pairs when trimming. Session recovery expires after 15 minutes of inactivity. Explicit `new conversation` or `stop` invalidates the old context and pending confirmations.
- Reuse existing account quota enforcement. Allow one active Alexa turn per owner. Do not treat the phone's $100 monthly budget as an Alexa budget. Record actual token usage and model identity for every completed/failed turn that reports usage.

Before an unattended implementation run, resolve these access inputs together. First discover existing configuration by names/status; ask Jeremy only for missing owner-controlled inputs:

- The Amazon developer account/vendor and the intended Echo/account/locale. No account or physical Echo was inspected in planning.
- Existing skill ID, if any; otherwise create one private development skill through the delivery workflow. Never create a duplicate after an uncertain creation response.
- Working SMAPI authorization, account-link configuration, and exact Amazon-generated redirect URLs. Credentials are supplied locally through hidden entry, never through chat.
- A physical Echo test window and a bounded paid model-test allowance. Proposed ceiling: $1 of model usage for the initial acceptance run, with no paid tool actions. This ceiling is a proposal until the execution instruction authorizes the trial.

Missing Alexa access blocks account/device acceptance, not code and deterministic tests. No new hardware purchase, public listing, telephone call, or third-party message is part of this plan.

## Architecture and behavior contract

alexa-request-path — keep one chat execution path across fast and slow turns

Flow: Echo -> ASK -> `cmd/alexa-skill` -> durable turn receipt and SQS -> `cmd/alexa-chat-worker` -> shared Live Ninja text-turn service, broker, and tool registry -> stored result -> ASK speech.

Every new chat turn uses the same worker path. The adapter waits only within its remaining response budget. If the result is ready, it speaks it. Otherwise it returns an honest pending receipt and tells the user to say `status`. `status` retrieves the existing result and never submits another model request. Do not keep a second synchronous implementation of the same chat behavior. Do not call an arbitrary background operation a completed result.

Extract the existing fallback orchestration into one small, testable service used by the existing HTTP handler and the Alexa worker. Preserve the HTTP route's external behavior. Keep model calls in the realtime broker. Share the existing registry/dependency construction needed by the second real caller; do not clone the full web application or add a generic agent framework.

Serialize an explicit deadline into broker invocations, because cancelling the invoking Lambda's HTTP request does not automatically propagate a deadline into another Lambda. Each model request and tool boundary must observe the worker deadline and cancellation state.

alexa-identity-contract — authenticate the account and device before using context

Use native ASK-to-Lambda invocation in `us-east-1`. The Lambda resource policy must allow `alexa-appkit.amazon.com` only with the exact skill ID as `EventSourceToken`. Validate the expected application ID in code too, since IAM test invocations can bypass the ASK trigger condition. Reject malformed events and unsupported locales before paid work.

Extend the existing OAuth implementation for one explicitly registered confidential Alexa client and scope `alexa:chat`. Preserve public MCP client behavior, resource checks, and challenge headers. Alexa access credentials authorize only the Alexa adapter; they must not become general app credentials or MCP grants. Bind the Alexa resource to the registered client on the server; do not assume classic ASK supplies MCP's `resource` request parameter. Keep PKCE S256 enabled and validate the exact Amazon-issued redirect allowlist.

Retain 15-minute access tokens and the existing 30-day grant policy initially. Display the resulting re-link requirement honestly. Store the static Alexa client as persistent configuration rather than a 90-day dynamic registration. Revocation and Log out everywhere must stop future skill turns and worker tool calls.

Derive the internal device UUID from the verified owner, skill ID, and Alexa device ID. Persist only the necessary binding; do not expose raw Amazon identifiers or tokens in logs or web labels. A request cannot supply a Live Ninja owner/device ID to select someone else's context. A browser cannot register itself as Alexa. Revoked device rows must not be revived by a voice request; re-enabling requires an explicit authenticated website action.

alexa-chat-contract — support follow-ups without extending Alexa's listening window

- Define `ChatIntent` with one `AMAZON.SearchQuery` slot and valid carrier-phrase samples. Use supported slot elicitation for the next utterance, clearing the previously filled query. Prove bare follow-ups through the actual interaction model before promising them.
- Support launch, help, repeat, more, status, resume, new conversation, cancel, stop, fallback, and session-ended requests. Built-in stop/cancel must take precedence over chat capture.
- While listening remains open, the user can answer without invoking the skill again. After silence closes listening, explain how to reopen/resume. Do not promise indefinite listening or use repeated filler speech as a keep-alive.
- Bind the turn receipt to owner + device + Alexa request ID. Replayed delivery returns the same receipt/result. A deliberate new user request gets a new turn. Serialize changes to a conversation so delayed replies cannot overwrite newer state.
- Use the same owner-scoped profile, memory, rules, guides, and persona composition helpers as Live Ninja's supported server paths. Apply saved device settings on the next turn; invalidate pending confirmations if their governing settings or identity changed.
- Use the server-executable tool catalog. Exclude device-local tools and retain ordinary tool authorization and app-review rules. Do not grant the phone-specific direct-action exception to Alexa. A request that needs web approval returns its existing proposal and a brief direction to the website.
- Preserve server-issued proposal IDs, exact arguments, single-use confirmation, and idempotency. A spoken `yes` is not a bypass of required web approval. Never execute irreversible work merely because retrieved text or a persona requests it.
- Strip unsafe speech markup and return escaped text/controlled SSML. Do not speak credentials, long URLs, code dumps, or raw tool JSON. Store the complete result only under the applicable privacy policy; `more` and `repeat` must not create another model charge.
- Natural Alexa session expiry does not automatically discard a pending bounded turn: retain its result for the 15-minute recovery window. Explicit stop/cancel prevents further work at the next cancellable boundary. Report any operation that already completed; cancellation is not rollback.

alexa-device-contract — make website configuration effective and honest

Add `SurfaceAlexa = "alexa"` to the existing device model. Register real Echo entries through the trusted adapter path, not `PUT /api/v1/devices/current`. Preserve owner checks, customized names, active/revoked state, device limits, and unknown settings fields.

Reuse Device settings under About you, Persona, and Privacy. Alexa entries support those three sections. Reuse the current selected-device, account-default, inheritance, reset, and version-conflict behavior. Label the surface `Alexa` and explain that changes apply to the next Alexa turn.

Persona controls change Live Ninja's writing style/personality. Existing voice/accent fields may remain round-trippable in the section document for compatibility, but the Alexa summary must say Alexa controls the spoken voice. Do not advertise Voice engine, Microphone, Wake word, Turn detection, or Appearance as settings that control an Echo. Unsupported Alexa sections must not break apply-to-all for eligible web/Android devices.

Privacy controls permanent Live Ninja transcript history and memory extraction. Multi-turn operation still needs short-lived encrypted server context. Disclose its 15-minute recovery window. With transcript storage off, do not create permanent conversation history or feed exchanges into memory extraction. Explicit stop/new conversation removes transient content; expired content is rejected immediately and DynamoDB TTL performs eventual cleanup. Amazon's history is managed separately in Alexa. Include transient Alexa rows in account purge/export handling as applicable; never rely on a forgotten record prefix.

## alexa-foundation — establish the exact contracts and access prerequisites

depends on: none

### alexa-contract-tests — define the real acceptance cases before integration

depends on: none

- [x] alexa-contract-record — additive Alexa contract and internal/native header boundary documented. Existing settings schema is unchanged; its overrides already hold the selected sections.
- [~] alexa-fixture-set — request contract tests pass for launch/chat/follow-up/status/stop and rejected skill/device/locale/timestamp/intent. Stateful duplicate/expiry/cancellation tests accompany the worker implementation.

Expected files: new `internal/alexa/` package and fixtures; existing contracts and their relevant tests.

Definition of done, from the execution root: `go test ./internal/alexa -run TestContract -count=1 -timeout=5m` exits 0 and covers both accepted and rejected envelopes. This is a future test target, not a command already run during planning.

### alexa-access-check — resolve credentials without exposing values

depends on: none; access discovery can proceed alongside alexa-contract-tests

- [!] alexa-access-record — vendor M3M80BVXTGPEZW (Jeramie LLC) and local ASK access verified; skill listing exhausted and no Live Ninja skill exists. Default private en-US create-once intent applies. GitHub has no ALEXA configuration and SSM has no Alexa/SMAPI publishing parameters. Jeremy must supply dedicated CI publishing credentials through local hidden entry: variable ALEXA_SMAPI_CLIENT_ID and secrets ALEXA_SMAPI_CLIENT_SECRET/ALEXA_SMAPI_REFRESH_TOKEN. Interactive ASK credentials stay machine-local. Model access, generated redirects and device acceptance follow deployment; Jeremy confirmed readiness for the Echo test. Independent code/tests continue.
- [ ] alexa-delivery-helper — add only the required SMAPI create/update/account-link-configuration/build-status/simulation calls to `scripts/alexa-skill.py`, using Python's existing standard-library approach. Add `scripts/alexa-check.bat` as the Windows verification entrypoint and `scripts/test_alexa_skill.py` for state/error handling. No new CLI framework or local schedule.

Configuration names to verify before collecting any missing values: variables `ALEXA_VENDOR_ID`, `ALEXA_SKILL_ID`, `ALEXA_SMAPI_CLIENT_ID`, `ALEXA_ACCOUNT_LINK_CLIENT_ID`; secrets `ALEXA_SMAPI_CLIENT_SECRET`, `ALEXA_SMAPI_REFRESH_TOKEN`, `ALEXA_ACCOUNT_LINK_CLIENT_SECRET`. Keep deployment authorization as AWS OIDC. SMAPI/account-link OAuth secrets are not AWS access keys.

Use existing `scripts\set-secret.bat` for missing GitHub secrets. Display verified URLs: origin `https://live.jeremy.ninja`, owner login `/auth/lwa/login`, existing LWA callback `/auth/lwa/callback`, and OAuth authorization/token routes `/oauth/authorize` and `/oauth/token`. Obtain Alexa callback URLs from the created skill; do not copy sample vendor IDs. Confirm a working privacy-policy URL before setting it in the skill. If none exists, add the minimal accurate public page and its access rule as part of Alexa delivery.

If the policy page is absent, the planned destination is `GET /privacy`, implemented in `internal/webapp/pages_routes.go` with `web/templates/pages/privacy.html` and the necessary `cmd/authorizer/main.go` public-route entry. Extend existing rendering/authorization tests. State the actual Amazon/backend data flow, transcript controls, transient context, and deletion path. Do not claim the new URL exists until its deployed unauthenticated GET returns 200 with the policy content.

Definition of done: `python -B -m unittest discover -s scripts -p test_alexa_skill.py` exits 0; `scripts\alexa-check.bat preflight --evidence C:\dev\live-ninja-test-evidence\alexa\preflight.json` exits 0 only when required access is verified. A missing credential returns a named failure, never an interactive unattended prompt.

## alexa-settings — add owned Echo settings to the website

depends on: alexa-contract-tests

### alexa-device-persistence — bind Echo entries to the existing settings store

depends on: alexa-contract-tests

- [ ] alexa-device-store — implement trusted Alexa registration and explicit owner re-enable behavior, preserving ownership, revocation, name changes, quota, and concurrent-write guards.
- [ ] alexa-settings-resolution — resolve profile/persona/privacy using the verified Alexa device ID. Reject attempts to select another device through client headers or model arguments.

Expected files: `internal/store/types.go`, `devices.go`, existing store device tests, `internal/webapp/device_routes.go`, `settings_scope_routes.go`, and existing device/settings route tests.

Definition of done: `go test ./internal/store ./internal/webapp -run 'Alexa|Device|Settings|Privacy' -count=1 -timeout=5m` exits 0. Acceptance includes two Echo entries with different personas, a foreign owner denial, revoke/re-enable, a rename race, reset to inherited settings, and unaffected phone/browser settings.

### alexa-settings-ui — expose only settings Alexa uses

depends on: alexa-device-persistence

- [ ] alexa-website-controls — show Alexa device names and type, per-device configuration, connection/re-link guidance, and clear voice limitations in existing UI controls.
- [ ] alexa-help-copy — update the Help drawer in the same feature commit with invocation, website configuration, pending-result retrieval, silence recovery, approval behavior, and privacy boundaries. Do not announce the integration as working before its release gate passes.

Expected files: `web/static/js/device-settings.mjs`, related existing settings/device UI files only where required, `web/templates/pages/conversation.html`, `internal/webapp/help_drawer_ui_test.go`, and `tests/web/specs/device-settings.spec.mjs`.

Definition of done: `npm --prefix tests/web test -- specs/device-settings.spec.mjs` and `go test ./internal/webapp -run TestHelpDrawer -count=1 -timeout=5m` exit 0. Test narrow/mobile layout, selected-device isolation, conflict retry, unsupported-section behavior, and actual wording. Run browser tests through GitHub CI on OFFICEPC unless the operator has selected a permitted local driver.

## alexa-chat-service — reuse Live Ninja reasoning with durable turn state

depends on: alexa-contract-tests

### alexa-linked-account — implement the scoped ASK OAuth client

depends on: alexa-contract-tests

- [ ] alexa-oauth-client — extend existing OAuth primitives for the confidential registered Alexa client, PKCE, client-bound resource/scope, refresh, revocation, and owner consent. Preserve existing MCP clients and their tests.
- [ ] alexa-worker-authority — validate the linked active owner/device before admission and again before every tool call. Queue only internal receipt references, never access tokens or model credentials.

Expected files: `internal/mcpauth/service.go` and tests, `internal/webapp/mcp_auth.go` and tests, supporting existing auth/store code only where necessary, and new Alexa validation code. No blanket public-client or cross-resource relaxation.

Definition of done: `go test ./internal/mcpauth ./internal/webapp ./internal/alexa -run 'Alexa|MCP|OAuth|Scope|Revok' -count=1 -timeout=5m` exits 0. Wrong redirects, missing/wrong secrets, missing PKCE, scope escalation, expired grants, and revoked devices must fail without model calls.

### alexa-turn-execution — preserve follow-ups and survive slow replies

depends on: alexa-linked-account, alexa-device-persistence

- [ ] alexa-shared-turn-runner — extract/reuse the existing fallback tool loop and necessary dependency setup for the existing HTTP caller and Alexa worker. Keep the broker credential boundary and server-owned prompt composition. Add the Alexa model option and explicit downstream deadlines without changing other surfaces.
- [ ] alexa-turn-store — persist session/turn receipts in the existing table with conditional writes and bounded key queries. Include request replay, pending/complete/failed/cancelled/expired outcomes, per-owner concurrency, expiry, and cleanup. Add the records to existing account lifecycle handling.
- [ ] alexa-worker-path — implement `cmd/alexa-chat-worker` on the single queue path. Use existing registry reauthorization, argument validation, audit, and idempotency. A failure after an uncertain side effect must not replay the whole turn automatically.

Expected files: new `cmd/alexa-chat-worker/`, `internal/alexa/`, and scoped store chat files; existing `internal/webapp/api_routes.go`/fallback tests, `internal/realtime/fallback*.go`/tests, `cmd/realtime-broker/`, account lifecycle code, and shared dependency setup used by both callers.

Definition of done: `go test ./internal/alexa ./internal/store ./internal/realtime ./internal/tools ./internal/webapp ./cmd/realtime-broker ./cmd/alexa-chat-worker -count=1 -timeout=5m` exits 0. Required cases: referential follow-up, tool result in the next turn, settings changed between turns, one slow turn retrieved later, duplicate queue delivery, failed publish, worker crash, cancellation, privacy-off history, and no duplicate side effect.

## alexa-skill — connect spoken turns to the text service

depends on: alexa-contract-tests

### alexa-dialog-handler — implement the skill lifecycle and bounded responses

depends on: alexa-turn-execution

- [ ] alexa-handler — add `cmd/alexa-skill` with exact skill verification, linked identity, lifecycle controls, and bounded result waiting. Shorten long responses safely; preserve the full eligible result for `more`.
- [ ] alexa-interaction-model — add `alexa/skill.json` and `alexa/interaction-models/en-US.json`. Keep valid carrier phrases and test slot elicitation for conversational follow-ups. Account linking is required before private data or model/tool calls.

Definition of done: `go test ./internal/alexa ./cmd/alexa-skill -count=1 -timeout=5m` exits 0, and `python scripts/alexa-skill.py validate-local` exits 0. Mock delays at the deadline produce an honest pending/error response before 6.5 seconds; stop/repeat/status do not create unintended paid turns. A built skill model must demonstrate that follow-ups map to the expected intent/slot.

## alexa-delivery — release through GitHub and prove the actual device experience

depends on: alexa-access-check, alexa-settings-ui, alexa-dialog-handler

### alexa-infrastructure — add only the required serverless resources

depends on: alexa-access-check, alexa-settings-ui, alexa-dialog-handler

- [ ] alexa-stack — add two arm64 Lambda functions, a turn queue and dead-letter queue, bounded IAM grants, explicit seven-day log groups, and build entries to the existing template/Makefile. Keep skill ingress separate from worker concurrency so slow workers cannot consume every response slot. No EC2, NAT gateway, new load balancer, Secrets Manager, provisioned concurrency, CloudWatch alarm, or dashboard.
- [ ] alexa-ci-publish — extend `.github/workflows/deploy.yml` with idempotent development-skill metadata/model deployment through SMAPI. Commit the tested implementation on main and push here so the private development deployment exists before alexa-acceptance. Monitor the triggered workflows. Use GitHub OIDC for AWS. Do not install or deploy cloud artifacts locally.

Bootstrap order: deploy the real Lambda code without an ASK invoke permission; create/update the private development skill with that Lambda's actual ARN; persist the returned skill ID in deployment configuration; obtain the exact Alexa redirect URLs and configure the registered OAuth client/account linking; apply the exact-ID ASK permission through the same GitHub workflow. Persist create-once identifiers in standard SSM/stack configuration when GitHub variable write permission is unavailable. Never create a wildcard invoke permission or a fake endpoint to break the dependency. Reconcile skill creation before any retry.

Definition of done in Linux CI: `make build`, `sam validate --lint`, `go vet ./...`, and `go test ./...` exit 0. `python scripts/alexa-skill.py verify-deployment --evidence artifacts/alexa-deployment.json` exits 0 only after the stack and interaction-model build reach successful terminal states and the deployed skill/function IDs match.

### alexa-acceptance — verify conversation, settings, and access on an Echo

depends on: alexa-infrastructure

- [ ] alexa-simulator-gate — run the SMAPI simulation suite against the development skill. Verify at least 50 realistic utterances and sequences, including pronouns, correction, proper names, unrelated questions, repeat/more/status, and interrupted sessions. Target at least 95% correct routing for the supported vocabulary.
- [ ] alexa-latency-gate — run 100 bounded warm/cold or delayed test turns. Record total response time, model time, queue wait, final/pending/failure outcome, and token cost. All skill responses must arrive before Amazon's deadline; target normal-answer p95 under six seconds. Pending replies must not disguise a system that never answers ordinary chat promptly.
- [ ] alexa-echo-gate — the operator links the actual Echo and holds a five-minute multi-turn conversation. Change its persona/profile on the website and verify the next turn; test a second Echo if available to prove settings isolation. Verify five-/ten-/thirty-second pauses, stop, re-launch/resume, privacy-off, revoked access, and retrieval of one deliberately slow result. Simulator evidence cannot stand in for these observations.

Definition of done: `python scripts/alexa-skill.py simulate --evidence artifacts/alexa-simulations.json` and `scripts\alexa-check.bat acceptance --evidence C:\dev\live-ninja-test-evidence\alexa` exit 0. The acceptance command must validate actual provider response IDs/timings and the operator's recorded device observations. Missing physical results return failure/unverified; a handwritten `passed=true` is insufficient. All paid work stays inside the recorded trial allowance.

### alexa-release-record — finish delivery without changing the publication audience

depends on: alexa-acceptance

- [ ] alexa-main-release — commit only Alexa feature files on `main`, push, and monitor every triggered workflow to terminal success. Record exact commit, workflow IDs, skill/model version, endpoint ARN, and test evidence. Verify deployed website settings and the enabled development skill.
- [ ] alexa-rollout-summary — document the supported commands, effective settings, costs measured in the trial, account-link renewal behavior, known limits, and the command to disable new Alexa admissions without deleting user data. Keep the skill private.

Definition of done: `scripts\alexa-check.bat delivery --evidence C:\dev\live-ninja-test-evidence\alexa\deployment.json` exits 0 after resolving each recorded workflow ID and running `gh run watch` with `--repo JeremyProffittOrg/live-ninja --exit-status`. The checker must match commit IDs and terminal conclusions, not trust an unrelated green run.

## Execution controls

Implementation checkout: `C:\dev\live-ninja-alexa`, a clean checkout of the repository's current `main`. Do not switch, reset, stash, or clean the dirty `C:\dev\live-ninja` or other active checkouts. Discover an existing clean main checkout first; otherwise create an isolated clone after implementation is authorized. Compare current source with this plan before editing.

Absolute evidence root: `C:\dev\live-ninja-test-evidence\alexa`. All new helpers named above are planned deliverables, not existing successful checks. Record a run-specific subdirectory and identifiers before launching a job. Append commands, exit codes, facts, commits, and verification results here at each milestone. Commit milestones once implementation is authorized; no AI co-author trailer.

No local automation schedule is authorized. Short Windows commands must remain hidden across shell and child processes. Use direct file tools or `ProcessStartInfo` with `UseShellExecute=false`, `CreateNoWindow=true`, and redirected output. Do not use Windows MCP. On OFFICEPC, obtain the operator's chosen browser driver before any local website control; CI browser checks require no desktop control.

Restart policy:

- Local tests: five-minute per-package timeout, at most three total attempts per milestone. Fix deterministic failures before retry; preserve evidence. Never skip a failed assertion to pass a gate.
- GitHub delivery/model build: 60-minute ceiling, at most three corrected attempts. Watch success, failure, cancellation, and timeout. Retry a transient provider failure with 30-/60-second backoff. Repair deterministic pipeline failures before another push. Record workflow/build IDs.
- Alexa turn: the worker executes once per receipt; queue redelivery may recover a never-started turn but must not repeat an uncertain model/tool execution. Configure a small shared redelivery ceiling and a dead-letter path. Deadline-expired or ambiguous operations become explicit failed/unknown results for reconciliation.
- Skill creation: one create attempt until the result is reconciled. A timeout never licenses another skill creation. Updates use the recorded ID.
- Real Echo tests: one bounded acceptance session initially; repeat only after a specific fix and within the recorded spend allowance. No automatic calls, messages, purchases, device actions, or coding-agent launches are test probes.

## Stop conditions (only these)

- planning-only — resolved by the 2026-10-22 implementation instruction above.
- unavailable-access — a required credential, developer account, model entitlement, or Echo test resource is absent. Name the missing item and the owner/provider that can supply it; continue independent code/tests after implementation starts.
- scope-or-authority — public publication, a new paid dependency/framework, unrelated data deletion, broader account access, or another action outside this feature is required. Do not expand silently.
- ec2-boundary — any proposed workflow creates or starts EC2. Keep the non-EC2 path; otherwise require an explicit new resource decision.
- platform-contract — the built interaction model or actual Echo cannot support the promised chat flow, or an account/security requirement cannot be met. Record the failed proof and propose a smaller claim; do not ship a command-only feature as interactive chat.
- retry-or-budget — a recorded shared retry ceiling or trial allowance is exhausted. Preserve results and mark the dependent milestone blocked; do not hide the failure or spend past the allowance.

## Execution log

- 2026-10-22 — foundation red/green checks: TestContract first failed on missing Event, then passed. TestAlexaConfidentialClientIsIsolatedFromMCP first failed on missing implementation, then full mcpauth passed. HTTP TestAlexaOAuthTokenRequiresSecretAndCannotAccessMCP failed with invalid_client before the scoped confidential-client route, then Alexa/MCP/OAuth tests passed. TestAlexaDevicesAreOwnedDistinctAndExplicitlyReenabled failed before the store methods, then Alexa and PhoneSettings cases passed. Privacy render/authorizer cases first failed on absent page/route, then passed. Final `go test ./internal/alexa ./internal/mcpauth ./internal/store ./internal/webapp ./cmd/authorizer -count=1 -timeout=5m` exited 0 for all five packages. Help updated with the real privacy page link; Alexa availability is not advertised yet.
- 2026-10-22 — dedicated CI credential blocker mailed by operator_reporter; SES MessageId 010001a163ad7d60-465dd92b-dffa-4fb9-a684-201cfee38335-000000. No local Alexa deployment or credential copying was performed. ASK vendor/skill discovery was read-only. User asked whether dedicated CI credentials already exist; no values are requested through chat.

- 2026-10-22 — implementation started in clean main clone C:\dev\live-ninja-alexa. Preserved C:\dev\live-ninja and other dirty checkouts. GitHub names/status inventory found no ALEXA variables or secrets. Existing ASK CLI default profile successfully returned vendor M3M80BVXTGPEZW (Jeramie LLC), ROLE_ADMINISTRATOR; no credential values were printed. Read the approved contract, deployment rules, existing OAuth, device settings and fallback orchestration. Reconfirmed official GPT-6 Luna Chat Completions function calling requires reasoning_effort=none. Test interfaces are the plan's approved ASK envelope, OAuth, device/settings routes and shared turn runner; no new test framework.

- 2026-10-22 — Planning only. Read current remote source for chat, device settings, OAuth, tests, and delivery. Verified ASK response/session/account-linking contracts and the text-model lifecycle. Created this plan; no Alexa resource, credential, deployment, or device setting changed.
- 2026-10-22 — PowerShell plan audit passed: 10 milestones each have a dependency and a command-based definition of done; 22 pending task names are unique; all dependencies resolve; no trailing whitespace. `git status --short -- alexa-plan.md docs/alexa-integration-review.md` reported only the two local draft documents in that scoped check. Application tests were not run for this planning-only change.

## Platform sources

Consulted 2026-10-22. Recheck before implementation if the run starts later.

- [ASK session behavior](https://developer.amazon.com/en-US/docs/alexa/custom-skills/manage-skill-session-and-session-attributes.html) — continued turns, reprompts, and session attributes.
- [ASK response deadline](https://developer.amazon.com/en-US/docs/alexa/custom-skills/send-the-user-a-progressive-response.html) — approximately eight seconds; progressive replies do not extend it.
- [SearchQuery constraints](https://developer.amazon.com/en-US/docs/alexa/custom-skills/slot-type-reference.html#amazonsearchquery) — slot and sample requirements.
- [Account linking](https://developer.amazon.com/docs/alexaplus/account-linking/configure-authorization-code-grant) — ASK authorization code, PKCE, client credentials, and actual callback URLs.
- [Lambda skill permissions](https://developer.amazon.com/en-US/docs/alexa/custom-skills/host-a-custom-skill-as-an-aws-lambda-function.html) — exact skill-ID restriction.
- [Skill simulation API](https://developer.amazon.com/en-US/docs/alexa/smapi/skill-simulation-api.html) — development-stage tests and terminal results.
- [OpenAI deprecations](https://developers.openai.com/api/docs/deprecations) and [GPT-6 Luna](https://developers.openai.com/api/docs/models/gpt-6-luna) — current replacement guidance and function-calling requirements. Model entitlement and latency remain to be tested.
- Background review: `C:\dev\live-ninja\docs\alexa-integration-review.md`. This implementation plan selects ordinary ASK text chat; it does not depend on Alexa+ preview access.
