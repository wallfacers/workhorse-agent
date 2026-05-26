## Context

`workhorse-agent` is a local single-user, multi-session AI agent server (Go 1.22+, `modernc.org/sqlite`, single binary). The MVP (archived 2026-05-24) shipped session/event persistence, a tool system, the `internal/prompt` package (extracted in commit `9dd1179`), and an `auto-compact` summarizer (`internal/agent/compaction.go`) built on the fast-model channel — but no cross-session memory of any kind.

Hermes (Python) ships a four-layer memory system. After analyzing both codebases, the lower two layers (Prompt Memory and Session Archive) map cleanly onto our existing seams:

- `internal/prompt/template.go` already renders system prompts and `internal/prompt/builtins.go` owns the `BuildSystemPrompt(base string)` function. The single call site at `internal/agent/loop.go:371` reads `l.SystemPromptBase` directly, so a memory block can be prepended to that base string at the call site without changing the prompt package's surface — see D3 for the final shape.
- `internal/store/sqlite/migrations.go` already runs forward-only migrations; adding an FTS5 virtual table and triggers is a single migration.
- `internal/tools/` has well-isolated tool packages (`bash`, `read`, `grep`, ...) — three new tool packages drop in alongside them.
- `internal/tools/pathguard` is the single chokepoint for filesystem access; extending its allowlist is sufficient to expose `~/.workhorse-agent/memories/` to `memory_write`.

The higher two Hermes layers (Skills auto-curation, Honcho external memory) have larger architectural impact and are deferred to separate changes.

## Goals / Non-Goals

**Goals:**

- Give every new session immediate access to a small, user-edited preferences/profile block via the system prompt, with zero per-turn cost.
- Let the agent write back to that block via tool calls without invalidating Anthropic prompt-cache prefixes mid-session.
- Make every historical message searchable via FTS5 with an `session_search` tool, returning raw matches plus context — no LLM postprocessing.
- Preserve the existing `Store` interface and all current invariants (events `idx` monotonicity, message immutability, ULID identifiers).
- Maintain CGO-free build (`modernc.org/sqlite` only).

**Non-Goals:**

- Skills auto-curation (L3) and Honcho integration (L4) — separate changes.
- Per-project / per-workdir memory scoping — memories are profile-scoped to mirror Hermes.
- Encryption of memory files at rest — out of scope for MVP; relies on filesystem permissions (0600).
- Hot reload of `MEMORY.md` mid-session — explicitly rejected to preserve prompt-cache hit rate.
- LLM summarization of `session_search` results — explicitly rejected to match Hermes "zero LLM cost" semantics and avoid coupling search latency to fast-model availability.
- Backfilling FTS for archived/soft-deleted sessions — only live `messages` rows are indexed.

## Decisions

### D0: Versioned migration framework is a prerequisite

**Choice**: Before any FTS5 work begins, replace the single `schema []string` in `internal/store/sqlite/migrations.go:20-86` with a versioned framework. Concretely:

```go
type Migration struct {
    Version int
    Up      []string  // statements applied in order inside a single tx
    Down    []string  // optional, applied in reverse for rollback
}

var migrationsByVersion = []Migration{
    {Version: 1, Up: v1Schema, Down: nil},
    {Version: 2, Up: v2MemoryFTS, Down: v2MemoryFTSDown},
}
```

`migrate()` reads `schema_version`, iterates `migrationsByVersion` filtering to entries whose `Version > current`, and applies each inside its own transaction, bumping `schema_version` per step. The existing v1 schema becomes the body of the v1 migration entry — no SQL change.

**Why now**: The existing `migrations.go` only carries one snapshot. The comment at L17-19 ("Future versions add migrations under `migrationsByVersion`") describes a framework that does not yet exist. The L1+L2 change is the first one that needs it, so we build it here. Without this, tasks 7.2-7.4 cannot land.

