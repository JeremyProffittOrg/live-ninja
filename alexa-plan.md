# Alexa text chat and website device settings

Status: private publishing, account linking, shared settings and normal account-tool mode are deployed. Both Alexa functions report 478197841edb5edc1cc68506c4393358fb71463a, active/successful; ALEXA_ENABLED=true and the worker's ALEXA_TRIAL_MODE=false were read back on 2026-10-23. Final workflow 40056173776 completed SUCCESS, including browser quality. Jeremy confirmed linking and used Alexa earlier. Formal timed Echo acceptance and the current tools/settings Echo retest are not yet recorded. Authorized and created 2026-10-22.

## Outcome and scope

Build a private Alexa Skills Kit custom skill that sends recognized text to Live Ninja and speaks Live Ninja's text replies. Follow-up questions use the same conversation context. Each Echo used with the skill appears as an Alexa device on the Live Ninja website for naming and access control. One shared Alexa settings target controls profile, persona and transcript privacy for all Echoes.

The user requested this file by name. `C:\dev\live-ninja\alexa-plan.md` is the feature plan. Preserve the existing `plan.md`, `backlog.md`, and unrelated work during planning. When implementation starts, add a pointer from the execution checkout's `plan.md`; keep the detailed Alexa status here rather than duplicating it.

Non-goals: telephone integration, Alexa+ partner/MCP integration, raw microphone streaming, replacement of Alexa's voice, public Skill Store publication, mobile changes, new search providers, general autonomous job infrastructure, or changes to existing phone/browser/Android permissions and model choices.

Implementation files and command-based checks are listed under each workstream. The deployed backend is not proof that the private skill or a physical Echo works; those remain separate acceptance gates.

## Locked decisions (user-confirmed; do not revisit)

- 2026-10-23 — "Linked successfully" confirms actual Alexa account linking after the header fix.
- 2026-10-23 — "One shared Alexa group (Recommended)" supersedes per-Echo settings overrides. Native device identity and revocation remain distinct.
- 2026-10-23 — "Current conversation only (Recommended)" sets the unfinished-request lifetime, including Alexa resume. New conversations start fresh.
- 2026-10-23 — the owner requested that Alexa inherit signed-in account tools like Phone calls. Normal tool execution is authorized now; the earlier setup-trial-until-Echo-gate default is superseded. Existing tool approvals remain. Automated acceptance still uses bounded read-only/synthetic cases.

- 2026-10-22 — "implement alexa-plan.md". This starts the implementation and supersedes the planning-only stop condition. The private development delivery, existing-identity configuration and initial bounded acceptance trial in this plan are in scope. Keep the initial model trial within $1, with no paid tool actions.
- 2026-10-22 — "Yes, test when ready" in response to: "The plan ends with a five-minute test on a real Echo. Will you be available to link the private skill in the Alexa app and test an Echo after the build is ready? I'll continue implementation now."
- 2026-10-22 — "No, I need setup steps" in response to the missing dedicated SMAPI publishing-credential question. Provide the local hidden-entry authorization helper; do not copy the machine's ASK CLI authentication state to GitHub.
- 2026-10-22 — "amazon" in response to where authorization failed. The failure is on Amazon's page. The later provider error identified a client ID with a leading tab; the separate local start-page fix addressed a different defect. See the current publishing blocker and execution log for the corrected-input attempt.

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
- Devices: one entry per physical Echo for naming/revocation after its first authenticated request. One shared managed Alexa target, offered before first use, controls About you, Persona and Privacy through the existing deviceOverrides document. Use account defaults until that shared target is customized. Preserve historical per-Echo data without continuing a second settings path.
- Conversation: a new launch starts a new Alexa conversation. An explicit `resume` can recover an unexpired conversation on the same owned Echo. It never joins an active browser or phone session automatically.
- Voice: Alexa performs recognition and speech synthesis. Live Ninja selects the reply content and persona style. No Live Ninja audio model or telephone service runs for this surface.
- Text model candidate: `gpt-6-luna`, standard processing, `reasoning_effort=none` when using the existing Chat Completions tool format. The official model page supports that combination. Validate availability and response time before accepting it. Configure this only for Alexa; preserve existing fallback and voice model selection. `gpt-4o-mini` is deprecated with a published 2027-01-14 shutdown, so do not make it the new skill's permanent default.
- Bounds: adapter response budget 6.5 seconds; normal-answer target p95 below 6 seconds; worker deadline 30 seconds; at most five tool/model rounds and 512 output tokens per model response. Keep spoken chunks at most 600 characters and offer `more`. These are application choices, not claims about Amazon limits.
- Initial acceptance used `ALEXA_TRIAL_MODE=true`. The 2026-10-23 owner request enables normal tools; deployment defaults are now false. The optional trial switch and guarded bulk acceptance helper remain available, and a completed bulk/latency gate must not be claimed without evidence. Model requests retain the 128 KiB, five-round and 512-token bounds. Automated tests remain within the initial $1 model allowance with no paid tool actions.
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

