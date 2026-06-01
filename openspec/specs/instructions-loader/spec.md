# instructions-loader Specification

## Purpose

Discover, load, and render project-level and global instruction files (AGENTS.md / CLAUDE.md)
into an immutable session snapshot. Provide proximity injection of instruction files discovered
near files read by the agent. All logic resides in `internal/instructions/`.

## Requirements

### Requirement: File name search priority

The system SHALL search for instruction files using the following priority-ordered filename list: `["AGENTS.md", "CLAUDE.md"]`. For project-level discovery, the first filename in this list that has any match along the directory walk wins; once a match is found for a filename, subsequent filenames SHALL NOT be searched. Global-level discovery follows the same list with first-existing-file-wins semantics.

#### Scenario: AGENTS.md takes priority over CLAUDE.md

- **WHEN** both `AGENTS.md` and `CLAUDE.md` exist in the project root directory
- **THEN** only `AGENTS.md` is loaded; `CLAUDE.md` is ignored

#### Scenario: CLAUDE.md used as fallback

- **WHEN** `AGENTS.md` does not exist anywhere in the project tree but `CLAUDE.md` exists at the project root
- **THEN** `CLAUDE.md` is loaded

#### Scenario: No instruction files found

- **WHEN** neither `AGENTS.md` nor `CLAUDE.md` exists in the project tree or global config directory
- **THEN** the instruction snapshot is empty and no `<instructions>` block appears in the system prompt

### Requirement: Project-level file discovery

The system SHALL discover project-level instruction files by walking from the session workdir upward toward the nearest ancestor directory containing a `.git` subdirectory (the git root). All instances of the winning filename found along this path (from workdir to git root inclusive) SHALL be collected in bottom-up order (deepest first, closest to workdir first).

If no `.git` directory is found (non-git project), only the workdir itself SHALL be checked.

#### Scenario: Monorepo with nested AGENTS.md files

- **WHEN** the session workdir is `/repo/packages/sdk/` and `AGENTS.md` exists at both `/repo/packages/sdk/AGENTS.md` and `/repo/AGENTS.md`
- **THEN** both files are loaded, with the workdir-level file listed first and the repo-root file listed second

#### Scenario: Workdir is the git root

- **WHEN** the session workdir is the git root and `AGENTS.md` exists at the root
- **THEN** only the root-level `AGENTS.md` is loaded

#### Scenario: Non-git project

- **WHEN** the session workdir is `/tmp/my-project/` which has no `.git` directory
- **THEN** only `/tmp/my-project/AGENTS.md` is checked (if it exists)

### Requirement: Global-level file discovery

The system SHALL check for a global instruction file at `<profileDir>/AGENTS.md`, where `profileDir` defaults to `~/.workhorse-agent/`. If the file exists, its content SHALL be loaded alongside project-level files. Only one global file is supported (no fallback filenames at the global level).

#### Scenario: Global AGENTS.md loaded alongside project files

- **WHEN** both `~/.workhorse-agent/AGENTS.md` and `/project/AGENTS.md` exist
- **THEN** both files are loaded into the instruction snapshot, with project files listed before the global file

#### Scenario: Global AGENTS.md missing

- **WHEN** `~/.workhorse-agent/AGENTS.md` does not exist
- **THEN** no global instruction content is loaded; only project-level files are used

### Requirement: Instruction snapshot loading at session creation

The system SHALL load all discovered instruction files exactly once when a session is created. The resulting snapshot (a list of file paths and their contents) SHALL be immutable for the session lifetime. The snapshot MUST be loaded before the first system prompt is constructed.

#### Scenario: Session creation loads instruction snapshot

- **WHEN** a new session is created with workdir `/project/` and `AGENTS.md` exists at `/project/AGENTS.md`
- **THEN** the session's instruction snapshot contains the file path and content, and the snapshot does not change for the rest of the session

#### Scenario: File modified between sessions

- **WHEN** `AGENTS.md` is modified after session A starts but before session B starts
- **THEN** session A retains the pre-modification content and session B loads the post-modification content

### Requirement: No file size limit

