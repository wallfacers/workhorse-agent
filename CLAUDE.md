# Repo guidance for AI coding assistants

> **Project renamed (2025-05-24):** This project was formerly known as
> `data-agent` / `dataagent`. The current name is **workhorse-agent**.
> Module path: `github.com/wallfacers/workhorse-agent`. Binary: `workhorse-agent`.
> Config dir: `~/.workhorse-agent/`. All old names have been replaced throughout
> the codebase. If you encounter any remaining references to the old name, treat
> them as bugs and fix them.

## Project shape

Go 1.22+, single binary, local single-user multi-session AI agent server. Specs
live under `openspec/changes/init-workhorse-agent-mvp/`. Treat those specs as the
source of truth: proposal, design, the per-capability `specs/*/spec.md`, and
the implementation backlog in `tasks.md`.

When implementing a task from `tasks.md`, mark its checkbox `[x]` immediately
after the work lands, and keep the surrounding spec text untouched unless the
task explicitly says to edit it.

## Code style

- Default to no comments. Add one only when the *why* would surprise a future
  reader (a non-obvious invariant, a workaround for a specific OS quirk, a
  reference to a spec scenario by name).
- Don't preface comments with "added for task 5.13" or similar. The git history
  carries that.
- Follow `gofmt`/`gofumpt` output. `golangci-lint run` must stay clean.
- Avoid `panic` outside `main` and `init`. Agent loop has a top-level
  `recover()` and synthesizes a cancelled tool_result + emits
  `error{code:"internal_panic"}` instead of crashing the session.

## Network posture

- Bind `127.0.0.1` by default. Never bind `0.0.0.0` unless an operator
  explicitly sets `server.host`.
- Bearer-token comparison uses `crypto/subtle.ConstantTimeCompare`. The token
  value must never appear in logs, traces, or error messages.
- Origin enforcement is exact-host match via `url.Parse`, not string contains.

## Dangerous-command guard (Bash tool)

Eight pattern families force a permission prompt regardless of any
`allow_permanent` rule:

`rm -rf /`, `rm -rf ~`, `dd of=/dev/`, `mkfs.*`, redirect-to-block-device,
fork bomb, `chmod -R 777 /`, `shutdown`/`reboot`/`halt`/`poweroff`,
`base64 -d | sh` / `curl | bash`.

Known bypasses (hex escapes, absolute paths, alias indirection, base64
decoding into `sh`) are documented and explicitly **not** caught by MVP. Tests
must assert the bypasses are not caught — that's the spec, not a regression.

## Bash env isolation

The Bash tool strips a precise set of environment variables before exec:

- Exact match: `LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`,
  `DYLD_INSERT_LIBRARIES`, `DYLD_LIBRARY_PATH`,
  `DYLD_FALLBACK_LIBRARY_PATH`, `DYLD_FORCE_FLAT_NAMESPACE`,
  `PYTHONPATH`, `PYTHONSTARTUP`.
- Prefix: any variable starting with `DYLD_`.
- `NODE_OPTIONS` is shlex-tokenized; if any token starts with `--require`,
  `--import`, `--experimental-loader`, `--inspect`, or `--inspect-brk`, the
  variable is dropped. Other tokens pass through.

This logic lives in `internal/tools/bash/envfilter.go` and is shared by every
session-level env merge.

## Path traversal

All file-touching tools (Read, Write, Edit, plus any MCP adapter that touches
the filesystem) MUST resolve user-supplied paths via
`internal/tools/pathguard`:

1. `filepath.Clean`
2. `filepath.EvalSymlinks` (with a parent-directory fallback if the leaf does
   not exist yet — Write/Edit case)
3. `filepath.Rel` against the session workdir; reject if it escapes
4. Open with `O_NOFOLLOW` on Linux/macOS; on other platforms re-check with
   `os.Lstat` after open

## Persistence

`modernc.org/sqlite` only. No CGO. Events table uses `INTEGER PRIMARY KEY
AUTOINCREMENT` and is append-only; the `idx` value is the SSE `id:` field.
Session/message/agent IDs are ULIDs.

## Hot reload

`config.yaml` does NOT hot-reload (requires restart). Only
`~/.workhorse-agent/agents/*.yaml` and `~/.workhorse-agent/skills/*/skill.yaml` are
re-scanned dynamically.

## Memory subsystem

Two memory layers ship with the current version:

- **L1 Prompt Memory**: `internal/memory/` owns `Snapshot`, `Writer`, and `Block`.
  Two files (`MEMORY.md` and `USER.md`) live under `~/.workhorse-agent/memories/`.
  Content is loaded once at session start as an immutable snapshot and injected
  into the system prompt at the agent-loop call site (`internal/agent/loop.go`).
  Mid-session writes via `memory_write` update disk but do **not** mutate the
  snapshot (preserves Anthropic prompt-cache hit rate). Char limits are enforced
  at write time (default: MEMORY ≤ 2200, USER ≤ 1375 code points).

- **L2 Session Archive**: FTS5 virtual table `messages_fts` mirrors `messages`
  via triggers; backfilled on migration. The `session_search` tool (`internal/tools/sessionsearch/`)
  runs MATCH queries with CJK trigram synthesis and LIKE fallback, returning raw
  matches + context. No LLM summarization.

Three new built-in tools: `memory_read`, `memory_write`, `session_search`. They
are registered through the existing tool registry and gated by `allowed_tools`.

## External agents

External sub-agent CLIs (claude, codex, aider, etc.) are exposed to the LLM via
the `ExternalAgent` tool. Each adapter is defined by a YAML file validated against
`internal/extagent/schema/adapter.schema.json`.

**Adapter location**: Builtin adapters live in `internal/extagent/builtins/*.yaml`
(embedded via `//go:embed`). User-defined adapters go in
`~/.workhorse-agent/external-agents/*.yaml`. On-disk files override builtins by
name.

**Classes**: `sub_agent` (invoke via ExternalAgent tool) vs `cli_tool` (invoke
via Bash). Only `sub_agent` adapters appear in the tool's `agent_name` enum.

**Security model**: `security.trusted: true` (builtins) skip the per-session
approval prompt. Untrusted adapters prompt once per session; approval is not
persisted across sessions.

**Smoke tests**: Each adapter declares a `smoke_test` stanza. Results are cached
in `~/.workhorse-agent/cache/smoke/<name>.smoke` with a configurable TTL
(default 168 hours = 7 days). Adapters that fail smoke are excluded from the
tool surface.

**PATH scanning**: `internal/extagent/pathscan/` probes a curated allowlist of
binary names on `$PATH` to detect installed CLIs. Extend via
`external_agents.pathscan.extra` in config; disable specific probes via
`external_agents.pathscan.disabled`.

**To add a new adapter manually**: create a YAML file in
`~/.workhorse-agent/external-agents/` following the schema. Filename stem must
match the `name:` field. Restart the server (adapter YAMLs are loaded at
session creation, not hot-reloaded).