Reuse Device settings under About you, Persona, and Privacy through one shared Alexa target. Native Echo entries remain in device management only. Reuse the current selected-target, account-default, inheritance, reset, and version-conflict behavior. Label the target `Alexa` and explain that changes apply to every Echo's next turn.

Persona controls change Live Ninja's writing style/personality. Existing voice/accent fields may remain round-trippable in the section document for compatibility, but the Alexa summary must say Alexa controls the spoken voice. Do not advertise Voice engine, Microphone, Wake word, Turn detection, or Appearance as settings that control an Echo. Unsupported Alexa sections must not break apply-to-all for eligible web/Android devices.

Privacy controls permanent Live Ninja transcript history and memory extraction. Multi-turn operation still needs short-lived encrypted server context. Disclose its 15-minute recovery window. With transcript storage off, do not create permanent conversation history or feed exchanges into memory extraction. Explicit stop/new conversation removes transient content; expired content is rejected immediately and DynamoDB TTL performs eventual cleanup. Amazon's history is managed separately in Alexa. Include transient Alexa rows in account purge/export handling as applicable; never rely on a forgotten record prefix.

## alexa-foundation — establish the exact contracts and access prerequisites

depends on: none

### alexa-contract-tests — define the real acceptance cases before integration

depends on: none

- [x] alexa-contract-record — additive Alexa contract and internal/native header boundary documented. Existing settings schema is unchanged; its overrides already hold the selected sections.
- [x] alexa-fixture-set — request, durable replay, follow-up, expiry, cancellation, settings-change, privacy and pending-result tests pass. Provider/device acceptance remains a separate release gate.

Expected files: new `internal/alexa/` package and fixtures; existing contracts and their relevant tests.

Definition of done, from the execution root: `go test ./internal/alexa -run TestContract -count=1 -timeout=5m` exits 0 and covers both accepted and rejected envelopes. This is a future test target, not a command already run during planning.

### alexa-access-check — resolve credentials without exposing values

depends on: none; access discovery can proceed alongside alexa-contract-tests

- [x] alexa-access-record — vendor M3M80BVXTGPEZW (Jeramie LLC) and local ASK access verified. Dedicated authorization completed on 2026-10-22; the helper verified the new access token's vendor administrator role and exited 0. GitHub names-only verification found variable ALEXA_SMAPI_CLIENT_ID and secrets ALEXA_SMAPI_CLIENT_SECRET/ALEXA_SMAPI_REFRESH_TOKEN, written at 22:07:06–22:07:08 UTC. The preflight command exited 0. Interactive ASK credentials remain machine-local. Default private en-US create-once intent applies; the earlier skill listing found no Live Ninja skill, so CI must reconcile before creating it. Model access, registered redirects and device acceptance follow publishing; Jeremy confirmed readiness for the Echo test.
- [x] alexa-delivery-helper — standard-library SMAPI helper and Windows batch checker implemented. Package imports use the current package API. Create-once reconciliation, terminal failures, real endpoint validation, credential stdin handling, sanitized simulation evidence and rejection of missing physical proof pass twenty-two offline tests. Client ID entry removes pasted outer spaces/tabs; secret values remain exact. Draft preparation precedes exact-ID permission binding and package publishing. Account-link verification reads accountLinkingResponse and still rejects mismatched settings. Local delivery verification preserves its verified workflow IDs when updating evidence. Actual publishing/simulation is verified by the CI and acceptance gates below.

User-supplied CI configuration is limited to variable `ALEXA_SMAPI_CLIENT_ID` and secrets `ALEXA_SMAPI_CLIENT_SECRET` / `ALEXA_SMAPI_REFRESH_TOKEN`. The verified vendor is M3M80BVXTGPEZW. GitHub creates the first-party account-link client once and stores its secret in standard SecureString `/live-ninja/prod/alexa/account-link-secret`; applications can read only its hash and redirects in String `/live-ninja/prod/alexa/oauth-client`. Skill ID and create/import reconciliation state use the same standard SSM prefix. No GitHub variable-write token or additional manually entered account-link secret is needed. AWS deployment remains OIDC.

