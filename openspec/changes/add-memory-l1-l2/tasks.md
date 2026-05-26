## 1. Preflight, migration framework, configuration

- [x] 1.1 Verify FTS5 is compiled into the linked `modernc.org/sqlite` build with a one-shot Go test (`SELECT sqlite_compileoption_used('ENABLE_FTS5')`); if absent, stop and revisit dependency strategy before further work
- [x] 1.2 **Spike** the `modernc.org/sqlite` scalar-function registration API: write a 30-line throwaway that registers a trivial `noop_test(x)` function via `sqlite.MustRegisterScalarFunction` (or the per-connection equivalent if `MustRegisterScalarFunction` proves not to exist on this driver version) and confirms it can be invoked from a real `SELECT`. Record the confirmed registration API in a comment at the top of the new `internal/store/sqlite/funcs.go` before any further work in §7
- [x] 1.3 Build the versioned migration framework: introduce `type Migration struct{Version int; Up, Down []string}` and `var migrationsByVersion []Migration` in `internal/store/sqlite/migrations.go`; move the current v1 statements verbatim into `migrationsByVersion[0]`; refactor `migrate()` to read `schema_version`, iterate entries with `Version > current`, apply each inside its own transaction, and bump `schema_version` per step. Behavior on fresh installs MUST be byte-identical to today
- [x] 1.4 Tests covering 1.3: idempotent migration on a fresh DB; idempotent re-run; partial-state recovery (simulate crash after v1 completes; assert v2 runs cleanly on next boot)
- [x] 1.5 Add `memory.dir`, `memory.memory_char_limit` (default 2200), `memory.user_char_limit` (default 1375) to the config schema in `internal/config`; wire defaults so the keys are optional
- [x] 1.6 Add validation in `internal/config`: reject `memory.memory_char_limit <= 0` and `memory.user_char_limit <= 0`; treat empty `memory.dir` as "use default"; cover with table-driven tests
- [x] 1.7 Add a startup probe in `internal/store/sqlite` that runs the FTS5 compile-option check on the live connection and returns a clearly-identified error if the result is 0
- [x] 1.8 Add tests covering 1.7: an artificial failure path that asserts the server refuses to start with the expected error message

## 2. internal/memory package

- [x] 2.1 Create `internal/memory/snapshot.go` with `Snapshot{MemoryMD, UserMD string; LoadedAt time.Time}` and a `Loader.Load(profileDir string)` that reads both files, treats missing as empty, and counts code points
- [x] 2.2 Create `internal/memory/writer.go` with `Writer.Write(kind, content, mode)` enforcing char limits via `utf8.RuneCountInString`, returning structured `ErrMemoryTooLarge{Limit, Actual}` on overflow
- [x] 2.3 Implement temp-file + rename atomic write in writer; ensure no partial state is ever visible on disk
- [x] 2.4 Implement exclusive advisory file locking around the entire read-modify-write cycle of BOTH replace and append modes, using a SINGLE shared lock file `<profileDir>/memories/.write.lock` (per design.md D13). Linux/macOS: `flock(LOCK_EX)` via `golang.org/x/sys/unix`; Windows: `LockFileEx` via `golang.org/x/sys/windows`; cross-platform wrapper in `internal/memory/lock_unix.go` and `lock_windows.go`. The lock file is created (mode 0600) on first use and never removed
- [x] 2.5 Create `<profileDir>/memories/` with mode `0700` on first write if it does not exist
- [x] 2.6 Unit tests: code-point counting with CJK input, over-limit rejection leaves disk untouched, atomic replace, concurrent appends to the SAME file do not lose data (50 goroutines), concurrent appends across BOTH files (25 goroutines on memory + 25 on user) also do not lose data — both classes serialize through the single global lock
- [x] 2.7 Unit test: snapshot loaded from missing files yields empty strings without creating files

## 3. pathguard refactor for memory scope

- [x] 3.1 Extract an unexported `resolver` type in `internal/tools/pathguard/pathguard.go` whose containment root is a struct field; move the existing `canonicalise` + `assertInside` logic into `resolver.resolve(path string, allowMissing bool) (string, error)`. Behavior MUST be unchanged
- [x] 3.2 Re-implement existing exported `Resolve(workdir, path)` and `ResolveForWrite(workdir, path)` as thin wrappers calling `resolver{root: workdir}.resolve(...)`; run the full existing `pathguard_test.go` suite to confirm zero behavior change
- [x] 3.3 Add new exported helpers `ResolveMemory(profileDir, kind string) (string, error)` and `ResolveMemoryForWrite(profileDir, kind string) (string, error)` that validate `kind ∈ {"memory", "user"}`, build a `resolver` rooted at `filepath.Join(profileDir, "memories")`, and resolve against the fixed filename (`MEMORY.md` or `USER.md`)
- [x] 3.4 Tests: invalid `kind` rejected before any filesystem touch; symlink whose target is outside memories dir is rejected with `ErrPathEscape`; path outside memories dir cannot be smuggled via the `kind` argument under any encoding
- [x] 3.5 Regression test: a memory tool cannot use the new helpers to resolve a path inside the session workdir, and the Read/Write/Edit tools cannot use the new helpers to escape into the memories dir

