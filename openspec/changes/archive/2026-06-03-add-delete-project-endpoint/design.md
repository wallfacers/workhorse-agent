## Context

A project is derived, not stored: `handleListProjects` →
`store.ListProjects` (`internal/store/sqlite/crud.go`) returns distinct
`workdir`s aggregated from non-deleted sessions. Single-session delete already
exists end-to-end: `DELETE /v1/sessions/{id}` → `handleDeleteSession` →
`manager.DeleteSession` (cancels any running turn, graceful-shutdown timeout) and
hard delete via `store.PurgeSession` (`DELETE FROM sessions` + cascade to
messages/events/tool_calls). The assistant (companion change
`add-global-confirm-and-project-delete`) will call a new
`DELETE /v1/projects?workdir=` to clear a whole project.

## Goals / Non-Goals

**Goals:**
- One server-side endpoint to hard-delete all sessions for a `workdir`.
- Reuse the existing graceful-stop + purge path; no new delete semantics.
- Leave the on-disk directory untouched.

**Non-Goals:**
- No filesystem deletion.
- No soft-delete / undo.
- No change to single-session delete behavior.

## Decisions

### D1 — Loop existing per-session delete, don't add a bulk SQL path
`handleDeleteProject` enumerates the workdir's session ids and reuses
`manager.DeleteSession` per session (same code path as `DELETE /v1/sessions/{id}`),
which cancels running turns and purges with cascade. **Why:** running sessions
must be gracefully stopped before their rows vanish; a raw bulk
`DELETE FROM sessions WHERE workdir=?` would orphan in-flight turns and skip the
manager's teardown. **Alternative:** a single bulk SQL delete — rejected; it
bypasses cancellation and the manager's in-memory state.

### D2 — `?workdir=` query param, not a path segment
Mirror the existing `GET /v1/sessions?workdir=` convention. **Why:** workdirs are
filesystem paths; encoding them into a path segment is error-prone. Consistent
with the codebase.

### D3 — Idempotent on empty; 400 on missing workdir
Missing/empty `workdir` → `400`. A valid workdir with zero sessions → `200
{ "deleted": 0 }`. **Why:** the client may race a concurrent delete or pass an
already-empty project; idempotent success keeps the UI simple, while a missing
param is a programming error worth flagging.

### D4 — Enumerate via a store helper
Add `ListSessionIDsByWorkdir(ctx, workdir)` (or reuse `ListSessionsByWorkdir` and
map to ids). **Why:** keeps the handler thin and the SQL in the store layer,
matching existing structure.

## Risks / Trade-offs

- **Partial failure mid-loop** (one session fails to stop/purge) → some sessions
  deleted, others not. Mitigation: continue best-effort, collect errors, return
  the deleted count; surface a 500 with count only if at least one hard-failure
  occurs (client can retry — the endpoint is idempotent).
- **Many running sessions → slow sequential graceful stop.** Mitigation:
  acceptable for typical small counts; reuse the configured
  `GracefulShutdownTimeout` per session.
- **Concurrent create into the same workdir during delete** → a freshly created
  session might survive. Acceptable; the operation deletes what existed at
  enumeration time and is idempotent on retry.

## Open Questions

- Response shape on partial failure: `200 {deleted, failed:[ids]}` vs `500`?
  Leaning `200` with a `deleted` count and best-effort, matching idempotent intent
  — finalize during apply.