Use `scripts\setup-alexa-credentials.bat --authorize` to create dedicated publishing authorization with hidden local entry; normal mode fills only missing values. Its printed callback is `http://127.0.0.1:9090/cb`, and the browser start address is `http://127.0.0.1:9090/start`. Origin `https://live.jeremy.ninja`, owner login `/auth/lwa/login`, existing LWA callback `/auth/lwa/callback`, privacy `/privacy`, and OAuth `/oauth/authorize` / `/oauth/token` are verified application routes. Amazon documents that Alexa authorization-code callbacks are predetermined from the vendor, rather than returned in `redirectUrls` (that field is for app-to-app linking). The helper binds the three documented Alexa hosts to the verified M3M80BVXTGPEZW vendor. No sample vendor IDs or wildcard callbacks are used.

If the policy page is absent, the planned destination is `GET /privacy`, implemented in `internal/webapp/pages_routes.go` with `web/templates/pages/privacy.html` and the necessary `cmd/authorizer/main.go` public-route entry. Extend existing rendering/authorization tests. State the actual Amazon/backend data flow, transcript controls, transient context, and deletion path. Do not claim the new URL exists until its deployed unauthenticated GET returns 200 with the policy content.

Definition of done: `python -B -m unittest discover -s scripts -p test_alexa_skill.py` exits 0; `scripts\alexa-check.bat preflight --evidence C:\dev\live-ninja-test-evidence\alexa\preflight.json` exits 0 only when required access is verified. A missing credential returns a named failure, never an interactive unattended prompt.

## alexa-settings — add shared Alexa settings and owned Echo controls

depends on: alexa-contract-tests

### alexa-device-persistence — bind Echo entries to the existing settings store

depends on: alexa-contract-tests

- [x] alexa-device-store — trusted registration, explicit re-enable with a new generation, preserved names and owner/active-state transaction guards pass store tests. Existing code has no device-count quota to preserve.
- [x] alexa-settings-resolution — profile/persona/privacy resolve from the owner's shared Alexa target after native Echo authentication. Tool profile lookups use the same target. Browser headers cannot impersonate either target or Echo. Store, route, runtime, broker and tool regressions pass.

Expected files: `internal/store/types.go`, `devices.go`, existing store device tests, `internal/webapp/device_routes.go`, `settings_scope_routes.go`, and existing device/settings route tests.

Definition of done: `go test ./internal/store ./internal/webapp -run 'Alexa|Device|Settings|Privacy' -count=1 -timeout=5m` exits 0. Acceptance includes one shared settings choice for two independently managed Echo identities, a foreign owner denial, revoke/re-enable, reserved group protection, inherited settings, and unaffected phone/browser settings.

### alexa-settings-ui — expose only settings Alexa uses

depends on: alexa-device-persistence

- [x] alexa-website-controls — Settings offers one Alexa group for About you, Persona and Privacy. Device management still names and disables/enables individual Echoes. The updated browser cases passed workflow 40055289270 and subsequent 40055763181; unsupported sections retain apply-all behavior.
- [x] alexa-help-copy — invocation, controls, recovery and privacy copy is gated by ALEXA_ENABLED; the Help drift tests pass. Disabled deployments do not advertise the skill.

Expected files: `web/static/js/device-settings.mjs`, related existing settings/device UI files only where required, `web/templates/pages/conversation.html`, `internal/webapp/help_drawer_ui_test.go`, and `tests/web/specs/device-settings.spec.mjs`.

Definition of done: `npm --prefix tests/web test -- specs/device-settings.spec.mjs` and `go test ./internal/webapp -run TestHelpDrawer -count=1 -timeout=5m` exit 0. Test narrow/mobile layout, selected-device isolation, conflict retry, unsupported-section behavior, and actual wording. Run browser tests through GitHub CI on OFFICEPC unless the operator has selected a permitted local driver.

## alexa-chat-service — reuse Live Ninja reasoning with durable turn state

depends on: alexa-contract-tests

### alexa-linked-account — implement the scoped ASK OAuth client

depends on: alexa-contract-tests

- [x] alexa-oauth-client — static confidential client, PKCE, isolated resource/scope, refresh/revoke and owner consent pass existing and added OAuth tests. Production publishing credentials and account linking are configured; Jeremy confirmed successful linking on 2026-10-23. Do not repeat credential setup or ask for re-linking without new evidence.
- [x] alexa-worker-authority — adapter, worker and broker independently validate linked owner/device and current receipt/settings; queue bodies contain owner/receipt references only.

Expected files: `internal/mcpauth/service.go` and tests, `internal/webapp/mcp_auth.go` and tests, supporting existing auth/store code only where necessary, and new Alexa validation code. No blanket public-client or cross-resource relaxation.

