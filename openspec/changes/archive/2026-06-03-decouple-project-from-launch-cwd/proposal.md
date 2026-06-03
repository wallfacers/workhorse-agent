## Why

The desktop client (`workhorse-assistant`) lets the user pick **any local
directory** as a project — the `opencode`/Claude Code model — while this sidecar
runs **once** as a long-lived process. Three sidecar contracts still assume
"the sidecar's launch directory is the project," which leaks an accidental path
to the client and blocks arbitrary-directory projects:

1. `GET /health.default_workdir` falls back to `os.Getwd()` (the launch cwd), so a
   freshly-started sidecar advertises e.g. `D:\…\workhorse-agent` as "the default
   project," which the client adopts and then nags the user about.
2. `GET /v1/fs/list` confines browsing to a **single global** root
   (`cfg.DefaultWorkdir`), so any project opened elsewhere would be `403`-ed once
   that config is set.
3. `GET /v1/sessions` with no `workdir` returns only **in-memory live** sessions,
   so the client's cross-project session-management view cannot list idle
   persisted sessions from other projects.

This change is the sidecar half of `workhorse-assistant`'s
`decouple-project-from-launch-cwd`.

## What Changes

- **`default_workdir` resolution drops the launch-cwd fallback.** `defaultWorkdir()`
  becomes `server.default_workdir` (override) → `os.UserHomeDir()` → omitted. It
  SHALL NOT fall back to `os.Getwd()`, and is no longer required to be non-empty
  (an omitted value tells the client to show its project picker). **BREAKING**
  (value semantics of an existing field).
- **`GET /v1/fs/list` confinement follows the requested project root.** The
  endpoint accepts an explicit project root (a `root` query param; falls back to
  `default_workdir` when omitted) and confines results to **that** root rather
  than the single global `cfg.DefaultWorkdir`. Path-escape, virtual-FS, 404/400
  guards are unchanged. **BREAKING** (callers relying on the implicit global root).
- **`GET /v1/sessions` (no `workdir`) returns the full persisted set.** The no-arg
  branch sources from `store.ListSessions` (all projects, persisted) with live
  `running`/`idle` status overlaid, each row carrying its `workdir`. The
  `?workdir=P` branch is unchanged.

## Capabilities

### New Capabilities
<!-- none — deltas on existing capabilities -->

### Modified Capabilities
- `wsl-remote-sidecar`: `default_workdir` resolves to the user's home (not the
  process cwd) and may be omitted; `/v1/fs/list` confinement follows a per-request
  project root.
- `api-protocol`: the project/session endpoints add a no-`workdir` `GET /v1/sessions`
  that returns the full persisted session list with live status overlaid.

## Impact

- `internal/api/health.go` — `defaultWorkdir()` resolution chain.
- `internal/api/fs.go` — `handleFSList` + `isWithinWorkdir` scoped to the request
  root; new `root` query param.
- `internal/api/sessions.go` — `handleListSessions` no-`workdir` branch via
  `store.ListSessions` + live overlay (`store.ListSessions` / `ListProjects`
  already exist in `internal/store/sqlite/crud.go`).
- Tests: `internal/api/{health_test,fs_test,sessions_test}.go`.
- `protocol_version` unchanged (all `/health` changes are additive / value-only).
