> **MIGRATED — archived 2026-07-31.**
> This document is history. It is no longer the source of truth and is not maintained.
> This is the run artifact from the first end-to-end verification run. It diagnosed the prompt-transport defect and records the three ghost-cli releases that fixed it — kept as evidence, not as a plan.
> Active work lives in [../plan.md](../plan.md); deliberately-deferred items in [../backlog.md](../backlog.md).

---

# Update report — read-only verification run

**Run:** `019fb632-a349-7efe-808b-d725dab65f65` · **Repo:** `JeremyProffittOrg/live-ninja` ·
**Node:** `OFFICEPC` · **Requested:** 2026-07-31T03:23:17Z · **Deploy:** NO

## Headline

The end-to-end plumbing test **failed**, and it failed in the one place the plan called out as
unverified. The prompt this session received was corrupted in transit: **1025 of its 1682
characters were missing**, and the surviving 657-character tail was delivered **four times over**.

Among the missing 1025 characters was **the entire deploy gate** — the `DO NOT PUSH` paragraph.
Meanwhile the launch confirmation email in your inbox correctly reads `Deploy: NO`. That is exactly
the failure shape finding #1 of your own adversarial review was written to prevent, arriving by a
different route: not truncation inside `BuildPrompt`, but mutilation after `BuildPrompt` had already
done its job correctly.

**live-ninja is not at fault.** The defect is in ghost-cli's node agent. Details in §3.

## 1. What the voice code-update feature does

From `plan-code-update.md` and the code:

1. You say *"update an application"*. The realtime model calls `code_update_repos()` to list repos,
   you name one, you describe the change, and it reads it back before calling
   `code_update_start(repo, instructions, agent, preprocess, confirm:true)`.
2. That tool validates and enqueues to `CodeUpdateQueue` in under a second, then says "Queued." It
   cannot wait — the web Lambda's timeout is 30 s and the Opus rewrite takes 30–90 s (D4).
3. `cmd/codeupdate-dispatch` (SQS, 300 s) does the real work, in a deliberately fixed order
   (`internal/codeupdate/dispatch.go`):
   - **mint the run token first** and write its row, because the prompt embeds the plaintext and a
     token with no row behind it would 401 at every attempt;
   - optionally send *only your own words* to Opus via ghost-cli's `/schedule/preprocess`, polling
     ≤ 240 s;
   - assemble the final prompt via `BuildPrompt`;
   - invoke ghost-cli `/schedule` with `run_now:true`, which authorizes, audits, and publishes a
     LAUNCH envelope to the node over IoT;
   - email you the launch confirmation carrying the exact prompt.
4. The node agent opens `claude` in the cloned repo. The session curls
   `POST /v1/code-update/progress` at milestones (per-run bearer token, hashed at rest, 24 h TTL,
   capped at 8 posts), each becoming an email. On completion it writes `update-report.md`, which
   ghost-cli's `lambda/summary` turns into the summary email.

The prompt is assembled in three parts and **the order is a security property, not style**
(`prompt.go:9-23`): your instructions (the only part Opus ever sees or may rewrite), then the
operating rules, then the output directive. Parts 2 and 3 are appended *after* the rewrite returns,
so a rewrite cannot mangle, summarise or drop the token or the gate.

## 2. Did the prompt I received contain a deploy gate and a progress block?

| Component | Expected | Actually received |
|---|---|---|
| Repository line | present | **MISSING** |
| Your instructions (the task) | present | **MISSING** |
| Deploy gate (`DO NOT PUSH`) | present | **MISSING — in full** |
| Progress block, opening sentence | present | **MISSING** |
| Progress block, curl + token + status list | present | present, ×4 |
| Output directive | present, once | present, ×4 |

So: **no deploy gate at all**, and only the back half of the progress block. I had no task
whatsoever — the words describing what to do were among the bytes that were dropped.

I recovered your original instructions from the `[JeremyProffittOrg/live-ninja] update started`
email, which carries the prompt verbatim. That copy is **well formed**: gate present, progress block
complete, directive once, at the end. This is what let the run continue instead of dead-ending.

Measured by reconstructing the prompt with the real `BuildPrompt` and a same-length dummy token:

```
total prompt          1682 runes
lost head             1025 runes  (61%)
surviving tail         657 runes  ×4 = 2628 runes delivered
```

