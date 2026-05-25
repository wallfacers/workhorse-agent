# Capability verification audit

This document maps each of the 10 OpenSpec capabilities under
`openspec/changes/init-workhorse-agent-mvp/specs/` to the tests that exercise
its Scenarios. It serves task 15.4 from the implementation plan: a
single-page reference for manual verification.

## How to run the full audit

```sh
# Unit + integration tests for every capability, with the race detector.
go test -race -count=1 ./...

# Cross-arch builds (15.3).
scripts/build.sh all

# Lint (15.2).
golangci-lint run

# Nginx reverse-proxy integration (9.22) — requires Docker.
go test -tags=nginx ./internal/api/...
```

## Per-capability mapping

| Capability | Scenarios | Primary implementation | Test files |
|---|---:|---|---|
| agent-loop | 8 | `internal/agent/loop.go`, `internal/agent/compaction.go`, `internal/agent/retry.go` | `internal/agent/loop_test.go`, `internal/agent/orchestrator_test.go` |
| api-protocol | 44 | `internal/api/server.go`, `internal/api/stream_*.go`, `internal/api/sessions.go`, `internal/api/shutdown.go`, `internal/api/protocol/` | `internal/api/sessions_test.go`, `internal/api/stream_post_test.go`, `internal/api/stream_get_test.go`, `internal/api/middleware_test.go`, `internal/api/health_test.go`, `internal/api/shutdown_test.go`, `internal/api/protocol/protocol_test.go`, `test/e2e/e2e_test.go` |
| configuration | 8 | `internal/config/config.go`, `internal/config/load.go` | `internal/config/config_test.go` |
| mcp-integration | 8 | `internal/mcp/host.go`, `internal/mcp/transport_*.go`, `internal/mcp/adapter.go`, `internal/mcp/jsonrpc.go` | `internal/mcp/mcp_test.go` |
| multi-agent | 10 | `internal/coord/agenttype.go`, `internal/tools/dispatch/dispatch.go`, `internal/tools/dispatch/pump.go` | `internal/coord/agenttype_test.go`, `internal/tools/dispatch/dispatch_test.go` |
| permission-control | 6 | `internal/permission/manager.go`, `internal/permission/matcher.go` | `internal/permission/manager_test.go` |
| provider-abstraction | 16 | `internal/provider/provider.go`, `internal/provider/anthropic/anthropic.go`, `internal/provider/openai/openai.go`, `internal/provider/retry.go` | `internal/provider/provider_test.go`, `internal/provider/anthropic/anthropic_test.go`, `internal/provider/openai/openai_test.go`, `test/mockprovider/mockprovider_test.go` |
| session-management | 19 | `internal/session/session.go`, `internal/session/manager.go`, `internal/store/sqlite/` | `internal/session/session_test.go`, `internal/session/manager_test.go`, `internal/session/leak_test.go`, `internal/store/sqlite/sqlite_test.go` |
| skills-loader | 6 | `internal/skills/loader.go`, `internal/skills/injector.go`, `internal/skills/loadtool.go` | `internal/skills/loader_test.go`, `internal/skills/injector_test.go`, `internal/skills/loadtool_test.go`, `internal/skills/integration_test.go` |
| tool-system | 17 | `internal/tools/registry.go`, `internal/tools/builtin/`, `internal/tools/bash/`, `internal/tools/pathguard/`, `internal/tools/truncate.go` | `internal/tools/registry_test.go`, `internal/tools/builtin/builtin_test.go`, `internal/tools/bash/{bash,danger,envfilter}_test.go`, `internal/tools/pathguard/pathguard_test.go`, `internal/tools/truncate_test.go` |

## Spot-check matrix for the highest-risk scenarios

These are the load-bearing invariants worth re-verifying by reading the
relevant test before a release.

| Invariant | Source | Verifying test |
|---|---|---|
| Graceful-shutdown event ordering (cancelled/interrupted BEFORE server_shutdown) | `internal/api/shutdown.go` | `test/e2e/e2e_test.go::TestE2E_GracefulShutdownOrdering` |
| Last-Event-ID replay under concurrent writes (no dup/skip) | `internal/api/stream_get.go` | `test/e2e/e2e_test.go::TestE2E_LastEventIDRace` |
| Single active GET handover (race-free) | `internal/api/stream_get.go` | `internal/api/stream_get_test.go` |
| Cancel cascade synthesises cancelled tool_results | `internal/agent/loop.go::finishCancelledTurn` | `internal/agent/loop_test.go::TestLoop_CancelMidTurn_*` |
| Panic recovery keeps session usable | `internal/agent/loop.go::handlePanic` | `internal/agent/loop_test.go::TestLoop_ToolPanicDoesNotKillSession` |
| Bash dangerous-command guard | `internal/tools/bash/danger.go` | `internal/tools/bash/danger_test.go` |
| Env filter (LD_PRELOAD etc.) strips before exec | `internal/tools/bash/envfilter.go` | `internal/tools/bash/envfilter_test.go` |
| Path traversal blocked (Read/Write/Edit/Grep) | `internal/tools/pathguard/pathguard.go` | `internal/tools/pathguard/pathguard_test.go` |
| `crypto/subtle.ConstantTimeCompare` for bearer token | `internal/api/middleware.go::bearerMW` | `internal/api/middleware_test.go`, `test/e2e/e2e_test.go::TestE2E_BearerAuthAtAllEndpoints` |
| Sub-agent depth cap (max_depth=5) | `internal/tools/dispatch/dispatch.go` | `internal/tools/dispatch/dispatch_test.go::TestDispatch_MaxDepthRejects` |
| Goroutine count returns to baseline after 100 create/delete cycles | `internal/session/manager.go` | `internal/session/leak_test.go::TestManager_NoGoroutineLeak_After100Cycles` |

## Things NOT covered by `go test` alone

- nginx reverse-proxy SSE — needs Docker + `-tags=nginx` (task 9.22).
- Real provider round-trip — needs an Anthropic or OpenAI API key (task 14.5).
- Multi-arch artifact spot-check on macOS arm64 hardware — `scripts/build.sh all` only proves they build; a sanity-run on a Mac is task 15.3.
