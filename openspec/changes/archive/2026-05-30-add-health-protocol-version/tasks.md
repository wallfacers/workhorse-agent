# Tasks: Record protocol_version and capabilities on GET /health in the spec

> The production code already returns these fields (`internal/api/health.go`).
> This change closes the spec drift and adds the missing test coverage; §1–§2 are
> verification-only, not new code.

## 1. Confirm existing implementation

- [x] 1.1 `internal/api/health.go` defines `ProtocolVersion = "1"` and `DefaultCapabilities` (incl. `frontend_tools`)
- [x] 1.2 `handleHealth` emits `protocol_version` and `capabilities` alongside `ok`/`version`/`uptime_sec`/`sessions_active`
- [x] 1.3 `GET /health` remains exempt from bearer auth and Origin checks (no middleware change)

## 2. Consolidated spec

- [x] 2.1 Update `api-protocol` "Health and Diagnostics Endpoints" requirement to document `protocol_version` and `capabilities` as additive, backward-compatible fields

## 3. Test coverage

- [x] 3.1 Extend `TestHealth_OKShape` to assert `protocol_version == ProtocolVersion`
- [x] 3.2 Assert `capabilities` contains `frontend_tools`

## 4. Validate

- [x] 4.1 `go build ./...` and `go vet ./...` clean
- [x] 4.2 `go test ./internal/api/...` passes (full `go test ./...`: 41 ok, 0 fail)
- [x] 4.3 `openspec validate add-health-protocol-version --strict` clean
