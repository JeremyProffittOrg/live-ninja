> **MIGRATED — 2026-07-24.** This plan is archived. Its unfinished items were consolidated into
> [/plan.md](../plan.md) and any deliberately-deferred items into [/backlog.md](../backlog.md).
> Kept here as a historical record — do not edit; track live work in /plan.md.

# Live Ninja — Base Knowledge Layer + Tool-Failure Agentic RCA

> **Status:** authoring / awaiting owner go · **Owner:** jeremy · 2026-07-19
> Extends plan.md with three new milestones (M15–M17). Written after a code review of
> the current tool/memory/mint pipeline; every finding below cites the real seam.

## 0. The problems, grounded in code

**P1 — The assistant has no baseline context.** Session instructions are assembled in
`internal/realtime/mint.go` as `persona.Instructions + memoryUsageDirective +
instructionsSuffix`. Nothing tells the model who it's talking to, where they are, what
time it is locally, or what units they use. Result: "what's the weather" forces the
model to ask (or guess) a location every time, even though the home address sits in the
memory layer.

**P2 — Memory ≠ profile.** `memory_search` is retrieval-on-demand: the model must
*think* to search, per-session, and personal facts come back as prose. Stable,
always-relevant facts (home location, timezone, units, name) should not depend on the
model remembering to search — they belong in every session's instructions, structured.

**P3 — The weather tool is quirky by construction** (`internal/tools/weather.go`):
- `location` is **required** — no default, so the no-context model must supply one.
- Geocoding sends the raw string to Open-Meteo's name search. That API matches bare
  names ("Charlotte") and postal codes ("28078") but routinely returns **zero results
  for "City, ST" compounds** ("Huntersville, NC") — exactly the shape a US-centric
  model produces. Hence the owner's observation: *city fails, zip works*.
- `count=1` takes the first global match — "Paris" → France even if the user is in
  Texas; no profile-based disambiguation.
- Units default is hardcoded imperial rather than a per-user preference.

**P4 — Tool failures die silently from the owner's perspective.**
`internal/tools/registry.go` has a single egress (`finish`) that stamps
`outcome ∈ {ok, duplicate, error}`, writes an audit `LOG#` row (tool, callId, args,
error, 90-day TTL) and an EMF metric. Failures are *recorded* but nobody analyzes them;
the owner discovers breakage only by noticing odd assistant behavior live.

**Existing assets to build on:** SES email path (`cmd/email-dispatch`, SQS-fed,
`jeremy@jeremy.ninja` sender); Bedrock SDK usage + IAM patterns
(`internal/memory/embedder.go`, Titan embeddings); versioned per-user settings document
with JSON-schema validation + cross-surface sync (the natural home for a profile);
transcript store partitions holding the full conversation around any failure.

---

## M15 — Base Knowledge Layer  `[ ]`

**Definition of Done:** every minted session (web, Android, Tab5, fallback turns)
carries a server-built BASE KNOWLEDGE block — identity, home location, local date/time,
timezone, units, contact email; the weather tool works with **no location argument**
(profile default, straight to coordinates — no geocode leg at all) and correctly
resolves "City, ST" when a location *is* given; profile is owner-editable in the web
Settings drawer and versioned like the rest of the settings doc.

Ordered tasks:
- `[ ]` **O** — **Profile schema** (`contracts/settings.schema.json` + `internal/store`):
  new `profile` section of the settings document (rides the existing optimistic-
  concurrency version + cross-tab sync for free):
  `displayName`, `pronouns?`, `homeLocation {label, postalCode?, city?, admin1?,
  country?, lat, lon, timezone}`, `workLocation?` (same shape), `units
  (imperial|metric)`, `locale?`, `contactEmail`, `quietHours?`, `notes[]` (short
  free-form owner facts, each ≤200 chars, capped ~20 — the "everything else" bucket).
  Locations are stored **geocode-verified** (lat/lon resolved at save time, never at
  question time).
- `[ ]` **S** — **Server-side directive builder** (`internal/realtime/baseknowledge.go`):
  `BuildBaseKnowledge(profile, now)` → a compact block appended in the mint chain
  (after `memoryUsageDirective`, before the guide suffix — same anti-injection posture:
  server-resolved, never client-supplied text). Includes **current local date + time**
  computed from the profile timezone at mint (the model currently has no clock at all —
  this fixes "what time is it"/"today"-class errors too). Also injected into
  fallback-turn completions (same builder, `realtime-broker` fallback path).
