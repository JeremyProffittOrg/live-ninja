# Backlog

Future actions **deliberately kept OUT of the plan**. Not scheduled work. Consolidated by
`/clean-plans` on **2026-07-24**.

Promote an item by moving it into [plan.md](plan.md) under a workstream/milestone. Nothing here
should be picked up as "next" without that decision being made explicitly.

## Voice engines

- **Re-enable Nova Sonic (M12).** Fully built and connect-chain-verified, then **disabled by the owner** because the ALB + Fargate standing cost wasn't worth it. Re-enable = flip `NovaBridgeEnable` (two-phase: repo/ALB → image push → `NovaBridgeReady=true`, isolated deploy, nothing else in the same push) + restore the picker option; the ECR repo was force-deleted. ⟵ archive/plan.md M12 / §8 M14 item 3
- **Nova Sonic's empty tool/persona config.** A real defect, explicitly ruled out of scope for the M18–M20 tool-parity work. Only matters if Nova is ever re-enabled. ⟵ archive/tool-parity-plan.md §0
- **Speed / Energy / Register voice knobs.** Proposed to the owner, never picked. ⟵ archive/plan.md §8 M14 item 11b

## Tab5 / firmware

- **On-device tool invocation.** The firmware has *no* tool-invoke plumbing on any engine — OpenAI `function_call`s are ignored by design, and Gemini `functionCall`s get an immediate `{"error":"tool execution is not available on this device"}` response so turns never stall. Full on-device invocation is a deliberate future item. ⟵ archive/gemini-plan.md §10 · archive/tool-parity-plan.md §0
- **Uplink shedding during playback.** 20–50 KB bursts of mic audio are dropped while downlink audio plays (SDIO/WiFi full-duplex ceiling). Options: pace/trim uplink during SPEAKING (barge-in only needs VAD-grade audio), or tune esp-hosted buffers. Non-blocking. ⟵ archive/plan.md §8
- **Echo-triggered self-barge-in at high volume.** AEC is imperfect at volume; also produces a benign `response_cancel_not_active` server warn. Same class the web solved with `micEagerness=low`; mitigable today via the device sensitivity setting. ⟵ archive/plan.md §8
- **Transient `Could not lock ws-client within 1000 timeout`** during heavy downlink — self-recovers; watch only. ⟵ archive/plan.md §8
- **Root-cause the internal DMA heap exhaustion during session setup** (lwIP TCP buffers + esp-tls internals + AFE + fragmentation suspected). Explicitly filed "for someday" — the PSRAM-fallback patches made it a non-issue. ⟵ archive/plan.md §8 item 11
- **Slim the mint response for the device.** The Tab5 only consumes `clientSecret`/`model`/`mode`/`wsUrl`, but receives rates + full sessionConfig + the ~20-tool manifest (which is why the HTTP body cap had to go 16→64 KB). ⟵ archive/plan.md §8 bug 4
- **More wake phrases from the esp-sr zoo** (Jarvis, Computer, Hey Willow, Mycroft, Sophia, Hi Jason…) — one sdkconfig bool each; ~340 KB partition headroom. ⟵ archive/plan.md §8 item 10

## Web / conversation UX

- **Wake-buffer replay.** First words spoken during "Connecting" are lost to the model entirely. The batch-5 connect-latency prefetch may be enough; replay is the full fix. ⟵ archive/plan.md §8 M14 item 11c
- **Multi-message tool confirmations in fallback turns.** Fallback turns are stateless per message, so "yes, send it" needs a live session. Candidate feature. ⟵ archive/plan.md §8
- **Ship args with fallback-turn tool results.** `tools.Result` has no `Args` field server-side, so the Details dialog shows "(no input recorded)" for fallback turns. Possible future backend tweak. ⟵ archive/plan.md §8 Task #9

## Deferral decisions the owner explicitly parked

⟵ docs/qa-report.md "Deferral decisions (owner call)" — each is a real finding the owner chose not to schedule.

- **History device/topic filter pagination edge case.** Acceptable to defer until history exceeds one page; the fix would be a server-side fill-the-page loop, or keeping "Load more" visible while a cursor exists.
- **Strict refresh-reuse posture.** A benign multi-tab web refresh race triggers a full family-revoke + security alert. Confirm that's acceptable UX for real multi-tab usage, or soften it.
- **Custom-accent-as-text-color AA contrast risk.** Either accept, or derive an independent accent-ink colour / clamp contrast.

## Idea catalogs (unscheduled proposals — not plan items)

Two large proposal documents exist and are **not** scheduled. They are kept where they are as
reference; nothing in them is committed work until an item is promoted into `plan.md`.

- **[docs/agentic-expansion-review.md](docs/agentic-expansion-review.md)** (2026-07-19) — 84 suggestions from a 16-agent review, each anchored to real files, effort-rated S/M/L/XL, with adversarial verifier corrections quoted inline. Themes: remote coding sessions, media playback, news/feeds, briefings, safety.
- **[docs/agentic-expansion-suggestions.md](docs/agentic-expansion-suggestions.md)** (2026-07-20) — 43 capabilities across nine themes + six foundational fixes, sequenced as a proposed **M15–M24** roadmap for an in-car assistant.
  ⚠️ **Numbering collision:** that document reuses **M15–M17**, which are already taken by the Base Knowledge / RCA milestones in `plan.md`. Renumber before promoting anything from it. (Its own F1 correctly identifies Base Knowledge as the hard dependency to ship first — that part is already in the plan.)

## Platform capabilities not being pursued

- **FCM push for settings fan-out.** No Firebase account — web and Android use poll/foreground reconcile instead; the Tab5 uses the IoT shadow. Locked at M6. ⟵ archive/plan.md §8 M6
- **Porcupine wake engine.** Catalog-flagged unavailable (needs Picovoice seats); openWakeWord is the free default and the reason training is never blocked. ⟵ archive/plan.md §8 M6
- **SNS ops-topic email subscription confirmation.** Optional by design: the owner wants **no CloudWatch alerts**, budgets email directly, and the topic's only producer is SES bounce/complaint. Confirm the subscription only if bounce/complaint notices are wanted. ⟵ SETUP.md · archive/plan.md §8 M14 item 12
- **Bedrock Nova Sonic model access in `us-east-1`.** Was only ever needed for M12, which is disabled. ⟵ SETUP.md
