## 1. `default_workdir` → home, not launch cwd

- [x] 1.1 In `internal/api/health.go`, change `defaultWorkdir()` to
      `cfg.DefaultWorkdir` (override) → `os.UserHomeDir()` → omit. Remove the
      `os.Getwd()` fallback.
- [x] 1.2 When neither override nor home resolves, omit `default_workdir` from the
      `/health` JSON (or return empty) — never report the launch directory.
- [x] 1.3 Update `internal/api/health_test.go`: no override → home; override
      honored; unresolvable home → omitted/empty, never the process cwd.

## 2. `/v1/fs/list` confinement follows the requested root

- [x] 2.1 In `internal/api/fs.go` `handleFSList`, accept an optional `root` query
      param (the project being browsed); the enumeration root is `root` or, when
      omitted, `default_workdir`.
- [x] 2.2 Evaluate `isWithinWorkdir` against the request's enumeration root, not
      the global `cfg.DefaultWorkdir`; a path escaping that root returns `403`.
- [x] 2.3 Preserve virtual-FS 403, 404, 400-not-a-dir, single-level, dotfiles,
      sort behavior unchanged.
- [x] 2.4 Update `internal/api/fs_test.go`: browsing an arbitrary `root` succeeds
      even with a different global default configured; escape attempts 403.

## 3. `GET /v1/sessions` (no workdir) returns the full persisted list

- [x] 3.1 In `internal/api/sessions.go` `handleListSessions`, change the
      no-`workdir` branch to source from the persisted store across all projects
      (`store.ListAllSessions`) instead of `manager.ListSessions()` (in-memory
      live only). (Added `ListAllSessions` to the store interface + sqlite impl,
      sharing the SessionSummary projection with `ListSessionsByWorkdir`.)
- [x] 3.2 Overlay live status per row (`running` when a live session is mid-turn,
      else `idle`), via the shared `summariesToMeta`; each row carries `workdir`.
- [x] 3.3 Keep `?workdir=P` unchanged (in-app switcher); only the no-arg branch
      changes.
- [x] 3.4 Update `internal/api/project_endpoints_test.go`: no-arg list returns
      idle persisted sessions across multiple workdirs, each with its `workdir`.

## 4. Verification

- [x] 4.1 `go test ./...` green for changed packages (`internal/api`,
      `internal/store/sqlite`, `internal/session`). NOTE: `internal/agent`'s
      `TestLoop_FirstMessage_BroadcastsTitle` is a pre-existing flaky test
      (pass/fail/pass across reruns); it is untouched by this change.
- [x] 4.2 `protocol_version` unchanged (changes are additive / value-only — no
      wire-version bump).
- [ ] 4.3 Cross-check field names against the client's
      `decouple-project-from-launch-cwd` change (param name `root`, `SessionMeta`
      shape) before merging either side. *(Pending the assistant-side apply.)*
