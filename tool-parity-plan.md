# Live Ninja — Tool Manifest Parity & Correctness

> **Status:** READY TO EXECUTE — all open questions answered 2026-07-20 · **Owner:** jeremy · 2026-07-20
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

## M18 — Registry becomes prompt-ready  `[ ]`

**Why this comes first:** M19 flips every engine onto the registry's descriptions. If the
registry's wording is worse than the hand-tuned manifest copy, flipping ships a
prompt-quality regression that lasts until someone fixes it. Doing content first makes the
flip a pure improvement with **no regression window**. Nothing binds the registry manifest
yet, so every task here is behaviour-neutral in production and safe to land incrementally.

**Definition of Done:** every `Definition` in `internal/tools/` carries the wording and
constraints we actually want the model to see; `go test ./...` green; **no production
behaviour has changed yet** (nothing binds `Manifest()` until M19).

Ordered tasks:

- `[ ]` **A1 — Description reconciliation** (WS-A). For each of the 20 tools, compare the
  `mint.go` manifest description against the registry `Description` and land the better
  wording in the registry. The manifest copy is voice-tuned and often terser; the registry
  copy is more precise about failure modes. Per **Q1**, keep whichever is stronger. Record
  the chosen wording per tool in the notes block below so the diff is reviewable.
- `[ ]` **A2 — `set_timer` param surface** (WS-A). Per **Q2**: drop `seconds` from the
  advertised `Params` while `resolveFireTime` (scheduler.go) keeps accepting **both**
  spellings — that alias is load-bearing prod-compat and removing it re-breaks the tool.
  Per **Q3**: set `inSeconds` `Max` to 86400. Per **D-a**: the `Description` must tell the
  model to use `set_reminder` beyond 24h, and the out-of-range `invalid_args` message must
  name `set_reminder` so the model can self-correct. Add a test asserting `inSeconds` and
  `seconds` resolve to an identical fire time.
- `[ ]` **A3 — `get_weather` description fix** (WS-A). `days` description currently says
  "(default 1)" in the manifest and "(default 3)" in the registry; the code defaults to 3.
  Make the registry say 3. Confirm `units` enum + default are described.
- `[ ]` **A4 — Audit every `ParamSpec` for advertise-ability** (WS-A). Walk all 20 tools ×
  all params: any constraint the router enforces (`MinLen`/`MaxLen`/`Min`/`Max`/`Enum`/
  `SafeName`) must be present on the `ParamSpec` so `jsonSchema()` renders it. This is
  mostly verification — `jsonSchema()` (registry.go:374) already renders all six — but
  confirm no constraint lives only in handler code where the schema can't see it.
  Known handler-only constraints to either promote or document as deliberate:
  `remember_note.tags` (≤10, each ≤50), `memory_write.attrs`/`relations` (≤20 each),
  `plan_upsert.steps` (≤30, each ≤500), `deliverable_zip.deliverableIds` (1..MaxZipSources).
  These are array-element rules `jsonSchema()` cannot express today — decide per tool
  whether to fold them into the description prose instead.
- `[ ]` **A5 — Byte-length caveat** (WS-A). `MinLen`/`MaxLen` are measured in **bytes**
  (`len(s)`), not runes, so multi-byte content hits the cap earlier than a user expects.
  Either switch to `utf8.RuneCountInString` or state the unit in the rendered description.
  Prefer the former; it is a two-line change in `validateArgs` and strictly less surprising.

**Implementation notes (append as work proceeds):**
_(empty — WS-A to fill)_

---

## M19 — Single-source manifest  `[ ]`

**Definition of Done:** `internal/realtime/mint.go` contains **no** hand-written tool
literal; `toolManifest` is derived from `internal/tools`; a parity test makes silent
re-drift impossible; the OpenAI, Gemini and fallback paths all bind a manifest that is
byte-identical to the schema `Invoke` enforces; `go test ./...` and `go vet` green.

Ordered tasks:

- `[ ]` **B1 — Extract the definition slice** (WS-B). Lift the 20-entry literal out of
  `NewRegistry` (registry.go:303-328) into `func definitions() []*Definition`. `NewRegistry`
  then ranges over `definitions()`. Pure refactor, no behaviour change.
- `[ ]` **B2 — Package-level `CatalogManifest()`** (WS-B). Add
  `func CatalogManifest() []map[string]any` rendering from `definitions()` with **no
  `Deps` required**. Refactor the existing `(*Registry).Manifest()` and the new function
  onto one shared renderer so they can never diverge — a single `renderManifest([]*Definition)`.
  Fix the now-true doc comment at registry.go:344-346.
