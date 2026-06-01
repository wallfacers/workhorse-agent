## Why

AGENTS.md is becoming the de facto standard for project-level AI agent instruction files (adopted by opencode, Cursor, and others). workhorse-agent currently has no mechanism to discover or inject project-specific instructions — the system prompt is fully hardcoded or limited to global memory files. Users cannot place an AGENTS.md in their project root and have the agent automatically pick up project-specific coding conventions, architecture notes, or behavioral preferences.

## What Changes

- **New `internal/instructions/` module**: Discovers, loads, and renders AGENTS.md files from the project tree and global config directory.
- **Project-level file discovery**: At session creation, walk from the session workdir upward to the git repository root, searching for `AGENTS.md` (primary) and `CLAUDE.md` (compatibility fallback). First filename with any match wins — all instances of that filename along the path are collected.
- **Global-level file discovery**: Check `~/.workhorse-agent/AGENTS.md` as a user-level instruction file. First existing file wins (exclusive).
- **System prompt injection**: Add an `<instructions>` segment to the system prompt template, positioned after `<environment>` and before `<memory>`. Content is loaded once at session creation and frozen for the session lifetime (same pattern as memory snapshot).
- **Read tool proximity injection**: When the Read tool reads a file, walk upward from the file's directory to the workdir root looking for AGENTS.md files that are NOT already in the system prompt. Inject found files as `<system-reminder>` blocks appended to the Read output. Session-level deduplication prevents the same file from being injected twice.
- **No file size limits**: Unlike memory files (which are LLM-written and capped), AGENTS.md is human-authored project configuration and is not truncated.

## Capabilities

### New Capabilities

- `instructions-loader`: AGENTS.md / CLAUDE.md file discovery (project-level findUp + global-level), snapshot loading at session creation, system prompt block rendering, and Read tool proximity injection with session-level deduplication.

### Modified Capabilities

- `prompt-module`: `SystemPromptInput` struct gains an `Instructions` field. The `SystemPrompt` template gains a `{{if .Instructions}}` segment. `BuildSystemPrompt` passes the new field through.
- `prompt-memory`: The documented assembly order changes from `base → environment → memory` to `base → environment → instructions → memory`. The byte-stable delimiter pattern is reused for the new segment.

## Impact

- **New package**: `internal/instructions/` (~4 files: loader, snapshot, block, resolver).
- **Modified files**:
  - `internal/prompt/builtins.go` — template and struct changes.
  - `internal/session/session.go` — new `InstructionSnapshot` and `InstructionResolver` fields on Session.
  - `cmd/workhorse-agent/cmd_serve.go` — instruction loading in runner factory.
  - `internal/tools/builtin/read.go` — proximity injection call.
  - `internal/tools/tool.go` — possible `Env` struct extension to carry resolver reference.
- **Prompt cache**: The `<instructions>` segment sits after `<environment>` (already dynamic), so it does not fragment the static base prefix cache. Different projects get different instruction content but share the same base+cancelled cache prefix.
- **No API changes**: This is purely server-side prompt assembly. No new endpoints, no config schema changes for MVP.
