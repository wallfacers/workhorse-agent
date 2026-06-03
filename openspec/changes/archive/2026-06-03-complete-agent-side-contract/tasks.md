## 1. default_workdir → home, not launch cwd

- [x] 1.1 In `internal/api/health.go`, change `defaultWorkdir()` resolution to `cfg.DefaultWorkdir` (override) > `os.UserHomeDir()` > omit. Remove the `os.Getwd()` fallback entirely.
- [x] 1.2 When neither override nor home is resolvable, omit `default_workdir` from the `/health` JSON (or return empty) so the assistant routes to the picker — never report the launch directory.
- [x] 1.3 In `internal/session/workdir.go`, change `ValidateWorkdir("")` fallback from `os.Getwd()` to `os.UserHomeDir()`. This is a global change: all session creation paths (HTTP POST, external agent, tests) use home as the default, never the launch cwd.
- [x] 1.4 Update `internal/api/health_test.go`: no-override → home; override honored; unresolvable home → omitted/empty, never the process cwd. Update `internal/session/workdir_test.go`: empty workdir → home, not cwd.

## 2. /v1/fs confinement follows the requested workdir

- [x] 2.1 Add `?root=` query parameter to `handleFSList` in `internal/api/fs.go`; use it as the confinement boundary in `isWithinWorkdir` when provided.
- [x] 2.2 Fall back to `cfg.DefaultWorkdir` when `?root=` is not provided (preserve existing behavior).
- [x] 2.3 Keep path-escape rejection: a path outside the scoped root still returns `403 forbidden`; preserve the existing virtual-FS guard.
- [x] 2.4 Update `internal/api/fs_test.go`: browse within an arbitrary requested root succeeds even when a different global default is configured; escape attempts 403.

## 3. GET /v1/sessions (no workdir) returns the full persisted list

- [x] 3.1 In `internal/api/sessions.go`, change the no-`workdir` branch of `handleListSessions` to source from `store.ListSessions(ctx, includeDeleted=false)` (returns `[]*store.Session`, the full persisted set across projects) instead of `manager.ListSessions()` (in-memory live only).
- [x] 3.2 Write a `metaFromSession(s *store.Session, status string)` converter (new; the existing `metaFromSummary` works on `*store.SessionSummary`, which has a different shape). Overlay live status: for each persisted row, call `manager.GetSession(id)` — if found and mid-turn → `running`, else `idle`. Ensure each row carries its `workdir`.
- [x] 3.3 Keep `?workdir=P` behavior unchanged (still returns `P`'s sessions for the in-app switcher).
- [x] 3.4 Update `internal/api/sessions_test.go`: no-arg list returns idle persisted sessions from multiple workdirs, each with its `workdir` field.

## 4. Verification

- [x] 4.1 `go test ./...` green
- [x] 4.2 Confirm `protocol_version` is unchanged (all `/health` changes are additive / value-only — no wire-version bump)