Definition of done: `go test ./internal/mcpauth ./internal/webapp ./internal/alexa -run 'Alexa|MCP|OAuth|Scope|Revok' -count=1 -timeout=5m` exits 0. Wrong redirects, missing/wrong secrets, missing PKCE, scope escalation, expired grants, and revoked devices must fail without model calls.

### alexa-turn-execution — preserve follow-ups and survive slow replies

depends on: alexa-linked-account, alexa-device-persistence

- [x] alexa-shared-turn-runner — HTTP and Alexa use one five-round runner and server dependency builder. Alexa-only Luna settings, resolved persona and bounded no-retry broker calls pass tests; phone tests remain green.
- [x] alexa-turn-store — conditional session/receipt/owner-lock records and account deletion fences pass tests. Account export/purge already query the full owner partition. Usage is a separate content-free ledger; opted-in History uses existing LOG/CONV formats with configured expiry.
- [x] alexa-worker-path — SQS worker, shared registry, per-call checks and stable idempotency keys pass tests. Running receipt redelivery cannot repeat uncertain paid work. Privacy-off skips audit/RCA/automatic memory, while explicit saved notes remain supported.

Expected files: new `cmd/alexa-chat-worker/`, `internal/alexa/`, and scoped store chat files; existing `internal/webapp/api_routes.go`/fallback tests, `internal/realtime/fallback*.go`/tests, `cmd/realtime-broker/`, account lifecycle code, and shared dependency setup used by both callers.

Definition of done: `go test ./internal/alexa ./internal/store ./internal/realtime ./internal/tools ./internal/webapp ./cmd/realtime-broker ./cmd/alexa-chat-worker -count=1 -timeout=5m` exits 0. Required cases: referential follow-up, tool result in the next turn, settings changed between turns, one slow turn retrieved later, duplicate queue delivery, failed publish, worker crash, cancellation, privacy-off history, and no duplicate side effect.

## alexa-skill — connect spoken turns to the text service

depends on: alexa-contract-tests

### alexa-dialog-handler — implement the skill lifecycle and bounded responses

depends on: alexa-turn-execution

- [x] alexa-handler — exact skill/envelope checks, dialog controls, bounded receipt polling and PlainText chunks pass tests. Amazon dialog/model build is still unverified.
- [~] alexa-interaction-model — manifest, dialog model, carrier phrases, unprefixed slot samples and 50 simulation utterances are implemented and locally validated. Amazon build/routing remains unverified until CI publishing access is supplied.

Definition of done: `go test ./internal/alexa ./cmd/alexa-skill -count=1 -timeout=5m` exits 0, and `python scripts/alexa-skill.py validate-local` exits 0. Mock delays at the deadline produce an honest pending/error response before 6.5 seconds; stop/repeat/status do not create unintended paid turns. A built skill model must demonstrate that follow-ups map to the expected intent/slot.

## alexa-delivery — release through GitHub and prove the actual device experience

depends on: alexa-access-check, alexa-settings-ui, alexa-dialog-handler

### alexa-infrastructure — add only the required serverless resources

depends on: alexa-access-check, alexa-settings-ui, alexa-dialog-handler

- [x] alexa-stack — two arm64 Lambdas, encrypted SQS/DLQ, scoped roles and seven-day log groups are deployed. Initial bootstrap run 39809992183 verified queue visibility 210 seconds, redelivery cap three, and log retention seven days. Run 39867833118 subsequently bound the exact private skill ID and enabled admissions. Normal tool mode is now enabled under the 2026-10-23 owner request: worker ALEXA_TRIAL_MODE=false was read back at 4781978. No EC2 or fixed-charge monitoring resource was added.
- [x] alexa-ci-publish — run 39867833118 at b3569fcb9ea7b4242ff51e52efe5618ba67eb939 completed successfully, including the browser-quality job. Private skill amzn1.ask.skill.e05d84c2-933f-46b2-a371-9ea3925742b1, exact Lambda permission, account linking and enabled runtime are verified. Successful import ID: amzn1.ask-package.import.0b90282a-2702-4ab5-8555-ef950f2d3a26. The three-attempt publishing loop reached successful terminal completion. CI artifacts are retained under C:\dev\live-ninja-test-evidence\alexa\2026-10-22\ci-39867833118. Only a verified terminal failed creation absent from the vendor list can be replaced; unknown/in-progress creations remain blocked.

Bootstrap order: deploy the real Lambda code without an ASK invoke permission; create and verify the private development draft without an endpoint; persist the returned skill ID in deployment configuration; apply the exact-ID ASK permission through CloudFormation; import the package with the real Lambda ARN and interaction model; configure account linking; apply the completed runtime configuration and verify it through the same GitHub workflow. Amazon's CREATE_SKILL resource schema permits an omitted custom endpoint. Persist create-once identifiers in standard SSM/stack configuration when GitHub variable write permission is unavailable. Never create a wildcard invoke permission or a fake endpoint to break the dependency. Reconcile skill creation before any retry.

