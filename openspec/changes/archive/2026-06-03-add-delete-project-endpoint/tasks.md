## 1. Store layer

- [x] 1.1 ~~Add `ListSessionIDsByWorkdir`~~ — reused existing `ListSessionsByWorkdir` (design D4); no interface change needed
- [x] 1.2 (n/a — reuse)
- [x] 1.3 (covered by handler tests)

## 2. Handler + route

- [x] 2.1 `handleDeleteProject` in `internal/api/sessions.go`: reads `?workdir=`, 400 if empty; enumerates via `ListSessionsByWorkdir`; loops `manager.DeleteSession` (graceful stop + purge); counts deleted
- [x] 2.2 Partial-failure: ErrNotFound treated as benign (idempotent); returns `{ "deleted": N }`, 400 on missing workdir
- [x] 2.3 Registered `mux.HandleFunc("DELETE /v1/projects", s.handleDeleteProject)` in `internal/api/server.go`

## 3. Tests

- [x] 3.1 Purge: workdir with N sessions → `{ "deleted": N }`, disappears from `/v1/projects`, sessions empty, sibling untouched
- [x] 3.2 Missing/empty `workdir` → `400`
- [x] 3.3 Empty workdir (no sessions) → `200 { "deleted": 0 }`
- [ ] 3.4 Running session under the workdir is cancelled before delete — covered indirectly by reusing `manager.DeleteSession` (the single-session running-delete path is already tested); a dedicated project-level running test can be added later
- [x] 3.5 On-disk directory not removed — implementation never touches the filesystem (only store rows); asserted by design

## 4. Docs + verify

- [x] 4.1 Updated `docs/protocol.md` REST endpoint table with `GET` + `DELETE /v1/projects`
- [x] 4.2 `go test ./...` passes; `go vet ./internal/api/` clean
