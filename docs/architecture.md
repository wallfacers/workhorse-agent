# workhorse-agent architecture

A single-binary, single-user, multi-session local AI agent server. This
document maps the source tree to responsibilities and calls out the
cross-package invariants that are easy to break without realising.

## Top-down module map

```
workhorse-agent (single binary)
└── cmd/workhorse-agent              CLI assembly (init / serve / version)
    │
    ├── internal/config              4-source loader (defaults < yaml < env < CLI)
    │
    ├── internal/store/sqlite        modernc.org/sqlite — sessions, messages,
    │                                events, tool_calls, permissions
    │
    ├── internal/session             Session struct, 6-state machine, manager
    │   └─ inbox / outbox channels
    │
    ├── internal/agent               Loop, tool Orchestrator, Compactor, retry
    │   │
    │   ├── internal/provider        Provider abstraction, ModelPolicy
    │   │   ├── anthropic            Hand-written Messages API client
    │   │   └── openai               Hand-written Chat Completions client
    │   │
    │   ├── internal/permission      5-decision model + glob matcher
    │   │
    │   ├── internal/tools           ToolRegistry, Tool interface, truncation
    │   │   ├── pathguard            Path canonicalisation + workdir check
    │   │   ├── bash                 Bash tool + danger guard + env filter
    │   │   ├── builtin              Read / Write / Edit / Grep
    │   │   └── dispatch             Sub-agent Dispatch tool
    │   │
    │   ├── internal/coord           agent_type loader (~/.workhorse-agent/agents)
    │   │
    │   ├── internal/skills          Skills loader + LoadSkill tool
    │   │
    │   └── internal/mcp             MCP host + adapter (stdio + HTTP transports)
    │
    └── internal/api                 HTTP server, middleware, Streamable HTTP
        ├── server.go                Router, lifecycle, stream-slot map
        ├── middleware.go            Origin / Bearer / MaxBytes / logging / recovery
        ├── sessions.go              REST CRUD (POST/GET/DELETE, cancel, compact)
        ├── stream.go + stream_post  ClientEvent intake (POST → 202)
        ├── stream_get.go            SSE writer + Last-Event-ID replay
        ├── shutdown.go              7-step graceful shutdown
        ├── health.go                /health
        └── protocol/                ClientEvent / ServerEvent / ErrorCode types
```

## Package responsibility table

| Package                          | Responsibility                                                                                                            | Key types / files                                          |
|----------------------------------|---------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------|
| `cmd/workhorse-agent`            | Subcommand dispatch (`init`, `serve`, `version`). Wires every other package and runs the graceful shutdown sequence.       | `main.go`, `cmd_init.go`, `cmd_serve.go`                   |
| `internal/config`                | Configuration schema; merges defaults < yaml < `WORKHORSE_AGENT_*` env < CLI flags; validates ranges; path expansion.      | `config.go`, `load.go`                                     |
| `internal/store/sqlite`          | Persistence on `modernc.org/sqlite` (no CGO). Schema migrations, CRUD, incremental event queries (`idx > N`).              | `sqlite.go`, `migrations.go`, `crud.go`                    |
| `internal/session`               | One `Session` per conversation. Owns the six-state machine, the inbox / outbox channels, the per-session write lock, and the `Emit` helper that allocates `idx` inside the events insert. | `session.go`, `manager.go`                                 |
| `internal/agent`                 | The agent loop. LLM call → tool batches → re-call. Top-level `recover()` synthesises a cancelled `tool_result` and emits `error{internal_panic}`. Compaction and provider retry live here. | `loop.go`, `orchestrator.go`, `compaction.go`, `retry.go`  |
| `internal/provider`              | Provider abstraction (`Provider`, `Request`, `ProviderEvent`, internal `Message` / `ContentBlock`). `ModelPolicy` enforces the same-family rule for fast/compaction model selection. | `provider.go`, `policy.go`, `retry.go`                     |
| `internal/provider/anthropic`    | Hand-written HTTP + SSE client for Anthropic Messages API. No vendor SDK.                                                  | `anthropic.go`, `stream_state.go`, `wire.go`               |
| `internal/provider/openai`       | Hand-written HTTP client for OpenAI Chat Completions with incremental `tool_calls` accumulation and the `role:"tool"` translation. No vendor SDK. | `openai.go`, `wire.go`                                     |
| `internal/tools`                 | `Tool` interface, `ToolRegistry`, `AllowedTools` filtering, result truncation.                                             | `tool.go`, `registry.go`, `truncate.go`                    |
| `internal/tools/pathguard`       | The single point that canonicalises user-supplied paths. `filepath.Clean` → `EvalSymlinks` (with leaf fallback) → `filepath.Rel` workdir check → `O_NOFOLLOW` open on Linux/macOS. Every file-touching tool MUST go through this. | `pathguard.go`, `open_unix.go`, `open_other.go`            |
| `internal/tools/bash`            | The Bash tool: `exec.CommandContext` in a process group, SIGTERM → 1.5 s → SIGKILL on cancel. The danger guard (eight regex families) and env filter (`LD_PRELOAD`, `DYLD_*`, vetted `NODE_OPTIONS`) live here. | `bash.go`, `danger.go`, `envfilter.go`                     |
| `internal/tools/builtin`         | `Read`, `Write` (atomic temp+rename), `Edit` (exact-match replace), `Grep` (pure Go regex).                                | `read.go`, `write.go`, `edit.go`, `grep.go`                |
| `internal/tools/dispatch`        | The `Dispatch` tool. Creates a child session with inherited workdir / env / provider / model and a fresh history. Cascades cancellation. | `dispatch.go`, `pump.go`                                   |
| `internal/permission`            | Five-decision model (`allow_once` / `allow_session` / `allow_permanent` / `deny` / `deny_permanent`). Glob matcher with `*`, `**`, `?`, literal segments. Permanent rules persist; session and once stay in memory. | `manager.go`, `glob.go`                                    |
| `internal/coord`                 | Loads `~/.workhorse-agent/agents/*.yaml` at startup and before every `Dispatch` call.                                       | `agenttype.go`                                             |
| `internal/mcp`                   | JSON-RPC 2.0 MCP client (initialize, tools/list, tools/call, notifications/cancelled). Stdio + HTTP transports. `host.go` runs the per-process MCP host; `adapter.go` wraps each MCP tool as an internal `Tool`. | `jsonrpc.go`, `transport_stdio.go`, `transport_http.go`, `host.go`, `adapter.go` |
| `internal/skills`                | Scans `~/.workhorse-agent/skills/*/skill.yaml`, injects the catalogue into the system prompt, and exposes the `LoadSkill` tool that swaps `AllowedTools`. | `loader.go`, `injector.go`, `loadtool.go`                  |
| `internal/api`                   | HTTP surface (REST + Streamable HTTP). Owns the per-session stream slot, the seven-step shutdown sequence, and the Bearer / Origin / max-bytes middleware chain. | `server.go`, `middleware.go`, `stream*.go`, `shutdown.go`  |
| `internal/api/protocol`          | Pure-data ClientEvent / ServerEvent / ErrorCode definitions. Imported by the api server and by `pkg/client`. | `protocol.go`                                              |
| `pkg/client`                     | Reusable Go client SDK over the protocol package.                                                                          | —                                                          |
| `web/`                           | Embedded reference web UI mounted at `/ui`.                                                                                | —                                                          |

