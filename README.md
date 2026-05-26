# workhorse-agent

> Research-grade local AI agent server in Go. Single binary, single user, multi-session.
> Speaks MCP 2025-11-25 Streamable HTTP (POST + GET SSE).

## Status

Pre-release. Specs frozen in `openspec/changes/init-workhorse-agent-mvp/`.
Implementation is spec-driven: every change has a written proposal, design
note, per-capability requirements, and a numbered task list. The codebase
should not advance ahead of those documents.

## Compliance / Scope

`workhorse-agent` is an independent reimplementation of a class of local AI
agent servers. It is **not** a clone or port of any existing product. The
reference architecture (messages → tools → loop, parallel tool batching, MCP
integration, sub-agent dispatch, automatic context compaction, file-based
persistence) is studied at a conceptual level and rebuilt from scratch with
original code, identifiers, error strings, on-disk paths, and module layout.

Concretely, the project commits to four invariants:

- **No verbatim copies.** No code, names, error strings, on-disk paths, or
  schema fields are copied from any other agent runtime.
- **No transliteration.** Renaming `foo_bar` to `fooBar` does not make a copy
  original; structures are re-derived from the spec.
- **Public protocols only.** The wire format follows published specs — MCP
  2025-11-25 Streamable HTTP, standard SSE, Anthropic Messages, OpenAI Chat
  Completions. Internal `Message` / `ContentBlock` types are owned by this
  project and bridge to those APIs.
- **No vendor SDKs.** HTTP clients against Anthropic and OpenAI are
  hand-written against the published request/response shapes. The official
  `github.com/anthropics/anthropic-sdk-go` and `github.com/openai/openai-go`
  modules are not imported.

The codebase is therefore distributable as an independent work under the
license below.

## Quick start

Requires Go 1.22 or newer.

```sh
go build -o workhorse-agent ./cmd/workhorse-agent
./workhorse-agent init                 # writes ~/.workhorse-agent/{config.yaml,mcp.json,skills/,agents/}
$EDITOR ~/.workhorse-agent/config.yaml # set providers.anthropic.api_key or providers.openai.api_key
./workhorse-agent serve                # binds 127.0.0.1:7821 by default
```

Hit the server with curl:

```sh
# 1. Create a session.
SESSION=$(curl -sS -X POST http://127.0.0.1:7821/v1/sessions \
  -H 'Content-Type: application/json' \
  -d '{"workdir":"/tmp/proj","provider":"anthropic","model":"anthropic:claude-sonnet-4-6"}' \
  | jq -r .id)

# 2. Open the SSE stream in one terminal.
curl -N -H 'Accept: text/event-stream' \
  "http://127.0.0.1:7821/v1/sessions/$SESSION/stream"

# 3. Send a user message in another.
curl -X POST "http://127.0.0.1:7821/v1/sessions/$SESSION/stream" \
  -H 'Content-Type: application/json' \
  -d '{"type":"user_message","content":"list the files under workdir"}'
```

If a network drops mid-conversation, resume by passing the last seen `id:`
field on either header or query:

```sh
curl -N -H 'Accept: text/event-stream' -H 'Last-Event-ID: 42' \
  "http://127.0.0.1:7821/v1/sessions/$SESSION/stream"

# Or with query string when the client cannot set headers:
curl -N "http://127.0.0.1:7821/v1/sessions/$SESSION/stream?last_event_id=42"
```

## Configuration

Configuration is assembled from four sources, merged in order so each later
source overrides the earlier ones:

1. Built-in defaults from `internal/config/config.go` (`Default()`).
2. YAML file at `~/.workhorse-agent/config.yaml`. Unknown keys are rejected as
   typos; missing keys keep their default value.
3. Environment variables prefixed `WORKHORSE_AGENT_` (see `internal/config/load.go`
   for the explicit allowlist — common bindings are `WORKHORSE_AGENT_HOST`,
   `WORKHORSE_AGENT_PORT`, `WORKHORSE_AGENT_AUTH_ENABLED`,
   `WORKHORSE_AGENT_AUTH_BEARER_TOKEN`, `WORKHORSE_AGENT_PROVIDERS_ANTHROPIC_API_KEY`,
   `WORKHORSE_AGENT_PROVIDERS_OPENAI_API_KEY`, `WORKHORSE_AGENT_LOG_LEVEL`).