Definition of done in Linux CI: `make build`, `sam validate --lint`, `go vet ./...`, and `go test ./...` exit 0. `python scripts/alexa-skill.py verify-deployment --evidence artifacts/alexa-deployment.json` exits 0 only after the stack and interaction-model build reach successful terminal states and the deployed skill/function IDs match.

### alexa-acceptance — verify conversation, settings, and access on an Echo

depends on: alexa-infrastructure

- [!] alexa-simulator-gate — account linking is confirmed and the ASK enablement query now returns HTTP 204. Two bounded launch simulations returned FAILED with zero skill invocation results and Amazon's message `An unexpected error occurred.` (b03ca190-25d3-4250-b522-deee8738592a and e9753cf0-d326-4f66-9573-e53413a0d647). Amazon must restore simulator execution before the formal 50-case routing suite can be measured. Do not ask Jeremy to repeat working credential setup or account linking. The current Echo tools/settings check can proceed independently.
- [ ] alexa-latency-gate — run 100 bounded warm/cold or delayed test turns. Record total response time, model time, queue wait, final/pending/failure outcome, and token cost. All skill responses must arrive before Amazon's deadline; target normal-answer p95 under six seconds. Pending replies must not disguise a system that never answers ordinary chat promptly.
- [!] alexa-echo-gate — Jeremy supplies a real Echo observation; linking is already complete. The current retest asks for Jobs through Alexa and checks one shared Alexa settings group. Formal acceptance still needs a five-minute multi-turn observation, shared persona/profile changes on the next turn, five-/ten-/thirty-second pauses, stop, re-launch/resume, privacy-off, independent Echo revocation, and a deliberately slow result. A second Echo, if available, should share settings while keeping separate access control. Simulator or Azure phone-model evidence cannot stand in for physical Echo observations.

Definition of done from C:\dev\live-ninja-alexa: `scripts\alexa-check.bat simulate --evidence C:\dev\live-ninja-test-evidence\alexa\acceptance\simulations.json`, `scripts\alexa-check.bat latency --evidence C:\dev\live-ninja-test-evidence\alexa\acceptance\latency.json`, and `scripts\alexa-check.bat acceptance --evidence C:\dev\live-ninja-test-evidence\alexa\acceptance` exit 0. The acceptance command checks actual provider response IDs/timings, deployed commit, and the operator's Echo observations against real stored receipts. Missing physical results fail; a handwritten `passed=true` is insufficient. All model calls stay inside the recorded trial allowance.

Physical evidence file: `C:\dev\live-ninja-test-evidence\alexa\acceptance\echo-observations.json`. Record `skillId`, the website's internal Alexa `deviceId`, UTC `startedAt`/`endedAt` at least 300 seconds apart, and `observations`. Each observation has `case`, exact test `utterance`, actual `heard` response and the matching opaque turn `receipt`. Required case names are `follow-up`, `persona-profile`, `pause-five`, `pause-ten`, `pause-thirty`, `stop`, `resume`, `privacy-off`, `revocation`, `slow-result`. At least five distinct turn receipts must match the actual Echo and test window. Collect receipt references from the numeric worker logs; never copy Alexa access tokens or provider device identities. A second Echo test is optional when one is available, and its absence must be reported. Capture before expired receipt cleanup.

### alexa-release-record — finish delivery without changing the publication audience

depends on: alexa-acceptance

- [x] alexa-main-release — 478197841edb5edc1cc68506c4393358fb71463a is deployed and both Alexa functions report active/successful, with normal tools enabled. Workflow 40056173776 completed SUCCESS including browser quality. Previous main runs 40055289270 and 40055763181 also succeeded. Linking and one real Echo registration are confirmed; the separately tracked post-change physical retest remains pending. No public publication is authorized.
- [x] alexa-rollout-summary — commands, shared settings, privacy, ordinary approvals and reversible switches are documented here and in Help. Access tokens last 15 minutes and the linked grant lasts 30 days. The formal 50/100-case routing/latency suite and timed physical acceptance remain unmeasured. Normal tool mode is intentionally enabled by the later owner request; do not silently disable it to run the old setup-only bulk helper.

Definition of done: `scripts\alexa-check.bat delivery --evidence C:\dev\live-ninja-test-evidence\alexa\deployment.json` exits 0 after resolving each recorded workflow ID and running `gh run watch` with `--repo JeremyProffittOrg/live-ninja --exit-status`. The checker must match commit IDs and terminal conclusions, not trust an unrelated green run.

