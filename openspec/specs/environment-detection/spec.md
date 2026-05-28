# environment-detection Specification

## Purpose
TBD - created by archiving change add-external-agent-tool. Update Purpose after archive.

## Requirements

### Requirement: Curated PATH allowlist for CLI tool discovery

The system SHALL scan the user's `PATH` for a curated allowlist of binaries known to be useful for task work (file format conversion, browser automation, source control, language runtimes, container tooling, etc.). Wholesale enumeration of `/usr/bin` is explicitly forbidden. The allowlist MUST be embedded in the binary and MUST include at minimum: `git`, `gh`, `jq`, `yq`, `curl`, `wget`, `rg`, `fd`, `pandoc`, `libreoffice`, `soffice`, `ffmpeg`, `convert`, `magick`, `identify`, `yt-dlp`, `playwright`, `chromium`, `chrome`, `firefox`, `python3`, `node`, `npm`, `pnpm`, `yarn`, `deno`, `bun`, `go`, `cargo`, `rustc`, `docker`, `podman`, `kubectl`, `terraform`, `ansible`, `asciidoctor`, `marp`. (The allowlist MUST NOT contain `imagemagick` — that is a package name, not a binary; ImageMagick ships `convert`, `magick`, `identify`.)

#### Scenario: Detected allowlist tool listed

- **WHEN** the operator's machine has `pandoc` installed and resolvable via `exec.LookPath`
- **THEN** the next session's `<environment>` block lists `pandoc @ <resolved path> (<version if detectable>)` under `cli_tools`

#### Scenario: Allowlist entry not present on PATH

- **WHEN** the operator's machine has no `ffmpeg` binary
- **THEN** the `<environment>` block does NOT mention `ffmpeg`

#### Scenario: Non-allowlist binary not surfaced

- **WHEN** the operator has `dd` and `mkfs` on `PATH`
- **THEN** neither is mentioned in the `<environment>` block (not on allowlist)

### Requirement: User extensions and exclusions

The system SHALL honor `external_agents.pathscan.extra: [name, ...]` (additional binary names to scan beyond the builtin allowlist) and `external_agents.pathscan.disabled: [name, ...]` (names to suppress even if on the builtin allowlist). Both lists default to empty.

#### Scenario: Extra binary scanned

- **WHEN** `external_agents.pathscan.extra: [poetry]` is configured
- **AND** `poetry` is installed on `PATH`
- **THEN** the `<environment>` block lists `poetry @ ...` under `cli_tools`

#### Scenario: Disabled binary suppressed

- **WHEN** `external_agents.pathscan.disabled: [docker]` is configured
- **AND** `docker` is installed on `PATH`
- **THEN** the `<environment>` block does NOT mention `docker` even though it is on the builtin allowlist

#### Scenario: Disabled binary cannot be re-added by extra

- **WHEN** both `pathscan.disabled: [docker]` and `pathscan.extra: [docker]` are configured
- **THEN** `docker` is suppressed (disabled wins)

### Requirement: Version probe per detected binary

The system SHALL attempt to capture a version string for each detected binary by invoking `<binary> --version` with a 2 second wall-clock timeout. Failures (non-zero exit, timeout, no output) are tolerated — the binary appears in the block without a version suffix. Probes MUST be executed with concurrency capped at 8 to avoid pathological startup delay.

#### Scenario: Version captured

- **WHEN** `git --version` returns `git version 2.43.0` within 2 seconds
- **THEN** the `<environment>` block entry reads `git @ /usr/bin/git (2.43.0)` (the leading `git version ` prefix MAY be stripped for readability)

#### Scenario: Version probe times out

- **WHEN** a detected binary's `--version` invocation takes longer than 2 seconds
- **THEN** the probe is killed, the binary is still listed in the block without a version, and a structured log line records the timeout

#### Scenario: Probe failure tolerated

- **WHEN** `<binary> --version` exits non-zero or writes nothing
- **THEN** the binary is listed without a version and the failure is logged at debug level only (not error)

### Requirement: PATH scan cache

The system SHALL cache the PATH scan result on disk at `<profileDir>/cache/pathscan.json` with a default TTL of 24 hours (overridable via `external_agents.pathscan.cache_ttl`). The cache MUST be invalidated and re-scanned when (a) the cache file is missing, (b) the cache `scanned_at` is older than TTL, or (c) the configured `pathscan.extra` or `pathscan.disabled` lists differ from those recorded in the cache.

#### Scenario: Fresh cache reused

- **WHEN** a session starts and `<profileDir>/cache/pathscan.json` exists with `scanned_at` within TTL and matching extra/disabled fingerprints
- **THEN** the PATH scan is NOT re-executed and the cached result is used to populate the `<environment>` block

#### Scenario: TTL expiry triggers re-scan

- **WHEN** the cache is older than `cache_ttl`
- **THEN** the next session start re-runs the scan and overwrites the cache file