**Alternative**: Inline the new DDL into the v1 `schema` array and bump `currentSchemaVersion` to 2 with an `OR REPLACE` re-creation. Rejected — this works on fresh installs but causes existing installs to silently re-execute the v1 statements; over time the strategy is unmaintainable.

### D1: Profile-scoped memory (`~/.workhorse-agent/memories/`), not workdir-scoped

**Choice**: A single `MEMORY.md` + `USER.md` pair lives under the profile dir, shared across all sessions and all workdirs.

**Alternatives considered**:
- *Per-workdir*: separate `MEMORY.md` per project. Rejected — splits "user preferences" (which are global) from "project facts" (which already belong in `CLAUDE.md` checked into the repo). Hermes also chose profile-scoped.
- *Per-agent-type*: separate memory per agent definition. Rejected — agent definitions are configuration, not identity; memory should follow the user.

### D2: Frozen snapshot per session, no hot-reload

**Choice**: Memory files are read once when a session is created. The loaded text becomes part of the system prompt for the lifetime of that session. Mid-session `memory_write` calls update the disk file but **do not** mutate the in-memory snapshot.

**Why**: Anthropic prompt caching keys on byte-exact prefix match. If memory mutation changed the system prompt mid-conversation, every subsequent turn would miss the cache. The user-visible cost (a memory write taking effect "next session") is acceptable because writes are rare and the user almost always continues into a new session shortly after.

**Alternative**: Hot-reload with cache-bust would work correctly but burn tokens. Rejected.

### D3: New `internal/memory/` package, separate from `internal/prompt/`. Concatenation happens at the call site, NOT via a signature change.

**Choice**: A new `internal/memory/` package owns:
- `Snapshot` struct: `{MemoryMD string; UserMD string; LoadedAt time.Time}`
- `Loader.Load(profileDir string) (*Snapshot, error)`
- `Writer.Write(kind, content string) error` (handles char limits + flock)
- `Block(snapshot *Snapshot) string` — pure formatter that produces the delimited memory block (empty string when both fields are empty)

The current signature `prompt.BuildSystemPrompt(base string) string` is **preserved**. The agent loop (`internal/agent/loop.go:371`) prepends `memory.Block(session.snapshot)` to `l.SystemPromptBase` (which is `internal/agent/loop.go:69`) before calling `BuildSystemPrompt`. This avoids a breaking signature change with cross-package fanout and keeps memory rendering testable in isolation.

**Block ordering**: `USER.md` content appears BEFORE `MEMORY.md` content within the block. Rationale: `USER.md` describes stable user identity (role, preferences) that rarely changes; `MEMORY.md` is agent-curated facts that change more often. Placing the more-stable content earlier in the prefix maximizes Anthropic prompt-cache hit rate when `MEMORY.md` is updated between sessions.

**Block delimiters** (load-bearing — cache stability depends on byte-exact format):

```
<memory>
USER:
{userMD content verbatim}
---
MEMORY:
{memoryMD content verbatim}
</memory>
```

When `userMD` is empty, the `USER:\n...\n---\n` block is omitted entirely (but `MEMORY:` is still emitted if `memoryMD` non-empty). When `memoryMD` is empty and `userMD` non-empty, the `---\nMEMORY:\n` lines are omitted. When both are empty, `Block` returns the empty string and no `<memory>` tag is rendered.

**Why split this way**: `internal/prompt` was just extracted in commit `9dd1179` to keep it free of IO concerns. Pushing file IO and flock logic into it would regress that boundary. The prompt package stays pure rendering. Concatenation at the call site is one extra line in `loop.go` and zero churn in `prompt`.

### D4: Char limits enforced at write time, configurable but with Hermes-aligned defaults

**Choice**: Default `memory.memory_char_limit = 2200`, `memory.user_char_limit = 1375`. Counted in Unicode code points (`utf8.RuneCountInString`), not bytes. Writes exceeding the limit return a structured tool error (`error{code:"memory_too_large", limit:N, actual:M}`) without touching the file.