## Execution controls

The reversible admission switch is repository variable `ALEXA_ENABLED=false`; the next main GitHub deployment applies it. Re-enable with `true` after the recorded blocker is resolved. Do not delete devices, grants, queues or stored conversations to disable admissions. `ALEXA_TRIAL_MODE` remains `true` until all acceptance gates pass. Main pushes are the delivery path; an authorized existing Deploy workflow dispatch can re-apply changed repository configuration without a local cloud deploy. `scripts/deploy-stack.sh` rejects every other context. It is shared by the initial Lambda bootstrap and the exact-skill-ID binding step.

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

- 2026-10-22 — release-record workflow 39871545523 completed SUCCESS for c2d353e69e5f305ee7ec200208f9e200c0821eea; every applicable test, deploy and web-quality job passed. CI artifacts are retained under C:\dev\live-ninja-test-evidence\alexa\2026-10-22\ci-39871545523. `python -B scripts/alexa-skill.py delivery --evidence C:\dev\live-ninja-test-evidence\alexa\deployment.json` exited 0 with enabled=true, UPDATE_COMPLETE, matching code/package commit, import ID amzn1.ask-package.import.186243a7-e6cb-4ab5-a1d0-1b7c642d66db and preserved workflowIds=[39871545523]. There is no remaining code diff. This documentation-only checkpoint does not redeploy the verified runtime. Alexa developer enablement still returned HTTP 404 at the last read-only check; Jeremy must enable the private Live Ninja skill and link the owner account in the Alexa app before the 50-case simulation, 100-turn latency and physical Echo gates. The app-link question remains pending and no paid model trial has run.

- 2026-10-22 — run 39867833118 completed SUCCESS for b3569fcb9ea7b4242ff51e52efe5618ba67eb939. Every applicable job, including web-quality, passed. Browser installation was slow but completed without cancellation, disabling checks or a retry. `python -B scripts/alexa-skill.py verify-deployment --evidence C:\dev\live-ninja-test-evidence\alexa\2026-10-22\private-runtime-ready.json` exited 0 and reported development stage, enabled=true, UPDATE_COMPLETE and the matching code/package commit. CI deployment evidence retains workflowIds=[39867833118]. The local delivery checker had a separate evidence-record defect: it overwrote those verified IDs with an empty local CI-context list. A regression reproduced that loss; a one-line fix preserves the checked IDs. All 22 helper tests pass. `python -B scripts/alexa-skill.py delivery --evidence C:\dev\live-ninja-test-evidence\alexa\2026-10-22\verified-delivery-b3569fc.json` then exited 0 and retained workflowIds=[39867833118]. The final release-record change is a normal verification-helper delivery after the publishing loop succeeded. App linking is pending; the read-only ASK enablement-status request returned HTTP 404 for the developer test account. The private-ready HTML email was accepted by SES with MessageId 010001a165dca3e2-10918cfe-f1bb-4fe3-a2c7-cad4d4bbbe80-000000; receipt and HTML are under C:\dev\live-ninja-test-evidence\alexa\2026-10-22\mail-private-ready.

- 2026-10-22 — corrected publishing attempt 39865975036 at 2508b01b0f1be0e1fddd3c94ecb7dd741fb4de90 successfully created draft amzn1.ask.skill.e05d84c2-933f-46b2-a371-9ea3925742b1, bound its exact Lambda permission and built package import amzn1.ask-package.import.27c90b12-eec2-47b9-bebe-790c11003599. The next check returned `Account-link verification failed for type`. A filtered read-only API check showed the root key `accountLinkingResponse` and confirmed AUTH_CODE, REQUEST_BODY_CREDENTIALS, scope alexa:chat and PKCE ENABLED/S256. No client ID or secret value was displayed. The verifier now compares the fields inside that response envelope. Its regression failed with the same type error before the fix, then passed; the complete helper suite passes 21 tests and still rejects an incorrect grant type without replacing the stored client secret. Read-only checks independently returned manifestEndpointMatches=true and modelContractMatches=true. The next push is the final corrected publishing attempt in this milestone's three-attempt ceiling.

