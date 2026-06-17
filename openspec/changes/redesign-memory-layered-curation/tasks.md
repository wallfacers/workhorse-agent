# Tasks

## 1. Storage & schema (SQLite)

- [ ] 1.1 Add `memory_entries` table migration (columns per design D1) via the
  versioned migration framework, forward-only, `IF NOT EXISTS`.
- [ ] 1.2 Add `memory_entries_fts` FTS5 virtual table + AFTER INSERT/UPDATE/DELETE
  sync triggers mirroring `(name, trigger, content)`; mirror the `messages_fts`
  migration shape. Backfill from any rows present.
- [ ] 1.3 Add `memory_curation_lease(id=1, holder, expires_at, heartbeat_at)` table
  migration; seed a single row.
- [ ] 1.4 Add store accessors for entry CRUD (insert/upsert/get-by-name/delete/list)
  and FTS query, all transactional; reuse `DB() *sql.DB` accessor.

## 2. Entry store & two-layer snapshot

- [ ] 2.1 Reshape `internal/memory` `Snapshot` to hold assembled `(pinnedContent,
  manifestLines)` instead of `(MemoryMD, UserMD)`; keep it immutable.
- [ ] 2.2 Implement `Loader.Load()` to read entries from the store and assemble the
  snapshot: pinned full content (name-sorted), non-pinned manifest lines
  (`score desc, name asc`).
- [ ] 2.3 Rewrite `Block()` to render the `PINNED:` / `---` / `INDEX:` regions
  inside `<memory>…</memory>` with byte-stable layout; empty store → "".
- [ ] 2.4 Implement manifest budget handling: include highest-scored lines up to
  `manifest_budget_chars`, append the `… N more …` line, `slog.Warn` the dropped
  count. Never silently drop.
- [ ] 2.5 Implement pinned budget enforcement at write/pin time
  (`pinned_budget_exceeded`).
- [ ] 2.6 Implement per-entry `entry_content_max_chars` enforcement (code points)
  with `memory_too_large`.
- [ ] 2.7 Confirm injection at the `internal/agent/loop.go` call site is unchanged
  in signature and ordering (`base → environment → instructions → memory`).

## 3. Tools

- [ ] 3.1 Reshape `memory_write` to single-entry upsert/append (schema rejects
  arrays); transactional; `next_session_effective: true`.
- [ ] 3.2 Reshape `memory_read` to read one entry by name from the store; MUST NOT
  bump usage score.
- [ ] 3.3 Add `LoadMemory{name}` (≈ port of `skills/loadtool.go`), read-only +
  parallel-safe, returning full content; record an idempotent best-effort usage hit
  (`hit_count++`, `last_used_at`) per design D8.
- [ ] 3.4 Add `MemorySearch{query, limit?}` over `memory_entries_fts`, reusing the
  `sessionsearch` CJK-trigram + LIKE fallback; return `{name, trigger, snippet}`.
- [ ] 3.5 Add `memory_delete{name}` (transactional).
- [ ] 3.6 Add `memory_merge{names, into}` (atomic write+delete in one transaction).
- [ ] 3.7 Register all tools through the existing registry; gate by `allowed_tools`;
  set read-only/parallel flags correctly.
- [ ] 3.8 Implement the `memory export` artifact path (CLI command and/or HTTP
  endpoint) that renders all entries to a human-readable markdown file for
  inspection/git backup; resolve the output path through `pathguard`. (Docs in 8.2.)
- [ ] 3.9 Implement the usage-logger goroutine + buffered channel (design D8) that
  `LoadMemory`/`MemorySearch` enqueue to; drop-on-full with DEBUG log.

## 4. Migration (flat files → entries)

- [ ] 4.1 One-time idempotent startup migration with a marker file; skip when marker
  present.
- [ ] 4.2 `USER.md` → single pinned `category=user` `durability=evergreen` entry.
- [ ] 4.3 `MEMORY.md` → split on markdown sections into `volatile` entries; trigger
  defaults to first sentence; non-splittable block → single entry.