**Why**: Char-not-byte avoids penalizing CJK content. Hermes uses code-point counts for the same reason.

### D5: FTS5 via SQLite-native virtual table + triggers, not application-level

**Choice**: New migration creates:

```sql
CREATE VIRTUAL TABLE messages_fts USING fts5(
  content,
  content='messages',
  content_rowid='rowid',
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, extract_text(new.content_json));
END;
-- + AFTER DELETE, AFTER UPDATE
```

`extract_text()` is implemented in Go as a custom SQLite function registered at connection setup. It walks the `content_json` block array and concatenates the `text` field of each text-typed `ContentBlock`, ignoring tool_use/tool_result blocks (those would explode the index with JSON noise).

**Spike required before implementation**: there are no custom SQLite functions in the codebase today, and `modernc.org/sqlite`'s registration API differs from `mattn/go-sqlite3`. Reading the modernc source: the right path is `sqlite.MustRegisterScalarFunction("extract_text", 1, func(ctx *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) { ... })`, called once during `*sql.DB` setup before any connection is used. This is currently an unverified hypothesis. Task 7.0 (preceding 7.1) is a spike: write a 30-line throwaway that registers a trivial function and call it from a real query; only proceed to 7.1 once the registration mechanism is concretely confirmed. If it turns out registration is per-connection rather than per-DB, the connector setup hook (`sql.Register(...)` wrapper) must be used instead.

**Alternatives**:
- *App-level indexing*: write a Go indexer that listens to message inserts and writes to FTS. Rejected — race-prone, needs its own crash recovery, doubles the write path.
- *Use `content_json` directly as FTS content*: indexes raw JSON keys/escapes. Rejected — pollutes results and inflates index size.
- *Implement `extract_text` purely in SQL with `json_extract`*: tempting because it removes the custom-function dependency. Rejected — `content_json` is an array of heterogeneous blocks; selecting only `type='text'` blocks and concatenating their `text` fields requires a recursive CTE per trigger fire. Goes from one Go function call to a complex SQL fragment in three triggers, and the JSON contract is more naturally maintained in Go where `ContentBlock` is defined.

### D6: CJK handling via length-gated trigram fallback in the tool, not in tokenizer

**Choice**: Index with `unicode61 remove_diacritics 2` (no `tokenchars`, no `trigram`). The `session_search` tool, on receiving a query, classifies it:

- Query contains CJK characters AND has ≥3 CJK characters per run → rewrite to FTS5 trigram-style phrase queries (we wrap consecutive CJK runs into trigram MATCH clauses).
- Query < 3 CJK chars → fall back to `LIKE '%query%'` on `messages.content_json` text portions (slower but correct for short queries).
- Pure-ASCII query → pass through to FTS5 MATCH unchanged.

**Why**: `unicode61` alone splits CJK badly (each CJK char becomes its own token, drowning relevance). The Hermes approach of compiling a custom trigram tokenizer is cleaner but requires either a custom SQLite build or runtime tokenizer registration — `modernc.org/sqlite` does not expose tokenizer registration in pure Go. Doing the trigram synthesis in the query layer sidesteps that limitation.

**Algorithm** (reference for task breakdown):