The system SHALL NOT impose a character or byte limit on instruction files. The full content of each discovered file SHALL be loaded without truncation.

#### Scenario: Large AGENTS.md loaded in full

- **WHEN** an `AGENTS.md` file contains 50,000 characters of content
- **THEN** the entire content is loaded into the instruction snapshot without truncation

### Requirement: System prompt block rendering

The system SHALL render the instruction snapshot as an `<instructions>` XML block injected into the system prompt. Each file's content SHALL be prefixed with a header line `Instructions from: <filepath>`. Multiple files SHALL be separated by a `---` delimiter. When the snapshot is empty (no files loaded), the block SHALL be omitted entirely (no empty framing).

The rendered block SHALL use byte-stable delimiters so that prompt cache prefixes remain identical across turns of the same session.

#### Scenario: Multiple instruction files rendered

- **WHEN** the snapshot contains two files: `/project/AGENTS.md` and `~/.workhorse-agent/AGENTS.md`
- **THEN** the rendered block is:
  ```
  <instructions>
  Instructions from: /project/AGENTS.md
  {content of project file}
  ---
  Instructions from: /home/user/.workhorse-agent/AGENTS.md
  {content of global file}
  </instructions>
  ```

#### Scenario: Empty snapshot produces no block

- **WHEN** no instruction files were found
- **THEN** the `Block` function returns an empty string and no `<instructions>` element appears in the system prompt

### Requirement: Read tool proximity injection

When the Read tool reads a file, the system SHALL walk upward from the file's parent directory to the session workdir root, looking for instruction files (using the same filename priority: `AGENTS.md` → `CLAUDE.md`). Any found file that is NOT already in the system-level instruction snapshot and has NOT been previously proximity-injected in this session SHALL be appended to the Read tool's output as a `<system-reminder>` block.

The walk SHALL stop at the session workdir root and SHALL NOT ascend above it.

#### Scenario: Subdirectory AGENTS.md injected on first Read

- **WHEN** the agent reads `/project/src/foo/bar.go` and `/project/src/AGENTS.md` exists but is not in the system-level snapshot
- **THEN** the Read output is appended with:
  ```
  <system-reminder>
  Instructions from: /project/src/AGENTS.md
  {content}
  </system-reminder>
  ```

#### Scenario: Already-injected file not repeated

- **WHEN** the agent reads `/project/src/foo/bar.go` (triggers injection of `/project/src/AGENTS.md`) and then reads `/project/src/baz/qux.go`
- **THEN** `/project/src/AGENTS.md` is NOT injected again for the second Read

#### Scenario: System-level instruction file not proximity-injected

- **WHEN** `/project/AGENTS.md` is already loaded in the system-level snapshot and the agent reads `/project/src/foo.go`
- **THEN** the walk reaches `/project/` but skips it because it is already in the snapshot

#### Scenario: No proximity injection for files in workdir root

- **WHEN** the agent reads `/project/README.md` and the workdir is `/project/`
- **THEN** no proximity injection occurs (there are no ancestor directories between the file and the workdir root)

### Requirement: Session-level deduplication

The system SHALL maintain a set of file paths that have been proximity-injected during the session. Once a path is added to this set, it SHALL NOT be injected again for the remainder of the session. The set SHALL be stored on the session object and be thread-safe for concurrent Read tool invocations.

#### Scenario: Concurrent Reads of files in the same directory

- **WHEN** two concurrent Read calls both discover the same subdirectory AGENTS.md
- **THEN** the file is injected exactly once (the first call to record the path wins; the second call skips it)

### Requirement: New module location

All instruction loading, snapshot, block rendering, and proximity injection logic SHALL reside in a new `internal/instructions/` package. The package SHALL NOT import `internal/agent`, `internal/tools`, `internal/session`, or `internal/config` (same dependency constraints as `internal/prompt` and `internal/memory`).

#### Scenario: Boundary test passes

- **WHEN** `internal/instructions/boundary_test.go` scans package imports
- **THEN** no import of `internal/agent`, `internal/tools`, `internal/session`, `internal/config`, `internal/coord`, `internal/provider`, `internal/api`, or `internal/store` appears
