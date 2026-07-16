# Tasks: memory-hybrid-retrieval-locomo

## 1. Schema & store foundation

- [x] 1.1 Migration v8 in `internal/store/sqlite/migrations.go`: `ALTER TABLE memory_entries ADD COLUMN event_date INTEGER` + `ADD COLUMN fact_source TEXT NOT NULL DEFAULT ''`; create `memory_embeddings` and `memory_entities` tables + `idx_memory_entities_norm`; migration test
- [x] 1.2 Extend `memory.Entry` + `EntryStore` scan/upsert for the new columns (preserve upsert keep-fields semantics); unit tests
- [x] 1.3 `EntryStore.Delete`/`Merge` cascade deletes to `memory_embeddings`/`memory_entities` (same transaction); unit tests
- [x] 1.4 Entity accessors on the store: `PutEntities(name, []string)` (normalized), `EntityMatchRanks(tokens)`; unit tests incl. CJK normalization

## 2. Embedding infrastructure

- [x] 2.1 Config: `memory.embedding.{base_url,model,api_key,dimensions,timeout_seconds}` structs + defaults + validation (restart-only); docs in config comments
- [x] 2.2 `internal/embedding`: `Client` interface + OpenAI-compatible HTTP implementation (batch, `dimensions` passthrough, key never logged); httptest unit tests
- [x] 2.3 Vector codec + math: float32 LE BLOB encode/decode, cosine top-k scan; unit tests with known fixtures
- [x] 2.4 Write-behind embedder goroutine (D8 usage-logger pattern) + startup/backfill sweep (missing or model-mismatched rows); unit tests with fake client
- [x] 2.5 Wire into `cmd_serve.go`: construct client from config (nil when unconfigured → INFO), hook entry writes to enqueue embeds

## 3. Hybrid retrieval

- [x] 3.1 `internal/memory/retrieval`: three-signal search (vector cosine, FTS5 BM25 via existing plan/fallback, entity match) + RRF fusion with deterministic tiebreak; table-driven unit tests incl. full degradation matrix
- [x] 3.2 Upgrade `MemorySearch` tool: `top_k` (default 8, cap 50, `limit` alias), hybrid backend, `[event:]`/`[recorded:]` rendering; keep read-only/parallel-safe; English description; unit tests
- [x] 3.3 Degradation behavior tests: no embedding client ≡ BM25+entity; no entities ≡ current FTS behavior byte-compatible

## 4. Extraction pipeline

- [ ] 4.1 `AdapterGeneration`-style prompt in `internal/prompt`: extraction system+user prompts (facts+entities+event_date strict JSON, agent facts first-class, session date injection)
- [ ] 4.2 `internal/memory/pipeline`: batch → one LLM call (reuse `curation.NewProviderCaller`) → tolerant JSON parse → per-fact validation (existing budget checks) → ADD-only writes (slug+ULID names, fact_source/event_date/entities) → curation `OnWrite` hook; unit tests with fake caller
- [ ] 4.3 Config: `memory.pipeline.{enabled(default true), extract_model(default=judge_model)}` + inert mode when provider missing
- [ ] 4.4 Session-end trigger in the loop/session teardown path: async, WARN on failure, skip trivial batches; integration test with fake provider
- [ ] 4.5 real_e2e smoke: pipeline extraction against recorded fixtures (record/replay JSONL, follow memory_test.go pattern)

## 5. LoCoMo bench

- [ ] 5.1 `cmd/locomo-bench` skeleton: dataset loader (LoCoMo JSON), flags (`--data --run-dir --conversations --questions --top-k --retrieval fts|hybrid`), env credentials (`LOCOMO_BASE_URL/API_KEY/MODEL`), key-hygiene guard
- [ ] 5.2 Ingest stage: per-conversation temp store → pipeline per session with session date
- [ ] 5.3 Answer stage: single-pass answer prompt over top-k retrieval results (timestamps rendered); JSONL artifacts + resume
- [ ] 5.4 Judge stage: LLM-as-a-Judge prompt aligned with mem0ai/memory-benchmarks; per-category + overall report (adversarial excluded)
- [ ] 5.5 Baseline run (sampled, `--retrieval fts`) then hybrid run; record A-B uplift in change notes; iterate prompts/top_k until absolute J ≥ 66% on the sample or blockers documented

## 6. Hardening & docs

- [ ] 6.1 `golangci-lint run` clean; `TestLocalToolDescriptionsAreEnglish` passes
- [ ] 6.2 Update CLAUDE.md memory subsystem section (pipeline, embedding, hybrid retrieval, bench)
- [ ] 6.3 Full-suite `go test ./...` + existing real_e2e memory/curation tests unaffected