```text
Input: query string Q
Output: FTS-eligible matchExpr OR fallback flag → LIKE plan

1. Tokenize Q into runs by character class:
     - run.kind = "ascii"  if all chars match [\p{L}\p{N}_'-]+ in ASCII range
     - run.kind = "cjk"    if all chars are in the CJK character set (see below)
     - run.kind = "ws"     for whitespace separators (dropped)
     - run.kind = "other"  for punctuation (treated as token boundary, dropped)

2. CJK character set is the union of these Unicode ranges/blocks:
     - CJK Unified Ideographs (U+4E00..U+9FFF)
     - CJK Unified Ideographs Extension A (U+3400..U+4DBF)
     - CJK Unified Ideographs Extension B (U+20000..U+2A6DF)
     - Hiragana (U+3040..U+309F), Katakana (U+30A0..U+30FF)
     - Hangul Syllables (U+AC00..U+D7AF), Hangul Jamo (U+1100..U+11FF)

3. For each run, build a sub-expression:
     - ascii run R          → FTS5 token literal: lower(R)
     - cjk run R, len(R)>=3 → AND-join of all consecutive 3-grams in R:
                               "abc" AND "bcd" AND "cde" AND ...
                               each wrapped in FTS5 double-quotes as a phrase
     - cjk run R, len(R)<3  → emit a "fallback marker" and abort FTS plan;
                               the whole query degrades to LIKE-only

4. If no fallback marker emitted:
     - Combine all sub-expressions with FTS5 AND
     - Execute via `messages_fts MATCH ?`
   Else:
     - Build LIKE plan: `WHERE content_text LIKE '%R1%' AND content_text LIKE '%R2%' ...`
     - Source `content_text` from `extract_text(content_json)` evaluated on the fly
     - Order by `messages.created_at DESC` (no rank available)
```

**Implementation breakdown**: this is the largest single piece of new logic. It must be split across tasks (see tasks.md §8): (a) Unicode range classifier and run tokenizer; (b) trigram synthesizer; (c) ASCII+CJK combiner producing the final FTS expression; (d) LIKE fallback executor; (e) test fixtures with CJK Ext-A characters and Hangul/Kana.

**Alternative**: Build a separate FTS table with `tokenize='trigram'`. Rejected for MVP — doubles index storage; deferrable if CJK recall proves insufficient. **Risk threshold**: if observed CJK recall during dogfooding falls below 80% of LIKE-baseline expected matches on representative queries, abandon the query-layer trigram approach in favor of a trigram-tokenized side-table.

### D7: `session_search` returns raw matches + context, no LLM summary

**Choice**: Each hit returns `{session_id, message_idx, role, snippet, context_before[], context_after[]}` where snippet uses SQLite `snippet()` (≤ 30 chars around match). `context_before/after` default to ±5 messages, capped at ±20.

**Why**: Matches Hermes's "zero LLM cost" search; avoids coupling search latency to fast-model availability; keeps the tool deterministic and easy to test.

### D8: Backfill at migration time, bounded one-time cost

**Choice**: The same migration that creates `messages_fts` runs:

```sql
INSERT INTO messages_fts(rowid, content)
SELECT rowid, extract_text(content_json) FROM messages;
```

For a fresh install this is a no-op. For an existing install it's bounded by current `messages` row count and runs inside the migration transaction.

**Risk**: Large existing DBs could make first startup slow. Mitigation: the migration logs progress every 10k rows; if this proves painful we can move backfill to a lazy background job in a follow-up.

### D9: FTS5 availability assertion at boot, fail fast

**Choice**: On startup, `internal/store/sqlite` runs `SELECT sqlite_compileoption_used('ENABLE_FTS5')` and refuses to start if it returns 0. The error message names the failing module and instructs the user to file an issue (since `modernc.org/sqlite` v1.34+ should always have FTS5).

**Why**: A silent degradation here would surface as "search returns nothing" much later; fail-fast is friendlier.

### D10: Tool surface — three new tools, registered through existing registry

| Tool | Inputs | Output | Char-limit enforced? |
|---|---|---|---|
| `memory_read` | `{kind: "memory" \| "user"}` | `{content, char_count, char_limit}` | n/a (read) |
| `memory_write` | `{kind, content, mode: "replace" \| "append"}` | `{accepted: bool, char_count, char_limit, next_session_effective: true}` | yes — pre-write |
| `session_search` | `{query, session_id?, scope?, limit?, context_before?, context_after?}` | `{hits: [...], truncated: bool}` | n/a |

All three go through `internal/tools/pathguard` (memory tools) or `internal/store` (search). They are subject to the existing `allowed_tools` filter on agents and skills.