- 2026-10-22 — private publishing attempt 39862908583 at a09ac717b8d2562f54ed405e9c4cb2a0c4a2f1e5 verified CI credentials and deployed the stack, then returned `Amazon request failed: HTTP 404` from package import. Read-only SMAPI reconciliation found assigned ID amzn1.ask.skill.d2f4ad10-2902-49f2-b431-268ec34b952d absent from the vendor listing; its manifest returned `lastUpdateRequest.status=FAILED` with `The trigger setting for the Lambda arn:aws:lambda:us-east-1:759775734231:function:live-ninja-alexa-skill is invalid.` This was a failed creation record, not a usable private skill. The provider's CREATE_SKILL schema confirms no required fields in CustomApis; the downloaded schema (without its temporary download URL) is retained at C:\dev\live-ninja-test-evidence\alexa\2026-10-22\create-manifest-schema.json. The helper now prepares an endpoint-free draft, and CI binds the exact permission before package import. Regression tests reproduce the prior missing-permission submission and cover terminal versus in-progress creation recovery, application-bound drafts, and rejected versus uncertain imports. `python -B -m unittest discover -s scripts -p test_alexa_skill.py` passes 20 tests; `python -B scripts/alexa-skill.py validate-local`, workflow YAML parsing and the whitespace check pass. The next main push is corrected publishing attempt two of three. No local cloud write or wildcard permission was used.

- 2026-10-22 — publishing access resolved. After the leading-tab client ID fix, an attempt received only one character for the client secret and returned HTTP 400. Jeremy re-entered the full value locally and confirmed "ah, that worked!" The setup helper exited 0 at 22:07:08 UTC after vendor-role validation and GitHub writes. Names-only checks confirmed ALEXA_SMAPI_CLIENT_ID, ALEXA_SMAPI_CLIENT_SECRET and ALEXA_SMAPI_REFRESH_TOKEN; no values were read back. `python -B scripts/alexa-skill.py preflight --evidence C:\dev\live-ninja-test-evidence\alexa\2026-10-22\publishing-access-ready.json` exited 0, reporting local ASK access verified and CI credential names present. This milestone commit queues the first credentialed private publish; retain its workflow ID and terminal state under the same evidence root. The prior run's unit, race and Alexa helper checks and stack deployment passed; its browser checks are finishing.

- 2026-10-22 — Amazon reported `Unknown client_id` / `lwa-unknown-party-client-id`; the operator's request showed an encoded leading tab in the client ID. No credential, OAuth state or full request was retained. A synthetic regression against `read_value` reproduced the unwanted `%09` before the fix. Client ID entry now strips outer ASCII spaces/tabs before counting or URL encoding, with existing newline/NUL rejection unchanged. Secrets remain byte-for-byte unchanged and whitespace-only client IDs use the existing skip path. `python -B -m unittest discover -s scripts -p test_alexa_skill.py -k test_client_id_paste_removes_outer_whitespace` failed before the fix; `python -B -m unittest discover -s scripts -p test_alexa_skill.py` passed all 16 tests after it. The direct input evidence and deterministic regression make a broader authentication investigation unnecessary unless the fresh provider attempt fails. Stop only the recorded obsolete helper before opening the corrected batch entry point; record process and final workflow status under the existing external evidence root.

- 2026-10-22 — final tool-boundary verification: `go test ./internal/alexa ./internal/tools ./internal/webapp ./cmd/realtime-broker -count=1 -timeout=5m` passed all four packages; `python -B -m unittest discover -s scripts -p test_alexa_skill.py` passed 15 tests. `go vet ./internal/alexa ./internal/tools ./internal/webapp ./cmd/realtime-broker`, `python -B scripts/alexa-skill.py validate-local`, and `git -c core.whitespace=cr-at-eol -c core.safecrlf=false diff --check` each exited 0. A fresh names-only GitHub check still returned no Alexa variables or secrets. Final delivery uses the next main push; its workflow identifier and terminal result are retained in GitHub and the external evidence directory.

- 2026-10-22 — final bootstrap attempt 2df2b216779124d84013b95545567f34a6705821 succeeded in GitHub run 39809992183; every applicable job, including web-quality, completed SUCCESS. CloudFormation returned UPDATE_COMPLETE. Read-only Lambda verification returned Active/Successful, arm64, matching commit, ALEXA_ENABLED=false and empty skill ID for both functions. Adapter timeout is 7 seconds; worker timeout is 35 seconds with its existing 30-second turn deadline. Both explicit log groups have retention 7; the queue reports SqsManagedSseEnabled=true, VisibilityTimeout=210 and maxReceiveCount=3. The three-attempt infrastructure retry loop reached a verified successful terminal result.
- 2026-10-22 — final shared-tool checks found the existing web-search broker rejected the new Alexa surface. The Alexa caller now forwards its admitted receipt and the broker verifies it and inherits the deadline before a search; invalid/cancelled receipts cannot spend. Red/green route tests cover this. Alexa Ghost proposals also require a separate authenticated spoken confirmation turn, with conversation/settings binding; a model cannot confirm its own newly issued proposal in the same tool loop. Existing phone/web behavior remains unchanged. Focused Alexa/Ghost/search tests pass; a final focused main push follows.
- 2026-10-22 — Amazon authorization blocker email accepted by SES: MessageId 010001a16479e0ad-eb9b347a-7e2e-439c-b284-600e684694e9-000000. Delegated sender verified the successful bootstrap run, used the HTML blocker template, and retained HTML/receipt under C:\dev\live-ninja-test-evidence\alexa\2026-10-22\mail-amazon-blocker. The email explicitly separates the Amazon page failure from the local helper fix. No paid provider/physical acceptance has run. The 50-case fixture now includes fresh “yes” capture and resume into a new Alexa session; critical controls must all route correctly even when overall routing exceeds 95%.

