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

## Where things actually stand (2026-07-31, second pass)

Voice-driven code updates are **live and being used** — four launches through the full path today,
all with the deploy gate closed, DLQ empty. All three email legs (started / progress / completion
summary) are confirmed end to end.

Two production defects were found by *running it*, not by reviewing it. Both are now **fixed in
code and deployed**: the prompt transport (ghost-cli v1.1.52) and the Opus status vocabulary
(live-ninja `744d930`). §2.1's persistence question was decided by the owner and shipped
(`658f112`).

**The one thing left on §1 is the live proof, and it is blocked — see §1.4.** The node is running
agent `1.1.51-dev`, which predates the prompt-transport fix. That did not matter while the rewrite
was being discarded, because the launched prompt was always the owner's short wording. Now that the
rewrite is actually collected, a ~3400-character brief reaches the node for the first time — and on
`1.1.51-dev` it is typed as 3400 keystrokes, which is exactly the defect v1.1.52 exists to fix.
Verifying before the node is rolled would burn a session to re-prove a known bug.

---

## §1 — Opus pre-processing remediation `[~]` — code shipped `744d930`, live proof blocked

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

### 1.4 Verify in production `[!]` — blocked on rolling the node agent

- `[!]` **Roll OFFICEPC to ghost-cli ≥ v1.1.52 first.** The node reports `1.1.51-dev`
  (`GET /nodes`, 2026-07-31 15:48Z). `stageLaunchPrompt` does not exist at `v1.1.51` and does at
  `v1.1.52` — verified with `git grep` at both tags. Releases stamp the version via
  `-ldflags -X main.Version` (`agent-release.yml:149`), so a `-dev` suffix means a local build, not
  a release. Tags exist up to `v1.1.53`. Until this is rolled, the first genuinely long prompt this
  system has ever produced gets typed in as 3400 keystrokes.
- `[ ]` **S** — Then one `preprocess:true` request end to end. Success = worker log
  `rewritten:true` within ~60 s (not ~240 s), and the launched prompt visibly the expanded brief.
- `[ ]` **S** — Then one real **spoken** run, which is the only path that still has never been
  exercised (the voice tool defaults `preprocess` to true, so this covers both at once).

Owner authorised the canary run on 2026-07-31 (`ftwr-codeagent-canary` is on the launch allowlist
and the node is `live`/`connected`); it is held only by the agent version above.

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
- `[ ]` **H** — **`stageLaunchPrompt` writes `0600`, which Windows ignores** — the staged prompt file
  lands `-rw-r--r--` and carries the run token. Needs a real ACL. (From v1.1.52's own report.)

### 2.3 Closed this pass — do not redo

- `[x]` **Prompt transport.** The node agent typed 3400 keystrokes into the TUI; the head was lost to
  volume and the verifier keyed on the destroyed head, so all four retries fired and the deploy gate
  never arrived. Fixed in **ghost-cli v1.1.52**: prompts over 300 runes are staged to a file and only
  a pointer is typed. Two wrong turns are recorded in the archived report — read it before touching
  that path again. **Fixed in the repo, NOT yet on the node** — see §1.4, and the standing rule that
  an agent fix is not done until it is tagged, released *and rolled*.
- `[x]` **The `live-ninja` grant is now deploy-owned.** It was hand-seeded in SSM, and *every*
  ghost-cli deploy overwrote `/ghost-cli/authz-allowlist` with an owner-only document, silently
  killing the feature. The two-entry document now lives in ghost-cli's `AUTHZ_ALLOWLIST_JSON` repo
  variable and survives deploys.

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
  means raising the budget deliberately (currently 9200).
- **A read-only instruction conflicts with the mandatory output directive.** "Change nothing" plus
  "write `update-report.md`" means the report is never written and the summary email arrives empty.