### D11: pathguard refactor to introduce a containment-root abstraction

**Choice**: Current `pathguard` exposes `Resolve(workdir, path)` and `ResolveForWrite(workdir, path)` — both hard-code the containment root as the second-argument workdir. Refactor in two steps:

1. Extract an unexported `resolver` type with the containment root as state:
   ```go
   type resolver struct{ root string }
   func (r *resolver) resolve(path string, allowMissing bool) (string, error) { ... }
   ```
   Move the existing `canonicalise` + `assertInside` logic into `resolver.resolve`.
2. Keep the existing exported `Resolve(workdir, path)` and `ResolveForWrite(workdir, path)` functions as thin wrappers calling `resolver{root: workdir}.resolve(...)` — zero behavior change at call sites.
3. Add new exported helpers `ResolveMemory(profileDir, kind)` and `ResolveMemoryForWrite(profileDir, kind)` that build a `resolver{root: filepath.Join(profileDir, "memories")}` and validate that `kind ∈ {"memory", "user"}` before resolving against the canonical filename (`MEMORY.md` or `USER.md`).

The memory tools call ONLY the new helpers; they cannot pass arbitrary paths. This removes user-supplied path strings from the memory-write surface entirely — the only inputs are the enum `kind` and the content body.

**Why split**: One PR-sized change keeps the diff focused on memory work while leaving the public surface used by Read/Write/Edit/Grep untouched. The `resolver` extraction is the only structural change; everything else is additive helper functions.

### D12: `session_search` bypasses the `Store` interface and holds the raw `*sql.DB`

**Choice**: The `Store` interface in `internal/store/store.go:12-61` is documented as the "database-agnostic persistence boundary" and has 17 CRUD methods, none of which can express FTS5 MATCH, recursive CTE on `parent_id`, or the snippet/context join needed by `session_search`. Rather than bloating the interface with SQLite-shaped methods that would never be re-implemented for another backend, the `session_search` tool receives the underlying `*sql.DB` (exposed via a new `(*sqlite.Store).DB() *sql.DB` accessor) and runs SQL directly.

**Why this and not the alternative**: Adding `SearchMessages(...) `, `SessionAncestors(...) `, `SessionDescendants(...)` to the `Store` interface would pretend those operations are portable. They are not — they require FTS5 (SQLite-only) and recursive CTE (works on SQLite/Postgres but with different syntax). The Store abstraction's value is that someone could later swap in Postgres if needed; the day that happens, search would have to be reimplemented anyway, and the interface bloat would just be a tax until then. Concentrating SQLite-specificity inside the `sessionsearch` package keeps the boundary honest.

**Trade-off**: `session_search` is harder to unit-test against a fake store. Mitigation: tests use an in-memory SQLite (`:memory:` via the same `modernc.org/sqlite` driver) seeded with fixtures — the same approach existing `internal/store/sqlite` tests use.

### D13: Single global advisory lock file for memory writes

**Choice**: One lock file `<profileDir>/memories/.write.lock` guards both `MEMORY.md` and `USER.md` writes. The lock is acquired before reading current content (for append mode) and released only after the temp-file rename succeeds.

**When the lock is held**: BOTH `replace` and `append` modes acquire the lock for the entirety of their write cycle. Append needs it because of the read-modify-write step (otherwise a concurrent write between its read and its rename would be lost). Replace needs it because a concurrent append might be holding the file mid-RMW, and dropping a replace into that window would race against the append's pending rename — the append could either lose the user's replace, or write to a now-stale base. Therefore: lock is taken before the first I/O of either mode and released only after the rename has succeeded.

