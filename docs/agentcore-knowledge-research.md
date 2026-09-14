# Replacing Live Ninja's memory + knowledge with AWS managed services — research (2026-09-14)

Question from the owner: can Amazon Bedrock AgentCore Memory (plus Bedrock Knowledge Bases)
replace the current home-built memory/RAG layer and the home-network knowledge relay, and what
does it cost? This file is the evidence behind the email sent 2026-09-14 and the backlog entry
`agentcore-knowledge` in [backlog.md](../backlog.md). Every AWS number carries its source; docs
pages have no publish date, so they are marked "accessed 2026-09-14".

## 1. What runs today (verified in this repo)

Two separate things are called "knowledge" in the product:

**A. Per-user memory** (`internal/memory`, `internal/store/entities.go`, `internal/tools/memory.go`)
- One DynamoDB table `live-ninja` (PAY_PER_REQUEST). Memories are `USER#<uid>/ENT#<type>#<id>`
  items (person | place | info | project | task | plan); each has a sibling `EMB#<id>` row holding
  a base64 float32 vector from `amazon.titan-embed-text-v2:0`, 512 dims, normalised
  (`internal/memory/embedder.go:26-37`).
- Search is brute force: embed the query (one Bedrock call), Query every `EMB#` row in the user's
  partition (hard cap `MaxEmbeddingsPerUser = 2000`, `internal/store/entities.go:64-68`), cosine in
  the Lambda, then one GetItem per hit (`internal/memory/search.go:33-85`).
- Nothing is extracted from transcripts. A memory exists only when the model calls `memory_write`
  or `plan_upsert`, or the user edits it on the Memory page. The mint directive at
  `internal/realtime/mint.go:70-84` exists because prod showed "6× memory_write, 0× memory_search"
  while the owner asked for his own address (2026-07-18).
- Memory is never pre-loaded into the system prompt; only the profile (BASE KNOWLEDGE) and Guides
  are. Recall happens only if the model chooses to call `memory_search` mid-session.
- No TTL on `ENT#`/`EMB#`. No metric counts embedding calls or rows per user
  (`cmd/usage-rollup` sums tokens and seconds only).
