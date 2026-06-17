## Why

L1 Prompt Memory shipped as two flat files (`MEMORY.md` ≤ 2200, `USER.md` ≤ 1375
code points) loaded **whole** into every session's system prompt. Two structural
problems surface as memory accumulates:

1. **Prompt cost scales with total memory.** Every fact ever written sits in the
   cache prefix of every session, whether or not it is relevant to the current
   task. The only governor is a hard char cap that **rejects new writes** once
   full — that is truncation, not maintenance. The agent cannot accumulate more
   than ~2200 chars of durable knowledge, period.

2. **No curation.** Near-duplicate facts, stale environment quirks (the kind that
   carry `file:line` citations and rot quickly), and one-off observations never
   get merged or evicted. The single flat file just grows until it hits the cap.

The `skills` subsystem already demonstrates the fix for problem 1: a lightweight
**manifest** (name + trigger) lives in the prompt, and full content is pulled
**on demand** via a load tool. This change applies that two-layer pattern to
memory and adds the missing curation engine for problem 2 — deterministic
pressure-triggered eviction scoring (LRU/LFU/age/durability, Redis-style) feeding
an LLM judgment step that merges near-duplicates and evicts low-value entries,
run by a background worker that is safe under multi-process operation via a
DB leader lease.

## What Changes

- **Replace the two flat files with a single per-entry store.** Memory becomes a
  set of entries in SQLite (`memory_entries`), one row per fact, with metadata
  columns (`pinned`, `durability`, `category`, `hit_count`, `last_used_at`, …).
  There is **one** memory mechanism, not flat-files-plus-entries.
- **Two-layer prompt assembly.** At session start the snapshot contains (a) the
  full content of all `pinned` entries and (b) a lightweight **manifest** of all
  non-pinned entries as `name — trigger` lines. Full non-pinned content is loaded
  on demand. Prompt cost is now ≈ pinned content + one short line per entry,
  decoupling prompt budget from total memory size.
- **Hybrid recall.** `LoadMemory` (by name, after a manifest hit) plus
  `MemorySearch` (FTS5 over a `memory_entries_fts` virtual table, reusing the
  `session-archive` tokenizer/CJK-trigram approach) as the backup when the
  manifest does not surface what is needed.
- **Curation engine (new capability).** Writes past a water line enqueue a
  background curation pass: a deterministic scorer selects eviction candidates
  and near-duplicate clusters; an LLM step decides keep/evict/merge and emits
  `memory_delete`/`memory_merge`. A DB leader lease guarantees exactly one
  process curates at a time; entry mutations are DB transactions.
- **Per-entry limits replace the whole-file cap.** Each entry's content has a
  char limit; total store size is governed by curation, not by rejecting writes.
  Two prompt budgets (pinned, manifest) bound what reaches the system prompt;
  manifest overflow is surfaced explicitly (never silently truncated).
- **One-time migration** of existing `MEMORY.md`/`USER.md` content into entries;
  legacy files are backed up, not deleted.

## Capabilities

### New Capabilities
- `memory-curation`: pressure-triggered curation lifecycle — deterministic
  eviction scoring, LLM merge/evict judgment, background worker, DB leader lease
  for multi-process safety, failure isolation.

### Modified Capabilities
- `prompt-memory`: replaces the two-flat-file model with a per-entry SQLite store,
  pinned/manifest two-layer prompt assembly, `LoadMemory`/`MemorySearch` recall,
  `memory_write`/`memory_delete`/`memory_merge` entry tools, per-entry char limits
  and pinned/manifest budgets, and a flat-file→entry migration. The immutable
  per-session snapshot and delayed-effect (next-session) write semantics are
  preserved.
- `configuration`: adds the `memory.*` config block (budgets, per-entry limit,
  curation water lines, lease TTL, scoring weights).

## Impact

- **Code**: `internal/memory/` gains an entry store (SQLite-backed), a two-layer
  snapshot assembler (pinned + manifest), and per-entry limit enforcement;
  retains the immutable-snapshot and atomic-write invariants. New
  `internal/memory/curation/` package (scorer, LLM judgment driver, background
  worker, leader lease). `internal/tools/memorytool/` gains `LoadMemory`,
  `MemorySearch`, `memory_delete`, `memory_merge` and reshapes `memory_write`/
  `memory_read` around entries. `internal/store/sqlite/` adds the
  `memory_entries` table, `memory_entries_fts` virtual table + sync triggers, and
  the `memory_curation_lease` table via the versioned migration framework.
  `internal/agent/loop.go` injection call site is unchanged in signature; the
  `<memory>` block layout changes to PINNED/INDEX sections.
- **Persistence**: new SQLite objects only; the source-of-truth for memory moves
  from files to `memory_entries`. Forward-only migration; legacy files preserved
  under `memories/legacy/`.
- **Prompt cache**: the snapshot stays immutable per session and is assembled
  base-prefix-stable (pinned sorted by name, then manifest sorted by score then
  name); mid-session writes still take effect only at the next session start.
- **Config**: new `memory.*` keys with Hermes-compatible defaults; lease TTL and
  curation water lines are candidates for the hot-reload subset.
- **Out of scope**: semantic/embedding recall (FTS only here); cross-profile or
  external (Honcho-style) memory; UI for browsing entries beyond an `export`/
  inspect command.