- [ ] 4.4 Copy legacy files to `memories/legacy/`; do not delete originals.

## 5. Curation engine

- [ ] 5.1 New `internal/memory/curation` package skeleton (scorer, lease, worker).
- [ ] 5.2 Deterministic scorer: implement the `norm`/`recency`/`age_penalty`/
  `volatility_penalty` functions with the documented formulas (design D5), combine
  with configurable weights; exclude pinned; output ranked eviction candidates.
- [ ] 5.2a Near-duplicate clustering: FTS pre-filter → exact character-trigram
  Jaccard ≥ 0.7 → union-find clusters (design D5).
- [ ] 5.3 Pressure trigger hook on successful write (count/size/time water lines),
  idempotent debounced enqueue; off the request hot path.
- [ ] 5.4 Leader lease: CAS acquire (`WHERE expires_at < now()`), heartbeat renewal,
  TTL takeover, in-process mutex backstop.
- [ ] 5.5 Background maintenance worker: acquire lease → run scorer → LLM judgment
  step → emit `memory_delete`/`memory_merge`; fail-safe (WARN + no-op on error).
- [ ] 5.6 LLM judgment driver: short model call (configurable `judge_model`,
  candidate cap `max_candidates_per_pass`) given candidates + clusters →
  keep/evict/merge decisions, emitting `memory_delete`/`memory_merge`.
- [ ] 5.6a Design the curation judge prompt template (system/user) that elicits a
  structured keep/evict/merge decision; add it under `internal/prompt`.

## 6. Configuration

- [ ] 6.1 Add the `memory` config block — `pinned_budget_chars`,
  `manifest_budget_chars`, `entry_content_max_chars`, `trigger_max_chars`, and
  `curation.{entry_count_high, min_interval_minutes, lease_ttl_seconds, judge_model,
  max_candidates_per_pass, weights}` — with defaults and load-time validation.
- [ ] 6.2 Hot-reload subset = `curation.entry_count_high`,
  `curation.min_interval_minutes`, `curation.lease_ttl_seconds` only; all other keys
  (budgets, per-entry limits, `judge_model`, `max_candidates_per_pass`, `weights`)
  are restart-only (WARN + ignore on reload).

## 7. Tests (no tests = not done)

- [ ] 7.1 Snapshot assembly: PINNED/INDEX ordering and byte-stability; empty store →
  no block.
- [ ] 7.2 Budgets: pinned rejection, manifest overflow with explicit `… N more …`
  line + WARN, per-entry `memory_too_large`, CJK code-point counting.
- [ ] 7.3 Tools: `memory_write` single-entry/array-rejection/upsert/append;
  `memory_read` no-hit; `LoadMemory` hit idempotent + read-only honest;
  `MemorySearch` MATCH + CJK fallback; `memory_merge` atomic rollback.
- [ ] 7.4 Curation: scorer determinism, pinned-exempt, volatile vs evergreen age,
  trigger debounce.
- [ ] 7.5 Multi-process: lease CAS (one winner), TTL takeover, concurrent-write
  transaction safety, crash release.
- [ ] 7.6 Migration: USER→pinned, MEMORY split, idempotent re-run, legacy backup.
- [ ] 7.7 Curation flow with a **mocked** LLM judge: scorer → judge → mutations is
  correct and deterministic (CI-safe, no real model call).
- [ ] 7.7a (optional, manual, not in CI) Real-e2e with a live judge model: recall
  does not regress after a curation pass; gated like existing real-e2e tests.
- [ ] 7.8 `golangci-lint run` clean; `TestLocalToolDescriptionsAreEnglish` passes for
  new tools.

## 8. Docs

- [ ] 8.1 Update `CLAUDE.md` "Memory subsystem" section to describe the per-entry
  layered store + curation engine.
- [ ] 8.2 Add a `memory export` command (and brief docs) for human-readable
  inspection/git backup of entries.
