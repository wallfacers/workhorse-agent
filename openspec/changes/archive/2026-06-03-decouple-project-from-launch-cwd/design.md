## Context

This is the sidecar half of `workhorse-assistant`'s
`decouple-project-from-launch-cwd` (see that repo's `design.md` for the full
cross-repo rationale and the `opencode` comparison). The session/storage layer is
already directory-keyed: `POST /v1/sessions {workdir}`, SQLite store with
`ListSessions`, `ListSessionsByWorkdir`, and `ListProjects` already present
(`internal/store/sqlite/crud.go`), and `sessionMeta` already carries `workdir`.
Only three HTTP-surface points still assume "launch cwd = project."

## Goals / Non-Goals

**Goals:**
- `default_workdir` is a stable, meaningful value (home), never the launch cwd.
- File browsing is confined per requested project root, so any opened directory is
  browsable even when a global default is configured.
- The no-arg session list reflects all persisted projects, not just live memory.

**Non-Goals:**
- Changing `?workdir=P` listing, `/v1/projects`, history/rename/delete contracts.
- Git-root / worktree discovery — a project is the exact path passed in.
- Wire `protocol_version` bump — changes are additive / value-only.

## Decisions

### D1 — `defaultWorkdir()` = override → home → omit
Resolution chain becomes `cfg.DefaultWorkdir` (if non-empty) → `os.UserHomeDir()`
→ omit the field. Remove `os.Getwd()` and the "SHALL always be non-empty"
guarantee.

- **Why home over launch cwd:** a daemon's launch dir is incidental; home is
  stable and benign. The client only uses `default_workdir` as a cold-start seed
  when it has no remembered project.
- **Why omit (not "/") when home is unresolvable:** an absent field routes the
  client to its project picker (its `wsl-remote` cold-start contract), which beats
  seeding a useless root.

### D2 — `/v1/fs/list` takes an explicit `root`, confines to it
Add an optional `root` query param (the project being browsed). Confinement
(`isWithinWorkdir`) is evaluated against `root` (or `default_workdir` when `root`
is omitted), not the single global `cfg.DefaultWorkdir`.

- **Why an explicit param over session-derived:** the client opens/browses a
  project **before** any session exists, so deriving the root from a `session_id`
  is insufficient. This mirrors `opencode`'s per-request `Instance.directory`.
- All other guards (virtual-FS 403, 404, 400-not-a-dir, single-level, dotfiles,
  sort) are preserved verbatim.

### D3 — No-arg `GET /v1/sessions` via `store.ListSessions` + live overlay
The no-`workdir` branch reads `store.ListSessions(ctx, includeDeleted=false)`
(full persisted set) and overlays live status per id (same overlay
`listSessionsByWorkdir` already does), each row carrying `workdir`.

- **Why:** the client's session-management view lists across projects; the current
  `manager.ListSessions()` (live only) hides idle persisted sessions.
- **Why keep `?workdir=P`:** the in-app switcher stays project-scoped; only the
  management table wants the global view.
- **Implementation note:** added `store.ListAllSessions` returning
  `[]*SessionSummary` (the workdir-filtered query minus its `WHERE workdir`),
  *not* the bare `store.ListSessions` (`[]*Session`) — the management table needs
  `messageCount`/preview, which only the SessionSummary projection carries. The
  shared `sessionSummarySelect` + `scanSessionSummaries` back both list methods.

## Risks / Trade-offs

- **fs contract break** → sequence with the client's `ProjectBrowser` change so it
  always passes `root`; until `server.default_workdir` is configured, behavior is
  unchanged (omitted root → home, unrestricted-ish as before).
- **Home unresolvable in headless/CI** → field omitted; the client picker path
  covers it. Tests must assert "omitted, never launch cwd."
- **Larger no-arg list** → bounded by persisted session count; the client already
  paginates/sorts client-side. No new index needed (store query exists).
- **Rollback:** the three handlers are independent; revert any one without
  affecting the others.

## Open Questions

- Param name: `root` vs reusing `workdir` on `/v1/fs/list`? Leaning `root` to avoid
  confusing it with session `workdir` semantics — confirm with the client.
- Should the no-arg list exclude soft-deleted rows only, or also archived? Default:
  `includeDeleted=false`, archived included (matches `?workdir=P`).
