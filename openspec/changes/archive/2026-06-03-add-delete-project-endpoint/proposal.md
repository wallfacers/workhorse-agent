## Why

The assistant needs to let users delete a "project record". A project is not a
stored entity here — `GET /v1/projects` derives distinct `workdir`s that still
have at least one non-deleted session. So "deleting a project" means removing all
of that workdir's sessions. Today the API can delete one session at a time
(`DELETE /v1/sessions/{id}`) but offers no way to clear a whole workdir, forcing
the client to enumerate and loop. This change adds a single endpoint that does it
server-side, reusing the existing graceful-stop + hard-purge machinery.

## What Changes

- Add `DELETE /v1/projects?workdir=<path>`: hard-delete every session under the
  given `workdir`. For each session the server SHALL first cancel/gracefully stop
  any running turn (as `DELETE /v1/sessions/{id}` does), then hard-delete the row
  with cascade to messages / events / tool_calls (reuse `store.PurgeSession`).
- The endpoint operates only on the persisted session records; it does **not**
  touch the on-disk `workdir` directory.
- A missing/empty `workdir` query parameter SHALL return `400`. Success returns
  `200` with a deleted count. A `workdir` with no sessions is a no-op success
  (count 0).
- Add a store helper to enumerate a workdir's session ids (e.g.
  `ListSessionIDsByWorkdir`, or reuse `ListSessionsByWorkdir`).

## Capabilities

### New Capabilities
<!-- none; this extends existing capabilities -->

### Modified Capabilities
- `api-protocol`: Add `DELETE /v1/projects?workdir=` to the HTTP REST endpoint
  set, with success/`400` behavior.
- `session-management`: Add a requirement for project-scoped deletion — hard-delete
  (with running-turn cancellation and transcript cascade) of all sessions sharing
  a `workdir`, leaving the directory untouched.

## Impact

- `internal/api/server.go`: register `DELETE /v1/projects` route.
- `internal/api/sessions.go`: new `handleDeleteProject` (mirrors
  `handleDeleteSession`, looping the workdir's sessions through the manager +
  purge).
- `internal/store/store.go` + `internal/store/sqlite/crud.go`: add
  `ListSessionIDsByWorkdir` (or reuse `ListSessionsByWorkdir`); `PurgeSession`
  already exists.
- Tests: `internal/api/project_endpoints_test.go` (or a sibling) covering purge,
  list-disappears, missing-workdir 400, empty-workdir no-op.
- Consumer: the assistant's `bridge.delete_project` / `agent_delete_project` /
  `deleteAgentProject` (companion change `add-global-confirm-and-project-delete`).
  This endpoint must ship before/with that path.
- Not breaking: purely additive endpoint.