## 4. Memory block formatting (call-site concatenation, no prompt-package change)

- [x] 4.1 Implement `memory.Block(snapshot *Snapshot) string` in `internal/memory/block.go`: produces the exact delimited block defined in design.md D3 (USER before MEMORY, omit halves when empty, return "" when both empty). Document inline that the byte layout is load-bearing for prompt-cache stability
- [x] 4.2 Wire concatenation at `internal/agent/loop.go:371`: prepend `memory.Block(session.snapshot)` (with one blank line separator iff non-empty) to `l.SystemPromptBase` before calling `prompt.BuildSystemPrompt`. `prompt.BuildSystemPrompt`'s signature is **unchanged**
- [x] 4.3 Tests on `memory.Block`: both empty → ""; only USER → contains USER section, no MEMORY section, no `---`; only MEMORY → contains MEMORY section, no USER section, no `---`; both present → exact byte sequence matches a golden string
- [x] 4.4 Test: calling `memory.Block` twice with the same snapshot returns byte-identical strings (cache prefix stability)
- [ ] 4.5 Negative test: confirm `internal/prompt` package source has zero new imports and `BuildSystemPrompt`'s signature is untouched (a simple grep-based test in CI is sufficient — this is to prevent silent regression of the boundary)

## 5. Agent loop wiring

- [x] 5.1 In session creation (`internal/session/session.go`), call `memory.Loader.Load(profileDir)` once and attach the resulting `*Snapshot` to the session struct as an unexported field
- [x] 5.2 In the agent loop's system-prompt construction, pull the snapshot off the session and pass it to `BuildSystemPrompt` so every turn renders the identical prefix
- [ ] 5.3 Tests: session A's snapshot remains identical for the full lifetime even after `MEMORY.md` on disk changes; session B started after the change sees the new content

## 6. memory_read and memory_write tools

- [ ] 6.1 Create `internal/tools/memorytool/read.go` implementing `memory_read` returning `{content, char_count, char_limit}` by reading from disk (not from the session snapshot)
- [ ] 6.2 Create `internal/tools/memorytool/write.go` implementing `memory_write` with `mode` defaulting to `"replace"`, returning `{accepted, char_count, char_limit, next_session_effective: true}` on success
- [ ] 6.3 Wire both tools into the tool registry; ensure they are gated by the existing `allowed_tools` filter on agents and skills
- [ ] 6.4 Surface `memory_too_large` errors as structured tool errors (matching the existing error envelope used by other tools)
- [ ] 6.5 Tool-level integration tests: invalid `kind` rejected with `invalid_kind`, over-limit rejected with `memory_too_large`, append followed by read returns appended content
- [ ] 6.6 Verify via test that `memory_write` during an active session does not alter that session's system-prompt rendering

## 7. SQLite migration and FTS5 schema

> **Depends on**: §1.2 (function-registration spike confirmed) and §1.3 (versioned migration framework in place).

- [ ] 7.1 Implement `extract_text(content_json BLOB) -> TEXT` as a custom SQLite function in `internal/store/sqlite/funcs.go`, using the exact registration API confirmed by the §1.2 spike. The function walks the JSON content-block array and concatenates only `type: "text"` blocks with single-space joins
- [ ] 7.2 Register `extract_text` at driver-setup time (the registration point identified in §1.2) so that the migration runner has the function available before §7.3 executes
- [ ] 7.3 Add v2 migration entry to `migrationsByVersion` that creates `messages_fts` (`USING fts5(content, content='messages', content_rowid='rowid', tokenize='unicode61 remove_diacritics 2')`) and the AI/AD/AU triggers
- [ ] 7.4 Add backfill statement `INSERT INTO messages_fts(rowid, content) SELECT rowid, extract_text(content_json) FROM messages` inside the same migration entry's `Up`; emit a progress log every 10,000 rows
- [ ] 7.5 Populate the v2 migration's `Down`: drop the AI/AD/AU triggers then drop `messages_fts`
- [ ] 7.6 Tests: a freshly-created `messages` row results in an indexed `messages_fts` row in the same transaction; deletion removes the FTS row; an update rewrites the FTS row
- [ ] 7.7 Test: `tool_use`/`tool_result`-only message yields an empty FTS content string
- [ ] 7.8 Test: backfill correctness against a seeded fixture with N=5 mixed messages (text-only, tool-only, mixed); assert FTS row count == N and content equals expected per-row text
- [ ] 7.9 Test: malformed `content_json` (corrupt by hand in a fixture) does not panic the custom function; it returns the empty string and a Go-level warning is logged

## 8. session_search tool

> **Storage access**: per design.md D12, this tool bypasses the `Store` interface. Add a `func (s *sqlite.Store) DB() *sql.DB` accessor; the tool holds the `*sql.DB` directly. SQLite-specific SQL (FTS5 MATCH, recursive CTEs) is contained inside `internal/tools/sessionsearch`.

