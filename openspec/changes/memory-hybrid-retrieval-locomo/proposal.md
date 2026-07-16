# Proposal: memory-hybrid-retrieval-locomo

## Why

The L1 prompt-memory store only fills when the LLM explicitly calls `memory_write`, and retrieval is FTS5-trigram-only — so the system cannot automatically distill facts from long conversations, and semantic/temporal recall is far below the published state of the art (Mem0 v3 reports 92.5 on LoCoMo; the original Mem0 paper 66-68 vs. our untested baseline). Porting the proven, simple parts of the Mem0 v3 algorithm (ADD-only extraction, multi-signal retrieval, time-aware ranking) onto our existing store + curation engine closes that gap with bounded engineering risk.

## What Changes

- New pure-Go embedding client (`internal/embedding`) speaking the OpenAI-compatible `/v1/embeddings` protocol (default target: local Ollama with `qwen3-embedding` / `bge-m3`; any cloud endpoint with the same protocol works). Unconfigured → nil client → every vector path degrades to today's FTS5 behavior (fail-safe, mirrors curation's inert mode).
- Migration v8: `memory_entries` gains `event_date` (nullable, unix micros) and `fact_source` (`''|user|agent|extraction`) columns; new side tables `memory_embeddings` (float32 LE BLOB per entry, rebuildable on model change) and `memory_entities` (normalized entity → entry index).
- ADD-only extraction pipeline (`internal/memory/pipeline`): one LLM call per message batch extracts facts + entities + event dates as strict JSON; each fact is stored as a new entry (never overwrites), entities indexed, embedding enqueued async. Enabled by default (`memory.pipeline.enabled: true`), model defaults to `memory.curation.judge_model`. Redundancy from ADD-only accumulation is handled by the existing curation engine (trigram clustering + LLM judge merge/evict) — no new dedup machinery.
- Hybrid retrieval: three parallel signals — vector cosine (Go-side, BLOB scan), FTS5 BM25 (existing `ORDER BY rank`), entity exact-match — fused with Reciprocal Rank Fusion. `MemorySearch` gains an optional `top_k` (default 8, max 50) and renders `[event: …] [recorded: …]` timestamps per hit; a Go API (`retrieval.Search`) serves large-budget callers.
- New `cmd/locomo-bench` harness: ingest LoCoMo conversations through the pipeline into a throwaway store, answer questions single-pass from retrieved memories, score with LLM-as-a-Judge (prompt aligned with mem0ai/memory-benchmarks), report per category, support sampling / resume / `--retrieval fts|hybrid` A-B. Credentials via env only, never logged.
- No third-party vector library, no CGO, no driver change: `modernc.org/sqlite` stays; cosine top-k over a few thousand BLOBs is microseconds in Go.

## Capabilities

### New Capabilities

- `memory-embedding`: embedding client protocol, config (`memory.embedding.*`), vector side-table lifecycle (async write-behind, startup backfill, model-change rebuild), degradation rules.
- `memory-extraction`: ADD-only fact extraction pipeline — triggers, prompt/JSON contract, entry naming, fact_source/event_date semantics, failure isolation, config (`memory.pipeline.*`).
- `memory-hybrid-retrieval`: three-signal RRF fusion, degradation matrix (vector+BM25+entity → BM25+entity → BM25-only ≡ status quo), time-aware result rendering, `retrieval.Search` API contract.
- `locomo-bench`: benchmark harness CLI — dataset ingestion, single-pass answering, judge scoring, per-category reporting, sampling/resume, credential handling.

### Modified Capabilities

- `prompt-memory`: `MemorySearch` gains `top_k` and hybrid backend with timestamped results; entry model gains `event_date`/`fact_source`; `memory_delete`/`memory_merge` cascade to embedding/entity side tables.

## Impact

- **Code**: new `internal/embedding`, `internal/memory/pipeline`, `internal/memory/retrieval` (or extension of `internal/tools/memorytool`), `cmd/locomo-bench`; migration in `internal/store/sqlite/migrations.go` (v8); `EntryStore` cascade updates; config structs + defaults in `internal/config`; wiring in `cmd/workhorse-agent/cmd_serve.go`.
- **Config**: new `memory.embedding.{base_url,model,api_key,dimensions,timeout_seconds}` and `memory.pipeline.{enabled,extract_model}` keys (restart-only, consistent with other `memory.*` keys except the existing hot-reload trio).
- **Runtime cost**: pipeline adds one small-model LLM call per session batch (default on); embedding adds local Ollama calls (or none when unconfigured).
- **Dependencies**: none added. No CGO. No vector DB.
- **Compatibility**: existing entries keep working (new columns nullable/defaulted); with no embedding config and pipeline disabled, behavior is byte-identical to today except the additive `MemorySearch` schema field.