## Cross-package invariants

These are the invariants that span more than one package. Anyone editing
the affected files should read this section first.

### Single active GET per session

There is at most one live SSE writer per session at any moment. The
session's write lock serialises three things: appending to the outbox
channel, swapping the SSE writer when a new GET arrives, and replaying
events under `Last-Event-ID`. Holding this lock guarantees that the
outbox order and the SSE delivery order match.

The handover sequence is enforced in `internal/api/stream_get.go` and
tracked through the `streamSlots` map on `api.Server`. Skipping the
"write `: superseded`, then close the old writer, then admit the new
handler" order risks a brief window where two writers race on the same
session.

### `idx` = SSE `id:` = events table primary key

`ServerEvent.idx` is an `int64` allocated by SQLite's
`INTEGER PRIMARY KEY AUTOINCREMENT` on the `events` table. The same
value becomes the `id:` field on the SSE frame and the value the client
sends back in `Last-Event-ID`. Three places must agree:

- `internal/store/sqlite/migrations.go` (the table definition).
- `internal/session/session.go` (the `Emit` helper that inserts the row
  and stamps `idx` on the in-memory `ServerEvent` before publishing).
- `internal/api/stream_get.go` (the SSE writer that prints `id: <idx>`
  and the `Last-Event-ID` parser).

Changing any one of these without the others corrupts replay.

### Seven-step graceful shutdown ordering

Implemented in `internal/api/shutdown.go`. The strict order matters:

1. Stop accepting new HTTP connections (existing SSE stays open).
2. Cancel every non-`Idle` session — produces synthesised cancelled
   `tool_result` rows and `interrupted` events.
3. Wait for step 2's events to land in the events table and the
   outbox channel.
4. On each SSE stream emit `error{server_shutdown}` and flush, then
   close the writer.
5. Wait for session goroutines to exit, bounded by
   `server.graceful_shutdown_timeout_seconds`.
6. Close SQLite and the MCP host.
7. Exit 0 (or 1 on timeout).

The key invariant: cancelled `tool_result` and `interrupted` MUST reach
the client before `error{server_shutdown}`. Reordering steps 2 and 4
loses these events for ephemeral sessions because the SSE stream is the
only place they exist.

### Path resolution flows through `pathguard`

`Read`, `Write`, `Edit`, and every MCP adapter that opens a file MUST
call `internal/tools/pathguard`. Bypassing it means the workdir escape
checks and the `O_NOFOLLOW` defence are not applied.

### Bash env filter is the only env merge gate

Any code path that builds a child-process environment — the Bash tool,
MCP stdio server launch, session-scoped env overrides — must run the
result through `internal/tools/bash/envfilter`. The filter drops
`LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`, the `DYLD_*` family,
`PYTHONPATH`, `PYTHONSTARTUP`, and any `NODE_OPTIONS` value that
contains a dangerous token (`--require`, `--import`,
`--experimental-loader`, `--inspect`, `--inspect-brk`). Centralising
the table means one fix repairs every transport.

### Persistence is `modernc.org/sqlite`, no CGO

The project compiles without CGO on every supported platform. The
SQLite driver is `modernc.org/sqlite`. Do not import `mattn/go-sqlite3`
or the C bindings; the build matrix and the static-binary release
artefacts assume a pure-Go toolchain.

### No vendor SDKs for the LLM providers

`internal/provider/anthropic` and `internal/provider/openai` are
hand-written against the public HTTP specs. The vendor packages
(`github.com/anthropics/anthropic-sdk-go`,
`github.com/openai/openai-go`) are intentionally not in `go.mod`. This
keeps the licence story for the project's "independent reimplementation"
posture intact.