**Why one lock for both files**: Concurrent `memory_write` calls — one with `kind: "memory"` and one with `kind: "user"` — are independent in terms of file targets but share the directory inode. A single global lock removes any cross-file race and is essentially free because contention on memory writes is expected to be near-zero (writes are agent-initiated, rare, and serialized by the agent loop's turn structure in any case). Per-file locks would be a premature optimization.

The lock file is created on first use with mode `0600` and is never removed (an empty lock file is harmless and avoids a removal-vs-acquisition race).

## Risks / Trade-offs

- **Risk**: `modernc.org/sqlite` could ship without FTS5 in some future version → **Mitigation**: D9 startup probe; pin minor version in `go.mod` if needed.
- **Risk**: Backfilling FTS on a multi-GB existing `messages` table stalls startup → **Mitigation**: D8 progress logging; follow-up plan to make backfill lazy if reports come in.
- **Risk**: User mutates `MEMORY.md` mid-session via an external editor and expects it to take effect → **Mitigation**: documented behavior; `memory_read` re-reads disk so the tool can surface drift; the snapshot-vs-disk gap is also surfaced in `--debug` logs.
- **Risk**: Char-limit too tight for power users → **Mitigation**: configurable via `memory.memory_char_limit` / `memory.user_char_limit`; defaults match Hermes but operator can raise them.
- **Trade-off**: D6's query-layer CJK trigram synthesis is uglier than a custom tokenizer but keeps us on pure-Go SQLite. Acceptable for MVP; revisit if recall complaints arise. **Concrete kill-switch**: if dogfooding shows < 80% CJK recall relative to a LIKE-only baseline on representative queries, switch to a `tokenize='trigram'` side-table within one follow-up change.
- **Trade-off**: D8 backfill is synchronous and could be slow on huge histories; we accept this because the alternative (lazy backfill with a "is this session indexed yet?" check on every query) adds complexity that the current install base doesn't justify.
- **Trade-off**: Frozen-snapshot semantics will confuse users at first ("I wrote to memory, why doesn't the agent know?"). Mitigation: `memory_write` response includes a `next_session_effective: true` hint string the model can relay to the user.

## Migration Plan

1. Single migration `0002_memory_fts.up.sql` (paired with `down.sql` that drops the virtual table + triggers).
2. On first boot after upgrade: migration runs inside the existing migration transaction; backfill completes before the server starts accepting requests.
3. Rollback: `down.sql` drops `messages_fts` and triggers; source-of-truth `messages` table is untouched. Memory files on disk are left alone (operator can `rm -rf ~/.workhorse-agent/memories/` if desired).
4. No config changes required to use defaults; new keys are optional.

## Open Questions

- **Prompt-cache: the USER-before-MEMORY ordering does not actually save cache cost without a second cache_control breakpoint.** Anthropic prompt caching keys on byte-exact prefixes terminated by a `cache_control` marker. With a single marker placed at (or after) the entire system prompt, any byte change inside the `<memory>` block — including a MEMORY-only change — invalidates the whole cached prefix; the USER section's stability is wasted. To make the ordering pay off, the call site would need to emit TWO cache breakpoints: one after the static system text + USER section, and one after the MEMORY section. The Anthropic SDK supports up to 4 breakpoints per request, so this is feasible, but the existing agent loop does not currently set any breakpoints. **Resolution**: keep the ordering in this change (zero-cost forward compatibility), defer the breakpoint wiring to a follow-up that touches the provider request builder. Document this so future-us doesn't claim the savings before they actually exist.
- Should `memory_write` mode default to `replace` or `append`? Leaning `replace` (matches Hermes) but `append` is friendlier for incremental note-taking. **Resolution**: ship `replace` as default, `append` available; revisit after dogfooding.
- Should `session_search` be restricted to the current session's ancestors (via `parent_id` chain) by default, with a flag to widen scope? Leaning yes — privacy posture is "you see your conversation tree, not unrelated history". **Resolution**: default scope = current session + ancestors + descendants; opt-in `scope: "all"` for cross-tree search.
- Do we expose a `memory_diff` tool to show "what changed since the snapshot was loaded"? Probably not for MVP — the model can call `memory_read` to compare. **Resolution**: defer; revisit if dogfooding shows confusion.
