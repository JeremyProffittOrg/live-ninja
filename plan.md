# Plan

Consolidated **2026-07-31**. Single source of truth for **active work**.
Deliberately-deferred future items live in [backlog.md](backlog.md) — those are **not** scheduled and
must not be pulled in here without a decision.

Archived (history preserved in full, banners at the top of each):

| Archived | What it was |
|---|---|
| [completed/plan.md](completed/plan.md) | The 2026-07-24 consolidated master plan (WS-1…WS-6, M0–M31). Its still-open items are carried forward in §3 below. **§8 of [archive/plan.md](archive/plan.md) remains the deepest record of how this system actually works — read it before resuming anything old.** |
| [completed/plan-code-update.md](completed/plan-code-update.md) | The voice code-update design + adversarial review. The feature is **built, deployed and in use**; its residue is §2 below. |
| [completed/2026-07-31-launch-prompt-transport-report.md](completed/2026-07-31-launch-prompt-transport-report.md) | The first end-to-end run's own report. Diagnosed the prompt-transport defect and records the three ghost-cli releases that fixed it. Evidence, not a plan. |

**Status markers:** `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked.
**Model routing:** **H** Haiku · **S** Sonnet · **F** Fable (→ Opus if unavailable, never Sonnet) · **O** Opus.

---

## Where things actually stand (2026-08-01)

**Newest first.** The mobile conversation shell shipped today (§4) — seven UI items, four green
pipelines, verified on the physical tablet against production. Running it on real hardware turned up
a **latent whole-page failure mode that predates it** (§4.3): the service worker serves `/static/*`
stale-while-revalidate on the stated assumption that everything there is fingerprinted, but JS
modules import their siblings by *logical* path, so a deploy that changes one module can hand a
client a mismatched sibling and kill the entire page silently. The owner asked for the
fingerprinted-import-specifier fix the same day and **§4.3 is now shipped**: an import map stamped
into every page makes all 24 modules resolve to content-addressed URLs, so the service worker's
"serving cached is always safe" premise is finally true rather than merely stated.

Everything below this line is the 2026-07-31 pass and is unchanged.

## Where things actually stood (2026-07-31, second pass)

Voice-driven code updates are **live and being used** — four launches through the full path today,
all with the deploy gate closed, DLQ empty. All three email legs (started / progress / completion
summary) are confirmed end to end.

Two production defects were found by *running it*, not by reviewing it. Both are now **fixed in
code and deployed**: the prompt transport (ghost-cli v1.1.52) and the Opus status vocabulary
(live-ninja `744d930`). §2.1's persistence question was decided by the owner and shipped
(`658f112`).

**§1 is now proven live.** A `preprocess:true` request on 2026-07-31 at 21:22:45Z collected its
rewrite and launched in **22.8 seconds** — the same work that used to run to the 240 s ceiling and
report a timeout that never happened. Full evidence in §1.4.

The one path still never exercised end to end is a real **spoken** run. Everything beneath the voice
tool is now verified against production.

---

## §1 — Opus pre-processing remediation `[~]` — fixed, deployed and PROVEN LIVE (`744d930`)

### 1.1 The defect

`preprocess` defaults to **true** — it is the headline behaviour of the feature ("use Opus to
pre-process the prompt, unless told not to"). It has **never worked in production**, and every
`preprocess:true` request silently wastes ~4 minutes and one Opus job.

The two sides disagree on the job-status vocabulary, in two independent ways:

| | ghost-cli writes (`lambda/command/schedule_prompt_job.go:61-63`) | live-ninja compares (`internal/ghost/client.go:305-307`) |
|---|---|---|
| in progress | `"pending"` | `"PENDING"` |
| success | `"done"` | `"DONE"` |
| failure | **`"error"`** | **`"FAILED"`** |

`internal/codeupdate/dispatch.go:300,313` switches on those constants. Neither `case` can ever match,
so:

- a **successful** rewrite is never collected — the poll loop runs to its 240 s ceiling, returns
  `noteTimedOut`, and the run launches with the owner's raw wording;
- a **failed** rewrite is not detected either — it also burns the full 240 s instead of failing fast.

**Verified live, 2026-07-31.** A real job (`019fb7af-a659-70df-94c3-b425a3980d54`) was submitted
through the internal-invoke path: `202 {"status":"pending"}` → **`"done"` with a 3405-character
rewrite in ~30 s**. Bedrock, the quota, the model and the worker are all healthy. **The rewrite is
produced correctly and then thrown away by a string comparison.**

Cost of the bug per request: ~240 s of dispatch-worker time, one unit of the 10-per-10-minute Opus
quota, and one Bedrock Opus call — all discarded. It is *reported* (the confirmation email says "the
Opus rewrite did not finish in time"), so it is not silent, but it is wrong and it is misleading:
nothing timed out.

### 1.2 The fix `[x]` — shipped in `744d930`

- `[x]` `internal/ghost/client.go`: the constants are ghost-cli's actual wire values —
  `pending` / `done` / `error`. `PreprocessFailed` was deleted, not aliased; nothing read it.
- `[x]` `ghost.PreprocessIs` compares with `strings.EqualFold` (plus `TrimSpace`), so a future
  casing change on either side costs a slow path rather than a discarded rewrite.
- `[x]` `internal/codeupdate/dispatch.go` switches on the corrected values, and an **unrecognised**
  status ends the wait after the poll that produced it, naming the value it could not read.
- `[x]` `noteRewriteError` distinguishes "the rewrite reported an error" from `noteTimedOut`, which
  now means only that the deadline genuinely passed.
- `[x]` **Found during review, same defect class, not in the original plan:** every poll *error* was
  swallowed as retryable, so a 404 (TTL-expired row, or a job that was never ours) also spun the
  full 240 s and reported the same false timeout. `ErrNotFound` / `ErrNotAuthorized` are now
  terminal; everything else still retries, because one poll failing is not the job failing.

### 1.3 The test that should have caught it `[x]` — shipped in `744d930`

- `[x]` `TestPreprocessStatusesMatchGhostCLI` pins the three literals with the file:line reference,
  in the same shape as `TestInternalInvokeTaskMatchesGhostCLI`. This is the **third** pinned
  cross-repo contract; `docs/system-map.md` now says three, and the RCA map budget was raised
  9200 → 9600 to fit it (deliberate, recorded in `internal/rca/prompt.go`).
- `[x]` Every uppercase fixture is gone. `TestDispatchUsesOpusRewriteWhenRequested` now drives the
  real lowercase values; it had passed for the whole life of the bug because its fixture returned
  `"DONE"`, which production never sends.
- `[x]` `TestUnknownPreprocessStatusFailsFastRatherThanTimingOut` (one poll, and a pending job still
  waits out its deadline), `TestPreprocessErrorIsReportedAsAnErrorNotATimeout`, and
  `TestUnreachablePreprocessJobFailsFast`. All were mutation-checked: each fails when the fix is
  reverted.
- `[x]` Adversarial review: 16 agents, four lenses, 12 findings raised and **all 12 refuted** on the
  source. Nothing survived to fix.

### 1.4 Verify in production `[~]` — the rewrite is PROVEN live; the spoken path is not

- `[x]` **Node rolled.** OFFICEPC reported `1.1.51-dev` at 15:48Z, which predates `stageLaunchPrompt`
  (absent at `v1.1.51`, present at `v1.1.52`; releases stamp the version via `-ldflags -X
  main.Version`, so a `-dev` suffix is a local build). It is now on **`1.1.54`** and the verification
  below ran against it.
- `[x]` **One `preprocess:true` request, end to end, 2026-07-31 21:22:45Z.** Request
  `019fba0e-e310-73fa-9ad1-74440cffc139`, repo `ftwr-codeagent-canary`, deploy gate closed.
  **The rewrite was collected and launched in 22.8 s.** ghost-cli's own log tells the story the
  old code could not read:

  | | | |
  |---|---|---|
  | poll 1 | 6 s | `pending` |
  | poll 2 | 12 s | `pending` |
  | poll 3 | 17 s | `pending` |
  | job completed | 18 s | — |
  | **poll 4** | **22 s** | **`done`** |

  Poll 4 is the exact instant the bug used to fire: `done` matched no `case`, the loop kept going to
  240 s, and the owner was told it timed out. It now launches immediately —
  `codeupdate: launched … "rewritten":true` at 21:23:21Z, run `019fba0f-6dc9-76dd-b659-652250588f28`,
  row `status=launched` / `rewritten=true` with no `rewriteNote` and no `error`. DLQ empty.
  `rewritten=true` is set only on the branch that assigns `body = refined`, so the launched prompt
  IS the expanded brief by construction, not by inference.
- `[ ]` **S** — One real **spoken** run — still the only path never exercised end to end. The voice
  tool defaults `preprocess` to true, so it covers the tool layer and the rewrite at once. Everything
  below the tool is now proven.

---

## §2 — Code-update residue `[~]`

### 2.1 Ours

- `[x]` **Three run rows stuck `RUNNING`** (`newgh-smoke-test` 08:31, `ai-template` 08:51,
  `ftwr-codeagent-canary` 09:08) — sessions killed during testing. The 2 h grace expired by 11:08;
  it was waited out, which was the right answer. **Do not** weaken the per-repo in-flight guard —
  it is working as designed, and it is the reason a killed session cannot be relaunched over.
- `[x]` **`CODEUPD#` persists `instructions`.** Owner decision 2026-07-31: persist, bounded by the
  row's existing `RecordTTL` (24 h), in the owner's own partition, never logged, and deliberately
  **not** returned by `code_update_status` — the reader is a human diagnosing a failed run, not the
  model, and a test pins that. Shipped in `658f112`.

### 2.2 ghost-cli follow-ups (pre-existing gaps our path makes reachable)

- `[ ]` **S** — `POST /schedule/preprocess` is authorized but **never audited**. It is a write gated
  on `ActionLaunch` and a billable Opus spend, yet `SchedulePromptHandler` has no audit sink — one of
  the five internally-reachable routes writes no hash-chain entry.
- `[ ]` **S** — `GET /launch/repos` performs **no `Authorize` call at all**; it checks only that a
  principal is non-empty, which on the internal-invoke path is a tautology.
- `[ ]` **S** — **`GET /schedule` returns each event's full prompt, which contains the live `cu_` run
  token.** Bounded (the token only emails the owner, 8 posts, 24 h) but it should be redacted.
- `[~]` **H** — **Emit `no_push` from the cloud.** The agent honours it as of ghost-cli v1.1.53.
  **live-ninja's half is DONE** (2026-08-01): `ghost.LaunchRequest.Deploy` is sent on every
  `/schedule` call, non-`omitempty` so the security-relevant `false` is always transmitted. Pinned
  by `TestLaunchCarriesTheDeployDecisionOnTheWire`.

  Safe to have shipped ahead of ghost-cli reading it, and that was VERIFIED not assumed:
  `lambda/command/schedule.go:294` decodes the create body with a plain `json.Unmarshal`, so an
  unknown key is ignored. The strict `DisallowUnknownFields` decoder is on the AGENT's command
  envelope, one hop further on.

  **Remaining: the ghost-cli half** — read `deploy` on schedule create, carry it to
  `buildLaunchParams` (`lambda/command/params.go:157`), emit `no_push: true` when it is false.

  **DECIDED (owner, 2026-08-01): option (a)** — converge the fleet first, then emit `no_push`
  unconditionally. No version gate. The alternative considered and rejected was gating the
  emission on each node's reported `agent_version`.

  **Fleet state — measured 2026-08-01 12:5x UTC, not assumed.** Read from the IoT retained status
  messages on `cockpit/nodes/<id>/status` (the same source `GET /nodes` uses), taking the
  RETAINED-MESSAGE TIMESTAMP as well as the version. The timestamp is what makes this table mean
  something: a node that has not published in six weeks is not a node running an old version, it
  is a node that is off.

  | Node | agent_version | retained at | state | ≥ 1.1.53 |
  |---|---|---|---|---|
  | Windows2 | 1.1.55 | 08-01 12:51 | IDLE_ALIVE | yes |
  | Right-Board | 1.1.55 | 08-01 12:50 | IDLE_ALIVE | yes |
  | OFFICEPC | 1.1.54 | 08-01 12:50 | IDLE_ALIVE | yes |
  | **Windows1** | **1.1.49** | 08-01 12:50 | **CRASHED** | **no** |
  | Left-Board | 1.1.39 | 07-28 11:44 | stale (4 d) | no |
  | rog-18 | 1.1.33 | 07-26 00:32 | OFFLINE | no |
  | Lenovo14 | 1.0.13 | 06-21 10:23 | stale (6 wk) | no |
  | rog-flow | 1.0.11 | 06-21 05:07 | OFFLINE | no |
  | elite001, acer-gpu, 4x twix-gpu | — | no retained message | unknown | unknown |

  Rollout target is **1.1.56** — `releases/latest.json` AND `releases/canary.json` in
  `s3://ghost-cli-releases-759775734231` both read 1.1.56. So nothing is being held back by the
  rollout; the laggards are simply not applying it.

  **What this reframes.** "Converge the fleet" is mostly not a software task:

  - Only **four** nodes are actually live (published within the hour). Three of them are already
    ≥ 1.1.53. The others cannot converge while they are powered off — an update cannot be pushed
    to a machine that is not running, so this is blocked on physical access, not on code.
  - The ONE live node below the bar is **Windows1, and it is CRASHED** — see the separate item
    below. It will not self-update in that state.
  - Offline nodes are not a live brick risk either: a LAUNCH cannot reach a node that is not
    connected. The exposure is a node that has been off for months coming back and receiving a
    HELD launch inside its first update poll. Narrow, but real, and it is the residue option (a)
    accepts by design.
  - Even OFFICEPC (1.1.54, live, healthy) has not taken 1.1.56 — so the self-update path is
    lagging on a node with no other problem. Worth understanding before declaring convergence
    done, because it suggests the updater, not the machines, is the thing that is stuck.

  **Next actions, in order:** (1) get Windows1 out of CRASHED and onto 1.1.56; (2) find out why a
  healthy OFFICEPC sat two versions behind; (3) decide whether the six no-retained-message things
  are decommissioned (and should leave the inventory) or dormant; (4) only then ship the ghost-cli
  half unconditionally.

  **Raised in importance by the 2026-08-01 default flip (§2.4).** While the default was "hold", a
  prompt-only gate failing open meant a run shipped work the owner had not asked to ship. Now that
  the default is "push", the only runs that carry the gate at all are ones where the owner
  *explicitly said don't* — so every failure of this prompt-only mechanism is now a direct
  violation of a stated instruction, on the exact request most likely to be sensitive. Fewer runs
  depend on it; the ones that do depend on it more.

- `[~]` **Windows1 reports CRASHED — but the earlier diagnosis of WHY was wrong.** Re-measured
  2026-08-01 20:20Z direct from the retained topics and the ghost-cli source; the corrections
  matter because the old reading pointed at the wrong fix.

  **What is true:** `cockpit/nodes/Windows1/status` carries `state=CRASHED` at
  `agent_version=1.1.49`, `uptime_s=163460` (45.4 h), heartbeating every ~20 s.

  **Correction 1 — CRASHED is a SESSION state, not an agent state.**
  `agent/internal/intelligence/engine.go` `sessionState.inferState` returns `state.Crashed` when a
  session's process is observed dead and that session had seen activity ("CRASHED overrides
  everything (process died)"). The AGENT is alive and publishing — a crashed agent would go
  silent, and this one has not. So "the agent crashed" was never what the topic said.

  **Correction 2 — "a crashed agent will not self-update" is unsupported.** There is no state
  gate in `agent/internal/updater`. Nothing found so far ties session state to update eligibility,
  so CRASHED does not explain the stale version and restarting the node would not fix it.

  **Still open:** why Windows1 sits on 1.1.49 when the fleet manifest is 1.1.57. Needs node-side
  updater logs — not answerable from the retained topic. The `updater: manifest version was
  applied within the cooldown` anti-thrash path (`updater.go:264`) is the first place to look;
  it fires when a published binary's stamped version does not match its manifest, which would
  look exactly like "stuck" — though 1.1.57 stamps correctly for the two nodes that took it.
  **Restarting is NOT the indicated action** on the current evidence, and it belongs to whoever
  owns that machine (its status shows a `C:\Users\joshua` profile).

- `[~]` **The fleet has PARTLY converged since this was written — re-measured 2026-08-01 20:20Z.**
  The original claim ("no node is on the published fleet version") is no longer true, and the
  shape of the problem changed with it.

  | Node | State | Version | Canary member? |
  |---|---|---|---|
  | Windows2 | IDLE_ALIVE | **1.1.57** | no — took the fleet manifest |
  | Right-Board | IDLE_ALIVE | **1.1.57** | yes |
  | OFFICEPC | IDLE_ALIVE | **1.1.54** | **yes** |
  | Windows1 | CRASHED (session) | **1.1.49** | no |

  `releases/latest.json` and `releases/canary.json` are BOTH 1.1.57 (identical sha256), and
  `releases/canary-nodes.json` is `{"nodes":["Right-Board","OFFICEPC"]}`. So the canary/fleet
  split is not the cause of the spread — both manifests point at the same build.

  **The sharp remaining anomaly is OFFICEPC**, and it is the opposite of what the old note said:
  it is a *canary member*, so it should receive a build FIRST, and it is the furthest behind of
  the three alive nodes. Two nodes taking 1.1.57 cleanly proves the manifest, the signature, the
  bucket policy and the apply path all work — so this is specific to OFFICEPC, not "the updater".
  Needs that node's updater log; nothing in the retained status says why.

- `[ ]` **Six inventory entries have never published a status.** `elite001`, `acer-gpu`,
  `left-twix-gpu0/1`, `right-twix-gpu0/1` have no retained message at all, so `GET /nodes` renders
  them as `-` and no version-based logic can reason about them. Decide whether they are
  decommissioned (remove them from the IoT inventory so the fleet count means something) or
  dormant-but-real (they are then a live brick risk the moment they wake and take a held launch).


### 2.3 Closed this pass — do not redo

- `[x]` **Staged prompt now has a real ACL.** `os.WriteFile(..., 0o600)` sets no permissions on
  Windows — Go maps the mode onto the read-only attribute and the file inherits its parent's DACL,
  so it landed `-rw-r--r--` in `%TEMP%` carrying the run token. `writeOwnerOnlyFile` creates it with
  a PROTECTED DACL granting only the running user and SYSTEM. **ghost-cli v1.1.53/54**, verified live
  on OFFICEPC: `NT AUTHORITY\SYSTEM` + `OFFICEPC\Jeremy`, no inherited ACEs.
- `[x]` **The deploy gate is no longer only a sentence in the prompt.** `launcher.ApplyNoPush`
  installs a `pre-push` hook in the workspace for a launch that is not deploy-authorized, and
  REMOVES its own hook when one is — a stale hook would otherwise block a legitimate push and look
  like a git fault. A hook the owner wrote is never touched. Proven against real git: push refused
  with the commit still allowed, and pushing restored after removal. **Agent half only — see 2.2.**

- `[x]` **Prompt transport.** The node agent typed 3400 keystrokes into the TUI; the head was lost to
  volume and the verifier keyed on the destroyed head, so all four retries fired and the deploy gate
  never arrived. Fixed in **ghost-cli v1.1.52**: prompts over 300 runes are staged to a file and only
  a pointer is typed. Two wrong turns are recorded in the archived report — read it before touching
  that path again. **Rolled**: OFFICEPC is on v1.1.54 and a live launch confirmed the pointer on
  attempt 1 with `verified=true`. Other nodes converge on their next ~4 h poll (`latest.json` =
  1.1.54).
- `[x]` **The `live-ninja` grant is now deploy-owned.** It was hand-seeded in SSM, and *every*
  ghost-cli deploy overwrote `/ghost-cli/authz-allowlist` with an owner-only document, silently
  killing the feature. The two-entry document now lives in ghost-cli's `AUTHZ_ALLOWLIST_JSON` repo
  variable and survives deploys.

### 2.4 Locked decision — the deploy gate is open by default (owner, 2026-08-01)

**Decided:** `deploy` defaults to **TRUE**. A launched run commits *and pushes* and watches the
pipeline to a terminal result. Holding a change is now the explicit opt-out ("...but don't push").

**Why the original closed default was reversed.** It was costing more than it saved. A run would do
the work, verify it, and stop with everything committed and unpushed; the owner then had to come
back and say "push" by hand — a second decision point on a change they had already asked for and
already confirmed once. Finished work sat on a machine nobody was watching. The trigger was the
2026-08-01 Help-panel run, which did exactly that.

**What the closed default was protecting, and why that cover is not lost.** It existed so a
*misheard* voice command could not ship to production. That protection does not actually live in
this flag: `code_update_start` already requires an explicit `confirm`, which the model may only set
after stating the repository and the change and getting agreement. Nothing reaches the deploy
decision without the owner having heard it back and said yes. The flag was a second lock on a door
that already had one — and it locked the side the owner was standing on.

**What is genuinely weaker now:** the hold path is prompt-only until the `no_push` item in §2.2
ships. See the note there.

**Where it lives:** default at the tool boundary (`internal/tools/codeupdate.go`, the `deploy`
parse + schema text); wording in `deployRules` (`internal/codeupdate/prompt.go`). The wire/queue
default is deliberately still the zero value, so a malformed or truncated message holds rather than
deploys — only real owner intent flips it on.

---

## §3 — Carried forward from the archived plan

Unchanged in substance; these were open when [completed/plan.md](completed/plan.md) was archived.

### 3.1 Verification, owner/hardware gated `[~]`
- `[~]` **Gemini Flash Live E1/E2** — production mint/audio/tool/cost work and one real
  within-token recycle is proven; E1 partial, >30-minute continuation policy/implementation-blocked.
- `[~]` **Tool-manifest live smoke** (owner) — now also covers the three `code_update_*` tools, which
  take the catalog to 29.
- `[~]` **Android device** — needs the owner-unlocked physical tablet.

### 3.2 Owner decision needed `[ ]`
- `[ ]` WS-3.3 from the archived plan — still awaiting the owner.

### 3.3 M31 named devices + per-device settings `[~]`
- `[~]` Android legacy-row retest pending (web path passes).

### 3.4 M8 Launch `[!]`
- `[!]` **Distribution** — web is live; the `v0.2.2-hal`/code-5 release workflow needs owner-managed
  signing material. SES production access, cost-allocation tags and the production end-to-end smoke
  are all done.

---

## §4 — Mobile conversation shell + the module-graph cache hazard (2026-08-01)

### 4.1 The shell `[x]` — shipped, four green pipelines

Seven owner-requested UI items on the **web** `/conversation` page. The Android app was deliberately
NOT touched: it is native Compose (`android/.../ui/screens/ConversationScreen.kt`) with its own
bottom `NavigationBar`, and the request was written in CSS/DOM terms ("44×44 **CSS** pixels",
"~80**vh**", "a `<select>`", "**html2canvas**", "**local storage**") naming controls that exist only
on the web page — the edge tabs and the `＋` glyph. Recorded so nobody re-litigates the surface.

| Commit | What | Run | Result |
|---|---|---|---|
| `bb488e3` | Mobile shell (all seven items) | 30699017769 | success |
| `5ee4cc2` | 390px bottom-bar fit | 30699237771 | success |
| `e5baa39` | Playwright edge-bar spec | 30699422933 | success, `web-quality` green |
| `48f7aa2` | `update-report.md` | 30699711731 | success |

What landed: two snap panels at ≤900px (swipe up for the transcript, chain back for the voice
panel); a persistent bottom bar (Show Conversation / History / Memory / Downloads / Audio); a modal
`<dialog>` conversation overlay mirroring `#transcript` live; Copy, a dependency-free canvas→PNG
Screenshot, and a localStorage-backed Tag for review, all bound by `data-conv-action`; edge tabs
moved to the left at 16px glyphs with the 44px target intact; `NEW` in place of `＋`.

**Definition of done (all passed):** `go build ./... && go vet ./... && go test ./...`;
`cd tests/web && npx playwright test` → 71 passed / 21 skipped / 0 failed;
`gh run view 30699711731 --json conclusion` → `success`.

Two decisions worth not re-opening:
- **There is no audio-quality setting in this product.** The bottom bar's picker drives the real
  `turnDetection.micEagerness` (the same value the rail's Low/Med/High chips write, synced through
  `syncMicChips()`), and is labelled **Audio**, not "Audio quality". `auto` is offered as a fifth
  option because it is the schema default.
- **Tag for review is local** (`localStorage['ln.reviewTags']`). There is no server-side review
  queue to post to; the Help panel says so rather than implying the note was filed.

Full evidence — measurements, screenshots, the drafted audio-verification email — in
[update-report.md](update-report.md).

### 4.2 A regression this run caused and fixed `[x]`

`tests/web/specs/settings-accordion.spec.mjs` pinned the settings opener flush **right**
unconditionally; moving the tabs to the left edge on phones broke it under the `mobile-chrome`
project (expected 412, got 44). `web-quality` is `continue-on-error`, so the deploy still went
green — it was caught only because that job had been **green on the two runs before**. The spec now
asserts the real contract (same size, opposite edges, viewport-aware). Shipped in `e5baa39`.

**Gotcha:** `web-quality` failing does NOT fail the run. Compare against the previous runs' result
before dismissing it as noise.

### 4.3 The module-graph cache hazard `[x]` — fixed 2026-08-01 (owner asked; import map shipped)

**The defect (verified live, not theorised).** On the Tab S9 FE, after the `bb488e3` deploy,
**nothing driven by JavaScript worked** — not the new overlay, and not the Settings or Help drawers
that have shipped since July. Read off the device's real console over the Samsung Internet DevTools
socket:

```
Uncaught SyntaxError: The requested module './wakeword.mjs' does not provide
an export named 'applyWakeWordSettings'
  @ https://live.jeremy.ninja/static/js/conversation.fe215b7cbdb9.mjs:40
```

A module-linking failure kills the **whole** of `conversation.mjs`, which is why every button was
inert while plain `<a>` links and CSS scrolling still worked. There is nothing on screen to say why.

**Verified facts** (each confirmed by command, not inferred):
- The origin is correct — `curl https://live.jeremy.ninja/static/js/wakeword.mjs` contains
  `export async function applyWakeWordSettings`, and `fetch(url,{cache:'reload'})` on the device
  agreed. The stale copy was client-side.
- `web/sw.js` serves `/static/*` **stale-while-revalidate**, and its own header comment states the
  premise: *"they are fingerprinted/immutable at build, so serving cached is always safe"*.
- That premise is **false for JS modules**: `web/templates/**` loads entry modules by their
  fingerprinted URL via `asset()`, but every module imports its siblings by **logical** path
  (`from './wakeword.mjs'` — 14 distinct specifiers across `web/static/js/*.mjs`).
- So a deploy that changes `conversation.mjs` mints a fresh, guaranteed-correct URL for it while its
  siblings keep URLs the SW may satisfy from a pre-deploy cache entry. **Every future
  `conversation.mjs` change is a coin-flip for any client holding an old sibling.**

**The chosen fix — fingerprinted import specifiers via an import map** (owner asked 2026-08-01):

- `[x]` `internal/webapp/assets.go` — `buildImportMap()` runs at `NewAssets` and maps every logical
  `/static/**/*.mjs` to its hashed path (24 entries), plus `ImportMapCSPHash()` over the exact
  rendered bytes. `json.Marshal` sorts map keys, which is what makes the bytes stable enough to hash
  once at startup.
- `[x]` `internal/webapp/pages_routes.go` — `pageCSPWith()` splices the hash INSIDE `script-src`
  (appending to the policy string would have landed it in `frame-ancestors`);
  `SecurityHeaders(*Assets)` uses it; `importMap` template func added.
- `[x]` `web/templates/layouts/base.html` — emits `{{importMap}}` above `{{block "head"}}`.
- `[x]` `cmd/web/main.go` — passes `assets` to `SecurityHeaders`.
- `[x]` `internal/webapp/import_map_test.go` — five guards: every `.mjs` mapped; every relative
  specifier *actually written in the sources* resolves to a mapped entry (this is the one that fails
  when a new module is added and the map silently stops being complete); the map precedes the first
  module `<script>` on all six pages; the CSP hash matches the rendered bytes; the hash lands in
  `script-src` and `'unsafe-inline'` never appears there.
- `[x]` `web/sw.js` — comment only. Its "always safe" premise is now true, but only *because* of the
  import map, so the dependency is named where someone would otherwise remove it.

**Verified, not assumed** — a throwaway harness ran the real `NewAssets` + `NewRenderer` +
`SecurityHeaders` and was driven in Chromium: **16/16 `.mjs` fetched from fingerprinted URLs, zero
logical stragglers**; dynamic `import('./personaeditor.mjs')` and the vendored ORT module both
resolved fingerprinted; no CSP violation; `go test ./...` green; `npx playwright test` 71 passed /
21 skipped / **0 failed**, including the service-worker cache regressions.

**No Help-panel change.** The Help maintenance rule covers user-visible features, settings, pages
and tools; this is invisible to users — nothing they can see or do changed.

**Constraints that shape it** (do not rediscover these):
- **CSP forbids inline scripts** (`script-src 'self' 'wasm-unsafe-eval'`), so the import map needs a
  **hash source**, not `'unsafe-inline'`. External import maps (`<script type="importmap" src=…>`)
  were removed from the spec and are not implemented anywhere — that door is closed.
- **Import maps do not apply to worklets.** `wakeword.mjs` loads
  `/static/js/wakeword-worklet.js` via `audioWorklet.addModule()`; that URL stays logical and is out
  of scope for this fix. It is a classic worklet with no imports, so it cannot fail to *link* — the
  worst case is behavioural drift, not a dead page.
- `import(ORT_MODULE_URL)` **is** covered (a `/static/…` URL specifier is remapped), and
  `ort.env.wasm.wasmPaths = ORT_WASM_DIR` is set explicitly, so fingerprinting the ORT module's URL
  does not move its `.wasm` lookup.
- A browser without import-map support ignores the map and resolves logical specifiers — i.e. it
  degrades to today's behaviour rather than breaking.
- **Do not "fix" this by weakening the service worker.** SWR on genuinely content-addressed URLs is
  correct; the bug is that the URLs were not content-addressed.

**Definition of done:** `go test ./internal/webapp/` green; `cd tests/web && npx playwright test`
green (it has explicit module/caching regressions); the deployed `/conversation` HTML contains an
`importmap` whose entries all resolve 200; the pipeline reaches `success`.

**A regression the fix itself caused, and the test gap behind it** (`1d4e0fa`, same day):

The first cut mapped **every** `.mjs`, including the vendored onnxruntime module. But
`template.yaml` routes `/static/vendor/*` and `/static/models/*` to the **`assets-s3` origin**, not
to this app — they are the oversized ORT WASM bundle and the wake-word models, which cannot go
through Lambda. That bucket holds the real filenames only, so a fingerprinted key does not exist and
S3 answers **403** (not 404):

```
403  /static/vendor/ort/ort.wasm.min.f53ed4792e75.mjs
200  /static/vendor/ort/ort.wasm.min.mjs
```

`import(ORT_MODULE_URL)` therefore rejected and **the wake-word engine could not start in
production for ~25 minutes.** Caught on the tablet, not by any test.

**Why nothing caught it:** all five original guards asked *"is every module mapped?"* — none asked
*"is every mapped URL actually servable?"* The local harness served everything from the Go handler,
so it could not reproduce the CDN's origin split. `TestImportMapSkipsEveryS3BackedPath` now parses
`template.yaml` for behaviours whose `TargetOriginId` is the S3 origin and requires `assets.go`
(`s3BackedStaticPrefixes`) to exclude each one, so a new S3-backed behaviour fails the build instead
of 403-ing in production.

Excluding them is right on the merits: they are leaves reached by absolute URL, not part of the
cross-module export graph the map exists to keep consistent, and `wakeword.mjs` already pins those
payloads by SHA-256 client-side. Final map: **22 entries, all under `/static/js/`.**

**Rejected alternative, recorded so it is not retried:** rewriting the import specifiers inside the
`.mjs` bodies at fingerprint time. It needs transitive hashing (`hash(A) = f(content(A), hash(deps))`)
with cycle detection, or it silently pins clients to a consistent-but-stale graph — strictly more
machinery than an import map for the same result.

### 4.4 Locked decisions — mobile shell corrections (owner, 2026-08-01) `[x]`

Shipped in `5b23986`. **Do not revisit these three; they are the owner's, not inferences.**

1. **The bottom bar owns what it carries.** Anything on the bar is not repeated on the panel above
   it: the rail's `.conv-rail__nav` (History/Memory/Downloads) and `.conv-miclineup` (Mic Test +
   Low/Med/High) are `display:none` at ≤900px, and the scroll-hint button is deleted. Both rail
   blocks stay for desktop, where no bar exists. The Audio picker's "Mic test…" is what replaces the
   hidden Mic Test button.
2. **Show Conversation is the entire screen, and the same button says Hide.**
3. **Never more than one scrollbar on screen.**

**The non-obvious consequence, recorded because it is easy to "fix" backwards:** (2) makes a modal
dialog impossible. `showModal()` inerts everything outside the dialog, so the bottom-bar button that
is supposed to hide it again cannot be pressed. The overlay is therefore a **non-modal** `<dialog>`
(`.show()`) laid out **in flow** as a flex sibling of `.conv-body` inside `.conv-app` — which is
also what makes it stop exactly where the bar starts with nobody measuring the bar's height.
Reverting it to `showModal()` silently breaks the toggle. What the modal used to supply is now
explicit: Escape and focus handling in `conversation.mjs`, and
`.conv-app.is-overlay-open .conv-body { display: none }` in place of the modal's inertness — which
is also half of (3).

**How (3) is achieved:** the snap scroller's own scrollbar is hidden (`scrollbar-width: none`)
because it is a **panel switcher**, not content — each child is exactly one scrollport tall, so its
scrollbar only ever said "there is another panel" while the panel being read showed its own. Hiding
a scrollbar does not disable wheel, touch or keyboard scrolling.

**Found by looking at the rendered page, not in review:** the `position: fixed` Help/Settings edge
tabs painted OVER the in-flow overlay and clipped its left edge (timestamps rendered as "01 AM").
They are hidden while the overlay is open; the bottom bar is deliberately the only chrome that
outlives it.

Guarded by `TestBottomBarControlsAreNotDuplicatedAbove` and the rewritten
`TestConversationOverlayContract` / `TestMobileSnapPanels` in
`internal/webapp/mobile_shell_ui_test.go`.

## §5 — Corner tabs, an always-on icon bar, working personas (2026-08-01, second pass)

All items owner-requested in one batch and shipped together. `[x]` unless noted.

### 5.1 Web `[x]`

- `[x]` **Settings and Help are now two ~40px tabs in the UPPER-LEFT corner**, at every width, gear
  above `?`, each icon-only with a native `title` tooltip (`--ln-edge-tab: 40px`). They replace the
  vertically-centred 40dvh bars on the right edge.
- `[x]` **`NEW` moved to the left of the orb row and 15px below the orb's bottom edge**
  (`.ln-orb-newconv { left: 0; bottom: -15px }`).
- `[x]` **The bottom icon bar is shown at every window size**, not just ≤900px. Its rules moved out
  of the mobile media block to the top level, and the rail's duplicate `.conv-rail__nav` /
  `.conv-miclineup` are now hidden at every width — the bar is the single home for History /
  Memory / Downloads and the Audio picker.
- `[x]` **The page can no longer scroll past the bottom bar.**

### 5.2 The root-scroll defect, and why `.conv-app { overflow: hidden }` was not enough `[x]`

Worth recording because the fix is not where the symptom is. Below 900px `.conv-body` is a snap
scroller whose two children are each a **full scrollport tall**, so the second one extends a whole
viewport past `.conv-app`'s box. `.conv-app`'s `overflow: hidden` clips it *visually*, but the
**root scroller still counted it**: the document got a phantom ~viewport-tall scroll range, and
scrolling into it carried the bottom bar off the top of the screen. Measured in Chromium at
390×844 before the fix: `documentElement.scrollHeight` **1526** against a `clientHeight` of **844**,
and a real wheel gesture moved `window.scrollY` to **682**.

Two traps in diagnosing it:

1. `scrollHeight` alone does not tell you whether something is *scrollable* — an `overflow: hidden`
   box is still programmatically scrollable, so `window.scrollTo(0, 99999)` "succeeds" even after
   the bug is fixed. **Verify with a real wheel gesture** (`page.mouse.wheel`), not `scrollTo`.
2. `overflow: hidden` on `<html>` does not fix it either. The value that reaches the viewport is
   propagated from `<body>` when `<html>` is `visible`, so the rule has to land on `body`.

Fix: `pages_routes.go` stamps `BodyClass: "ln-body--fixed"` on `/conversation` only (the mechanism
existed and had never been used), and app.css gives that class `overflow: hidden` +
`overscroll-behavior-y: none`. Every other page keeps normal page scrolling.

**How it was reproduced without a server.** `/conversation` needs DynamoDB/KMS/SSM to boot, so
there is no local harness. A throwaway Node script stripped the Go template actions out of
`conversation.html`, inlined `audio_viz.html`, and inlined `app.css` into one static file, which
Playwright then measured at three viewports. That is what turned "the scrollbar goes too far" into
a number, and it is the cheapest way to re-check a pure-layout change on this page.

### 5.3 Android `[x]`

- `[x]` **Settings is a 48dp square tab in the upper-left corner** (`SETTINGS_TAB_SIZE`), mirrored
  to the upper-right inside the settings modal. It replaces a bar that was centred on the RIGHT
  edge and 40% of the screen tall — **that bar was the "conversation is cut off"**: it sat directly
  on the transcript's right-hand column and clipped every user bubble behind it. A corner tab can
  only ever overlap one corner, and `ConversationScreen` reserves exactly that corner.
- `[x]` **The tab needed `windowInsetsPadding(WindowInsets.statusBars)`** — it is drawn outside the
  `Scaffold`, so nothing had applied the status-bar inset and it landed under the system clock.
  Caught on the physical tablet, not in review. The modifier must precede `size()`.
- `[x]` **State pill + cost badge in the two top corners**, matching web's rail top row. The cost
  now shows whenever an estimate exists, not only while the session is live — blanking it the
  instant the session ended is what made it look like the app had no cost display at all.
- `[x]` **New conversation** (`startNewConversation()`): a full stop/start when live, because the
  session id is what the backend keys `LOG#/CONV` rows against — the same thing the spoken
  `start_new_conversation` tool does, deliberately not a transcript clear.
- `[x]` **Mic pickup Low/Med/High** on the conversation screen, writing `micEagerness` to the
  settings document. It is consumed **server-side** (`api_routes.go` reads it at mint,
  `mint.go` turns it into `turn_detection.eagerness`), so it applies to the **next** session and
  the screen says so while one is up rather than implying it already landed. Verified end to end:
  the tablet's synced document already held `"high"` from the web client and the chip reflected it.

### 5.4 Personas `[x]`

- `[x]` **Yoda (`swamp-master`) removed.**
- `[x]` **Twelve working personas added**, in two families. **PDLC** — `product-owner`
  (marin/Kore), `staff-developer` (cedar/Iapetus), `staff-sre` (echo/Schedar). **ESP32** — one per
  chip in the family: `esp32-engineer` (the original dual-core part), `-s2`, `-s3`, `-c2`, `-c3`,
  `-c5`, `-c6`, `-h2`, `-p4`. These are a different class from the entertainment built-ins: senior
  colleagues who **disagree out loud, say why, and name the alternative**.
- The per-chip split (owner request, same day) is justified by the silicon: which parts have a
  radio at all, how many cores, how much RAM, what the sleep current is. A single "embedded
  engineer" would have to hedge on every one of those. Each names the trap specific to its part
  and would be a non-issue on its neighbours — no Bluetooth on the S2, no Wi-Fi on the H2, no radio
  whatsoever on the P4, a bill of materials in cents on the C2, coexistence on the C6. ESP8266 is
  deliberately absent; it predates the family and shares almost none of the tooling. The P4 one is
  the most concretely grounded because it is the board this project ships
  (`firmware/sdkconfig.defaults`: M5Stack Tab5, 32MB hex PSRAM, ESP32-C6 radio slave, explicit
  internal-RAM discipline, OTA partitions).
- `[x]` **Picker groups** (owner request, same day). `Persona.Group` +
  `GroupGeneral/PDLC/ESP32/Fun` + `GroupOrder`, surfaced through `PersonaInfo`, the personas API,
  and one `<optgroup>` per group in the web picker. Counts verified against the real registry
  through the actual picker code: General 1, PDLC 3, ESP32 9, Fun 15. `groupOrDefault` files an
  untagged persona under General so it can never exist on the server and be unreachable in the UI,
  and `TestBuiltinPersonaGroups` fails if a persona is tagged with a group `GroupOrder` does not
  render. A `Group · Persona` caption sits under the picker, because a collapsed `<select>` shows
  only the option text and the `<optgroup>` heading disappears with the list.
- `[x]` **Per-persona picker off-switch** (owner request, same day). `persona.hidden` in
  `contracts/settings.schema.json` — additive, validated in `settings_routes.go`, registered as a
  known key in `store/settings.go` so an inherit/apply-all does not mistake it for a foreign
  additive field. Settings → Persona renders one fieldset per group with a whole-group button.
  **Opt-OUT by design**: only the switched-off ids are stored, so a persona added in a future
  deploy appears on its own — an allow-list would silently hide everything new.
  Two rails, both verified in a browser against the real registry: the persona currently
  SELECTED always renders even when hidden (otherwise the `<select>` shows a value it does not
  contain), and `default` can never be hidden — the route rejects it — so the picker cannot empty.
  With every id in the list the picker still renders General(1). It is presentation only:
  `ResolvePersona` never reads it, so a hidden persona still mints a working session if another
  device or a stored document still names it.
- `[x]` **Android's persona picker now reads the API** (was `[!]` earlier the same day). It was a
  hardcoded list offering `focused` / `friendly` / `coach` / `analyst` — **none of which exist in
  the server registry**. `ResolvePersona` falls back to `default` for an unknown id, so choosing
  "Coach Ninja" on Android silently gave you the standard persona and nothing said so. That is a
  latent bug this closes, not just a refresh.
  New `net/PersonaDtos.kt` + `listPersonas()` on `LiveNinjaApi`; `refreshPersonas()` mirrors
  `refreshGeminiVoices()` (failure leaves the list alone rather than emptying the picker). The
  dropdown is grouped with the same fixed order as web. `buildPersonaPresets` is pure and unit
  tested (6 cases) for the three rules that make it safe: hidden personas are dropped, the
  SELECTED persona survives even when hidden (otherwise the picker shows a different persona than
  the document holds and the next save writes that back), `default` is never dropped, a stored id
  the catalog no longer lists is kept and labelled rather than silently swapped, and an empty
  catalog falls back instead of offering only "custom". The picker is rebuilt on every document
  change, so hiding a persona on the web reaches Android on the next sync with no refetch.
- `[!]` **The Android picker is NOT yet visually verified on the tablet.** Reinstalling wiped the
  app's shared_prefs (directory recreated at install time; no crash and no exception in logcat —
  the app started clean), so the device is back at onboarding step 1 of 8, which needs an
  interactive Amazon sign-in. A populated picker also needs a signed-in session, because the
  catalog fetch is authenticated. **Unblocked by:** the owner signing in on `R52XC06P9KJ` once,
  after which `Settings > Persona` shows the grouped catalog. Compilation, the full Android unit
  suite and the six `PersonaPresetBuilderTest` cases all pass.
- Researched by a 10-agent workflow against **26 dated sources published 2026-05-02 → 2026-07-31**,
  then adversarially critiqued for capability leaks, spoken-form survivability (they arrive as one
  to three sentences of audio, so the rigour has to show up as *which question is asked first*) and
  cross-persona collision. The load-bearing ideas: the eval is what an AI feature actually promises
  and cost-per-finished-task is its unit economics; review capacity is the delivery ceiling once
  agents write the diff, so the plan is the artifact worth arguing; and reliability for an agentic
  system is *containment* — what stops it, and where in the call path that stop executes.
- `[x]` **Closed the `docs/qa-report.md` persona coverage gap in passing**: the seed-set test
  sampled 11 of 17 built-ins, so a wrong voice on an unsampled one went unnoticed. It now iterates
  the whole registry and checks the Gemini voice too. New `TestWorkingPersonasPushBackWithoutTaking
  Power` pins both halves of the three working personas.

## §6 — Cross-device agent collaboration over IoT push (planned 2026-08-01) `[ ]`

**Goal.** Several devices, each running Live Ninja under a *different* persona, work on one project
together: they share a document and a plan, and every device is told the moment either changes.

**Why this design.** The sharing half already works and needs no code — every store in this system
is keyed on `USER#<uid>`, not on the device, and `file_read` / `memory_search` are tool calls made
*during* a session, so they already read live state. The missing half is notification, and it is
missing for two independent reasons recorded in `internal/sync/sync.go:1-20`: there is no
server→client channel for web or Android (no FCM, no WebSocket API — both explicitly declined), and
even with one, the realtime session runs client↔provider directly, so the server has no seam into a
live conversation. This section closes the first gap with IoT Core, and the second by having the
*client* nudge its own session through the injection path it already owns.

### Locked decisions (user-confirmed 2026-08-01; do not revisit)

1. **Surfaces: web (browser) and Android only.** The M5Stack is out of scope for this section even
   though it is the one surface already IoT-provisioned.
2. **Auth: an AWS IoT custom authorizer that verifies the existing first-party ES256 access JWT.**
   Not Cognito, not backend-vended STS. Rationale: LWA is the identity provider and there is no
   Cognito Identity Pool; the JWT, its JWKS, and the `tokensValidAfter` kill-switch already exist
   and are already verified by `cmd/authorizer`, so this adds one Lambda and no second identity
   system.
3. **On push during a live session: auto-nudge — the agent speaks up unprompted.** The operator
   chose this over the two quieter options after the cross-device echo risk was stated. It is
   therefore built as specified, with the turn-taking rail in WS-5 as the mitigation rather than
   the softer behaviour as the mitigation.
4. **Events fanned out: document/deliverable writes, memory + plan writes, and session presence.**
   All three.
5. **Standing authorization.** Deploying these template changes through the normal push-to-`main`
   pipeline is pre-authorized, including the new IoT authorizer, the new Lambda, and the CSP change.
   Creating IoT Core resources in the account is pre-authorized. Deleting or reprovisioning any
   EXISTING M5Stack Thing or certificate is **not**.

### Verified facts (each confirmed by reading the file named)

- `internal/sync/sync.go:1-20` — the no-FCM / no-WebSocket decision, and that the M5Stack is
  currently "the ONLY real-push surface".
- `internal/sync/sync.go:79-178` — a working `Publisher` over `iotdataplane` already exists, with
  cached `iot:DescribeEndpoint` resolution. The publish half is largely built.
- `template.yaml:411-413` — the web function ALREADY holds `iot:Publish` on
  `arn:…:topic/liveninja/*`. No new IAM is needed to publish.
- `template.yaml:2472-2501` — `IotDevicePolicy` is X.509-cert based and scopes every action to
  `liveninja/${iot:Connection.Thing.ThingName}/*`. It is a per-Thing policy and does **not** cover
  a user-scoped topic; a browser has no Thing and no cert.
- `internal/auth/session.go:35` — `AccessTokenTTL = 15 * time.Minute`. An MQTT connection must
  outlive the token that opened it; see WS-1 M1.3.
- `cmd/authorizer/main.go:1-18` — the HTTP API authorizer already does ES256 verification against
  the cached JWKS plus the `tokensValidAfter` kill-switch. This is the logic WS-1 reuses.
- `internal/webapp/pages_routes.go:56` — the page CSP's `connect-src` does not include any IoT
  endpoint. A browser MQTT-over-WSS connection is blocked until it does.
- `android/app/build.gradle.kts:151-180` — the Android app has **no MQTT client**. okhttp and
  retrofit are present; `aws-crt`/Paho are not.
- `web/static/js/` + `internal/webapp/assets.go` — the web app has **no JS bundler**. Modules are
  served as plain `.mjs` through a stamped import map, and the CSP forbids CDN scripts.
- `web/static/js/realtime.mjs:2291` — `sendUserText()` already does
  `conversation.item.create` + `response.create`. The client-side injection primitive for the
  auto-nudge exists and does not need inventing.
- `internal/auth/device.go:201` — `ProvisionIoT` is the M5 Thing/cert seam. Untouched by this work.
- **Account IoT ATS endpoint** (resolved 2026-08-01): `a17oe0gnthrosw-ats.iot.us-east-1.amazonaws.com`.
  This is the origin M3.2 must add to the page CSP's `connect-src`, as `wss://`.
- `aws iot list-authorizers` returns `[]` — there is no custom authorizer in the account yet, so
  WS-1 is genuinely greenfield and nothing existing can break.

### Assumptions (NOT verified — treat as risk, prove in WS-1 M1.1)

- ~~That an IoT custom authorizer can authenticate MQTT-over-WebSocket by carrying the token in
  the MQTT CONNECT username/password rather than an HTTP header.~~ **RESOLVED 2026-08-01 — true,
  per the AWS IoT developer guide. See M1.1.** This was the assumption the whole design rested on.
- That `refreshAfterInSeconds` re-invokes the authorizer with the ORIGINAL connect token. If it
  does, a 15-minute JWT cannot survive refresh and WS-1 M1.3's reconnect strategy is mandatory
  rather than optional.

### Cost

IoT Core connectivity is ~$0.08 per million connection-minutes. Two devices connected continuously
is ~86,400 connection-minutes/month, i.e. **under one cent**. Messaging at $1/million is noise at
this volume. The new Lambda is invoked once per connect plus per policy refresh. Per the standing
rules: **no CloudWatch alarms and no dashboards**, and the new function's log group is declared
explicitly in the template with `RetentionInDays: 7`.

---

### WS-1 — IoT custom authorizer `[ ]` (blocks WS-2, WS-3, WS-4)

- `[~]` **M1.1 Feasibility: ANSWERED — the design holds. Stop condition 1 does not fire.**
  Settled 2026-08-01 from the AWS IoT Core developer guide rather than by live test, because the
  account has **zero** authorizers (`aws iot list-authorizers` → `[]`) and hand-creating one with
  `aws iot create-authorizer` would be exactly the local infra deploy `deploy.md` forbids. The
  live proof therefore moves AFTER M1.4, when the pipeline has deployed a real one.

  **The load-bearing quote** ("Understanding the custom authentication workflow"): *"The device
  passes credentials in either the request's header fields or query parameters (for the HTTP
  Publish or MQTT over WebSockets protocols), **or in the user name and password field of the MQTT
  CONNECT message (for the MQTT and MQTT over WebSockets protocols)**."* A browser cannot set
  WebSocket handshake headers — and does not need to. The JWT rides in the MQTT CONNECT username.

  Two constraints this turned up that the plan did not account for. Both change WS-1, neither
  breaks it:

  - **M1.5 (NEW) — token signing.** Signing is optional, but AWS is explicit: *"If you leave
    signing enabled, you can prevent excessive triggering of your Lambda by unrecognized
    clients."* With it on, AWS validates an RSA signature over the token **before** invoking the
    Lambda; with it off, anyone who knows the endpoint can spin our Lambda at will. A browser
    cannot hold a private key, so the signature must be minted SERVER-side and handed to the
    client alongside the JWT — the token mint returns `{token, tokenSignature}`. The signature
    must be URL-encoded when sent from browser JavaScript.
  - **M1.6 (NEW) — the authorizer Lambda has a hard 5-second timeout.** *"The Lambda function
    timeout limit for custom authorizer is 5 seconds."* Ours does a JWKS fetch plus a
    `tokensValidAfter` read. Warm that is nothing (`cmd/authorizer` already caches JWKS 24 h and
    users 60 s), but a COLD start that must also fetch JWKS is the case to measure — and there is
    no retry, a timeout is a failed connection.

  Also noted for M1.3: *"the next invocation can be delayed for up to 5 minutes on idle
  connections"*, so revocation latency on a backgrounded tab is up to 5 min **plus** the refresh
  interval — not the refresh interval alone.
  *DoD (still open, but now RUNNABLE):* the authorizer is deployed and ACTIVE
  (`arn:aws:iot:us-east-1:759775734231:authorizer/live-ninja-iot`), and a direct invoke of the
  function returns the correct deny shape (`{"isAuthenticated":false,"principalId":"deny",
  "policyDocuments":null}`) for a bad token. What remains is the browser leg: a tab subscribing to
  `liveninja/user/<uid>/#` and receiving a message published by `aws iot-data publish`. That needs
  a real signed-in session for the JWT, so it lands with WS-3 M3.3 rather than before it.
- `[!]` **M1.5 Token signing — WIRED BUT OFF, and it needs the owner.** The template carries the
  `IotAuthorizerSigningPublicKey` parameter and the `IotAuthorizerSigningEnabled` condition, so
  turning it on is a parameter change and nothing else. It ships **disabled** because enabling it
  needs an RSA keypair, and the private half is a secret this agent must never generate, see or
  commit (`scripts/set-secret.sh` is the only sanctioned path). **Unblocked by:** the owner
  generating a keypair, storing the private half via `set-secret.sh`, and passing the public half
  as the stack parameter; the token mint then returns `{token, tokenSignature}`.
  **Risk while off:** anyone who knows the endpoint can trigger the authorizer Lambda. AWS: *"if
  you leave signing enabled, you can prevent excessive triggering of your Lambda by unrecognized
  clients."* Bounded by the 5s timeout and the function's tiny cost; not a data-exposure risk,
  since an unsigned request still has to present a valid ES256 JWT to get any policy at all.
  *DoD (unchanged):* a connect attempt carrying a bad signature is rejected **without** the Lambda
  being invoked (no log line for it).
- `[~]` **M1.6 Cold-start budget — measured on the DEPLOYED function, comfortably inside.**
  2026-08-01, `live-ninja-iot-authorizer`, invoked with an unparseable token so the path ran
  JWKS-fetch-then-fail (the worst case: `Verify` fetches the JWKS document *before* it parses the
  JWT, so a cold deny pays the full network cost):

  | | Init | Duration | Total | Budget |
  |---|---|---|---|---|
  | cold | 71.03 ms | 619.86 ms | **690.9 ms** | 5000 ms |
  | warm | — | 1.14 ms | **1.1 ms** | 5000 ms |

  Max memory 30 MB. Cold is ~14% of the hard ceiling, so the JWKS fetch does NOT need to move out
  of the request path.
  **Honest gap:** that is ONE cold sample, not the ten the DoD asks for — forcing ten real cold
  starts means mutating the deployed function's config ten times, which is not worth it for a
  number already 7x inside budget. Left `[~]` rather than claimed as `[x]`.
- `[x]` **M1.2 `cmd/iot-authorizer`.** Extract the JWT/JWKS/`tokensValidAfter` verification shared
  with `cmd/authorizer` into `internal/auth`, and return an IoT policy scoped to
  `liveninja/user/<userId>/#` for Subscribe/Receive and `liveninja/user/<userId>/presence/<deviceId>`
  for Publish. Deny everything else.
  *DoD:* `go test ./internal/auth/... ./cmd/iot-authorizer/...` passes, including a case asserting
  that a token for user A yields a policy that does **not** match user B's topic.
- `[x]` **M1.3 Token lifetime.** `disconnectAfterInSeconds=3600`, `refreshAfterInSeconds=300`;
  `exp` checking untouched. Pinned by `TestConnectionBoundsAreSet`. ORIGINAL TEXT: The JWT lives 15 minutes; the connection must not. Set
  `disconnectAfterInSeconds` to 3600 and have the client reconnect when it refreshes its JWT.
  Do NOT weaken `exp` checking to keep a connection alive.
  *DoD:* a documented test showing a connection surviving a client token refresh via reconnect,
  and a revoked user (`tokensValidAfter` bumped) failing to reconnect.
- `[x]` **M1.4 Template.** `AWS::IoT::Authorizer` + the function + its log group at
  `RetentionInDays: 7`. No alarms, no dashboards.
  *DoD:* `sam validate --lint` passes and the deployed stack reaches `UPDATE_COMPLETE`.

**Landed 2026-08-01 (WS-1 build).** `internal/auth/tokenverify.go` now holds the whole "is this
token good right now" decision — JWKS fetch/cache, ES256 + iss/aud/exp, user lookup, active-account
check, `tokensValidAfter` kill-switch — and BOTH authorizers call it. `cmd/authorizer` was rewired
onto it and its 12 existing tests still pass unchanged, which is the evidence the extraction was
behaviour-preserving. The sharing is not tidiness: an MQTT connection is authorized once and then
held for up to an hour, so a kill-switch tightened on the HTTP side and missed on the IoT side
would leave a revoked user with a live subscription.

`cmd/iot-authorizer` returns a policy scoped to `liveninja/user/<uid>/`, built from the VERIFIED
token subject and never from anything the client asserted. Publish is granted on `presence/*`
**only** — doc/memory events are server-authored, and a client that could forge one could make
every other device of that user announce a change that never happened. The client id is
interpolated into a resource ARN, so it is character-allowlisted first; `TestDenials` covers a
wildcard and a quote-injection attempt.

One bug found and fixed during the extraction: `Verify` originally dropped `claims` on the
user-lookup path, which nil-dereferenced in the "no user record" log line. The fix was to the
contract, not the log — claims are now non-nil for every outcome where the JWT itself verified.

**Note for whoever reconciles retention:** the new log group is `RetentionInDays: 7` per the
standing rule, while every pre-existing group in this template is `5`. Both satisfy the rule's
purpose (never "Never expire"); the inconsistency is deliberate and flagged rather than silently
matched either way.

### WS-2 — Publish side `[ ]` (depends on WS-1 only for topic shape, can start in parallel)

- `[ ]` **M2.1 Topic namespace.** `liveninja/user/<userId>/doc`, `/memory`, `/presence/<deviceId>`.
  Guard: a Thing may never be named `user`, or it would collide with the existing
  `liveninja/<thingName>/telemetry` namespace.
  *DoD:* `go test ./internal/sync/ -run TestTopicNamespace` covers the collision guard.
- `[ ]` **M2.2 `Publisher.PublishEvent`.** Extend `internal/sync`'s existing publisher. Payload
  carries `{type, id, version, actorDeviceId, actorPersona, summary}` — `actorDeviceId` is what lets
  a client ignore its own edit.
  *DoD:* `go test ./internal/sync/` passes.
- `[ ]` **M2.3 Hook the writes.** Publish after `deliverable_create` / `file_create`,
  `memory_write` and `plan_upsert` commit. Publishing must never fail the tool call — log and
  continue.
  *DoD:* `go test ./internal/tools/` passes with a case asserting a publisher error does not
  surface as a tool error.

### WS-3 — Web client `[ ]` (depends on WS-1)

- `[ ]` **M3.1 Minimal MQTT 3.1.1 codec, `web/static/js/mqtt.mjs`.** CONNECT/CONNACK, SUBSCRIBE/
  SUBACK, PUBLISH (QoS 0 in), PINGREQ/PINGRESP, DISCONNECT. Hand-rolled deliberately: there is no
  bundler and the CSP forbids CDN scripts, so vendoring mqtt.js would mean checking a UMD bundle
  into `static/js` and stamping it through the import map.
  *DoD:* `node --test` unit tests over the packet encoder/decoder pass.
- `[ ]` **M3.2 CSP.** Add the account's IoT ATS `wss://` origin to `connect-src`
  (`internal/webapp/pages_routes.go:56`).
  *DoD:* `go test ./internal/webapp/ -run TestCSP` passes with a case pinning the IoT origin.
- `[ ]` **M3.3 Connect + LWT presence.** Subscribe on session start; set an MQTT Last Will on
  `presence/<deviceId>` so a dropped connection self-clears.
  *DoD:* two browser tabs — killing one removes its presence in the other within 30s.
- `[ ]` **M3.4 Auto-nudge.** On a `doc`/`memory` event whose `actorDeviceId` is not this device,
  inject through the existing `sendUserText()` path so the agent announces the change.
  **Suppress your own edits** and honour the WS-5 speaking lock.
  *DoD:* device A edits the doc; device B's agent speaks the change unprompted; device A stays
  silent.

### WS-4 — Android client `[ ]` (depends on WS-1)

- `[ ]` **M4.1 MQTT over OkHttp WebSocket.** Port the same minimal codec to Kotlin rather than
  adding `aws-crt-android`. Rationale: release builds are arm64-only and this device family has a
  live 16 KB page-alignment problem with existing native libs, so adding another native dependency
  compounds a known defect. **Fallback if the codec overruns two days:
  `aws-iot-device-sdk-java-v2`**, accepting the alignment risk and testing it explicitly.
  *DoD:* `./gradlew :app:testDebugUnitTest --tests '*MqttCodec*'` passes.
- `[ ]` **M4.2 Lifecycle.** Connect while the conversation screen is foregrounded or a session is
  live; disconnect otherwise. Do **not** hold a socket open in the background — Samsung's One UI
  will kill it and the reconnect churn is not worth the latency.
  *DoD:* verified on the Tab S9 FE (`R52XC06P9KJ`) via adb: connection present in the foreground,
  gone within 60s of backgrounding.
- `[ ]` **M4.3 Auto-nudge parity.** Same suppression and lock rules as M3.4.
  *DoD:* tablet and browser each nudge on the other's edit, neither on its own.

### WS-5 — Turn-taking rail `[ ]` (depends on WS-3 + WS-4) — the mitigation for locked decision 3

Auto-nudge means several agents can decide to speak at once, in one room, each hearing the others.
Per-device echo cancellation cannot help: AEC only cancels a device's own playout. This workstream
is what makes decision 3 survivable and is **not optional**.

- `[ ]` **M5.1 Presence registry.** Each client publishes `{deviceId, persona, state}` retained,
  with the LWT clearing it.
  *DoD:* three surfaces show a consistent roster within 5s of any change.
- `[ ]` **M5.2 Speaking lock.** Before an *unprompted* nudge a device claims
  `liveninja/user/<uid>/speaking`; others defer and merge the change into their next turn instead.
  Lock auto-expires after 30s so a crashed holder cannot mute the fleet.
  *DoD:* a scripted simultaneous edit produces exactly one speaking device.
- `[ ]` **M5.3 Distinct wake words per device.** Configuration only — `wakeWord` is already a
  per-device overridable section in `contracts/settings.schema.json`. Prevents one agent's reply
  waking another.
  *DoD:* documented in the Help drawer; each device set to a different phrase.

### WS-6 — Help copy `[ ]` (depends on WS-3)

- `[ ]` **M6.1** Per `CLAUDE.md`, any feature/setting/capability change updates the Help drawer in
  the SAME commit. Cover: shared project state across devices, what a nudge is and why an agent
  spoke unprompted, and how to run different personas per device.
  *DoD:* `go test ./internal/webapp/ -run TestHelpDrawer` passes.

---

### Sequencing

WS-1 M1.1 is the long pole and the gate — start it first and alone. WS-2 can proceed in parallel
once the topic namespace is fixed, because the publish side needs no client. WS-3 and WS-4 are
independent of each other. WS-5 needs both clients. WS-6 is last.

### Restart policy (per job)

| Job | Failure detected by | Restart | Ceiling |
|---|---|---|---|
| `sam deploy` via push to `main` | workflow conclusion != success | fix forward, push again | 5 pushes, then stop and report |
| Gradle `assembleDebug` / unit tests | non-zero exit | fix, re-run | 3, then `[!]` and move on |
| Playwright / adb verification | non-zero exit or missing marker | re-run once, then investigate | 2 |

Transient failures retry with backoff; a deterministic failure gets a fix first — re-running an
identical command that failed deterministically is a loop, not persistence.

### Stop conditions (only these)

1. **WS-1 M1.1 fails** — browser MQTT-over-WSS with custom auth proves impossible. Everything else
   depends on it, and the fallback (Cognito Identity Pool) is a different plan, not a workaround.
2. **The deploy pipeline fails 5 times on the same cause.**
3. **Any action would delete or reprovision an existing M5Stack Thing or certificate** — explicitly
   outside the standing authorization above.
4. **Credentials missing** for the deploy path.

Anything else — including an ugly workaround, a milestone that has to be marked `[!]`, or the
Android codec overrunning into its stated fallback — is worked around and reported at the end,
not paused on.

## Standing rules (carried forward — these do not expire)

- **Deploy = push to `main`.** Never deploy from a local machine. Watch the run to a terminal result.
- **No CloudWatch alarms or dashboards** (owner decision 2026-07-19). Every new log group gets
  `RetentionInDays: 7`.
- **Agents never see secret values.** Non-secret config goes in repo *variables*, not secrets.
- **No DynamoDB `Scan` on a serving path.** Lambdas are arm64.
- **A ghost-cli agent fix is not done until it is tagged, released and rolled to the node.**

## Gotchas that cost real time (don't re-learn these)

- **`TapToTalkConnectingStateTest` is flaky on the CI emulator (seen 2026-08-01).** It failed on
  `assertExists` for the "Connecting…" label, in a commit that touched only Settings/net files.
  Evidence it is environmental, not a regression: the same test passed on the previous CI run with
  identical `ConversationScreen` code, it passes on the physical Tab S9 FE, the failing run's log
  carries three `adb ... exit code 1` lines plus `Failed to start Emulator console for 5554`, and a
  re-run with **zero code changes** went green.
  **Unproven hypothesis, worth checking before trusting it again:** the eight instrumented tests
  share one process and one singleton `RealtimeSessionController`, and `startSession()` returns
  early *without* setting `CONNECTING` when `controller.connected.value` is already true — so a
  test that leaves a session connected would make this one fail, order-dependently. If it recurs,
  look there first rather than at whatever commit happened to trigger it.


- **The node's IoT Thing name is `OFFICEPC`, uppercase.** ghost-cli's node ACL compares exactly with
  no case folding, and the name is interpolated into `cockpit/nodes/<name>/cmd`. A lowercase value
  either reads as a permissions failure or publishes to a topic nobody subscribes to.
- **ghost-cli's preprocessor always appends its own output directive** to a rewrite. Appending blindly
  emits it twice and strands everything after it.
- **Truncate the prompt *body*, never the assembled string.** The operating rules sit at the end;
  truncating the whole thing eats the deploy gate first.
- **`docs/system-map.md` rides in every RCA prompt** under a character budget. Adding a subsystem
  means raising the budget deliberately (currently 9600, raised from 9200 on 2026-07-31 for the
  third cross-repo contract; the reasoning for each raise is recorded in `internal/rca/prompt.go`).
- **A read-only instruction conflicts with the mandatory output directive.** "Change nothing" plus
  "write `update-report.md`" means the report is never written and the summary email arrives empty.
- **"Every button on the page is dead" is a module-linking failure, not a UI bug.** One bad import
  kills the entire module, so plain `<a>` links and CSS scrolling keep working while nothing
  scripted does. Read the console before suspecting whatever changed most recently — see §4.3.
- **`adb shell input tap` does NOT activate page elements in Samsung Internet** (measured
  2026-08-01: four attempts, no effect, on both a new button and the year-old settings tab). Use a
  1px short press — `input swipe X Y X+1 Y 130`. `input swipe` for scrolling works either way. Four
  screenshot rounds were spent on this before the real console was read.
- **Read the tablet's real console instead of inferring from screenshots.**
  `adb forward tcp:9333 localabstract:Terrace_devtools_remote` (Samsung Internet; Chrome uses
  `@chrome_devtools_remote`), `curl http://127.0.0.1:9333/json/list` for the page's
  `webSocketDebuggerUrl`, then CDP `Runtime.enable` + `Log.enable` + `Runtime.evaluate` over it with
  the `ws` package already in `tests/web/node_modules`. This gave §4.3's cause in one line.
- **`web-quality` failing does not fail the deploy run** (`continue-on-error: true`). Check whether
  it was green on the previous runs before writing it off as flake — that is how §4.2 was caught.