- `[ ]` **S** — **Tool-context defaults** (`internal/tools`): `Deps` gains the resolved
  profile. `get_weather.location` becomes **optional** → defaults to profile
  coordinates (skips the geocode leg entirely — faster and un-breakable);
  `units` defaults from profile; scheduler/timer tools default to profile timezone.
- `[ ]` **S** — **Weather geocoding hardening** (fixes the named quirk even when a
  location IS passed): split "City, admin" on comma → `name=City`, `count=5`, then
  rank candidates by admin1/country match against the remainder and by proximity to
  the profile home (kills "Paris, TX → France"); detect bare postal codes and pass
  through unchanged (already works — keep it). Table-driven tests for the shapes that
  fail today: "Huntersville, NC", "Paris, TX", "Charlotte", "28078".
- `[ ]` **S** — **Settings UI: "About you" section** in the conversation drawer
  (`conversation.html` + `settings.mjs`): name, pronouns (optional), home/work location
  (typeahead against the Open-Meteo geocoder — selection only, per house UI rules;
  ZIP accepted; saves the resolved lat/lon+timezone, shows the resolved label back),
  units toggle, notes list. Server PUT validates via the schema.
- `[ ]` **S** — **Bootstrap from memory:** one-time assisted seed — a small admin
  action ("Suggest my profile") that runs `memory_search` for home/work/name facts and
  pre-fills the form for the owner to confirm (never silently copies memory → profile).
- `[ ]` **H** — contracts/api.md + settings.schema.json docs; plan.md notes.

**Explicitly not in M15:** putting the profile on-device for the Tab5 (it mints through
the same broker — the directive arrives server-side; nothing device-side changes).

## M16 — Knowledge Refinement Loop  `[ ]`

**Definition of Done:** the assistant (and the RCA pipeline in M17) can *propose*
base-knowledge additions; proposals queue as pending suggestions the owner
approves/rejects in Settings; approved ones merge into the profile with full version
history; nothing ever auto-writes identity/location facts without confirmation.

Ordered tasks:
- `[ ]` **S** — `profile_suggest` tool (assistant-callable, session manifest): proposes
  a field change or a new `notes[]` fact with a reason. Writes a `PROFSUGG#` item
  (pending, TTL 30 days), **never** mutates the profile directly. The tool result tells
  the model "suggested — Jeremy will confirm in Settings."
- `[ ]` **S** — Suggestions UI in the "About you" section: pending list with
  Approve/Reject (approve = normal versioned settings PUT). Badge on the drawer tab
  when suggestions are pending.
