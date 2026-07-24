> **MIGRATED — 2026-07-24.** This plan is archived. Its unfinished items were consolidated into
> [/plan.md](../plan.md) and any deliberately-deferred items into [/backlog.md](../backlog.md).
> Kept here as a historical record — do not edit; track live work in /plan.md.

# Live Ninja — Tool Manifest Parity & Correctness

> **Status:** COMPLETE — M18 `[x]`, M19 `[x]`, and M20 `[x]` all complete 2026-07-20 (see
> notes, incl. the recorded A2 deviations). The A2 deploy constraint ("do not push/deploy
> until M19 (B3) lands") is **satisfied**: B3 landed, so M18+M19 ship in the same push as
> required. Only the post-M19 manual live-audio smoke (Verification section) remains, and
> it is owner-only/post-deploy by design. · **Owner:** jeremy · 2026-07-20
> Extends plan.md with three milestones (M18–M20). Written after the voice tool-surface
> audit of 2026-07-20; every finding below cites the real seam.
>
> **Explicitly out of scope (owner decision, 2026-07-20):** Nova Sonic's empty tool/persona
> config, and Tab5 firmware tool execution. Both are real defects — see
> `docs/` audit and `gemini-plan.md` §Phase-D — but neither is addressed here.

---

## 0. The problems, grounded in code

**P1 — Two hand-maintained tool catalogs, no test between them.**
`internal/tools/registry.go:347` `Manifest()` renders the catalog from the real
`Definition`s and its doc comment claims it is "the `tools` array the broker binds into
every session config, and the exact schema Invoke later enforces." **That claim is false
today.** Nothing in production calls it — it is referenced only from `registry_test.go`,
`file_test.go`, `memory_test.go`. What every engine actually binds is the hand-written
literal `var toolManifest` at `internal/realtime/mint.go:117-500`, marshaled once at init
into `toolManifestJSON` (mint.go:503) and consumed by `mint.go:708`,
`gemini_mint.go:261`, and `cmd/realtime-broker/main.go:448` via `ToolManifestJSON()`.
The two catalogs have drifted. Tracked as item **F2 "Manifest de-drift"** in
`docs/agentic-expansion-suggestions.md:181`.

**P2 — The drift causes hard, unpredictable failures.** Tool *names* match 20/20 in both
directions — the damage is entirely in parameters:

| Tool | Advertised (mint.go) | Enforced (internal/tools) | Effect |
|---|---|---|---|
| `set_timer` | `seconds` only, **required**, 1–86400 | `inSeconds` (primary) \| `seconds` (alias), neither required, 5–31 622 400 | `seconds:1` is hard-rejected; the range above 86400 is unreachable |
| `set_reminder` | `at`+`message` both required | `message` required; `at` **xor** `inSeconds` | relative-offset reminders unreachable |
| `device_control` | `action`: unconstrained string, prose names 6 | `action`: 9-value enum | `ping`/`identify`/`reboot` never surfaced |
| `recall_note` | `query` only, **required** | `query` (optional), `tag`, `limit` | tag-filter and "list recent notes" unreachable |
| `get_weather` | `location`, `days` | + `units` (imperial\|metric) | metric output unrequestable; desc says "default 1", code defaults 3 |
| `remember_note` | no length caps | `text` MaxLen 2000 | occasional unpredictable `invalid_args` |

Plus **14 tools with hand-reworded descriptions** not byte-identical to their registry
`Description`, and ~12 length caps the router enforces but the manifest never advertises
(`send_email.subject` 200, `send_email.body` 10000, `web_lookup.query` 200,
`web_research.query` 300 / `url` 500, `memory_search.query` 300, `memory_write.name` 200,
`entity_get`/`forget`/`plan_upsert` id caps 100, `deliverable_create.name` 100 /
`content` 100000, `deliverable_zip.name` 100, `deliverable_deliver.deliverableId` 64).
Each is an `invalid_args` the model has no way to predict.

> **Historical note that matters:** `scheduler.go:38-41` records that `seconds` exists
> *only* as a compatibility alias added because prod models kept sending it and **every
> `set_timer` call failed `invalid_args`**. This exact drift has already caused a
> production outage of one tool. It is not theoretical.

**P3 — Six tools are never described to the model.** `internal/realtime/personas.go:55`
`coreInstructions` names 14 tools verbatim. `deliverable_create`, `deliverable_zip`,
`deliverable_deliver`, `file_list`, `file_read`, `file_create` are in every engine's
manifest but appear in no persona prompt, so the model reaches for them rarely.

**P4 — Gemini schema translation is unsanitized.** `gemini_mint.go:120`
`geminiToolDeclarations()` forwards each tool's `parameters` map **by reference**,
stripping only the `"type":"function"` discriminator. JSON-Schema keywords `genai.Schema`
does not model (`minLength`, `maxLength`, `pattern`) pass through verbatim. The SDK-typed
constraints copy (`sdkFunctionDeclarations()`, gemini_mint.go:191) round-trips through
`json.Marshal`→`Unmarshal` into `[]*genai.FunctionDeclaration` and silently drops them,
while the raw wire `setup` frame (`buildGeminiSetup`, gemini_mint.go:139) still carries
them. So the minted token's constraints and the client-sent setup frame can legitimately
differ in schema detail for the same session. Nothing tests for it. Latent, not yet
observed in the wild.

**Existing assets to build on:** `Registry.Manifest()` already emits the **byte-identical
wire shape** to the literal (`{type,name,description,parameters}`, registry.go:364-369) —
this is a drop-in, not a rewrite. Definition constructors (`setTimerDefinition()` etc.)
take **no dependencies**, so they can be rendered without a live `Deps`. Neither package
imports the other, so there is **no import cycle** in either direction. Two real parity
tests already exist for the derived paths (`fallback_tools_test.go:19`,
`gemini_mint_test.go:78`) and will keep the Gemini and fallback paths honest for free.

---

## Workstreams

| WS | Scope | Model routing | Depends on |
|---|---|---|---|
| **WS-A** | Registry content: descriptions, bounds, param surface | Sonnet (mechanical edits against a fixed spec) | owner answers Q1–Q3 |
| **WS-B** | Structural: extract `definitions()`, package-level `CatalogManifest()`, flip the binding | Fable (cross-package wiring, init-order risk) | WS-A complete |
| **WS-C** | The missing parity test + regression suite | Fable (this test is the whole point — it must not be trivially satisfiable) | WS-B in progress |
| **WS-D** | Gemini sanitizer + `coreInstructions` copy | Sonnet (sanitizer), Haiku (persona copy) | independent — runs parallel to WS-A/B |

WS-D is fully independent and should be launched **concurrently** with WS-A.

---

## M18 — Registry becomes prompt-ready  `[x]`

**Why this comes first:** M19 flips every engine onto the registry's descriptions. If the
registry's wording is worse than the hand-tuned manifest copy, flipping ships a
prompt-quality regression that lasts until someone fixes it. Doing content first makes the
flip a pure improvement with **no regression window**. Nothing binds the registry manifest
yet, so every task here is behaviour-neutral in production and safe to land incrementally.

**Definition of Done:** every `Definition` in `internal/tools/` carries the wording and
constraints we actually want the model to see; `go test ./...` green; **no production
behaviour has changed yet** (nothing binds `Manifest()` until M19).

Ordered tasks:

- `[x]` **A1 — Description reconciliation** (WS-A). For each of the 20 tools, compare the
  `mint.go` manifest description against the registry `Description` and land the better
  wording in the registry. The manifest copy is voice-tuned and often terser; the registry
  copy is more precise about failure modes. Per **Q1**, keep whichever is stronger. Record
  the chosen wording per tool in the notes block below so the diff is reviewable.
- `[x]` **A2 — `set_timer` param surface** (WS-A). Per **Q2**: drop `seconds` from the
  advertised `Params` while `resolveFireTime` (scheduler.go) keeps accepting **both**
  spellings — that alias is load-bearing prod-compat and removing it re-breaks the tool.
  Per **Q3**: set `inSeconds` `Max` to 86400. Per **D-a**: the `Description` must tell the
  model to use `set_reminder` beyond 24h, and the out-of-range `invalid_args` message must
  name `set_reminder` so the model can self-correct. Add a test asserting `inSeconds` and
  `seconds` resolve to an identical fire time.
- `[x]` **A3 — `get_weather` description fix** (WS-A). `days` description currently says
  "(default 1)" in the manifest and "(default 3)" in the registry; the code defaults to 3.
  Make the registry say 3. Confirm `units` enum + default are described.
- `[x]` **A4 — Audit every `ParamSpec` for advertise-ability** (WS-A). Walk all 20 tools ×
  all params: any constraint the router enforces (`MinLen`/`MaxLen`/`Min`/`Max`/`Enum`/
  `SafeName`) must be present on the `ParamSpec` so `jsonSchema()` renders it. This is
  mostly verification — `jsonSchema()` (registry.go:374) already renders all six — but
  confirm no constraint lives only in handler code where the schema can't see it.
  Known handler-only constraints to either promote or document as deliberate:
  `remember_note.tags` (≤10, each ≤50), `memory_write.attrs`/`relations` (≤20 each),
  `plan_upsert.steps` (≤30, each ≤500), `deliverable_zip.deliverableIds` (1..MaxZipSources).
  These are array-element rules `jsonSchema()` cannot express today — decide per tool
  whether to fold them into the description prose instead.
- `[x]` **A5 — Byte-length caveat** (WS-A). `MinLen`/`MaxLen` are measured in **bytes**
  (`len(s)`), not runes, so multi-byte content hits the cap earlier than a user expects.
  Either switch to `utf8.RuneCountInString` or state the unit in the rendered description.
  Prefer the former; it is a two-line change in `validateArgs` and strictly less surprising.

**Implementation notes (append as work proceeds):**

**A1 — per-tool description reconciliation record (Q1: stronger wording wins).**
"Registry kept" = the registry `Description` was judged stronger (or the two were
already equivalent) and the registry file is unchanged for that tool's top-level
description; "manifest ported" / "merged" = the registry was edited. Param-level
prose added under A4 is listed with A4 below, not here.

| # | Tool | Winner | Rationale |
|---|---|---|---|
| 1 | `send_email` | registry kept | Both state the confirmExternal contract; registry adds the owner-inbox default phrasing and matches the enforced surface. |
| 2 | `set_timer` | **merged (rewritten)** | Manifest's terser "one-shot timer that notifies the user when it fires" + new Q3 24h cap sentence + D-a `set_reminder` handoff sentence (scheduler.go:45). |
| 3 | `set_reminder` | registry kept | Manifest copy was the P2 drift itself (absolute `at` only, `at`+`message` both required); registry documents `at` xor `inSeconds` and email delivery. |
| 4 | `device_control` | **merged** | Manifest's "Send a control action to one of the user's own registered … devices (e.g. the M5Stack terminal)" + its ownership sentence, keeping the registry's full action list incl. ping/identify/reboot (devicecontrol.go:36). |
| 5 | `get_weather` | registry kept | Registry already says "(default 3)" — matches the code default; `units` enum + imperial default described. See A3. |
| 6 | `web_lookup` | **merged** | Manifest's "factual topic (encyclopedia-style summary)" framing + registry's "on Wikipedia … with a source link" (weblookup.go:25). |
| 7 | `deliverable_create` | registry kept | Registry already carries the already_exists / never-overwritten contract the manifest copy was cribbed from. |
| 8 | `deliverable_zip` | registry kept | Equivalent; registry adds background-build detail. (Param prose added under A4.) |
| 9 | `deliverable_deliver` | registry kept | Equivalent; registry names link-TTL and email-to-own-inbox behavior. |
| 10 | `file_list` | registry kept | Byte-identical intent — the manifest copy was authored from the registry (M9); no edit. |
| 11 | `file_read` | registry kept | Same as file_list: copies already matched; no edit. |
| 12 | `file_create` | registry kept | Same: already_exists / never-overwrite contract identical in both; no edit. |
| 13 | `remember_note` | registry kept | Manifest's one-liner ("Save a note for the user to recall later.") is strictly weaker. (Tags prose added under A4.) |
| 14 | `recall_note` | registry kept | Manifest copy was the P2 drift (required `query`, no `tag`/`limit`); registry describes optional query, tag filter, recent-notes listing. |
| 15 | `memory_search` | **manifest ported** | The manifest's "ALWAYS call this before answering any question about the user's personal facts … and before saying you don't know such a fact" directive is the stronger prompt; ported into memory.go:101. |
| 16 | `memory_write` | registry kept | Equivalent top-level wording; registry lists the six entity types in prose. (attrs/relations prose added under A4.) |
| 17 | `entity_get` | registry kept | Equivalent; registry names memory_search *and* memory_write as ID sources. |
| 18 | `plan_upsert` | **merged** | Registry wording + manifest's "The steps list replaces any previous steps — it is not appended to." appended (memory.go:162). |
| 19 | `forget` | registry kept | Both carry the explicit-request-only guard; registry adds the search-index-entry detail. |
| 20 | `web_research` | registry kept | Registry names the recency-directive default and the URL allow-list precisely. |

**A2 notes.** `scheduler.go`: added `maxTimerLead = 24h` and `timerOverflowHint`
(names `set_reminder (with inSeconds)`, per D-a); `set_timer.inSeconds` Max →
86400, `seconds` alias kept in `resolveFireTime` but marked `Unadvertised`.
`registry.go`: new `ParamSpec.Unadvertised` (skipped by `Manifest()` rendering,
still validated/coerced) and `ParamSpec.OutOfRangeHint` (appended to the
router's min/max `invalid_args` message). Tests: `scheduler_test.go` —
alias-equivalence (`TestResolveFireTimeSecondsAliasMatchesInSeconds`,
`TestSetTimerAcceptsUnadvertisedSecondsAlias`), Q2 manifest surface
(`TestSetTimerManifestAdvertisesOnlyInSeconds`), D-a hint
(`TestSetTimerOverflowNamesSetReminder`), and cap-scope
(`TestSetReminderStillAllowsBeyondTimerCap`).

> **A2 DEVIATION (recorded on review, 2026-07-20): the 24h cap enforces
> immediately, not at M19.** `ParamSpec.Max` is shared between advertisement
> and enforcement, so narrowing both spellings' Max to 86400 makes the cap
> live the moment this change deploys — before B3 flips the binding. Strictly,
> M18's "no production behaviour has changed yet" DoD and D-a's "A2 is
> behaviour-neutral until B3" premise do not hold for one sliver: an
> *off-schema* `set_timer` call with 86400 < value ≤ 31 622 400 previously
> succeeded and now returns `invalid_args`. Mitigations, why this is accepted:
> (1) the currently-bound mint.go literal already advertises `seconds` max
> 86400, so schema-compliant models never hit the window; (2) the error hint
> names `set_reminder (with inSeconds)` and the router *already accepts*
> `set_reminder.inSeconds` (registered, just unadvertised), so the handoff
> target is executable even pre-M19; (3) per repo policy nothing deploys until
> pushed. **Deploy constraint: do not push M18 standalone — land B3 (M19) in
> the same push so the advertised handoff target exists when the cap becomes
> user-visible.** *(SATISFIED 2026-07-20: B3 landed in the same working tree
> as M18 — both go out in one commit/push, so the constraint holds by
> construction.)*

> **A2 ADDENDUM (review fix, 2026-07-20): `set_reminder.seconds` is also
> `Unadvertised` now.** A2's letter scoped Q2 to `set_timer`, but the M19 flip
> advertises `set_reminder.inSeconds` for the first time (D-a) — leaving its
> `seconds` alias advertised would have taught the model the exact
> inSeconds/seconds synonym pair Q2 was locked to eliminate, one tool over.
> The alias remains accepted by `resolveFireTime` forever. Guarded by
> `TestSetReminderManifestAdvertisesOnlyInSeconds`.

**A3 notes.** Verification only — the registry already read "(default 3)"
(weather.go:36), matching the code default of 3, and `units`
(imperial|metric, default imperial) was already enumerated and described. No
edit needed; the wrong "(default 1)" lives only in the mint.go literal, which
M19 deletes.

**A4 notes.** All six renderable constraint kinds were confirmed present on
`ParamSpec`s wherever the router enforces them. The four known handler-only
array-element rules were **folded into description prose** (the per-tool
decision the task allows), since `jsonSchema()` cannot express them:
`memory_write.attrs` (≤20 entries, key ≤50 / value ≤500 chars),
`memory_write.relations` (≤20 entries, relationType ≤50 / targetEntityId ≤100
chars), `plan_upsert.steps` (≤30 steps, each ≤500 chars),
`remember_note.tags` (≤10 kept, each ≤50 chars, extras dropped silently — the
prose says so), `deliverable_zip.deliverableIds` (1..`deliv.MaxZipSources`,
rendered via `fmt.Sprintf` so it can't drift from the constant).
**Review fix (2026-07-20):** the handler checks those sentences document were
still measuring **bytes** (`len(s)`) while the new prose promises
"characters" and A5 made the router measure runes — so the handler-side
element checks in `parseAttrPairs`/`parseRelationPairs`, `handlePlanUpsert`'s
step check (memory.go) and `handleRememberNote`'s tag filter (notes.go) now
use `utf8.RuneCountInString` too. Handler and router errors now agree that
"characters" means runes. Regression: `TestMemoryWriteElementLimitsAreRunes`
(400-rune / 1200-byte Japanese attr value accepted; 501 runes rejected).

**A5 notes.** `ParamSpec.coerce()` (registry.go) now measures `MinLen`/`MaxLen`
in runes via `utf8.RuneCountInString`, per the task's preferred option. The
error copy ("at least/at most N characters") is now literally true for
multi-byte content.

---

## M19 — Single-source manifest  `[x]`

**Definition of Done:** `internal/realtime/mint.go` contains **no** hand-written tool
literal; `toolManifest` is derived from `internal/tools`; a parity test makes silent
re-drift impossible; the OpenAI, Gemini and fallback paths all bind a manifest that is
byte-identical to the schema `Invoke` enforces; `go test ./...` and `go vet` green.

Ordered tasks:

- `[x]` **B1 — Extract the definition slice** (WS-B). Lift the 20-entry literal out of
  `NewRegistry` (registry.go:303-328) into `func definitions() []*Definition`. `NewRegistry`
  then ranges over `definitions()`. Pure refactor, no behaviour change.
- `[x]` **B2 — Package-level `CatalogManifest()`** (WS-B). Add
  `func CatalogManifest() []map[string]any` rendering from `definitions()` with **no
  `Deps` required**. Refactor the existing `(*Registry).Manifest()` and the new function
  onto one shared renderer so they can never diverge — a single `renderManifest([]*Definition)`.
  Fix the now-true doc comment at registry.go:344-346.
- `[x]` **B3 — Flip the binding** (WS-B). Replace `var toolManifest = []map[string]any{…}`
  (mint.go:117-500, ~383 lines) with `var toolManifest = tools.CatalogManifest()`.
  Everything downstream derives automatically: `toolManifestJSON` (mint.go:503),
  `ToolManifestJSON()` (mint.go:514), `geminiToolDeclarations()` (gemini_mint.go:121),
  `chatCompletionTools` (fallback_tools.go:64). **Watch init order** — `toolManifestJSON`
  is a package-level `func()` var initialised from `toolManifest`; Go resolves
  package-level dependency order automatically, but confirm with a test that
  `ToolManifestJSON()` is non-empty and parses to 20 entries.
- `[x]` **B4 — Verify the two existing parity tests still pass unmodified** (WS-C).
  `fallback_tools_test.go:19` and `gemini_mint_test.go:78` assert per-index equality of
  `name`/`description`/`parameters` against `toolManifest`. They should pass untouched —
  if either needs editing to go green, the flip changed a derived path and that is a bug,
  not a test to update. **One sanctioned exception, per D-c:** `gemini_mint_test.go`'s
  `description` assertion is expected to need updating once D1 lands, because the Gemini
  sanitizer intentionally augments descriptions. That exception covers that one assertion
  and nothing else.
- `[x]` **C1 — THE missing parity test** (WS-C). New test asserting whatever the broker
  binds equals what the router enforces. Must be **deep, not a name/count check** — the
  `gemini_mint_test.go:53` count-only assertion is exactly the weak form that let this
  drift survive. Assert, per tool: name set equality both directions, and for each tool a
  full `reflect.DeepEqual` of the rendered `parameters` against a freshly rendered
  `CatalogManifest()` entry. Place it in `internal/realtime` (the consumer side) so it
  fails when someone reintroduces a literal.
- `[x]` **C2 — Drift-resistance test** (WS-C). Add a test that constructs a `Definition`
  with every `ParamSpec` field populated and asserts `renderManifest` surfaces all six
  constraint kinds (`enum`, `minLength`, `maxLength`, `pattern`, `minimum`, `maximum`).
  Guards against a future `jsonSchema()` edit silently dropping a constraint kind.
- `[x]` **B5 — Delete `_ = def`** (WS-B). `finish()` (registry.go:539) takes a
  `*Definition` it never reads, ending in a bare `_ = def`. Drop the parameter. Trivial,
  but it is the kind of dead seam that invites a future reader to wire something to it.

**Implementation notes (append as work proceeds):**

**B1 notes.** `registry.go`: the 20-entry constructor literal moved out of
`NewRegistry` into `func definitions() []*Definition` (registry.go:333),
which documents that the constructors are dependency-free so the slice can
be rendered without a live `Deps`. `NewRegistry` now ranges over
`definitions()` and `register()`s each one. Canonical catalog order is
unchanged (send_email … web_research). Pure refactor, no behaviour change.

**B2 notes.** `registry.go`: added `func CatalogManifest() []map[string]any`
(registry.go:375) = `renderManifest(definitions())`, no `Deps` needed.
`(*Registry).Manifest()` was refactored onto the same shared renderer — it
collects its registered `*Definition`s in `r.order` and calls
`renderManifest(defs)` — so the two can never diverge; the single renderer
is `renderManifest([]*Definition)` (registry.go:395), which owns the
`{type,name,description,parameters}` wire shape, the sorted `required`
list, and the `Unadvertised` exclusion. The previously-false doc comment
(registry.go:344-346, "the tools array the broker binds…") now lives on
`CatalogManifest` where it is true by construction; `Manifest`'s comment
points at the shared renderer. A stale "the future CatalogManifest"
reference in the `ParamSpec.Unadvertised` doc comment was updated on the
review pass.

**B3 notes.** `mint.go`: the entire hand-written `var toolManifest =
[]map[string]any{…}` literal (~383 lines, the P1/P2 root cause) is deleted
and replaced by `var toolManifest = tools.CatalogManifest()` (mint.go:120)
with a doc comment stating the advertised schema and the enforced schema
are now the same object by construction. `internal/realtime` now imports
`internal/tools` (no cycle — `tools` never imports `realtime`).
Everything downstream (`toolManifestJSON`, `ToolManifestJSON()`,
`geminiToolDeclarations()`, `chatCompletionTools`) derives automatically;
none of those seams changed. Init order: `toolManifestJSON` is still a
package-level `func()` var initialised from `toolManifest`; Go's
package-level dependency ordering handles the cross-package init, and
`TestToolManifestJSONInitOrder` (internal/realtime/tool_manifest_test.go)
pins it — ToolManifestJSON() non-empty, parses to exactly 20 entries, each
a complete `{type:function,name,description,parameters:{type:object}}`
declaration.

**B4 notes.** Verified: `fallback_tools_test.go` and `gemini_mint_test.go`
are byte-untouched by M19 (git diff shows only mint.go + registry.go
modified, plus the two new test files) and the full suite is green — the
derived paths survived the flip with zero edits. The one sanctioned D-c
exception (gemini description-prefix assertion) had already been consumed
by D1 in M20, which landed before the flip; no further edit was needed or
made.

**C1 notes.** `internal/realtime/tool_manifest_test.go`
`TestBrokerBoundManifestMatchesRouterCatalog` — deliberately on the
CONSUMER side so a reintroduced mint.go literal fails it regardless of how
plausible the literal looks. Compares the actual bound bytes
(`ToolManifestJSON()`, unmarshaled) against a freshly rendered
`tools.CatalogManifest()` JSON-normalized to wire form. Asserts: name-set
equality in BOTH directions, no duplicate declarations on either side,
identical catalog order, and per tool — wire `type`, exact `description`
equality, and a full `reflect.DeepEqual` of the `parameters` schema (the
deep form C1 mandates; never a name/count check). Plus a consumer-visible
spot check of the A2/Unadvertised contract: `set_timer` and `set_reminder`
both advertise `inSeconds` and never the `seconds` compat alias.

**C2 notes.** `internal/tools/manifest_render_test.go`
`TestRenderManifestSurfacesEveryConstraintKind` — a synthetic Definition
populating every `ParamSpec` field proves `renderManifest` surfaces all six
renderable constraint kinds (enum, minLength, maxLength, pattern, minimum,
maximum — string/integer/number/string_array/boolean param types all
covered) and excludes the `Unadvertised` param from both `properties` and
`required` (the synthetic `legacy` alias is marked `Required: true` +
`Unadvertised: true`, proving Unadvertised wins — added on the review pass
to make the required-list assertion's comment literally true).
`TestRenderManifestConstraintTestCoversEveryParamSpecField` reflects over
`ParamSpec` and fails on any added/removed field, forcing the constraint
test and the known-fields list to be updated together.
`TestCatalogManifestEqualsRegistryManifest` closes the render loop:
package-level `CatalogManifest()` deep-equals a live registry's
`Manifest()`.

**B5 notes.** `finish()` (registry.go:545) dropped the unused
`*Definition` parameter and its trailing `_ = def`; the sole call site
(the `defer` in `Invoke`, registry.go:482) updated. Signature is now
`finish(ctx, l, inv, res, start)`.

**Verification.** `go build ./... && go vet ./... && go test ./... -count=1`
all green after the flip and again after the review fixes. The manual
live-audio smoke (steps 1–5 above) is post-deploy/owner-only and remains
outstanding by design.

---

## M20 — Engine hardening & tool discoverability  `[x]`

Independent of M18/M19 — launch WS-D concurrently at the start.

**Definition of Done:** Gemini receives only schema keywords `genai.Schema` models, with
no information lost to the model; all 20 tools are discoverable from the persona prompt;
`go test ./...` green.

Ordered tasks:

- `[x]` **D1 — Gemini schema sanitizer** (WS-D). In `geminiToolDeclarations()`, deep-copy
  each `parameters` map (it is currently shared **by reference** with `toolManifest` —
  fixing that alone removes an aliasing footgun) and strip keywords `genai.Schema` does not
  model. **Verify the supported set against the vendored `genai.Schema` struct definition,
  not from memory** — do not guess which fields survive. Per **Q4**, every stripped
  constraint is appended to that parameter's `description` in plain prose (e.g.
  `Max 100 characters.`, `Letters, digits, dot, dash and underscore only.`) so the model
  can comply rather than being rejected by the router. Generate that prose from the
  `ParamSpec` — never hand-write it per tool, or it becomes the next thing to drift.
  See **D-c**: this deliberately breaks an existing test assertion.
- `[x]` **D2 — Setup/constraints equivalence test** (WS-D). Assert the raw wire `setup`
  frame (`buildGeminiSetup`) and the SDK-typed constraints (`buildGeminiConstraints`)
  declare the **same** tool schemas post-sanitization. This is the gap that lets the minted
  token and the client's setup frame disagree today.
- `[x]` **D3 — Upgrade the count-only assertion** (WS-D).
  `gemini_mint_test.go:53` asserts only `len(toolManifest) == len(cc.Tools[0].FunctionDeclarations)`.
  Make it assert content.
- `[x]` **D4 — Persona tool coverage** (WS-D). Add the six missing tools to
  `coreInstructions` (personas.go:55-70): `deliverable_create`, `deliverable_zip`,
  `deliverable_deliver`, `file_list`, `file_read`, `file_create`. Keep it terse — this
  string is on every session's token budget for every engine. Group them as one
  "documents and downloads" clause rather than six separate sentences.
- `[x]` **D5 — Persona/manifest coverage test** (WS-D). Assert every tool name in the
  manifest appears in `coreInstructions` (or sits on an explicit, named allow-list of
  deliberately-unmentioned tools). Prevents P3 from silently recurring when tool 21 lands.

**Implementation notes (append as work proceeds):**

**D1 notes.** `gemini_mint.go`: `geminiSchemaKeywords` is computed by
**reflecting over the vendored `genai.Schema` struct's `json` tags** (not a
hand list) and was verified 2026-07-20 against google.golang.org/genai
v1.64.0 `types.go` `type Schema struct` — the modeled set is anyOf, default,
description, enum, example, format, items, maxItems, maxLength,
maxProperties, maximum, minItems, minLength, minProperties, minimum,
nullable, pattern, properties, propertyOrdering, required, title, type.
Every keyword today's 20 real tools use IS modeled, so on real data nothing
is stripped yet; the mechanism is proven against synthetic schemas
(additionalProperties, const, multipleOf, uniqueItems) in
`gemini_schema_sanitizer_test.go`. `sanitizeGeminiParameters` deep-copies
(`deepCopyValue`) so declarations never alias `toolManifest` (the P4
by-reference footgun), then `sanitizeSchemaNode` strips unmodeled keywords
depth-first (recursing `properties`/`items`) and folds each into **that
parameter's own** description via `describeStrippedConstraint` — prose is
generated from keyword+value, never hand-written per tool (Q4), and stripped
keys are sorted so output is deterministic.

**D2 notes.** `TestGeminiSetupAndConstraintsDeclareSameToolSchemas`
(gemini_schema_sanitizer_test.go) asserts the raw wire `setup` frame and the
SDK-typed constraints declare the same post-sanitization tool schemas —
closing the minted-token-vs-setup-frame gap P4 described.

**D3 notes.** `gemini_mint_test.go` `TestGeminiMintBuildsConstrainedTokenAndSetup`:
the `len()==len()` assertion now checks per-index name equality across all 20
declarations plus a deep representative check that `file_create.name`'s
minLength/maxLength/pattern and required survive the SDK round trip.

**D4 notes.** `personas.go` `coreInstructions` gained one terse "documents and
downloads" clause naming all six previously-unmentioned tools
(`deliverable_create`/`file_create`, `file_list`/`file_read`,
`deliverable_zip`, `deliverable_deliver`) — one clause, not six sentences,
per the token-budget instruction. The "byte-for-byte pre-personas text"
doc-comment claim was updated since it no longer holds.

**D5 notes.** `persona_tool_coverage_test.go`
`TestEveryManifestToolIsDiscoverableFromPersonaPrompt` asserts every manifest
tool name appears verbatim in `coreInstructions`, with an explicit named
allow-list (`deliberatelyUnmentionedTools`) that is **empty today**;
`TestDeliberatelyUnmentionedToolsStaysNearEmpty` keeps the escape hatch from
silently growing.

**D-c honored.** `TestGeminiToolDeclarationsMirrorManifest`'s `description`
assertion — the one sanctioned exception — now asserts prefix equality plus
sanitizer-shaped suffix; the `parameters` equality assertion was left fully
intact (nothing is stripped from real tools at the current SDK pin, so it
still holds exactly).

---

## Risk register

| Risk | Likelihood | Mitigation |
|---|---|---|
| Flipping the binding changes prompt wording and degrades tool selection | Medium | M18 lands content **before** M19 flips — no regression window by construction |
| `set_timer` regression (this tool has broken in prod before) | Medium | A2 keeps the handler accepting both spellings regardless of what is advertised; add an explicit test that both `inSeconds` and `seconds` resolve identically |
| Init-order surprise on the derived package-level vars | Low | B3 adds an explicit non-empty/parses-to-20 assertion |
| The new parity test is written weakly enough to pass through future drift | Medium | C1 mandates `DeepEqual` on rendered parameters, not names or counts — the count-only form is what failed us |
| Gemini sanitizer strips a keyword Gemini actually supports | Low | D1 requires reading the vendored `genai.Schema` struct, not recalling it |

## Verification (run at the end of every milestone)

```
go build ./... && go vet ./... && go test ./...
```

Manual smoke after M19 (owner, live audio — automation profile mic is hard-blocked, same
constraint as M12/M13 verification):
1. "Set a timer for 20 minutes" → fires; confirm no `invalid_args` in the `LOG#` audit rows.
1b. "Set a timer for 3 days" → the model should hand off to `set_reminder` rather than
   dead-ending (D-a). One `invalid_args` row naming `set_reminder` followed by a successful
   `set_reminder` call is the expected, healthy shape here.
2. "What's the weather in London in celsius" → `units:metric` actually requested.
3. "Reboot the terminal" → `device_control` with `action:reboot` (previously unsurfaced).
4. "What notes do I have tagged work" → tag filter used; "read me my recent notes" with no
   query → succeeds (previously unreachable).
5. Repeat 1–2 on a `gemini-flash-live`-pinned device.

---

## Locked decisions (owner, 2026-07-20)

- **Q1 → Port the better wording per-tool.** Walk all 20; whichever of the manifest or
  registry description is stronger becomes the registry's. No prompt-quality regression.
- **Q2 → Advertise `inSeconds` only.** `seconds` remains accepted by the handler forever
  but is absent from the schema. One unambiguous name; no synonym pair for the model.
- **Q3 → Cap `set_timer` at 24h (86400s).** `set_reminder` owns anything longer.
- **Q4 → Fold stripped constraints into description prose** for Gemini, so the model still
  knows the rule instead of being rejected by the router.

### Second-pass decisions derived from the above (baked, not open)

These surfaced on the review pass after the answers landed. Each has a default baked in —
none blocks execution.

- **D-a — `set_timer` overflow behaviour.** With the 24h cap, "set a timer for 3 days" must
  not dead-end. The registry `Description` explicitly instructs the model to use
  `set_reminder` beyond 24h, and `resolveFireTime` returns `invalid_args` with a message
  **naming `set_reminder`** so the model can self-correct conversationally rather than
  apologising to the user. This works only because the flip also advertises
  `set_reminder.inSeconds` for the first time (it is in the registry today, just
  unadvertised — see P2), so the relative-offset handoff is actually reachable. Verify that
  ordering holds: **M19 must land before the 24h cap is user-visible**, or the handoff
  target does not exist yet. Sequencing is already correct (A2 is behaviour-neutral until
  B3 flips the binding) — do not reorder.
- **D-b — Already-scheduled long timers are unaffected.** EventBridge schedules are
  one-shot with `ActionAfterCompletion: DELETE`; a pending 30-day timer created before the
  cap already exists as its own schedule and still fires. The cap narrows *new* calls only.
  No migration, no backfill, nothing to clean up.
- **D-c — Q4 deliberately breaks an existing parity assertion.** Folding constraints into
  prose means Gemini's tool descriptions are intentionally **not** byte-identical to the
  manifest's. `gemini_mint_test.go:78` currently asserts exact `description` equality and
  **will fail by design**. Update it to assert the Gemini description *begins with* the
  manifest description and that any appended text is generated by the sanitizer — not to
  simply delete the assertion. This is the **one sanctioned exception** to B4's rule that a
  parity test needing edits means a bug; it applies to `gemini_mint_test.go`'s description
  assertion only, and to nothing else.

