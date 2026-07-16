# Design: memory-hybrid-retrieval-locomo

## Context

L1 prompt-memory (per-entry SQLite store, migration v7) is written only when the
LLM calls `memory_write`, and recall is FTS5-trigram MATCH with a LIKE fallback.
There is no embedding, vector, or entity code anywhere in the repo; the
`provider.Provider` interface is `Name()+Stream()` only. The curation engine
(scorer + trigram-Jaccard clustering + LLM judge, DB leader lease) already
performs background merge/evict.

Mem0's April-2026 v3 algorithm (LoCoMo 71.4 → 92.5 on their managed platform)
attributes its gains to: single-pass ADD-only extraction, agent-generated facts
stored first-class, entity linking, multi-signal retrieval (semantic + BM25 +
entity, fused), and time-aware ranking — with a large single-pass retrieval
budget (top_200) and no agentic answer loop. Independent audits show LoCoMo
absolute numbers are not comparable across vendors (6.4% answer-key errors,
judge leniency), so this change measures itself A-B against its own FTS-only
baseline under one fixed harness.

Constraints: pure Go, no CGO (`modernc.org/sqlite` stays — libSQL embedded
requires CGO and the new Turso engine lacks full FTS5), no third-party vector
library, prompt-cache prefix must stay stable (memory block already sits at the
tail), all budgets in Unicode code points, timestamps in unix micros.

## Goals / Non-Goals

**Goals:**

- Automatic fact extraction from conversations (ADD-only, one LLM call per batch).
- Hybrid recall: vector + BM25 + entity signals fused with RRF, degrading
  gracefully to today's behavior when embedding is unconfigured.
- Time-aware retrieval output (event + recorded timestamps rendered per hit).
- A reproducible LoCoMo harness proving the uplift (FTS-only vs hybrid A-B,
  per-category scores, Mem0-aligned judge prompt).

**Non-Goals:**

- Graph memory (Mem0g-style triplets) — future change, gated on measured need.
- A-MEM link generation / memory evolution; MIRIX-style memory partitions.
- ANN indexes (HNSW/DiskANN) or driver swaps — linear cosine over ≤ tens of
  thousands of BLOBs is microseconds-to-milliseconds in Go.
- Changing snapshot/PINNED/INDEX assembly or the prompt-cache layout.
- UPDATE/DELETE resolution at write time (Mem0 v1 paper design) — superseded by
  ADD-only + curation cleanup + time-aware retrieval.

## Decisions

### D1: ADD-only extraction, curation as the dedup backstop

One LLM call per ingest batch returns strict JSON facts; every fact becomes a
new entry (unique name = slug + short ULID suffix). No write-time UPDATE/DELETE
tool-call loop. Rationale: Mem0 v3 abandoned write-time reconciliation and
scored higher; our curation engine (Jaccard ≥ 0.7 clustering + judge merge)
already exists to absorb the accumulation, which is precisely the capability
Mem0 keeps proprietary. Alternative considered: the original ADD/UPDATE/DELETE/
NOOP pipeline — rejected: 2-3× LLM calls per write, higher complexity, lower
published score.

### D2: Embedding = OpenAI-compatible HTTP client; nil = degrade

`internal/embedding.Client` (`Embed(ctx, texts) ([][]float32, error)`) with one
HTTP implementation. Default config targets local Ollama
(`http://127.0.0.1:11434/v1`, `qwen3-embedding:0.6b`; `bge-m3` equally valid) —
Chinese-friendly, 639MB, and the same protocol reaches any cloud provider.
Unconfigured (`base_url` or `model` empty) → nil client → vector signal absent,
retrieval continues on BM25+entity. Mirrors the curation worker's inert mode.
Alternative considered: pure-Go local inference (hugot/spago) — rejected:
immature, heavy, and Ollama is already the local-inference standard.

### D3: Vectors in a side table, scanned in Go

`memory_embeddings(entry_name PK, model, dims, vec BLOB, updated_at)`; float32
little-endian. Cosine top-k is a Go loop over all rows. Model change detection:
rows whose `model` differs from config are treated as missing and re-embedded
by the backfill sweep. Side table (not a column) keeps `memory_entries`, its
FTS triggers, and curation untouched, and makes full re-embeds a `DELETE`.
Embedding writes are async via a single background goroutine (usage-logger D8
pattern): entry visibility never waits on Ollama; missing vectors are backfilled
at startup and on write. Alternatives considered: libSQL native vector (CGO or
remote-only in Go — violates constraints), sqlite-vec (extension/CGO/wasm — new
driver risk), storing vectors in `memory_entries` (couples lifecycles).

