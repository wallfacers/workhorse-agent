# prompt-memory Specification

## Purpose
TBD - created by archiving change add-memory-l1-l2. Update Purpose after archive.
## Requirements
### Requirement: Memory file location and naming

The system SHALL store exactly two memory files per profile: `MEMORY.md` (agent-curated facts and instructions) and `USER.md` (stable user identity and preferences). Both files MUST reside directly under `<profileDir>/memories/`, where `profileDir` defaults to `~/.workhorse-agent/` and is controlled by the existing profile-directory resolution. No nested subdirectories are read or written by the memory subsystem.

#### Scenario: Default profile directory resolution

- **WHEN** the server starts with no `memory.dir` override and no `WORKHORSE_AGENT_HOME` env var
- **THEN** memory files are resolved to `~/.workhorse-agent/memories/MEMORY.md` and `~/.workhorse-agent/memories/USER.md`

#### Scenario: Missing memory file treated as empty

- **WHEN** a session starts and one or both memory files do not exist on disk
- **THEN** the snapshot for the missing file is treated as an empty string, no error is raised, and the file is NOT created until a `memory_write` actually writes content

#### Scenario: Unexpected files in memories directory are ignored

- **WHEN** the memories directory contains files other than `MEMORY.md` and `USER.md` (e.g. backups, editor swap files)
- **THEN** the loader ignores them silently and does not list, read, or modify them

### Requirement: Snapshot loading at session start

The system SHALL read both memory files exactly once when a session is created and bind the resulting `(memory_md, user_md)` pair to that session as an immutable snapshot. The snapshot MUST be loaded before the first system prompt is constructed for that session.

#### Scenario: New session loads current disk state

- **WHEN** a session is created via `POST /sessions`
- **THEN** the server reads both memory files from disk at session-creation time and attaches the result to the session as a read-only snapshot

#### Scenario: Snapshot is immutable for the session lifetime

- **WHEN** any code path attempts to mutate a session's loaded memory snapshot after the session has started
- **THEN** the attempt MUST be rejected and the snapshot returned to callers remains byte-identical to what was loaded at session start

#### Scenario: Subsequent session reflects disk changes

- **WHEN** memory files have been modified (by `memory_write` or by the user editing the file directly) between session A finishing and session B starting
- **THEN** session B's snapshot reflects the post-edit disk content, while session A's snapshot remained the pre-edit content for its full duration

### Requirement: System prompt injection

The system SHALL inject memory snapshot content into the system prompt of every
session via a single `{{.Memory}}` template variable rendered by the
`internal/prompt` package. The memory block SHALL be positioned **after** the
static base段, the `<environment>` block, and the `<instructions>` block
(组装顺序为 `base → environment → instructions → memory`，见 agent-loop spec
「System prompt 组装顺序优先静态前缀」), so that the static cache prefix precedes
the dynamic memory content. When both memory files are empty, the variable expands
to an empty string and the system prompt MUST NOT contain any memory-related
framing or headers.

#### Scenario: Non-empty memory rendered with stable delimiters

- **WHEN** a session has non-empty `MEMORY.md` or `USER.md` content
- **THEN** the rendered system prompt contains the memory text within byte-stable
  delimiters (so that prompt cache prefixes remain identical across turns of the
  same session), positioned after the base, environment, and instructions segments

#### Scenario: Empty memory produces no memory section

- **WHEN** both memory files are empty for a session
- **THEN** the rendered system prompt contains no memory-related text, headers, or
  delimiters

### Requirement: Character-limit enforcement on writes

The system SHALL enforce maximum character counts on memory files at write time, counted as Unicode code points (NOT bytes). Defaults are `MEMORY.md` ≤ 2200 code points and `USER.md` ≤ 1375 code points. Limits are configurable via `memory.memory_char_limit` and `memory.user_char_limit`. A write whose resulting content would exceed the active limit MUST be rejected atomically — the file on disk MUST NOT be modified.

#### Scenario: Write within limit succeeds

- **WHEN** `memory_write` is called with content whose code-point count is at or below the active limit
- **THEN** the file is replaced or appended (per mode) and the tool returns `{accepted: true, char_count, char_limit}`

