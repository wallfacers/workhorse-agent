# data-agent

> Research-grade local AI agent server in Go. Single binary, single user, multi-session.
> Speaks MCP 2025-11-25 Streamable HTTP (POST + GET SSE).

## Status

Pre-release. Specs frozen in `openspec/changes/init-data-agent-mvp/`. Implementation in progress.

## Scope

`data-agent` is an independent reimplementation of a class of local AI agent servers.
It is **not** a clone or port of any existing product. The reference architecture
(messages → tools → loop, parallel tool batching, MCP integration, sub-agent
dispatch, automatic context compaction, file-based persistence) is studied at a
conceptual level and rebuilt from scratch with original code, identifiers,
strings, and module layout.

Concretely:

- No code, names, error strings, on-disk paths, or schema fields are copied from
  any other agent runtime.
- The wire format follows the publicly published MCP specification
  (2025-11-25 Streamable HTTP) and standard SSE semantics — not a private
  protocol.
- Provider-side message shapes (Anthropic Messages, OpenAI Chat Completions)
  follow each vendor's public API and are bridged through a small internal
  message type owned by this project.

The codebase is therefore distributable as an independent work, subject to the
license below.

## License

[AGPL-3.0](LICENSE). Network use is considered distribution. If you run a
modified version on a server reachable by users, you must offer them the
corresponding source code under the same terms.

## Documentation

- Specs: `openspec/changes/init-data-agent-mvp/`
- Implementation plan: `openspec/changes/init-data-agent-mvp/tasks.md`
- Protocol reference (after task 14.2): `docs/protocol.md`
- Deployment guide (after task 14.2.1): `docs/deployment.md`
- Architecture overview (after task 14.3): `docs/architecture.md`

## Build

Requires Go 1.22 or newer.

```sh
go build -o dataagent ./cmd/dataagent
```

A static stripped build script ships under `scripts/build.sh`.

## Run

```sh
./dataagent init    # generate ~/.dataagent/{config.yaml,mcp.json,skills/,agents/}
./dataagent serve   # bind 127.0.0.1:7821 by default
```

Configuration reference: `openspec/changes/init-data-agent-mvp/specs/configuration/spec.md`.