- 2026-10-22 — corrected run 39808254940 reported `aws sts get-caller-identity; exit 252; CLI_INPUT_JSON`. AWS CLI rejected its pipe-backed `--cli-input-json` file before making the account check. Public request JSON now uses a normal structured argument; SSM secret values use only the scalar `--value file:///dev/stdin` input. New regression tests check both transports and prove secret values stay out of arguments. No check was bypassed. Initial attempt and first corrected attempt failed; the next push is the final corrected attempt in this milestone's three-attempt budget.
- 2026-10-22 — checked the bootstrap's empty skill parameter with the installed SAM parser before the final retry: `AlexaSkillId=` was rejected; literal `AlexaSkillId=""` parsed as `{'AlexaSkillId': ''}`. The shared deploy script now supplies the accepted empty-string form. `python -B -m unittest discover -s scripts -p test_alexa_skill.py` passes 15 cases; `bash -n scripts/deploy-stack.sh` and SAM template lint pass.

- 2026-10-22 — infrastructure commit 6d9cc5271a9ca7eeb6f170ec29054ad74b8b9b4d passed GitHub tests and local SAM validation. Deploy run 39805596228 failed before `sam deploy` in the bootstrap configuration read. Bash command substitution hid the helper's sanitized error. The first corrected attempt makes that diagnostic visible and preserves provider payload redaction; it does not bypass the failing check. No new Alexa resources were deployed by that run. Retry budget: initial attempt failed; two corrected attempts remain for this delivery milestone.

- 2026-10-22 — worker/settings checkpoint b8f99846eedc05b27ce873e647499dd510b265fb deployed through GitHub run 39797222669. Every applicable job, including test, deploy and web-quality, concluded SUCCESS; the watcher exited 0. The next infrastructure checkpoint adds the real skill adapter/worker resources, with admissions disabled until the private skill and account linking exist.
- 2026-10-22 — `aws sts get-caller-identity --query Account --output text` returned 759775734231. Reconciled CloudWatch Logs retention across all 17 active regions: 173 groups in us-east-1, 17 in us-east-2, none elsewhere; all 190 already had 7-day retention, so no update was needed. Evidence: C:\dev\live-ninja-test-evidence\alexa\2026-10-22\log-retention.json.
- 2026-10-22 — `sam validate --lint --template-file template.yaml` passed for the two arm64 Lambdas, encrypted SQS/DLQ, exact-ID invoke permission and seven-day log groups. Alexa tool audits now use the Echo retention window. Ghost proposal tests prove a changed Alexa conversation/settings context cannot consume an older proposal; other surfaces retain their existing binding. Initial acceptance denies tool actions and uses numeric cost checks; no paid acceptance call has run.

- 2026-10-22 — foundation commit 689a8daa8b5c7d4821388f8ff7c8e5ce258ca13f deployed through GitHub run 39782522746, conclusion SUCCESS. Unauthenticated GET https://live.jeremy.ninja/privacy returned HTTP 200 with expected policy text.
- 2026-10-22 — durable-worker checkpoint: `go vet ./...` exited 0. `go test ./... -count=1 -timeout=5m` passed every package except RCA fixture comparisons affected by this Windows checkout's CRLF conversion; restoring the unchanged fixture/system-map bytes to repository LF and rerunning `go test ./internal/rca -count=1 -timeout=5m` passed. No RCA source or fixture change is included. Focused Alexa/Help tests pass in store, webapp and worker; four offline Python credential-helper tests pass. Browser checks will run in GitHub CI. No model/provider acceptance request has been made.
- 2026-10-22 — user reported authorization failure. The running local helper's root URL reproduced HTTP 400 with “Authorization failed”; /start returned HTTP 302. The helper now redirects root to /start and reports only sanitized provider error codes. Its regression test failed before the fix and passed afterward. The old ten-minute authorization process ended; no ALEXA GitHub names appeared. Jeremy was given the corrected batch restart command. The exact Amazon error, if any, remains unverified; this local fix is not proof of completed authorization.

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
