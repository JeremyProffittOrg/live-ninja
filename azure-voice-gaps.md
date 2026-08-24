# Azure voice engines — gap register

Findings from an eleven-reviewer audit of `/c/dev/live-ninja/azure-voice-plan.md` against the
repository, the live Azure subscription and current Microsoft documentation, run 2026-08-24 on
branch `alexa-version` at `b474735`.

**This file is the findings register. It is not a plan.** Items marked `[x]` are already applied to
`azure-voice-plan.md`; items marked `[ ]` are outstanding and belong to the milestone named in their
ANCHOR. Nothing here is a backlog item — a backlog item goes in `backlog.md`.

## How this was produced

Eleven reviewers took non-overlapping slices, each grounded in the repository or in live external
sources rather than in the plan's own prose: engine-enum plumbing, mint and config, secrets and the
deploy pipeline, live Azure resources, the web client, the Android client, the wire contract and
version gate, cost and rates, the Azure facts re-read from Microsoft Learn, a sweep of every
definition of done on its own, and docs/catalog/Help.

Every finding then faced two adversarial verifiers whose default answer was "refuted": one on
CORRECTNESS (re-run the evidence, check the line numbers, search the whole plan for a section the
reviewer missed) and one on CONSEQUENCE (assume the evidence is right — does an unattended run
actually behave differently?). A finding survived only if neither verifier refuted it.

98 raw findings. 59 survived. 39 were refuted and are listed at the end so they are not re-raised.
A completeness critic then looked for what no slice covered and added 8 more. Merging the cases
where several reviewers found the same defect gives the **47** entries below. The load-bearing ones
were re-run a third time in the main thread before being recorded here.

## Verification legend

- **VERIFIED** — a command was run, or a file was read, and the output is quoted.
- **INFERRED (Azure)** — reasoning about Azure service behaviour from documentation, not from this
  repository. Re-check against current Microsoft documentation before acting.

---

## Read this first: the plan cannot currently reach WS-F M1

WS-F M1 is the gate — "Nothing below this line is done until this passes." Four independent findings
each make it unreachable on their own, so fixing any one of them does not unblock the plan:

- **W3** — WS-D M1 gates on `X-LN-Client`, which never reaches the broker. The broker is not an HTTP
  handler; `internal/webapp/api_routes.go:363` builds a JSON payload by hand and calls
  `deps.Lambda.Invoke`. Under the plan's own fail-closed rule every client is "unknown", so every
  session routes to `openai-realtime` and no Azure engine can ever be selected.
- **S1** — WS-A M4 cannot run. The only Azure identity on this machine is a service principal with no
  directory role; `az ad app list` returns "Insufficient privileges to complete the operation." It
  cannot create the `ln-voicelive-client` app registration, which blocks all of WS-C and both Voice
  Live engines. This is stop condition 1.
- **F1 / M2 / W2** — no Azure credential can reach a deployed Lambda. `scripts/set-secret.sh` writes
  GitHub Actions secrets, not SSM; nothing in the plan edits the workflow step that writes SSM; and
  the broker's IAM policy grants `ssm:GetParameter` on two exact ARNs that do not include an Azure
  parameter.
- **S4** — WS-F M1's own definition of done requires a human at a microphone for four sessions, in a
  plan written for a run where "the operator is unreachable the moment work starts".

**Q1** is the finding to act on before any code is written: if WS-B M6 ships before WS-D M1, both
clients treat an unknown `mode` as `openai-direct` and will POST an Azure `ek_` to `api.openai.com`.


## The false "Verified facts"

- [ ] **F1. WS-A M6 names a script that cannot write SSM at all — set-secret.sh writes GitHub Actions secrets** BLOCKER.
      ANCHOR: WS-A M6; Locked decision 1; Standing authorizations ("Write SSM SecureString parameters ... via /c/dev/live-ninja/scripts/set-secret.sh only")
      CLAIM: WS-A M6: "Three SecureString parameters, all written by `/c/dev/live-ninja/scripts/set-secret.sh` with the operator typing the values: `/live-ninja/prod/azure/openai_api_key`, `/live-ninja/prod/azure/voicelive_client_secret`, and `/live-ninja/prod/azure/voicelive_client_id`." Locked decision 1: "The key is written by `/c/dev/live-ninja/scripts/set-secret.sh` into SSM SecureString".
      REALITY: set-secret.sh never calls AWS. Its only write is `gh secret set` — a GitHub Actions repository secret. It also rejects every name WS-A M6 asks for: it enforces `^[A-Z][A-Z0-9_]*$` and exits 2 on anything else, so `/live-ninja/prod/azure/openai_api_key` is rejected before any work happens. The real chain is: operator sets an UPPER_SNAKE_CASE GitHub secret -> push to main -> deploy.yml's "Sync secrets to SSM" step runs `aws ssm put-parameter` -> the parameter exists. WS-A M6 is not executable as written, and the Standing authorization it rests on authorizes an impossible action.
      EVIDENCE:
      ```
      $ cat -n scripts/set-secret.sh
          19	[[ "$NAME" =~ ^[A-Z][A-Z0-9_]*$ ]] || { echo "ERROR: secret names are UPPER_SNAKE_CASE" >&2; exit 2; }
          47	printf '%s' "$VALUE" | gh secret set "$NAME" -R "$REPO"
          54	echo "Reminder: secrets reach running stacks only on the next deploy (push to main)."
      $ grep -n 'aws\|ssm' scripts/set-secret.sh
      (no output)
      $ sed -n '283,296p' .github/workflows/deploy.yml
            - name: Sync secrets to SSM
              env:
                OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
      ...
                aws ssm put-parameter \
                  --name "/live-ninja/prod/openai/api_key" \
                  --type SecureString \
                  --value "$OPENAI_API_KEY" \
                  --overwrite >/dev/null
      $ sed -n '23,26p' internal/config/config.go
      // SSM parameter names, fixed by the shared spec (deploy.md / plan.md M0).
      // These are created (as empty/placeholder slots) by CloudFormation-adjacent
      // tooling and populated with real values by the deploy workflow from
      // GitHub secrets/variables — never by application code.
      $ gh secret list -R JeremyProffittOrg/live-ninja
      ANDROID_DEBUG_KEYSTORE_B64	2026-07-20T12:09:08Z
      GEMINI_API_KEY	2026-07-26T11:22:01Z
      GEMINI_SERVICE_ACCOUNT_JSON	2026-07-25T20:14:18Z
      LWA_CLIENT_SECRET	2026-07-17T19:06:32Z
      OPENAI_API_KEY	2026-07-17T19:06:21Z
      ```
      FIX: Rewrite WS-A M6 to the real two-stage chain. Stage 1: set three GitHub repo secrets named `AZURE_OPENAI_API_KEY`, `AZURE_VOICELIVE_CLIENT_SECRET` (and repo *variable* `AZURE_VOICELIVE_CLIENT_ID`, which is an identifier, not a secret) — DoD `gh secret list -R JeremyProffittOrg/live-ninja | grep -c AZURE_` returns 2. Stage 2 is the new deploy.yml milestone (see the next finding). Change the Standing authorization from "via scripts/set-secret.sh only" to "via a GitHub repo secret plus the deploy.yml sync step".
      VERIFIED. Reviewer slice: secrets-and-pipeline.
- [ ] **F2. R5 is false on transcription: audio.input.transcription.model is an OpenAI model id that Azure rejects — Azure requires a deployment name, and WS-A M3 deploys none** BLOCKER.
      ANCHOR: Verified facts R5 (azure-voice-plan.md:152-160); WS-B M1 (:459-468); WS-B M3 (:531-545)
      CLAIM: R5: "That is byte-for-byte the object Azure's GA `/openai/v1/realtime/client_secrets` accepts." WS-B M3: "Only three things differ from `Minter`: the URL ..., the auth header ..., and the model id."
      REALITY: A fourth thing differs. `buildAudioInput` hardcodes `"transcription": {"model": "gpt-4o-mini-transcribe"}` at mint.go:278. Microsoft Learn documents this exact field as an Azure deviation: the value must be the name of an existing model deployment in the resource, not a raw OpenAI model id. WS-A M3 deploys only `gpt-realtime-2-1` and `gpt-realtime-2-1-mini` — no transcription deployment exists to name. So the reused builder either 400s the mint or silently drops transcription, which reintroduces exactly the bug the comment at mint.go:275-277 records ("the user's own speech never appeared in any transcript"). WS-B M1's smoke curl cannot catch this: its body is `{"session":{"type":"realtime","model":"gpt-realtime-2-1","audio":{"output":{"voice":"cedar"}}}}` — it contains no `audio.input` block at all, so B1 prints MINT_OK while the payload the Go code will actually send is broken.
      EVIDENCE:
      ```
      $ grep -n "gpt-4o-mini-transcribe" internal/realtime/mint.go
      278:			"model": "gpt-4o-mini-transcribe",
      
      https://learn.microsoft.com/en-us/azure/ai-foundry/openai/realtime-audio-reference (ms.date 2026-05-13), verbatim:
      "**Azure deviation:** The accepted values for the `model` field in `input_audio_transcription` settings differ from the OpenAI reference. Azure OpenAI requires the name of the existing model deployment for the field, like `my-gpt-4o-transcribe-deployment`."
      
      $ sed -n '459,468p' azure-voice-plan.md  (WS-B M1 DoD body)
      -d '{"session":{"type":"realtime","model":"gpt-realtime-2-1","audio":{"output":{"voice":"cedar"}}}}'
      ```
      FIX: Three edits. (1) Add to WS-A M3: "also deploy `gpt-4o-mini-transcribe` as a deployment named `gpt-4o-mini-transcribe`, and record the deployment name in the Execution log" — and extend its DoD to require three names. (2) Change WS-B M3 from "only three things differ" to four, and make `buildAudioInput` take the transcription deployment name as a parameter (its OpenAI caller passes `"gpt-4o-mini-transcribe"` unchanged) rather than hardcoding it. (3) Change WS-B M1's `-d` body to the full builder shape including `"input":{"turn_detection":{"type":"semantic_vad","interrupt_response":true},"noise_reduction":{"type":"near_field"},"transcription":{"model":"gpt-4o-mini-transcribe"}}` so the smoke test exercises what the code will send.
      VERIFIED. Reviewer slice: mint-and-config.
- [ ] **F3. The Android client's live X-LN-Client value does not match the header grammar, so the gate fails closed for Android too** BLOCKER.
      ANCHOR: WS-D M1; "Verified facts" R12; WS-E M2
      CLAIM: R12: "`X-LN-Client` parsing already exists... `parseClientVersion` returns `(clientVersion{surface,major,minor,patch,build}, ok)`". WS-D M1 then treats an unparseable header as an old client and routes it away from the Azure engines.
      REALITY: R12's description of parseClientVersion is accurate, but the plan never checks what the shipping Android app actually sends. `ClientId.HEADER_VALUE` is `"android/${BuildConfig.VERSION_NAME}+r${BuildConfig.VERSION_CODE}"` and VERSION_NAME is `0.2.2-hal`, producing `android/0.2.2-hal+r5`. The grammar requires `<surface>/<d>.<d>.<d>+<build>` — the `-hal` suffix makes it unparseable. So the Android surface would also fail closed forever, regardless of what minimum WS-D M1 picks, and WS-E M2/M4 (spoken turn on `gpt-live-azure` on the Galaxy S9) could never reach an Azure engine. No milestone normalizes versionName.
      EVIDENCE:
      ```
      internal/webapp/version.go:37 (the grammar):
      var clientHeaderPattern = regexp.MustCompile(`^(web|android|m5stack)/(\d+)\.(\d+)\.(\d+)\+([A-Za-z0-9._-]+)$`)
      
      android/app/src/main/java/ninja/jeremy/liveninja/net/AuthInterceptor.kt:17:
          val HEADER_VALUE: String = "android/${BuildConfig.VERSION_NAME}+r${BuildConfig.VERSION_CODE}"
      
      android/app/build.gradle.kts:29-30:
              versionCode = 5
              versionName = "0.2.2-hal"
      
      $ for s in "android/0.2.2-hal+r5" "android/2.1.0+r48" "web/0.7.0+gabc123"; do if echo "$s" | grep -qE '^(web|android|m5stack)/([0-9]+)\.([0-9]+)\.([0-9]+)\+([A-Za-z0-9._-]+)$'; then echo "MATCH   $s"; else echo "NOMATCH $s"; fi; done
      NOMATCH android/0.2.2-hal+r5
      MATCH   android/2.1.0+r48
      MATCH   web/0.7.0+gabc123
      ```
      FIX: Add a numbered step to WS-E M2: set `versionName = "0.3.0"` and `versionCode = 6` in /c/dev/live-ninja/android/app/build.gradle.kts:29-30 (dropping the `-hal` suffix, which the X-LN-Client grammar rejects), and add an assertion to android/app/src/test/java/ninja/jeremy/liveninja/net/AuthInterceptorTest.kt that `ClientId.HEADER_VALUE` matches `^android/\d+\.\d+\.\d+\+[A-Za-z0-9._-]+$`. Note in R12 that the currently-installed Android build sends an unparseable value.
      VERIFIED. Reviewer slice: client-android, wire-and-gate. Raised independently by 2 reviewers.
- [ ] **F4. The "anti-injection" claim in "## The token problem" is false — persona instructions already reach the client on all four existing engines** MAJOR.
      ANCHOR: ## The token problem, stated honestly (azure-voice-plan.md:102-105)
      CLAIM: "**The persona instructions leave the server.** On every other engine the raw instruction text never reaches the client (the anti-injection rule in `/c/dev/live-ninja/internal/realtime/personas.go`). On Voice Live WebRTC the client *is* the thing that sends `session.instructions` ... The Help drawer and `docs/voice-engines.md` must say so (WS-F M2, WS-F M3)."
      REALITY: Every existing engine already ships the full raw instruction text to the client in the `sessionConfig` field of the bootstrap response. `mint.go:305` puts `"instructions": InstructionsForSurface(persona, surface) + SessionDirectives + instructionsSuffix` into `sessionConfig`, `mint.go:362` marshals that same map into `MintResult.SessionConfig`, and `cmd/realtime-broker/main.go:464` returns it as the wire field `sessionConfig` on `mode: "openai-direct"`. Nova does the same (`main.go:503` `BuildNovaSessionConfig(instructions)` -> `nova_config.go:34` `SystemPrompt: systemPrompt` -> `main.go:552`), and Gemini does the same (`gemini_mint.go:508-514` "full persona+directive instructions", `:529` `systemInstruction`, `:661` `SessionConfig: setupJSON` -> `main.go:632`). The anti-injection rule cited is about the INBOUND direction only: `personas.go:3` reads "clients send a persona ID, never instructions — anti-injection". The correct distinction for Voice Live is enforcement (the token is not config-bound), not disclosure. As written, WS-F M2 and WS-F M3 are instructed to publish a user-facing security statement that is untrue of this product.
      EVIDENCE:
      ```
      $ sed -n '295,306p' internal/realtime/mint.go
      	sessionConfig := map[string]any{
      		"type":  "realtime",
      		"model": m.model,
      ...
      		"instructions": InstructionsForSurface(persona, surface) + SessionDirectives + instructionsSuffix,
      		"tools":        manifest,
      	}
      
      $ sed -n '362,374p' internal/realtime/mint.go
      	cfgJSON, err := json.Marshal(sessionConfig)
      ...
      		SessionConfig: cfgJSON,
      
      $ grep -n "SessionConfig" cmd/realtime-broker/main.go
      138:	SessionConfig json.RawMessage        `json:"sessionConfig,omitempty"`
      464:		SessionConfig: res.SessionConfig,
      503:		novaConfig := realtime.BuildNovaSessionConfig(instructions)
      552:		SessionConfig:        configJSON,
      632:		SessionConfig:  res.SessionConfig,
      
      $ sed -n '1,4p' internal/realtime/personas.go
      // Package realtime implements the M2 realtime-voice backend pieces owned
      // by the broker Lambda: server-side persona resolution (clients send a
      // persona ID, never instructions — anti-injection, plan.md M2), the
      // config-bound OpenAI ephemeral-token mint, ...
      ```
      FIX: Replace azure-voice-plan.md:102-105 with the accurate statement: "**The persona instructions are not enforceable.** The raw instruction text already reaches the client on every engine — the broker returns it in `sessionConfig` (`internal/realtime/mint.go:305,362`; `cmd/realtime-broker/main.go:464,552,632`). The anti-injection rule (`personas.go:3`) is inbound-only: clients send a persona ID, never instructions. What is different on Voice Live is *enforcement*: on the other engines the minted credential is config-bound at mint, so a client that edits `sessionConfig` changes nothing; on Voice Live the client's copy IS the session config. Help and doc copy must say the configuration is not enforced, not that the text is secret."
      VERIFIED. Reviewer slice: docs-catalog-help.
- [ ] **F5. Verified fact R1 misstates IsClientDirect, and the WS-B M2(b) step derived from it is already done** MINOR.
      ANCHOR: Verified facts R1 (azure-voice-plan.md:117-121); WS-B M2 (b) (:473-475)
      CLAIM: R1: "`Engine.Valid()` (`:35-42`) and `Engine.IsClientDirect()` (`:33`) switch over exactly those four." WS-B M2 (b): "Redefine `IsClientDirect()` as \"not `nova-sonic`\" — **all four new engines are client-direct**".
      REALITY: `IsClientDirect` does not switch over anything and does not mention the four engines. It is a single not-equal comparison against nova-sonic at engine.go:32 (not :33), and it is already literally the definition WS-B M2 (b) asks the run to change it to. The four new engines are already client-direct under it with no edit. The only real work in (b) is the second half — giving it a first caller. A run following (b) as written either makes a no-op edit or, worse, treats an already-correct function as needing rewriting.
      EVIDENCE:
      ```
      $ grep -n 'IsClientDirect|func (e Engine) Valid' internal/voiceengine/engine.go
      28:// IsClientDirect reports whether the engine uses the client-direct transport
      32:func (e Engine) IsClientDirect() bool { return e != EngineNovaSonic }
      35:func (e Engine) Valid() bool {
      
      $ grep -rn 'IsClientDirect' --include=*.go . | grep -v worktrees
      ./internal/voiceengine/engine.go:28:…
      ./internal/voiceengine/engine.go:32:func (e Engine) IsClientDirect() bool { return e != EngineNovaSonic }
      ```
      FIX: Change R1's last sentence to: "`Engine.Valid()` (`:35-42`) switches over exactly those four; `Engine.IsClientDirect()` (`:32`) is already `e != EngineNovaSonic` and needs no change." Change WS-B M2 (b) to: "(b) `IsClientDirect()` (engine.go:32) is already correct for all four new engines — leave the body alone and give it its first real caller so it can no longer drift unnoticed (R1)."
      VERIFIED. Reviewer slice: completeness-critic.

## Definitions of done that pass while the work is undone

Every definition of done below was executed against the untouched tree at `b474735`, with no
milestone started. Each one passed. This is the plan's dominant defect class and the most
dangerous one for an unattended run: a run can mark the milestone `[x]`, commit, and move on
having written nothing.

