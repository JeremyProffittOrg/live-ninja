# Plan

## inbound-answer-cap — stop canceling incoming calls before the PIN prompt
Outcome: recognize the existing configured owner caller numbers without a PIN, answer unknown callers and hang up immediately, and establish the provider duration cap only after the recognized caller answers the recording-consent prompt. Non-goals: change recording consent, caller allowlist, budgets, call duration, model, voice, routing, or add a background process. Files: tools/phone-trial/inbound_owner.py and test_inbound_owner.py; recorder.py dispatch; obsolete scripts/enroll-phone-pin.py and .bat; activate_inbound.py and its existing tests; phonebudget.mjs and its existing tests; Help copy, contracts/api.md, deploy.md and plan.md. Stored private PIN data is left untouched and is no longer used for admission.

### inbound-cap-order — enforce the cap only after answer
- [x] caller-number-admission — remove PIN authentication and obsolete enrollment tools; preserve the existing two configured owner callers, signed provider callbacks, active-owner checks and recording consent. Proof: existing tests admit an allowlisted caller without PIN configuration, unknown callers get exactly Hangup with no reservation or model access, and obsolete PIN callbacks cannot mint a session.
- [x] inbound-cap-regression — emulate the provider rejecting a time-limit update while the incoming call is ringing. Proof: existing Python tests reproduce the old premature goodbye, then pass with an immediate recording-consent prompt, bounded elapsed deadline and fail-closed provider rejection after answer.
- [~] inbound-cap-release — depends on: inbound-cap-regression. Proof: `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` and `go test ./internal/webapp -run TestHelpDrawer -count=1` exit0; focused main commit/push and all triggered workflows complete success. Actual owner speech remains a separate unverified result until a real call.
Supervision: 20-minute local test deadline,60-minute workflow deadline; three classified attempts per job. No outgoing test call, private PIN access, config change, quota reset or scheduler. Existing stop conditions apply. The user-approved PIN removal supersedes the initial PIN-path draft before it was committed. Immutable sanitized evidence will be C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15\inbound-answer-cap-2026-10-18-final.json.

Verified facts: latest owner call CA11c59a6299fad98d9580d688780d5fdd was admitted at1792337174, then ended in the same second with endReason provider_cap_failed, providerStatus canceled and pinAttempts0. Existing code applies TimeLimit during the initial incoming webhook, before returning answer TwiML. Twilio documents canceled as cancellation while ringing and realtime operations as requiring in-progress state: https://www.twilio.com/docs/voice/api/call-resource and https://www.twilio.com/docs/api/errors/21220. The swallowed provider error code itself was not retained; state/order evidence supports the fix, not a claimed observed21220 code. Read-only workflow39140531066 verifies routing/allowlist/enabled/PIN, no active calls and no Debugger errors; its dated manual-budget readiness check fails as expected and does not evaluate the new recurring policy.

Current task 2026-10-18: incoming-daily-budget is deployed and active for the verified primary caller. Remaining allowance10USD verified; midnight America/New_York reset. Live caller speech is still unverified. The October17 manual grant expired; deployed owner_budget_status confirms dailyGrantedUSD0, dailyRemainingUSD0, allocatedUSD20 and waiting_approval. The user now authorizes a recurring incoming-only $10 daily allowance for the exact private primary caller. The dated delivery status below is historical; it does not override this standing policy.

Status: PARTIAL DELIVERY 2026-10-17. One requested owner-message audio call completed with recordingready; it used Twilio TTS, not Azure. Incoming number routing is now corrected from the old/unavailable path, owner/PIN calling is enabled, and the original routing repro passes. Azuredeploymentgpt-realtime-2-1-mini is actualmodelgpt-realtime-2.1-mini/version2026-07-07. The user has been asked to retry an incoming call; actual owner Azureconversation/quality acceptance remains unverified until that call. Today's10USD extra allowance remains subject to local-midnight expiry. Car testing is deferred because the S9 is not in the car. Dialer/takeover, real interviews and provider-copy retention remain open; Android Auto stays in backlog.md. Final routing/release evidence: C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15\inbound-live-route-2026-10-17-final.json.

Single active source: repository-root plan.md; current implementation checkout C:\dev\live-ninja-plan-build\plan.md. Completed history: C:\dev\live-ninja\completed\plan-2026-10-15-history.md and earlier completed/ archives. Deferred work: C:\dev\live-ninja\backlog.md. The current plan covers telephony, Jobs, S3 recordings, car Bluetooth, and their acceptance tests. All Android Auto work is in backlog.md.

## Locked decisions (user-confirmed; do not revisit)

- 2026-10-18 — user: "Incoming calls should not have a pin, they should just answer based on my phnone number,, if it's not me, then answer and hang up". Remove PIN prompts and authentication from incoming owner calls. Preserve the existing configured owner-number allowlist and automatic signed caller-number check. Unknown callers are answered and hung up without prompts or model access. Recording consent and the recurring incoming $10/day budget stay in force; this supersedes earlier PIN requirements, not the approved caller list or recording policy.

- 2026-10-18 — user requests a standing $10/day budget for incoming calls from the supplied private number, verified as the existing primary alias. Exact caller stays in private runtime configuration, never this repository. This supersedes one-time daily renewal only for that caller's incoming calls. Reset at midnight America/New_York; preserve PIN, recording consent, duration limits, stop controls, monthly reporting and independent outgoing/hotel approval. No new test call is required to activate this allowance.

## incoming-daily-budget — automatic allowance for the designated incoming caller

Outcome: admit the designated incoming caller with a separate $10 daily reservation cap even when the general monthly allowance is exhausted. Non-goals: change outgoing/hotel grants, caller allowlist, PIN, model, voice, car testing or routing. Files: tools/phone-trial/owner_messages.py, inbound_owner.py and their existing tests; web/static/js/phonebudget.mjs, tests/web/unit/phonebudget.test.mjs, web/templates/pages/conversation.html, plan.md.

### incoming-budget-delivery — test and deploy the isolated daily allowance
- [x] incoming-budget-reservation — atomic caller-bound daily reservation with local midnight reset, no outgoing credit, and existing stop/deletion guards. Proof: `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` exits 0, including exact-caller isolation, exhausted quota and date rollover.
- [x] incoming-budget-display — separate incoming allowance from manual outgoing approval. Proof: `node --test tests/web/unit/phonebudget.test.mjs` and `go test ./internal/webapp -run TestHelpDrawer -count=1` exit 0.
- [x] incoming-budget-release — depends on: incoming-budget-reservation, incoming-budget-display. Proof: focused commit pushed to main; every resulting delivery run reaches success via `gh run watch <run-id> --repo JeremyProffittOrg/live-ninja --exit-status`.
- [x] incoming-budget-policy — depends on: incoming-budget-release. Install only the private primary-caller policy, using conditional owner/PIN/allowlist checks. Proof: deployed owner_budget_status reports incoming limit10, reserved0, remaining10; general daily grant stays0. No call initiated. Actual caller speech remains unverified until the owner calls.

Supervision: existing 20-minute test and 60-minute workflow deadlines; three total classified attempts, deterministic failures require a fix before retry. No local recurring process. Existing stop conditions apply; the October17 telephone hold is superseded by later explicit call authorization and this standing incoming allowance; physical car tests remain deferred. Private configuration install uses one conditional transaction; an ownership or policy conflict requires re-reading before any retry.

## Earlier locked decisions

- 2026-10-17 — user: "when I call live ninja, I get we are sorry and application error has occured.  I shoudl be connected to a live chat-gpt live model, the from azure". Fix the incoming phone-number path to the existing authenticated Azure Realtime owner conversation; private PIN and recording consent remain. This authorizes the required exact owned-number routing/configuration activation within the current10USD allowance. Physical car testing remains deferred.
- 2026-10-17 — user: "the audio quality is also quite poor, how do we improve it". Explain and diagnose quality on the correct Azure path before changing model/voice/codecs. Stop the second prerecorded outbound test; the first owner-message test used Twilio default TTS and cannot prove Azure conversation quality.

- 2026-10-17 — user: "ok, make the calls" after the status answer explicitly described zero owner test-call requests and disabled calling. Resume controlled calls to the existing primary owner alias now, within the recorded10USD daily allowance. This supersedes the telephone-test hold; physical car testing remains deferred because the S9 is not in the car. Start with one existing owner-message audio test (90-second provider cap,0.30USD reservation); verify terminal outcome before any second distinct test. Maximum two planned owner-message tests in this run; no automatic redial on no-answer/failure/unknown outcome, no hotel contacts and no inbound routing activation implied.

- 2026-10-17 — user: "resume, granted $10 budget". This supersedes the earlier stop for controlled owner acceptance tests. Set today's extra allowance to10USD total in America/New_York (current grant/spend0 verified); expiry2026-10-18T00:00:00-04:00, no automatic renewal. Preserve monthly accounting, one active call, existing duration limits and no automatic redial. Do not replay the expired October16 rehearsal or start a hotel batch from this instruction.
- 2026-10-17 — user: "parked car testing"; readiness answer: "Yes—ready now". Prioritize the parked Suburban/S9 native Bluetooth baseline, then controlled owner telephone audio if needed. Verify car microphone and speaker separately; a successful provider connection or software test is not physical-audio proof. Android Auto stays in backlog.
- 2026-10-17 — user: "I will check later" in response to the native parked audio test. Physical testing is deferred. Stop the one-time route sampler; do not place a test call or activate routing while readiness is deferred. The recorded10USD allowance still expires at local midnight; this is not authorization to carry it into another day.
- 2026-10-17 — user: "The s9 isn't in the car, we'll test that later". This explains the observed disconnected HFP/A2DP state; it is expected and is not evidence of a Bluetooth defect. Do not restart hardware monitoring until the S9 is actually in the parked car and the owner is ready.

- 2026-10-17 — user: "pin set". Owner reports private PIN enrollment complete; verify only enrollment status, never the PIN or verifier contents.
- 2026-10-17 — user: "agressively go after this". Resume blocker removal with multiple agents. Fix the Android lint blocker and verify dependency-ready phone work. The existing explicit stop-calls and retention decisions still require separate answers.

Historical quotes below remain unchanged. Later dated decisions supersede earlier planning holds and Android Auto scope; only the latest applicable decision governs execution.

- 2026-10-17 — user: "use multiple agents to build and test them". Implement and test the remaining dependency-ready plan work with multiple agents; preserve stopped calls and existing spending limits.
- 2026-10-17 — user: "put anything for android auto in the backlog". All Android Auto implementation, projection, DHU tests, Play distribution and Auto-specific access requirements move to backlog.md. Bluetooth and native-phone work remain active. This supersedes the Auto portions of earlier locked decisions without changing their historical quotes.
- 2026-10-17 — user: "Start a new conversation". Inbound owner calls start a fresh conversation under the existing owner account. The session-choice question is resolved; private PIN enrollment and authenticated server integration remain required. This does not authorize calls or change number routing.

- 2026-10-16 — user: "Using multiple agents, implement plan.md". This supersedes the historical planning-only hold for implementation and deployment. It does not lift the later call stop.
- 2026-10-16 — user: "stop calls". Keep CONFIG/OWNER_MESSAGES enabled=false and PHONE#CONTROL/VOICE activeRequest=owner-stop-2026-10-16. No outbound test, hotel, rehearsal, or paid call smoke until explicit new call authorization.

- 2026-10-16 — user: "Then let's call 10 hotels in ca asking for the vpn configuration". Exactly ten distinct verified California hotels (hotel-02 through hotel-11), immediate single-call concurrency; guest Wi-Fi login requirements and reported VPN restrictions only, no private router settings or credentials. Existing monthly $20 and explicit $5 daily permission rules remain. No automatic redial.
- 2026-10-16 — user selected "Claude/ChatGPT remote MCP with browser sign-in" and "Only my configured cell numbers". MCP exposes owner phone messages, existing Jobs and memory. General background AI workers were asked about, not authorized as an implementation expansion. Existing caller values remain private runtime configuration.


- 2026-10-16 — user: "yes" to adding menu navigation before consent, announced keypad choices, front-desk/customer-service requests, and limits on transfers and waiting.
- 2026-10-16 — inbound owner calling authorized for exactly two owner-supplied numbers. Exact user wording and E.164 allowlist are stored outside the public repository at C:\dev\live-ninja-test-evidence\inbound-owner-2026-10-16\locked-decisions.txt. Existing owner/PIN requirement remains. Session choice resolved on 2026-10-17: start a new owner conversation; private PIN setup remains pending. Login with Amazon refers to the existing internal owner account; no inbound routing change yet.


1. **2026-09-22 — bypass spoken smokes.** Do not run F1 or the E4 spoken turn. Do not wait for a microphone. Quote: "let's bypass the smoke tests and make the plan about pushing everything out."
2. **2026-09-22 — push the rest.** A push to `main` is the production deploy. Never deploy from a laptop.
3. **2026-09-22 — `emb-retire` runs now.** The operator said "perform the emb-retire early." The 2026-09-28 date is lifted. `ENT#` rows stay.
4. **Do not flip `SigningDisabled` on authorizer `live-ninja-iot`.** AWS IoT does not allow that update. Tab5 stays on that authorizer. Signing for web and Android is a new authorizer.
5. **G1–G6 stay in the backlog.** Do not pull them in.
6. **2026-09-20 — wake training is off.** Bundled phrases only. Default `hey-jarvis`. Do not resume custom wake training.
7. **No agent handles secret values.** Names only. `scripts/set-secret.sh` is the path for a new secret.

8. **2026-10-15 — phone provider direction.** Verbatim: "okay, let's build what we have to in twillio and use azure where we can for the backend"
9. **2026-10-15 — Azure credit preference.** Verbatim: "i want this mostly in azure because I have credits there"
10. **2026-10-15 — planning only.** Verbatim: "create a plan for that, we'lll be adding a few more things, create the plan, do not start" No implementation or deployment starts from this draft.

11. **2026-10-15 — expanded calling feasibility and plan scope.** Verbatim: "okay, I would also like to see if we can hook up the audio in live ninja directly to twillio to allow live-ninja to allow me to make calls from inside the application, and allow live ninja to listen in on that call, or take over the call - or make calls it's self" This adds planning scope; the do-not-start instruction remains.

12. **2026-10-15 — hotel research job and credential-helper exception.** Verbatim: "Finally, I'd like live ninja to be able to run a job, where it interacts with someone, the test would be to call 5 different hotels and discuss with them if their internet connection requires anything other than a password or simple accept type button, unfortunately, my work vpn is rather picky on what we can connect to.;  create an msdos batch file to ask me for any credentials including for twillio you'll need, the script should ask me for the credential, if something is entered, tell me the length and last 4 characters and put it into the github secret." Add the job to the draft; create and test only the local credential helper now. No calls or deployments are started.

13. **2026-10-15 — Bluetooth repair, Android Auto, and extensive call tests.** Verbatim: "Also, when I use live=ninja in my car, it does now alloe me to use my car's hands free over bluetooth, research and add that fix, and add update the live-ninja app to also be an android auto application.  Add extensive testing include test calls." This adds research and planned implementation/testing; the start hold remains.
14. **2026-10-15 — USB test access.** Verbatim: "make sure you can see the samsugn s9 on the computer's usb port and can control it for testing" Basic USB/device control verification is authorized now; this is not authorization to start the planned calling rollout.
15. **2026-10-15 — credential setup reported complete.** Verbatim: "I ran the script and set the secrets" Treat this as operator-reported; verify names/access before execution, never request configured credentials again without a concrete missing-value result.

16. **2026-10-15 — phone logs and S3 recordings.** Verbatim: "all phone interactions should be logged and I should be able to download and listen to the recording in the live-ninja application, save in s3" This replaces all earlier no-recording proposals; no retention duration was specified.
17. **2026-10-15 — planning standard.** Verbatim: "make sure the plan is built in acccordance wiht the claude code rule for planning" Apply the global Claude planning conventions and preserve the no-start instruction.


18. **2026-10-15 — hotel selection.** Verbatim: "random hotels inc alifornia". Select five random distinct California properties and verify their official telephone numbers before execution; another destination-choice question is not needed. No extra VPN restrictions were supplied.
19. **2026-10-15 — target vehicle.** Verbatim: "chevy suburban 2027 stock head unit". Target the 2027 Chevrolet Suburban stock head unit; test window, symptom surface, newer-phone access and Play access remain unconfirmed.
20. **2026-10-15 — spending control.** Verbatim: "after $20 per month, don't make calls without permission for a $5 per day budget, if it goes over $5 for that day, re-ask for new permissions". Implement call-budget as the admission rule; do not spend past a grant while waiting for another approval.
21. **2026-10-15 — disclosure/default response.** Verbatim: "no discolosure, this is fine". Historical response, superseded for disclosure by locked decision 22. Interpret the remaining proposed defaults as accepted: owner-only caller number plus PIN, explicit approved tasks, and 180-day S3 retention. The later friendly-introduction answer supersedes the no-disclosure preference; web/Android release cadence was not specified.

22. **2026-10-15 — friendly AI introduction and recording request.** Verbatim: "create a fun introduction, I'm Jeremy's awsome AI assistant helping him while he's planning his world domination tour and we need your help, and by the way, I'm recording this for him as well."  or soemthing like that, fun and dale carnige in tone and ask" This supersedes the earlier no-disclosure response. Use a short friendly introduction and ask for recording permission; capture conversation audio after affirmative permission.

23. **2026-10-15 — execute today's trial and email results.** Verbatim: "make 20 test calls today using that, and email me the results, along with temporary s3 links to the recordings (7 day ttl)" This authorizes only the bounded 20-call trial and its required backend/recording/email deployment now. Broader app/Android Auto implementation remains on hold.

## Verified facts

Phone planning evidence read on 2026-10-15 (the older facts below describe earlier work):
- C:\dev\live-ninja\.github/workflows/deploy.yml triggers deployment on pushes to main. This draft stays local to avoid starting delivery.
- C:\dev\live-ninja\go.mod uses Go 1.25 and already includes github.com/gorilla/websocket.
- C:\dev\live-ninja\cmd/realtime-broker/main.go contains Azure handling and client capability gates; phone support requires an explicit integration.
- C:\dev\live-ninja\internal/realtime/rules.go builds the account rule index; voiceprefs.go resolves persona preferences.
- C:\dev\live-ninja\internal/tools/registry.go binds execution to verified account/session context and applies authorization and idempotency.
- C:\dev\live-ninja\internal/realtime/surface_tools_test.go checks that tools match each supported surface.
- Twilio documents bidirectional Media Streams: https://www.twilio.com/docs/voice/media-streams . Wire messages, 8 kHz mu-law, playback marks, and queue clearing: https://www.twilio.com/docs/voice/media-streams/websocket-messages .
- Azure Container Apps supports WebSockets: https://learn.microsoft.com/en-us/azure/container-apps/ingress-overview . Account eligibility and actual call-duration behavior are not yet verified.
- Pricing research on 2026-10-15 found US local rental $1.15/month, inbound $0.0085/minute, and Media Streams $0.0044/minute: https://www.twilio.com/en-us/voice/pricing/us . Telephone estimate: $1.15 + $0.0129 per minute, or $5.02 at 300 minutes and $14.05 at 1,000 minutes. Excludes taxes, model usage, and hosting. Recheck before purchase. Azure credit coverage and complete monthly cost remain unverified.


- Current USB evidence (2026-10-15): serial 4633424442303098, SM-G965U, Android 10/API 29, app 0.3.15/code 21. Windows USB status OK; wake and MainActivity control passed. Full evidence and current Android Auto requirements: C:\dev\live-ninja\docs\android-auto-research.md. Physical car audio not tested.
- C:\dev\live-ninja\template.yaml DeliverablesBucket blocks public access, uses AES256 encryption, and expires objects after 180 days. Existing deliv downloads are attachment redirects; call playback needs a scoped inline/range implementation.
- Rule sources verified: C:\Users\Jeremy\.claude\CLAUDE.md Planning and Claude Appendix; C:\dev\live-ninja\CLAUDE.md; C:\dev\live-ninja\deploy.md. Native Claude plan/task tools are unavailable in this runtime.


## Execution contract

The user authorized implementation using multiple agents. On2026-10-17, controlled owner-call resumption and10USD daily authority were granted, then physical testing was deferred. Continue independent code/readiness work; retain disabled calling until the parked test is ready and routing is verified. Historical dated hotel trials are completed history, not permission to call those hotels again.

Rule sources read: C:\Users\Jeremy\.claude\CLAUDE.md, C:\dev\live-ninja\CLAUDE.md, and C:\dev\live-ninja\deploy.md. Apply the planning conventions here. This runtime has no EnterPlanMode, ExitPlanMode, TaskCreate, or TaskUpdate tools; do not claim those were used. The task mirror below is the runtime-neutral task list; a future Claude run must create its native tasks with the same milestone names and blocked-by relationships. Update plan status and native task status together.

Working directory for this run: C:\dev\live-ninja-plan-build (clean main checkout). Android commands run in C:\dev\live-ninja-plan-build\android. Preserve unrelated changes in C:\dev\live-ninja. User-run Windows entrypoints are real .bat files. Short processes launch hidden across the full process chain. Do not print secrets or open/control the computer desktop without permission. All ADB calls select a device serial explicitly.