- `[ ]` **O** — **Policy** (locked now, revisit later): auto-apply is allowed only for
  `units` and `notes[]` additions the owner spoke *explicitly* ("remember that I
  prefer metric") — and even those surface a toast + undo. Location/name/email always
  require Settings confirmation. Rationale: a mis-set home location silently poisons
  every weather/time answer.
- `[ ]` **H** — memoryUsageDirective updated so the model knows the split: *stable
  facts → profile (visible in your instructions); episodic facts → memory tools*.

## M17 — Tool-Failure Agentic RCA (Bedrock Opus → email)  `[ ]`

**Definition of Done:** when a tool invocation ends `outcome=error` in prod, an
automated root-cause analysis runs within ~1 minute: it pulls the failing call + the
surrounding conversation window + the tool's contract, asks **Claude Opus on Bedrock**
for a structured RCA, emails the report to the owner, and files any base-knowledge /
code-fix suggestions into the M16 queue. Deduped, rate-capped, and off the request path.

Architecture (all pieces reuse existing patterns):

```
Registry.finish (outcome=error)          ← the single egress that already exists
  └→ SQS rca-queue (async, fire-and-forget — request path untouched)
       └→ rca-analyzer Lambda (arm64, isolated role)
            ├─ gather: failing Invocation (tool, args, error, txId)
            │          transcript window: LOG# rows ±10 turns around ts
            │          tool Definition (params/description) + relevant contract snippet
            │          last 5 RCA# items for this tool+errorCode (dedupe/context)
            │          the profile + a system-map cheat sheet (static, versioned in repo:
            │          how tools/mint/memory fit together — the "how this system works")
            ├─ analyze: Bedrock InvokeModel, Claude Opus inference profile,
            │          structured-output prompt → {symptom, evidence[], rootCause,
            │          confidence, fixes[] (code-level, file:line where inferable),
            │          baseKnowledgeSuggestions[], reproSteps}
            ├─ persist: RCA# item in DynamoDB (30-day TTL) — dedupe key
            │          tool+errorCode+rootCauseHash, cooldown 1/hour per key, 10/day cap
            ├─ email: SES (existing sender identity) → proffitt.jeremy@gmail.com,
            │          subject "Live Ninja RCA: <tool> — <symptom>", plain-text report
            └─ file:  baseKnowledgeSuggestions[] → M16 PROFSUGG# queue
```

Ordered tasks:
- `[ ]` **S** — SQS `live-ninja-rca` + enqueue in `Registry.finish` on
  `outcome=error` (include `CodeNotFound`/`CodeUpstreamError`/validation errors —
  malformed-args failures are *exactly* the prompt/schema bugs RCA should catch; skip
  `duplicate`). Non-blocking send, errors logged never raised. Template: queue, DLQ,
  Lambda, per-function role (`bedrock:InvokeModel` on the Opus inference profile ARN,
  `ses:SendEmail` scoped to the identity, Dynamo RW on `RCA#`/`PROFSUGG#`, transcript
  partition **read-only**).
- `[ ]` **O** — `rca-analyzer` Lambda: context gathering + the analysis prompt. The
  prompt embeds a repo-versioned `docs/system-map.md` (≤2K tokens: surfaces, mint
  chain, tool registry, memory layer, settings/profile — reviewed like code) so Opus
  reasons about *this* system, not a generic one. Token budget ≤8K in / ≤2K out.
- `[ ]` **S** — Report email formatting + `RCA#` persistence + dedupe/cooldown/cap
  logic (the caps are the cost story: worst case 10 Opus calls/day ≈ low single-digit
  dollars/month at realistic failure rates; normal case ≈ pennies).
- `[ ]` **H** — **Owner manual step:** enable Anthropic Claude Opus model access in
  Bedrock us-east-1 (same console flow as the Nova Sonic request). Fallback if Opus
  access is denied/slow: per the house model-routing rule this falls back **up** only —
  hold RCA disabled rather than silently shipping a weaker analyst; flag it.
- `[ ]` **S** — Tests: fake Bedrock + fake SES; dedupe window; cap; a golden RCA
  prompt snapshot test so context-gathering regressions are visible in review.
- `[ ]` **F** — Client-side failures (phase 2, after server RCA proves out): the web
  `toolerror` path and Tab5 `ln_rt` fatal errors POST a lightweight
  `/api/v1/rca/client-event` breadcrumb onto the same queue — captures failures that
  never reach the tool router (e.g. this afternoon's device-side saga).

## Sequencing, cost, risk

- **Order: M15 → M17 → M16-polish.** M15 kills the daily annoyances immediately
  (weather, location, clock) and M17 needs M15's profile + system map for good RCAs.
  Estimated effort: M15 one focused session; M17 one session; M16 rides along.
- **Standing cost:** ~zero (SQS/Lambda/Dynamo well inside free tier). RCA Opus usage is
  hard-capped; no secrets manager, no new always-on infra. All new stack resources get
  the six mandatory cost tags automatically (stack-level tags).
- **Risks:** RCA email noise → cooldown+dedupe+daily cap from day one, and the report
  includes "suppressed N similar" counts; prompt injection via transcript content into
  the RCA model → RCA output is email + suggestions queue only, executes nothing;
  wrong auto-profile edits → M16 confirmation policy above.

## Open questions for the owner (answer before build; defaults baked in)

1. RCA email recipient stays `proffitt.jeremy@gmail.com`? *(default: yes)*
2. RCA daily cap 10 / cooldown 1 h per failure signature OK? *(default: yes)*
3. Should validation-only errors (model sent malformed args, no user impact beyond a
   retry) email too, or just persist + weekly digest? *(default: email — they're the
   best early signal of prompt/schema drift)*
4. Home address: seed the profile from the existing memory entity automatically on
   first deploy (one-time, with a confirmation email), or wait for you to fill the
   Settings form? *(default: pre-fill pending suggestion, you approve in Settings)*
5. Opus specifically, or "best available Anthropic model on Bedrock at build time"?
   *(default: Opus per your ask; fallback is hold-disabled, never downgrade)*
