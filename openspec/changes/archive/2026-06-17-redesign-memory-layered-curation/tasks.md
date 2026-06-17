# Tasks

## 1. Storage & schema (SQLite)

- [x] 1.1 Add `memory_entries` table migration (columns per design D1) via the
  versioned migration framework, forward-only, `IF NOT EXISTS`.
- [x] 1.2 Add `memory_entries_fts` FTS5 virtual table + AFTER INSERT/UPDATE/DELETE
  sync triggers mirroring `(name, trigger, content)`; mirror the `messages_fts`
  migration shape. Backfill from any rows present.
- [x] 1.3 Add `memory_curation_lease(id=1, holder, expires_at, heartbeat_at)` table
  migration; seed a single row.
- [x] 1.4 Add store accessors for entry CRUD (insert/upsert/get-by-name/delete/list)
  and FTS query, all transactional; reuse `DB() *sql.DB` accessor.

## 2. Entry store & two-layer snapshot

- [x] 2.1 Reshape `internal/memory` `Snapshot` to hold assembled `(pinnedContent,
  manifestLines)` instead of `(MemoryMD, UserMD)`; keep it immutable.
- [x] 2.2 Implement `Loader.Load()` to read entries from the store and assemble the
  snapshot: pinned full content (name-sorted), non-pinned manifest lines
  (`score desc, name asc`).
- [x] 2.3 Rewrite `Block()` to render the `PINNED:` / `---` / `INDEX:` regions
  inside `<memory>…</memory>` with byte-stable layout; empty store → "".
- [x] 2.4 Implement manifest budget handling: include highest-scored lines up to
  `manifest_budget_chars`, append the `… N more …` line, `slog.Warn` the dropped
  count. Never silently drop.
- [x] 2.5 Implement pinned budget enforcement at write/pin time
  (`pinned_budget_exceeded`).
- [x] 2.6 Implement per-entry `entry_content_max_chars` enforcement (code points)
  with `memory_too_large`.
- [x] 2.7 Confirm injection at the `internal/agent/loop.go` call site is unchanged
  in signature and ordering (`base → environment → instructions → memory`).

## 3. Tools

- [x] 3.1 Reshape `memory_write` to single-entry upsert/append (schema rejects
  arrays); transactional; `next_session_effective: true`.
- [x] 3.2 Reshape `memory_read` to read one entry by name from the store; MUST NOT
  bump usage score.
- [x] 3.3 Add `LoadMemory{name}` (≈ port of `skills/loadtool.go`), read-only +
  parallel-safe, returning full content; record an idempotent best-effort usage hit
  (`hit_count++`, `last_used_at`) per design D8.
- [x] 3.4 Add `MemorySearch{query, limit?}` over `memory_entries_fts`, reusing the
  `sessionsearch` CJK-trigram + LIKE fallback; return `{name, trigger, snippet}`.
- [x] 3.5 Add `memory_delete{name}` (transactional).
- [x] 3.6 Add `memory_merge{names, into}` (atomic write+delete in one transaction).
- [x] 3.7 Register all tools through the existing registry; gate by `allowed_tools`;
  set read-only/parallel flags correctly.
- [x] 3.8 Implement the `memory export` artifact path (`workhorse-agent memory
  export [--out F]` CLI) that renders all entries to a human-readable markdown
  document for inspection/git backup; `--out` resolves through `pathguard`
  relative to the working directory (stdout when omitted). Renderer is a pure
  function (`memory.RenderExport`) with unit + CLI tests.
- [x] 3.9 Implement the usage-logger goroutine + buffered channel (design D8) that
  `LoadMemory`/`MemorySearch` enqueue to; drop-on-full with DEBUG log.

## 4. Migration (flat files → entries)

- [x] 4.1 One-time idempotent startup migration with a marker file; skip when marker
  present.
- [x] 4.2 `USER.md` → single pinned `category=user` `durability=evergreen` entry.
- [x] 4.3 `MEMORY.md` → split on markdown sections into `volatile` entries; trigger
  defaults to first sentence; non-splittable block → single entry.
- [x] 4.4 Copy legacy files to `memories/legacy/`; do not delete originals.

## 5. Curation engine

- [x] 5.1 New `internal/memory/curation` package skeleton (scorer, dedup, lease,
  judge, worker).
- [x] 5.2 Deterministic scorer: implement the `norm`/`recency`/`age_penalty`/
  `volatility_penalty` functions with the documented formulas (design D5), combine
  with configurable weights; exclude pinned; output ranked eviction candidates.
  (Also wired as `memory.Loader.ScoreFn` so manifest survival uses the same ranking.)
- [x] 5.2a Near-duplicate clustering: exact character-trigram Jaccard ≥ 0.7 →
  union-find clusters (design D5); accepts an optional candidate-pair pre-filter
  (the FTS5 pre-filter is supplied by the 5B runtime path).