Authority: deployments, deployment retries, normal deployment cost, and required restarts have standing authorization dated 2026-08-16; do not ask for deployment approval again. The 2026-10-16 implementation instruction lifts the historical no-start hold; the separate call stop remains effective. Number acquisition and test calls must fit the locked spending rule; recording retention is 180 days. Any expanded autonomy must be resolved before affected execution; Android Auto distribution is deferred. No unrelated deletion, OS flashing, new hardware purchase, new local schedule, or third-party contact outside the approved list is authorized.

Verification contract: every active milestone below has an exact gate command, plus its focused tests. Planned scripts are deliverables of phone-verification-tools, not existing programs or claimed results. The gate reads evidence from C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15, outside the repo. It must reject missing evidence, wrong commit/account/device, failed tests, unverified audio claims, unresolved required decisions, and nonterminal workflows. Never implement it as a check of a hand-written passed=true flag. For live checks, reconcile source artifacts/provider IDs with recorded observations; human audio confirmation is explicit evidence and cannot be inferred from metadata.

Use append-only attempt directories under that evidence root; never overwrite prior attempts. No credentials, private keys, presigned URLs, or raw PINs enter evidence or commits. Production recordings are stored only through the protected S3 path. Write the deployed SHA, actual job/run/CallSid identifiers, result, exit code, and evidence path into Execution log as work lands.