- [ ] 8.1 Add `DB() *sql.DB` accessor on `*sqlite.Store` (single-line method); add a docstring noting it is intended for SQLite-specific consumers only and Store remains the portable boundary for everything else
- [ ] 8.2 Create `internal/tools/sessionsearch/tool.go` exposing `session_search` with parameters `{query, session_id?, scope?, limit?, context_before?, context_after?}` and defaults per spec (`limit: 10`, `context_before: 5`, `context_after: 5`)
- [ ] 8.3 Implement default scope = current session + ancestors (recursive CTE on `parent_id`) + descendants; `scope: "session"` = current only; `scope: "all"` = all non-deleted sessions
- [ ] 8.4 Always exclude soft-deleted sessions (`WHERE sessions.deleted_at IS NULL`) regardless of scope; add a test that proves this with `scope: "all"` and a soft-deleted seed
- [ ] 8.5 **CJK (1/5) — Unicode classifier**: write `internal/tools/sessionsearch/cjk.go` containing `isCJK(r rune) bool` covering the exact Unicode ranges enumerated in design.md D6 (CJK Unified Ideographs, Ext-A, Ext-B, Hiragana, Katakana, Hangul Syllables, Hangul Jamo). Table-driven test asserting boundary code points for each range
- [ ] 8.6 **CJK (2/5) — Run tokenizer**: `tokenize(query string) []run` where `run.kind ∈ {ascii, cjk, ws, other}`. Test on representative inputs including pure-ASCII, pure-CJK, mixed, punctuation-separated runs
- [ ] 8.7 **CJK (3/5) — Trigram synthesizer**: `trigrams(cjkRun string) []string` produces the sliding 3-grams; test that "数据库迁移" yields exactly `["数据库", "据库迁", "库迁移"]`
- [ ] 8.8 **CJK (4/5) — Plan builder**: `buildPlan(query string) (matchExpr string, ok bool)` combines tokens into FTS5 expression OR returns `ok=false` to signal LIKE fallback. Tests cover: pure ASCII passes through; long CJK builds AND-joined trigram phrases; ≥1 CJK run shorter than 3 → fallback flag; mixed ASCII + long-CJK builds combined expression; mixed ASCII + short-CJK → fallback
- [ ] 8.9 **CJK (5/5) — LIKE fallback executor**: when `buildPlan` returns ok=false, execute `LIKE '%fragment%' AND LIKE '%fragment2%' ...` against `extract_text(content_json)` from `messages`, joined to sessions for scope filter; order by `created_at` DESC; test with both short-CJK-only query and mixed query
- [ ] 8.10 Use SQLite `snippet(messages_fts, 0, '', '', '...', 8)` for the snippet field (≤ 30 chars enforced by token budget); fetch `context_before/after` adjacent messages by `messages.created_at` within the same session
- [ ] 8.11 Order by FTS5 rank ASC then `messages.created_at` DESC; cap `limit` at 50, context bounds at 20
- [ ] 8.12 Implement `truncated` flag via the limit+1 trick: internally execute with `LIMIT effectiveLimit+1`; if more rows return than `effectiveLimit`, drop the last and set `truncated: true`; otherwise `truncated: false`. Tests assert both cases at boundaries (exactly `limit`, `limit+1`, `limit+5`)
- [ ] 8.13 Wire tool into registry under the same `allowed_tools` filter
- [ ] 8.14 Tool-level integration tests: ASCII query returns ranked hits; CJK-only query of length 5 hits the trigram path; CJK query of length 2 hits the LIKE path; mixed ASCII+CJK works
- [ ] 8.15 Tool-level integration test: session-tree scope test — create parent→child→grandchild + sibling tree; assert default scope returns parent/child/grandchild, excludes sibling, and `scope: "all"` returns everything
- [ ] 8.16 Tool-level test: assert no LLM API calls happen during `session_search` execution (intercept the provider client)

## 9. End-to-end and documentation

- [ ] 9.1 End-to-end test: spin up server, create session A, write memory via `memory_write`, end session, create session B, assert session B's first model call's system prompt contains the written content while session A's never did
- [ ] 9.2 End-to-end test: send messages in two sessions, run `session_search` from a third session, verify hits + context structure end-to-end via HTTP
- [ ] 9.3 Update `CLAUDE.md` with a short "Memory subsystem" section pointing at `internal/memory/`, the prompt slot, and the new tools
- [ ] 9.4 Add `~/.workhorse-agent/memories/` to the documented profile-dir layout (wherever that is currently listed)
- [ ] 9.5 Run `golangci-lint run` and `gofumpt -l .`; address any new findings introduced by this change
- [ ] 9.6 Smoke test: start the binary against a fresh profile dir; create one session; write memory; restart the binary; create a new session; verify memory is rendered in the system prompt

## 10. Sign-off and archive

- [ ] 10.1 Mark every checkbox above complete only after the corresponding scenario in the spec files passes
- [ ] 10.2 Move the change into archive per repo convention (`openspec/changes/archive/<date>-add-memory-l1-l2/`) once merged