- `[ ]` **B3 — Flip the binding** (WS-B). Replace `var toolManifest = []map[string]any{…}`
  (mint.go:117-500, ~383 lines) with `var toolManifest = tools.CatalogManifest()`.
  Everything downstream derives automatically: `toolManifestJSON` (mint.go:503),
  `ToolManifestJSON()` (mint.go:514), `geminiToolDeclarations()` (gemini_mint.go:121),
  `chatCompletionTools` (fallback_tools.go:64). **Watch init order** — `toolManifestJSON`
  is a package-level `func()` var initialised from `toolManifest`; Go resolves
  package-level dependency order automatically, but confirm with a test that
  `ToolManifestJSON()` is non-empty and parses to 20 entries.
- `[ ]` **B4 — Verify the two existing parity tests still pass unmodified** (WS-C).
  `fallback_tools_test.go:19` and `gemini_mint_test.go:78` assert per-index equality of
  `name`/`description`/`parameters` against `toolManifest`. They should pass untouched —
  if either needs editing to go green, the flip changed a derived path and that is a bug,
  not a test to update. **One sanctioned exception, per D-c:** `gemini_mint_test.go`'s
  `description` assertion is expected to need updating once D1 lands, because the Gemini
  sanitizer intentionally augments descriptions. That exception covers that one assertion
  and nothing else.
- `[ ]` **C1 — THE missing parity test** (WS-C). New test asserting whatever the broker
  binds equals what the router enforces. Must be **deep, not a name/count check** — the
  `gemini_mint_test.go:53` count-only assertion is exactly the weak form that let this
  drift survive. Assert, per tool: name set equality both directions, and for each tool a
  full `reflect.DeepEqual` of the rendered `parameters` against a freshly rendered
  `CatalogManifest()` entry. Place it in `internal/realtime` (the consumer side) so it
  fails when someone reintroduces a literal.
- `[ ]` **C2 — Drift-resistance test** (WS-C). Add a test that constructs a `Definition`
  with every `ParamSpec` field populated and asserts `renderManifest` surfaces all six
  constraint kinds (`enum`, `minLength`, `maxLength`, `pattern`, `minimum`, `maximum`).
  Guards against a future `jsonSchema()` edit silently dropping a constraint kind.
- `[ ]` **B5 — Delete `_ = def`** (WS-B). `finish()` (registry.go:539) takes a
  `*Definition` it never reads, ending in a bare `_ = def`. Drop the parameter. Trivial,
  but it is the kind of dead seam that invites a future reader to wire something to it.

**Implementation notes (append as work proceeds):**
_(empty — WS-B/WS-C to fill)_

---

## M20 — Engine hardening & tool discoverability  `[ ]`

Independent of M18/M19 — launch WS-D concurrently at the start.

**Definition of Done:** Gemini receives only schema keywords `genai.Schema` models, with
no information lost to the model; all 20 tools are discoverable from the persona prompt;
`go test ./...` green.

Ordered tasks:

- `[ ]` **D1 — Gemini schema sanitizer** (WS-D). In `geminiToolDeclarations()`, deep-copy
  each `parameters` map (it is currently shared **by reference** with `toolManifest` —
  fixing that alone removes an aliasing footgun) and strip keywords `genai.Schema` does not
  model. **Verify the supported set against the vendored `genai.Schema` struct definition,
  not from memory** — do not guess which fields survive. Per **Q4**, every stripped
  constraint is appended to that parameter's `description` in plain prose (e.g.
  `Max 100 characters.`, `Letters, digits, dot, dash and underscore only.`) so the model
  can comply rather than being rejected by the router. Generate that prose from the
  `ParamSpec` — never hand-write it per tool, or it becomes the next thing to drift.
  See **D-c**: this deliberately breaks an existing test assertion.
- `[ ]` **D2 — Setup/constraints equivalence test** (WS-D). Assert the raw wire `setup`
  frame (`buildGeminiSetup`) and the SDK-typed constraints (`buildGeminiConstraints`)
  declare the **same** tool schemas post-sanitization. This is the gap that lets the minted
  token and the client's setup frame disagree today.
- `[ ]` **D3 — Upgrade the count-only assertion** (WS-D).
  `gemini_mint_test.go:53` asserts only `len(toolManifest) == len(cc.Tools[0].FunctionDeclarations)`.
  Make it assert content.
- `[ ]` **D4 — Persona tool coverage** (WS-D). Add the six missing tools to
  `coreInstructions` (personas.go:55-70): `deliverable_create`, `deliverable_zip`,
  `deliverable_deliver`, `file_list`, `file_read`, `file_create`. Keep it terse — this
  string is on every session's token budget for every engine. Group them as one
  "documents and downloads" clause rather than six separate sentences.
- `[ ]` **D5 — Persona/manifest coverage test** (WS-D). Assert every tool name in the
  manifest appears in `coreInstructions` (or sits on an explicit, named allow-list of
  deliberately-unmentioned tools). Prevents P3 from silently recurring when tool 21 lands.

**Implementation notes (append as work proceeds):**
_(empty — WS-D to fill)_

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

