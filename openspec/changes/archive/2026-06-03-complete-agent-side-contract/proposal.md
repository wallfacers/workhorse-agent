## Why

The `decouple-project-from-launch-cwd` change was completed on the assistant side, but its 3 corresponding sidecar (Go) contract changes were never implemented: `default_workdir` still falls back to `os.Getwd()` (the sidecar launch directory), `/v1/fs` confinement uses a single global `DefaultWorkdir` instead of the request's scoped root, and `GET /v1/sessions` without a `workdir` query returns only live sessions instead of the full persisted list across projects. These gaps mean the assistant's project-decoupling work is incomplete at the sidecar boundary.

## What Changes

- **`default_workdir` → home directory**: `GET /health` resolves `default_workdir` to `cfg.DefaultWorkdir` (override) > `os.UserHomeDir()` > omit. Removes `os.Getwd()` fallback entirely.
- **`/v1/fs` request-scoped confinement**: The file-listing handler accepts an explicit `?root=` query parameter; confinement follows this root rather than the global `cfg.DefaultWorkdir`. Preserves the virtual-FS guard (`/proc`, `/sys`, `/dev`, `/run`).
- **`GET /v1/sessions` full persisted list**: When no `?workdir=` is provided, returns the full persisted session list (from `store.ListSessions`) with live-status overlay, instead of only live sessions from the in-memory manager. Each row carries its `workdir`. The `?workdir=P` filter behavior is unchanged.

## Capabilities

### Modified Capabilities

- `api-protocol`: `/health` default_workdir resolution changes (no more os.Getwd()); `/v1/fs` gains request-scoped `?root=` confinement; `/v1/sessions` no-workdir returns full persisted list
- `session-management`: `ListSessions` (no filter) exposes full persisted set across projects with live overlay

## Impact

- **API**: `internal/api/health.go` — `defaultWorkdir()` resolution logic
- **API**: `internal/api/fs.go` — `handleFSList` adds `?root=` param, `isWithinWorkdir` uses request-scoped root
- **API**: `internal/api/sessions.go` — `handleListSessions` no-workdir branch uses `store.ListSessions`; new `metaFromSession` converter
- **Session**: `internal/session/workdir.go` — `ValidateWorkdir("")` fallback from `os.Getwd()` to `os.UserHomeDir()` (global, affects all session creation paths)
- **Tests**: `internal/api/health_test.go`, `internal/api/fs_test.go`, `internal/api/sessions_test.go`, `internal/session/workdir_test.go`
- **Config**: `internal/api/server.go` — no structural change; `Config.DefaultWorkdir` field semantics clarified