- [ ] **D1. WS-A M6's DoD can never pass by executing WS-A M6, and WS-B M1 is scheduled before the parameter can exist** BLOCKER.
      ANCHOR: WS-A M6 DoD; Sequencing items 1 and 2; WS-B M1 ("Depends on A3 and A6")
      CLAIM: WS-A M6 DoD: "`aws ssm get-parameters-by-path --path /live-ninja/prod/azure --query "Parameters[].Name" -o text` prints three names." Sequencing item 1 puts "WS-A M1–M6" in the first parallel wave; item 2 says "WS-B M1 as soon as A3 and A6 land".
      REALITY: The named action (run set-secret.sh) writes GitHub secrets, which do not reach SSM until the deploy job runs. So executing WS-A M6 exactly as written leaves the DoD failing. Passing it requires an artifact edit the plan does not contain (deploy.yml), a commit, a push to main, and a successful `deploy` job — none of which WS-A M6 or the Sequencing section schedules. The path is currently empty, confirming the DoD does not trivially pass; the defect is that no action in the plan makes it pass. WS-B M1, which the plan schedules immediately after A6, therefore cannot run when scheduled.
      EVIDENCE:
      ```
      $ MSYS_NO_PATHCONV=1 aws ssm get-parameters-by-path --path /live-ninja/prod/azure --query "Parameters[].Name" --output text; echo "[exit $?]"
      
      [exit 0]
      $ MSYS_NO_PATHCONV=1 aws ssm get-parameters-by-path --path /live-ninja/prod --recursive --query "Parameters[].Name" --output text
      /live-ninja/prod/device/cred_pepper	/live-ninja/prod/gemini/api_key	/live-ninja/prod/gemini/service_account_json	/live-ninja/prod/lwa/client_id	/live-ninja/prod/lwa/client_secret	/live-ninja/prod/openai/api_key
      $ sed -n '235,244p' .github/workflows/deploy.yml
        deploy:
          needs: [test, build-wakeword-container, build-nova-container]
          if: >-
            always() &&
            needs.test.result == 'success' && ...
      ```
      FIX: Split WS-A M6 into M6a (set the GitHub secrets, DoD `gh secret list`), M6b (edit deploy.yml + template.yaml IAM, commit, push to main), and M6c (the SSM DoD, gated on `gh run watch` reaching `completed success`). Move WS-B M1's dependency from "A3 and A6" to "A3 and A6c", and change Sequencing item 2 to say WS-B M1 runs only after the deploy run that publishes the parameter has finished.
      VERIFIED. Reviewer slice: secrets-and-pipeline.
- [ ] **D2. WS-B M2 DoD passes today with zero work done** BLOCKER.
      ANCHOR: WS-B M2 (line 483-488)
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./internal/voiceengine/ ./internal/realtime/ ./internal/webapp/ ./cmd/realtime-broker/` passes, with new cases asserting `PinToEngine("gpt-live-azure", nil, "") == EngineGPTLiveAzure` …
      REALITY: The command exits 0 right now, before any of (a)-(e) is written. The "with new cases asserting …" clause is prose the command cannot enforce: `go test` on a package reports success whether or not those cases exist.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./internal/voiceengine/ ./internal/realtime/ ./internal/webapp/ ./cmd/realtime-broker/
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/voiceengine	2.203s
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/realtime	0.319s
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/webapp	0.619s
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.253s
      exit=0
      
      $ git rev-parse --short HEAD
      b474735
      $ git status --porcelain internal/ cmd/ contracts/
      (no output)
      $ go test -count=1 ./internal/voiceengine/ ./internal/realtime/ ./internal/webapp/ ./cmd/realtime-broker/
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/voiceengine	0.892s
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/realtime	0.269s
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/webapp	0.569s
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.219s
      EXIT=0
      
      $ grep -rn "contracts/" --include=*.go . | grep -v "^./.claude"
      ./internal/webapp/auth_routes.go:77:	app.Post("/auth/lwa/exchange", r.exchange) // contracts/api.md canonical alias
      ./internal/webapp/memory_routes.go:135:	api.Delete("/memory/:id", handleForgetEntity(deps))                 // contracts/api.md "forget" path
      ./internal/webapp/memory_routes.go:473:	Body     string `json:"body"` // contracts/api.md alias for text
      ./internal/webapp/telemetry_routes.go:117:			return apiBadRequest(c, fmt.Sprintf("events batch exceeds the max of %d per contracts/telemetry.schema.json", telemetryMaxBatch))
      (all four are comments; nothing parses the schema)
      ```
      FIX: Name the three tests and make the DoD a command that fails on the current tree. Replace the DoD line with:
`cd /c/dev/live-ninja && go test -count=1 -v ./internal/realtime/ ./internal/webapp/ -run 'TestPinToEngineAcceptsAzureEngines|TestSettingsPutAcceptsAzureEngineDefault|TestDeviceOverrideAzureEnginePinResolves' 2>&1 | grep -c '^--- PASS'` prints `3`, **and** `cd /c/dev/live-ninja && go test -count=1 ./internal/voiceengine/ ./internal/realtime/ ./internal/webapp/ ./cmd/realtime-broker/` passes, **and** `grep -c -e '"gpt-live-azure"' -e '"gpt-live-azure-mini"' -e '"azure-voice-live"' -e '"azure-voice-live-lite"' /c/dev/live-ninja/contracts/settings.schema.json` returns `8` (four ids in each of the two enums at :192-197 and :206-209).
The three named tests must live in `internal/realtime/engine_test.go` and `internal/webapp/settings_routes_test.go`; each of the three clauses fails today, so the DoD can no longer pass while the work is undone.
      VERIFIED. Reviewer slice: dod-sweep, enum-plumbing. Raised independently by 4 reviewers.
- [ ] **D3. WS-B M3's DoD is satisfied by seven pre-existing OpenAI/Gemini tests and cannot fail if azure_mint.go is never written** BLOCKER.
      ANCHOR: WS-B M3 (azure-voice-plan.md:531-545)
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run 'Mint|Azure' -v` passes, including a test asserting the Azure request URL carries **no** `api-version` query parameter and that the OpenAI minter's URL is unchanged.
      REALITY: The pattern `Mint|Azure` already matches seven existing tests, none of them Azure-related — `Azure` matches nothing in the tree today. The command exits 0 on the current worktree, so the milestone's DoD is green before any Azure minter exists.
      EVIDENCE:
      ```
      $ go test ./internal/realtime/ -run 'Mint|Azure' -count=1
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/realtime	0.177s
      EXIT=0
      
      $ go test ./internal/realtime/ -run 'Mint|Azure' -v -count=1 | grep "^--- PASS"
      --- PASS: TestGeminiMintBuildsConstrainedTokenAndSetup (0.00s)
      --- PASS: TestGeminiMintUsesCurrentV1BetaRESTContract (0.01s)
      --- PASS: TestMiniMinterBindsExactModelInRequestAndResult (0.00s)
      --- PASS: TestCheckMintHappyPathNoWarnings (0.00s)
      --- PASS: TestRecordMintSessionCapBookkeeping (0.00s)
      --- PASS: TestRecordMintWritesLedger (0.00s)
      --- PASS: TestCheckSessionRedeemsRecordedMint (0.00s)
      ```
      FIX: Change the DoD to name the two required tests so absence is a failure: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run 'TestAzureMinterURLHasNoAPIVersion|TestOpenAIMinterURLUnchanged|TestAzureAndOpenAISessionConfigIdenticalExceptModel' -v -count=1 | grep -c '^--- PASS' | grep -qx 3 && echo B3_OK`.
      VERIFIED. Reviewer slice: dod-sweep, mint-and-config. Raised independently by 2 reviewers.
- [ ] **D4. WS-B M4 DoD passes today — `-run 'Voice|Catalog'` matches existing catalog tests** BLOCKER.
      ANCHOR: WS-B M4 (line 510-511)
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run 'Voice|Catalog'` passes, and a new test asserts every engine's default voice is a member of that engine's own catalog.
      REALITY: Exits 0 today with `SupportedAzureRealtimeVoices` absent and no default-voice-membership test written.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./internal/realtime/ -run 'Voice|Catalog'
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/realtime	0.213s
      exit=0
      ```
      FIX: Require the new test by name and require the catalog to be non-empty: `go test ./internal/realtime/ -run TestEveryEngineDefaultVoiceIsInItsOwnCatalog -v -count=1 2>&1 | grep -q '^--- PASS' && test "$(go test ./internal/realtime/ -run TestSupportedAzureRealtimeVoicesHas35Entries -v -count=1 2>&1 | grep -c '^--- PASS')" = 1`.
      VERIFIED. Reviewer slice: docs-catalog-help, dod-sweep. Raised independently by 2 reviewers.
- [ ] **D5. WS-B M5 DoD passes today — `-run Rates` matches the existing rate tests, including the one that asserts the defect** BLOCKER.
      ANCHOR: WS-B M5 (line 525-528)
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run Rates` passes and a new `TestRatesCoverEveryShippedEngine` asserts every model id reachable from the eight engine constants has an explicit `modelRates` key.
      REALITY: `go test -run Rates` exits 0 today while the R10 billing defect is still live and `TestRatesCoverEveryShippedEngine` does not exist. The DoD's own guard sentence ("`TestRatesForUnknownModelFallsBack` must not be the thing that makes it pass") is prose, and today that test is exactly what makes it pass.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./internal/realtime/ -run Rates
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/realtime	0.180s
      exit=0
      ```
      FIX: Make the DoD the new test alone: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run TestRatesCoverEveryShippedEngine -v -count=1 2>&1 | grep -q '^--- PASS: TestRatesCoverEveryShippedEngine'` (fails today: no such test), then `go test ./internal/realtime/ -run Rates -count=1` for regression.
      VERIFIED. Reviewer slice: dod-sweep.
- [ ] **D6. Four DoD commands in this slice pass today with none of the work done** BLOCKER.
      ANCHOR: WS-B M6, WS-C M2, WS-D M1, WS-D M2 — the DoD line of each
      CLAIM: WS-B M6 DoD: "`cd /c/dev/live-ninja && go test ./cmd/realtime-broker/` passes". WS-C M2 DoD: "`go test ./cmd/realtime-broker/ -run VoiceLive` passes". WS-D M1 DoD: "`go test ./cmd/realtime-broker/ -run 'Gate|ClientVersion'` passes". WS-D M2 DoD: "`go test ./cmd/realtime-broker/ -run Session` passes".
      REALITY: All four commands exit 0 right now, on the unmodified tree. `go test -run <pattern>` that matches zero test functions reports `ok ... [no tests to run]` and exits 0 — it is not a failure. An unattended run following the plan literally would mark all four milestones `[x]` without writing a single line of code. The prose after each DoD names the required assertions but is not part of the pass/fail command.
      EVIDENCE:
      ```
      $ go test ./cmd/realtime-broker/
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	(cached)
      
      $ for r in "VoiceLive" "Gate|ClientVersion" "Session"; do go test ./cmd/realtime-broker/ -run "$r" -count=1; echo exit=$?; done
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.161s [no tests to run]
      exit=0
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.150s [no tests to run]
      exit=0
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.190s [no tests to run]
      exit=0
      
      $ go test ./cmd/realtime-broker/ -run "Gate|ClientVersion" -v -count=1
      testing: warning: no tests to run
      PASS
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.174s [no tests to run]
      ```
      FIX: Name the exact new test functions in each DoD and require them to run, e.g. WS-B M6: `go test ./cmd/realtime-broker/ -run 'TestAzureDirectMode|TestVoiceLiveDirectMode|TestAzureMintFailureFallsBack' -v 2>&1 | grep -c '^--- PASS' ` returns 3; WS-C M2: `-run TestVoiceLiveResponseHasNoBridgeFields -v` must print `--- PASS`; WS-D M1: `-run TestClientVersionGateRoutesUnknownClientToOpenAI -v` must print `--- PASS`; WS-D M2: `-run TestOpenAIDirectResponseCarriesCallsURL -v` must print `--- PASS`. Add to each DoD: `go test -run` matching zero tests exits 0 — a `[no tests to run]` line is a FAIL for this milestone.
      VERIFIED. Reviewer slice: dod-sweep, wire-and-gate. Raised independently by 2 reviewers.
- [ ] **D7. Name the new test functions in WS-C M1 and WS-C M2 DoDs — both commands pass today with zero tests** BLOCKER.
      ANCHOR: WS-C M1 (azure-voice-plan.md:551-563) and WS-C M2 (:565-576)
      CLAIM: C1 DoD: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run Entra` passes, including a test that a cached unexpired token is reused... C2 DoD: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run VoiceLive` passes, including a test asserting the response contains no `wsUrl` and no `bridgeUrl` field.
      REALITY: No test in either package has a name matching `Entra` or `VoiceLive`. `go test -run <pattern>` with no matching test prints `ok ... [no tests to run]` and exits 0. Both DoDs therefore pass right now, before `entra_token.go` or `handleVoiceLiveDirect` exists. An unattended run can mark both milestones `[x]` having written no code and no test.
      EVIDENCE:
      ```
      $ go test ./internal/realtime/ -run Entra -count=1
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/realtime	0.237s [no tests to run]
      EXIT=0
      
      $ go test ./cmd/realtime-broker/ -run VoiceLive -count=1
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.209s [no tests to run]
      EXIT=0
      ```
      FIX: Replace both DoD commands with ones that name the required test functions and fail when they are absent. C1: `cd /c/dev/live-ninja && go test ./internal/realtime/ -run 'TestEntraTokenCachedTokenIsReused|TestEntraTokenNeverInErrorString' -v -count=1 | grep -c '^--- PASS' | grep -qx 2 && echo C1_OK`. C2: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run 'TestVoiceLiveDirectResponseShape|TestVoiceLiveDirectHasNoWsUrlOrBridgeUrl' -v -count=1 | grep -c '^--- PASS' | grep -qx 2 && echo C2_OK`.
      VERIFIED. Reviewer slice: dod-sweep, mint-and-config. Raised independently by 2 reviewers.
- [ ] **D8. WS-C M2 DoD passes today and reports "[no tests to run]"** BLOCKER.
      ANCHOR: WS-C M2 (line 572-573)
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run VoiceLive` passes, including a test asserting the response contains no `wsUrl` and no `bridgeUrl` field.
      REALITY: Zero tests match `VoiceLive`; the command exits 0 with `[no tests to run]`. The most safety-critical assertion in WS-C — that legacy clients cannot mistake the Voice Live bootstrap for a Nova bridge bootstrap — is verified by a command that passes with nothing written.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run VoiceLive
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.182s [no tests to run]
      exit=0
      ```
      FIX: `test "$(cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run TestVoiceLiveResponseHasNoWsUrlOrBridgeUrl -v -count=1 2>&1 | grep -c '^--- PASS')" = 1` — fails today, passes only when that exact test exists and passes.
      VERIFIED. Reviewer slice: dod-sweep.
- [ ] **D9. WS-D M1 DoD passes today and reports "[no tests to run]" — the version gate is unverified** BLOCKER.
      ANCHOR: WS-D M1 (line 589-592)
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run 'Gate|ClientVersion'` passes, with a test asserting that a request carrying no `X-LN-Client` and a `voiceEngine.default = "gpt-live-azure"` pin receives `mode: "openai-direct"`…
      REALITY: Zero tests in `cmd/realtime-broker` match `Gate` or `ClientVersion`; the command exits 0 with `[no tests to run]`. The plan calls this gate "the reason an already-installed client can never receive an Azure credential" (line 587) — the single control that makes locked decision 3 safe — and its DoD passes with the gate entirely absent.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run 'Gate|ClientVersion'
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.151s [no tests to run]
      exit=0
      ```
      FIX: `test "$(cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run 'TestMissingClientHeaderFallsBackToOpenAI|TestOldClientVersionFallsBackToOpenAI' -v -count=1 2>&1 | grep -c '^--- PASS')" = 2`, plus a negative assertion that no Azure model id appears in the response body.
      VERIFIED. Reviewer slice: dod-sweep.
- [ ] **D10. WS-D M2 DoD passes today and reports "[no tests to run]"** BLOCKER.
      ANCHOR: WS-D M2 (line 598-599)
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run Session` passes, with a test asserting `callsUrl` is present and correct on an `openai-direct` response.
      REALITY: Zero tests match `Session` in that package; exits 0 with `[no tests to run]` while `SessionResp` still has no `CallsURL` field.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run Session
      ok  	github.com/JeremyProffittOrg/live-ninja/cmd/realtime-broker	0.163s [no tests to run]
      exit=0
      ```
      FIX: `test "$(cd /c/dev/live-ninja && go test ./cmd/realtime-broker/ -run TestOpenAIDirectResponseCarriesCallsURL -v -count=1 2>&1 | grep -c '^--- PASS')" = 1`, and add `grep -q 'CallsURL string `json:"callsUrl' /c/dev/live-ninja/cmd/realtime-broker/main.go`.
      VERIFIED. Reviewer slice: dod-sweep.
- [ ] **D11. Make WS-D M3's CSP definition of done fail closed — it is green today with the Azure origins absent** BLOCKER.
      ANCHOR: WS-D M3 ("D3. Contract and CSP"), azure-voice-plan.md:601-611
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run CSP` passes, with a test in the shape of `mobile_shell_ui_test.go:338-353` asserting both origins land **inside** the `connect-src` directive and not merely somewhere in the policy string.
      REALITY: Four tests already match the regexp `CSP` and all pass right now, with neither Azure origin in the policy. The command is therefore green before any of D3's work is done. It is green afterwards too whether or not the new test is ever written, because `go test -run <no match>` exits 0 with "[no tests to run]". The DoD can never fail, so it verifies nothing.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./internal/webapp/ -run CSP -v 2>&1 | grep -E "^(=== RUN|--- |ok)"
      === RUN   TestImportMapCSPHashMatchesTheRenderedBytes
      --- PASS: TestImportMapCSPHashMatchesTheRenderedBytes (0.02s)
      === RUN   TestPageCSPCarriesTheImportMapHashWithoutUnsafeInline
      --- PASS: TestPageCSPCarriesTheImportMapHashWithoutUnsafeInline (0.01s)
      === RUN   TestIoTOriginIsInTheCSP
      --- PASS: TestIoTOriginIsInTheCSP (0.00s)
      === RUN   TestPageCSPMatchesSpec
      --- PASS: TestPageCSPMatchesSpec (0.00s)
      PASS
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/webapp	0.233s
      
      $ grep -rn "azure" internal/webapp/pages_routes.go; echo "exit=$?"
      exit=1
      
      $ go test ./internal/webapp/ -run TestThisDoesNotExistAtAll 2>&1; echo "EXIT=$?"
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/webapp	0.180s [no tests to run]
      EXIT=0
      
      (For reference, R9 itself checks out verbatim — internal/webapp/pages_routes.go:56 is
      	"connect-src 'self' https://api.openai.com wss://generativelanguage.googleapis.com https://live-ninja-wakewords-759775734231.s3.amazonaws.com https://live-ninja-wakewords-759775734231.s3.us-east-1.amazonaws.com wss://a17oe0gnthrosw-ats.iot.us-east-1.amazonaws.com; " +
      and it is one concatenated const with a single application point (pageCSPWith at :68-73, called at :372), so adding an origin really is a one-line change.)
      ```
      FIX: Replace the DoD command with a fail-closed form that names the new test:
`cd /c/dev/live-ninja && go test ./internal/webapp/ -run CSP -v 2>&1 | grep -q '^--- PASS: TestAzureOriginsAreInTheCSP'`
Verified on this machine: that grep form exits 0 against the existing `TestIoTOriginIsInTheCSP` name and exits 1 against `TestAzureOriginsAreInTheCSP`, which does not yet exist.
      VERIFIED. Reviewer slice: client-web, dod-sweep. Raised independently by 2 reviewers.