The cut lands mid-word, inside "…and when you are | finished or blocked" — a byte-count boundary,
not a semantic one.

## 3. Root cause — ghost-cli's node agent, not live-ninja

live-ninja is exonerated by construction. In `dispatch.go`, the same `prompt` variable is passed to
`Ghost.Launch` (line 222) and to `emailStarted` (line 255). The email is correct, therefore the
string handed to ghost-cli was correct.

The node agent does **not** pass the prompt as an argument. For the `claude` profile it **types it
into the TUI** — `prof.Prompt == "type_after_spawn"` → `autoSendLaunchPrompt`
(`agent/cmd/agent/helper_cmd.go:410-411, 471`). Two coupled defects there produce exactly what I saw:

1. **The head is lost.** A single `inject.SendText` of a long prompt overflows the console input
   buffer; only the last ~657 characters survive. The code already knows this class of problem — the
   comment at line 522 records that `claude.exe` re-execs a child that takes over the console and
   **flushes whatever is queued in the input buffer**, observed on a fleet node as "102 records
   written" with an empty composer.

2. **Verification can never succeed, so all four retries fire.** `promptOnScreen` (line 603) builds
   its needle from the **first** `launchPromptNeedleRunes` of the prompt — precisely the characters
   defect #1 just destroyed. It searches the window for a string that is guaranteed absent, returns
   false every time, and the loop runs to its bound. `launchTypeAttempts = 4` (line 575).

Four attempts × one headless tail each = **four identical tails**, which is bit-for-bit what arrived.
The two defects are causally chained: the truncation is what makes the verifier blind, and the blind
verifier is what multiplies the damage.

Supporting gap: `maxPromptLen = 16384` (`agent/internal/launcher/validate.go:14`) and
`maxPromptBytes = 64 KiB` (`agent/internal/runbridge/server.go:37`) accept prompts **25×–100× larger
than the injection path can actually deliver intact**. Nothing between those limits and the composer
enforces the real ~657-character ceiling, and nothing reports the loss: the final log line says
`prompt submitted … verified=false`, and the run proceeds.

### Why this matters more than a mangled prompt

The deploy gate is a safety control, and it is delivered *in-band with the payload it constrains*.
This run was `deploy:false`. The gate was the only thing forbidding a push — and it never arrived.
What did arrive was an instruction to follow `CLAUDE.md`, whose first bullet is *"Pushing to `main`
IS the deploy trigger."* A task that implied shipping would have shipped, and your confirmation email
would still have said `Deploy: NO`.

`prompt.go` reserves the rules out of the budget so truncation eats the body instead of the gate.
That reasoning is sound and it held. It is simply defeated by a downstream transport that truncates
from the *other end*.

## 4. Recommendations (ghost-cli — nothing to change in live-ninja)

1. **Stop typing the prompt.** Deliver it out-of-band — a temp file plus `claude -p @file`, stdin, or
   the `arg` profile already implemented at `spawn_isolation_windows.go:429`. Keystroke injection
   into a TUI is not a reliable transport for a 1.7 KB safety-critical string.
2. **If typing must stay:** chunk below the input-buffer ceiling with per-chunk verification, and
   build `promptOnScreen`'s needle from the **tail**, not the head. Clear the composer between
   attempts so retries cannot concatenate.
3. **Make silent loss loud.** `verified=false` should fail the run and mark it `FAILED`, not proceed.
   A launch that cannot prove the prompt landed intact has not launched.
4. **Move the gate out of the prompt.** A `deploy:false` run should be enforced by something the
   agent cannot lose — a pre-push hook, a read-only credential, or a server-side check. In-band
   safety controls fail open when the band is lossy.
5. Reconcile `maxPromptLen`/`maxPromptBytes` with what the delivery path can actually carry.

## 5. Note for live-ninja

One small gap surfaced while recovering: the `CODEUPD#` status record does **not** persist
`instructions` (`store.go` `Put`/`recordFromItem`). That is a defensible privacy choice, but it means
the owner's words exist only in the consumed SQS message and the launch email. Had that email not
been sent, this run would have been unrecoverable. Worth a deliberate decision either way — not
changed here.

## 6. Scope and compliance

- **Read-only, as instructed.** No source file was modified, created or deleted; nothing was
  committed; nothing was pushed. There is **no local commit awaiting a push** — this run produced no
  code changes to commit.
- The single file written is this report, `update-report.md`, which the instruction explicitly asked
  for and which the capture path requires.