- Original design chose S3 Vectors (`PRD.md:2061-2080`, `FR-MEM-06`: "avoid always-on cost
  floors"); the SDK gap that forced the DynamoDB fallback closed on 2026-07-17
  (`internal/store/entities.go:16-24`). `contracts/api.md:209-211` still documents S3 Vectors.

**B. Owner knowledge** (`internal/tools/knowledge.go`, commits `bf8c1bc` / `e3fae4b`)
- `knowledge_search` and `knowledge_recent` are owner-only tools. They enqueue a request on SQS
  `kp-query-requests` and poll DynamoDB `kp-query-results` every 200 ms inside a 2,500 ms budget.
- The answering system is the `knowledge-plane` repo's relay worker on a home-network box
  (`elite001`) with no public listener. If it is asleep the tool answers "the home knowledge store
  did not answer" and the assistant says so. Sources: session, session_summary, email, page, note,
  gh_commit, gh_pr, gh_issue, gh_doc, gh_repo, digest.

**Current cost of A and B inside this account** is close to zero and has no floor: Titan V2 at
$0.02 per 1M tokens, DynamoDB on-demand reads of at most 2,000 small rows per search, SQS and
DynamoDB for the relay. The case for change is capability and reliability, not savings.

## 2. What AWS offers

### 2.1 Amazon Bedrock AgentCore Memory (for A)

- GA since 2025-10-13, available in us-east-1
  [whats-new 2025-10-13](https://aws.amazon.com/about-aws/whats-new/2025/10/amazon-bedrock-agentcore-available/),
  [regions](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/agentcore-regions.html).
- Model: short-term memory = raw events per `actorId` + `sessionId` (`CreateEvent`); long-term
  memory = records extracted and consolidated asynchronously by built-in strategies
  (Semantic, UserPreference, Summary, Episodic; max 6 per memory), read back with
  `RetrieveMemoryRecords` (semantic search, `topK` ≤ 100, cosine-derived score) scoped by
  namespace such as `/users/{actorId}/facts/`
  [memory types](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory-types.html),
  [namespaces](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/specify-long-term-memory-organization.html),
  [Retrieve API](https://docs.aws.amazon.com/bedrock-agentcore/latest/APIReference/API_RetrieveMemoryRecords.html).
- IAM condition keys `bedrock-agentcore:namespace` / `namespacePath` can fence reads per user.
- Standalone: usable from Lambda with plain IAM and the Go SDK
  `github.com/aws/aws-sdk-go-v2/service/bedrockagentcore` (v1.48.0, 2026-09-09). AgentCore Runtime
  and Gateway are not required
  [memory](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory.html).
- Latency (AWS blog, not an SLA): retrieval about 200 ms; extraction + consolidation 20–40 s after
  the event
  [deep dive 2025-10-15](https://aws.amazon.com/blogs/machine-learning/building-smarter-ai-agents-agentcore-long-term-memory-deep-dive/).
- Retention: `eventExpiryDuration` 7–365 days (docs disagree on a 3-day floor; treat 7 as safe).
  Long-term records persist until deleted. Encryption at rest with an AWS-owned key by default,
  CMK optional.
- Quotas (us-east-1 defaults): `CreateEvent` 200 TPS per account, 5 TPS per actor+session;
  `RetrieveMemoryRecords` 30 TPS; 150 memories per account
  [quotas](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/bedrock-agentcore-limits.html).
- Deletion: no "delete everything for actor" call. Account purge = `ListSessions` → `ListEvents` →
  `DeleteEvent` (20 TPS) plus `ListMemoryRecords` by namespace → `BatchDeleteMemoryRecords`.
  No bulk export API; record streaming to Kinesis is the export path.
- Data residency: built-in extraction uses cross-region inference inside the US geography; opt out
  only with a built-in-with-overrides strategy and your own model
  [cross-region](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/cross-region-inference.html).

### 2.2 Amazon Bedrock Knowledge Bases (for B)

- Two shapes. Customer-managed KB: you pick the vector store and embedding model.
  Bedrock Managed Knowledge Base (GA 2026-06-17, us-east-1): AWS runs the datastore, parser,
  embeddings and reranker
  [blog 2026-06-17](https://aws.amazon.com/blogs/aws/introducing-amazon-bedrock-managed-knowledge-base-for-faster-more-accurate-enterprise-ai-applications/).
- Ingestion from S3 is incremental (`StartIngestionJob`); `Retrieve` returns chunks with scores;
  Go SDK `bedrockagentruntime` v1.63.0 has `Retrieve`, `RetrieveAndGenerate`, `Rerank`.
- AWS guidance is to use AgentCore Memory for personal context loaded at session start and a
  Knowledge Base as an on-demand tool
  [memory + RAG](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/memory-ltm-rag.html),
  [reference build 2026-02-25](https://aws.amazon.com/blogs/machine-learning/building-intelligent-event-agents-using-amazon-bedrock-agentcore-and-amazon-bedrock-knowledge-bases/).

## 3. Pricing

All from [AgentCore pricing](https://aws.amazon.com/bedrock/agentcore/pricing/),
[Bedrock pricing](https://aws.amazon.com/bedrock/pricing/),
[S3 pricing](https://aws.amazon.com/s3/pricing/),
[OpenSearch pricing](https://aws.amazon.com/opensearch-service/pricing/),
[Aurora pricing](https://aws.amazon.com/rds/aurora/pricing/), accessed 2026-09-14.

| Line item | Price | Fixed floor |
|---|---|---|
| AgentCore Memory, short-term events | $0.25 per 1,000 events | none |
| AgentCore Memory, long-term records (built-in strategies) | $0.75 per 1,000 records stored per month | none |
| AgentCore Memory, long-term records (override / self-managed) | $0.25 per 1,000 per month + your own model calls | none |
| AgentCore Memory, retrieval | $0.50 per 1,000 `RetrieveMemoryRecords` calls | none |
| Managed Knowledge Base, index storage | $5.00 per GB of raw data per month | none stated |
| Managed Knowledge Base, standard retrieval | $1.00 per 1,000 `Retrieve` calls | none |
| Managed Knowledge Base, agentic retrieval | $4.00 per 1,000 + $1.00 per 1,000 underlying retrieves | none |
| S3 Vectors (customer-managed KB alternative) | $0.06 per GB-month, $0.20 per GB PUT, $2.50 per 1M queries | none |
| Titan Text Embeddings V2 | $0.02 per 1M input tokens (AWS blog; pricing page is JS-rendered) | none |
| OpenSearch Serverless Classic | $0.24 per OCU-hour | $175 (dev) to $350 (prod) per month |
| Aurora Serverless v2 pgvector, always on | $0.12 per ACU-hour | $44 per month at 0.5 ACU (0 ACU auto-pause exists, 15 s resume) |
| Neptune Analytics | derived $0.03 per m-NCU-hour | about $700 per month |
| AgentCore Runtime / Gateway | $0.0895 per vCPU-hour + $0.00945 per GB-hour / $0.005 per 1,000 invocations | not needed for Memory |

Worked estimate at today's scale (5 users, 20 sessions per day, 30 events per session,
40 long-term records per user per month, 2,000 retrievals per month):

| Component | Month 1 | Month 12 |
|---|---|---|
| Events: 18,000 × $0.25/1,000 | $4.50 | $4.50 |
| Records: 200 new per month, cumulative | $0.15 | $1.80 |
| Retrievals: 2,000 × $0.50/1,000 | $1.00 | $1.00 |
| Managed KB: < 0.1 GB storage + 2,000 retrieves | about $1.50–2.25 | same |
| **Total** | **about $7–8** | **about $9–10** |

At 10× (50 users, 200 sessions per day): Memory about $57–73 per month, Managed KB about $20–25
per month, total about $80–95 per month. No fixed floor in either variant.

## 4. Pricing concerns (read before deciding)

1. **The move does not save money.** Today's layer costs cents. AgentCore adds roughly $7–10 per
   month now and scales linearly with turns. The justification is automatic extraction from
   transcripts, consolidation, no 2,000-row cap, no brute-force scan, and no home box that can be
   asleep.
2. **Events are billed per turn.** A long voice session can be 100+ turns, not 30. At 100 turns per
   session the events line is $15 per month at today's session count, $150 at 10×. The plan must
   batch turns into fewer events (one event per exchange or per N turns) and measure before
   enabling every surface.
3. **Retrievals are billed per call.** If `memory_search` were called on every turn the retrieval
   line would dominate. The plan pre-loads once at session start (one `RetrieveMemoryRecords`) and
   keeps `memory_search` as an on-demand tool, so retrievals stay near session count.
4. **Long-term storage only grows.** Records persist until deleted, at $0.75 per 1,000 per month.
   At today's scale that is under $2 per month after a year, but there is no TTL. The plan adds a
   pruning job (delete records older than N days that were never retrieved) and the account-purge
   path.
5. **Managed KB storage is per GB of raw data at $5.** Fine under 1 GB. Do not point it at the raw
   email or session archive without curating; the knowledge-plane corpus should be exported as
   compact text.
6. **Do not use Agentic Retrieval** ($4 per 1,000 plus underlying calls). Standard `Retrieve` is
   enough for the voice tool.
7. **Avoid every store with a floor**: OpenSearch Serverless Classic ($175–350 per month), Aurora
   always-on ($44), Neptune Analytics (about $700). This matches `FR-MEM-06`.
8. **Cross-region inference for built-in extraction** moves prompts within the US. If that is not
   acceptable, the override strategy (your own model, $0.25 per 1,000 records + model cost) is the
   opt-out; it costs more engineering, not more money.
9. **Cost guard without CloudWatch alarms** (house rule): one AWS Budgets monthly budget on the
   `bedrock-agentcore` and `bedrock` service dimensions (the first two budgets are free), plus the
   existing usage-rollup gaining an events/retrievals count per user so a runaway is visible in the
   owner's own numbers.
10. **Unverified numbers**: the default extraction model name, Amazon Rerank 1.0 price verbatim,
    any per-call charge for customer-managed KB `Retrieve`, and Bedrock KB support for NextGen
    OpenSearch Serverless (scale-to-zero) collections. None of these change the recommendation.

## 5. Recommendation

Adopt in two independent stages, both without a fixed floor:

- **Stage 1 (memory):** one AgentCore Memory in us-east-1 with `SemanticMemoryStrategy` and
  `UserPreferenceMemoryStrategy`, namespaces `/users/{actorId}/…`, `eventExpiryDuration` 30.
  The web Lambda writes one event per exchange from the existing transcript sink; the broker
  pre-loads the top 10 records into the BASE KNOWLEDGE block at mint; `memory_search` becomes a
  thin `RetrieveMemoryRecords` call; `memory_write` becomes an explicit event; `forget` deletes
  records. Keep DynamoDB `ENT#` rows as the editable "Memory page" view during dual-write, then
  retire `EMB#` and the cosine path.
- **Stage 2 (knowledge):** a Bedrock Managed Knowledge Base fed from an S3 export of the
  knowledge-plane corpus (work in the `knowledge-plane` repo: nightly export to S3 as compact text
  with source metadata). `knowledge_search`/`knowledge_recent` call `Retrieve` directly; the SQS
  relay stays as the fallback until the KB answers the owner's smoke set better than the relay.
  If the corpus export is not wanted, Stage 1 stands alone.

## 6. Risks that are not about price

- Extraction is asynchronous (20–40 s). A fact stated at the end of one session may not be
  retrievable at the start of the next if the user reconnects immediately. Mitigation: keep the
  explicit `memory_write` path for "remember this" so it is stored synchronously as a record.
- No bulk delete or export. The account-purge Lambda must page through sessions and records; the
  export must page `ListMemoryRecords`. Both are bounded by 20–30 TPS quotas.
- Built-in strategy prompts and schemas are AWS-controlled; records are plain text so leaving is
  possible, but the extraction quality is not tunable without the override strategy.
- The Help drawer, Memory page copy, and `contracts/api.md` all describe today's behaviour and
  must change in the same commits (house rule).
