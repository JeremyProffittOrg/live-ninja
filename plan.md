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

- `[ ]` **Windows1 is CRASHED and has been reporting it.** Found 2026-08-01 while measuring the
  fleet for the item above, not by looking for it. Its retained status on
  `cockpit/nodes/Windows1/status` is fresh (published 12:50, same minute as the healthy nodes, so
  the agent process is alive and talking) but carries `state=CRASHED` at `agent_version=1.1.49`.
  A crashed agent will not self-update, which is why it alone is stuck below 1.1.53 while every
  other LIVE node is at 1.1.54+. Nothing alerts on this: it is visible only to whoever runs
  `ghost node list` or reads the retained topic. **This is a prerequisite for the `no_push` item
  above** — it is the only live node blocking convergence. Diagnose the crash before restarting,
  or the restart just resets the clock on whatever caused it.

- `[ ]` **The self-update path is lagging on healthy nodes.** `releases/latest.json` is 1.1.56 and
  has been long enough for every node to have polled (~4 h interval), yet OFFICEPC — live, healthy,
  used all day — is on 1.1.54, and Windows2/Right-Board are on 1.1.55. No node is on the published
  fleet version. That points at the updater rather than at the machines, and it is the reason
  "wait for the fleet to converge" is not a plan by itself. Worth a look at whether the poll is
  running, whether it is failing silently, and whether anything would ever say so.

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

## Standing rules (carried forward — these do not expire)

- **Deploy = push to `main`.** Never deploy from a local machine. Watch the run to a terminal result.
- **No CloudWatch alarms or dashboards** (owner decision 2026-07-19). Every new log group gets
  `RetentionInDays: 7`.
- **Agents never see secret values.** Non-secret config goes in repo *variables*, not secrets.
- **No DynamoDB `Scan` on a serving path.** Lambdas are arm64.
- **A ghost-cli agent fix is not done until it is tagged, released and rolled to the node.**

## Gotchas that cost real time (don't re-learn these)

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