Dynamic workflow IDs are resolved from recorded deployment metadata. Exact PowerShell watcher command after the gate has validated that metadata:
  $delivery = Get-Content -Raw 'C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15\deployment.json' | ConvertFrom-Json
  foreach ($runId in $delivery.runIds) { gh run watch $runId --repo JeremyProffittOrg/live-ninja --exit-status; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
The verifier must establish each run's repository, workflow, commit SHA, status and conclusion via gh, not accept an unrelated successful run ID.

Definition-of-done rule: all commands named by a milestone must exit 0 and all required evidence must exist. Missing hardware, failed provider calls, partial hotel answers, and unsupported native-phone modes remain explicit statuses; they cannot be converted to green by omitting cases. A partial hotel report can be delivered while the unmet five-interview target remains open.

## Open decisions (asked together on 2026-10-15)

- 2026-10-17 — provider-copy-retention: S3 retention is locked at180 days; the separate Twilio audio-copy policy is not locked. Asked owner whether to delete Twilio audio after verified S3 persistence or retain it180 days too. No answer is authorization; no normal provider-copy deletion or schedule is enabled while this is pending. Account deletion remains a separate explicitly requested privacy action.

One asynchronous question batch was answered on 2026-10-15. The verbatim answers are in Locked decisions. The recording follow-up was also answered: use a friendly AI introduction and ask to record. Remaining resource/access inputs are listed below; they do not block completing the draft. Do not ask another permission question for deployment.

- hotel-target-input — resolved: select five random California hotels. Verify official property numbers and freeze the selected list before dialing. No additional VPN constraints supplied; do not claim verified compatibility.
- vehicle-test-input — resolved model: 2027 Chevrolet Suburban, stock head unit. Remaining: native app/browser and affected directions, parked test window and a newer phone for modern Bluetooth route checks. Auto-specific phone and Play access are deferred to backlog.md. Discover available resources before asking for anything missing.
- budget-and-window-input — resolved spending rule: $20 monthly threshold, then explicit $5 daily grants renewed only by owner permission. Test calls share this ledger. No deadline or area preference was supplied: no fixed completion deadline; use an available US local number within budget. Proposed call-duration/concurrency defaults remain 30 minutes normal, 10 minutes hotel, one conversation, no redial.
- access-and-retention-input — owner response accepted the defaults except disclosure: owner-only plus PIN, explicitly approved tasks, and 180-day S3 retention. The follow-up explicitly requests a friendly AI introduction and a recording request. Wait for affirmative permission before recording; if refused, log the refusal without audio and end politely. Public product release timing remains unspecified; prefer independent verified releases within approved distribution scope.

Discover exact account/resource identities, existing credentials by names only, model quota, eligible credit offer, official hotel numbers, installed artifact SHA and driver/tool availability from the environment before asking the operator to resolve ambiguity. A missing SDK tool is an implementation/preflight dependency, not an operator question by default.

When answers arrive, copy each answer verbatim with the date into Locked decisions and remove only its unresolved marker. Before unattended execution, record any remaining details from the scope references in the same locked batch; do not wait until a call is running to ask about budget, voicemail, disclosure, destination limits, or stop conditions.

## Assumptions and proposed defaults

- The reported car symptom means "does not allow" hands-free audio; exact failure awaits reproduction.
- Twilio carries telephone traffic; Azure handles the new voice/backend work where practical; AWS retains existing Live Ninja data and the requested S3 recordings.
- Owner-only access and explicit task-scoped outbound authority are accepted. One concurrent conversation and no automatic redial remain conservative operational defaults.
- Accepted 180-day retention matches the current private deliverables S3 lifecycle; prefer reuse after confirming audio ingestion/ownership requirements.
- Supported newer-phone availability for Bluetooth tests and seamless bot-to-conference transport are unverified. They are explicit gates, not promised facts.
- Do not interpret caller speech or hotel answers as permission to change task scope, expose owner data, or make further calls.

## Job supervision and restart policy

Each workstream declares its job classes below and inherits this policy. Before launch, append a job registry entry to Execution log with milestone name, exact command, PID/session or GitHub run ID, start time, timeout, attempt number, watcher, and expected exit condition. Do not leave untracked jobs running.

- local-test-job — run the milestone's exact Go, Node, PowerShell, or Gradle command in a hidden child process. Watch exit code and captured bounded output for success and Error/FAILED/Killed/OOM/timeout. Unit-test ceiling 20 minutes; build/instrumentation ceiling 45 minutes. Restart command is the same recorded command after a classified fix; at most three total attempts per milestone, not three per wrapper.
- delivery-job — push focused commits on main only after explicit start. Track all triggered runs. Watch using the exact PowerShell command in Execution contract. Deadline 60 minutes per run. On deterministic failure, fix then commit/push; on verified transient failure, "gh run rerun $runId --failed --repo JeremyProffittOrg/live-ninja" uses the captured ID. One shared ceiling of three attempts per milestone; 30-second then 60-second backoff. Never disable a failing check.
- topology-proof-job — at most two experiments, each bounded by locked call duration/spend. Record the changed approach before retry. End all test call legs on timeout.
- controlled-call-job — exact scenario command is "scripts\verify-phone-evidence.bat run-case CASE-NAME", where CASE-NAME is one of the fixed scenario names in the milestone's evidence requirements, not an arbitrary dial target. This command must refuse to place calls without a locked destination/budget file and explicit execution-enabled flag. Three attempts per scenario maximum; observed failure, timeout, stop, or budget cap ends the call and preserves evidence. Never use hotels as debugging targets.
- hotel-call-job — one attempt per approved hotel by default, one active call. Durable job/run/property IDs and provider status are watched; unknown call-creation outcomes are reconciled before retry. Remaining approved hotels continue after one unavailable contact. An amended retry ceiling must be locked before the job starts.
- recording-transfer-job — durable callback/queue state; three total transfer attempts including queue redelivery, with 30/60-second delays. Watch ready/absent/failed and the transfer deadline. Retry the same RecordingSid through the recorded worker retry entrypoint; never redial the telephone call. Capture that deployed worker entrypoint in deployment.json so the verifier can invoke it without guessed resource names.
- human-hardware-job — parked-car test with a named available operator and device. Exact case, duration, observed evidence, and timeout are registered before launch. No response means unverified, not passed. Do independent work while waiting.
- research-agent-job — existing android_car_research completed. Any later delegation must have a unique name and nonoverlapping scope, include hidden-process rules, and return evidence checked by the parent. No new agents or emails are scheduled by this plan.

At a ceiling, mark the affected item [!] with exact provider/run/hardware dependency and who can supply it; continue independent authorized work. No silent retries, no overlapping budgets, no local Task Scheduler/cron/timer/service creation. Stop the entire run only for the conditions below. Product-level spend waits requested by the owner pause new calls while retaining results and recordings.

## Stop conditions (only these)

- operator-hold — implementation is authorized; parked testing was deferred by "I will check later" after controlled-call authorization. No test calls or routing activation while test readiness is deferred. Continue code, offline tests, read-only checks and deployment.
- unresolved-run-input — an essential owner-only decision, required credential/resource, parked-car/newer-phone access is absent. Mark the dependent work [!] and continue unrelated authorized work.
- budget-or-scope-boundary — a purchase/contact/action exceeds locked limits or requires materially different infrastructure or audience.
- destructive-boundary — user data deletion, number retirement, OS flashing, resetting pairing, discarding uncommitted work, or history rewriting is required without explicit authorization. Approved future retention rules apply only to the objects covered by that rule.
- identity-boundary — an action would broaden the federated identity beyond the verified repository/ref or expose credentials.
- platform-boundary — Provider capabilities prohibit the planned feature; do not bypass provider restrictions.
- preserved-system-boundary — do not change SigningDisabled on live-ninja-iot, delete ENT# data, resume custom wake training, or pull unrelated backlog work into this run.
- retry-ceiling — the shared retry ceiling is exhausted; defer the affected work with actionable evidence, without stopping independent work.

## Task mirror and execution order

This table of named tasks is the task-system fallback for the current runtime. It is generated from the milestone dependency lines below. [ ] means not started; the credential-helper and USB checks have separate completed evidence. Do not mark a milestone done from a child's report without its gate passing. Before resuming, read this section and Execution log, then compare the working tree; preserve unrelated changes.

Execution sequence: resolve the front-loaded batch and verification tools first. Start provider checks and car reproduction access early. Then run independent Bluetooth repair, Azure/call foundation, and media topology work where their actual dependencies permit. Recording ingestion is required before final calling acceptance. Final release verification follows the native-phone and Bluetooth tests. Android Auto is excluded from the active dependency graph.

- [~] phone-verification-tools — offline checks and read-only provider/device reconciliation implemented; full run manifest remains pending; gate: powershell.exe -NoProfile -NonInteractive -File scripts/verify-phone-evidence.tests.ps1
- [ ] phone-targets — blocked by: phone-verification-tools; gate: scripts\verify-phone-evidence.bat phone-targets
- [~] phone-account-session — fresh owner conversation implemented and PIN enrollment verified; call activation and actual telephone acceptance remain pending; gate: scripts\verify-phone-evidence.bat phone-account-session
- [~] phone-azure-service — bridge/workflow deployed and audio probe passed; actual telephone cold-start acceptance remains; gate: scripts\verify-phone-evidence.bat phone-azure-service
- [~] phone-audio-loop — authenticated inbound media and cleanup implemented and tested; actual telephone acceptance remains; gate: scripts\verify-phone-evidence.bat phone-audio-loop
- [ ] phone-number-routing — blocked by: phone-audio-loop, call-budget-gate; gate: scripts\verify-phone-evidence.bat phone-number-routing
- [!] call-topology-proof — controlled owner calls authorized2026-10-17 with10USD daily allowance, but user deferred test readiness; concrete conference transport and real audio evidence remain required; gate: scripts\verify-phone-evidence.bat call-topology-proof
- [ ] call-client — blocked by: call-topology-proof, phone-account-session; gate: scripts\verify-phone-evidence.bat call-client
- [ ] assistant-call-task — blocked by: call-topology-proof, phone-number-routing, call-budget-gate; gate: scripts\verify-phone-evidence.bat assistant-call-task
- [ ] recording-capture — blocked by: phone-audio-loop, phone-account-session; gate: scripts\verify-phone-evidence.bat recording-capture
- [ ] recording-s3-ingest — blocked by: recording-capture; gate: scripts\verify-phone-evidence.bat recording-s3-ingest
- [~] recording-app-access — web/native playback and cross-tab capture exclusion are deployed; exact Jobs recording links are implemented. Complete expanded call-mode acceptance remains dependent on call-client; gate: scripts\verify-phone-evidence.bat recording-app-access
- [ ] phone-acceptance — blocked by: phone-number-routing, recording-app-access; gate: scripts\verify-phone-evidence.bat phone-acceptance
- [ ] call-modes-acceptance — blocked by: phone-acceptance, call-client, assistant-call-task, recording-app-access; gate: scripts\verify-phone-evidence.bat call-modes-acceptance
- [~] hotel-task-provider — five-catalog-hotel assistant-only Jobs implemented and offline-tested; actual calling remains blocked by owner stop and daily budget. Conference/takeover proof is not required for this implementation; gate: scripts\verify-phone-evidence.bat hotel-task-provider
- [~] hotel-results — five-property outcomes, evidence and exact recording links implemented; real interviews and full call-mode acceptance remain unverified; gate: scripts\verify-phone-evidence.bat hotel-results
- [~] car-usb-access — S9 access restored and signed0.3.17/code23 installed/verified2026-10-17. Formal evidence gate and physical car test remain open; gate: scripts\verify-phone-evidence.bat car-usb-access
- [ ] car-repro — blocked by: car-usb-access; gate: scripts\verify-phone-evidence.bat car-repro
- [ ] car-route-fix — blocked by: car-repro; gate: scripts\verify-phone-evidence.bat car-route-fix
- [ ] car-test-matrix — blocked by: car-route-fix, call-modes-acceptance, recording-app-access; gate: scripts\verify-phone-evidence.bat car-test-matrix
- [ ] car-release-proof — blocked by: car-test-matrix; gate: scripts\verify-phone-evidence.bat car-release-proof
- [ ] recording-live-proof — blocked by: recording-app-access, call-modes-acceptance, hotel-results, car-release-proof; gate: scripts\verify-phone-evidence.bat recording-live-proof
- [~] call-budget-gate — owner-message daily grants and app controls implemented; complete multi-leg ledger awaits call-topology-proof; gate: scripts\verify-phone-evidence.bat call-budget-gate

## recordings-current-run — deliver independent recording work without calls
depends on: existing owner-authenticated recording endpoints

Outcome: native Android recording playback/download, bounded shared recording-copy retries, web audio exclusion across tabs, and evidence gates that reject unsupported proof. Non-goals: new calls, spending grants, inbound routing, PIN invention, Android Auto or unrelated refactors. Worktree: C:\dev\live-ninja-plan-build on main; preserve C:\dev\live-ninja user changes. The workstreams below run independently, then root integrates their changes and publishes through GitHub.

Files: existing tools/phone-trial recorder, owner-message implementation and tests; Android recording DTO/view model/screen, API and Files navigation plus focused tests; web audio ownership and recording tests; scripts/verify-phone-evidence files; Help, plan.md and backlog.md. Reuse existing dependencies and test infrastructure.

### android-native-recordings — expose existing private recordings on Android
depends on: existing owner-authenticated recording endpoints
- [x] android-recordings-client — add source selection, states, playback/seek, fresh authenticated links and native download within the existing Files surface; protect microphone ownership and sign-out cleanup.
Definition of done: from C:\dev\live-ninja-plan-build\android, `gradlew.bat :app:testDebugUnitTest --tests ninja.jeremy.liveninja.net.PhoneRecordingsTest` and `gradlew.bat :app:assembleDebug :app:lintDebug` exit 0. The client implementation and focused tests pass; the full milestone remains partial because the existing AppUpdateInstaller.kt:123 NewApi lint error blocks lintDebug. Device playback remains unverified until an authorized device can be tested; unit/build results do not satisfy physical acceptance.

### recording-retry-recovery — share a durable transfer ceiling across callback paths
depends on: existing recording ingestion
- [x] recording-copy-budget — three transfer attempts total, 30/60-second retry delays, a claim lease longer than the deployed worker timeout, stale-worker protection and preserved ready/deleted/expired states. No redial or provider source deletion.
Definition of done: from C:\dev\live-ninja-plan-build, `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` exits 0. Verified locally: 89 tests passed on 2026-10-17. Full user deletion/account purge and live recovery remain outside this completed subtask.

### web-recording-audio-exclusion — prevent recording playback entering an assistant session
depends on: existing recording playback
- [x] web-audio-owner — coordinate recording playback, assistant microphone and wake capture across tabs using existing browser support; release claims after stopping or closing the owning surface.
Definition of done: from C:\dev\live-ninja-plan-build, `node --test tests/web/unit/recording-history.test.mjs` exits 0; root runs the existing wider web unit suite before delivery. Mock tests do not establish physical audio isolation.

### verifier-proof-repair — reject incomplete or unrelated acceptance evidence
depends on: existing evidence verifier
- [x] verifier-proof-binding — strengthen existing proof checks and regression fixtures without treating synthetic evidence as live acceptance.
Definition of done: from C:\dev\live-ninja-plan-build, `powershell.exe -NoProfile -NonInteractive -File scripts/verify-phone-evidence.tests.ps1` exits 0. No stopped call case runs.

### recordings-integrated-delivery — review, verify and publish the combined change
depends on: android-native-recordings, recording-retry-recovery, web-recording-audio-exclusion, verifier-proof-repair
- [x] recordings-help-and-checks — root updates Help for shipped behavior, reviews the complete diff, and runs focused tests plus `go test ./internal/webapp/ -run TestHelpDrawer -count=1` and `git diff --check`; all must exit 0.
- [x] recordings-main-release — root commits only task files, pushes main and watches every triggered workflow to a terminal result. Verify the deployed SHA without creating a call or changing owner-stop/budget state.
Definition of done: `git push origin main` exits 0 and `gh run watch <recorded-run-id> --repo JeremyProffittOrg/live-ninja --exit-status` exits 0 for each triggered delivery run; root appends exact SHA/run identifiers and evidence to Execution log. Android source verification alone is not an installed app release.

Supervision: local tests have a 20-minute deadline; Android build/lint has 45 minutes; delivery runs have 60 minutes. Register session/run identifiers and monitor both success and failure. Classify failures before retrying the recorded command; one ceiling of three attempts per milestone applies across parent and agents. Existing Stop conditions govern this run. No local schedules or new call permissions are created.

## inbound-fresh-owner-build — build authenticated fresh phone sessions while calls stay stopped
Depends on: existing recorder, broker, Azure bridge and owner APIs; live activation depends on private PIN enrollment and explicit resumption of calls.
Outcome: allowlisted owner calls authenticate with PIN, consent to recording, and enter a fresh owner conversation using existing persona, rules, memory, transcript and quota services. Initial phone tool set is read-only: memory_search, entity_get, knowledge_search, knowledge_recent, job_list, job_status. No phone token can mutate account data, start another call, control devices or bypass review. Non-goals: active-session handoff, routing activation, new providers/dependencies, call-stop release, new spending grants, Android Auto.
Files: recorder/inbound helper and existing tests, bridge owner media path/tests, existing broker/realtime/webapp auth and tests, deployment wiring, contracts, Help and this plan. Preserve outbound hotel behavior.
### inbound-broker-boundary — bind owner privileges to one live call session
- [x] inbound-phone-capability — IAM-only mint after recorder authorization; fresh active-owner lookup, quota session and phone token expiring at the call deadline plus 60 seconds; exact tool/transcript sinks and session binding; shared six-tool policy enforced in prompt, manifest and execution.
Definition of done: `go test ./cmd/realtime-broker ./internal/realtime ./internal/webapp ./internal/tools -count=1` exits 0, including negative authorization/session tests. Agent /root/phone_verification owns these files.
### inbound-recorder-bridge — authenticate caller before owner data or recorded conversation
- [x] inbound-pin-media — signed callbacks, exact From/To allowlist, persisted PIN attempt limit, no raw PIN logging, consent before recording, bounded one-mint bootstrap, one-use bridge redemption, owner model/tool/transcript transport, 180-second cap and matching session/voice-lock cleanup. Reuse existing ledger reservations; never release owner-stop.
- [x] inbound-private-pin-helper — real .bat entrypoint with hidden local entry and salted verifier storage, no PIN in arguments/chat/temp files and no automatic activation.
Definition of done: `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` and `go test ./cmd/phone-trial-bridge -count=1` exit 0. Agent /root/phone_backend owns recorder, bridge and helper files.
### inbound-closed-delivery — publish implementation without enabling calls
- [x] inbound-contract-release — root verifies integration, Help/contract truth, IAM resource scope and absent/disabled inbound configuration; commit/push main; watch exact GitHub run IDs to terminal success. Real call proof remains open.
Definition of done: focused tests and existing main/phone deployment workflows succeed, unauthenticated inbound requests are rejected, owner-stop remains set and no call was created.
Supervision: local test20 minutes; delivery60 minutes; at most three classified attempts per milestone. Missing private PIN or call authority blocks runtime activation only. No local scheduled job, provider routing update, or secret read.

## recording-account-lifecycle — include recordings in existing account privacy actions
Depends on: recording-s3-ingest and existing authenticated account export/delete.
Outcome: account export includes bounded recording metadata and authenticated download references; account deletion removes owned provider media, S3 versions and private metadata. A durable tombstone prevents late ingestion recreating audio. Non-goals: no live deletion, provider mutation, retention change, new service or framework during this run.
Files: new recorder account_lifecycle.py and tests; existing webapp account export and account-purge worker/tests; exact Lambda adapter, template IAM and recorder packaging; existing ingestion callbacks get shared tombstone guards. Agent /root/phone_verification owns lifecycle and Go integration, /root/phone_backend owns recorder hooks, root owns deployment/Help/contracts.
### recording-account-privacy — retain owner boundaries through partial failures and retries
- [x] recording-export-purge — paginated owner-bound exports exclude PINs, verifiers, capabilities and credential data; deletion marks state before side effects, validates provider/recording/object identity, removes versions and metadata only after successful media cleanup, and preserves retry evidence. No actual account-delete request is executed.
Definition of done: `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'`, `go test ./cmd/account-purge ./internal/webapp -count=1`, `go test ./internal/webapp -run TestHelpDrawer -count=1` and `git diff --check` exit 0. GitHub deployments reach terminal success; live destructive acceptance stays unperformed.
Supervision:20-minute tests,60-minute delivery, three classified attempts; inherit root Stop conditions. Data deletion tests use isolated fakes only.

## Scope references

These named sections define requirements and evidence; they are not executable milestones. The milestone graph and commands below govern execution.

#### phone-access — Twilio calling with an Azure backend

Status: IMPLEMENTATION AUTHORIZED. Original scope added 2026-10-15; implementation started by later locked decisions. Existing call stop and deployment-only delivery remain in force. Preserve the linked completed history.

##### phone-outcome — scope and implementation path

Outcome: call one phone number and talk to the existing Live Ninja account. Twilio supplies a US local number and bidirectional Media Streams. A small Go service in Azure Container Apps handles call webhooks and the persistent audio WebSocket. An Azure realtime model handles speech. Existing Live Ninja services remain the authority for account data, personas, saved rules, memory, tools, history, and permissions.

Proposed initial scope: inbound access plus in-app outbound calling, optional assistant listening, takeover/take-back, and authorized assistant-initiated calls. One owner account and one concurrent conversation remain proposed limits; a conversation can have multiple provider call legs. Proposed authentication: enrolled caller number plus keypad PIN before accessing private data. Caller ID alone is not authentication.

Non-goals: migrating Live Ninja's AWS data stores, SMS, number porting, public callers, multi-account routing, Teams, or unrelated backlog work. These boundaries remain proposals while the operator adds requirements.

Planning changes C:\dev\live-ninja\plan.md; the later phone-credential-helper section authorizes only its three named helper/test files in addition. Expected implementation areas: cmd/phone-bridge, internal/telephony, existing realtime/auth/webapp integration, containers/phone-bridge, infra/azure-phone, .github/workflows/deploy-phone.yml, contracts/api.md, deploy.md, and the Help block in web/templates/pages/conversation.html. Confirm actual files after reading the full relevant source. Do not scaffold future layers.

##### phone-decisions — inputs required before a run

The front-loaded batch is recorded in Open decisions and Locked decisions. The bullets below are requirement details; resolved choices are not to be asked again. Discover account/access facts read-only before escalating only missing owner inputs.

- Target accounts: discover Azure subscriptions, credit eligibility, existing model deployments, and Twilio account status using identifiers only. Ask the operator only if multiple eligible targets remain. Record the exact subscription, tenant, resource group, region, deployment, Twilio account, and Live Ninja account.
- Number: proposed US local number. Operator supplies area code or accepts any available US local number. Record number purchase ceiling and recharge policy.
- Access: confirm owner-only caller enrollment plus PIN, and which account/persona receives calls. PIN entry stays local; never write it into this plan or logs.
- Budget: use the confirmed call-budget rule: $20/month, then explicit $5-for-today permissions, renewed only with a new owner grant. Default one conversation, 30 minutes per normal call, no redial. Azure credits do not cover Twilio.
- Model: prefer an existing eligible Azure deployment with realtime audio and tools. Verify formats, voice support, region, quota, rates, and credit coverage. Do not silently fall back to a paid non-Azure model.
- Acceptance: agree on an available test caller before execution. Proposed live test proves speech, interruption, rule/memory access, one harmless tool, and hangup. The old spoken-smoke bypass does not prove telephone audio.
- Schedule: no execution deadline supplied. No local recurring automation authorized. Finish collecting the operator's added requirements before freezing the run.


#### app-calling — make calls inside Live Ninja with optional assistant participation

Status: IMPLEMENTATION AUTHORIZED; expanded call topology remains unproven and its live experiment is blocked by stop-calls. Provider building blocks do not establish end-to-end takeover behavior.

##### app-call-modes — requested behavior

- Human call: the user dials from Live Ninja and speaks through the app microphone/speaker. The assistant receives no call audio unless enabled.
- Listen: the assistant receives both sides of the selected call, keeps context, and remains silent to the remote party. Enforce silence in the audio bridge, not only in model instructions.
- Take over: on an authenticated user command, the assistant speaks to the remote party using the call context. The user can remain listening and use Take back to resume speaking. Transition must not redial or drop the remote party.
- Assistant call: the backend places an outbound call for an authorized task and runs the conversation without needing an open browser. The owner can inspect status, join where supported, take back control, or end the call.

The phrase "make calls it's self" requests the capability; it does not yet define standing autonomous dialing authority. Proposed default is an explicit request with resolved recipient, purpose, and call limits. Unattended or trigger-based calling requires a separately locked scope, destinations, budget, retry policy, and permitted actions. No real calls are authorized during this planning task.

##### app-call-evidence — verified building blocks and unresolved topology

Read on 2026-10-15:
- Twilio Voice JavaScript SDK supports browser calling: https://www.twilio.com/docs/voice/sdks/javascript .
- Twilio Call resource creates outbound calls: https://www.twilio.com/docs/voice/api/call-resource .
- Twilio conferences support app, telephone, and SIP participants and participant controls: https://www.twilio.com/docs/voice/conference and https://www.twilio.com/docs/voice/api/conference-participant-resource .
- Unidirectional Media Streams can receive both call tracks, but cannot inject audio. Bidirectional streams receive only the inbound track and block subsequent TwiML while connected: https://www.twilio.com/docs/voice/media-streams and https://www.twilio.com/docs/voice/twiml/stream .
- C:\dev\live-ninja\web\static\js\mic.mjs currently owns the assistant microphone state and creates a RealtimeSession. Existing mic and assistant sessions must be coordinated with a phone call to prevent duplicate capture, echo, and unintended speech. No evidence yet that the current app audio can simply be attached to Twilio unchanged.

Proposed implementation path: use the Twilio Voice SDK for the user's app audio, Twilio for dialing/conference control, and the Azure service for AI and orchestration. Retain the existing Live Ninja account/tool backend. Prove a conference topology with a separately connected assistant media participant so human and AI speech can be gated independently. The assistant-to-conference connection needs an explicit supported transport; do not claim that Connect/Stream and Dial/Conference on the same call leg form a working bridge. Evaluate that specific connection before selecting an additional SIP/media gateway or dependency.

The topology proof is a prerequisite for committing to seamless listen/takeover behavior. It must establish both-side audio, remote audio continuity, AI audio injection, feedback avoidance, participant cleanup, and continued AI calls when the browser leaves. If an extra service or materially different transport is required, record its cost and obtain scope approval before building it. Do not quietly narrow takeover into hanging up and redialing.

##### app-call-decisions — add to the pre-run question batch

Resolve these with phone-decisions once the operator finishes adding requirements:
- Surfaces: native Android and Bluetooth are explicitly requested; Android Auto is deferred. Native SDK, audio-focus and Telecom integration are in planned scope. Preserve the proposed web dialer; lock whether web and Android ship together before execution.
- Autonomy: explicit per-call tasks only (recommended initial behavior), or standing authorization for specified tasks/contacts/triggers. Define recipient resolution, caller ID, geographic destinations, duration, daily/monthly spend, voicemail behavior, and retry limits.
- Participation: assistant off by default or listen by default; user microphone state during takeover; whether the call continues if the user disconnects. Proposed behavior is off by default, user muted but listening during takeover, and continuation only after explicit handoff.
- Disclosure and data: define how the remote party is told when AI listens or speaks, and whether third-party call transcripts are retained or used for memory. Record all Live Ninja phone conversations in S3 with in-app playback/download per call-recordings. Use the accepted friendly introduction/recording request and 180-day S3 retention; preserve sensitive-input exclusions and do not automatically promote third-party statements into owner rules or memory.
- Acceptance contacts: operator provides consenting test recipients and test-call spend allowance before execution. Contact details and credentials stay out of public Help copy.

These are pending product decisions, not additional questions during a running build.


#### hotel-internet-job — call five hotels and compare guest Wi-Fi access

Status: IMPLEMENTATION AUTHORIZED. Depends on assistant-outbound and call-expanded-delivery; no hotel dialing while stop-calls remains active.

Outcome: the owner submits one job with five specific hotels. Live Ninja calls each property, asks follow-up questions about joining its guest internet, and returns a comparison focused on the owner's restrictive work VPN. A successful phone connection alone is not a successful hotel interview. A hotel's answer is reported evidence, not a verified VPN compatibility test.

##### hotel-existing-jobs — verified foundation

Read on 2026-10-15:
- C:\dev\live-ninja\internal\jobs\README.md and types.go describe durable reminders and review checkpoints, not external execution. Approval of an existing review records acknowledgement only; it must not silently acquire permission to call.
- C:\dev\live-ninja\internal\webapp\jobs_routes.go has authenticated, account-scoped Jobs routes, fresh authorization, request IDs, version checks, and web/Android mutation restrictions.
- C:\dev\live-ninja\web\static\js\jobs.mjs implements the existing Jobs interface. Reuse it rather than add a separate job product.
- These Jobs files include user-owned uncommitted/untracked work. Preserve it. Their presence does not prove deployed provider support.
- The current job aggregate is bounded below 250KB and retains up to 50 runs. Design a small per-hotel result/call ledger within existing limits; do not insert unbounded call transcripts into the aggregate.

##### hotel-job-decisions — add to the single pre-run question batch

- Five properties: select five random distinct California hotels. Build an eligible list from official property sites, record the selection seed/list, and verify each public property number. Freeze the chosen five in the job snapshot; do not ask the owner to choose hotels again. Do not substitute a central booking line without recording that fact.
- Call scope: one manual job, five distinct properties, one call at a time; proposed maximum 10 minutes each and zero automatic redials. Lock total monetary allowance, calling hours in each hotel's timezone, and voicemail/transfer policy before running.
- VPN criteria: optional operator-supplied restrictions beyond password/simple acceptance. Never request corporate VPN credentials, internal endpoints, or confidential policy documents. If specifics are unavailable, compare access methods without claiming a compatibility verdict.
- Report retention and disclosure: use the previously locked AI participation policy and transcript preferences. Proposed opening identifies Live Ninja as an automated assistant calling for a traveler to ask about guest internet. No bookings, payment commitments, or other tasks.
- Permission: approve the exact hotel list, objective, question boundaries, and cost/time limits once for the batch. Ordinary follow-up questions within that objective do not require new owner input. Adding hotels, booking rooms, calling unrelated numbers, or expanding purpose does.

##### hotel-questionnaire — distinguish simple access from extra requirements

Suggested opening: "Hi! I'm Jeremy's AI assistant, helping plan his world-domination tour — starting with hotel Wi-Fi his work VPN will tolerate. Could you help with a quick question? May I record our chat so Jeremy can listen later?"

Wait for yes before recording. Then ask for the actual laptop connection steps. If the person declines, log declined-recording-consent, end politely, and leave audio absent with that reason. Keep the tone friendly, appreciative, brief, and curious. Do not imply a booking or pressure staff to agree.

Ask for the actual steps, not just whether the hotel has free Wi-Fi:
- Can a laptop connect using only a Wi-Fi password, or a simple browser Accept button?
- Does the browser require a room number and last name, account creation, email verification, SMS code, payment, or another sign-in step?
- Must the guest install an app, certificate, security software, or device-management profile?
- Does access expire or require repeated sign-in? Can staff provide a direct-access network or wired Ethernet without those extra steps?
- Does staff know of VPN restrictions? If the answer is uncertain, can an IT/network contact clarify? Record a transfer as the same property's inquiry and stay within the approved call budget.
- Confirm the answer applies to the specific property's guest-room network, not just the lobby or brand-wide policy. If the employee is unsure, record unknown; do not infer "simple access."

Classify each hotel as password-only, simple-accept, extra-login, installation-required, or unknown, with the actual steps preserved. Multiple requirements can apply; the report must not force ambiguous answers into a favorable category. Keep staff-reported VPN support separate from observed access requirements. No real VPN test occurs in this phone job.

## Completed setup — credential helper

### phone-credential-helper — local user-run setup only
phone-credential-helper — local user-run setup only

This is the sole implementation exception to the phone planning hold, requested 2026-10-15. Files:
- C:\dev\live-ninja\scripts\setup-phone-credentials.bat — real cmd.exe entry point.
- C:\dev\live-ninja\scripts\setup-phone-credentials.ps1 — Windows PowerShell helper for hidden input and stdin-based gh writes.
- C:\dev\live-ninja\scripts\setup-phone-credentials.tests.ps1 — offline checks with synthetic inputs and no provider writes.

Contract for future phone workflows (not wired into CI yet):
- Repository Actions secrets: TWILIO_AUTH_TOKEN (webhook signature validation), TWILIO_API_KEY_SECRET (matching Voice SDK/REST key secret).
- Repository variables: TWILIO_ACCOUNT_SID and TWILIO_API_KEY_SID. These are identifiers, not secret values.
- Existing repository Actions secret AZURE_OPENAI_API_KEY: optional prompt only when absent, for the existing Azure OpenAI engine. Skip when using managed identity or when the intended Azure resource is not selected. No new Azure client secret or static AWS credential.
- Target is pinned to JeremyProffittOrg/live-ninja; no environment or organization secret scope. The helper verifies origin, GitHub identity/access, and existing names before asking for values.
- Empty input skips without any write/delete. Configured values are skipped unless the user explicitly invokes --replace-existing. One hidden entry per value; print its character count and requested last four characters locally only. Values of four or fewer characters do not print in full. Nonempty length is not provider validation.
- Use gh's supported stdin flow, not --body values, shell interpolation, environment substitution, or temporary files. Verify successful writes via exit status and a names-only read. Report failures accurately and never automatically retry writes.
- --check performs read-only preflight; no prompts or writes. The helper does not buy numbers, place calls, change providers, push, or deploy.
- URLs printed come from existing project configuration. The privacy URL documented in the older setup is not backed by a verified current route; no working privacy page is claimed. Twilio media/call callbacks are not deployed and must not be confused with the existing login callback.

Credential status: the operator reported completing the helper on 2026-10-15. The earlier unauthenticated result is historical, not a current blocker. Before execution, verify GitHub access and names only; do not ask for the same values again unless a named required entry is actually absent. Credential values are never requested in chat.



Definition of done: "powershell.exe -NoProfile -NonInteractive -File scripts/setup-phone-credentials.tests.ps1" exited 0 with the stdin roundtrip and 11 offline checks. Operator reported saving credentials; current provider validity is not independently verified. No further helper changes are part of this plan revision.

## phone-preflight — lock targets and contracts before implementation
depends on: none

Jobs: local-test-job, delivery-job, controlled-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat phone-targets" to check the terminal gate and its dependency evidence.



### phone-verification-tools — executable evidence gates for the requested test coverage
depends on: none

- [x] phone-evidence-tools — create scripts/verify-phone-evidence.bat, its safe PowerShell helper, and focused offline tests using the existing PowerShell/Go/Node tooling; no framework or service. The .bat accepts one exact milestone name to verify its evidence, or run-case with a declared scenario name. No user secret in arguments. Provide a read-only gate mode separate from paid case execution.
- [x] phone-evidence-schema — define fixed required cases for every milestone from the acceptance criteria in this plan. Store immutable attempt artifacts outside the repo at the exact evidence root in Execution contract. Include command/exit/result source, commit, device/account identifiers, provider call/recording IDs, audio-route observations and human confirmation where needed. Do not accept missing/skipped required cases as success.
- [ ] phone-run-manifest — save the locked nonsecret account/resource names, device serials, budgets, exact approved targets, deployment run IDs, and scenario-to-test mappings before executing dependent cases. Validate format, account scope, stale decisions, and recorded SHA; reject unresolved fields. Missing values fail closed. Keep secrets in the existing secure flows only.

Definition of done: from C:\dev\live-ninja-plan-build, "powershell.exe -NoProfile -NonInteractive -File scripts/verify-phone-evidence.tests.ps1" exits 0. Tests must prove the verifier fails on missing evidence, incorrect owner/commit, failed command, no human audio evidence, unknown case, and attempted call execution without a locked budget/target/start flag. Positive fixtures are synthetic and explicitly labeled; they cannot satisfy live gates. The verifier is planned test support and has not been created or run during planning.

### phone-targets — verify account access and cost
depends on: phone-verification-tools

- [ ] phone-target-evidence — record exact provider identifiers, credential names/status only, eligible credit offer, model quota, number selection, spending ceiling, and call limits. Confirm noninteractive access and session availability for the run. Use hidden background commands for short work; no desktop control without permission.
- [ ] phone-contract-evidence — read existing mint, authentication, tool, transcript, and usage handlers in full. Choose the smallest call-scoped server integration. Document trusted identity binding, expiration, replay protection, and revocation. Resolve API/schema changes and required dependencies before coding.
- [ ] phone-run-lock — replace tentative paths and verification commands with exact account-specific values. No unresolved decision required by a runnable workstream may be deferred to that workstream. Missing car access blocks only its dependents.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat phone-targets" exits 0. Required checks: gh auth status; gh secret list --repo JeremyProffittOrg/live-ninja; account/resource identity and noninteractive access checks. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## phone-foundation — Azure hosting and trusted Live Ninja access
depends on: phone-targets

Jobs: local-test-job, delivery-job, controlled-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat phone-azure-service" to check the terminal gate and its dependency evidence.


### phone-account-session — preserve account behavior and permissions
depends on: phone-targets

- [~] phone-session-binding — fresh owner conversation implementation uses short-lived call-bound authentication and private PIN enrollment is verified2026-10-17; explicit call activation remains an owner decision. Reuse existing persona, rules, memory, tools, and transcript logic. Never trust a caller-supplied user ID or model argument as identity. No unrestricted account impersonation or direct AWS credentials in the Azure bridge.
- [x] phone-tool-filter — support the phone surface explicitly. Exclude unsupported camera/volume/device controls. Preserve tool authorization, confirmation, and idempotency. Phone location follows existing explicit-current/home rules; a phone number is not GPS.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat phone-account-session" exits 0. Required checks: go test ./internal/realtime/ ./internal/tools/ ./internal/webapp/ ./cmd/realtime-broker/ -count=1. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### phone-azure-service — host the persistent audio connection
depends on: phone-account-session


- [~] phone-host — Implementation deployed and tested; complete live acceptance remains. Original acceptance requirement: add the Go bridge and Azure Container Apps configuration with HTTPS/WSS, health checks, bounded queues, explicit timeouts, graceful cleanup, and no raw audio/PIN/token logs. Verify cold-start and connection behavior. Use scale-to-zero only if answer deadlines pass; otherwise record and price the smallest warm instance within budget.
- [~] phone-azure-workflow — Implementation deployed and tested; complete live acceptance remains. Original acceptance requirement: add a separate GitHub Actions workflow on main using azure/login@v2, id-token: write, and contents: read. Preserve the AWS workflow. Verify the exact JeremyProffittOrg/live-ninja federated subject before adding credentials; never reuse the event repository's identity. Prefer managed identity for Azure model access. Use existing hidden credential entry for Twilio secrets and supported app-secret injection; no paid secrets service.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat phone-azure-service" exits 0. Required checks: go test ./cmd/phone-trial-bridge/ -count=1; GitHub run and deployed health/commit checks from deployment.json. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.



## call-budget — enforce the owner's monthly threshold and daily grants
depends on: phone-targets

Jobs: local-test-job, delivery-job, controlled-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat call-budget-gate" to check the terminal gate and its dependency evidence.

Confirmed 2026-10-15: automatic call spending stops at $20 per month. Beyond that threshold, an explicit owner permission grants an additional $5 budget for that day. When that daily grant is exhausted, request fresh permission for another $5; never treat silence, an old job approval, or an earlier day's grant as approval.

### call-budget-gate — reserve call costs and wait for explicit grants
depends on: phone-targets

- [ ] call-cost-ledger — persist measured and reserved call costs across all provider legs, recording charges, and the selected backend usage allocation. Record gross billed and credit-offset amounts separately. Until a more specific accounting decision is supplied, use the conservative gross phone-feature cost for admission; do not assume Azure credits make calls free. Use the owner's configured timezone, falling back to America/New_York, for day/month boundaries.
- [ ] call-spend-admission — before dialing, reserve a conservative cost bound for the remaining call duration and all participants/recording. Do not start or continue spending past the authorized balance while waiting for delayed billing events. Stop the active call politely before its reserved allowance expires. No duplicate reservations on task/callback retry.
- [~] call-daily-approval — after the monthly $20 threshold, put pending calls/jobs into waiting_approval with a clear $5-for-today request. Approval must come from the authenticated owner interface and bind to that day's remaining allowance. Exhaustion requires a new $5 approval; midnight invalidates unused daily authority. This is product behavior requested by the owner, not an agent deployment permission prompt.
- [~] call-budget-observability — show month usage, current daily grant/remaining amount, pending reservation, and why dialing is paused. Keep stored recordings and completed job results accessible when new calls are paused. Do not delete recordings to meet a calling budget.


Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat call-budget-gate" exits 0. Required checks: go test ./cmd/phone-trial-bridge/ ./internal/jobs/ ./internal/tools/ ./internal/webapp/ -count=1. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## phone-media — connect telephone audio to the Azure model
depends on: phone-azure-service, call-budget-gate

Jobs: local-test-job, delivery-job, controlled-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat phone-number-routing" to check the terminal gate and its dependency evidence.


### phone-audio-loop — authenticate and run one conversation
depends on: phone-azure-service

- [~] phone-ingress-validation — Implementation deployed and tested; complete live acceptance remains. Original acceptance requirement: verify Twilio signatures on webhooks and WebSocket upgrades against the actual public URL, including proxy handling. Check expected account, destination, CallSid, StreamSid, and short-lived stream binding. A Twilio signature verifies the provider, not the caller's personal identity.
- [~] phone-caller-auth — Implementation deployed and tested; complete live acceptance remains. Original acceptance requirement: implement enrolled-number/PIN validation before private data or privileged tools. Bound retries and reject unknown callers. Keep PIN digits out of model input and logs. Return valid TwiML on acceptance and rejection.
- [~] phone-stream-adapter — Implementation deployed and tested; complete live acceptance remains. Original acceptance requirement: use bidirectional Connect/Stream. Handle start/media/mark/clear/stop and Azure session events. Twilio uses base64 8 kHz mono mu-law audio; negotiate the selected model's actual formats and convert only when needed. On interruption, cancel generation, clear queued playback, and reconcile heard audio with model state.
- [~] phone-call-cleanup — Implementation deployed and tested; complete live acceptance remains. Original acceptance requirement: enforce concurrency and duration limits. Deduplicate callbacks and tool events. Close model connections and settle transcript/usage once on hangup or error. Give a short audible failure response where possible; never leave a paid silent call open.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat phone-audio-loop" exits 0. Required checks: go test ./cmd/phone-trial-bridge/ -count=1; go test -race ./cmd/phone-trial-bridge/ -count=1. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### phone-number-routing — attach the authorized number
depends on: phone-audio-loop, call-budget-gate


- [ ] phone-number-config — acquire or reuse exactly one voice-capable number. Set inbound voice/status callbacks to the verified Azure endpoint. Do not enable SMS or extra numbers. Enable call recording as required by call-recordings. Enable outbound destinations only after assistant-outbound/call-controls authorization and geographic limits are locked; inbound setup alone does not enable unrestricted dialing.
- [ ] phone-cost-verification — record provider number/configuration evidence and call usage without credentials. Track Twilio minutes, Azure model usage, and hosting separately. Application limits enforce the agreed bounds; budget alerts alone are not hard caps.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat phone-number-routing" exits 0. Required checks: provider number ownership/capability/webhook checks against the locked account and deployed URLs. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## call-topology — prove the expanded media path before feature work
depends on: phone-targets, phone-azure-service

Jobs: local-test-job, topology-proof-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat call-topology-proof" to check the terminal gate and its dependency evidence.


### call-topology-proof — verify a supported user/remote/assistant connection
depends on: phone-targets, phone-azure-service

- [!] call-transport-proof — controlled owner calls authorized2026-10-17 within the recorded10USD allowance, but the owner then deferred parked readiness. Calling controls remain disabled pending verified routing and a ready test. Select the exact assistant conference transport from official documentation and existing code before a bounded two-party/assistant experiment. Record all legs, stream directions, disconnects and cost; no unsupported takeover implementation is implied.
- [ ] call-cost-model — replace inbound-only totals with separate human, listening, takeover, and assistant-only formulas. Include each telephone/SDK/SIP leg, conference participant-minute, media stream, Azure model, and hosting charge. A single conversation may have multiple billed legs. Recheck current official rates before provisioning: https://www.twilio.com/en-us/voice/pricing/us .



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat call-topology-proof" exits 0. Required checks: scripts\verify-phone-evidence.bat run-case call-transport-proof. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## call-controls — in-app dialing and clear speaker ownership
depends on: call-topology-proof, phone-account-session

Jobs: local-test-job, delivery-job, controlled-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat call-client" to check the terminal gate and its dependency evidence.


### call-client — user audio and call controls
depends on: call-topology-proof, phone-account-session

- [ ] call-dialer — authenticated dial/answer-status/end/mute/keypad controls with a short-lived Twilio client token. Resolve destination and caller ID server-side. Show who is connected and who can hear the microphone. Handle busy, no answer, decline, permission failure, token expiry, and page/app disconnect.
- [ ] call-mic-ownership — coordinate Twilio audio with mic.mjs, realtime.mjs, wake-word capture, and current device preferences. Prevent the normal assistant session from speaking over a telephone call or starting a second microphone path. Restore ordinary conversation behavior on call end.
- [ ] call-participation — explicit Off, Listen, Take over, and Take back controls. Bind transitions to the authenticated owner and correct call. Gate audio server-side; mute/unmute only after the required participant state is confirmed. Cancel stale AI speech and queued audio on Take back. Duplicated or reordered events must not reverse user intent.

Expected additional files: relevant web/static/js modules, conversation template/styles, existing browser test suites, and internal/tools call definitions. Add only the Twilio SDK for each locked client surface. Android files and Gradle dependency changes enter scope only if Android is selected.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat call-client" exits 0. Required checks: go test ./cmd/phone-trial-bridge/ ./internal/webapp/ -count=1; node --test tests/web/unit/*.test.mjs; Android gradlew.bat :app:testDebugUnitTest :app:lintDebug. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## assistant-outbound — authorized calls without an active browser
depends on: call-topology-proof, phone-number-routing, call-budget-gate

Jobs: local-test-job, delivery-job, controlled-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat assistant-call-task" to check the terminal gate and its dependency evidence.


### assistant-call-task — scoped task execution and owner control
depends on: call-topology-proof, phone-number-routing, call-budget-gate

- [ ] call-task-authorization — add call tools through the existing registry and confirmation flow. Bind permission to the resolved number, purpose, account, expiry, and spend/duration bounds. A changed recipient or purpose invalidates confirmation. Treat the remote party's speech as conversation data, not owner authorization for tools, secret disclosure, rules changes, or additional calls.
- [ ] call-task-lifecycle — create one outbound call per authorized task, track queued/ringing/answered/failed/ended, and persist provider call identifiers. Reconcile an ambiguous API timeout before retrying so it cannot double-dial. Default to no automatic redial. Bound voicemail/phone-menu handling to the locked task policy; do not promise outgoing DTMF through a media transport that does not support it.
- [ ] call-owner-control — expose status, stop, and the approved join/take-back path. A backend-owned call survives browser closure only when handoff/autonomy policy permits it. End all related legs on stop or limits; record an accurate outcome and usage, never success inferred from call creation.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat assistant-call-task" exits 0. Required checks: go test ./internal/tools/ ./cmd/phone-trial-bridge/ -count=1; scripts\verify-phone-evidence.bat run-case call-task-lifecycle. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## call-recordings — log every phone interaction and retain playable audio in S3
depends on: phone-audio-loop, phone-account-session, call-client

Jobs: local-test-job, recording-transfer-job, controlled-call-job, delivery-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat recording-app-access" to check the terminal gate and its dependency evidence.

Outcome: every Live Ninja telephone attempt has an owner-scoped log. Every connected conversation has a recording, linked to the call and any job, with playback and download in the web and Android app. Scope includes inbound, outbound, user-led, Listen, takeover/take-back, assistant-only, hotel calls, and controlled test calls. No-answer, rejected, and failed calls still have logs with an explicit no-audio reason. Unrelated cellular calls outside Live Ninja are not captured.

Capture the actual call participants, not the phone room microphone continuously. Log authentication success/failure without PINs. Exclude authentication PIN/credential entry from audio and transcripts; show excluded intervals rather than silently claiming an uninterrupted recording. Record call events and audio status separately from infrastructure logs. Never place audio, credentials, or presigned URLs in CloudWatch/debug logs.

Verified source facts: C:\dev\live-ninja\internal\webapp\deliverables_routes.go already authenticates downloads and emits no-store redirects; internal/store/deliverables.go enforces owner isolation and indexes S3 objects without Scan. internal/deliv/deliv.go currently signs attachment downloads, restricts camera upload media types, and caps direct content at 1 MiB. Reuse the ownership/storage patterns but do not route long recordings through the text upload path. template.yaml DeliverablesBucket is private and AES256-encrypted with an existing 180-day lifecycle; recording retention must be explicit rather than accidentally inheriting expiry.

Implementation path: provider-side recording at the selected Twilio call/conference boundary, followed by authenticated ingestion into S3. Prefer a single coherent conversation recording, with linked segments where transfer/topology requires them. Do not assume one conference channel per speaker: Twilio recording/channel support varies by API and account configuration. Prove both human and AI audio are captured through handoffs. S3 is the durable system of record; Twilio availability alone is not ready-for-playback status.
Sources: https://www.twilio.com/docs/voice/api/recording and https://www.twilio.com/docs/voice/twiml/conference .

### recording-capture — durable call journal and recording lifecycle
depends on: phone-audio-loop, phone-account-session

- [ ] recording-call-journal — persist account/call/job identity, direction, participants, timestamps, ringing/answer/end/failure, user/AI mode transitions, actions, recording status, and costs. Deduplicate callbacks and keep causal timestamps. No shared mutable log across users.
- [ ] recording-provider-capture — enable recording for each authorized call path with the locked disclosure/acknowledgement behavior. Ensure prompt start, both directions, human/AI handoffs, and visible sensitive-input exclusions. Obtain actual recording metadata using the verified account and RecordingSid, not an arbitrary callback URL. Verify callback signatures and bind account/call/conference/recording IDs to the saved owner.
- [ ] recording-state — track requested, recording, processing, copying, ready, absent, failed, and deleted states. Handle delayed, missing, duplicate, out-of-order, and failed callbacks. A completed conversation with missing media must show that defect; do not fabricate an empty success recording.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat recording-capture" exits 0. Required checks: go test ./cmd/phone-trial-bridge/ ./internal/store/ ./internal/webapp/ -count=1. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### recording-s3-ingest — verified private storage and bounded recovery
depends on: recording-capture

- [ ] recording-object-copy — use an AWS-owned worker with the existing workload role to fetch authenticated Twilio media and write the owner-scoped S3 object. Azure handles voice/orchestration; no static AWS keys in Azure or Twilio. Accept only known provider recording endpoints and validated IDs; restrict redirects and size/type. Stream bytes with bounded memory and record integrity checksum, byte count, duration, content type, and source RecordingSid.
- [ ] recording-storage-index — use private encrypted S3 plus existing owner-scoped index patterns. Reuse the deliverables bucket only if the locked retention fits its 180-day lifecycle; otherwise define a recording-specific bucket/lifecycle without changing unrelated deliverables. Playback-ready requires a verified S3 object and metadata commit. Never overwrite a different recording on a repeated event.
- [ ] recording-recovery — retain pending work durably and reconcile missed callbacks without redialing. Limit transfer attempts to three total across queue/worker retries with 30-second then 60-second delay. Keep failed records visible for explicit retry. Never delete a provider source before verified S3 persistence; provider-copy cleanup and S3 expiration require the locked retention policy.
- [~] recording-data-lifecycle — account deletion/export is implemented; automatic cleanup of the extra Twilio copy awaits the owner retention choice. Cover user deletion/account purge/export, object versions if enabled, metadata, provider copies, and in-flight callbacks. Deleted calls must not be recreated by late ingestion. Separate recording retention from mandatory 7-day CloudWatch log retention.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat recording-s3-ingest" exits 0. Required checks: go test ./internal/deliv/ ./internal/store/ ./internal/webapp/ ./cmd/account-purge/ -count=1; ingestion-worker integrity/retry tests. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### recording-app-access — play and download from call history and Jobs
depends on: recording-s3-ingest, call-client

- [ ] recording-call-history — show call details and recording state in Live Ninja history, with links from the corresponding Jobs run and each hotel result. Web and Android both support play/pause/seek, duration, and download. Display processing, failed, absent, expired, and deleted states honestly.
- [ ] recording-authorized-media — authenticate and reauthorize ownership before issuing short-lived playback or attachment URLs. Support HTTP byte ranges/seeking and audio content types; refresh an expired URL only after fresh authorization. Never expose Twilio credentials or raw provider URLs to clients. Account revocation prevents new links; document remaining lifetime of any already issued signed URL.
- [ ] recording-client-safety — coordinate playback with live-call/microphone ownership so playback cannot be relayed into an active conversation accidentally. Limit browser media/CORS changes to the selected S3 origin. Do not cache private audio in a service worker. Downloads use safe filenames and the native Android download/share path.
- [ ] recording-help — update Help, contracts, privacy/retention copy, and history/Jobs guidance in the same feature commits. Recordings are now requested functionality, not a hidden diagnostic feature.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat recording-app-access" exits 0. Required checks: go test ./internal/webapp/ ./internal/deliv/ -count=1; node --test tests/web/unit/*.test.mjs; Android gradlew.bat :app:testDebugUnitTest :app:lintDebug. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## phone-delivery — help, live proof, and terminal deployment checks
depends on: phone-number-routing, recording-app-access

Jobs: local-test-job, delivery-job, controlled-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat phone-acceptance" to check the terminal gate and its dependency evidence.


### phone-acceptance — prove the shipped call path
depends on: phone-number-routing, recording-app-access

- [ ] phone-help — update the Help drawer in the same commit as user-visible behavior. Explain calling, authentication, interruption, ending calls, limits, and supported tools. Publish no PIN/private caller data. Update contracts/api.md and deploy.md for actual interfaces and Azure delivery.
- [ ] phone-live-call — perform the authorized test call. Verify two-way speech, interruption without stale audio, the correct persona and rules/memory, a harmless tool, rejection before authentication, and complete cleanup on hangup. Record CallSid, commit, model deployment, timestamps, test outcomes, and usage. Verify the required recording reaches S3 and can be played/downloaded in Live Ninja.
- [ ] phone-release — commit scoped changes on main, push, and watch every triggered delivery workflow to a terminal result. Preserve unrelated changes. Remove scratch/debug code. Append exact evidence to the root Execution log as milestones land.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat phone-acceptance" exits 0. Required checks: go test ./internal/webapp/ -run TestHelpDrawer -count=1; go test ./... -count=1; scripts\verify-phone-evidence.bat run-case phone-live-call. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## call-expanded-delivery — verify every requested call mode
depends on: phone-acceptance, call-client, assistant-call-task, recording-app-access

Jobs: local-test-job, delivery-job, controlled-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat call-modes-acceptance" to check the terminal gate and its dependency evidence.


### call-modes-acceptance — verify user calls, listening, handoff, and independent calls
depends on: phone-acceptance, call-client, assistant-call-task, recording-app-access

- [ ] call-help-modes — update Help in each user-visible feature commit with dialing, audience/microphone state, AI participation, Take back, stopping calls, limits, and task authorization. Keep unreleased modes out of Help.
- [ ] call-live-matrix — prove app-to-phone audio; assistant hearing both parties while silent; takeover and take-back without redial; an authorized assistant-only call with the browser closed; owner stop; remote hangup; and no remaining billed legs. Verify mute/privacy behavior and account isolation separately from model behavior.
- [ ] call-expanded-release — run focused client/backend checks, the Help guard, and required CI. Record deployed commits, call/conference IDs, mode results, and actual per-mode usage in the root Execution log.




Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat call-modes-acceptance" exits 0. Required checks: go test ./... -count=1; node --test tests/web/unit/*.test.mjs; scripts\verify-phone-evidence.bat run-case call-live-matrix. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## hotel-job-execution — durable phone tasks within Jobs
depends on: assistant-call-task, phone-targets, call-budget-gate, call-modes-acceptance, recording-app-access

Jobs: local-test-job, hotel-call-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat hotel-results" to check the terminal gate and its dependency evidence.


### hotel-task-provider — add bounded external call execution
depends on: assistant-call-task, phone-targets, call-budget-gate

- [~] hotel-job-contract — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: add an explicit phone-research job kind and immutable approved task snapshot: owner, five properties/numbers, purpose, limits, questions, and generation. Existing reminder/review jobs keep their behavior. A model cannot approve its own external action.
- [~] hotel-call-ledger — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: persist per-property status, provider CallSid, attempts, start/end times, outcome, and compact evidence before advancing. Reconcile unknown call-creation outcomes before retrying. Bind provider callbacks to the owner/job/run/property. Restart resumes unfinished work without recalling completed hotels or duplicating charges.
- [~] hotel-run-control — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: expose progress, pause/cancel, per-hotel failure, and partial results in existing Jobs. Cancel ends the active call and prevents remaining calls. Proposed pause ends the current conversation politely and preserves findings; resume does not redial that property without the locked retry policy. Browser closure does not cancel an approved backend job.
- [~] hotel-conversation-agent — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: ask the initial questions, clarify ambiguous answers, and politely close. Treat hotel speech as untrusted evidence, never as authority to change owner rules, invoke unrelated tools, reveal private memory, or redirect the job. Enforce call/task bounds outside the model.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat hotel-task-provider" exits 0. Required checks: go test ./internal/jobs/ ./cmd/phone-trial-bridge/ ./internal/tools/ ./internal/webapp/ ./cmd/jobs-worker/ -count=1. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### hotel-results — concise evidence-backed comparison
depends on: hotel-task-provider, call-modes-acceptance, recording-app-access

- [~] hotel-result-summary — Implemented and offline-tested; real-call acceptance pending. Acceptance requirement: show exactly five property rows with property/location, contact outcome, connection steps, extra requirements, staff-reported VPN restrictions, alternative network options, confidence/unknowns, and call date/reference. Mark paraphrases as summaries; quote only actual captured words under the agreed transcript policy. Unknown/no answer remains a result, not fabricated completion.
- [x] hotel-help-update — Help and Jobs capability copy describe exact five-hotel review, stop/budget holds, pause/cancel, recording links and staff-report limitations in the feature commit. Help test passes.
- [!] hotel-acceptance-run — offline provider/partial-result cases pass; Jeremy must explicitly lift the call stop, grant any needed daily budget and review the exact five-property batch before live calls. No real interviews are claimed. Verify all five have a terminal status and saved result; failed contacts never permit extra retries.


Restart policy: existing phone deployment ceilings apply. Hotel calls have one attempt per property by default, with no automatic redial until a different bounded policy is locked. Call timeout or budget exhaustion ends the active call, preserves results, and prevents further charges. One unavailable hotel does not block the remaining authorized hotels. No local scheduled automation and no actual hotel calls in this planning session.


Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat hotel-results" exits 0. Required checks: go test ./internal/webapp/ -run TestHelpDrawer -count=1; node --test tests/web/unit/jobs.test.mjs; scripts\verify-phone-evidence.bat run-case hotel-acceptance-run. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## car-audio — restore car Bluetooth hands-free audio
depends on: phone-verification-tools

Jobs: local-test-job, human-hardware-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat car-route-fix" to check the terminal gate and its dependency evidence.

Status: DRAFT, NOT STARTED. Research and USB access verification completed on 2026-10-15; implementation remains on hold. Evidence: docs/android-auto-research.md. Interpret the reported symptom as "does not allow car hands-free"; exact failure mode still needs reproduction.

Outcome: use the car microphone and speakers for Live Ninja assistant sessions and the planned app calls. Fix the root routing/lifecycle cause after reproducing it. Do not accept car speaker output with the phone microphone as success.

Non-goals: replacing the phone OS, resetting Bluetooth pairings, moving app data, enabling disabled voice engines, building Android Automotive OS, disguising assistant audio as a music app, or modifying unrelated Jobs work.

Expected files: AndroidManifest.xml; existing realtime transports, SessionOrchestrator, and relevant wake/microphone lifecycle code; a shared routing owner only where existing multiple transport callers justify it; relevant existing unit/instrumentation tests; Help copy. Do not edit these during planning.

### car-usb-access — verified phone control
depends on: phone-verification-tools

- [x] car-phone-control — Windows enumerates SAMSUNG Mobile USB Composite Device with Status OK. ADB serial 4633424442303098 is authorized and online. Device is SM-G965U, Android 10/API 29, Live Ninja 0.3.15/code 21. KEYCODE_WAKEUP succeeded and MainActivity launch returned Status: ok; mResumedActivity confirmed Live Ninja. Only basic device access/control tested; no calls made.
- [ ] car-artifact-match — resolve the installed 0.3.15/code 21 artifact and commit before reproducing. Local Gradle declares 0.3.13/code 19; do not silently downgrade/install over the newer app.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat car-usb-access" exits 0. Required checks: "C:\Users\Jeremy\AppData\Local\Android\Sdk\platform-tools\adb.exe" -s 4633424442303098 get-state; installed artifact/commit match. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### car-repro — capture the exact failure before changing code
depends on: car-usb-access

- [ ] car-baseline — create a focused two-way audio check and record its exact command before implementation. On the target parked car, test assistant session start, car microphone uplink, car speaker downlink, and end. Record the active engine and selected/routed input and output with a fixed nonprivate test phrase at each end. Run it at least once and record the actual failure; failure to reproduce remains unverified.
- [ ] car-cause — after the baseline exists, test source-backed hypotheses one at a time: forced speaker on API 29/30; SCO/route lifecycle; transport/capture format differences; wake microphone/focus contention. Inspect the actual installed code/commit. Existing source forces speaker on API 29/30 in WebRTC, Gemini, and Nova paths; that fact is not yet the runtime diagnosis.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat car-repro" exits 0. Required checks: scripts\verify-phone-evidence.bat run-case car-baseline; the gate passes only when a valid red-capable baseline is recorded, not when missing hardware is mislabeled as a reproduced bug. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### car-route-fix — one route owner and verified cleanup
depends on: car-repro

- [ ] car-legacy-route — fix the reproduced API 29/30 standalone-assistant path using the appropriate legacy HFP/SCO connection lifecycle. Wait for observed connection, bound timeout, stop/release on failure or end, handle cellular interruption, and preserve user route choices. Do not confuse A2DP playback with two-way HFP.
- [ ] car-modern-route — on API 31+, observe available communication devices, successful selection, and route-change events; handle HFP/BLE where supported, disconnect/reconnect, and cleanup. Add only required permissions with API-level handling.
- [ ] car-audio-ownership — coordinate transport, wake microphone, audio focus, and the future Telecom calling owner. Never issue AudioManager routing commands while Telecom owns the call. Preserve existing echo cancellation unless measurements justify a specific change.
- [ ] car-regression-proof — extend the existing orchestration/voice tests and the correct device test seam. The original failing physical check must pass. Verify all shipped transport families affected by the change, without enabling disabled engines.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat car-route-fix" exits 0. Required checks: Android gradlew.bat :app:testDebugUnitTest :app:lintDebug :app:connectedDebugAndroidTest with ANDROID_SERIAL=4633424442303098; scripts\verify-phone-evidence.bat run-case car-regression-proof. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## car-call-testing — extensive device, head-unit, and real-call verification
depends on: car-route-fix, call-modes-acceptance, recording-app-access

Jobs: local-test-job, delivery-job, controlled-call-job, human-hardware-job. Watchers, exact restart commands, and shared ceilings are defined in Job supervision and restart policy; register concrete IDs before launch.
Authorization: explicit start plus locked account, spending, contact, retention and distribution scope as applicable; standing deployment authorization already applies.
Workstream done: every contained milestone gate exits 0; run "scripts\verify-phone-evidence.bat car-release-proof" to check the terminal gate and its dependency evidence.


### car-test-matrix — automated and physical coverage
depends on: car-route-fix, call-modes-acceptance, recording-app-access

- [ ] car-unit-matrix — test API 29/30 legacy routing and API 31+ routing; connection arriving late; refused route; missing permissions; route removal during capture; focus denied/lost/regained; wake mic contention; duplicate end; process/session cancellation; Telecom endpoint ownership; stale AI output after Take back. Existing tests first, new test classes only for new observable behavior.
- [ ] car-device-matrix — on the S9, run Bluetooth HFP and handset/wired checks. On a supported newer phone, run modern HFP and BLE if hardware supports it. Unsupported hardware modes are explicitly not covered, never silently passed.
- [ ] car-real-call-matrix — place controlled inbound and outbound Twilio calls through the parked car, covering human speaking, assistant Listen, Take over, Take back, and assistant-only continuation. Verify separate microphone uplink and speaker downlink with a test phrase at each end. Check remote party cannot hear muted user audio and hears no AI audio in Listen mode.
- [ ] car-failure-matrix — during controlled calls, test car/Bluetooth disconnect, headset/handset route change, phone lock/background, cellular interruption, network loss, remote hangup, and owner stop. Verify no duplicate dial, no dropped remote leg during an intended handoff, no persistent capture, and no orphaned paid call legs.
- [ ] car-soak — after basic success, run 10 connect/start/end cycles on the S9 and on the supported modern phone, plus one 20-minute controlled call with interruptions and Bluetooth reconnects. Use the locked budget; lack of budget is not permission to run unbounded calls.

Acceptance thresholds: 10/10 lifecycle cycles release their resources; every required mode has audible two-way evidence; zero duplicate calls, unintended microphone disclosure, AI speech in Listen mode, or orphaned call legs. Capture route-acquisition and handoff timing rather than invent a universal Bluetooth latency limit. Any failed criterion fails its scenario; preserve partial results and fix before repeating. Allow at most three attempts per failing scenario, with a classified change between attempts.



Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat car-test-matrix" exits 0. Required checks: Android gradlew.bat :app:testDebugUnitTest :app:lintDebug :app:connectedDebugAndroidTest; scripts\verify-phone-evidence.bat run-case car-real-call-matrix; scripts\verify-phone-evidence.bat run-case car-failure-matrix; scripts\verify-phone-evidence.bat run-case car-soak. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

### car-release-proof — publish only verified behavior
depends on: car-test-matrix

- [ ] car-help-release — update Help in the same feature commits, including Bluetooth setup, route choice, microphone state, call controls, and recovery instructions.
- [ ] car-ci-release — use existing GitHub Android Release workflow, select a valid version above the installed code 21, verify signature/hash and installed version without reading keys. Preserve existing AWS delivery and APK updates.
- [ ] car-installed-acceptance — repeat the key parked-car audio and controlled-call checks on the exact released artifact installed through the appropriate channel. Passing a debug build alone does not prove the release.





Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat car-release-proof" exits 0. Required checks: go test ./internal/webapp/ -run TestHelpDrawer -count=1; scripts\verify-phone-evidence.bat run-case car-installed-acceptance; GitHub/installed-version checks. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.

## recording-acceptance — verify final call audio and S3 playback
depends on: recording-app-access, call-modes-acceptance, hotel-results, car-release-proof

Jobs: controlled-call-job, local-test-job, delivery-job. Use the shared watcher/restart registry and ceilings.
Authorization: explicit start, locked test contacts and budget; standing deployment authorization applies.
Workstream done: "scripts\verify-phone-evidence.bat recording-live-proof" exits 0.

### recording-live-proof — recordings are part of every call acceptance test
depends on: recording-app-access, call-modes-acceptance, hotel-results, car-release-proof

- [ ] recording-live-matrix — after controlled inbound/outbound tests, listen to and download each S3-backed result in both app surfaces. Verify the known spoken phrase from each participant, chronological mode transitions, assistant takeover/take-back audio, duration, integrity, and no unintended private microphone/PIN audio. Verify all five hotel rows link to recordings or a truthful no-audio/failure reason.
- [ ] recording-cost-proof — measure recording minutes, temporary provider storage, S3 capacity/requests/download traffic, and any AWS transfer/processing charges in addition to existing telephone/Azure costs. Include storage volume at the chosen retention in the locked monthly spending rule.
- [ ] recording-release-proof — verify ingestion and playback on the final deployed backend and released Android artifact, not only local fakes. Keep missing or unverified recording cases open and visible.


Expected files: existing telephony callbacks, one scoped ingestion worker only if existing workers do not fit, internal/deliv and store/webapp routes, history/Jobs web and Android views, template.yaml lifecycle/IAM, relevant existing tests, contracts/api.md, and Help copy. No infrastructure mutation occurs during planning. No new credentials are assumed; verify existing Twilio API-key media permissions by name/access status before requesting any additional secret.

Definition of done: from C:\dev\live-ninja-plan-build, "scripts\verify-phone-evidence.bat recording-live-proof" exits 0. Required checks: scripts\verify-phone-evidence.bat run-case recording-live-matrix; go test ./internal/webapp/ -run TestHelpDrawer -count=1; final GitHub/installed-artifact/S3 checks. All acceptance criteria in the items above are mandatory. Android Gradle commands run in C:\dev\live-ninja-plan-build\android with the recorded ANDROID_SERIAL. Gate evidence records exact executed commands and their exit codes.


## Execution log

- 2026-10-18 — caller-number-admission and inbound-cap-regression verified. Old pre-answer update reproduced the exact unavailable-session goodbye under a provider-state fixture rejecting pre-answer TimeLimit; production evidence is provider_cap_failed/canceled before any PIN response, not an observed numeric21220 code. The revised requested flow removes PIN altogether: configured callers get recording consent; other callers receive only Hangup with no quota/model use. Consent claims once, validates signed current caller, sets the remaining provider cap, then mints and records. Obsolete enrollment API/scripts removed; existing stored private data left intact. `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` exits0,186 tests; independent41 inbound tests and10 activation tests pass. `node --test tests/web/unit/*.test.mjs` exits0,127 tests; `go test ./internal/webapp -run TestHelpDrawer -count=1` exits0,2.559s; diff check passes. Review found no blocker. Current deployed budget remains10/day with1.4 reserved and8.6 remaining from the failed call; no reset or extra grant. Read-only diagnostic39140531066 ended at its known historical manual-grant readiness check; routing and caller allowlist passed.

- 2026-10-18 — incoming-daily-budget delivered at code44196ded81644d972badf4f5c84ebfb12c40c60a: Deploy39115864752 and Phone39115864797 both completed success. Main tests/race/deploy/browser quality passed; Phone tests/Azure audio probe/idle release passed, dated trial skipped. Conditional private runtime policy installed for the existing primary caller, policyversion1/inboundconfigversion3; PIN unchanged. Transaction binds owner/profile, exact destinations and timezone presence, PIN enrollment version/shape, empty VOICE lock and both deletion barriers. Fresh deployed status: incoming enabledtrue/limit10/reserved0/remaining10/day2026-10-18/timezoneAmerica/New_York/expiresAt1792382400; manual grant0/spent0 and monthly allocation20 unchanged. No telephone call initiated and no incoming ledger fabricated. Final documentation push will be watched to terminal; immutable sanitized evidence is C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15\incoming-daily-budget-2026-10-18-final.json.

- 2026-10-18 — incoming-budget-release pushed code44196ded81644d972badf4f5c84ebfb12c40c60a to main. GitHub Deploy39115864752 monitored by /root/android_calling session62896; Phone39115864797 monitored by /root/phone_backend session93276. Both watchers use --exit-status and track failures and success, with60-minute ceiling. No dated trial execution or runtime policy write before successful deployment.

- 2026-10-18 — incoming-budget-reservation verified: old HEAD source loaded in memory fails the new exact admission regression with the reported unavailable-session goodbye; no source files replaced. New source: `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` exits0, 184 tests (root session95452,13.377s; backend session19531,11.681s). `node --test tests/web/unit/*.test.mjs` exits0,127 tests. Final `go test ./internal/webapp -run TestHelpDrawer -count=1` exits0,2.576s. `git diff --check` passes. Independent review found no blocking defect; midnight/month rollover, policy version, full reservation below monthly threshold, concurrent conflict and outgoing isolation are covered. Delivery begins after this focused commit; workflow IDs will be registered when GitHub assigns them.

- 2026-10-18 — incoming-daily-budget diagnosis: deployed IAM `owner_budget_status` returns HTTP200, day2026-10-18 America/New_York, monthly allocated20, manual daily grant0/spent0/remaining0, pausedReason waiting_approval, pendingReservation false. Existing primary caller match, active owner, enabled inbound, valid PIN enrollment and absent deletion/VOICE guards verified with projections/server-side COUNT; no credential values read. Existing signed start catches this reservation refusal and speaks the reported unavailable-session message.
- 2026-10-18 — incoming-budget-display: `node --test tests/web/unit/phonebudget.test.mjs` exits0, 4/4 pass; independent `go test ./internal/webapp -run TestHelpDrawer -count=1` exits0. Separate incoming display does not create outgoing grants. Reviewer ambiguity about $5 top-ups resolved by explicitly limiting that text to other calls; incoming allowance waits for midnight when less than1.40 remains. Backend and independent review agents are tracked as /root/phone_backend and /root/phone_verification; UI reviewer /root/android_calling completed. No local scheduled job or provider call started.

### owner-call-now — perform the newly requested controlled owner calls
- [x] owner-message-first-check — canonical active owner/currentdaygrant verified, only OWNER_MESSAGES enabled and only knownstop cleared atomically. Existing IAM send invoked exactly once for primary alias/request owner-audio-check-20261017-a. Call completed and recordingready; see owner-message-first-result. Inbound/PIN/routing were unchanged by this test.
- 2026-10-17 — first owner call submitted exactly once: request owner-audio-check-20261017-a, CallSid CA464ae8a63082ace4443c531013dc9145, acceptedHTTP200/providerqueued. Atomic runtime transaction enabled ONLY OWNER_MESSAGES and removed ONLY owner-stop-2026-10-16; inbound remainsfalse and number routing/PIN unchanged. Spoken microphone phrase is "microphone check, violet seven" (no car implication). Initial playbackfalse/recordingpending; no result inferred from queueing.
- [x] owner-message-second-check — cancelled before invocation when the user reported incoming-call failure and poor quality. No second call placed; focus moved to the requested Azure incoming conversation.
- [x] owner-message-first-result — request owner-audio-check-20261017-a / CA464ae8a63082ace4443c531013dc9145 completed, playbackCompleted=true, private recordingready207098bytes at21:32:07UTC. Poll session5481 exited0. Monthlyconservativeallocation20.00USD, dailygrant10/spent0, voice lockreleased; inboundstillfalse. This is prerecorded playback evidence, not human-hearing, conversational or car-audio proof.

### inbound-live-repair — connect the owned number to Azure Realtime and verify the reported failure
- [x] inbound-number-route-probe — exact live repro39072287697 attempt1 failed only on old/unavailable route; afteractivation the SAME read-only run attempt2 passed, with exact /inbound-owner/voice and/status POST callbacks, inboundReadytrue, owner/PINenabled, noactiveownedcalls andremaining10USD. Historical11200 forCA27c230da8b947c12cbe4beb603f75987 remains evidence of the old fault, not a new failure. No rawrequest/PIN/phone/secret output.
- 2026-10-17 — actual red loop: push-triggered `phone-test-preflight.yml` run39072287697 at67924d6, helper/unit/credential checks passed and read-only diagnostic failed specifically `BLOCKED: inbound_route_red`. IncomingCA27c230da8b947c12cbe4beb603f75987 matched allowedcaller; VoiceUrl path/unavailable, methodPOST; numberStatusCallback empty; Twilioerror11200; no application/trunkoverride or activeownedcall; PINenrolled, ownerEnabledtrue/inboundEnabledfalse, remaining10USD. No provider settings changed. Root/code/liveHTTP correlate /unavailable with missinghandler/404; no inboundledger/brokerinvocation. This is the user's actual failure, not a synthetic model error.
- Ranked predictions: wrongnumberroute is confirmed; correctvoice/statusroutes should reachPIN rather thanproviderapology. DisabledINBOUND is a separate confirmedgate; enablingafterrouteverification should allow authenticatedentry. Azurecoldstart issecondary (a15-secondPOST probe timedoutduringmin0/rollout); warmhealth beforetest and verify rolloutterminal. Model/contextfailure is not yet supported because no broker invocation occurred. Preserve currentAzuremodel/voice/PCMU until actualsessionevidence saysotherwise.
- 2026-10-17 — helper10 offline boundary tests and full `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` passed172 tests in7.918s. Root reviewed exact four-field routing write/readback, preservedSMS/application/trunk state, active-call checks and conditional owner/PIN/budget/deletion/voice-lock guards. Publish read-only probe first; no activation branch has run yet.
- [x] inbound-number-activation — authorized manual workflow39072670166 at67924d6 succeeded: exact four callback fields verified, SMS/application/trunk settings preserved, owner/PINenabled atomically with noactivecall andcurrentbudget10. Output activatedtrue/routeMatchestrue/callsMade0. No newnumbers, grants, outboundcalls, modelchanges orlocaldeployment. This supersedes earlier deferred telephone activation; car testing is still deferred.
- [~] inbound-azure-acceptance — user invited to call existing number, privately enter PIN, press1 for recording consent and ask a short question. Verify actual broker/media/transcript/recording and cleantermination within180seconds; no livevoice/human-quality claim before that evidence. Watcher80521/PID36836 includes calls since21:50UTC and has harddeadline22:03:14UTC; predecessor25560 stopped to avoid duplicate watchers. No inboundentry observed yet; no test call is originated by the watcher. Carproof is separate.
- 2026-10-17 — corehelperrelease67924d6 passed Main39072287637 and Phone39072287700 including fulltests, web-quality and realAzureaudio probe; no datedhoteltrial ran. App Help updated after activation to describe fresh ownerphoneconversations, keypadPIN, recordingconsent, readonlyphone tools,3-minute cap and telephoneaudio limits. `go test ./internal/webapp -run TestHelpDrawer -count=1` passed. Final Help/status push uses existingmainCI; exact terminalproof is stored at the final routing-evidence path above rather than recursively committing workflow results.
Outcome: eliminate the reported incoming applicationerror and reach the alreadydeployed AzureRealtime conversation. Non-goals: another prerecordedtest, hotelbatch, modelupgrade, newtransport/framework, provider-copy deletion orcarcontrol. Files: helper+existingworkflow/offlinetests; root plan.md/deploy.md/Help as applicable. Proof: actualreadonly routing/error repro, mutationfake tests, verifiedproviderreadback, then real bounded incoming-call evidence. Runtime pipeline max3attempts; classify failures and fix before retry; never redial ambiguous calls.
- Read-only baseline2026-10-17: no recentinbound ledger/brokerinvocation; recorder23events/noerrors; Azure /health200 ready=true, unsigned /owner-media403. ActualAzure account ln-aoai-eastus2/resourcegroup ln-azure-openai-rg reports deploymentgpt-realtime-2-1-mini → modelgpt-realtime-2.1-mini/version2026-07-07/Succeeded. These facts do not by themselves identify the failing Twilio route.
- Quality facts: firstoutboundtest usedplain<Say>; actualownerbridge requests PCMU and forwards originalbase64 withouttranscoding. TwilioMediaStreams requires8kHz monoμ-law; directappWebRTC is the higher-fidelity alternative. Preserve savedvoice; brokerdefault isCedar. Sources: https://www.twilio.com/docs/voice/media-streams/websocket-messages , https://developers.openai.com/api/docs/guides/realtime-conversations#voice-options , https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/realtime-audio-webrtc .
Outcome: place and verify the requested controlled owner calls now. Non-goals: parked-car proof, hotel calls, conversational/inbound activation, routing changes, new providers or new grants. Files: plan.md only. Proof: durable call IDs/status/playback/recording, budget readback and actual human feedback if supplied; provider completion is not proof the owner heard it. Invocation timeout unknown is reconciled from the same ledger row, never repeated under a new ID. End observation after5minutes and preserve any unresolved voice lock. Runtime operational changes use existing schemas/endpoints; no infrastructure deployment from this machine.

### parked-car-resumption — use the authorized allowance for controlled owner audio checks
- [x] parked-day-allowance — exact-day conditional grant verified2026-10-17: before granted0/spent0; after granted10TOTAL/spent0/remaining10USD. Timezone America/New_York; expiry1792296000 (2026-10-18T00:00:00-04:00). Receipt parked-car-2026-10-17-total-10; deterministic transaction token and day-version/canonical-active-owner/timezone/deletion fences preserve idempotency. Monthly allocation untouched. Owner-stop and both enabled=false flags remain unchanged; no calls or routing writes.
- [!] parked-native-baseline — Jeremy deferred the actual audio test with "I will check later". S9 Android10/app0.3.17/code23 and microphone permission verified. One-time session6920/PID29884 stopped:31 sanitized samples20:38:25–20:41:02UTC showed ADB online, no active HFP device, connection0/audio10; A2DP spot check also had no active device. No microphone/speaker observation or routing transition occurred, so this is not proof of an app defect. Jeremy must reconnect the actual test phone for car phone-call audio and provide a parked test window. No audio/calls were started.
- [ ] parked-owner-call-baseline — depends on: parked-native-baseline. Use only configured owner destinations and existing bounded paths; no hotel calls. Before any call, verify the exact owned-number callback, current PIN enrollment, active owner and budget. Existing owner-message path is90seconds/downlink-only; real inbound owner sessions are180seconds and require private PIN plus keypad recording consent. A new current-date activation workflow is needed if routing is not already correct; the October16 rehearsal cannot be reused.
- [!] parked-route-release — route never connected during the observation, so release cannot be measured. Sampler session6920 closed after intentional bounded stop; no background monitor remains. Requires an actual connected native test before acceptance.
- [x] inbound-status-contract — corrected unsupported per-call StatusCallback update: admission now sends exactly TimeLimit, and provider-signed number-level /inbound-owner/status binds canonical configured owner, inbound CallSid, direction and exact stored numbers before cleanup. Unknown/deleted calls create no records; duplicates and unrelated/owner-stop locks remain safe. `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` passed162 tests in9.059s; root reviewed both diffs and diff check passed. Files inbound_owner.py/test_inbound_owner.py; deploy.md documents both required future number callbacks. No provider routing, calling flags or calls changed. Sources: https://www.twilio.com/docs/voice/api/call-resource and https://www.twilio.com/docs/phone-numbers/api/incomingphonenumber-resource .
- parked-preflight-delivery — publish this correction and decision log through existing main/Phone workflows, watch all terminal outcomes (maximum3 attempts; fix deterministic failures before retry). Final immutable workflow/control/budget proof: C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15\parked-preflight-2026-10-17-final.json. Until that artifact reportsPASS, deployment of this correction is unverified. No call or routing activation is part of delivery.
Outcome: determine whether the shipped native app uses the parked car microphone and speakers, then test an owner telephone path within today's10USD allowance. Non-goals: Android Auto, hotel batches, a new calling architecture, general scheduling or unchosen provider-copy deletion. Initial files: plan.md only; any required activation workflow must reuse existing provider/OIDC handling and be recorded before editing. Proof: sanitized ADB observations plus explicit fixed-phrase human feedback, exact budget readback, terminal call/recording evidence if a call occurs. Existing evidence verifier remains fail-closed for missing observations. Stop a test for loss of parked readiness, an unknown call outcome or exhausted allowance; do independent preparation while waiting. No redial without a new deliberate test step. Record all process/workflow/call identifiers, commands and terminal results here.

### blocker-clearance — remove executable blockers after PIN enrollment
- [x] blocker-pin-check — /root/phone_backend verified projected enrollment version1 and server-side verifier-shape COUNT without retrieving secret values. PIN is enrolled; inbound and owner messages remain disabled; owner-stop-2026-10-16 is intact. October allocation19.70USD, October17 grant/spend0.00USD; the1.40USD reserve requires new daily authority.
- [x] blocker-session-cleanup — inbound_owner.end now finalizes the known AI session even if provider hangup fails, records unconfirmed cleanup and retains the paid-call lock. Regression failed before the fix (final_transcript call count0). `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` passed138 tests in11.163s. Root reviewed the diff; no runtime controls changed.
- [x] blocker-android-lint — existing SDK guard now exposes its API31 boundary to lint, preserving Android confirmation behavior. `gradlew.bat :app:testDebugUnitTest --tests '*UpdateTrustTest' :app:lintDebug --console=plain` exited0 in2m47s from C:\dev\live-ninja-plan-build\android:4 tests,0 failures/errors; lint0 errors,147 warnings. Job56840 terminal. No suppressions, dependency changes or APK version change.
- [x] blocker-dependency-review — existing assistant-only questionnaire transport can support phone-research Jobs without human/conference takeover. The missing seams are owner-approved immutable targets, durable dispatch/reconciliation and Jobs results/control. Conference proof remains required for takeover, but is not an implementation prerequisite for assistant-only Jobs. Deployed foundation entries were corrected to implemented/live-acceptance-pending. Aggregate MCP evidence still has no successful token exchange or tool completion; owner client sign-in remains unverified.
- [x] phone-research-offline-build — Go Jobs/worker/API, web review/results and recorder dispatch/callbacks are implemented. Root independently passed `go test ./internal/jobs ./internal/webapp ./internal/tools ./cmd/jobs-worker ./cmd/web ./cmd/account-purge -count=1` and matching `go vet`; `node --test tests/web/unit/*.test.mjs` passed126 tests; `go test ./internal/webapp -run TestHelpDrawer -count=1` passed. Backend full Python suite passed157 tests. Existing browser spec adds explicit five-target approval and no-call-on-save coverage; execution remains CI-gated. No calls or grants occurred.
- [x] phone-research-delivery — core feature6af7d95 passed Main39058868724 and Phone39058868725, including full Go/race/browser checks and real Azure audio probe. Live catalog/control checks passed; no calls made. Final editor-loading and scheduler-rule corrections use the same main pipeline, with exact terminal/runtime proof at C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15\blocker-clearance-39058868724-final.json. Maximum3 delivery attempts; failed deterministic checks require a fix before retry.
- 2026-10-17 — phone-research commit6af7d9575dc72a57d4d3a9bf801fd8240555beb7 pushed main. Main39058868724 watched by /root/phone_verification session50869; Phone39058868725 watched by /root/phone_backend session77699. Both attempt1of3,60-minute ceiling from18:36:49UTC. No Android artifact change in this release; prior0.3.17/code23 remains installed.
- 2026-10-17 — Phone39058868725 completed success at6af7d95:157 Python tests, Go bridge/race checks, recorder/Azure rollout, real Azure audio probe and idle release passed; trial skipped. Watcher77699 exited0. Live IAM catalog returned200 with20 distinct targets/numbers, exact release catalog, officialHTTPS provenance and canonical-owner media binding. Safe projections/COUNT verify PIN enrollmentv1, owner messages disabled, inbound disabled, owner-stop-2026-10-16 unchanged. No new Jobs, calls, grants, routing changes or provider-data mutations were performed.
- 2026-10-17 — root read-only AWS retention audit verified account759775734231,17 active regions and188 existing log groups, all7 days; no drift or runtime correction needed. New worker log-group retention remains a post-deployment verification.
- deployment-rule-correction: SAM transformed `Enabled: !If [...]` into literal `State: ENABLED` even though JobsSchedulingEnabled=false. Runtime worker guard stayedfalse and prevented generic work. Replace only that property with `State: !If [JobsSchedulerEnabled, ENABLED, DISABLED]`; installed SAM translator reproduced the old defect and preserves the corrected conditional. `sam validate --lint --template-file template.yaml --region us-east-1` passed. Official contract: https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/sam-property-function-schedule.html . Verify actual EventBridge DISABLED after final pipeline; no local rule mutation.
- 2026-10-17 — Main39058868724 completed success at6af7d95, including all Go/race tests, arm64/Nova builds, SAM deployment and browser/accessibility/SEO checks. Watcher50869 exited0. Read-only runtime proof: Jobs worker Active, JOBS_WORKER_ENABLED=false and all15 declared log groups retain7days. The final main push contains only the late-catalog UI fix, the SAM State correction, its browser regression and this status update; no phone calling controls change.
- late-catalog-editor: opening New job before the catalog arrived left hotel choices empty. jobs.mjs now fills the open editor on response and preserves current selection; existing browser test holds the response until the editor is open, then requires all choices and exact five-hotel approval. Focused Jobs Node tests12/12 and syntax checks pass; delayed-response browser regression must pass in final CI.
- phone-research review fixes: provider hangup must confirm terminal state before publishing cancellation; pending cancellation is reconciled on late SID callbacks; owner-stop is rechecked immediately before POST; stale provider commands cannot reverse newer owner control; historical completed-run reads survive job edits; late recording evidence can enrich terminal runs without reopening calls; notification failures cannot recreate deleted owner rows. Existing Android Jobs lacks the exact five-target review, so phone start/resume/create/edit require web; safe stop controls remain available. Model start/resume/retry proposals direct the owner to web Jobs.
- phone-research implementation contract: choose exactly five distinct entries from the existing20 California hotel catalog; display name/location/phone/source for owner review. Fixed Wi-Fi/VPN questions,180 seconds per call, one attempt per property, maximum7USD allocation (five1.40USD reservations); monthly and explicit daily grants still apply independently. The main JOB stores the immutable snapshot, owner approval, expiry and commandVersion. Recorder IAM operations catalog/advance/status reread the canonical active owner and exact saved JOB generation/control before dialing. Existing callbacks advance only approved running work; an IAM notification to the existing jobs-worker synchronizes results without enabling generic reminder schedules. Ledger stays in existing owner/run partitions for export/purge. Python files phone_research.py/test_phone_research.py plus existing recorder/owner helpers/deployment; Go Jobs/worker/web routes and exact IAM; existing web Jobs UI/tests. No new provider SDK or service.
- Deployment configuration review found14 declared log groups with5-day retention in template.yaml despite the required7-day policy. Set those declarations to7 during the Jobs worker configuration change so future deployments preserve the required retention. No alarms or dashboards were found.
- phone-budget-reservation: existing grant guard used0.30USD short-message reserve and stranded full1.40USD conversations when remaining allowance was0.30–1.39USD. Align grant eligibility with the1.40USD full-call reserve; require the same explicit authenticated5USD approval, same day and same idempotency protections. Root updates phonebudget.mjs, its existing test and Help to explain the reservation. `node --test tests/web/unit/phonebudget.test.mjs` passed3 tests. Backend adds threshold regression; no grant was issued.
- exact-job-recording-links: Jobs links include validated source/CallSid; History selects the source and locates only that call through existing authenticated pages (maximum10 pages per load, then explicit Load more). No media is fetched or played before explicit selection; existing cross-tab microphone guard remains. Files recording-history.mjs and its existing tests. `node --test tests/web/unit/recording-history.test.mjs` passed14 tests including exact second-page selection, bounded lookup and invalid-link rejection.
- 2026-10-17 — blocker fixes3042fd2cc15760bf2461f89dbbaa107e99bfe3da verified: Deploy39057352507, Phone39057352470 and Android39057352439 all completed success. Main included web-quality; Android included emulator tests; signed APK publication skipped as intended. No retries or phone calls.
- 2026-10-17 — lint/cleanup commit3042fd2cc15760bf2461f89dbbaa107e99bfe3da pushed main. /root/android_calling monitors Deploy39057352507, Phone39057352470 and Android39057352439 to terminal, deadline19:16UTC, shared maximum3 attempts. No signed APK republish is needed for unchanged installer behavior.
Outcome: verified PIN status and a passing Android lint gate, plus dependency-ready implementation supported by code evidence. Non-goals: no call activation, budget grant, provider-copy deletion, or Android Auto work without the applicable owner decision. Initial files: AppUpdateInstaller.kt, UpdateTrustTest.kt and plan.md. Root owns integration and existing GitHub delivery. Agents report terminal pass/fail; deterministic failures require a fix before retry, maximum three attempts per test/deployment job. Call and retention questions were asked together before dependent work.

- 2026-10-17 — corrected delivery verified: main39028551114 and Phone39028551161 succeeded atef808d78c002351bb6a4884fb93feccd6aeb0c37; final main39029116533 and Phone39029116502 succeeded at262586c75677ae901f9661c5c54fde4fa8512f17. Latest main completed all test/deploy/browser/accessibility gates; latest phone passed137 Python tests, Go race checks, recorder/Azure deployment and real audio probe, skipped trial calls and released idle capacity. All watchers closed. Read-only export again completed7 pages/118 rows with forbidden fields absent. Owner messages remain disabled, owner-stop retained and inbound PIN enrollment absent. No calls, purge, provider-data deletion or enrollment were executed.

- 2026-10-17 — corrected provider lifecycle committed asef808d78c002351bb6a4884fb93feccd6aeb0c37. Phone39028551161 completed success including136 Python tests, bridge race checks, recorder/Azure deployment/audio probe; trial skipped and owner stop unchanged. Main39028551114 deployment passed and final browser checks are monitored by /root/phone_verification (session96504).
- 2026-10-17 — named cleanup regression reproduced a lost known bootstrap when a post-mint persistence CAS loses to hangup/deletion. Local failure cleanup now retains the returned exact session capability without reminting. Regression proves exact session finalization and no recording POST; full Python suite137 tests passes. No other speculative scope added.

- 2026-10-17 — recording provider-state correction complete locally: completed media uses verified DELETE; absent/deleted means no media; processing stays pending. Uncertain recording creation retains call/callback state until a signed late callback resolves it under deletion markers. Explicit not-requested/rejected states and confirmed no-answer/busy/canceled calls do not wait for nonexistent audio. Tombstoned journal writes return410 so bridges cannot treat discarded intent as approval. Both deletion-before-intent and deletion-after-intent/pre-POST paths persist safe no-call outcomes; uncertain submitted calls remain blocked. No live provider action was run.
- 2026-10-17 — final correction checks: `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` exit0,136 tests; `go test ./cmd/phone-trial-bridge ./cmd/account-purge ./internal/webapp -count=1` exit0; `go vet ./cmd/phone-trial-bridge` exit0; `git diff --check` exit0. These fixes replace the earlier incomplete provider-state assumptions; current deletion behavior remains tested with isolated fakes, not real data deletion.

- 2026-10-17 — evidence commit067abf53789248d26721d16d5859b74e6f81c57d deployed successfully via39026998581; all jobs terminal success, watcher97037 exit0. Final provider-contract review then confirmed that only completed Twilio media can be deleted; absent/deleted states need no media DELETE, and processing must remain pending. An empty recording list does not resolve an uncertain create. Existing lifecycle implementation is being corrected and retested without any live deletion. Primary source: https://www.twilio.com/docs/voice/api/recording#delete-a-recording . Twilio keeps deleted media metadata40 days; Help/contracts now state that limit.

- 2026-10-17 — all code deliveries terminal success: main39025863230/34eff07ce86b89c18996233f3af12daeb4ad4b6f and39026045269/83aa4149ab4fa21417078f63c9feb897c1f0ff26; phone39025863237/34eff07 and39026045322/83aa414. Latest main includes complete Go tests/vet/race, arm64 builds, SAM, browser/accessibility/Lighthouse success. Latest phone includes120 Python tests, Go race tests, recorder/Azure deployment, real model-audio probe, skipped telephone trial and idle scale-down. Android release/install proof is recorded above. All watcher sessions are closed.

### remaining-execution-gates — only incomplete work stays open
- [!] provider-copy-retention — Jeremy must choose whether the extra Twilio audio copy is deleted after verified S3 persistence or kept180 days. This blocks only normal provider-copy cleanup, not the implemented account-delete cleanup. Current in-app S3 expiry remains180 days.
- [!] inbound-live-acceptance — PIN verified; controlled-call resumption authorized and10USD total grant recorded for2026-10-17. User deferred the actual parked test. Need ready operator/device, corrected status-callback contract and exact owned-number routing verification before activation. Grant expires at local midnight; it is not authority for another day. No routing change or call occurred.
- [!] expanded-call-topology — user deferred the real parked experiment after authorizing controlled owner tests for2026-10-17. Dialer/listen/takeover/take-back still require a concrete conference transport and measured media proof. Assistant-only phone-research Jobs are already delivered independently; a hotel batch still requires exact target review and is not authorized by the parked-owner-test instruction.
- [!] bluetooth-physical-proof — Jeremy must provide the parked Suburban test window and verify actual microphone/speaker audio. S9 USB access, signed installation and existing Bluetooth unit tests pass; vehicle acoustics and full recording playback have not been physically verified.
- [x] android-existing-lint — resolved by blocker-android-lint on2026-10-17; focused update tests and lintDebug pass with0 errors. Installation behavior is unchanged.
- [!] generated-cache-cleanup — automatic execution policy rejected deletion of `tools/phone-trial/__pycache__/account_lifecycle.cpython-314.pyc` with reason `blocked by policy`; the generated file is not committed. Local operator cleanup can remove it outside this blocked execution path.
- Android Auto is deferred exclusively to backlog.md; no Auto implementation, Play publication or Auto hardware acceptance was performed.

- 2026-10-17 — latest Phone39026045322 at83aa4149ab4fa21417078f63c9feb897c1f0ff26 completed success, real Azure audio probe passed, trial skipped, idle release passed. Unauthenticated inbound voice/redeem requests each returned403. Consistent projections confirm owner messages disabled, owner-stop-2026-10-16 retained, inbound configuration absent. Root IAM broker probe with a nonexistent owner returned403/owner_required and no session token; no quota session or call created.
- 2026-10-17 — live read-only account export check:7 pages,118 projected rows, sources hotel-trial-2026-10-15=109 and messages=9. No forbidden credential/PIN/raw-number/bootstrap/link fields, no repeated cursor; source complete. No export artifact, signed media URL, purge or enrollment generated. Both web and account-purge runtime envs point to the exact us-east-1 recorder ARN.

- 2026-10-17 — Decimal wire correction83aa4149ab4fa21417078f63c9feb897c1f0ff26 pushed; main Deploy39026045269 and Phone39026045322 registered (attempt1of3 for corrected release,60-minute deadline). Both are monitored after preceding deployments, with no cancellation or local deployment. Phone39025863237 completed success including model-audio probe; dated trial skipped.

- 2026-10-17 — inbound/privacy commit34eff07ce86b89c18996233f3af12daeb4ad4b6f pushed main; Deploy39025863230 and Phone39025863237 tracked by /root/phone_verification and /root/phone_backend,60-minute deadlines, attempt1of3. Root found actual DynamoDB Decimal bootstrap deadlines would serialize as fractional JSON and fail Go int64 decoding; serializer now preserves integral values, with a real-Decimal regression. Full Python suite120 tests passes. Calls stay disabled throughout both deployments.

- 2026-10-17 — final inbound/lifecycle local integration: `go test ./cmd/web ./cmd/account-purge ./cmd/authorizer ./cmd/realtime-broker ./cmd/phone-trial-bridge ./internal/realtime ./internal/tools ./internal/webapp -count=1` exit0; same packages `go vet` exit0. `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` exit0,119 tests. `node --test tests/web/unit/*.test.mjs` exit0,118 tests. PowerShell evidence verifier exit0,38 synthetic checks. Owner media tests cover immediate clear/close before slow cleanup, late created response cancellation, scoped tool/transcript wire, token budgets/malformed usage/duplicate responses and sink failures. Action-bound MAC rejects cross-action replay.
- 2026-10-17 — account lifecycle tests cover owner-bound export, all S3 versions, provider identity and strict already-gone responses, in-flight transfer barriers, callback-after-deletion, exact lock ownership, and retry after partial callback metadata deletion. No live deletion performed. Phone enrollment only stores a salted verifier and leaves inbound disabled. Fresh account status and logout watermark checked during call heartbeat.
- 2026-10-17 — Android push run39024286735 attempt2 completed success; all30 instrumented tests passed. Both Android pipelines are terminal success; no tests skipped to repair the transient failure.
- 2026-10-17 — automatic execution policy rejected cleanup of generated `tools/phone-trial/__pycache__/account_lifecycle.cpython-314.pyc` with reason `blocked by policy`. File is excluded from staging. No alternate deletion path attempted by root.

- 2026-10-17 — Android publication39024285982 success, email step skipped. Public manifest0.3.17/code23 atf2a72a48dd8af99a34888adaa8ec873d7179d132 independently verified by root. APK SHA2561915e1819f2a87052cec6e8eb73b8cc66481e66dc028c1d2055553733ae8ca84; signer SHA2563d6c8b91dddd1e14a8fa457aa4cddae8fdce2eda218d5d381a5ca7d1eeb6b39d matches installed. Agent adb install -r returnedSuccess; installed version readback0.3.17/code23. Push test39024286735 had existing JobsComposeTest SnapshotStateObserver thread race; same-SHA dispatch passed full emulator suite. Classified transient,30-second backoff then gh run rerun39024286735 --failed, attempt2of3; monitored by /root/android_calling.

- 2026-10-17 — recordings code f2a72a48dd8af99a34888adaa8ec873d7179d132: Deploy39024286733 and Phone39024286790 completed success. Main includes full Go/vet/race/browser/accessibility gates; Phone real Azure probe passed and trial job skipped. Public audio-access.mjs returned200 with capture lock implementation.

- 2026-10-17 — read-only runtime preflight: AWS account759775734231; all17 active regions,188 log groups retain7 days, no corrections. CONFIG/OWNER_MESSAGES enabled=false, PHONE#CONTROL/VOICE activeRequest=owner-stop-2026-10-16, CONFIG/INBOUND_OWNER absent. Verified broker physical name live-ninja-realtime-broker and Azure ingress live-ninja-phone-trial.greenmeadow-28b6b789.eastus2.azurecontainerapps.io. No provider configuration changed.

- 2026-10-17 — recordings commit f2a72a48dd8af99a34888adaa8ec873d7179d132 pushed main. Tracked runs: Deploy39024286733, Phone39024286790, Android39024285982 and39024286735. Android publish dispatched with notify_email=false. Root monitors Deploy/Phone; /root/android_calling monitors both Android runs, verifies exact release hash/signature and installs only the matching signed APK if USB remains available. Deadline60 minutes, attempt1 of3.

- 2026-10-17 — Android job 45895: assembleDebug passed; lintDebug failed at unchanged AppUpdateInstaller.kt:123 (NewApi: setRequireUserAction requires API31). No recording-feature lint errors reported. Existing updater code is outside this change and no check was disabled. Release verification continues through the repository workflow.

### recordings-local-proof — 2026-10-17 build and test evidence
- Main baseline 9e74a845a6ff9e61061b091af6192a1df0041510; original dirty worktrees preserved. Agents: /root/phone_backend, /root/android_calling, /root/phone_verification.
- `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` exited 0: 89 tests.
- `powershell.exe -NoProfile -NonInteractive -File scripts/verify-phone-evidence.tests.ps1` exited 0: 38 checks, synthetic only.
- `node --test tests/web/unit/*.test.mjs` exited 0: 118 tests. `node --test tests/web/unit/recording-history.test.mjs` exited 0: 11 tests, including cross-tab exclusion and pending-response races.
- `go test ./internal/webapp ./internal/tools -run 'TestPhone|TestOwnerPhone|TestHelpDrawer' -count=1` exited 0; final Help guard repeated after copy change, exit 0.
- Android `gradlew.bat :app:testDebugUnitTest --tests '*PhoneRecordingsTest' --tests '*CarAudio*Test' --console=plain` exited 0: 65 tests. SDK environment normalized for this command only. Build/lint job 45895 monitored by /root/android_calling, 45-minute deadline, attempt 1 of 3; source test jobs 28134 and 46225 terminated successfully.
- Release candidate 0.3.17/code 23 follows verified stable 0.3.16/code 22. Existing Android workflow gets an optional email input preserving its default; this run dispatches notify_email=false because no new operator email was requested.
- S9 USB access now works; installed 0.3.15/code 21 read back. Physical playback/car audio still unverified. Inbound code remains incomplete: authenticated broker phone surface, PIN enrollment and owner-session bridge are required; missing PIN prevents activation, not code development. Live topology remains blocked by the existing explicit call stop.
- Web Locks reference: https://www.w3.org/TR/web-locks/ . Shared microphone locks preserve wake/conversation capture; private recording URLs load only with exclusive access. Closing clears media before unlocking. Unsupported browsers can download but cannot play inline.


### phone-web-foundation — budget controls, private recordings and prompt quality

- Phone backend workflow 38926826412 (c11da61c) completed success; dated trial job skipped, no calls. Main workflow 38926826437 completed success, including browser quality checks.
- Authorization follow-up commit 8af7457220643b17a2eb0c10e82a104f5b7fbfa1 uses existing strongly consistent owner reads. Main workflow 38927291929 completed success at 2026-10-16T16:07:34Z: tests/races, SAM deploy, Nova bootstrap and web-quality passed.
- Production phonebudget.mjs and recording-history.mjs both returned 200 and exactly matched release contents. Unauthenticated budget/recording API reads returned 403; recorder HTTP grant injection returned 404; wrong-owner IAM recording list returned 403.
- Post-deployment DDB read verified enabled=false, owner-stop-2026-10-16 still held, and only the explicit hotel-trial-2026-10-15 legacy recording source bound.
- One bounded adb reconnect changed Samsung state from offline to unauthorized. Owner must unlock the phone and approve USB debugging. No pairing reset, install, or UI input occurred. Evidence: C:\dev\live-ninja-test-evidence\plan-run-2026-10-16\android-reconnect.json.
- Operator blocker email confirmed SES MessageId 010001a14569affb-a924f787-7163-4d54-bcda-208c12aa9a9b-000000. The delegated prior attempt lost its response and may also have delivered; parent completed the required receipt verification with a direct send.


- Published c11da61ca730ac8c23f5e28daa5408cf100bc4c7 to main. Phone workflow 38926826412 and app workflow 38926826437 started.
- Live IAM-only budget read: HTTP-style status 200, allocatedUSD=19.7, dailyGrantedUSD=0, pausedReason=disabled. No grant submitted.
- Live owner-bound legacy recording list: status 200, 13 records, 4 ready. Private playback link expiresIn=300; S3 Range request returned 206, audio/mpeg, 64 bytes. No URL retained in evidence or logs; no audio played.
- Follow-up uses existing GetUserConsistent for both new owner authorization boundaries. go test ./internal/webapp/ ./internal/store/ -count=1 passed; no new abstraction or store API.

- Implemented fixed $5 daily grants for owner messages, local-day expiry, no pre-stacking, atomic cost reservation and owner-only web controls. Grants preserve disabled configuration and the durable owner-stop. Dated hotel trials retain their stricter $20 cap. Multi-leg accounting remains dependent on call-topology-proof.
- Implemented owner History recording list/play/download through the existing Lambda/S3 path. Explicit legacy source binding compares canonical owner transactionally. Every new five-minute media link requires fresh owner access; existing issued links can survive revocation for at most five minutes. Native Android/Jobs integration and cross-tab microphone exclusion remain open.
- Fixed trial prompts use Polly.Joanna-Neural en-US. Existing consent wording/state remain unchanged; speech overhead allowance is $0.25 inside the existing $1.40 reservation. No listener test or telephone call occurred.
- go test ./... -count=1: exit 0, all Go packages passed.
- go test ./internal/webapp/ -run "TestHelpDrawer|TestPhone" -count=1: exit 0.
- go vet ./internal/webapp/ ./cmd/phone-trial-bridge/: exit 0.
- C:\Users\Jeremy\.cache\codex-runtimes\codex-primary-runtime\dependencies\python\python.exe -B -m unittest discover -s tools/phone-trial -p test_*.py: exit 0, 82 tests passed.
- node --test --test-reporter=tap tests/web/unit/*.test.mjs: exit 0, 112 passed, 0 failed, 0 skipped.
- powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File scripts/verify-phone-evidence.tests.ps1: exit 0, 36 checks; synthetic fixtures only, no provider calls.
- git diff --check: exit 0. Deployment and runtime checks follow in this log; no broad live milestone is marked complete from these local results.


### plan-implementation-start — resume authorized independent work

Outcome: implement authorized independent plan work while preserving stopped calls. Non-goals: no new call, caller routing change, speculative parallel bridge, or unverified car release. Files: existing bridge/recorder, owner web budget and recording access, evidence scripts, Help and this plan. Proof: focused Go/Python/browser/evidence checks and terminal GitHub delivery; live milestones remain blocked where required evidence cannot be obtained.

- 2026-10-16: clean main checkout C:\dev\live-ninja-plan-build at 9f2465bea412c43261c909feb6ebc49cc2616ffd. Original checkout has unrelated uncommitted work and is preserved.
- Parallel agents: plan_audit owns evidence tools; budget_controls owns IAM budget admission; car_audit owns neural prompts and authenticated budget UI. Root owns integration, plan, Help, verification, and delivery.
- Current main already contains dialog.go question progression and TestRehearsalDoesNotRepeatQuestions. Do not add a second dialog engine. Listener verification remains blocked by the call stop.
- Historical inputs (2026-10-16), updated 2026-10-17: fresh owner conversation selected; private PIN setup, newer Bluetooth test phone and parked Suburban window remain. Android Auto/Play inputs are deferred to backlog.md.
- All live acceptance remains unchecked. No synthetic receipt can satisfy a provider or human-audio gate.
- Read-only AWS checks confirmed canonical owner equals phone-config owner; CONFIG/OWNER_MESSAGES enabled=false and PHONE#CONTROL/VOICE activeRequest=owner-stop-2026-10-16. No grants or calls were created.
- Android preflight: Samsung S9 serial 4633424442303098 is offline in adb; getprop/package checks exit 1. Owner must reconnect/unlock USB debugging. Evidence: C:\dev\live-ninja-test-evidence\plan-run-2026-10-16\android-preflight.json.
- Existing CarAudio unit tests: 59 passed. Existing Android lint fails AppUpdateInstaller.kt:123 NewApi (setRequireUserAction requires API 31; minSdk 29). No Android source was changed in this run. Report: android/app/build/intermediates/lint_intermediate_text_report/debug/lintReportDebug/lint-results-debug.txt.
- Evidence run-case validates captured scenario evidence only; it never originates a call. Scenario execution and human audio observation must be recorded separately under explicit call authorization.


- 2026-10-15 — car research delegated to android_car_research, completed as docs/android-auto-research.md; primary agent added source inspection and device evidence. Windows USB status OK; adb devices -l shows 4633424442303098 device. getprop reports SM-G965U / Android 10 / API 29; package reports 0.3.15 / 21. KEYCODE_WAKEUP exited 0 and mWakefulness=Awake. MainActivity launch exited 0 with Status: ok; mResumedActivity confirms Live Ninja. No APK installed, call placed, or audio-fix code changed. Added Bluetooth/Android Auto work and extensive controlled-call tests; physical car audio remains untested.

- 2026-10-15 — final helper verification: "cmd.exe /d /c scripts\setup-phone-credentials.bat --check" exited 1 with the expected missing-GitHub-login message, before input or writes. "git diff --check -- plan.md scripts/setup-phone-credentials.bat scripts/setup-phone-credentials.ps1 scripts/setup-phone-credentials.tests.ps1" exited 0. All three new files passed an explicit trailing-whitespace check. Plan readback matched. No commit/push because the phone feature remains on hold and pushing triggers deployment.

- 2026-10-15 — added hotel-internet-job, five-property evidence/report criteria, and the explicitly requested credential helper. Preserved existing user-owned Jobs work. Hidden local checks verified origin JeremyProffittOrg/live-ninja; gh secret/variable inventory could not run because gh is unauthenticated. "powershell.exe -NoLogo -NoProfile -NonInteractive -File scripts/setup-phone-credentials.tests.ps1" exited 0: "PASS: stdin roundtrip and 11 offline credential checks; no provider calls." Read-only helper -CheckOnly exited 1 before prompts: "STOP: sign in locally with: gh auth login --hostname github.com". No secrets were entered or changed.

- 2026-10-15 — expanded the draft with app calling, silent assistant listening, takeover/take-back, and assistant-initiated calls. Read existing web mic ownership and official Twilio SDK, conference, Call resource, and stream documentation. Added a required topology proof and per-mode cost checks. Only plan.md changed; no implementation or calls started.

- 2026-10-15 — phone-access draft added after reading repository guidance, deployment configuration, relevant realtime/tool source, and official provider documentation. Only plan.md edited. No implementation, provider setup, tests, commit, push, or deployment started. Draft read back and content verified in-process without launching local commands.

- 2026-09-22 — spoken smokes bypassed. Commit `88c808f`. Deploy `35716826005` success.
- 2026-09-22 — completed history moved to `completed/plan-2026-09-22.md`. This file is only the unfinished build.
- 2026-09-22 — `broker-route-close` passed. Command: `go test ./cmd/realtime-broker/ -count=1`. Output: `ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.578s`. Exit 0. No handler change.
- 2026-09-22 — `voice-catalog` quoted 34 ids from https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live-how-to section "Supported voices". Older notes said 35. That count was one high. Default `ava`. No picker, so Help was not changed. `gpt-live-azure` stays on `SupportedVoices` / `cedar`. `go test ./internal/realtime/ -run "Voice|Catalog" -count=1` ok. `go test ./internal/webapp/ -run TestHelpDrawer -count=1` ok. `go test ./... -count=1` exit 0. Pushed as `d18202d2303b8c3c81c300c1d5862507f25a851f`. Deploy run `35737704441` succeeded. Android Release run `35737704501` failed in `Instrumented tests (emulator)`: `SettingsRevampComposeTest > hostPickerKeepsUnsupportedHostVisibleAndExplicitlyDisabled` with `RootViewWithoutFocusException` and `Failed to start Emulator console`. JVM unit tests passed. That test does not read the voice catalog.
- 2026-09-22 — `iot-signed-authorizer` pushed as `1babb949129073169198a034a7aa1ce83350074e`. Deploy run `35739047890` succeeded. `aws iot describe-authorizer --authorizer-name live-ninja-iot --region us-east-1` returned `status ACTIVE`, `signingDisabled true`, `tokenKeyName null`. `aws iot describe-authorizer --authorizer-name live-ninja-iot-signed --region us-east-1` returned `status ACTIVE`, `signingDisabled false`, `tokenKeyName token`. `go test ./internal/webapp/ -run IoT -count=1` ok. `node --test tests/web/unit/liveevents.test.mjs` 18 pass.
- 2026-09-22 — `android-signature-release`. Version bump `5c00ff5dd936b71bbc8dacd039cc30c34ccbd33f` is versionCode 18, versionName `0.3.12`. Deploy run `35740023211` succeeded. Android Release `workflow_dispatch` run `35740036332` `build-and-publish` succeeded. `https://live.jeremy.ninja/v1/app/android/latest` returned `versionName` `0.3.12`, `versionCode` 18, `gitSha` `5c00ff5dd936b71bbc8dacd039cc30c34ccbd33f`. APK sha256 matched `e3f8b812e8f7679a92a26fe4810c3954fe8469bd2499d917eaab29ca6b131169`. `adb -s 4633424442303098 install -r` printed `Success`. `adb -s 4633424442303098 shell dumpsys package ninja.jeremy.liveninja` printed `versionCode=18` and `versionName=0.3.12`.
- 2026-09-22 — `emb-retire` started early. The operator said "perform the emb-retire early." `ENT#` rows stay. `rg -n "EMB#|Cosine\(" internal cmd` printed nothing. `go test ./internal/memory/ ./internal/store/ ./internal/tools/ ./internal/webapp/ ./internal/rca/ ./cmd/account-purge/ -count=1` passed. `go test ./... -count=1` failed only `TestGoldenRCAPrompt` until the golden prompt was re-recorded; `go test ./internal/rca -run TestGoldenRCAPrompt -count=1` then passed. Titan `bedrock:InvokeModel` statement for `amazon.titan-embed-text-v2:0` removed from `template.yaml`. `MaxEmbeddingsPerUser` deleted.
- 2026-10-01 — `rules-store-tools`, `current-location`, `rules-ui-help` built by persona agents; security-qa review fixed 5 defects. `go build ./... && go vet ./... && go test ./... -count=1` printed no FAIL lines. Android `:app:testDebugUnitTest :app:assembleDebug` EXIT=0.
- 2026-10-01 — commit `b118e0c`. Deploy `36933592432` success. Android Release `36934390733` success: `/v1/app/android/latest` shows `0.3.13` / `19`. S9 `4633424442303098` `adb install -r` Success, `versionName=0.3.13`. Logcat `AppUpdate: update check (FOREGROUND): installed=19 remote=19`. Unauthenticated `POST /api/v1/location/current` returns 403 (route live behind auth).



- 2026-10-15 — applied Claude planning rules from the global and repository CLAUDE.md files. Archived completed pre-telephony work as completed/plan-2026-10-15-history.md. Consolidated task/dependency map, acceptance gate interfaces, per-job watchers/restart ceilings, and root stop conditions. Added required S3 recording/app access and the $20 monthly/$5 daily grant control. Moved Play test distribution before physical Android Auto acceptance. Recorded all four operator replies verbatim; friendly recording permission and remaining hardware/access gaps are explicit. No feature code, build, deployment, or call started.

- 2026-10-15 — today-phone-trial commit ffd2eae7b2800c6ab815250edff66d5b0ba92db6 started workflow 38733305985. Test job passed including race checks. Recorder deployment failed because the legacy artifact bucket is us-east-2 while Lambda is us-east-1. Fixed deploy_recorder.py to resolve the existing us-east-1 SAM SourceBucket, verify region, and recover only a failed trial stack whose retained table is proven empty. No calls started. Retry ceiling: deployment attempt 2 of 3. All 17 active AWS regions passed the mandatory 7-day log retention check.

- 2026-10-15 — workflow 38734141507 completed recorder deployment; its unsigned /report returned 403 and an invalid download token returned 404. Azure environment creation outlived the 900-second CLI wait; the environment subsequently reached Succeeded with no deployment error. The next attempt reuses that ready environment instead of recreating it. Added failed-call/stream canary stops, complete recording length/SHA256 checks, and concise duplicate-safe email output before any calls. Focused verification: Go tests passed and 29 Python tests passed. Azure host retry 2 of 3; recorder already succeeded. No hotel call has started.

- 2026-10-15 — workflow 38738647654 deployed the recorder and healthy Azure bridge, but the real Azure-only probe returned no audio under its special 32-token limit, so the hotel-call job was skipped. Probe now uses the same 512-token bound as real calls and requires non-silent PCMU, a transcript, and a completed response; safe status/reason diagnostics remain on failure. Go regression tests passed. This is audio-probe attempt 2 of 3; infrastructure is already verified. No hotel call has started.


- 2026-10-15 13:30 UTC — run 38741739636 at a5988f2faced13cdca959308d724ddc4b320e142: test and deploy jobs success. Deploy job 116445331887 log: "2026/10/15 13:23:40 Azure audio probe passed". Trial job 116446902974 is waiting for 09:00 America/Los_Angeles (16:00 UTC / noon America/New_York); zero hotel calls and no report email yet. Normal Deploy run 38741739768 success. Independent official-source verification matches all 20 targets; SHA256 41b49f81001d36abcf4113679746c5387924afcdb96eac92b141252298e874fe matches tools/phone-trial/targets.json. Evidence: C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15\hotel-target-verification.json. Monitor agent /root/phone_trial_recordings tracks calls, recording readiness, failure, and SES MessageId; parent retains repair responsibility. This is readiness evidence, not completed-call evidence.


- 2026-10-15 21:27 UTC — user confirmed: "Run the calls now". Earlier run 38741739636 failed before any call ID: HTTP 400; CONTROL attempted=0, one uncreated hotel-01 intent. Email failed before SES on float count. Repair normalizes email counts, emits only numeric provider error codes, and reconciles uncreated intents against Twilio history before retry. Unknown/nonempty provider history still blocks. 31 Python tests passed. Resume through main workflow immediately within today's open window; preserve budget, consent, one-call concurrency and no duplicate calls.


- 2026-10-15 21:34 UTC — repair commit bfad48400858b01f6d59d0fe5c5bc7c76dfd0aba passed 31 Python tests, Go race/vet and real Azure probe; workflow 38810244191 deploy success. Provider-history reconciliation found no prior call. Trial returned "Trial terminal: 0/20 attempted. Trial interrupted: HTTP 400 provider code 21216. Reconcile before retry; do not redial." Twilio https://www.twilio.com/docs/api/errors/21216 describes an outbound provider restriction (possible customer-profile, regulatory, or destination restriction); exact account reason is not verified. Do not try other numbers to evade the restriction. Owner/Twilio must clear it. Email step succeeded with SES MessageId 010001a1417c8bd4-a714ede5-5056-4ff1-8bbf-7f00bd0ca1fb-000000; parent independently read job 116683299678 logs. Zero calls, zero recordings. Azure cleanup succeeded. Execution attempts=2; one bounded resume remains only after the restriction is cleared and within the authorized date. No automatic reschedule.


- 2026-10-16 — owner test CAb94e13e6e957caf39001548c9f8df45e completed for 19 seconds; recording ready in private S3. Separately authorized hotel call CA1bf192ae28df1e515261715c96fd2bca completed for 15 seconds; consent answer unknown, no recording or transcript, no Wi-Fi findings. Twilio restriction is cleared for these destinations. No general renewed 20-call authorization was inferred.
- 2026-10-16 — ivr-navigation local verification: go test ./cmd/phone-trial-bridge -count=1 exit 0; go vet ./cmd/phone-trial-bridge exit 0; go test ./internal/webapp/ -run TestHelpDrawer -count=1 exit 0; python -B -m unittest discover -s tools/phone-trial -p test_*.py exit 0 (33 tests). Deployment pending; no new calls made by this workstream.


- 2026-10-16 — IVR release 3dec66ba0d816c2d3e83ce2f9e04362529612190: CI tests and deployment completed; Azure-only probe attempt 1 returned handshake HTTP 0 before any HTTP response. Job retry 2 of 3 requested after backoff; no call job permitted on the expired trial date. Caller allowlist values moved out of the public plan into the private local decision record; original commit history is unchanged.


- 2026-10-16 10:16 UTC — ivr-navigation release verified: workflow 38885171231 attempt 2 deploy/test success; expired trial job skipped. Real Azure audio probe passed in job 116923238208. Normal app Deploy 38885171200 success. No additional hotel calls. Phone-menu flow remains live-call unverified; mock signed callbacks cover menu -> hold -> person -> consent -> recording, and all navigation bounds. Historical inbound session question was resolved on 2026-10-17: start a new conversation; PIN setup remains pending. Caller numbers removed from latest public plan but remain in earlier commit history; history not rewritten.

## today-phone-trial — execute the dated twenty-call run and report evidence
depends on: verified trial credentials and real Azure audio probe

All commands in this workstream run from C:\dev\live-ninja-phone-run, the isolated main checkout.

Active scope authorized 2026-10-15: twenty distinct California hotel attempts using the friendly AI/recording permission request, one call at a time, 180-second hard call limit, no automatic redial, the $20/month rule and explicit new $5 grants when needed. Targets are tools/phone-trial/targets.json in clean checkout C:\dev\live-ninja-phone-run; original uncommitted work in C:\dev\live-ninja is preserved. Trial uses 09:00–17:00 America/Los_Angeles today, with a bounded GitHub job waiting before credentials are assumed. No local scheduled task.

Outcome files: private S3 recordings, durable trial journal, and HTML SES report to proffitt.jeremy@gmail.com from Jeremy Proffitt <jeremy@jeremy.ninja>, Reply-To proffitt.jeremy@gmail.com. Seven-day capability links exchange for short-lived S3 downloads because STS-signed S3 URLs cannot outlive the signing credentials. Link issuance occurs at report generation; files retain the accepted 180-day S3 lifecycle. No false recording link for refusal, no answer, or failed audio.

### today-trial-readiness — verify credentials, tests and deployed services
depends on: none
- [x] trial-credentials — GitHub Phone test preflight verified all five configured names, active non-trial Twilio account, and two voice-capable numbers. No values printed.
- [x] trial-oidc-binding — exact subject verified from GitHub: repo:JeremyProffittOrg@299835367/live-ninja@1303872500:ref:refs/heads/main. New Azure app live-ninja-phone-github has Contributor only on rg-liveninja-azure-prod; no client secret.
- [x] trial-offline-tests — 20 Python recorder/runner tests passed; bridge go test/go vet/Linux build passed; workflow adds race tests.
- [x] trial-deployment — push only isolated trial files on main; GitHub deploys AWS recorder and Azure bridge; run real Azure PCMU/audio probe before telephone calls.
Definition of done: Phone trial 2026-10-15 workflow test and deploy jobs both succeed; go test -race ./cmd/phone-trial-bridge -count=1 and python -B -m unittest discover -s tools/phone-trial -p test_*.py -v exit 0; real -probe returns nonempty Azure audio.

### today-trial-calls — finish attempts and email actual outcomes
depends on: today-trial-readiness
- [!] trial-twenty-calls — Twilio account owner/Support must resolve outbound restriction 21216 before any retry.  GitHub run_trial.py waits for today's California window, verifies monthly Twilio usage and preserves $8 conservative infrastructure reserve, reserves $1.20 per call, and records actual/capped Azure usage. No call creation outside the dated window. Ambiguous creation or nonterminal prior call prevents retry.
- [ ] trial-recordings — consent first, validated Twilio recordings copied into private S3; bounded reconciliation must succeed before advancing an accepted recording. Declined/unanswered calls remain honest no-audio outcomes.
- [x] trial-email — Blocked-results email accepted by SES on 2026-10-15; no recording links exist because no calls connected.  delegated sender implementation send_report.py sends HTML via SES; parent verifies returned SES MessageId. Email includes hotel/source/results, call and recording status, and actual seven-day links only for ready recordings.
Definition of done: workflow trial job reaches success or an explicit actionable blocked result; exactly twenty attempted destinations are reconciled (or remaining count is explicitly reported), all existing recordings are ready or visibly failed, and SES MessageId proves delivery acceptance. Waiting/scheduled is not twenty completed calls.

Jobs: local-test-job, delivery-job, controlled-call-job, recording-transfer-job. GitHub workflow concurrency serializes this run; six-hour trial-job ceiling; 09:00-17:00 California date guard. Track each Actions run ID. Failure signatures include error, failed, canceled, timeout and cost stop. Existing deployment ceiling three; transfers three; each hotel one call. On technical canary failure, stop remaining calls, fix first, resume only from verified terminal ledger without redial. No broad app feature or existing number routing change is authorized by the trial.

## ivr-navigation — reach front desk before requesting recording consent
depends on: today-trial-readiness

### ivr-prompt-routing — navigate common English phone menus
- [x] ivr-listen-first — signed navigation callbacks listen before consent; choose only announced relevant digits; request front desk/customer service from voice menus; wait through hold/transfer announcements.
- [x] ivr-navigation-bounds — 75-second wall-clock cap, eight gathers, four selections/requests; existing whole-call limit retained. Unknown menus/voicemail end without recording; no hotel redial or new calls in this implementation run.
- [x] ivr-consent-evidence — prevent recording/model entry while navigating; record action evidence without pre-consent speech. Reports distinguish navigation failure from refusal. Per-call conservative reservation $1.40 includes at most ten Gather requests and existing model/telephony caps.
- [x] ivr-verification — Local checks below exited 0; CI race and real Azure voice probe passed. go test ./cmd/phone-trial-bridge -count=1; go vet ./cmd/phone-trial-bridge; go test ./internal/webapp/ -run TestHelpDrawer -count=1; python -B -m unittest discover -s tools/phone-trial -p test_*.py. CI runs Go race tests and real Azure-only voice probe. All must exit 0.
- [x] ivr-delivery — Verified release workflow 38885171231 attempt 2 success; normal code deployment 38885171200 success. commit only IVR-related bridge, recorder/report, budget, tests, Help, workflow and plan files to main; GitHub deploy and normal Deploy terminal success. Expired 2026-10-15 trial skips calls; deployment releases idle bridge to zero. No local deployment.

Assumptions and limits: prompt matching is English and bounded; no universal IVR understanding, account/payment entry, bookings, voicemail messages, or post-consent transfer continuation. Live hotel IVR behavior is unverified until another explicitly authorized call. Incoming assistant calls are separate scope pending session clarification, not enabled by this workstream.

Restart policy: classify deterministic failures and fix before a new commit; at most three deployment repair attempts. Never retry phone calls as part of deployment. Stop only for the plan's existing stop conditions and required inbound-session clarification; independent IVR work continues.

## Inbound discovery evidence — verified 2026-10-16
- internal/auth/lwa.go and internal/auth/access.go: Login with Amazon profile maps to a separate Live Ninja user UUID.
- internal/store/users.go GetOwner resolves the owner binding; internal/webapp/api_routes.go starts authenticated owner conversations and stores transcript/history.
- cmd/realtime-broker/main.go creates fresh session IDs; cmd/nova-bridge/main.go redeems per-session tokens once. An active web token cannot be reused for phone login.
- Nova uses PCM16 16 kHz input and 24 kHz output; Twilio uses PCMU 8 kHz. Inbound Nova needs explicit audio conversion and an authenticated phone entry point.
- Decision resolved on 2026-10-17: new conversation under the same owner; private PIN setup remains required. No account identity inferred from caller ID alone.

## hotel-vpn-ten — execute the explicitly requested October 16 calls
depends on: ivr-delivery
### hotel-ten-execution — call exactly the approved destinations within limits
- [x] hotel-ten-runner — shared runner profile hotel-vpn-ten-2026-10-16, own CONTROL ledger, existing recorder event namespace; 40 offline tests pass, workflow Python syntax checked.
- [x] hotel-ten-calls — Workflow 38891213603 success; ten terminal attempts; two consented ready recordings, eight navigation exits. GitHub workflow phone-hotel-ten.yml; one 180-second call at a time, 00:00–17:00 California date guard reflects user's explicit immediate request. Hold PHONE#CONTROL/VOICE activeRequest=batch:hotel-vpn-ten-2026-10-16 for the batch; release only after terminal provider states. Unknown creation holds lock for reconciliation.
- [x] hotel-ten-results — Results email accepted by SES010001a1448085a2-5da7351d-8e0c-4add-ad0e-63e95cbddc93-000000. Ten attempts and two ready recordings measured; ASR/email in progress. reconcile all attempted CallSids, consent, transcript evidence and S3 recordings; report accurate results and unattempted count. A queued call never proves a conversation or VPN compatibility.
Definition of done: GitHub trial job exits0 for ten terminal attempts, or explicit budget/provider blocker with counted attempts; read durable batch CONTROL and call/event records and verify existing recordings.
Retry policy: each hotel one call; job resumes only verified terminal prior calls and no ambiguous creation, maximum three batch executions. Deployment retry ceiling three; model/audio preflight must pass. No local scheduled automation.

## owner-rehearsal — owner plays the hotel front desk on the hotel-call path
Owner request 2026-10-16, verbatim: `okay, call me acting like a hotel and let's try the ocnversations`.
Owner answers 2026-10-16 (AskUserQuestion, verbatim): number = owner allowlisted number, stored as secret OWNER_REHEARSAL_NUMBER (not committed); mode = `Several takes` — "Up to 3 calls in a row, so you can play different front-desk behaviours (refuse recording, transfer, IVR menu). Same caps per call."
- [x] owner-rehearsal-runner — f147c23; 67 python tests pass. batch owner-rehearsal-2026-10-16 in tools/phone-trial/run_trial.py reuses the hotel runner: own CONTROL ledger, existing event namespace, voice lock, intent ledger, consent before recording, 180 s provider+bridge caps, one attempt per take, no redial. Destination comes only from the OWNER_REHEARSAL_NUMBER secret; any other number is refused. Takes 1..3 run strictly in sequence with a 45 s pause after a terminal take; a declined recording or hangup still lets the next take run. Hotel 09:00–17:00 window skipped for this batch only (owner's own phone); 2026-10-16 date guard kept. Budget floor also counts the ten-batch ledger and the shared PHONE#BUDGET month allocation, and each take publishes its reservation there; $20 threshold unchanged, no self-granted allowance.
- [!] owner-rehearsal-workflow — run 38901224896 placed take 1 then stopped on the cost threshold (see results); takes 2 and 3 not placed. .github/workflows/phone-owner-rehearsal.yml (workflow_dispatch, takes 1..3, default 3) points the existing Azure bridge TARGETS_JSON at the owner target only, runs the Azure audio probe, calls, emails the report through send_report.py --batch-id owner-rehearsal-2026-10-16, then restores the 20 hotel targets and min-replicas 0.
Definition of done: python -B -m unittest discover -s tools/phone-trial -p 'test_*.py' exits 0; rehearsal workflow reaches a terminal result with each take's CallSid, status, duration, recording state and SES MessageId recorded here, or the exact blocker.

Results 2026-10-16 (run 38901224896, conclusion failure = expected exit for a blocked batch; Azure probe, report and restore steps succeeded):
- take-1: CallSid CAdebbeeb654690d3519827db4b7babbad, completed, 113 s, consent granted by speech, ended remote_hangup; recording RE7cc29ad90682520fae2a7e2a3a1c8fd2 in S3, state ready.
- takes 2 and 3: not placed. take-1 usage was incomplete (usageComplete=false), so it settled at the full $1.40 reserve; month spend 19.70 + 1.40 would pass the $20 threshold.
- Report email SES MessageId 010001a144b73615-3c888f6c-5b3e-40e4-804a-ae4a401937ff-000000.
- Bridge restored: 20 hotel targets, min-replicas 0, no active call.
- Unblock (owner only): an explicit $5 daily cost grant, and release of the voice lock held as `owner-stop-2026-10-16` (not set by this run). A re-test after the repeat fix is the owner's decision.

### rehearsal-repeat-fix — ask each question once, then close
Owner report 2026-10-16 after take 1, verbatim: "look at the call, it kept asking the same question, over and over again".
- [x] rehearsal-repeat-diagnosis — call CAdebbeeb654690d3519827db4b7babbad (run 38901224896, 113 s, remote_hangup). Stored events (live-ninja-phone-trial, PK RUN#hotel-trial-2026-10-15) show the room-number/surname/verification question asked five times (12:34:41–12:35:37 UTC) after staff answered "they clicked on a password and that's it", "No.", "the Wi-Fi password.". Transcript: C:\dev\live-ninja-test-evidence\owner-rehearsal-2026-10-16\CAdebbeeb654690d3519827db4b7babbad-transcript.json.
- [x] rehearsal-repeat-root-cause — cmd/phone-trial-bridge/media.go bridgeLoop sent `response.create` with no instructions on every `input_audio_buffer.committed` (old media.go:259-266, and the queued path :364-368). The model re-derived the whole checklist from the session instructions every turn, with no record of what was already asked or answered, and nothing ever advanced or closed.
- [x] rehearsal-repeat-fix — new cmd/phone-trial-bridge/dialog.go: the bridge owns questionnaire state (four questions). Each staff turn waits for its whisper transcript (3 s cap; a missing transcript advances, never repeats), classifies answer / non-answer / hold / withdrawal, and sends one explicit step with answered topics and staff replies as data. Non-answer or 10 s silence after playback re-asks once, then closes politely with end_call. Consent-first, recording-after-consent, 180 s cap, transfer/withdrawal rules and token budgets unchanged.
Definition of done: `go test ./cmd/phone-trial-bridge -count=1 -run TestRehearsalDoesNotRepeatQuestions` fails on the old media.go ("reply to \"Thank you.\" carries no turn instructions") and passes after; full `go vet`/`go test ./cmd/phone-trial-bridge` and `python -B -m unittest discover -s tools/phone-trial -p 'test_*.py'` pass; phone-trial.yml deploys with calls_authorized=false (no call). A re-test call is the owner's decision (needs a $5 daily cost grant; voice lock owner-stop-2026-10-16 held).

## mcp-owner-tools — expose phone messages, existing Jobs and memory
depends on: none; phone-send enablement depends on hotel-ten-results
### mcp-oauth-login — browser sign-in through existing owner account
- [x] mcp-oauth-implementation — Local tests, vet and deployment passed; actual client browser connection pending. agent /root/phone_trial_bridge owns internal/mcpauth and webapp mcp_auth handlers; owner browser consent, PKCE, exact redirects, hashed opaque tokens and refresh rotation.
Definition of done: go test ./internal/mcpauth ./internal/webapp -run 'TestMCP|TestOAuth' exits0; deployed metadata and unauthorized challenge verified.
### mcp-shared-tools — preserve existing authorization and side-effect controls
- [x] mcp-transport — Local protocol, scope, registry memory, owner-phone and idempotency tests pass. root owns narrow MCP tool adapter, registry phone definitions, route/IAM wiring, Help and docs. Existing job mutation proposals still require the Jobs review interface; no general background AI executor.
- [x] mcp-phone-service — 60 combined Python tests pass; configuration privately seeded after terminal batch with October $18 conservative floor. Deployment and owner-message smoke test verified: provider completed, playbackCompleted=true, S3 recording ready. agent /root/phone_trial_recordings owns existing recorder Lambda IAM-only owner-message backend and tests; configured destination aliases only, durable idempotency, single-call lock, budget reservation, signed callbacks and private recordings. Runtime config remains absent/disabled until hotel batch is terminal; seed budgetFloorUSD from final batch allocation before enablement.
Definition of done: focused Go/Python tests pass for scopes, owner boundaries, no duplicate calls, budget stop, recording and status; GitHub deployment passes; MCP initialize/tools/list/tools/call protocol verified. Actual Claude/ChatGPT connection requires owner browser sign-in and cannot be claimed from mocked tests.
Scope: no inbound call session-handoff implementation, no arbitrary phone destinations, no general task worker, no local scheduling. Fresh owner conversation selected on 2026-10-17; private PIN setup and server integration remain separate.

### mcp-release-verification — deploy and verify actual service behavior
depends on: mcp-oauth-login, mcp-shared-tools
- [x] mcp-production-deploy — Final code deployment38895530974 at a4410848f35ab2b82cf63e007504357bea6dbd8b success, including full Go tests, race checks, app health and browser quality. publish main through existing GitHub pipeline; add focused race checks. Verify metadata and 401 challenge, reject invalid origin/token, verify private owner phone message callback and S3 recording. Actual Claude/ChatGPT browser sign-in remains owner action, not inferred from API tests.
- [x] hotel-asr-report — Workflow38894847379 success; two private two-channel ASR artifacts, transcript attachment and seven-day audio links emailed. Recorder IAM batch filter verified live: status200, zero requested calls returns zero calls/events. publish phone-hotel-report.yml separately only after recorder changes deploy; convert canonical dual-channel WAV for the same two consented RecordingSids using standard libraries (no FFmpeg/dependency). Bound to four channels of at most 180 seconds, reserve $0.20 once, no generated model response. Store WAV/ASR privately beside existing MP3, label speakers unverified, email links+transcript attachment through existing SES sender; verify SES MessageId. No new hotel call.
Definition of done: deployment workflows terminal success; live metadata and unauthorized rejection confirmed; phone-message provider status/playback/recording measured; report workflow terminal and SES MessageId verified or precise operator blocker reported.

### mcp-execution-evidence — retain facts for a cold reader
- 2026-10-16 — all ten calls in batch hotel-vpn-ten-2026-10-16 settled; spentUSD 17.58806 is conservative allocation, not invoice. hotel-06 and hotel-11 consented recordings ready; other seven unsupported_menu and one navigation_limit. Neither consented call generated live transcript/model usage, so no completed interview or VPN findings verified.
- 2026-10-16 — private CONFIG/OWNER_MESSAGES and PHONE#BUDGET#2026-10 seeded transactionally after verifying no shared voice lock; two user-approved destination aliases, existing active owner UUID, budgetFloorUSD=18.00 and budgetFloorMonth=2026-10. No private numbers committed.
- 2026-10-16 — media fix: independent WSS signature vector reproduced HTTP403 before fix; accept trusted configured WSS origin and existing HTTPS proxy form. Signed, call-bound Connect completion records failed model entry immediately. Proven in tests; original deployed logs lacked handshake details, so causal diagnosis remains consistent with symptoms rather than live-proven.
- 2026-10-16 — actual Marriott request-inspector trace announced that transfer was already underway; IVR now waits before considering menu digits. Hilton voice-menu prompt still requests front desk. No automatic hotel redial.
- 2026-10-16 — go test ./internal/tools ./internal/webapp ./internal/mcpauth ./cmd/authorizer ./cmd/phone-trial-bridge ./cmd/phone-trial-transcribe -count=1 passed; corresponding go vet passed; python -B -m unittest discover -s tools/phone-trial -p test_*.py passed 60 tests. Existing app Help updated in same release.

- 2026-10-16 — main release 90f3f4d2881d0285ac7fcfa3a79b4dc2ec34d475 CI found missing persona names for the two new phone tools and the catalog's expected count still46. Added operational phone instructions and updated count48 with explicit names; no guard disabled. go test ./internal/realtime -count=1 passed. Local Windows full-suite run additionally reports existing RCA golden/prompt-budget checks; untouched LF/CRLF fixture differences are outside this change, so Linux CI remains the production full-suite gate. MCP release repair attempt2 of3.

- 2026-10-16 — releasing report-only workflow after live recorder filtering verification. It uses the same two consented RecordingSids, authenticated canonical WAV download, bounded transcription-only Azure sessions, private S3 artifacts and SES result email. No FFmpeg/new dependency and no telephone call creation. Email runs even on an authenticated ASR failure and reports unavailable text; sender deduplicates the exact transcript status.

- 2026-10-16 — report workflow38894847379 completed with two successful S3 ASR uploads and SESMessageId010001a1448085a2-5da7351d-8e0c-4add-ad0e-63e95cbddc93-000000. Root independently verifies delivery logs before final reporting. Recorded consent is not proof of an interview; no live interview findings existed.
- 2026-10-16 — owner-message smoke request mcp-release-test-20261016 was denied atomically by IAM before any provider call: missing dynamodb:ConditionCheckItem. Consistent read confirms no message request row, monthly allocatedUSD still18.00. Add only the missing action to the existing table ARN policy, then retry the SAME request ID after deployment. No duplicate call or phone reservation occurred.

- 2026-10-16 — integrated MCP origin validation now runs before bearer processing, so disallowed Origin receives403 even without credentials as required by Streamable HTTP. Extended existing auth challenge test; go test ./internal/webapp -run TestMCP -count=1 passed. Root read both private ASR artifacts: only thanks/recording notice, no VPN answers. Root independently verified report SESMessageId010001a1448085a2-5da7351d-8e0c-4add-ad0e-63e95cbddc93-000000.

- 2026-10-16 — final code deployment38895530974 completed success; recorder/media deployment38895245729 success. Live protected-resource and authorization-server metadata both200; PKCE S256 advertised. Unauthenticated MCP401 with resource-metadata challenge; invalid Origin403. Temporary DCR smoke client201, valid PKCE request302 to existing /auth/lwa/login with HttpOnly+secure+SameSite=Lax cookie; invalidcode400. Temporary client/pending test rows removed, no real owner grant fabricated.
- 2026-10-16 — owner-message request mcp-release-test-20261016, callCA56787f9da0873ad56764ff44bf6e0a47 completed; signed post-script callback playbackCompleted=true, humanHeard remains not_verified, private S3 recording ready. Gateway download302 then S3 rangedGET206 audio/mpeg; first-issued link lifetime measured approximately604760seconds remaining after initial status read (seven-day lifetime). PHONE#CONTROL/VOICE released; monthly conservative allocatedUSD18.30.
- 2026-10-16 — report38894847379 completedsuccess; two consented ASR artifacts and HTML email with seven-day recording links/transcript attachment accepted by SES010001a1448085a2-5da7351d-8e0c-4add-ad0e-63e95cbddc93-000000. Audio transcripts contain acknowledgments and recording notice only, no VPN findings. No further hotel calls authorized or made.

### mcp-client-connection — complete the owner's real remote-client connection
depends on: mcp-release-verification
- [!] mcp-owner-sign-in — Jeremy must add https://live.jeremy.ninja/mcp as an OAuth remote MCP connection in Claude or ChatGPT, complete Login with Amazon and approve the displayed scopes. Then test a read such as job_list. Server/unit tests and unauthenticated live probes do not prove a real client connection. Async question remains pending; no credentials requested in chat.
Definition of done: owner completes browser consent and an actual client initialize/tools/list/read-tool result is observed without exposing tokens. Fresh inbound conversation selected on 2026-10-17; private PIN setup remains pending and inbound routing is not enabled.

## mcp-consent-redirect — fix invalid_grant during remote-client connection
depends on: mcp-release-verification
### mcp-return-policy — permit the registered client after browser consent
- [x] mcp-redirect-reproduction — production route trace on 2026-10-16: authorize GET200 twice, Allow POST302 at12:05:08UTC, second POST400 at12:05:11UTC, no token exchange. TestMCPOAuthConsentPolicyAllowsClientRedirect failed with 'consent policy blocks registered client callback: https://client.example'. Consent CSP form-action self excludes the cross-origin OAuth return; Chromium can enforce CSP on this redirect chain.
- [x] mcp-callback-policy-fix — consent CSP includes only self and the already-registered callback origin; URL validation excludes wildcard/policy-injection hosts. CSRF, active-owner, PKCE, audience and one-use code/consent checks remain. Help instructs fresh client connection after stale/used grant.
- [x] mcp-redirect-delivery — Code d851b43b38ab2bbbb446831dfb0da3c8a454a931 deployed; workflow38899020386 completed success, including full tests, race checks and browser quality. go test ./internal/mcpauth ./internal/webapp -count=1 and go vet ./internal/mcpauth ./internal/webapp passed. Commit/push through main; CI/deploy must reach terminal success. Then Jeremy must start a fresh client connection, not reuse the consumed consent page.
Definition of done: focused regression, OAuth negative tests and CI pass; deployed code contains the exact-origin policy; actual client returns after Allow and exchanges code successfully. Browser verification remains operator-dependent on OFFICEPC unless an approved browser driver is supplied.
Non-goals: do not relax grant replay, accept arbitrary redirect origins, change memory/jobs/call permissions, or retry phone calls. Files: mcp_auth.go/test, mcpauth/service.go/test, Help, plan. Deployment repair ceiling three; no credential values or callback query strings in logs.
Primary reference: https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Content-Security-Policy/form-action

- 2026-10-16 — mcp-consent-redirect release verified: workflow38899020386 success atd851b43b38ab2bbbb446831dfb0da3c8a454a931. Production affected requests identified as Chromium using only sanitized browser-family metadata. CloudFront leaves CSP to app and SecurityHeaders preserves the consent-specific policy. No debug instrumentation added, no tokens/callback query values printed, no phone calls made. Operator asked to close stale consent page and start a fresh client connection; no post-fix token exchange observed yet. Keep mcp-owner-sign-in pending until actual client success.