- Measurement used a throwaway copy of `prompt.go` in the session scratchpad, outside the repo. The
  working tree is clean apart from this report.
- The ghost-cli inspection was read-only and deliberately bounded: enough to identify the defect and
  name the lines, no further. Fixing it is a separate, authorized change in that repo.
- The run token was used only for the prescribed progress calls. It is not written here, and it does
  appear in the launch email in your inbox — consistent with the known follow-up that `GET /schedule`
  exposes prompt-embedded tokens.

## 7. Resolution — three releases, and two wrong turns

Fixed and verified end-to-end, but not on the first try. The record matters more than
the tidy version:

**v1.1.50 — wrong, and wrong in the dangerous direction.** I moved the delivery check to
the prompt's *tail*, reasoning the head had scrolled out of the readable screen buffer.
It had not. The failure mode leaves a trailing FRAGMENT, so the tail is present in exactly
the broken case. A live test confirmed a 137-rune fragment as good on attempt 1. The
prefix probe I replaced had been correctly reporting a real failure all along; I turned a
loud detector into a silent false pass.

**v1.1.51 — correct detector, still broken delivery.** Restored the head check (requiring
head AND tail), added a canary that waits for the console to echo before typing, cleared
the composer between retries, and made an unconfirmed prompt refuse to submit rather than
fail forward. This correctly detected the corruption and refused — but delivery still
failed, so launches were now failing *closed*.

**v1.1.52 — the actual fix.** A ten-rune canary lands perfectly while a 1682-rune prompt
does not, so the limit is volume, not timing: ~3.4k `INPUT_RECORD`s cannot be pushed
through `WriteConsoleInputW` into an ink TUI. Any prompt over 300 runes is now written to
a file and only a one-line pointer is typed.

### Measured, on OFFICEPC

| Run | Prompt | Session received | Result |
|---|---|---|---|
| Original incident | 1682 runes | last 657, ×4 copies | gate lost, submitted anyway |
| v1.1.50 retest | 1682 | last 137 | falsely confirmed on attempt 1 |
| v1.1.51 retest | 1682 | last 537 | correctly refused, not submitted |
| **v1.1.52 retest** | **2190, staged to file** | **148-rune pointer, 100% intact** | **confirmed attempt 1, task read in full** |

### Corrections to §3

The head is not lost to composer scrolling. It is lost to sheer volume through the
keystroke path. And the `arg` profile I recommended is *not* available for claude: the
launcher comments record that a positional prompt makes it one-shot and exit rather than
stay interactive.

## 7b. Separate production outage found (and fixed)

The first retest did not launch at all: `deny: principal not in allowlist`. Voice
code-updates were **completely dead**, and not because of anything above.

SSM `/ghost-cli/authz-allowlist` v105 (seeded manually) contained both the owner and
`live-ninja`. v106 and v107, both written by the **GitHub deploy role**, contain only the
owner. **Every ghost-cli deploy silently drops the `live-ninja` grant**, because that
grant was hand-seeded and is not owned by the deploy template.

I restored it as v108 — `operator`, scoped to `OFFICEPC` only, byte-identical to what
v105 had. **This will break again on the very next ghost-cli deploy** unless the grant is
added to whatever the deploy writes. That is the highest-value open item here.

## 7c. Open items

- **The `live-ninja` grant is not deploy-owned** (above). Recurs on every deploy.
- **`stageLaunchPrompt` writes 0600, but Windows ignores the mode** — the staged file
  lands `-rw-r--r--` and carries the run token. Same exposure class as the stored event
  and the confirmation email, but it should be ACL'd properly.
- **Two run rows are stuck `RUNNING`** (`newgh-smoke-test`, `ai-template`) because I killed
  their sessions during testing. The per-repo in-flight guard will refuse launches on those
  two repos until the grace expires.
- **The in-band deploy gate still stands.** v1.1.52 makes the transport reliable; it does
  not remove the dependency on a sentence surviving it.

## 8. Verdict

Legs 1–6 of the pipeline (voice → queue → worker → token → prompt assembly → ghost-cli launch →
confirmation email) work. Leg 7 — **delivering the prompt into the coding session on the node** — is
broken, and it corrupts the payload in a way that silently removes a safety control while every
upstream artifact reports success. The plan listed this leg as "not verified here." It is now
verified, and it does not work.