#### Scenario: Config change triggers re-scan

- **WHEN** the user adds an entry to `pathscan.extra` between sessions
- **THEN** the next session start re-scans (the fingerprint no longer matches) regardless of cache age

### Requirement: Environment block injection into system prompt

The system SHALL render an `<environment>` block via a new `EnvironmentBlock(input EnvironmentInput) string` helper in `internal/prompt` and inject it by **Go-level prepending** at the agent-loop call site (`internal/agent/loop.go`), parallel to how `add-memory-l1-l2` prepends the memory block. The `BuildSystemPrompt` signature MUST NOT change (it continues to accept a single `base string` argument). When the helper produces an empty string (no detected tools and no sub_agents loaded), the agent loop MUST NOT prepend any framing — the system prompt is unchanged from its baseline form.

Composition order at the call site: `environment block → memory block → base prompt`. Rationale: stability gradient — environment is most stable across sessions (changes on tool install only), memory next, base prompt most variable.

The rendered block MUST include the following sections in this fixed order: `os`, `shell`, `cwd`, `cli_tools` (only when ≥1 detected), `sub_agents` (only when ≥1 healthy `sub_agent` adapter). Section ordering, alphabetization within each section, and delimiter format MUST be byte-stable across calls with identical inputs (load-bearing for prompt-cache prefix stability).

#### Scenario: Full block rendered

- **WHEN** a session starts on Linux with `pandoc` and `git` detected and the `claude-code` adapter healthy
- **THEN** the rendered `<environment>` block contains in order: `os`, `shell`, `cwd`, then `cli_tools:` listing `git` and `pandoc` alphabetically, then `sub_agents:` listing `claude-code`

#### Scenario: Empty block produces empty slot

- **WHEN** no allowlist tools are detected, no sub_agent adapters are healthy
- **THEN** the `{{.Environment}}` template variable expands to an empty string and the system prompt contains no `<environment>` framing

#### Scenario: Byte-stable across calls

- **WHEN** the helper is invoked twice with identical detection inputs
- **THEN** the two output strings are byte-for-byte identical (no map iteration order leaks; entries are stably sorted)

### Requirement: Snapshot per session, no mid-session re-detection

The system SHALL detect the environment exactly once per session-creation event and inject the resulting block as an immutable snapshot for that session's lifetime. Tool installations or removals during a live session MUST NOT mutate that session's `<environment>` block; only the next session reflects changes.

#### Scenario: Mid-session install not reflected

- **WHEN** a session is in progress without `ffmpeg` in its `<environment>` block
- **AND** the user installs `ffmpeg` mid-session
- **THEN** the current session's system prompt is unchanged
- **AND** the next session created reflects `ffmpeg` in its `<environment>` block (subject to cache TTL or fingerprint change)

#### Scenario: Snapshot immutable

- **WHEN** any code path attempts to mutate the in-memory environment snapshot of a live session
- **THEN** the mutation is rejected and the snapshot remains byte-identical to what was rendered at session start

### Requirement: Compose stably with memory snapshot

The environment block produced by this capability and the memory block produced by `add-memory-l1-l2` MUST compose at the agent-loop call site in the fixed order `environment → memory → base prompt`, joined by `"\n\n"` separators (only between non-empty pieces). When a block is empty, the prepend for that block MUST be entirely skipped — no separator, no trailing/leading whitespace. The resulting system prompt's prefix MUST be byte-stable across sessions with identical environment + memory snapshots.

#### Scenario: Both memory and environment present

- **WHEN** a session has non-empty memory and non-empty environment
- **THEN** the system prompt begins with `<environment>...\n\n<memory>...\n\n<base>` with exactly the two `"\n\n"` joiners and no extra whitespace

#### Scenario: Only one block non-empty produces no spurious whitespace

- **WHEN** a session has non-empty environment but empty memory
- **THEN** the system prompt begins with `<environment>...\n\n<base>` (one joiner, no blank line where memory would have been)

#### Scenario: Both blocks empty leaves base prompt unchanged

- **WHEN** a session has no detected environment and no memory
- **THEN** the system prompt equals exactly what `BuildSystemPrompt(base)` would return on its own — no leading whitespace, no leading newlines

### Requirement: PATH-scan large-allowlist warning

When the PATH scan detects more than 80 binaries (the sum of builtin allowlist and `pathscan.extra`), the system SHALL emit a single structured log line `pathscan.large name_count=N profile_dir=...` at warn level. No truncation MUST occur — every detected binary still appears in the `<environment>` block. The warning exists so operators notice unusual growth.

#### Scenario: Warn fires above threshold

- **WHEN** the scan detects 95 binaries
- **THEN** a single `pathscan.large` warn log line is emitted citing `name_count=95`
- **AND** all 95 binaries appear in the rendered block (no truncation)

#### Scenario: No warn below threshold

- **WHEN** the scan detects 40 binaries
- **THEN** no `pathscan.large` log line is emitted