- [ ] **D12. WS-E M2's DoD command passes today, before the R7 fix exists** BLOCKER.
      ANCHOR: WS-E M2 (azure-voice-plan.md:638-641)
      CLAIM: DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest --tests '*RealtimeSessionParseTest*'` passes, with a new case asserting an Azure `callsUrl` in the JSON is what reaches `connect()` — **and** a case asserting a response with no `callsUrl` still reaches OpenAI's.
      REALITY: `RealtimeSessionParseTest` already exists with 5 green tests and none of them touches `callsUrl`. The DoD command therefore exits 0 right now, with `parseSession` still ignoring `callsUrl` entirely. Nothing in the command detects whether the two required new cases were written. The gradle filter is not vacuous (the class matches), which makes it worse: it returns a real, green, meaningless pass.
      EVIDENCE:
      ```
      Ran the DoD verbatim (JAVA_HOME repaired, see the separate finding):
      $ cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest --tests '*RealtimeSessionParseTest*' --console=plain
      > Task :app:testDebugUnitTest
      BUILD SUCCESSFUL in 25s
      31 actionable tasks: 1 executed, 30 up-to-date
      EXIT=0
      
      $ grep -o 'tests="[0-9]*" skipped="[0-9]*" failures="[0-9]*" errors="[0-9]*"' android/app/build/test-results/testDebugUnitTest/TEST-ninja.jeremy.liveninja.realtime.RealtimeSessionParseTest.xml
      tests="5" skipped="0" failures="0" errors="0"
      
      $ grep -rn "callsUrl" android/app/src/test/java/ninja/jeremy/liveninja/realtime/RealtimeSessionParseTest.kt
      (no output — zero matches)
      
      Existing test methods (android/app/src/test/java/ninja/jeremy/liveninja/realtime/RealtimeSessionParseTest.kt): geminiDirect_parsesEndpointTokenAndSessionConfig, novaBridge_parsesRequiredSessionConfig, novaBridge_missingSessionConfig_throwsInvalidResponse, plus two more — none reference callsUrl.
      ```
      FIX: Name the two new test methods in the DoD so the command fails when they are absent: `./gradlew :app:testDebugUnitTest --tests 'ninja.jeremy.liveninja.realtime.RealtimeSessionParseTest.azureDirect_callsUrlFromJsonReachesConnect' --tests 'ninja.jeremy.liveninja.realtime.RealtimeSessionParseTest.openaiDirect_absentCallsUrlFallsBackToOpenAi'`. Gradle fails with "No tests found for given includes" when a fully-qualified method filter matches nothing, so the DoD then genuinely gates the work.
      VERIFIED. Reviewer slice: client-android, dod-sweep. Raised independently by 2 reviewers.
- [ ] **D13. WS-E M3's DoD is a bare unit-test run that is already green with zero Voice Live code** BLOCKER.
      ANCHOR: WS-E M3 (azure-voice-plan.md:643-646)
      CLAIM: E3. Android: `VoiceLiveTransport.kt`. New `RealtimeTransport` implementation … DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest` passes.
      REALITY: The command passes today across 48 test classes while `VoiceLiveTransport.kt` does not exist. It constrains the new transport in no way whatsoever: it does not require the file, does not require a test for it, and would stay green if E3 were skipped entirely. It is the single weakest DoD in the Android slice.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest --console=plain
      > Task :app:testDebugUnitTest
      BUILD SUCCESSFUL in 13s
      31 actionable tasks: 1 executed, 30 up-to-date
      EXIT=0
      
      $ ls android/app/src/main/java/ninja/jeremy/liveninja/realtime/ | grep -i voicelive
      (no output — no VoiceLiveTransport.kt)
      
      $ ls android/app/build/test-results/testDebugUnitTest/*.xml | wc -l
      48
      ```
      FIX: Replace the DoD with a filter on a test class that cannot exist before the work does, e.g. `./gradlew :app:testDebugUnitTest --tests 'ninja.jeremy.liveninja.realtime.VoiceLiveTransportTest.sdpCreateFrameCarriesOfferAndSessionConfig' --tests 'ninja.jeremy.liveninja.realtime.VoiceLiveTransportTest.sdpCreatedAnswerIsAppliedAsRemoteDescription'`, and require the new test to assert against the frame shapes rather than instantiating the native PeerConnectionFactory.
      VERIFIED. Reviewer slice: client-android, dod-sweep. Raised independently by 2 reviewers.
- [ ] **D14. WS-E M4's DoD package check already prints a package on the S9 with the app not installed** BLOCKER.
      ANCHOR: WS-E M4 (azure-voice-plan.md:665-667)
      CLAIM: DoD: `adb -s 4633424442303098 shell pm list packages | grep ninja.jeremy.liveninja` prints the package, the app launches and the process stays alive…
      REALITY: `grep` is an unanchored substring match. The S9 already has two unrelated packages whose names contain that string, so the command prints output and exits 0 today — while `ninja.jeremy.liveninja`, the applicationId this plan actually builds, is not installed on the device at all.
      EVIDENCE:
      ```
      $ adb -s 4633424442303098 shell pm list packages | grep ninja.jeremy.liveninja
      package:ninja.jeremy.liveninja.azure.test
      package:ninja.jeremy.liveninja.azure
      exit=0
      
      $ adb -s 4633424442303098 shell pm path ninja.jeremy.liveninja
      (no output — not installed)
      
      $ adb -s 4633424442303098 shell pm path ninja.jeremy.liveninja.azure
      package:/data/app/ninja.jeremy.liveninja.azure-agyA2i0Hift_tLiFZ8G2NA==/base.apk
      
      $ adb -s 4633424442303098 shell pm list users
      Users:
      	UserInfo{0:Owner:13} running
      
      The applicationId under test is `ninja.jeremy.liveninja` (android/app/build.gradle.kts:27, and android/app/build/outputs/apk/debug/output-metadata.json "applicationId": "ninja.jeremy.liveninja").
      ```
      FIX: Replace the grep with an exact-identity check: `adb -s 4633424442303098 shell pm path ninja.jeremy.liveninja` must print a `package:/data/app/...` line (or `... | grep -x 'package:ninja.jeremy.liveninja'`). Add a line telling the run that `ninja.jeremy.liveninja.azure` is a pre-existing unrelated sideload on this device and must not be mistaken for the build under test.
      VERIFIED. Reviewer slice: client-android, dod-sweep. Raised independently by 2 reviewers.
- [ ] **D15. WS-F M2 DoD passes today — TestHelpDrawer already exists and passes with no Azure copy written** BLOCKER.
      ANCHOR: WS-F M2 (line 688)
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/ -run TestHelpDrawer` passes.
      REALITY: Exits 0 today. The existing guard checks the drawer's structure, not its content, so it cannot detect a missing entry for any of the four new engines, and cannot detect that the Voice Live rows omit the mandatory "preview, no SLA, session config not enforced server-side" wording that the milestone calls non-negotiable.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./internal/webapp/ -run TestHelpDrawer
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/webapp	0.178s
      exit=0
      
      $ grep -c 'gpt-live-azure\|azure-voice-live' /c/dev/live-ninja/web/templates/pages/conversation.html
      0
      ```
      FIX: Add a content assertion to the DoD: `for s in gpt-live-azure gpt-live-azure-mini azure-voice-live azure-voice-live-lite 'public preview' 'not enforced'; do grep -q "$s" /c/dev/live-ninja/web/templates/pages/conversation.html || { echo "MISSING: $s"; exit 1; }; done` run alongside `go test ./internal/webapp/ -run TestHelpDrawer -count=1`.
      VERIFIED. Reviewer slice: docs-catalog-help, dod-sweep. Raised independently by 2 reviewers.
- [ ] **D16. WS-F M3 DoD passes today if "non-zero" is read as the exit code, and one occurrence of the string satisfies it either way** BLOCKER.
      ANCHOR: WS-F M3 (line 695-696)
      CLAIM: DoD: `grep -c 'gpt-live-azure' /c/dev/live-ninja/docs/voice-engines.md` returns non-zero and the client support matrix has a column for each new engine.
      REALITY: "Returns non-zero" is ambiguous and both readings are broken. Read as an exit code, `grep -c` returns 1 today (no match) — the DoD passes before the file is touched. Read as the printed count, one occurrence of the string anywhere in the file satisfies it, so pasting the engine name into a single sentence marks the whole documentation milestone done — the token-problem table, the cost section, and the other three engine names go unchecked. The second clause ("a column for each new engine") has no command at all; the existing matrix at `docs/voice-engines.md:194` is keyed by transport mode, not engine, so it is not even clear what would satisfy it. Separately, the plan's own scripts run under `set -euo pipefail` (`scripts/set-secret.sh:14`), where a `grep` exiting 1 aborts the script.
      EVIDENCE:
      ```
      $ grep -c 'gpt-live-azure' /c/dev/live-ninja/docs/voice-engines.md
      0
      exit=1
      
      $ sed -n '192,196p' /c/dev/live-ninja/docs/voice-engines.md
      ## Client support matrix
      
      | Surface   | OpenAI-direct | Nova-bridge | Gemini-direct | Notes |
      |-----------|:-------------:|:-----------:|:-------------:|-------|
      | Web (`realtime.mjs`)        | ✅ | ✅ | ⚠️ per-surface until verified | Triple path: WebRTC/WSS to OpenAI, WSS to the bridge, or WSS to Google. |
      ```
      FIX: Replace with an explicit multi-string check that names the real column headings: `for s in gpt-live-azure gpt-live-azure-mini azure-voice-live azure-voice-live-lite 'Azure-direct' 'Voice-Live-direct' 'The token problem, stated honestly' 'eight realtime speech-to-speech backends'; do grep -qF "$s" /c/dev/live-ninja/docs/voice-engines.md || { echo "MISSING: $s"; exit 1; }; done; echo DOCS_OK`.
      VERIFIED. Reviewer slice: dod-sweep.
- [ ] **D17. Give WS-E M1 an automated definition of done that actually exercises realtime.mjs — `go test ./internal/webapp/` never reads it** MAJOR.
      ANCHOR: WS-E M1 ("E1. Web: honour callsUrl, add the Voice Live transport"), azure-voice-plan.md:628-631
      CLAIM: DoD: `cd /c/dev/live-ninja && go test ./internal/webapp/` passes, and a browser session pinned to `gpt-live-azure` completes a turn and shows a non-zero cost badge.
      REALITY: The Go half passes right now with zero work done, and it can never fail for a realtime.mjs change: no file under internal/webapp reads web/static/js/realtime.mjs. The only mention of that file anywhere in the package is a comment. So the sole machine-checkable half of E1's DoD is unrelated to the milestone. A `node --test` harness that could check it does exist and realtime.mjs imports cleanly under Node, so this is closable without new infrastructure.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja && go test ./internal/webapp/ 2>&1; echo "EXIT=$?"
      ok  	github.com/JeremyProffittOrg/live-ninja/internal/webapp	0.237s
      EXIT=0
      
      $ grep -rn "realtime.mjs" internal/webapp/
      internal/webapp/pages_routes.go:51:	// engine's client-direct Live API socket (M13, realtime.mjs
      (one comment; zero _test.go hits)
      
      $ node -e "import('./web/static/js/realtime.mjs').then(m=>console.log('IMPORT_OK', Object.keys(m).join(',')))"
      IMPORT_OK RealtimeError,RealtimeSession,acquireMicStream,prefetchSession
      
      $ node --version && node --test "tests/web/unit/*.test.mjs" 2>&1 | tail -6
      v24.11.1
      ℹ tests 30
      ℹ pass 30
      ℹ fail 0
      EXIT=0
      
      (Note the run command documented in tests/web/unit/mqtt.test.mjs:7, "node --test tests/web/unit/", is broken on node v24.11.1:
      $ node --test tests/web/unit/
      Error: Cannot find module 'C:\dev\live-ninja\tests\web\unit'
      Use the glob form above.)
      ```
      FIX: Add a second, fail-closed DoD clause to E1 alongside the Go one:
`cd /c/dev/live-ninja && test -f tests/web/unit/realtime-callsurl.test.mjs && node --test "tests/web/unit/realtime-callsurl.test.mjs"`
The test imports `RealtimeSession` from web/static/js/realtime.mjs and asserts (a) a mint body carrying `callsUrl` is what the SDP POST targets, (b) a body with no `callsUrl` still targets `OPENAI_CALLS_URL`, and (c) a `voice-live-direct` body is accepted by `#mint`. The `test -f` prefix is required: `node --test` on a non-matching glob exits 0 (verified: `node --test "tests/web/unit/does-not-exist*.test.mjs"` → EXIT=0).
      VERIFIED. Reviewer slice: client-web.
- [ ] **D18. WS-A M5's DoD queries subscription scope while the budgets it checks for are created at resource-group scope, so it returns zero rows after the work is done** MINOR.
      ANCHOR: WS-A M5 (azure-voice-plan.md:439-446)
      CLAIM: "Two budgets, one per resource group ... DoD: `az consumption budget list --query "[?contains(name,'ln-')].[name,amount]" -o tsv` prints two rows"
      REALITY: `az consumption budget list` is scope-addressed. Without `-g` it GETs the subscription-scope collection `/subscriptions/{id}/providers/Microsoft.Consumption/budgets`; with `-g` it GETs `/subscriptions/{id}/resourceGroups/{rg}/providers/Microsoft.Consumption/budgets`. A budget created per-resource-group lives at the second URL and is invisible at the first. So after M5 is correctly done, the DoD as written still prints nothing — identical to its output right now, before any work. The run cannot tell a completed M5 from an untouched one, and this is the plan's self-declared sole spend backstop.
      EVIDENCE:
      ```
      $ az consumption budget list --debug 2>&1 | grep -iE "Request URL"
      DEBUG: cli.azure.cli.core.sdk.policies: Request URL: 'https://management.azure.com/subscriptions/adc40fff-bab3-4bd2-b961-1832d0375052/providers/Microsoft.Consumption/budgets?api-version=2023-05-01'
      
      $ az consumption budget list -g rg-liveninja-azure-prod --debug 2>&1 | grep -iE "Request URL"
      DEBUG: cli.azure.cli.core.sdk.policies: Request URL: 'https://management.azure.com/subscriptions/adc40fff-bab3-4bd2-b961-1832d0375052/resourceGroups/rg-liveninja-azure-prod/providers/Microsoft.Consumption/budgets?api-version=2023-05-01'
      
      Current output of the DoD verbatim (nothing, before any work):
      $ az consumption budget list --query "[?contains(name,'ln-')].[name,amount]" -o tsv
      WARNING: This command is in preview and under development. Reference and support levels: https://aka.ms/CLI_refstatus
      (no rows)
      ```
      FIX: Replace the WS-A M5 DoD with a per-group form that fails loudly if either group's budget is missing: `for g in ln-azure-openai-rg ln-voicelive-rg; do az consumption budget list -g "$g" --query "[].[name,amount]" -o tsv; done | wc -l` must print `2`.
      VERIFIED. Reviewer slice: azure-resources.

## Milestones that name the wrong mechanism

- [ ] **W1. WS-A M6 DoD is not a valid AWS CLI command, is MSYS-mangled, and names a script that does not write SSM** BLOCKER.
      ANCHOR: WS-A M6 (line 446-453)
      CLAIM: "Three SecureString parameters, all written by `/c/dev/live-ninja/scripts/set-secret.sh` … DoD: `aws ssm get-parameters-by-path --path /live-ninja/prod/azure --query "Parameters[].Name" -o text` prints three names."
      REALITY: Three independent faults. (1) `-o` is not an AWS CLI option — the command exits 252 without ever calling AWS. (2) In Git Bash the bare `/live-ninja/prod/azure` is path-mangled to `C:/Program Files/Git/live-ninja/prod/azure` and AWS rejects it; only `MSYS_NO_PATHCONV=1` makes it reach the API. (3) `scripts/set-secret.sh` writes GitHub Actions repository secrets via `gh secret set`, not SSM, and rejects the names outright — it enforces `^[A-Z][A-Z0-9_]*$`, so `/live-ninja/prod/azure/openai_api_key` fails at line 20. SSM parameters are created only by `.github/workflows/deploy.yml:292` (`aws ssm put-parameter --type SecureString`) during a push-to-`main` deploy, and the plan adds no milestone that extends that block. As written the DoD can never print three names.
      EVIDENCE:
      ```
      $ aws ssm get-parameters-by-path --path /live-ninja/prod/azure --query "Parameters[].Name" -o text
      Unknown options: -o, text
      exit=252
      
      $ python -c "import sys;print(sys.argv[1])" /live-ninja/prod/azure
      C:/Program Files/Git/live-ninja/prod/azure
      
      $ MSYS_NO_PATHCONV=1 aws ssm get-parameters-by-path --path /live-ninja --recursive --query "Parameters[].Name" --output text
      /live-ninja/prod/device/cred_pepper	/live-ninja/prod/gemini/api_key	/live-ninja/prod/gemini/service_account_json	/live-ninja/prod/lwa/client_id	/live-ninja/prod/lwa/client_secret	/live-ninja/prod/openai/api_key
      
      $ sed -n '14,20p' /c/dev/live-ninja/scripts/set-secret.sh
      set -euo pipefail
      export MSYS_NO_PATHCONV=1
      
      NAME="${1:-}"
      [ -n "$NAME" ] || { echo "usage: $0 SECRET_NAME [--file PATH | --generate]" >&2; exit 2; }
      [[ "$NAME" =~ ^[A-Z][A-Z0-9_]*$ ]] || { echo "ERROR: secret names are UPPER_SNAKE_CASE" >&2; exit 2; }
      
      $ sed -n '292,296p' /c/dev/live-ninja/.github/workflows/deploy.yml
                aws ssm put-parameter \
                  --name "/live-ninja/prod/openai/api_key" \
                  --type SecureString \
                  --value "$OPENAI_API_KEY" \
                  --overwrite >/dev/null
      ```
      FIX: Restate the milestone as: operator runs `scripts/set-secret.sh AZURE_OPENAI_API_KEY`, `AZURE_VOICELIVE_CLIENT_SECRET`, `AZURE_VOICELIVE_CLIENT_ID` (GitHub secrets), the run adds three matching `aws ssm put-parameter` blocks to `.github/workflows/deploy.yml` beside line 292 and pushes. New DoD: `MSYS_NO_PATHCONV=1 aws ssm get-parameters-by-path --path /live-ninja/prod/azure --query "length(Parameters)" --output text` prints `3`.
      VERIFIED. Reviewer slice: dod-sweep, secrets-and-pipeline. Raised independently by 2 reviewers.
- [ ] **W2. R6's "two-line addition" omits the broker's IAM scope and the deploy workflow's SSM sync — every Azure credential read fails with AccessDenied in production** BLOCKER.
      ANCHOR: Verified facts R6 (azure-voice-plan.md:162-166); WS-B M3 (:531-545); WS-C M1 (:551-563)
      CLAIM: R6: "An Azure parameter is a two-line addition to that file." WS-B M3: "the auth header (`api-key: <key>` from `config.ParamAzureOpenAIAPIKey`, resolved through the same `config.Loader` — R6) ... Add the two config constants to `/c/dev/live-ninja/internal/config/config.go`." WS-C M1: "client id and secret from SSM (A6)."
      REALITY: The `config.go` half of R6 is correct — mint.go:319 is exactly where the key resolves, and `cacheTTL = 5 * time.Minute` is at config.go:51. But two lines in config.go are not sufficient for anything to work in production. (a) The broker Lambda's IAM policy grants `ssm:GetParameter` on two literal parameter ARNs with no wildcard (template.yaml:727-738); `/live-ninja/prod/azure/*` is not among them, so the first Azure mint and the first Entra exchange both fail with AccessDeniedException. (b) The deploy workflow's "Sync secrets to SSM" step (.github/workflows/deploy.yml:282-313) is a hardcoded list of four `aws ssm put-parameter` calls with no azure entry, so WS-A M6's SSM parameters are never created by the pipeline at all. The plan names `template.yaml` exactly once (line 451, for plain env vars) and never names `.github/workflows/deploy.yml`. Every DoD in WS-B and WS-C is a local `go test`, so nothing catches this before WS-F.
      EVIDENCE:
      ```
      $ sed -n '727,746p' template.yaml
              - Sid: OpenAiKeyParam
                Effect: Allow
                Action:
                - ssm:GetParameter
                Resource:
                - !Sub "arn:${AWS::Partition}:ssm:${AWS::Region}:${AWS::AccountId}:parameter/live-ninja/prod/openai/api_key"
              - Sid: GeminiKeyParam
                Effect: Allow
                Action:
                - ssm:GetParameter
                Resource:
                - !Sub "arn:${AWS::Partition}:ssm:${AWS::Region}:${AWS::AccountId}:parameter/live-ninja/prod/gemini/api_key"
      
      $ grep -n "put-parameter" .github/workflows/deploy.yml
      292:          aws ssm put-parameter \
      298:          aws ssm put-parameter \
      304:          aws ssm put-parameter \
      310:          aws ssm put-parameter \
      (names: openai/api_key, gemini/api_key, lwa/client_id, lwa/client_secret — no azure)
      
      $ grep -n "template.yaml\|deploy.yml" azure-voice-plan.md
      451:      secrets** — they go in `template.yaml` as plain environment variables.
      ```
      FIX: Correct R6 to "two constants in `config.go`, plus one IAM statement in `template.yaml` and one env-mapped `put-parameter` per credential in `.github/workflows/deploy.yml`." Add a milestone WS-B M3a (blocking WS-F): add Sid `AzureParams` to `RealtimeBrokerFunction`'s policy at template.yaml:727 granting `ssm:GetParameter` on `arn:${AWS::Partition}:ssm:${AWS::Region}:${AWS::AccountId}:parameter/live-ninja/prod/azure/*`, and add three `aws ssm put-parameter --type SecureString` calls with matching `env:` entries to the "Sync secrets to SSM" step. DoD after the deploy: `aws lambda invoke --function-name live-ninja-realtime-broker ... ` is not required — use `aws ssm get-parameters-by-path --path /live-ninja/prod/azure --query "Parameters[].Name" -o text` printing three names, and `grep -c 'live-ninja/prod/azure' template.yaml .github/workflows/deploy.yml` printing non-zero for both.
      VERIFIED. Reviewer slice: mint-and-config.
- [ ] **W3. WS-D M1 gates on a header the broker can never see — every client fails closed** BLOCKER.
      ANCHOR: WS-D M1 (azure-voice-plan.md:581-592)
      CLAIM: "In the broker's engine resolution, **before** any Azure mint: if the resolved engine is one of the four new ones and `X-LN-Client` is absent, unparseable, or below the per-surface minimum, route to `openai-realtime` instead... It fails closed by construction: an unknown client is an old client."
      REALITY: The realtime-broker is a separate Lambda invoked with a hand-constructed JSON payload, not a forwarded HTTP request. It receives no headers of any kind. `X-LN-Client` is therefore ALWAYS absent inside the broker, so the gate as written routes 100% of sessions — web, Android and m5stack, current and future — to `openai-realtime` permanently. No Azure engine can ever be reached, which makes WS-F M1 ("four sessions, four transcripts") unpassable and silently voids WS-B M2/B3/B6 and WS-C M2.
      EVIDENCE:
      ```
      $ grep -c "X-LN-Client" cmd/realtime-broker/main.go cmd/realtime-broker/main_test.go cmd/realtime-broker/gemini_test.go
      cmd/realtime-broker/main.go:0
      cmd/realtime-broker/main_test.go:0
      cmd/realtime-broker/gemini_test.go:0
      
      $ grep -rn "Headers\|header" cmd/realtime-broker/*.go
      cmd/realtime-broker/main.go:115:	// header and to fill the canonical error envelope's txId so a
      cmd/realtime-broker/main.go:153:	// QuotaWarning is the ready-to-emit X-LN-Quota-Warning header value
      
      cmd/realtime-broker/main.go:232: func (b *broker) Handle(ctx context.Context, req Request) (resp Response, _ error) {
      cmd/realtime-broker/main.go:909: 	lambda.Start(b.Handle)
      
      cmd/realtime-broker/main.go:65-82 (the ENTIRE invoke event shape — no header, no client version):
      type Request struct {
      	Mode string `json:"mode,omitempty"`
      	TxID          string `json:"txId,omitempty"`
      	UserID        string `json:"userId"`
      	Surface       string `json:"surface"`
      	DeviceID      string `json:"deviceId,omitempty"`
      	Persona       string `json:"persona,omitempty"`
      	VoiceOverride string `json:"voiceOverride,omitempty"`
      	MicEagerness string          `json:"micEagerness,omitempty"`
      	Payload      json.RawMessage `json:"payload,omitempty"`
      }
      
      internal/webapp/api_routes.go:485-493 (the only caller for session-mint — nothing header-derived is passed):
      		resp, err := invokeRealtimeBroker(c.Context(), deps, brokerRequest{
      			TxID:          TxID(c),
      			UserID:        userID,
      			Surface:       surface,
      			DeviceID:      deviceID,
      			Persona:       persona,
      			VoiceOverride: voice,
      			MicEagerness:  eagerness,
      		})
      
      internal/webapp/api_routes.go:373-376 (direct Lambda:Invoke of a marshaled struct — no HTTP request object):
      	out, err := deps.Lambda.Invoke(ctx, &lambda.InvokeInput{
      		FunctionName: aws.String(deps.BrokerFn),
      		Payload:      payload,
      	})
      
      internal/webapp/version.go:223-228 — VersionMiddleware is a fiber handler, and cmd/web/main.go:96 `app.Use(webapp.VersionMiddleware(deps))` mounts it on the WEB Lambda's fiber app only. The broker has no fiber app.
      ```
      FIX: Rewrite WS-D M1 to carry the header value across the invoke boundary as a fourth named edit: (a) add `ClientVersion string `json:"clientVersion,omitempty"`` to `Request` (cmd/realtime-broker/main.go:65-82); (b) add the same field to `brokerRequest` (internal/webapp/api_routes.go:272-289); (c) set `ClientVersion: c.Get("X-LN-Client")` at the session-mint call site (internal/webapp/api_routes.go:485-493); (d) gate on `req.ClientVersion` inside the broker. State explicitly that the broker never receives HTTP headers.
      VERIFIED. Reviewer slice: wire-and-gate. Raised independently by 2 reviewers.
- [ ] **W4. WS-D M2's callsUrl is dropped by the web function and never reaches any client** BLOCKER.
      ANCHOR: WS-D M2 (azure-voice-plan.md:594-600)
      CLAIM: "Add `CallsURL string `json:\"callsUrl,omitempty\"`` to `SessionResp` (`/c/dev/live-ninja/cmd/realtime-broker/main.go:130-156`). Emit it on **both** `openai-direct` (the existing OpenAI URL, so the field is exercised by the default path from day one and cannot rot) and the new `azure-direct` mode."
      REALITY: Editing only the broker puts nothing on the HTTP wire. The web function decodes the broker reply into its own mirror struct `brokerResponse` (internal/webapp/api_routes.go:289) with plain `json.Unmarshal` — no `DisallowUnknownFields`, so an unknown `callsUrl` key is SILENTLY DISCARDED — and then hand-builds the client-facing body as an explicit allowlist `fiber.Map` (api_routes.go:556-569). `callsUrl` is in neither. The field is therefore never 'exercised by the default path', it cannot 'rot' because it is never live, and WS-E M1 (web reads `callsUrl`) and WS-E M2 (Android `parseSession` reads `callsUrl`) are both unsatisfiable — which leaves R7's blocking client defect open while the plan believes it closed.
      EVIDENCE:
      ```
      internal/webapp/api_routes.go:373-386 — decode is non-strict:
      	out, err := deps.Lambda.Invoke(ctx, &lambda.InvokeInput{...})
      	...
      	var resp brokerResponse
      	if err := json.Unmarshal(out.Payload, &resp); err != nil {
      
      Only these mint fields exist in the mirror (api_routes.go:304-330): Mode, Engine, ClientSecret, Model, Voice, SessionConfig, ToolManifest, SessionID, WSURL, BridgeToken, BridgeTokenExpiresAt, GeminiEndpoint, AccessToken, QuotaWarning. No CallsURL.
      
      internal/webapp/api_routes.go:556-569 — the openai-direct HTTP body is an explicit allowlist:
      		return c.JSON(fiber.Map{
      			"mode":          mode,
      			"engine":        resp.Engine,
      			"clientSecret":  resp.ClientSecret,
      			"model":         resp.Model,
      			"voice":         resp.Voice,
      			"sessionConfig": resp.SessionConfig,
      			"toolManifest":  resp.ToolManifest,
      			"sessionId":     resp.SessionID,
      			"rates": realtime.RatesFor(resp.Model),
      		})
      
      No client parser is strict, so the extra field itself is harmless once it does arrive: web/static/js/realtime.mjs reads named keys off a JS object; android RealtimeSessionApi.kt:167-227 uses org.json `optString`/`optJSONObject`; firmware/components/ln_realtime/ln_rt_session.c uses cJSON `cJSON_GetObjectItemCaseSensitive` with a 64 KB body cap (`#define LN_RT_HTTP_BODY_CAP (64 * 1024)`, :71). The hazard is the drop, not the addition.
      ```
      FIX: WS-D M2 must name three edits, not one: (1) `CallsURL string `json:"callsUrl,omitempty"`` on `Response` in cmd/realtime-broker/main.go:112-172; (2) the same field on `brokerResponse` in internal/webapp/api_routes.go:289-340; (3) `"callsUrl": resp.CallsURL,` added to the openai-direct `fiber.Map` at internal/webapp/api_routes.go:556-569 and to the new azure-direct map. Extend the DoD to cover the HTTP body, e.g. `go test ./internal/webapp/ -run TestSessionResponseCarriesCallsURL -v` must print `--- PASS`.
      VERIFIED. Reviewer slice: wire-and-gate.
- [ ] **W5. WS-A M2's DoD fails even when the resource is created exactly as specified: kind AIServices does not put openai.azure.com in properties.endpoint** MAJOR.
      ANCHOR: WS-A M2 (azure-voice-plan.md:415-420)
      CLAIM: "A2. Create the Azure OpenAI (Microsoft Foundry) resource. Name `ln-aoai-eastus2`, kind `AIServices`, region `eastus2` (A3), with a custom subdomain — the `<resource>.openai.azure.com` host form in A1 requires one. DoD: `az cognitiveservices account show -n ln-aoai-eastus2 -g ln-azure-openai-rg --query "properties.endpoint" -o tsv` prints an `https://ln-aoai-eastus2.openai.azure.com/`-shaped URL."
      REALITY: For kind `AIServices`, ARM sets the scalar `properties.endpoint` to the `.cognitiveservices.azure.com` form. The `.openai.azure.com` host exists, but only inside the `properties.endpoints` MAP, under keys such as "OpenAI Realtime API". So the DoD command prints `https://ln-aoai-eastus2.cognitiveservices.azure.com/`, does not match the asserted shape, and an unattended run reads a correctly-provisioned resource as a failed milestone. It will then either loop on M2 or fall back to kind `OpenAI`, and everything downstream (WS-B M1's mint URL, WS-B/WS-E endpoint config) is built on a host the plan never actually confirmed. Proven against the live subscription, which already holds an `AIServices` account in `eastus2` with a custom subdomain — the same configuration M2 specifies.
      EVIDENCE:
      ```
      $ az cognitiveservices account list -o table
      Kind        Location    Name                     ResourceGroup
      ----------  ----------  -----------------------  -----------------------
      AIServices  eastus2     ai-liveninja-1339377283  rg-liveninja-azure-prod
      
      $ az cognitiveservices account show -n ai-liveninja-1339377283 -g rg-liveninja-azure-prod --query "{kind:kind,endpoint:properties.endpoint,subdomain:properties.customSubDomainName,endpoints:properties.endpoints}" -o json
      {
        "endpoint": "https://ai-liveninja-1339377283.cognitiveservices.azure.com/",
        "endpoints": {
          "AI Foundry API": "https://ai-liveninja-1339377283.services.ai.azure.com/",
          ...
          "OpenAI Realtime API": "https://ai-liveninja-1339377283.openai.azure.com/",
          ...
        },
        "kind": "AIServices",
        "subdomain": "ai-liveninja-1339377283"
      }
      
      The .openai.azure.com host IS the right one for the mint (docs confirm), it is just not what the DoD's query returns:
      learn.microsoft.com/en-us/azure/ai-foundry/openai/how-to/realtime-audio-webrtc line 143:
        url = https://{your azure resource}.openai.azure.com/openai/v1/realtime/client_secrets
      line 151:
        - **GA (current)**: `/openai/v1/realtime/client_secrets` (no API version parameter needed)
      ```
      FIX: Change the WS-A M2 DoD to query the endpoints map rather than the scalar: `az cognitiveservices account show -n ln-aoai-eastus2 -g ln-azure-openai-rg --query "properties.endpoints.\"OpenAI Realtime API\"" -o tsv` must print `https://ln-aoai-eastus2.openai.azure.com/`. Add one sentence to M2 recording that `properties.endpoint` on kind AIServices is the `.cognitiveservices.azure.com` form and is not the host WS-B M1 uses, so the fallback-to-kind-OpenAI reflex is not triggered.
      VERIFIED. Reviewer slice: azure-resources.
- [ ] **W6. WS-B M5's rate rows are keyed on A12's dotted model ids while WS-B M3 sends the hyphenated deployment name, so RatesFor falls through to defaultRates and re-creates the R10 defect on the new engines** MAJOR.
      ANCHOR: WS-B M5 (azure-voice-plan.md:515); WS-A M3 (:421-427); WS-B M3 (:495); "## What ships" (:330-331)
      CLAIM: WS-B M5: "Add rows for the deployed Azure model ids using A12's figures". WS-B M3: "and the model id (the deployment name from A3)". WS-A M3: "Deploy `gpt-realtime-2.1` as a **GlobalStandard** deployment named `gpt-realtime-2-1`". A12 lists rates for `gpt-realtime-2.1` and `gpt-realtime-2.1-mini`.
      REALITY: The plan uses three different spellings for the same thing and never says which one keys the rate table. The cost badge calls `realtime.RatesFor(resp.Model)`, and `resp.Model` is what the minter sent — per WS-B M3 that is the deployment name `gpt-realtime-2-1`. If WS-B M5 keys the rows on A12's ids (`gpt-realtime-2.1`), `RatesFor` misses and silently returns `defaultRates`, which is the full `gpt-realtime` table. `gpt-live-azure-mini` then bills at 32.00/64.00 instead of 10.00/20.00 in the badge — the exact silent-fallback defect R10 says this plan is fixing. Nothing catches it: WS-E M1's DoD only asks for "a non-zero cost badge", which the fallback satisfies, and WS-B M5's `TestRatesCoverEveryShippedEngine` is written against "model id reachable from the eight engine constants", which is the ambiguous term itself.
      EVIDENCE:
      ```
      internal/webapp/api_routes.go:553  `"rates":          realtime.RatesFor(resp.Model),`
      internal/webapp/api_routes.go:568  `"rates": realtime.RatesFor(resp.Model),`
      internal/realtime/rates.go:54  `var defaultRates = modelRates["gpt-realtime"]`
      internal/realtime/rates.go:58-63:
      	func RatesFor(model string) Rates {
      		if r, ok := modelRates[model]; ok {
      			return r
      		}
      		return defaultRates
      	}
      azure-voice-plan.md:330  table says Model `gpt-realtime-2.1`
      azure-voice-plan.md:422  deployment "named `gpt-realtime-2-1`"
      azure-voice-plan.md:464  B1's curl sends `"model":"gpt-realtime-2-1"`
      ```
      FIX: In WS-B M5 replace "the deployed Azure model ids" with: "the **deployment names recorded verbatim in WS-A M3's Execution log entry** (e.g. `gpt-realtime-2-1`, `gpt-realtime-2-1-mini`) — these, not A12's dotted marketing ids, are the strings `RatesFor(resp.Model)` receives." Add to its DoD: "and a test asserting `RatesFor(<the deployed name>) != RatesFor(\"gpt-realtime\")` for the mini deployment."
      VERIFIED. Reviewer slice: completeness-critic.
- [ ] **W7. "Ship with the cost badge suppressed" is not achievable — the badge unhides itself on sessionready regardless of rates and displays ~$0.000 for the whole session** MAJOR.
      ANCHOR: WS-B M5 bullet 3 (azure-voice-plan.md:519-521) and "Not stop conditions" (:388-389)
      CLAIM: "If the published rates cannot be found, ship the engines with `rates_missing` logged and the cost badge suppressed — do not invent a number and do not let the silent default cover it." Repeated at :389: "record `rates_missing` and ship the engine with the badge suppressed".
      REALITY: No code path suppresses the badge. `attachCostBadge` (web/static/js/conversation.mjs:1079) unhides it in the `sessionready` handler with no rates check at all (:1086 `costBadgeEl.hidden = false;` then :1087 `renderCostBadge();`), and `renderCostBadge` (:1040-1042) writes `formatCostUSD(0)` = `~$0.000`. The only rates guard is inside the `usage` listener (:1090-1091), which stops the total from updating but never hides the element. So a Voice Live session with unknown rates shows a permanently-zero dollar figure on a paid call — a wrong number, which is what the milestone says not to ship. Worse, the plan's own mechanism makes it likelier: `RatesForEngine` returns `(Rates, bool)`, and `Rates`'s json tags carry no `omitempty` (rates.go:10-15), so serialising the zero value gives the client `{"textInPer1M":0,...}`, which is truthy — `if (!rates) return` does not fire and every turn prices at exactly $0.00. Nothing in M5's DoD (`go test ./internal/realtime/ -run Rates` plus the coverage test) touches the bootstrap payload or the client.
      EVIDENCE:
      ```
      web/static/js/conversation.mjs:1081-1088 (verbatim):
        session.addEventListener('sessionready', (e) => {
          const sid = (e.detail && e.detail.sessionId) || '';
          if (sid) lastSessionId = sid;
          sessCost = trackSessionCost(sid);
          if (!costBadgeEl) return;
          costBadgeEl.hidden = false;
          renderCostBadge();
        });
      web/static/js/conversation.mjs:1040-1042 (verbatim):
      function renderCostBadge() {
        if (!costBadgeEl) return;
        costBadgeEl.textContent = formatCostUSD(costTotalUSD);
      web/static/js/conversation.mjs:1090-1091 (verbatim):
          const rates = session.rates;
          if (!rates) return; // nova-bridge, or a bootstrap that omitted rates
      internal/realtime/rates.go:10-15 — json tags are `json:"textInPer1M"` etc., none with omitempty.
      ```
      FIX: Add to WS-B M5: "Suppression is two changes, not one. (a) Server: when `RatesForEngine` returns false, OMIT the `rates` key from the bootstrap map entirely (internal/webapp/api_routes.go:553 and :568) — never serialise a zero `Rates`. (b) Client: gate `costBadgeEl.hidden = false` on `session.rates` in the `sessionready` handler at web/static/js/conversation.mjs:1086, same condition as :1091." Extend the DoD with: "`go test ./internal/webapp/ -run Bootstrap` asserts the JSON for a rates-missing engine has no `rates` key."
      VERIFIED. Reviewer slice: cost-and-rates.
- [ ] **W8. Add the voice-live-direct shape check to #mint — the catch-all at realtime.mjs:283 rejects it before the mode dispatch is ever reached** MAJOR.
      ANCHOR: WS-E M1, the "Add a `voice-live-direct` branch" sentence, azure-voice-plan.md:620-627
      CLAIM: Add a `voice-live-direct` branch: open the control WSS with the Entra token in the `Authorization` query parameter (URL-encoded — A7), send `rtc.call.sdp.create` … (the milestone names realtime.mjs:98 and :778-785 as the edit sites and no other).
      REALITY: The mode dispatch in `connect()` is not the first gate. `#mint()` validates the response shape before `connect()` ever branches, and its final clause is a catch-all that demands `clientSecret.value`. WS-C M2's Voice Live response carries `voiceLiveEndpoint`, `accessToken: {value, expiresAt}` and `sessionConfig` and deliberately no `clientSecret`, so every `voice-live-direct` mint throws `mint_failed` — "The voice service returned an invalid session." — at realtime.mjs:284, before the new branch runs. Implemented exactly as E1 is written, the Voice Live engine is dead on arrival, and the failure surfaces as a generic mint error that reads like a broker fault. Neither half of E1's DoD would catch it (findings 2 and 3). `azure-direct` is unaffected: it carries an `ek_…` in `clientSecret.value`, so it passes the same catch-all and falls through the dispatch `else` at :656-657 into `#connectOpenAI` as the plan expects.
      EVIDENCE:
      ```
      web/static/js/realtime.mjs:266-285, verbatim:
      266:  const mode = body && body.mode ? body.mode : 'openai-direct';
      267:  if (mode === 'nova-bridge') {
      268:    if (!body || !body.wsUrl || !body.sessionConfig) {
      269:      throw new RealtimeError('mint_failed', 'The voice service returned an invalid Nova session.');
      270:    }
      271:  } else if (mode === 'gemini-direct') {
      ...
      283:  } else if (!body || !body.clientSecret || !body.clientSecret.value) {
      284:    throw new RealtimeError('mint_failed', 'The voice service returned an invalid session.');
      285:  }
      
      web/static/js/realtime.mjs:648-658, verbatim (the dispatch that is never reached):
      648:    if (this.#mode === 'nova-bridge') {
      652:    } else if (this.#mode === 'gemini-direct') {
      656:    } else {
      657:      await this.#connectOpenAI(minted, rtc, t0, bootstrapMs);
      658:    }
      
      azure-voice-plan.md:552-556 (WS-C M2, the shape that gets rejected):
      "`handleVoiceLiveDirect` returns `mode: \"voice-live-direct\"` with `voiceLiveEndpoint` … `accessToken: {value, expiresAt}` carrying the **observed** `exp` from the token, and `sessionConfig`"
      ```
      FIX: Add one sentence to E1: "Also add a `voice-live-direct` arm to the shape validation in `#mint()` (`/c/dev/live-ninja/web/static/js/realtime.mjs:266-285`), mirroring the `gemini-direct` arm at `:271-282` — require `body.voiceLiveEndpoint`, `body.accessToken.value` and `body.sessionConfig`. Without it the catch-all at `:283` throws `mint_failed` before the dispatch at `:648-658` runs, and no Voice Live session can ever start."
      VERIFIED. Reviewer slice: client-web.

## Work with no milestone at all

- [ ] **M1. No milestone gets the run from the current alexa-version worktree onto main; git checkout main aborts on the modified plan.md** BLOCKER.
      ANCHOR: Locked decision 4 ("Work lands on `main`. Each milestone commits and pushes to `main`"); Standing authorizations ("NOT granted: Touching the `alexa-version` branch")
      CLAIM: The plan assumes every milestone commits and pushes to main, and forbids touching the alexa-version branch, but contains no step that moves the working tree to main.
      REALITY: HEAD is on alexa-version, 11 commits ahead of main. plan.md is modified in the worktree AND differs between the two branches, in the same leading hunk. `git checkout main` therefore aborts with "Your local changes to the following files would be overwritten by checkout: plan.md" — an unattended-fatal stall on the run's very first action, before WS-A M1. The plan itself (azure-voice-plan.md) is untracked, so it survives a switch, but the local plan.md edit that cross-references it does not, and the plan never says whether that edit should be carried to main, stashed, or dropped.
      EVIDENCE:
      ```
      $ git rev-parse --abbrev-ref HEAD
      alexa-version
      $ git status --short
       M plan.md
      ?? azure-voice-plan.md
      ?? bash.exe.stackdump
      ?? update-report.md.prev
      $ git rev-list --left-right --count main...alexa-version
      0	11
      $ git diff --stat main alexa-version -- plan.md
       plan.md | 4 ++++
       1 file changed, 4 insertions(+)
      $ git diff main alexa-version -- plan.md | head -8
      @@ -1,5 +1,9 @@
       # Plan
      +> **On the `alexa-version` branch only:** migration work to Azure is governed by
      $ git diff -- plan.md | head -8
      @@ -3,6 +3,12 @@
      +> **Adding Azure voice engines is governed by [azure-voice-plan.md](azure-voice-plan.md)** ...
      $ grep -n 'checkout\|merge\|rebase' azure-voice-plan.md
      (no output)
      ```
      FIX: Add a WS-A M0 precondition milestone with the exact commands: `cd /c/dev/live-ninja && git stash push -- plan.md && git checkout main && git pull --ff-only`, then re-apply the azure-voice-plan.md cross-reference to main's copy of plan.md by hand (the alexa-version banner must not be carried across). DoD: `git rev-parse --abbrev-ref HEAD` prints `main` and `git status --short` shows no `M plan.md`.
      VERIFIED. Reviewer slice: secrets-and-pipeline.
- [ ] **M2. Missing milestone: nothing in the plan edits .github/workflows/deploy.yml, the only thing that writes /live-ninja/prod/*** BLOCKER.
      ANCHOR: WS-A M6; Locked decision 1 ("read through the existing config.Loader exactly like ParamOpenAIAPIKey"); WS-B M3
      CLAIM: Locked decision 1 asserts the Azure key is handled "exactly like ParamOpenAIAPIKey", and WS-B M3 says the change is "Add the two config constants to /c/dev/live-ninja/internal/config/config.go beside ParamGeminiAPIKey".
      REALITY: For a parameter to exist in a deployed Lambda the way ParamOpenAIAPIKey does, five artifacts must change. The plan names only two (config.go in WS-B M3, and template.yaml env vars in WS-A M6). It never names .github/workflows/deploy.yml as a file to edit — the file is mentioned exactly once in the whole plan, at R14, purely as a statement of fact about its triggers. Without a new `env:` entry and a new `aws ssm put-parameter` block in that step, no /live-ninja/prod/azure/* parameter ever comes into existence, no matter what else the run does.
      EVIDENCE:
      ```
      $ grep -n 'deploy\.yml\|workflow\|gh secret\|GitHub secret' azure-voice-plan.md
      217:  only thing that touches AWS; no local `sam deploy`. `.github/workflows/deploy.yml` is
      218:  `on: push: branches: [main]` plus `workflow_dispatch`. AWS auth is OIDC via
      (no other hit; 'gh secret' and 'GitHub secret' return nothing)
      $ grep -n 'template\.yaml' azure-voice-plan.md
      451:      secrets** — they go in `template.yaml` as plain environment variables.
      $ grep -rn 'ssm put-parameter' .github/workflows/
      .github/workflows/deploy.yml:292:          aws ssm put-parameter \
      .github/workflows/deploy.yml:298:          aws ssm put-parameter \
      .github/workflows/deploy.yml:304:          aws ssm put-parameter \
      .github/workflows/deploy.yml:310:          aws ssm put-parameter \
      .github/workflows/deploy.yml:323:            aws ssm put-parameter \
      ```
      FIX: Add a milestone (WS-A M6b) that edits `.github/workflows/deploy.yml`: add `AZURE_OPENAI_API_KEY: ${{ secrets.AZURE_OPENAI_API_KEY }}` and `AZURE_VOICELIVE_CLIENT_SECRET: ${{ secrets.AZURE_VOICELIVE_CLIENT_SECRET }}` to the `env:` block at :284-288, and two `aws ssm put-parameter --type SecureString --overwrite` calls mirroring :292-296. State explicitly that the step must not be added before the GitHub secrets exist, because an unset secret expands to the empty string and `put-parameter` with an empty value fails the whole `deploy` job — i.e. it breaks every production deploy of the repo.
      VERIFIED. Reviewer slice: secrets-and-pipeline. Raised independently by 2 reviewers.
- [ ] **M3. Missing milestone: nothing edits internal/webapp/api_routes.go, so WS-C M2's voiceLiveEndpoint and accessToken never reach any client** BLOCKER.
      ANCHOR: WS-C M2 (azure-voice-plan.md:561-573); WS-B M6 (:530-543)
      CLAIM: WS-C M2: "`handleVoiceLiveDirect` returns `mode: "voice-live-direct"` with `voiceLiveEndpoint` (…), `accessToken: {value, expiresAt}` … and `sessionConfig`". Its DoD is `go test ./cmd/realtime-broker/ -run VoiceLive`.
      REALITY: The broker's SessionResp is not what the client sees. The web function re-serialises the broker response into an explicit per-mode fiber.Map with hand-listed keys, and it has branches only for `nova-bridge`, `gemini-direct`, and a default. A `voice-live-direct` response falls into the default map, which emits mode/engine/clientSecret/model/voice/sessionConfig/toolManifest/sessionId/rates and drops `voiceLiveEndpoint` and `accessToken` entirely. The string `api_routes.go` appears zero times in the plan, so no milestone adds the branch. Both Voice Live engines therefore cannot function no matter how correct WS-C M1 and M2 are, and WS-C M2's broker-only DoD cannot detect it.
      EVIDENCE:
      ```
      $ grep -c 'api_routes.go' azure-voice-plan.md
      0
      
      internal/webapp/api_routes.go:526-536 (nova-bridge branch), :538-554 (gemini-direct branch), :555-568 (default):
      		return c.JSON(fiber.Map{
      			"mode":          mode,
      			"engine":        resp.Engine,
      			"clientSecret":  resp.ClientSecret,
      			"model":         resp.Model,
      			"voice":         resp.Voice,
      			"sessionConfig": resp.SessionConfig,
      			"toolManifest":  resp.ToolManifest,
      			"sessionId":     resp.SessionID,
      			"rates": realtime.RatesFor(resp.Model),
      		})
      
      $ grep -rn 'voiceLiveEndpoint' internal/ web/ android/
      (no output)
      ```
      FIX: Add a milestone to WS-C: "**C3. Pass the new bootstrap shapes through the web function.** In /c/dev/live-ninja/internal/webapp/api_routes.go add `azure-direct` and `voice-live-direct` branches to the mode switch at :522-568, emitting `callsUrl` (azure-direct) and `voiceLiveEndpoint` + `accessToken` (voice-live-direct). DoD: `go test ./internal/webapp/ -run TestSessionVoiceLiveShape` passes, with a new test asserting a broker voice-live-direct response reaches the HTTP body with a non-empty `voiceLiveEndpoint` and `accessToken.value`."
      VERIFIED. Reviewer slice: completeness-critic.
- [ ] **M4. Missing milestone: nothing bumps android versionCode/versionName, so WS-D M1's minimum cannot distinguish the fixed client from the broken one** BLOCKER.
      ANCHOR: WS-D M1 (azure-voice-plan.md:586, "Set the minimums to the versions WS-E actually ships"); WS-E M2/M3/M4 (:633-669)
      CLAIM: WS-D M1: "Set the minimums to the versions WS-E actually ships." and "**This gate is the reason an already-installed client can never receive an Azure credential.** It fails closed by construction: an unknown client is an old client."
      REALITY: The Android X-LN-Client value is built from BuildConfig.VERSION_NAME/VERSION_CODE, which come from android/app/build.gradle.kts. WS-E M2 and WS-E M3 edit Kotlin source only; no milestone in the plan edits build.gradle.kts, and the string 'versionCode'/'versionName' does not appear anywhere in the plan. So the APK WS-E M4 builds reports the identical header value as the build already installed on every device — `android/0.2.2-hal+r5`. "The version WS-E actually ships" is therefore the same version the unfixed client already sends, and any minimum set to it admits the unfixed client. The gate cannot fail closed; it fails open for exactly the clients it exists to block. This repo bumps the version deliberately in each release commit, so the omission is not covered by an implicit convention.
      EVIDENCE:
      ```
      $ grep -n 'version\|minSdk' android/app/build.gradle.kts
      27:        minSdk = 29
      28:        targetSdk = 35
      29:        versionCode = 5
      30:        versionName = "0.2.2-hal"
      
      android/app/src/main/java/ninja/jeremy/liveninja/net/AuthInterceptor.kt:17:
          val HEADER_VALUE: String = "android/${BuildConfig.VERSION_NAME}+r${BuildConfig.VERSION_CODE}"
      
      $ grep -n -i 'versionCode|versionName|version bump' azure-voice-plan.md
      (no output)
      
      $ git log -p --follow -- android/app/build.gradle.kts | grep 'versionCode = |versionName = '
      -        versionCode = 4
      -        versionName = "0.2.1-hal"
      +        versionCode = 5
      +        versionName = "0.2.2-hal"
      -        versionCode = 3
      -        versionName = "0.2.0-hal"
      +        versionCode = 4
      +        versionName = "0.2.1-hal"
      ```
      FIX: Add a milestone before WS-E M4: "**E0. Bump the Android release version.** In /c/dev/live-ninja/android/app/build.gradle.kts set `versionCode = 6` and `versionName = "0.3.0"` (three numeric components — the current `0.2.2-hal` does not match the X-LN-Client grammar at internal/webapp/version.go:38). DoD: `grep -n 'versionName = "0.3.0"' android/app/build.gradle.kts` returns a line." Then change WS-D M1 to name that literal: "minimum android = 0.3.0".
      VERIFIED. Reviewer slice: completeness-critic.
- [ ] **M5. Missing milestone: the Android engine picker is a hardcoded four-item radio list, so WS-E M4's DoD instruction "pin that device to gpt-live-azure in Settings" cannot be carried out** BLOCKER.
      ANCHOR: WS-E M4 (azure-voice-plan.md:658); WS-F M2 (:684-688, "The picker gains four rows"); Locked decision 3 (:34-37, "Web and Android ship together")
      CLAIM: WS-E M4: "Then pin that device to `gpt-live-azure` in Settings and run one real spoken turn." WS-F M2: "The picker gains four rows" — with DoD `go test ./internal/webapp/ -run TestHelpDrawer`.
      REALITY: There are two engine pickers, not one. The Android picker is a hardcoded Compose `LabeledRadioGroup` with four literal `RadioOption` values in SettingsScreen.kt, plus four string resources per row. WS-F M2's DoD is a Go test over internal/webapp — it cannot touch the Android UI, and the string `SettingsScreen.kt` appears zero times in the plan. So no milestone adds an Azure row to Android, the S9 cannot be pinned to `gpt-live-azure` through the UI, and WS-E M4's DoD ("one spoken turn completes on gpt-live-azure") is unexecutable. This also breaks locked decision 3's "web and Android ship together".
      EVIDENCE:
      ```
      $ grep -c 'SettingsScreen.kt' azure-voice-plan.md
      0
      
      android/app/src/main/java/ninja/jeremy/liveninja/ui/screens/SettingsScreen.kt:1289-1318:
          LabeledRadioGroup(
              label = stringResource(R.string.settings_voice_engine_label),
              options = listOf(
                  RadioOption(value = "openai-realtime", …),
                  RadioOption(value = "openai-realtime-mini", …),
                  RadioOption(value = "nova-sonic", …),
                  RadioOption(value = SettingsViewModel.GEMINI_ENGINE, …),
              ),
              selected = voiceEngineDefault,
              onSelect = onSetVoiceEngine,
          )
      ```
      FIX: Add a milestone to WS-E: "**E5. Android engine picker.** Add four `RadioOption` entries plus their `settings_engine_*`/`settings_engine_*_desc` string resources to the `LabeledRadioGroup` at /c/dev/live-ninja/android/app/src/main/java/ninja/jeremy/liveninja/ui/screens/SettingsScreen.kt:1289-1318. DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest --tests '*SettingsEngineOptionsTest*'` passes, with a new test asserting the option list contains all eight engine values." Make WS-E M4 depend on it.
      VERIFIED. Reviewer slice: completeness-critic.
- [ ] **M6. azure-voice-live-lite ships with an undefined voice catalog — no fact, no milestone, no default** MAJOR.
      ANCHOR: ## What ships: the engine catalog (azure-voice-plan.md:333) and WS-B M4 (azure-voice-plan.md:504-512)
      CLAIM: Table row: "| `azure-voice-live-lite` | Azure AI Voice Live | `phi4-mm-realtime` | same | same | Azure standard TTS voices (`azure-standard`) |"
      REALITY: The string `azure-standard` occurs exactly once in the whole plan — that table cell. No Verified fact defines it: A9 (azure-voice-plan.md:292-294) covers `azure-realtime-native` voices for the `azure-realtime` model only, with default `ava`. WS-B M4 creates only `SupportedAzureRealtimeVoices`; nothing creates a `phi4-mm-realtime` voice list, names its default, or lists a single voice id. The plan also declares in WS-B M4's DoD that "every engine's default voice is a member of that engine's own catalog" — an assertion that cannot be written for this engine because the catalog does not exist. An unattended run reaches WS-E/WS-F with one of the four shipped pins having no voice to send.
      EVIDENCE:
      ```
      $ grep -n "azure-standard" azure-voice-plan.md
      333:| `azure-voice-live-lite` | Azure AI Voice Live | `phi4-mm-realtime` | same | same | Azure standard TTS voices (`azure-standard`) |
      
      $ grep -n "A9\." azure-voice-plan.md
      292:- **A9. `azure-realtime` is a distinct Azure-native speech-to-speech model with its own voices.**
      
      $ sed -n '292,295p' azure-voice-plan.md
      - **A9. `azure-realtime` is a distinct Azure-native speech-to-speech model with its own voices.**
      ...
        `{"type":"azure-realtime-native","name":"<name>"}`; default is `ava`. 35 voices across 20 locales —
      
      $ sed -n '507,508p' azure-voice-plan.md
            Separately, add `SupportedAzureRealtimeVoices` — the 35 `azure-realtime-native` voices from A9,
            default `ava` — as a new sibling on `GET /api/v1/realtime/voices`, following the
      ```
      FIX: Add a WS-A milestone that reads the `azure-standard` voice list off the deployed Voice Live resource before WS-B M4 (e.g. `az cognitiveservices account list-models` / the Voice Live voices endpoint), records the ids and the chosen default as fact A9b, and extend WS-B M4 to create `SupportedAzureStandardVoices` from it. If the list cannot be obtained, state in the plan that `azure-voice-live-lite` ships pinned to one named voice id and say which.
      VERIFIED. Reviewer slice: completeness-critic, docs-catalog-help. Raised independently by 2 reviewers.

## Sequencing

- [ ] **Q1. Order WS-D M1's gate ahead of WS-B M6 — both clients treat an unknown mode as openai-direct and will POST the Azure ek_ to api.openai.com** BLOCKER.
      ANCHOR: ## Sequencing item 1 (azure-voice-plan.md:754-755); Locked decision 3 (:34-37); WS-B M6 (:530-543); WS-D M1 (:581-592)
      CLAIM: Locked decision 3: "the server-side client-version gate (WS-D M1) is what makes that safe." Sequencing item 1: "**In parallel, immediately:** WS-B M2-M6 and WS-D M1-M3 (pure Go, no Azure dependency)". Locked decision 4: "the version gate rejects every client that cannot handle them."
      REALITY: Nothing in the plan requires WS-D M1 to be deployed BEFORE WS-B M6. Every milestone commits and pushes to main, and every push is a production deploy (locked decision 4), so B6 can be live for hours or days with no gate. Worse, the failure is not a rejection. Both shipped clients fall through to the OpenAI path for any mode they do not recognise, as long as a clientSecret is present — which an azure-direct response has. So the first production deploy of handleAzureDirect without D1 hands an already-installed client an Azure ek_ and it POSTs it to https://api.openai.com/v1/realtime/calls. That is exactly the R7 leak the plan says the gate prevents. (voice-live-direct fails closed because it carries no clientSecret; azure-direct does not.)
      EVIDENCE:
      ```
      web/static/js/realtime.mjs:266  `const mode = body && body.mode ? body.mode : 'openai-direct';`
      web/static/js/realtime.mjs:283  `  } else if (!body || !body.clientSecret || !body.clientSecret.value) {`
        -> mode 'azure-direct' matches neither the 'nova-bridge' nor 'gemini-direct' branch, hits this else-if, has clientSecret.value, so it is accepted and connects with the default transport.
      web/static/js/realtime.mjs:98   `const OPENAI_CALLS_URL = 'https://api.openai.com/v1/realtime/calls';`
      web/static/js/realtime.mjs:466  `constructor({ sessionPath = SESSION_PATH, callsUrl = OPENAI_CALLS_URL } = {}) {`
      web/static/js/realtime.mjs:778-779 `? this.callsUrl + '?model=' + encodeURIComponent(this.#model) : this.callsUrl;`
      android/.../realtime/RealtimeSessionApi.kt:168 `val mode = json.optString("mode").ifEmpty { RealtimeSession.MODE_OPENAI_DIRECT }`
      android/.../realtime/RealtimeSessionApi.kt:206 `                else -> if (value.isEmpty()) {`
        -> same catch-all: an azure-direct body with clientSecret.value passes and callsUrl is never read.
      internal/webapp/api_routes.go:555-566 the default fiber.Map emits `"mode": mode` and `"clientSecret": resp.ClientSecret` for any mode that is not nova-bridge or gemini-direct.
      ```
      FIX: In `## Sequencing`, split item 1: "WS-D M1 lands and deploys to `main` FIRST, alone. WS-B M6 and WS-C M2 must not be pushed until `git log origin/main` shows the D1 commit deployed." Add the same sentence as a `Depends on D1` line on WS-B M6 (azure-voice-plan.md:530) and WS-C M2 (:561).
      VERIFIED. Reviewer slice: completeness-critic.

## Steps that stall an unattended run

- [ ] **S1. WS-A M4 cannot run: the only Azure identity on this machine is forbidden from creating Entra app registrations, which halts WS-A M4 and all of WS-C** BLOCKER.
      ANCHOR: WS-A M4 (azure-voice-plan.md:429) and Standing authorizations (azure-voice-plan.md:351-352)
      CLAIM: "A4. Create the Voice Live resource and its service principal. ... Then an Entra app registration `ln-voicelive-client` with a client secret, granted `Cognitive Services User` **and** `Foundry User` (A7) scoped to `ln-voicelive`", backed by the standing authorization "**Create Microsoft Entra app registrations and role assignments** scoped to those resources only (WS-A M4)."
      REALITY: The operator granting authorization is not the same as the run having the privilege. The only credential cached on this machine is service principal `azure-owner-deployer` (appId f16364f7-e9d4-4b28-95aa-7b11e2fe8ea7, objectId 848fb359-9c80-416c-8f31-abb19cfde54c). It holds Owner at subscription scope — so it CAN create the role assignments — but it holds zero Microsoft Graph application permissions and zero Entra directory roles, so it cannot even READ the `/applications` collection, let alone create one. `az ad app create` will fail with Authorization_RequestDenied. There is no second identity to fall back to. In an unattended run this trips stop condition 1 ("A required Azure credential cannot be created or is rejected, and no substitute exists"), which stops WS-A M4 and, through the A4 dependency, the whole of WS-C (Voice Live token exchange) and the `voice-live-azure` / `azure-realtime` engines in WS-F.
      EVIDENCE:
      ```
      $ az account show -o json
        "user": {
          "name": "f16364f7-e9d4-4b28-95aa-7b11e2fe8ea7",
          "type": "servicePrincipal"
        }
      
      $ az account list --all --query "[].{name:name,id:id,user:user.name,type:user.type,state:state}" -o json
      [
        {
          "id": "adc40fff-bab3-4bd2-b961-1832d0375052",
          "name": "Azure subscription 1",
          "state": "Enabled",
          "type": "servicePrincipal",
          "user": "f16364f7-e9d4-4b28-95aa-7b11e2fe8ea7"
        }
      ]
      
      $ az ad signed-in-user show -o json
      ERROR: /me request is only valid with delegated authentication flow.
      
      $ az ad sp show --id f16364f7-e9d4-4b28-95aa-7b11e2fe8ea7 --query "{id:id,appId:appId,displayName:displayName}" -o json
      {
        "appId": "f16364f7-e9d4-4b28-95aa-7b11e2fe8ea7",
        "displayName": "azure-owner-deployer",
        "id": "848fb359-9c80-416c-8f31-abb19cfde54c"
      }
      
      $ az rest --method GET --url "https://graph.microsoft.com/v1.0/servicePrincipals(appId='f16364f7-e9d4-4b28-95aa-7b11e2fe8ea7')/memberOf" -o json
      {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#directoryObjects",
        "value": []
      }
      
      $ az rest --method GET --url "https://graph.microsoft.com/v1.0/servicePrincipals/848fb359-9c80-416c-8f31-abb19cfde54c/appRoleAssignments" -o json
      {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#appRoleAssignments",
        "value": []
      }
      
      $ az ad app list --query "[].{d:displayName,a:appId}" -o tsv
      ERROR: Insufficient privileges to complete the operation.
      
      $ az rest --method GET --url "https://graph.microsoft.com/v1.0/applications?\$top=1" -o json
      ERROR: Forbidden({"error":{"code":"Authorization_RequestDenied","message":"Insufficient privileges to complete the operation.","innerError":{"date":"2026-08-24T15:45:38","request-id":"6c5f9155-f324-4ad8-8170-2119379034e0","client-request-id":"6c5f9155-f324-4ad8-8170-2119379034e0"}}})
      
      (RBAC creation is NOT blocked — the SP is subscription Owner:)
      $ az role assignment list --assignee-object-id 848fb359-9c80-416c-8f31-abb19cfde54c --all --query "[].{role:roleDefinitionName,scope:scope}" -o tsv
      Owner	/subscriptions/adc40fff-bab3-4bd2-b961-1832d0375052
      ```
      FIX: This must be closed BEFORE the run starts, not discovered mid-run. Add to WS-A a new M0 precondition gate with DoD `az ad app list --query "[].appId" -o tsv` exiting 0 (it currently exits with "Insufficient privileges to complete the operation."), and record in `## Locked decisions` which of these two the operator did: (a) grant the `azure-owner-deployer` service principal the Microsoft Graph application permission `Application.ReadWrite.OwnedBy` (appRoleId bdfbf15f-ee85-4955-8675-146e8e5296b5) with admin consent, so `az ad app create` works; or (b) drop the app-registration path from WS-A M4 entirely and authenticate WS-C against `ln-voicelive` with the resource api-key in the query string, which A7 already records as supported ("The `api-key` alternative is also query-string capable"). Option (b) needs no directory privilege at all and is the one I would pick for an unattended run, at the cost of shipping a full-resource key — which is exactly why locked decision 2 puts Voice Live in its own resource group.
      VERIFIED. Reviewer slice: azure-resources.
- [ ] **S2. WS-A M5 DoD queries the wrong budget scope, exits 0 with zero budgets, and ends on a human confirming an email** BLOCKER.
      ANCHOR: WS-A M5 (line 439-444)
      CLAIM: "Two budgets, one per resource group … DoD: `az consumption budget list --query "[?contains(name,'ln-')].[name,amount]" -o tsv` prints two rows, and one test notification has been received."
      REALITY: Three faults. (1) `az consumption budget list` with no `-g` lists subscription-scope budgets; the milestone creates resource-group-scope budgets, which that invocation does not return. (2) The command exits 0 today with zero rows, so an unattended run keying on the exit code marks the milestone done with no budget in existence. (3) "one test notification has been received" requires a person to check an inbox and report back — the plan states the operator is unreachable once work starts. This DoD guards the plan's only spend backstop (line 748: "WS-A M5's budgets are the entire cost-safety story for this run") and stop condition 5 depends on it.
      EVIDENCE:
      ```
      $ az consumption budget list --query "[?contains(name,'ln-')].[name,amount]" -o tsv
      WARNING: This command is in preview and under development. Reference and support levels: https://aka.ms/CLI_refstatus
      exit=0
      
      $ az consumption budget list --help | head -6
      Command
          az consumption budget list : List budgets for an Azure subscription.
              WARNING: Command group 'consumption' is in preview and under development.
      Arguments
          --resource-group -g : Name of resource group.
      ```
      FIX: Query each resource group explicitly and assert a count, and drop the human step: `for RG in ln-azure-openai-rg ln-voicelive-rg; do test "$(az consumption budget list -g $RG --query "length([?starts_with(name,'ln-')])" -o tsv)" = "1" || exit 1; done; echo BUDGETS_OK`. Replace "one test notification has been received" with a config assertion the run can make itself: `az consumption budget show -g ln-voicelive-rg -n ln-voicelive-budget --query "length(notifications)" -o tsv` prints `6` (three thresholds x actual+forecast).
      VERIFIED. Reviewer slice: cost-and-rates, dod-sweep. Raised independently by 2 reviewers.
- [ ] **S3. WS-D M1's minimum client versions have no source, and the web surface sends no X-LN-Client at all** BLOCKER.
      ANCHOR: WS-D M1 ("Set the minimums to the versions WS-E actually ships"); WS-E M1
      CLAIM: "Set the minimums to the versions WS-E actually ships."
      REALITY: Nothing in the repo or in WS-E tells the run what those versions are, and for the web surface no version is sent at all. `grep -rn "X-LN-Client" web/` returns nothing: the browser client never sends the header on any request, including `GET /api/v1/realtime/session`. WS-E M1 edits only web/static/js/realtime.mjs for callsUrl and the Voice Live transport; it never adds the header, and no milestone bumps the repo VERSION (currently 0.7.0) as part of shipping. An unattended run reaching WS-D M1 has no value to write, and whatever it invents, the web surface can never satisfy it — so WS-F M1's requirement to 'start a session on web' on all four Azure pins cannot pass.
      EVIDENCE:
      ```
      $ grep -rn "X-LN-Client" web/
      (no output, exit 0)
      
      $ grep -rn "realtime/session" web/static/js/realtime.mjs
      web/static/js/realtime.mjs:97:const SESSION_PATH = '/api/v1/realtime/session';
      
      $ cat VERSION
      0.7.0
      
      internal/webapp/version.go:107-109 — the only version floors that exist, and they are backend config, not "what WS-E ships":
      func loadCompatVersions() compatVersionSet {
      	defaultMin := map[string]string{"web": "0.5.0", "android": "1.0.0", "m5stack": "1.0.0"}
      	defaultRecommended := map[string]string{"web": "0.9.0", "android": "2.1.0", "m5stack": "1.4.2"}
      
      Makefile:29 stamps only the BACKEND version (`internal/webapp.BuildVersion`), never a client version.
      ```
      FIX: In WS-D M1, replace "Set the minimums to the versions WS-E actually ships" with literal values written into the plan now, e.g. `azureMinClient = map[string]string{"web": "0.8.0", "android": "0.3.0", "m5stack": "99.0.0"}` (m5stack pinned unreachably high until firmware ships). Add to WS-E M1: send `X-LN-Client: web/<VERSION>+<gitSha>` on every `authFetch` in web/static/js/toolclient.mjs, and bump /c/dev/live-ninja/VERSION to 0.8.0 in the same commit.
      VERIFIED. Reviewer slice: wire-and-gate.
- [ ] **S4. WS-F M1's definition of done requires a human at a microphone and gates every remaining milestone, so the unattended run halts there with the mandatory Help copy unwritten** BLOCKER.
      ANCHOR: WS-F M1 (azure-voice-plan.md:675-682)
      CLAIM: WS-F M1: "For each of the four pins: set it as the account default, start a session on web, speak one turn, confirm audio out, confirm one tool call round-trips, and confirm barge-in interrupts playback. DoD: four sessions, four transcripts in the store … **Nothing below this line is done until this passes.**"
      REALITY: The plan is explicitly written for a run where the operator is unreachable ("## Standing authorizations", "## Stop conditions"). "Speak one turn" and "confirm audio out" cannot be executed by an unattended agent, there is no automated substitute named, and unlike WS-E M4 — which the plan deliberately gives an escape hatch (":668-669" "A failure here is recorded and reported — it is **not** a stop condition; finish everything else") — WS-F M1 has none. Because F1 gates F2 and F3, the run stops with the Help-drawer copy unwritten, which /c/dev/live-ninja/CLAUDE.md makes mandatory in the same commit as any capability change (the plan's own R13). The repo does have a browser harness the plan never mentions, so an automatable partial DoD exists.
      EVIDENCE:
      ```
      azure-voice-plan.md:675-682 (quoted above); contrast azure-voice-plan.md:668-669 for WS-E M4's explicit escape hatch.
      
      $ grep -c 'tests/web' azure-voice-plan.md
      0
      $ ls tests/web/specs
      a11y.spec.mjs  device-actions.spec.mjs  device-settings.spec.mjs  public-surface.spec.mjs  runtime-regressions.spec.mjs  settings-accordion.spec.mjs
      tests/web/playwright.config.mjs:13  const baseURL = process.env.LN_BASE_URL || 'https://live.jeremy.ninja';
      .github/workflows/deploy.yml:599-646  job `web-quality`, `needs: [deploy]`, runs `npx playwright test` against the deployed origin.
      ```
      FIX: Split WS-F M1 into F1a (automatable) and F1b (operator). F1a DoD: "`cd /c/dev/live-ninja/tests/web && LN_BASE_URL=https://live.jeremy.ninja npx playwright test specs/azure-engines.spec.mjs` passes — a new spec that, for each of the four pins, PUTs the pin, calls `GET /api/v1/realtime/session`, and asserts the returned `mode`, `model`, and the presence of `callsUrl` / `voiceLiveEndpoint`." Move "speak one turn / audio out / barge-in" into F1b and mark it, verbatim like WS-E M4: "A failure or a non-response here is recorded and reported — it is **not** a stop condition, and it does **not** gate F2 or F3."
      VERIFIED. Reviewer slice: completeness-critic.
- [ ] **S5. The plan file is untracked, absent from `main`, and the run starts on a branch the plan forbids touching** MAJOR.
      ANCHOR: Locked decisions item 4 (azure-voice-plan.md:39-40) and Standing authorizations, NOT granted (azure-voice-plan.md:365)
      CLAIM: "4. **Work lands on `main`.** Each milestone commits and pushes to `main` ..." and, under NOT granted: "- Touching the `alexa-version` branch or `azure-migration-plan.md`."
      REALITY: The checkout the run starts from IS `alexa-version`, `azure-voice-plan.md` is untracked, and `origin/main` does not contain it. plan.md's pointer to it is likewise uncommitted (a +6-line working-tree change on `alexa-version`). So the one artefact the unattended run is told to consult exists only in the working tree of a branch the plan withholds authorization to touch, and no milestone tells the run to switch to `main` or to commit the plan first. A session death loses the plan entirely (the Planning rule is that the plan file plus committed history must be enough to resume). Worse, `git switch main` is not a clean escape: plan.md both differs between `origin/main` and `HEAD` (4 lines) and has local modifications, which is exactly the state git refuses to check out over.
      EVIDENCE:
      ```
      $ git branch --show-current
      alexa-version
      
      $ git status --short
       M plan.md
      ?? azure-voice-plan.md
      ?? bash.exe.stackdump
      ?? update-report.md.prev
      
      $ git ls-tree --name-only origin/main -- azure-voice-plan.md plan.md backlog.md
      backlog.md
      plan.md
      
      $ git diff --stat origin/main HEAD -- plan.md
       plan.md | 4 ++++
       1 file changed, 4 insertions(+)
      
      $ git diff --stat plan.md
       plan.md | 6 ++++++
       1 file changed, 6 insertions(+)
      
      $ sed -n '365p' azure-voice-plan.md
      - Touching the `alexa-version` branch or `azure-migration-plan.md`.
      ```
      FIX: Add a WS-A M0 (or a preflight step above WS-A) that reconciles the branch before any milestone runs: `cd /c/dev/live-ninja && git switch -c azure-voice origin/main` (or `git switch main`), then move `azure-voice-plan.md` and the plan.md pointer paragraph onto that branch and commit them. DoD: `git ls-tree --name-only HEAD -- azure-voice-plan.md` prints `azure-voice-plan.md` and `git branch --show-current` does not print `alexa-version`. Also amend the NOT-granted bullet to say explicitly that carrying this plan file off `alexa-version` is the one permitted touch.
      VERIFIED. Reviewer slice: docs-catalog-help.
- [ ] **S6. WS-B M1's DoD needs $AZURE_OPENAI_KEY and nothing in the plan or the repo ever sets it — the milestone stalls an unattended run** MAJOR.
      ANCHOR: WS-B M1 (azure-voice-plan.md:459-468); depends on WS-A M6 (:445-453)
      CLAIM: DoD: `curl -sS -X POST "https://ln-aoai-eastus2.openai.azure.com/openai/v1/realtime/client_secrets" -H "api-key: $AZURE_OPENAI_KEY" ... | jq -e 'has("value")' > /dev/null && echo MINT_OK` prints `MINT_OK`.
      REALITY: `AZURE_OPENAI_KEY` is set by nothing. It appears nowhere in the repository outside the plan file and is not in the run's environment, so the header goes out as `api-key: ` and Azure returns 401. The plan's only credential path is WS-A M6, and every branch of it is closed: `scripts/set-secret.sh` writes a **GitHub Actions secret**, not an SSM parameter, and hard-refuses to run without a terminal; WS-A M6's own DoD forbids `--with-decryption`; and the Standing authorizations section says "Values are typed by the operator; no agent reads them back". The operator is unreachable the moment the run starts, so WS-B M1 blocks, and WS-B M4 (which depends on B1) blocks behind it.
      EVIDENCE:
      ```
      $ grep -rn "AZURE_OPENAI_KEY" . | grep -v azure-voice-plan.md
      (no output)
      
      $ sed -n '14,45p' scripts/set-secret.sh
      set -euo pipefail
      ...
          [ -t 0 ] || { echo "ERROR: interactive prompt needs a terminal (agents: run this in the user's terminal, do NOT pipe a value in)" >&2; exit 1; }
          read -r -s -p "Enter value for $NAME (hidden): " VALUE </dev/tty; echo >&2
      ...
      printf '%s' "$VALUE" | gh secret set "$NAME" -R "$REPO"
      
      $ sed -n '445,453p' azure-voice-plan.md
            -o text` prints three names. **Do not add `--with-decryption`.**
      ```
      FIX: Make WS-B M1 resolve the key itself from the resource it just created, without printing it. Prepend to the DoD, on one line: `AZURE_OPENAI_KEY="$(az cognitiveservices account keys list -n ln-aoai-eastus2 -g ln-azure-openai-rg --query key1 -o tsv)"` (already covered by the WS-A standing authorization, and the milestone already forbids `set -x`). Then add to WS-A M6: "`set-secret.sh` sets a GitHub Actions secret, not an SSM parameter — the operator must run it interactively before the unattended run begins; if the three secrets are not already present, mark A6 `[!]` and skip B1 rather than blocking."
      VERIFIED. Reviewer slice: dod-sweep, mint-and-config, secrets-and-pipeline. Raised independently by 3 reviewers.
- [ ] **S7. Every gradle DoD in WS-E fails on this machine before running a single test — JAVA_HOME is stored with literal quotes** MINOR.
      ANCHOR: WS-E M2 / M3 / M4 (azure-voice-plan.md:638, :646, :655)
      CLAIM: DoD: `cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest …` / `./gradlew :app:assembleDebug` — no precondition or environment setup is stated anywhere in the plan.
      REALITY: The user-scope JAVA_HOME on this machine contains literal double-quote characters, so `gradlew` rejects it and exits before any task runs. All three Android DoD commands fail identically, with an error that looks like a toolchain problem rather than a plan problem. An unattended run hits this on its first WS-E command and has nothing in the plan to tell it what to do. The plan has no preconditions section and never mentions JAVA_HOME.
      EVIDENCE:
      ```
      $ cd /c/dev/live-ninja/android && ./gradlew :app:testDebugUnitTest --tests '*RealtimeSessionParseTest*'
      ERROR: JAVA_HOME is set to an invalid directory: "C:\Users\Jeremy\jdk-temurin17\jdk-17.0.19+10"
      
      Please set the JAVA_HOME variable in your environment to match the
      location of your Java installation.
      
      $ powershell.exe -NoProfile -Command "[Environment]::GetEnvironmentVariable('JAVA_HOME','User'); '---machine---'; [Environment]::GetEnvironmentVariable('JAVA_HOME','Machine')"
      "C:\Users\Jeremy\jdk-temurin17\jdk-17.0.19+10"
      ---machine---
      C:\Program Files\Eclipse Adoptium\jdk-17.0.19.10-hotspot\
      
      The directory itself is fine — the quotes are part of the stored value:
      $ ls -d "C:/Users/Jeremy/jdk-temurin17/jdk-17.0.19+10"
      C:/Users/Jeremy/jdk-temurin17/jdk-17.0.19+10/
      
      Every gradle result in this audit was obtained only after `export JAVA_HOME="/c/Program Files/Eclipse Adoptium/jdk-17.0.19.10-hotspot"`.
      
      Note this contradicts the machine memory note "JAVA_HOME is valid again, don't re-apply the old workaround" — it is not valid as stored.
      ```
      FIX: Add a precondition line to the head of WS-E: "Before any `./gradlew` command, run `export JAVA_HOME="/c/Program Files/Eclipse Adoptium/jdk-17.0.19.10-hotspot"` — the user-scope JAVA_HOME on this machine is stored with literal quotes and gradlew rejects it. Verify with `"$JAVA_HOME/bin/java" -version` printing 17.0.19 before proceeding."
      VERIFIED. Reviewer slice: client-android.

## Cost

- [ ] **C1. A5's budgets alert, they never cap — the plan calls them "the entire cost-safety story" without stating the ~2-day exposure window or naming a kill switch** MAJOR.
      ANCHOR: ## Cost model (azure-voice-plan.md:747-748), WS-A M5 (:439-444), Stop condition 5 (:384-385)
      CLAIM: "**WS-A M5's budgets are the entire cost-safety story for this run.** Do not skip them and do not raise their thresholds." and WS-A M5: "This is the only spend backstop — the daily quota counters are inert today... so nothing else will surface a runaway."
      REALITY: An Azure Cost Management budget is a notification, not a cap, and it fires against cost data that lags by up to 24 hours and is evaluated only once per day. So the $100 'backstop' can arrive roughly 8-49 hours after the spend that tripped it, and it stops nothing when it does. The plan already admits the matching threat at :99 — "**A leaked token can open unlimited sessions** on that resource until it expires" — with a token lifetime of "~60-90 min (Entra minimum; not shortenable)" (:95), and the quota counters that would otherwise gate mints are inert (verified separately). The realistic worst case is therefore unbounded for a day or more, and the plan never says so, never names the action that actually stops it (delete the app registration's secret or its role assignment on `ln-voicelive`), and gives Stop condition 5 ("month-to-date spend... crosses $250") no detection command and no cadence — the run has no way to observe the condition it is told to stop on.
      EVIDENCE:
      ```
      https://learn.microsoft.com/en-us/azure/cost-management-billing/costs/tutorial-acm-create-budgets (verbatim): "Notifications are triggered when the budget thresholds are exceeded. Resources aren't affected, and your consumption isn't stopped." and "Cost and usage data is typically available within 8-24 hours and budgets are evaluated against these costs every 24 hours... When a budget threshold is met, email notifications are normally sent within an hour of the evaluation."
      azure-voice-plan.md:99-101 (verbatim): "**A leaked token can open unlimited sessions** on that resource until it expires. Mitigation: the resource is Voice-Live-only and carries its own Azure budget with actual + forecast alerts (WS-A M5)."
      ```
      FIX: Add to WS-A M5, verbatim: "An Azure budget notifies; it never caps ('Resources aren't affected, and your consumption isn't stopped'). Cost data lags 8-24 h and budgets evaluate every 24 h, so a $100 alert can arrive ~2 days after the spend. The actual stop is `az ad app credential delete --id <appId> --key-id <kid>` plus `az role assignment delete --assignee <appId> --scope <ln-voicelive resource id>` — run both the moment an alert or an unexplained spend appears." Give Stop condition 5 a detection command and cadence, e.g. `az consumption usage list --start-date <first-of-month> --end-date <today> --query "[?contains(instanceId,'ln-')].pretaxCost" -o tsv` summed, checked at each milestone boundary.
      VERIFIED. Reviewer slice: cost-and-rates.

## Azure facts that drifted

- [ ] **X1. Add the realtime deployment quota — WS-A M3 creates two GlobalStandard deployments with no stated capacity and no quota check** MAJOR.
      ANCHOR: ## Verified facts — Azure, A3 (azure-voice-plan.md:242-247) and WS-A M3 (:421-428)
      CLAIM: A3 lists deployable models and regions only. WS-A M3: "Deploy `gpt-realtime-2.1` as a **GlobalStandard** deployment named `gpt-realtime-2-1`, and `gpt-realtime-2.1-mini` as `gpt-realtime-2-1-mini`. If `2.1` is not offerable in this subscription, fall back to `gpt-realtime-2`, then `gpt-realtime`/`gpt-realtime-mini`." `grep -in "quota|TPM|RPM|capacity" azure-voice-plan.md` returns nothing about deployment capacity.
      REALITY: A GlobalStandard deployment is created with an explicit SKU capacity drawn from a subscription-level quota pool, and the pool for realtime models is small: gpt-realtime GlobalStandard is 200 RPM / 100,000 TPM at Tier 1 and stays at that value through Tier 5. Neither `gpt-realtime-2.1` nor `gpt-realtime-2.1-mini` nor `gpt-realtime-mini` has ANY row in ANY quota tier table, so the run has no published number to size against. Quota is now pooled per subscription across all regions, so the mini deployment competes with the full one. The plan's only fallback is on the MODEL NAME; an InsufficientQuota / capacity error is a different failure and the fallback chain does not clear it, so WS-A M3 stalls an unattended run at its first Azure write.
      EVIDENCE:
      ```
      Fetched https://learn.microsoft.com/en-us/azure/foundry/openai/quotas-limits (ms.date 2026-08-20). Verbatim, Tier 1 table: "| gpt-realtime | GlobalStandard | 200 | 100,000 |". Same row value in Tier 2, Tier 3, Tier 4 and Tier 5; Tier 6 is "| gpt-realtime | GlobalStandard | 300 | 150,000 |". Searching the same page for `gpt-realtime-2.1`, `gpt-realtime-2`, `gpt-realtime-1.5` and `gpt-realtime-mini` returns no rows in any tier — only `gpt-realtime`, `gpt-4o-realtime-preview` and `gpt-4o-mini-realtime-preview` appear.
      
      Also verbatim: "Subscription-level quota management in Microsoft Foundry started after **May 7, 2026**." and "- **Global Standard**: Deployments of the same model and version share one quota pool across all regions in a subscription."
      
      And from https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/realtime-audio: "The Realtime API has specific rate limits for audio tokens and concurrent sessions. Before deploying to production, review [Azure OpenAI quotas and limits](../quotas-limits) for your deployment type."
      ```
      FIX: Add to A3: "Realtime quota is pooled per subscription for GlobalStandard since 2026-05-07. The only published realtime figure is `gpt-realtime` GlobalStandard 200 RPM / 100,000 TPM (Tiers 1-5); `gpt-realtime-2.1`, `gpt-realtime-2.1-mini` and `gpt-realtime-mini` have no published quota row. Source: https://learn.microsoft.com/en-us/azure/foundry/openai/quotas-limits." Then make WS-A M3 read the available capacity before deploying rather than guessing: prepend `az cognitiveservices usage list -l eastus2 --query "[?contains(name.value,'Realtime')].[name.value,currentValue,limit]" -o tsv` and record the output in the Execution log, deploy each model with `--sku-capacity` set to at most half the reported remaining limit, and add an explicit `[!]` branch: an InsufficientQuota error on create is NOT the A3 model-name fallback — halve `--sku-capacity` once, then mark the milestone `[!]` and continue with WS-B and WS-C rather than retrying the same command.
      INFERRED (Azure). Reviewer slice: azure-external-facts.

---

## Corrections to apply as hygiene

These are not defects that change what the run does. Both were surfaced by the verification pass
rather than by a reviewer, and both are cheap and correct to apply.

- [ ] **H1. A9's voice count is 34 across 17 locales, not 35 across 20.** MINOR.
      ANCHOR: `## Verified facts — Azure` A9 (`:294`); also `:332` and `:507`.
      The plan's own enumeration lists 34 names; "35" appears three times and "20 locales" once, and
      no milestone or DoD consumes either number. Every path the run can take — copy the literal
      list, or re-read the cited source — lands on the same 34-entry catalog, which is why this is
      hygiene and not a stall. VERIFIED.
      FIX: correct the three counts. If WS-B M4 follows the `SupportedGeminiVoices` precedent of
      pinning an exact length, assert 34 and name the source in the failure message, so a future
      Microsoft addition reads as "the catalog changed" rather than "someone broke the slice".

- [ ] **H2. WS-B M5 contains a contradiction with itself.** MINOR.
      ANCHOR: WS-B M5 bullets at `:519-521` against its DoD at `:525-527`.
      The DoD requires `TestRatesCoverEveryShippedEngine` to assert that "every model id reachable
      from the eight engine constants has an explicit `modelRates` key", while the bullet three lines
      above forbids inventing a rate when none is published. `azure-realtime` — the model behind the
      `azure-voice-live` pin — appears in the Voice Live supported-models table but in none of the
      three published pricing tiers, so both clauses cannot hold at once. VERIFIED.
      FIX: scope the test to engines with published rates and assert that every engine is either in
      `modelRates` or on an explicit `ratesMissing` allowlist, so an unpriced engine is declared
      rather than silently defaulted.

---

## What the audit refuted

Recorded so these are not raised again. Each was proposed by a reviewer and killed by a verifier
that re-ran the evidence or found the plan already handled it. Several are factually true
observations whose consequence did not survive: a true statement that changes nothing an unattended
run does is noise, and the plan is better without it.

- **Add a milestone for the Android engine picker — its four engine rows are hardcoded and no milestone extends them**
  ANCHOR: WS-B M2 (d) (azure-voice-plan.md:479-482) and WS-E E4 / WS-F F2 (azure-voice-plan.md:663, 684-688) · slice: enum-plumbing
  WHY REFUTED: REFUTED at the claimed severity. The file evidence is accurate — SettingsScreen.kt:1291-1316 hardcodes exactly four RadioOptions and no milestone names that file — but the consequence claim is wrong on its central point. The Android app does not choose its engine from the picker. Transport is chosen from the server-resolved `mode` in the mint response (RealtimeSessionApi.kt:35-45, RealtimeSessionCoordinator.kt:121...

- **Correct R1 and drop WS-B M2 (b)'s redefinition — IsClientDirect is already "not nova-sonic"**
  ANCHOR: Verified facts R1 (azure-voice-plan.md:117-122) and WS-B M2 (b) (azure-voice-plan.md:473-476) · slice: enum-plumbing
  WHY REFUTED: The reviewer's facts are all correct, and all of them are inert. Under the consequence lens this changes nothing the run does, and the proposed fix would introduce a real defect the plan does not currently have. Why the finding is noise: 1. `IsClientDirect` has zero callers — the reviewer proved this and I re-confirmed it (only `engine.go:28` and `:32`, plus plan prose; no test file mentions it). A function with z...

- **R5 is false: there is no session-config builder to reuse — the object is an inline map literal inside (*Minter).Mint**
  ANCHOR: Verified facts R5 (azure-voice-plan.md:152-160); WS-B M3 (:531-545) · slice: mint-and-config
  WHY REFUTED: The reviewer's facts are correct — `sessionConfig` really is an inline `map[string]any` inside `(*Minter).Mint` at mint.go:295, and only `buildTurnDetection` (:248) and `buildAudioInput` (:264) are package-level — but under the consequence lens this is a wording imprecision, not a defect that changes the run. 1) The "not authorized" sub-claim is false, and it is the only part that could have made this a blocker. B...

- **WS-B M1's DoD discards the HTTP status and the error body, so the plan's own 400-vs-403 triage cannot be carried out and the failure is silent under set -e**
  ANCHOR: WS-B M1 (azure-voice-plan.md:459-468) · slice: mint-and-config
  WHY REFUTED: REFUTED under the CONSEQUENCE lens: the run proceeds correctly anyway, and two of the reviewer's three evidence pillars do not survive checking. 1. This is NOT a false-pass DoD. `jq -e 'has("value")'` returns 1 on any error body, so MINT_OK is not printed and B1 correctly fails when the work is undone. The highest-value defect class does not apply here. 2. The "silent under set -e" pillar is inapplicable. `grep -c...

- **WS-C M1's 80%-of-lifetime refresh hands clients a token with as little as 20% of its life left, contradicting the honesty table WS-C M2 is told to populate**
  ANCHOR: WS-C M1 (azure-voice-plan.md:551-563); WS-C M2 (:565-576); '## The token problem, stated honestly' table row `azure-voice-live` / `-lite` · slice: mint-and-config
  WHY REFUTED: Refuted on three grounds. (1) The plan already says this in writing, three lines below the table the reviewer cites. azure-voice-plan.md:107-108 states the client-visible window is "minutes-to-hours rather than seconds" — a ~12-minute cache-aged credential falls inside that. The reviewer read the table row and did not read the qualifying bullet immediately beneath it, which the refutation criteria explicitly call ...

- **R15 cites gemini_mint.go as the precedent for a cached OAuth token client; it is neither cached nor OAuth and it depends on a vendored SDK**
  ANCHOR: Verified facts R15 (azure-voice-plan.md:222-227); WS-C M1 (:551-563) · slice: mint-and-config
  WHY REFUTED: The alleged gap — "an implementer told 'follow the precedent' finds no caching to copy" — does not exist, because the plan states the entire caching contract itself at the milestone. WS-C C1 (azure-voice-plan.md:551-563) reads: "Hand-rolled, not `azidentity` (R15). Cache the token in memory keyed on scope and refresh at 80% of its lifetime", with a DoD that requires "a test that a cached unexpired token is reused"...

- **WS-A M6 mandates an interactive prompt that hard-fails without a TTY, contradicting the plan's unattended premise, while a permitted machine-to-machine path exists**
  ANCHOR: WS-A M6 ("with the operator typing the values"); Standing authorizations ("Values are typed by the operator; no agent reads them back"); Stop conditions 2 · slice: secrets-and-pipeline
  WHY REFUTED: The evidence is accurate but the consequence is not. Three reasons to refute, then three reasons the fix is independently wrong. REFUTING THE FINDING. 1. The run does not stall. set-secret.sh exits 1 in under a second (verified below); it does not block, hang, or wait on a prompt nobody answers. The plan's Stop conditions preamble then gives the run a defined, correct behaviour: "Anything not listed here is worked...

- **Locked decision 4's inertness claim is false: WS-B M6 can deploy to production before WS-D M1, sending Azure credentials to api.openai.com**
  ANCHOR: Locked decision 4; Sequencing item 1; WS-B M6; WS-D M1 · slice: secrets-and-pipeline
  WHY REFUTED: The reviewer's code evidence is accurate — I re-verified every line of it — but the leak is unreachable by the run's own actions, so it does not change what an unattended run does. The leak requires a production settings pin to an Azure engine. `handleAzureDirect` is only reached when `realtime.ResolveEngine(ctx, b.settings, b.table, req.UserID, req.DeviceID)` (cmd/realtime-broker/main.go:300) returns one of the n...

- **WS-A M5 is unimplementable with the command it names: az consumption budget create has no notification, threshold or contact-email parameter, and the alternative extension prompts interactively**
  ANCHOR: WS-A M5 (azure-voice-plan.md:439-446) · slice: azure-resources
  WHY REFUTED: The finding does not survive re-running its own evidence. Three independent errors. (1) MISATTRIBUTED COMMAND. The title says M5 "is unimplementable with the command it names," but the plan does not name `az consumption budget create` anywhere. A5 (azure-voice-plan.md:439-444) names exactly one command, `az consumption budget list`, in its DoD. `az costmanagement` appears nowhere in the plan file either. The revie...

- **WS-A M5's DoD includes "one test notification has been received", which no unattended run can confirm**
  ANCHOR: WS-A M5 (azure-voice-plan.md:445-446) · slice: azure-resources
  WHY REFUTED: The facts are right; the consequence is not. There is genuinely no way to trigger a test budget notification (verified against the live CLI and the Learn docs), so the second conjunct is indeed human-only. But under the CONSEQUENCE lens the run proceeds correctly anyway, for four reasons I verified in the plan itself: 1. The budgets still get created. The unsatisfiable conjunct is a *reporting* predicate, not a ga...

- **WS-A M3's DoD passes identically whether the primary model or a fallback was deployed, silently propagating a wrong model id to four workstreams**
  ANCHOR: WS-A M3 (azure-voice-plan.md:421-428) · slice: azure-resources
  WHY REFUTED: REFUTED on consequence. The finding's premise — that the backing model id is "the single fact that propagates to WS-B M2, WS-B M5, WS-E and WS-F" — is a misreading. The plan states at azure-voice-plan.md:495 that for this path "the model id" IS the deployment name: "and the model id (the deployment name from A3)". Every named downstream consumer keys on the deployment name, which is a plan constant and is exactly ...

- **WS-A M4 assigns Azure RBAC roles to an app registration, but role assignments and the DoD's --assignee lookup both target the service principal, which the plan never creates**
  ANCHOR: WS-A M4 (azure-voice-plan.md:429-438) · slice: azure-resources
  WHY REFUTED: REFUTED. The CLI mechanic the reviewer describes is real and I reproduced it, but the claim it is used to support — "the plan never creates the service principal" — is contradicted by the milestone's own title, which the reviewer's quote skips. The reviewer quoted A4 starting at line 431 ("Then an Entra app registration...") and omitted line 429: "A4. Create the Voice Live resource and its service principal." The ...

- **Verified fact A3 understates and mis-versions what this subscription can actually deploy: gpt-realtime-2.1 is fully offerable, and gpt-realtime-2's version is 2026-05-06, not 2026-05-07**
  ANCHOR: ## Verified facts — Azure, A3 (azure-voice-plan.md:242-249) · slice: azure-resources
  WHY REFUTED: The evidence is accurate — I reproduced it — but every one of the three corrections lands on text the run never acts on, and milestone WS-A A3 already does the right thing without them. (1) "Understates 2.1" changes nothing. Verified fact A3's sentence is additive ("The pricing page additionally lists GPT-Realtime-2.1..."), not exclusionary; it nowhere says 2.1 is undeployable. Milestone WS-A A3 (line 421) already...

- **The subscription already holds an unreferenced AIServices resource with realtime deployments in eastus2, outside both budgets and outside stop condition 5**
  ANCHOR: WS-A M1/M2 (azure-voice-plan.md:408-420) and Stop conditions 5 (azure-voice-plan.md:384-386) · slice: azure-resources
  WHY REFUTED: The evidence is accurate but the consequence does not exist, and the proposed fix would actively break WS-A M5. Nothing in the run changes. Every DoD in WS-A that could have collided with the pre-existing group is either tag-filtered or name-scoped, and I confirmed each is immune: - A1 DoD filters on `[?tags.plan=='azure-voice-plan']`; `rg-liveninja-azure-prod` has `"tags": null`, so it cannot be counted. Currentl...

- **Mark WS-E M1's browser turn as owner-verified — no harness in this repo can reach an authenticated /conversation session**
  ANCHOR: WS-E M1 DoD, second clause, azure-voice-plan.md:630-631 · slice: client-web
  WHY REFUTED: REFUTED as noise, and the proposed fix is actively harmful — do not apply it even in part. 1. The run does not stall. The reviewer says "the run has no sanctioned way past it and no listed stop condition covering it." The plan's own stop-conditions preamble is the blanket sanction: azure-voice-plan.md:371 — "Anything not listed here is worked around, marked `[!]` with the exact thing that unblocks it, and reported...

- **Correct "reuses the existing WebRTC path unchanged" — the ?model= concat and the oai-events channel label both diverge from Azure's documented contract**
  ANCHOR: WS-E M1, first bullet, azure-voice-plan.md:617-620 · slice: client-web
  WHY REFUTED: The reviewer's raw evidence is accurate — I reproduced every line — but the finding's load-bearing consequence claim is false, and with it the case for a plan edit collapses. The finding rests on: "nothing in E1's DoD would surface a channel that never opens." That is wrong. `realtime.mjs:811` is `await dcOpen;`, and `dcOpen` is a promise that rejects at `DC_OPEN_TIMEOUT_MS = 10_000` (`:99`, `:752-757`) with `Real...

- **GeminiLiveTransport is not the structural precedent for VoiceLiveTransport — Voice Live needs WebRTC, and Gemini has none**
  ANCHOR: WS-E M3 (azure-voice-plan.md:643-645) · slice: client-android
  WHY REFUTED: Every file-level citation the reviewer gives reproduces exactly — I re-ran all of them. `grep -c "org.webrtc" GeminiLiveTransport.kt` returns `0`; the header comment at :202-205 is verbatim as quoted; :855 is `AudioRecord(`, :879 is `val b64 = Base64.getEncoder().encodeToString(`, :904 is `val track = AudioTrack.Builder()`; `grep -n "webrtc|PeerConnection|DataChannel|IceCandidate"` over GeminiLiveTransport.kt retu...

- **WS-E M4's DoD requires a physically spoken turn and a hand-tapped Settings pin that no unattended run can perform**
  ANCHOR: WS-E M4 (azure-voice-plan.md:658, :665-669) · slice: client-android
  WHY REFUTED: REFUTED under the CONSEQUENCE lens, and one of its two factual halves is wrong. 1. The "hand-tapped Settings pin" half is false. The engine pin is resolved server-side by `ResolveEngine(ctx, g, table, userID, deviceID)` at internal/realtime/mint.go:425 from the stored settings document; the Compose picker at SettingsScreen.kt:1286 is merely one writer into that same document. `voiceEngine.default` and the per-devi...

- **R7 and WS-D M2 name a broker type that does not exist**
  ANCHOR: "Verified facts" R7 (azure-voice-plan.md:169-171); WS-D M2 (azure-voice-plan.md:594-595) · slice: wire-and-gate
  WHY REFUTED: The reviewer's facts are correct — `SessionResp` exists nowhere in Go source, and the broker's reply type is `Response` at `cmd/realtime-broker/main.go:112-172` — but the defect does not change what an unattended run does, so it is noise under the consequence lens. Three reasons the run proceeds correctly anyway: 1. The pointer is already file-and-line exact, and it lands inside the correct struct. `cmd/realtime-b...

- **WS-C M2's DoD half-asserts a field that has never existed, and ignores the existing test that already does this correctly**
  ANCHOR: WS-C M2 DoD (azure-voice-plan.md:572-573) · slice: wire-and-gate
  WHY REFUTED: The reviewer's individual quotes reproduce, but the defect built on them does not. 1. The "vacuous half" claim is wrong. The reviewer only grepped the Go broker. The legacy m5stack firmware probes FOUR key spellings, `bridgeUrl` among them, at firmware/components/ln_realtime/ln_rt_session.c:239-248, and sets `nova` if any of them is present. So a Voice Live response that named a field `bridgeUrl` WOULD be misroute...

- **Key the new rate rows on the A3 deployment names, not on A12's model ids — as written WS-B M5 re-creates the exact silent fallback it exists to close**
  ANCHOR: WS-B M5 (azure-voice-plan.md:513-528), depends on WS-A M3 (:421-427) and WS-B M3 (:497-504) · slice: cost-and-rates
  WHY REFUTED: The mechanism is real but the run would proceed correctly anyway, because B5's own DoD is the net that catches exactly this. Three lines below the bullet the reviewer quotes, the DoD requires a new TestRatesCoverEveryShippedEngine that asserts "every model id reachable from the eight engine constants has an explicit modelRates key", and adds "TestRatesForUnknownModelFallsBack must not be the thing that makes it pa...

- **WS-B M5's DoD test cannot be written as specified — there is no engine-to-model mapping to walk, and two of the eight model ids come from environment variables**
  ANCHOR: WS-B M5 DoD (azure-voice-plan.md:525-528) · slice: cost-and-rates
  WHY REFUTED: The core claim — "there is no engine-to-model mapping to walk" — is refuted by the plan text three lines above the DoD the reviewer quoted. B5's own body instructs the implementer to build that mapping in this same milestone: azure-voice-plan.md:523-524 says "Keep `RatesFor`'s fallback for genuinely unknown ids, and add `RatesForEngine` returning `(Rates, bool)` that logs `code=rates_missing` instead of guessing."...

- **Cost model calls the dominant term "cached text input" but the $1,056 figure only reproduces at audio rates — recomputing it from the stated text rates gives ~$194/month**
  ANCHOR: ## Cost model (azure-voice-plan.md:744-748) · slice: cost-and-rates
  WHY REFUTED: The arithmetic is correct and the label is genuinely imprecise, but under the consequence lens it changes nothing the run does, so it is documentation accuracy, not a defect. Verified the reviewer's math exactly: audio basis 30.8M x (32.00-0.40) = 973.28, +82.87 = 1056.15 (the printed ~$1,056); text basis 30.8M x (4.00-0.40) = 110.88, +82.87 = 193.75. The sibling's audio basis is confirmed at azure-migration-plan....

- **The WS-B M6 note points a future fixer at dead code — the production dayMints write is quota.go's RecordMint, and AddMonthUsage/BumpDayMints/SetUsageTotals have no callers at all**
  ANCHOR: WS-B M6 note (azure-voice-plan.md:536-540) and WS-G M6 (:717-719) · slice: cost-and-rates
  WHY REFUTED: The reviewer's raw facts all reproduce, but the "defect" changes nothing the run does, and the harm story is itself wrong. What I re-ran and confirmed: AddDayUsage is at usage.go:92 with exactly one caller (BumpDayMints, usage.go:112, passing `0, 0, 1`); BumpDayMints, AddMonthUsage (:102) and SetUsageTotals (:155) have zero callers; the production dayMints write is Gate.RecordMint's raw UpdateItem at quota.go:942,...

- **R10 is confirmed true in every load-bearing part, but its dating claim covers both rate rows when only the gpt-realtime row is from 2025-08**
  ANCHOR: ## Verified facts R10 (azure-voice-plan.md:189-196), consumed by WS-B M5 bullet 4 (:522) · slice: cost-and-rates
  WHY REFUTED: The reviewer's evidence reproduces perfectly — I re-ran all of it — and their factual point is literally correct, but it does not change what the run does, so it fails the "not a wording nitpick" bar. What I confirmed independently: rates.go:28 `var modelRates`, :29 `"gpt-realtime"`, :32-33 `AudioInPer1M: 32.00` / `AudioOutPer1M: 64.00`, :37-40 the Gemini provenance comment `verified 2026-07-19; M13`, :41 `"gemini...

- **"$0/month" is verified for the model deployments and budgets but not for A4's "Foundry resource", which the plan never pins to a resource type**
  ANCHOR: ## Cost model (azure-voice-plan.md:722-728) and WS-A M4 (:429-437) · slice: cost-and-rates
  WHY REFUTED: The finding rests on the claim that A4's "Foundry resource" is type-ambiguous and could be read as an Azure AI Foundry hub that drags in a Storage account and a Key Vault. That reading does not survive the plan's own cited sources. "Microsoft Foundry resource" is a defined Microsoft term, not a loose phrase. 1) The Voice Live how-to (the source the plan anchors A5/A7 to) states the prerequisite as "A [Microsoft Fo...

- **Delete A6's "no capacity planning" claim — Voice Live has hard per-resource quotas that cap the design**
  ANCHOR: ## Verified facts — Azure, A6 (azure-voice-plan.md:267-268); consumed by WS-A M4 (:429-434) and WS-E M1 (:617-631) · slice: azure-external-facts
  WHY REFUTED: The evidence is accurate — the quotas exist, A6's "No capacity planning" is loose, and the cited voice-live-webrtc page does not support it. But under the CONSEQUENCE lens nothing changes: the run proceeds identically with A6 unchanged, and the proposed fix would inject the very defect class this audit exists to catch. 1. The run cannot reach any of the three ceilings. WS-F M1 is the only milestone that opens a Vo...

- **Add the 60-minute hard session cap — it applies to BOTH new backends and no milestone handles reconnect**
  ANCHOR: ## Verified facts — Azure (azure-voice-plan.md:229-320) — missing fact; affects WS-E M1 (:617-631) and the Cost model (:722-746) · slice: azure-external-facts
  WHY REFUTED: REFUTED under the CONSEQUENCE lens. The external evidence is accurate, but the run proceeds correctly without the edit, and the behaviour the reviewer asks for already exists in the repo as inherited product behaviour. 1) No milestone in this plan holds a session anywhere near 60 minutes. WS-E M1's DoD is `go test ./internal/webapp/` plus "a browser session pinned to `gpt-live-azure` completes a turn and shows a n...

- **Correct A9's voice count: the documented catalog is 34 voices across 17 locales, not 35 across 20**
  ANCHOR: ## Verified facts — Azure, A9 (azure-voice-plan.md:292-298); quoted by WS-B M4 (:507) and by the engine catalog table (:332) · slice: azure-external-facts
  WHY REFUTED: The arithmetic is right and the fix is right, but the consequence claim is overstated: nothing an unattended run does changes. What I verified independently. The plan's own enumeration is 34 names (count command below), and "35" appears exactly three times (:294, :332, :507). "20 locales" appears exactly once, at :294 — `grep -n 'locale' azure-voice-plan.md` returns only that line, so no milestone, DoD, picker gro...

- **Record that `azure-realtime` appears in no published Voice Live pricing tier — WS-B M5 cannot key it on Pro/Basic/Lite**
  ANCHOR: ## Verified facts — Azure, A10 (azure-voice-plan.md:300-305) and A12 (:314-320); consumed by WS-B M5 (:515-522) and the Cost model (:722-746) · slice: azure-external-facts
  WHY REFUTED: The reviewer's raw observation is factually correct and I reproduced it verbatim: `azure-realtime` appears in the Voice Live supported-models table but in none of the three pricing-tier rows. A10's tier list at azure-voice-plan.md:300-305 is verbatim-accurate against the live doc, and the line numbers cited (A10 :300-305, A12 :314-320, M5 bullets :519-521, cost-model bullets :733-741) all check out. But this is no...

- **Fix A4's citation — the page it names contains no mention of cedar or marin**
  ANCHOR: ## Verified facts — Azure, A4 (azure-voice-plan.md:249-254) · slice: azure-external-facts
  WHY REFUTED: Refuted as noise under the CONSEQUENCE lens. The evidence is accurate — I re-fetched the cited page and confirmed it supports neither half of A4 — but the citation is not an input to any decision the run makes, so an unattended run proceeds identically with the plan unchanged. A4's branch is resolved empirically, not from documentation. WS-B M1 (azure-voice-plan.md:459-466) is a live mint carrying "voice":"cedar" ...

- **WS-E M1 DoD passes today and its second half needs a human at a browser**
  ANCHOR: WS-E M1 (line 630-631) · slice: dod-sweep
  WHY REFUTED: The two evidenced facts are true but the finding they support is not, and neither half changes what an unattended run does. (1) The headline "DoD passes today" is false about the DoD. Line 630-631 is a CONJUNCTION: `go test ./internal/webapp/` passes AND a live browser session on `gpt-live-azure` completes a turn with a non-zero cost badge. The second conjunct cannot pass today — no Azure resource exists (WS-A), t...

- **WS-A M4 DoD contains a literal `<appId>` placeholder and cannot execute**
  ANCHOR: WS-A M4 (line 435-437) · slice: dod-sweep
  WHY REFUTED: REFUTED as noise under the consequence lens, and the proposed fix is worse than the wording it replaces. 1) The placeholder changes nothing the run does. `<appId>` is the plan's consistent placeholder notation, used the same way at lines 272-273 (`Bearer <token>`), 294 (`"name":"<name>"`), 487 (`deviceOverrides[<id>]`), 493-494 (`<endpoint>/openai/v1/realtime/client_secrets`, `api-key: <key>`) and 563 (`&model=<mo...

- **WS-F M1 — the gate that everything below depends on has no command at all**
  ANCHOR: WS-F M1 (line 675-682) · slice: dod-sweep
  WHY REFUTED: REFUTED on consequence, and the proposed fix is independently broken. 1) The claimed consequence — "the run reaches WS-F M1 and stops permanently" — does not follow from the plan. The plan's own governance section already routes exactly this case: line 371-372 says anything not in the five listed stop conditions "is worked around, marked `[!]` with the exact thing that unblocks it, and reported at the end. Do not ...

- **WS-A M1 DoD exits 0 today with zero resource groups**
  ANCHOR: WS-A M1 (line 408-413) · slice: dod-sweep
  WHY REFUTED: The evidence is accurate and reproduces verbatim, but the finding is overstated as a BLOCKER: it does not change what the unattended run does. There are only two ways A1 can fail open, and neither changes the run's outcome. Case 1, the resource groups do not exist. A1's DoD exits 0 and an exit-code-keyed runner marks A1 done. But A1's own successor fails closed one milestone later with a precise diagnosis: `az cog...

- **WS-B M4 adds an Azure voice catalog with no settings field, validator, or resolver — the picker has nothing to write to**
  ANCHOR: WS-B M4 (azure-voice-plan.md:504-512), citing R11 (azure-voice-plan.md:189-194) · slice: docs-catalog-help
  WHY REFUTED: REFUTED on three independent grounds, and the proposed fix is separately wrong. 1) THE LOAD-BEARING CAUSAL CLAIM IS BACKWARDS. The finding turns on "Without the schema key and the validator, a settings PUT carrying an Azure voice is rejected by the additive-only contract, so the served catalog is unselectable." The opposite is true. contracts/settings.schema.json:7 is "additionalProperties": true (repeated at ever...

- **WS-G has no definition of done and nothing verifies the backlog transcription ever happens**
  ANCHOR: WS-G (azure-voice-plan.md:700-719) and WS-F M3 (azure-voice-plan.md:690-697) · slice: docs-catalog-help
  WHY REFUTED: The reviewer's facts check out, but the harm does not. Every factual claim verifies: 24 `DoD:` lines exist, the last is F3 at line 695, and lines 698-775 contain none, so WS-G genuinely has no DoD. F3's DoD is only the docs grep. backlog.md really uses `- **Title.** text ⟵ source` with no status markers. So an unattended run can pass all 24 DoDs with backlog.md untouched. That much is true. It is consequence-free....

- **WS-F M3's DoD is satisfiable by one string and its "returns non-zero" wording is already true today**
  ANCHOR: WS-F M3 (azure-voice-plan.md:690-697) · slice: docs-catalog-help
  WHY REFUTED: REFUTED — the headline is wrong because it analyses only the first half of a conjunction. WS-F M3's DoD is "`grep -c 'gpt-live-azure' …` returns non-zero AND the client support matrix has a column for each new engine". The second conjunct is FALSE on the untouched file today: docs/voice-engines.md:194 reads `| Surface | OpenAI-direct | Nova-bridge | Gemini-direct | Notes |` — no Azure column at all. So the DoD doe...

- **R11's line citations point at the wrong lines and undercount SupportedVoices**
  ANCHOR: ## Verified facts — the repository, R11 (azure-voice-plan.md:189-194) · slice: docs-catalog-help
  WHY REFUTED: The load-bearing half of this finding is factually wrong, and the reviewer's own evidence disproves it. The claim is that "`sed -n '33,45p'` shows only 9 of the 10 voices and omits `verse`." I ran that exact command: it prints all 10 entries, `verse` included. The reviewer quoted `sed -n '45,46p'` themselves and got `verse` on line 45 — inside the cited 33-45 range — then concluded verse falls outside it. They app...