#### Scenario: Write over limit rejected without disk mutation

- **WHEN** `memory_write` is called with content that would push the file's code-point count over the active limit
- **THEN** the tool returns a structured error with code `memory_too_large` reporting both `limit` and `actual`, and the on-disk file contents are byte-identical to their pre-call state

#### Scenario: Code-point counting handles CJK correctly

- **WHEN** content contains CJK characters that occupy multiple bytes in UTF-8
- **THEN** the character count is the number of Unicode code points (e.g. "你好" counts as 2), not the byte length

### Requirement: memory_read tool

The system SHALL expose a `memory_read` tool that returns the current on-disk content of a specified memory file along with metadata. The tool MUST read directly from disk (NOT from the session's frozen snapshot) so that the calling agent can observe writes made earlier in the same session.

#### Scenario: Read returns current disk content

- **WHEN** `memory_read` is called with `kind: "memory"` (or `"user"`)
- **THEN** the tool returns `{content, char_count, char_limit}` where `content` is the verbatim file content read from disk at call time

#### Scenario: Read of missing file returns empty content

- **WHEN** `memory_read` is called and the target file does not exist
- **THEN** the tool returns `{content: "", char_count: 0, char_limit}` and does NOT create the file

#### Scenario: Invalid kind rejected

- **WHEN** `memory_read` is called with any `kind` other than `"memory"` or `"user"`
- **THEN** the tool returns a structured error with code `invalid_kind` and performs no disk access

### Requirement: memory_write tool

The system SHALL expose a `memory_write` tool that updates a specified memory file using one of two modes: `replace` (overwrite the entire file) and `append` (concatenate after existing content with a separating newline if needed). The tool MUST perform the write atomically (write-to-temp + rename) and MUST acquire an exclusive file lock for the duration of the read-modify-write cycle on append.

#### Scenario: Replace mode overwrites file atomically

- **WHEN** `memory_write` is called with `mode: "replace"` and accepted content
- **THEN** the new content fully replaces the file, with the write performed via temp-file + rename so an interrupted call leaves either the pre-call or post-call state on disk (never partial)

#### Scenario: Append mode reads current content under lock

- **WHEN** `memory_write` is called with `mode: "append"`
- **THEN** the tool holds an exclusive advisory file lock from the moment it reads the current content until the moment the rename completes, so a concurrent append cannot lose data

#### Scenario: Mode defaults to replace

- **WHEN** `memory_write` is called without an explicit `mode` field
- **THEN** the tool treats it as `mode: "replace"`

#### Scenario: Successful write surfaces delayed-effect hint

- **WHEN** `memory_write` succeeds during an active session
- **THEN** the tool response includes `next_session_effective: true`, signaling that the new content will be visible to the model only at the next session start

### Requirement: Delayed-effect semantics

The system MUST guarantee that successful `memory_write` calls during an active session do NOT modify that session's already-injected memory snapshot, nor cause the system prompt rendered for any subsequent turn of the same session to change.

#### Scenario: Write during session does not change snapshot

- **WHEN** an agent calls `memory_write` mid-session and the call succeeds
- **THEN** subsequent turns of the same session continue to render the system prompt with the pre-write memory content

#### Scenario: Write effect observable only at next session

- **WHEN** session A writes memory and then session B is created
- **THEN** session B's snapshot reflects the post-write content

### Requirement: Path safety for memory writes

The system MUST refuse to write outside `<profileDir>/memories/` regardless of the content of the `kind` parameter. The implementation MUST resolve the target path through `internal/tools/pathguard` in a memory-scoped mode that uses the memories directory as the containment root, with symlink resolution and `O_NOFOLLOW` semantics consistent with other file tools.

#### Scenario: Symlinked memory file rejected

- **WHEN** `MEMORY.md` is replaced with a symlink pointing outside `<profileDir>/memories/`
- **THEN** `memory_write` refuses the write with a `path_unsafe` error and the symlink target is NOT modified

#### Scenario: Memory directory is created on first write

- **WHEN** `memory_write` is called and `<profileDir>/memories/` does not yet exist
- **THEN** the directory is created with mode `0700` before the write proceeds