4. CLI flags `--host`, `--port`, `--log-level`, `--config`.

`config.yaml` does **not** hot-reload — restart `serve` after editing.
`~/.workhorse-agent/agents/*.yaml` and `~/.workhorse-agent/skills/*/skill.yaml`
are re-scanned on demand.

The full schema (types, defaults, validation ranges) lives in
`internal/config/config.go`. The reference spec is at
`openspec/changes/init-workhorse-agent-mvp/specs/configuration/spec.md`.

## Provider compatibility

Two upstream protocols are supported:

| Provider     | Protocol                                  | Fast model (default)              |
|--------------|-------------------------------------------|-----------------------------------|
| `anthropic`  | Anthropic Messages API (`/v1/messages`)   | `claude-haiku-4-5-20251001`       |
| `openai`     | OpenAI Chat Completions (`/v1/chat/completions`) | `gpt-4o-mini`              |

Both clients are hand-written against the published HTTP request/response
shapes; no vendor SDK is imported. The internal `provider.Message` /
`provider.ContentBlock` types bridge both vendors.

Automatic context compaction uses a smaller "fast" model from the **same
family** as the session's main model — Anthropic sessions compact with Haiku,
OpenAI sessions compact with `gpt-4o-mini`. Compaction never crosses vendors.
See `internal/provider/policy.go` (`ModelPolicy.PickFast`).

## Grep behavior

The built-in `Grep` tool ships gitignore-aware by default. On a typical Node
or Python monorepo the practical effect is "search the source, not the
build output":

- `.git/`, `.hg/`, `.svn/` directories are **always** skipped (hard rule).
- Dot-files and dot-dirs are skipped unless `hidden: true` is set per request.
- Built-in default excludes (full list in
  `internal/tools/builtin/gitignore_walker.go::builtinDefaultExcludes`)
  prune `node_modules/`, `vendor/`, `dist/`, `build/`, `target/`, framework
  caches (`.next/`, `.turbo/`, `.mypy_cache/` …), coverage reports,
  `package-lock.json`, `*.min.js`, `*.lock`, `.DS_Store`, and similar.
  These apply regardless of the `ignore_vcs` flag.
- If the session workdir lives in a git repo, `.gitignore` is honored from the
  repo root down to the workdir, including `.git/info/exclude` and nested
  re-includes (`!important.log`).
- Binary files are detected by a NUL byte in the first 8 KiB and silently
  skipped — no garbled bytes leak into `tool_result.output`.

Per-request overrides (input fields):

- `ignore_vcs: false` — do not apply `.gitignore` (default excludes still
  apply; hard VCS skip still applies).
- `hidden: true` — walk into dot-files and dot-dirs.

Operator overrides (`~/.workhorse-agent/config.yaml`):

```yaml
tools:
  grep:
    workers: 0                 # 0 = min(runtime.NumCPU(), 8); 1 = serial codepath
    respect_gitignore: true    # input.ignore_vcs takes precedence
    default_excludes: []       # non-empty REPLACES the built-in list
```

The parallel walker pool (one walker goroutine + N workers) and
gitignore-driven pruning together give a ~10–20× speedup on multi-thousand
file repositories versus the original serial scan.

## Documentation

- [Protocol reference](docs/protocol.md) — HTTP REST surface and Streamable
  HTTP wire format, including the full event and error enums.
- [Deployment guide](docs/deployment.md) — local single-user setup, nginx
  reverse proxy, systemd unit, Bearer auth, backup.
- [Architecture overview](docs/architecture.md) — module map and key
  cross-package invariants.
- Specs: `openspec/changes/init-workhorse-agent-mvp/`
- Implementation plan: `openspec/changes/init-workhorse-agent-mvp/tasks.md`

## License

[AGPL-3.0](LICENSE). Network use is considered distribution. If you run a
modified version on a server reachable by users, you must offer them the
corresponding source code under the same terms.