### D4: Three-signal RRF fusion

Signals per query: (1) vector cosine rank; (2) FTS5 BM25 rank (existing
`ORDER BY rank` path, trigram tokenizer already CJK-capable); (3) entity match
rank (query tokenized with the shared sessionsearch tokenizer, exact-match
count against `memory_entities.entity_norm`). Fusion:
`score(e) = Σ_signals 1/(60 + rank_signal(e))`, descending; k=60 is the
standard RRF constant, no tuned weights to maintain. Absent signals simply drop
out of the sum. Rationale: RRF is the published default for hybrid search, is
rank-based (no score normalization across heterogeneous signals), and Mem0 v3
does not disclose its fusion — RRF is the reproducible choice.

### D5: Time-aware rendering, not time-aware reranking (v1)

Extraction stamps `event_date` when the batch content implies one (session
datetime + relative expressions resolved by the extractor LLM); `fact_source`
records provenance (`user|agent|extraction`). Retrieval output renders
`[event: 2023-05] [recorded: 2026-07-16]` per hit so the answering model does
temporal disambiguation itself — the Mem0 paper attributes OpenAI-memory's
temporal collapse to missing timestamps, and ByteRover's temporal lead to
dated instances. Query-intent-conditioned reranking (current/past/upcoming) is
deferred until the harness shows the rendering alone is insufficient.

### D6: Pipeline trigger and model

`memory.pipeline.enabled: true` by default (user decision). Production trigger:
session-end (loop teardown) over the messages accrued since the last ingest,
fire-and-forget with WARN on failure — never blocks or fails the session.
`memory.pipeline.extract_model` defaults to `memory.curation.judge_model`
(small/fast tier); calls go through the existing `curation.NewProviderCaller`
`provider:model` mechanism. JSON parsing reuses the judge's fence/prose-tolerant
`extractJSON` approach. The bench invokes the pipeline Go API directly with the
LoCoMo session date injected.

### D7: Bench is a separate binary with A-B and resume

`cmd/locomo-bench` (not wired into serve): per conversation, create a temp
SQLite store, ingest sessions through the pipeline, answer each question
single-pass from `retrieval.Search` top_k (large budget, default 50, flaggable
up to 200), judge with an LLM prompt aligned to mem0ai/memory-benchmarks,
aggregate per LoCoMo category (adversarial category excluded, per Mem0
convention). `--retrieval fts|hybrid` switches the retrieval backend for the
A-B. Run state is JSONL (resumable; API cost control via `--conversations`
/`--questions` sampling). Credentials only via env
(`LOCOMO_BASE_URL/LOCOMO_API_KEY/LOCOMO_MODEL`, plus `memory.embedding.*` for
vectors); never logged or committed — the anthropic-compatible DeepSeek endpoint
is the initially intended target.

## Risks / Trade-offs

- [ADD-only floods the store between curation passes] → curation pressure
  trigger already fires on `memory_write`; pipeline writes go through the same
  `OnWrite` hook. Bench stores are throwaway. If manifest pressure appears in
  production, `entry_count_high` is hot-reloadable.
- [Ollama down / embedding endpoint flaky] → async write-behind + startup
  backfill; retrieval degrades per-signal, never errors out. WARN-level logs only.
- [Extractor JSON malformed] → tolerant extraction + per-fact validation
  (existing `CheckEntryContent`/`CheckTrigger` budgets); a bad batch is a WARN
  and a no-op, mirroring curation fail-safe.
- [LoCoMo absolute scores not comparable to vendor claims] → success criterion
  is A-B uplift under our fixed harness plus the paper-range absolute floor
  (J ≥ 66%); 80+ is stretch. Judge prompt pinned in-repo for reproducibility.
- [Pipeline default-on adds an LLM call per session] → small-model default,
  single call per batch, config kill-switch; extraction skips trivially short
  batches.
- [Entity table growth] → rows are tiny (two short strings); cascade deletes on
  entry delete/merge keep it bounded by entry count.

## Migration Plan

Migration v8 is additive (two `ALTER TABLE ... ADD COLUMN`, two `CREATE TABLE`,
one index) — existing rows valid, downgrade-safe to read. No data backfill
required; embeddings/entities populate lazily. Rollback = revert binary; new
tables are ignored by v7 code.

## Open Questions

- Whether entity embeddings (Mem0 v3 "entities are embedded") add measurable
  uplift over exact normalized match — defer until the harness can measure it.
- Whether `MemorySearch`'s default `top_k=8` should rise once hybrid quality is
  proven (context-budget trade-off for interactive sessions).
