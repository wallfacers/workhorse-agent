## Why

The MVP shipped without any cross-session memory: every conversation starts from a blank slate, and there is no way to recall what a prior session discussed except by reading the raw `messages` table. Agents can already follow instructions inside a session, but cannot accumulate user preferences (e.g. "always use pnpm") nor cite a past decision ("we agreed to drop CGO last week"). Hermes's two lower layers — a small, deliberately constrained Markdown memory loaded as a system-prompt snapshot, and an FTS5 full-text index over historical messages — solve both problems with low blast radius and align cleanly with our existing `prompt` and `store` packages.

## What Changes

- Introduce **L1 Prompt Memory**: profile-scoped `MEMORY.md` (≤ 2200 chars) and `USER.md` (≤ 1375 chars) under `~/.workhorse-agent/memories/`, loaded once at session start as an immutable snapshot and injected into the system prompt by prepending a formatted memory block at the agent loop's call site (the `internal/prompt` package's `BuildSystemPrompt` signature is unchanged — see design.md D3).
- Add two new built-in tools, `memory_read` and `memory_write`, that respect the char limits, use file locking for concurrent safety, and **do not** affect the current session's already-injected snapshot — writes take effect at the next session start (preserves Anthropic prompt-cache hit rate).
- Introduce **L2 Session Archive**: a `messages_fts` FTS5 virtual table mirroring `messages.content_json`, populated via SQLite triggers; backfilled on first migration.
- Add a new built-in tool, `session_search`, that runs FTS5 MATCH queries and returns raw matched messages plus ±N message context. **No LLM summarization** — results are returned verbatim, matching Hermes's "zero LLM cost" search semantics.
- Extend `pathguard` to recognize the memories directory as a sanctioned write target (otherwise `memory_write` is blocked by existing traversal protection).
- Add a migration that creates the FTS table, triggers, and indexes; verify FTS5 is compiled into `modernc.org/sqlite` at startup and fail fast if not.

## Capabilities

### New Capabilities
- `prompt-memory`: lifecycle of `MEMORY.md` / `USER.md` (load, snapshot, write, char-limit enforcement, delayed-effect semantics).
- `session-archive`: FTS5 index over historical messages, `session_search` tool surface, tokenizer/CJK fallback policy.

### Modified Capabilities
<!-- None. New tools register through the existing tool-system without changing its spec-level requirements; the FTS index sits alongside messages without altering session-management semantics. -->

## Impact

- **Code**: new `internal/memory/` package (snapshot loader, file-locked writer, prompt-block formatter); new `internal/tools/memorytool/` and `internal/tools/sessionsearch/`; `internal/agent/loop.go:371` is the only `prompt.BuildSystemPrompt` call site touched (signature **unchanged** — concatenation happens at the call site, see design.md D3); `internal/store/sqlite/migrations.go` is refactored to introduce a versioned migration framework and adds an FTS5 migration; `internal/store/sqlite/funcs.go` is new (custom `extract_text` SQLite function); `internal/tools/pathguard` is refactored to extract a `resolver` type and adds memory-scoped helpers; `*sqlite.Store` gains a `DB() *sql.DB` accessor consumed only by `sessionsearch`.
- **Prerequisite refactor (included in this change)**: today `internal/store/sqlite/migrations.go` carries a single `schema []string` despite a comment promising a versioned `migrationsByVersion`. This change builds that framework as task §1.3 before adding the FTS5 migration. Behavior on fresh installs is byte-identical to current.
- **Persistence**: new SQLite objects (`messages_fts`, three triggers, one backfill); existing tables unchanged. Migration is forward-only; rollback story is "drop the FTS table" — the source-of-truth `messages` table is untouched.
- **Dependencies**: none added — modernc.org/sqlite v1.34.5 ships FTS5 in its default build; a startup probe will assert this.
- **Config**: optional `memory.dir`, `memory.memory_char_limit`, `memory.user_char_limit` keys under existing config schema (defaults match Hermes); validated at load time.
- **Out of scope**: L3 Skills auto-curation and L4 Honcho external memory are separate proposals.
