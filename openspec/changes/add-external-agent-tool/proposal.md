## Why

`workhorse-agent` today can only orchestrate work through its own tool surface — `Bash`, `Read`, `Write`, `Edit`, `Dispatch` (sub-session to an internal `agent_type`), `LoadSkill`, plus MCP-attached tools. The user's longer-term goal is task-oriented multi-agent operation: a main agent that hands off work to *other CLIs already installed on the box* — Claude Code for editing, Codex for review, `aider` for inline refactors, `gemini-cli` for experimentation, `pandoc` / `libreoffice` / `ffmpeg` / `playwright` for office and browser work. Every one of those is currently reachable only by hand-rolling a Bash invocation per turn, with no streaming integration, no resume semantics, no smoke testing, no provenance, and no way to extend the system without editing Go code.

This change introduces the *foundation* for treating external CLIs as first-class actors: a typed `adapter` schema, a hot-reloaded registry under `~/.workhorse-agent/external-agents/`, a single new `ExternalAgent` tool that runs `sub_agent`-class adapters as managed child processes with cancellation and timeout, and a `<environment>` block injected into the system prompt that enumerates `cli_tool`-class binaries the model can reach via plain `Bash`. The LLM-driven *generation* of adapters from `--help` output is a separate change (`add-llm-adapter-generator`) that depends on this one.

## What Changes

- Introduce **`external-agents`** as a new top-level concept with two classes:
  - **`sub_agent`**: long-running interactive CLIs (e.g. `claude`, `codex`, `aider`) reached via a dedicated `ExternalAgent` tool that handles invocation, streaming-aware output collection, cancellation, timeout, and sandboxed env.
  - **`cli_tool`**: one-shot utilities (e.g. `pandoc`, `ffmpeg`, `libreoffice`, `playwright`, `jq`) the model is expected to call via the existing `Bash` tool; they are *only* surfaced as hints in a new `<environment>` system-prompt block.
- Define the **adapter YAML schema** (complete version, ~50 lines per adapter) covering identity, invocation, session/resume, output parsing, lifecycle, capabilities, security, smoke test, and provenance. JSON Schema authored alongside.
- Add `internal/extagent/` package owning: adapter parsing & validation, registry, hot-reload (rescan on session start, parallel to skills), smoke-test runner, sub-process driver with cancel-signal/grace/SIGKILL semantics, output collection per declared `output.format`.
- Add `internal/tools/extagent/` registering a new **`ExternalAgent`** tool (`agent_name` + `prompt` + optional `inputs`), exposed only when at least one `sub_agent`-class adapter is loaded.
- Add `internal/extagent/pathscan/` performing a curated PATH scan (builtin allowlist plus user-configurable extras), feeding the result into a new `{{.Environment}}` slot rendered by the existing `internal/prompt/builtins.go` `SystemPrompt` template.
- Ship **3 built-in adapters** as reference + immediate usefulness: `claude-code.yaml`, `codex.yaml`, `aider.yaml`. They are embedded in the binary via `//go:embed` and seed the registry when no on-disk adapter overrides them.
- Extend `internal/tools/bash/envfilter.go` use-sites so `ExternalAgent` reuses the exact same env-stripping rules as the `Bash` tool — sub-agents inherit no broader privilege.
- Extend `internal/config` with optional `external_agents.dir`, `external_agents.pathscan.extra`, `external_agents.pathscan.disabled`, `external_agents.smoke_test.cache_ttl` keys. No existing config field changes.

## Capabilities

### New Capabilities
- `external-agents`: adapter lifecycle (schema, validation, loading, hot-reload), `ExternalAgent` tool surface, sub-process driver (cancellation, timeout, env isolation), smoke-test infrastructure, builtin-adapter shipping.
- `environment-detection`: PATH scan policy (builtin allowlist + user extras), `<environment>` system-prompt block format, integration with `internal/prompt/builtins.go`.

### Modified Capabilities
<!-- None at requirement level. Tools register through the existing `internal/tools/registry.go` without changing its spec-level requirements; the new prompt slot is additive (empty when nothing is detected) and does not change the `agent-loop` or `tool-system` specs. -->

## Impact

- **Code**: new `internal/extagent/` package (parser, validator, registry, driver, smoke runner, embedded builtin adapters), new `internal/tools/extagent/` (single `ExternalAgent` tool), new `internal/extagent/pathscan/` (PATH walker + builtin allowlist), one new `{{.Environment}}` slot in `internal/prompt/builtins.go` `SystemPrompt`, and one new helper `EnvironmentBlock()` in `internal/prompt`.
- **Persistence**: none. Adapters live on disk under `~/.workhorse-agent/external-agents/`; smoke-test results cache in a per-adapter sibling `.smoke` file (plain JSON). No new SQLite tables, no schema migration.
- **Dependencies**: none added. YAML parsing reuses the existing `gopkg.in/yaml.v3` already used by skills/agents loaders. JSON Schema validation reuses `github.com/santhosh-tekuri/jsonschema/v6` already vendored for MCP descriptors.
- **Config**: optional keys under `external_agents.*` namespace. All have safe defaults; existing installs are unaffected until a user drops an adapter file or installs a binary on the allowlist.
- **Network posture**: unchanged. `ExternalAgent` runs child processes locally; no new network bind.
- **Security**: `ExternalAgent` reuses the Bash envfilter; it does NOT extend `dangerous-command` patterns (those guard the `Bash` tool's user-supplied command string and have no analog when the adapter pre-declares the binary path). A new `security.trusted: false` adapter field gates first-invocation approval for non-builtin adapters.
- **Out of scope**: LLM-driven adapter generation (separate change `add-llm-adapter-generator`); streaming-forwarding of sub-agent output into the parent SSE channel (MVP collects then emits); auto-installation of missing binaries; cross-machine adapter sync.