- [x] 5.3 Pressure trigger hook on successful write (count + min-interval floor
  water lines), idempotent debounced enqueue (buffered(1) channel); off the
  request hot path (`memory_write` `OnWrite` → `Worker.Notify`).
- [x] 5.4 Leader lease: CAS acquire (`WHERE expires_at < now OR holder=self`),
  heartbeat renewal (ttl/3), TTL takeover, in-process mutex backstop.
- [x] 5.5 Background maintenance worker: acquire lease → run scorer → cluster →
  LLM judgment → apply `Delete`/`Merge`; fail-safe (WARN + no-op on error).
- [x] 5.6 LLM judgment driver: short model call (configurable `judge_model` via
  `NewProviderCaller`, candidate cap `max_candidates_per_pass`) given candidates +
  clusters → keep/evict/merge decisions, validated against the live store
  (pinned/unknown-name refusal, merge over-budget skip).
- [x] 5.6a Curation judge prompt template (system/user) eliciting a structured
  keep/evict/merge JSON decision; added under `internal/prompt`
  (`curation_judge.go`, IO-free, boundary-test compliant).

## 6. Configuration

- [x] 6.1 Add the `memory` config block — `pinned_budget_chars`,
  `manifest_budget_chars`, `entry_content_max_chars`, `trigger_max_chars`, and
  `curation.{entry_count_high, min_interval_minutes, lease_ttl_seconds, judge_model,
  max_candidates_per_pass, weights}` — with defaults and load-time validation.
- [x] 6.2 Hot-reload subset = `curation.entry_count_high`,
  `curation.min_interval_minutes`, `curation.lease_ttl_seconds` only (applied via
  `Worker.SetHotConfig` in `permReloader`); all other keys (budgets, per-entry
  limits, `judge_model`, `max_candidates_per_pass`, `weights`) are restart-only
  (`DiffReloadable` surfaces them under `memory` → WARN + ignore).

## 7. Tests (no tests = not done)

- [x] 7.1 Snapshot assembly: PINNED/INDEX ordering and byte-stability; empty store →
  no block. (`internal/memory/snapshot_test.go`)
- [x] 7.2 Budgets: pinned rejection, manifest overflow with explicit `… N more …`
  line + WARN, per-entry `memory_too_large`, CJK code-point counting.
  (`snapshot_test.go` overflow + `memorytool_test.go` write-time limits)
- [x] 7.3 Tools: `memory_write` single-entry/array-rejection/upsert/append;
  `memory_read` no-hit; `LoadMemory` hit idempotent + read-only honest;
  `MemorySearch` MATCH + CJK fallback; `memory_merge` atomic rollback.
  (`internal/tools/memorytool/memorytool_test.go`, 22 cases)
- [x] 7.4 Curation: scorer determinism, pinned-exempt, volatile vs evergreen age
  (4× ratio), tiebreak by name; worker floor + pressure water lines.
- [x] 7.5 Multi-process: lease CAS (one winner), TTL takeover, release-enables-
  immediate-takeover, in-process backstop. (Concurrent-write tx safety is covered
  by the existing entrystore/store transaction tests.)
- [x] 7.6 Migration: USER→pinned, MEMORY split, idempotent re-run, legacy backup.
  (`internal/memory/migrate_test.go`, 7 cases)
- [x] 7.7 Curation flow with a **mocked** LLM judge: scorer → judge → mutations is
  correct and deterministic (CI-safe, no real model call); includes fail-safe on
  bad/empty judge output and provider-call error, and pinned-evict refusal.
- [x] 7.7a (manual, not in CI) Real-e2e with a live judge model: recall does not
  regress after a curation pass (`test/real_e2e/curation_test.go`,
  `TestCuration_RecallDoesNotRegress`); build-tagged `real_e2e`, skips without
  `DASHSCOPE_API_KEY` like the other real-e2e tests. Also repaired the real_e2e
  harness (`helpers.go`) which the per-entry redesign had left uncompilable
  against the old memory/`memorytool` API.
- [x] 7.8 `golangci-lint run` clean on all touched packages (gosec G201 false
  positive in `memorytool/search.go` suppressed with justification);
  `TestLocalToolDescriptionsAreEnglish` passes (no new tools added). Note: a
  pre-existing G201/unusedwrite pair in `internal/tools/sessionsearch` (the
  session-archive change, out of scope here) is only surfaced by the newer local
  golangci-lint v1.64.8, not CI's pinned v1.62.0.

## 8. Docs

- [x] 8.1 Update `CLAUDE.md` "Memory subsystem" section to describe the per-entry
  layered store + curation engine (rewritten: store schema, two-layer snapshot,
  tools, curation engine, hot-reload subset, legacy migration).
- [x] 8.2 Add a `memory export` command (and brief docs) for human-readable
  inspection/git backup of entries (documented in CLAUDE.md + `memory --help`).
