# Build Prompt — Live Ninja PRD + Plan (for cross-model comparison)

You are a principal software architect + product designer. Working **autonomously** (make sensible default decisions, state them, never stop to ask), produce a complete, comprehensive design package for the product specified below.

**Your output: write exactly two files into your current working directory —**
- **`prd.md`** — a formal Product Requirements Document.
- **`plan.md`** — a formal implementation plan.

Both in GitHub-flavored Markdown, with **architecture, flow, sequence, ER, state, and gantt diagrams embedded as valid Mermaid** (```mermaid fenced blocks). Be concrete and technical — name components, endpoints (method/path/purpose), DynamoDB PK/SK/GSI keys, IoT topics, OAuth flows, token lifetimes, and libraries. Do not ask questions; do not stop early.

---

## PRODUCT: "Live Ninja"

A GPT-Realtime ("gpt-live") **speech-to-speech** voice-assistant PLATFORM with ONE common cloud backend serving THREE client surfaces, all gated by **Login with Amazon (LWA)**. Wake phrase "Hey Live Ninja"; wake words are user-**programmable** on every surface.

### Hard requirements (do not contradict)
- **Voice:** OpenAI GPT Realtime API. Backend brokers short-lived **ephemeral session tokens** — the OpenAI key never touches a client. Support barge-in, turn/VAD detection, tool/function calling, configurable persona.
- **Backend on AWS:** AWS **SAM**; **Lambda in Go on arm64/Graviton**; **Go-Fiber** for the web front end + HTTP handlers; **DynamoDB** for KV/session/state/device data — **Query/GetItem only, NEVER a Scan on a serving path** (design keys + GSIs accordingly); **S3** for uploads/downloads + audio; **SES** for email; **SSM Parameter Store** for config/secrets (no paid secrets manager). Optional **AWS IoT Core** for the M5Stack.
- **Deploy:** strictly **GitHub Actions + AWS OIDC** (no local deploys, no static keys). Push to `main` = production deploy. Production-only shop. Mandatory stack cost tags: Project, CostCenter, Environment=prod, ManagedBy=sam, DeployedVia=github-actions, Owner.
- **Clients:**
  1. **Android** — wake word "Hey Live Ninja"; becomes the phone's **primary/default voice assistant** (VoiceInteractionService + RoleManager.ROLE_ASSISTANT, assist gesture, launch-from-lock); always-on on-device programmable wake word (Porcupine/openWakeWord) in a foreground service; audio via WebRTC to OpenAI Realtime.
  2. **Website** — Go-Fiber-served browser voice UI; WebRTC → OpenAI Realtime with a backend-minted ephemeral token; optional in-browser programmable wake-word spotting + click-to-talk fallback; responsive; rich-UI + WCAG AA.
  3. **M5Stack Tab5** (ESP32-P4 + ESP32-C6 Wi-Fi, 5" 1280×720 landscape touch LCD, mic+speaker) — on-device wake word (ESP-SR/microWakeWord); streams Opus/PCM audio to the backend over **AWS IoT Core MQTT** (or a WS gateway) which bridges to OpenAI Realtime and streams TTS back; **hosts its own config web page** (SoftAP captive portal for Wi-Fi, then a served page) where the user does **Login with Amazon on the device**; native LCD firmware (ESP-IDF + LVGL) follows embedded-UI rules.
- **Auth & sessions:** LWA (Authorization Code + PKCE) front-ends all surfaces; backend validates LWA tokens and mints its **own first-party session credential**. **Web + Android = 30-day** sessions (rotating refresh). **M5Stack persists login for 10 years** (long-lived, silently-rotated, revocable device refresh credential minted after on-device LWA, stored in ESP32 encrypted NVS/flash with flash encryption + secure boot). Devices bind to a user via a pairing/registration step.
- **Wake words:** programmable everywhere; per-user config in DynamoDB, synced to devices (IoT shadow for the M5Stack).

### v1.1 capabilities (must also be specified)
- **Deliverables Store.** The assistant can **create files** (PDF/MD/CSV/JSON/ICS/image/artifact), **zip** several into one archive, and **deliver** them as separate, durable, per-user downloadables stored on S3 (`{userId}/{deliverableId}/{filename}`), indexed in DynamoDB (`PK=USER#{userId}, SK=DELIV#{ts}#{id}`, Query-only, GSI for share-by-id), delivered via short-lived presigned URLs, and browsable identically from a website **Download Center** and an Android **Files tab**. Every artifact-producing turn logs a deliverable ("all transactions"). Expose as tools `deliverable.create/zip/deliver`.
- **Memory Layer.** A structured personal memory whose first-class entities are **people, places, and information**, plus **organizational** (projects, lists, documents) and **planning** (goals, tasks, schedules) capabilities. Memory types: working / episodic / semantic / procedural. Compare candidate recall architectures — **local RAG on the user's PC** (LanceDB/sqlite-vec/Chroma), **Amazon S3 Vectors**, **DynamoDB entity graph**, **OpenSearch Serverless**, **pgvector/Aurora** — on cost, latency, privacy, ops, scale, and recommend a **hybrid**: DynamoDB entity/relationship graph (structured + organizational + planning) + S3 Vectors (semantic recall) + an **optional** local RAG sidecar (graceful fallback). Expose tools `memory.search`, `memory.write`, `entity.get`, `plan.upsert`; retrieve a relevant memory slice at session bootstrap to prime the persona; provide a memory browser with view/edit/forget (forget propagates to DynamoDB + vector index). Avoid always-on cost floors (OpenSearch/Aurora) unless scale justifies.
- **Guide Entities.** A special memory entity **injected into the system instructions of every session on every surface** — unconditional, not relevance-retrieved. User-managed (create/edit/enable/priority), versioned, device-synced; directives can steer tools (e.g., a recency rule constrains web search). Ship a default-enabled guide **"AI is an emerging technology"**: prefer technical documents/articles **published or updated within the last 30 days**; otherwise defer to the **official technical documentation of leading AI providers (Anthropic, OpenAI)**; always cite sources with dates and flag when the best source is older than 30 days.

### UI design rules
- Rich UI (Android, Web, M5Stack config portal): semantic/native controls, WCAG AA (light+dark), visible focus, ~44px targets, and **no blind free-text where the value set is known** (wake word, voice, language/locale, timezone, persona, device → pickers/lists/segmented controls). Present data as structured lists/tables/cards.
- M5Stack native LCD (1280×720 landscape touch, embedded): one primary value/decision per screen, big legible type, segmented/roller/full-screen list-select instead of dropdowns, 48–64px targets, "N of M" indicators, on-screen keyboard only for Wi-Fi passphrase / device name.

---

## Required contents

**`prd.md`:** executive summary + metadata; vision/goals/non-goals; personas & use cases; functional requirements grouped by surface (Backend, Voice, Auth, Android, Web, M5Stack, Wake-word, Deliverables Store, Memory Layer, Guide Entities) each with an ID and acceptance criteria; per-surface UX + brief wireframe notes; system architecture (with Mermaid diagrams); voice/realtime experience; authentication & sessions (30-day + 10-year) with login sequence diagrams; data model (DynamoDB keys + ER diagram); non-functional requirements; security & privacy (on-device wake, retention/deletion, threat model); KPIs; assumptions/dependencies; risks table (risk | impact | mitigation); open questions each answered with a chosen default.

**`plan.md`:** organized as **parallel workstreams** broken into sequential **milestones M0–M10** (M0 bootstrap/infra → M1 auth → M2 realtime voice backend + tools → M3 web → M4 Android → M5 M5Stack firmware + IoT + on-device 10-year login → M6 programmable wake-word + settings sync → M7 hardening/observability/cost/privacy → M8 launch → **M9 Deliverables Store** → **M10 Memory Layer + Guide Entities**). Each milestone: a Definition of Done + an ordered task list; every milestone and task carries a **status marker** (`[ ]` todo, `[~]` in progress, `[x]` done, `[!]` blocked) and a **model-routing** annotation (cheapest capable model per task). Include a workstream map, a CI/CD pipeline Mermaid, a roadmap gantt, a testing/verification strategy per milestone, and an execution-risks table.

Diagrams to include (valid Mermaid): high-level system context; AWS deployment; voice-turn sequence; web / Android / M5Stack(10-yr) login sequences; M5Stack audio-over-IoT sequence; DynamoDB ER; wake-word sync; M5Stack UI state machine; Deliverables flow; Memory architecture; CI/CD; roadmap gantt.

Write `prd.md` and `plan.md` now.
