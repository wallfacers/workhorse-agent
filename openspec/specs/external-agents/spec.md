# external-agents Specification

## Purpose
TBD - created by archiving change add-external-agent-tool. Update Purpose after archive.
## Requirements
### Requirement: Adapter file location and naming

The system SHALL load `sub_agent` and `cli_tool` adapter manifests from a single directory: `<profileDir>/external-agents/`, where `profileDir` defaults to `~/.workhorse-agent/` and is controlled by the existing profile-directory resolution. Each adapter is one YAML file. The filename stem (e.g. `claude-code.yaml` → `claude-code`) MUST equal the adapter's `name:` field; mismatch is a hard load error. The filename stem MUST also match the lowercase-kebab-case regex `^[a-z0-9][a-z0-9_-]{0,63}$`; files whose stems contain uppercase letters or unsafe characters are rejected at load time with an error log. No nested subdirectories are scanned.

#### Scenario: Default directory resolution

- **WHEN** the server starts with no `external_agents.dir` override
- **THEN** adapters are resolved from `~/.workhorse-agent/external-agents/*.yaml`

#### Scenario: Filename stem matches name field

- **WHEN** the loader reads `~/.workhorse-agent/external-agents/claude-code.yaml` and the file's `name:` field is `claude-code`
- **THEN** the adapter is loaded and registered under the name `claude-code`

#### Scenario: Filename stem differs from name field

- **WHEN** the loader reads `~/.workhorse-agent/external-agents/claudecode.yaml` whose `name:` field is `claude-code`
- **THEN** the loader rejects the file with a structured log line explaining the mismatch, the adapter is NOT registered, and other adapters in the directory continue to load

#### Scenario: Uppercase filename rejected

- **WHEN** the loader reads `~/.workhorse-agent/external-agents/Claude-Code.yaml`
- **THEN** the loader rejects the file because the stem does not match the lowercase-kebab-case regex; the adapter is NOT registered; a structured log line names the offending filename and the expected regex

#### Scenario: Unexpected files ignored

- **WHEN** the directory contains files that are not `.yaml` (backups, swap files, `.smoke` sidecar files)
- **THEN** the loader ignores them silently and processes only `*.yaml` files

#### Scenario: Directory does not exist on first load

- **WHEN** the server starts and `<profileDir>/external-agents/` does not exist
- **THEN** the directory is created with mode `0700` and the registry is seeded with builtin adapters only

### Requirement: Builtin adapters embedded and overridable

The system SHALL ship at least three builtin adapters embedded in the binary via `//go:embed`: `claude-code`, `codex`, `aider`. Builtin adapters have `provenance.source: builtin`. On registry construction, builtins seed the registry first, then on-disk adapters under `<profileDir>/external-agents/` are loaded and override any builtin with the same `name`.

#### Scenario: Builtin adapter available without disk file

- **WHEN** no file `<profileDir>/external-agents/claude-code.yaml` exists
- **AND** the `claude` binary is installed on `PATH`
- **THEN** the `claude-code` adapter is loaded from the embedded builtin and exposed via the `ExternalAgent` tool

#### Scenario: On-disk override replaces builtin

- **WHEN** `<profileDir>/external-agents/claude-code.yaml` exists on disk
- **THEN** the on-disk version REPLACES the builtin entirely, including its `provenance` (which becomes `user_yaml`)

#### Scenario: Builtin adapter retains builtin provenance

- **WHEN** a builtin adapter loads without an on-disk override
- **THEN** its `provenance.source` field is `builtin` and it is trusted by default (no first-invocation approval prompt)

### Requirement: Adapter schema validation

The system SHALL validate every loaded adapter against an embedded JSON Schema covering: identity (`name`, `binary`, `class`), invocation (`prompt_via`, `prompt_arg`, `extra_args`, `cwd`, `env_passthrough`, `env_override`), session (optional `supports_resume`, `resume_flag`, `session_id_arg`), output (`format`, `stderr`, optional `parser` containing `assistant_text?` and `session_id_path?`), control (`cancel_signal`, `cancel_grace_sec`, `default_timeout_sec`, `max_timeout_sec`), `security` (`network`, `filesystem`, `trusted`), `smoke_test` (`prompt`, `expected_substring`, `timeout_sec`), `description`, optional `usage_hints`, and `provenance` (`source` and provenance metadata). For `class: sub_agent`, the `invocation`, `output`, `control`, and `smoke_test` blocks MUST be present. For `class: cli_tool`, only identity, `description`, and `security` are required. The schema MUST also enforce that any `output.parser.*` JSONPath value (both `assistant_text` and `session_id_path`) matches the restricted grammar `^\$(\.[A-Za-z_][A-Za-z0-9_]*|\[(-?[0-9]+|\*)\])*$` (no filters, no recursive descent, no string-key brackets). Schema-invalid adapters MUST be rejected without registration; the server MUST continue to start.

#### Scenario: Valid sub_agent adapter loads

- **WHEN** an adapter file declares `class: sub_agent` with all required blocks present and field values matching the schema
- **THEN** the adapter is added to the registry and counted as healthy

#### Scenario: sub_agent adapter missing required block rejected

- **WHEN** an adapter file declares `class: sub_agent` but omits the `output` block
- **THEN** the adapter is rejected with a schema-validation error log, is NOT registered, and the server continues startup

#### Scenario: Invalid enum value rejected

- **WHEN** an adapter declares `output.format: xml` (not in the allowed enum)
- **THEN** the adapter is rejected and a log line names the offending field and the allowed values

#### Scenario: cli_tool adapter with minimum fields loads

- **WHEN** a `class: cli_tool` adapter declares only `name`, `binary`, `class`, `description`, `security`, and `provenance`
- **THEN** the adapter loads successfully

#### Scenario: Out-of-grammar JSONPath rejected

- **WHEN** an adapter declares `output.parser.assistant_text: $..text` (recursive descent, outside the restricted grammar)
- **THEN** schema validation fails and the adapter is rejected

### Requirement: Adapter binary resolution

The system SHALL resolve each adapter's `binary` field to an absolute filesystem path before the adapter is considered healthy. Resolution uses `exec.LookPath` for non-absolute values and `os.Stat` for absolute ones. Adapters whose binary cannot be resolved MUST be loaded into a "binary-missing" state where they appear in registry diagnostics but are absent from the `ExternalAgent` tool's enum.

#### Scenario: Binary on PATH resolved

- **WHEN** adapter declares `binary: claude` and `claude` resolves via `exec.LookPath` to `/usr/local/bin/claude`
- **THEN** the adapter is healthy and exposed via `ExternalAgent`

#### Scenario: Absolute binary path that exists

- **WHEN** adapter declares `binary: /opt/codex/bin/codex` and that path exists and is executable
- **THEN** the adapter is healthy

#### Scenario: Binary missing from PATH

- **WHEN** adapter declares `binary: gemini` but `gemini` is not on `PATH`
- **THEN** the adapter loads in binary-missing state, does NOT appear in the `ExternalAgent` enum, and a structured log line records the missing binary

#### Scenario: Binary path exists but not executable

- **WHEN** adapter's resolved binary path exists but has no execute bit
- **THEN** the adapter is treated identically to binary-missing

### Requirement: Hot reload at session start

The system SHALL rescan `<profileDir>/external-agents/` exactly once per session-creation event (e.g. each `POST /sessions`). The rescan produces a fresh registry snapshot for the new session. Changes to adapter files on disk MUST NOT propagate to sessions already in progress, EXCEPT for adapters published through the `adapter-generation` capability's approval flow during the live session — those adapters MUST be injected into the originating session's in-memory registry snapshot via a single chokepoint, `RegistryInjector.Inject(sessionID, adapter)`, so the model's next retry succeeds without waiting for a new session. The injection is the ONLY sanctioned mutation path on a live snapshot; all other code paths MUST treat the snapshot as immutable. Other live sessions' snapshots are NOT modified by the injection (each session's snapshot remains independent); they pick up the new adapter on their own next session-creation event via the standard rescan.

The rescan MUST skip any subdirectory of `<profileDir>/external-agents/` whose name starts with `.` (notably `.drafts/`, owned by the `adapter-generation` capability). The loader processes only `*.yaml` files at the top level of the scanned directory.

#### Scenario: New adapter file picked up by next session

- **WHEN** a session is in progress
- **AND** the user drops `<profileDir>/external-agents/gemini.yaml` on disk directly (not through agent_setup)
- **THEN** the current session's registry is unchanged and `ExternalAgent` cannot invoke `gemini`
- **AND** the next session created sees `gemini` in the `ExternalAgent` enum (subject to smoke-test outcome)

#### Scenario: Adapter file deletion takes effect next session

- **WHEN** an adapter file is deleted while a session is using it
- **THEN** the current session retains the in-memory adapter and can complete in-flight invocations
- **AND** the next session does not load that adapter

#### Scenario: Concurrent session creations see consistent snapshots

- **WHEN** two `POST /sessions` requests arrive concurrently
- **THEN** each session is given its own snapshot of the registry as scanned at its own creation time; an adapter added between the two scans appears in the second but not the first

#### Scenario: Approval-driven injection visible in originating session only

- **WHEN** session A triggers `agent_setup` for `gemini`, the user approves, and the adapter is published
- **THEN** session A's in-memory registry snapshot gains `gemini` immediately via `RegistryInjector.Inject(sessionA, gemini)` and the model's subsequent `ExternalAgent` call against it dispatches normally
- **AND** any other live session B does NOT see `gemini` in its snapshot until B is replaced by a new session

#### Scenario: Drafts directory ignored by rescan

- **WHEN** the rescan walks `<profileDir>/external-agents/`
- **THEN** the subdirectory `.drafts/` (and any other dot-prefixed subdirectory) is skipped without entering
- **AND** partial draft files under `.drafts/` are never loaded into any session's registry under any circumstance

#### Scenario: Snapshot immutable outside RegistryInjector

- **WHEN** any code path other than `RegistryInjector.Inject` attempts to add, remove, or modify entries in a live session's registry snapshot
- **THEN** the attempt MUST fail at compile time (the snapshot type's mutation API is package-private and reachable only via the injector) OR at runtime (a guarded mutex enforces single-writer access only via the injector)

### Requirement: Smoke test on first load and after mtime change

The system SHALL execute the adapter's `smoke_test` before exposing a `sub_agent`-class adapter to the `ExternalAgent` tool. Smoke results are cached in a sibling file `<name>.smoke` (JSON, same directory). The smoke test MUST re-run when (a) no `.smoke` cache exists, (b) the adapter file's mtime is newer than the cache's mtime, or (c) the cache's `ran_at` is older than `external_agents.smoke_test.cache_ttl` (default 168 hours).

#### Scenario: Successful smoke caches result

- **WHEN** an adapter's smoke test runs and the captured output contains `expected_substring`
- **THEN** a sibling `<name>.smoke` file is written with `{passed: true, ran_at, output_hash}` and the adapter is healthy

#### Scenario: Failed smoke records error and disables invocation

- **WHEN** an adapter's smoke test exits non-zero, times out, or produces output not containing `expected_substring`
- **THEN** the sibling `.smoke` file is written with `{passed: false, ran_at, error}`
- **AND** the adapter remains in the registry (visible in diagnostics) but `ExternalAgent` REJECTS invocations against it with a clear error citing the smoke failure

#### Scenario: Cached pass skips re-execution within TTL

- **WHEN** a session starts and an adapter's `.smoke` file shows `passed: true` with `ran_at` within `cache_ttl` and a mtime newer than the adapter file's mtime
- **THEN** the smoke test is NOT re-run and the adapter is healthy without latency cost

#### Scenario: Adapter mtime change forces re-run

- **WHEN** the adapter YAML is edited (mtime advances past the `.smoke` cache mtime)
- **THEN** the next session start re-runs the smoke test before exposing the adapter

#### Scenario: TTL expiry forces re-run

- **WHEN** an adapter's `.smoke` file is older than `cache_ttl`
- **THEN** the next session start re-runs the smoke test even without an mtime change to the adapter file

### Requirement: Smoke test sandbox

The system SHALL execute the smoke test in a sandbox isolating it from the operator's filesystem and environment beyond the minimum needed to invoke the binary. The sandbox MUST:

1. Create a fresh temporary directory under `os.TempDir()` with mode `0700` and use it as the child's `cwd`; the directory MUST be removed (with `os.RemoveAll`) via a deferred call so cleanup runs regardless of panic, timeout, or normal exit.
2. Compute the child env by calling `bash.Filter(os.Environ())`, restricting the kept set to `invocation.env_passthrough`, layering `invocation.env_override`, and FURTHER stripping to a minimum allowlist of `PATH`, `HOME`, `USER`, `LANG` for the smoke run only.
3. Enforce `smoke_test.timeout_sec` as a hard wall-clock cap.
4. Capture stdout, stderr, and exit code without writing to any operator-visible location.

#### Scenario: Smoke runs in temp cwd

- **WHEN** smoke test runs for an adapter
- **THEN** the child's working directory is a unique temporary directory under `os.TempDir()` and the directory is removed after the test exits

#### Scenario: Smoke env minimised

- **WHEN** the smoke test runs
- **THEN** the child env contains only `PATH`, `HOME`, `USER`, `LANG` plus any values from `invocation.env_override`; variables like `PWD`, `OLDPWD`, `SHLVL` from the parent are absent

#### Scenario: Smoke timeout enforced

- **WHEN** the smoke binary runs beyond `smoke_test.timeout_sec`
- **THEN** the child is killed via process-group SIGKILL, the smoke is recorded as failed with reason `timeout`, and the temporary cwd is removed

### Requirement: Smoke result file written atomically

The sibling `<name>.smoke` file SHALL be written using the temp-file + rename pattern: write JSON to `<name>.smoke.tmp` then `os.Rename(<name>.smoke.tmp, <name>.smoke)`. Readers MUST tolerate a missing or malformed file by treating the smoke as "not cached" and triggering a re-run. This guarantees that a server interrupted mid-write never leaves a corrupted `.smoke` file the next loader would misinterpret.

#### Scenario: Atomic write under interrupt

- **WHEN** the smoke runner is interrupted (process killed) between starting the write and completing it
- **THEN** the `<name>.smoke` path either contains the previous valid content or does not exist
- **AND** the next loader run treats the absence as "no cache" and re-runs the smoke

#### Scenario: Corrupted cache treated as missing

- **WHEN** the `<name>.smoke` file contains malformed JSON (e.g. from manual editing)
- **THEN** the loader logs a debug line, treats the cache as absent, and re-runs the smoke test

### Requirement: ExternalAgent tool registration

The system SHALL expose a tool named `ExternalAgent` to the agent loop if and only if at least one `sub_agent`-class adapter is healthy at session start. The tool's input schema MUST include:

- `agent_name` (string, required): an enum populated from the names of all healthy `sub_agent`-class adapters, stably sorted (deterministic order for prompt-cache stability).
- `prompt` (string, required): the user-facing instruction to hand to the sub-agent.
- `inputs` (object, optional): free-form key-value pairs the adapter MAY consume.
- `timeout_sec` (integer, optional): per-call timeout override; clamped to `[1, control.max_timeout_sec]` of the target adapter; defaults to `control.default_timeout_sec`.
- `resume_session_id` (string, optional): a prior session id; honored only when the target adapter declares `session.supports_resume: true`.

The tool's description MUST list each adapter and its `usage_hints` (if any), so the model can choose `agent_name` informedly.

#### Scenario: Tool exposed when sub_agent adapters exist

- **WHEN** at least one healthy `sub_agent` adapter is loaded at session start
- **THEN** the `ExternalAgent` tool is registered in the session's tool surface and visible in `GET /sessions/{id}/tools`

#### Scenario: Tool absent when no sub_agent adapters

- **WHEN** zero `sub_agent` adapters are healthy at session start (e.g. all in binary-missing state, all smoke-failed, or none configured)
- **THEN** the `ExternalAgent` tool is NOT registered and the model does not see it

#### Scenario: Enum reflects healthy adapters only

- **WHEN** three adapters are loaded but one is binary-missing and one is smoke-failed
- **THEN** the `ExternalAgent` `agent_name` enum lists only the one healthy adapter

#### Scenario: Enum order is stable

- **WHEN** the same set of healthy adapters loads across two sessions
- **THEN** the `agent_name` enum values appear in identical order across both sessions

### Requirement: ExternalAgent invocation against unknown adapter

The system SHALL reject `ExternalAgent` calls whose `agent_name` does not appear in the current session's enum, EXCEPT when the LLM-driven adapter-generation flow from the `adapter-generation` capability intercepts the call. The standard rejection path applies when any of the following holds: (a) `external_agents.generation.implicit_trigger_enabled` is `false`, OR (b) no binary matching `agent_name` resolves on `PATH` via `exec.LookPath`, OR (c) the per-session adapter-generation dedup map records `agent_name` in state `unavailable` (i.e. a prior generation in this session was rejected or expired). When the intercept fires (binary resolves, intercept is enabled, no `unavailable` dedup entry), the tool MUST NOT emit the standard "unknown agent" error; instead the intercept produces a tool_result naming the new approval_id and repeating the model's call parameters verbatim (per the `adapter-generation` Plan A requirement). In every case the rejection or intercept MUST occur before any sub-process is started.

#### Scenario: Unknown agent_name rejected (no intercept eligible)

- **WHEN** the model emits `ExternalAgent` with `agent_name: "gemini"` but `gemini` is not in the session's enum
- **AND** either `implicit_trigger_enabled` is false, OR no `gemini` binary resolves on PATH, OR the dedup map shows `gemini → unavailable`
- **THEN** no sub-process is started and the tool returns the standard error tool_result naming `gemini` and listing the available agents

#### Scenario: Unknown agent_name intercepted by Plan A

- **WHEN** the model emits `ExternalAgent` with `agent_name: "gemini"`, `gemini` resolves via `exec.LookPath`, `implicit_trigger_enabled` is true, and the dedup map has no entry for `gemini`
- **THEN** no sub-process is started, the adapter-generation flow runs synchronously, and the tool returns an intercept tool_result (NOT the standard error) naming the new approval_id; the dedup map records `gemini → pending`

### Requirement: ExternalAgent invocation against unhealthy adapter

The system SHALL reject `ExternalAgent` calls whose target adapter is in binary-missing or smoke-failed state. The rejection MUST cite the specific health reason.

#### Scenario: Smoke-failed adapter cannot be invoked

- **WHEN** the model emits `ExternalAgent` with `agent_name: "claude-code"` but the adapter's `.smoke` cache shows `passed: false`
- **THEN** the tool returns an error tool_result quoting the recorded smoke failure reason and does not start a sub-process

### Requirement: Sub-process invocation, env, and cwd

The system SHALL invoke the target adapter's binary as a child process with arguments and environment composed as follows:

1. **Args**: per `invocation.prompt_via`:
   - `arg`: `[binary, invocation.prompt_arg, prompt, ...invocation.extra_args]`
   - `stdin`: `[binary, ...invocation.extra_args]` with `prompt` written to stdin (then stdin closed)
   - `file`: create a temp file under `os.TempDir()` with mode `0600`, write `prompt` to it, then invoke `[binary, invocation.prompt_arg, <tempfile>, ...invocation.extra_args]`. The temp file MUST be removed via a deferred call so cleanup runs regardless of the child's exit, timeout, or cancel outcome. The temp file MUST NOT be created inside the session's workdir.
2. **Env** (in this exact order — `bash.Filter` MUST be the LAST step so no source can re-introduce a denied variable): parent env (`os.Environ()`) projected to map → restrict to `invocation.env_passthrough` allowlist → layer `invocation.env_override` (verbatim from YAML) → inject `PATH` and `HOME` if absent → re-serialize to `[]string` → call `bash.Filter(envSlice)` once on the merged result → call `bash.LogDropped(logger, dropped)` → assign `kept` to `exec.Cmd.Env`. This matches the Bash tool's ordering (`internal/tools/bash/bash.go:91-100`): merge first, filter once, no bypass.
3. **Cwd**: parent session's workdir (no override per-invocation in MVP).
4. **Process group**: child runs in its own process group (`setpgid`) so the driver can SIGKILL the whole group on cancel.

Resume invocations: if `resume_session_id` is provided AND the adapter supports resume, additionally pass `[invocation.session.resume_flag, invocation.session.session_id_arg, resume_session_id]`.

#### Scenario: arg-style invocation

- **WHEN** an adapter declares `prompt_via: arg` and `prompt_arg: --prompt` and `extra_args: ["--non-interactive"]`
- **AND** the model invokes `ExternalAgent` with `prompt: "do X"`
- **THEN** the child is executed with argv `[<binary>, "--prompt", "do X", "--non-interactive"]`

#### Scenario: stdin-style invocation

- **WHEN** an adapter declares `prompt_via: stdin`
- **AND** the model invokes `ExternalAgent` with `prompt: "do X"`
- **THEN** the child is executed with argv `[<binary>, ...extra_args]` and `"do X"` is written to the child's stdin, which is then closed

#### Scenario: env passthrough honored

- **WHEN** an adapter declares `env_passthrough: [ANTHROPIC_API_KEY]`
- **AND** the parent process env contains `ANTHROPIC_API_KEY=sk-xxx` and `LD_PRELOAD=/tmp/x.so`
- **THEN** the child env contains `ANTHROPIC_API_KEY=sk-xxx`, `PATH`, `HOME`, plus any `env_override` values
- **AND** the child env does NOT contain `LD_PRELOAD` (stripped by envfilter)

#### Scenario: env_override cannot re-inject denied variable

- **WHEN** an adapter declares `env_override: {LD_PRELOAD: "/tmp/evil.so", FOO: "bar"}`
- **THEN** the child env contains `FOO=bar` (passes envfilter) and does NOT contain `LD_PRELOAD` (stripped by post-merge `bash.Filter` regardless of source)
- **AND** a `bash.LogDropped` audit line records `LD_PRELOAD` was dropped

#### Scenario: env_override cannot re-inject dangerous NODE_OPTIONS token

- **WHEN** an adapter declares `env_override: {NODE_OPTIONS: "--require /tmp/x.js"}`
- **THEN** the child env does NOT contain `NODE_OPTIONS` (the value contains a dangerous token; envfilter drops the entire variable per the existing `nodeOptionsSafe` rule)

#### Scenario: Resume against non-resumable adapter rejected

- **WHEN** the model invokes `ExternalAgent` against an adapter with `session.supports_resume: false` and passes a `resume_session_id`
- **THEN** the tool returns an error before starting a sub-process, citing that the adapter does not support resume

#### Scenario: prompt_via file tempfile cleaned up

- **WHEN** an adapter declares `prompt_via: file` and the child exits
- **THEN** the tempfile created under `os.TempDir()` is removed regardless of whether the exit was graceful, on timeout, on cancel, or on panic

#### Scenario: env audit log emitted

- **WHEN** the sub-process driver composes the child env
- **THEN** any env vars stripped by `bash.Filter()` (e.g. `LD_PRELOAD`) are recorded via `bash.LogDropped(...)` at warn level — identical audit trail to a Bash tool invocation

### Requirement: Sub-process cancellation semantics

The system SHALL terminate the sub-process when the parent agent loop signals cancellation (user-initiated cancel, session timeout, parent panic recovery). Termination MUST:

1. Send `control.cancel_signal` (default `SIGINT`) to the entire process group.
2. Wait up to `control.cancel_grace_sec` (default 5s) for the child to exit.
3. If the child is still alive, send `SIGKILL` to the process group.
4. Return a `tool_result` whose text begins with the existing `[CANCELLED]` marker so the model knows the call was interrupted.

#### Scenario: Graceful cancel within grace period

- **WHEN** the parent loop cancels mid-invocation
- **AND** the child exits within `cancel_grace_sec` after receiving `cancel_signal`
- **THEN** the tool_result is emitted with the `[CANCELLED]` prefix and partial output collected up to cancel time

#### Scenario: Forceful kill after grace period

- **WHEN** the parent loop cancels mid-invocation
- **AND** the child does not exit within `cancel_grace_sec`
- **THEN** SIGKILL is sent to the process group, the tool_result is emitted with the `[CANCELLED]` prefix, and a structured log records the forceful kill

#### Scenario: Cancel cleans up child of child

- **WHEN** the sub-agent has spawned further child processes (e.g. `claude` spawns `git`)
- **AND** the parent loop cancels
- **THEN** all descendants are reaped by the process-group kill, leaving no orphans

### Requirement: Sub-process timeout enforcement

The system SHALL enforce a wall-clock timeout per invocation: `min(invocation.timeout_sec if provided else control.default_timeout_sec, control.max_timeout_sec)`. On timeout, the same teardown sequence as cancellation MUST be used, and the tool_result text MUST be prefixed `[TIMEOUT]`.

#### Scenario: Default timeout applied

- **WHEN** the model invokes `ExternalAgent` without `timeout_sec` against an adapter with `default_timeout_sec: 600`
- **THEN** the call has a 600 second wall-clock budget

#### Scenario: Per-call timeout clamp

- **WHEN** the model passes `timeout_sec: 999999` against an adapter with `max_timeout_sec: 3600`
- **THEN** the effective timeout is 3600 seconds

#### Scenario: Timeout triggers teardown

- **WHEN** the effective timeout elapses before the child exits
- **THEN** the same SIGINT→grace→SIGKILL sequence as cancel runs, and the tool_result is prefixed `[TIMEOUT]`

### Requirement: Output collection and parsing — single cap at `tool_result_max_bytes`

The system SHALL read stdout (and optionally stderr per `output.stderr`) from the child concurrently, counting bytes as they accumulate in a memory buffer. Combined buffer size MUST be capped at the existing global `tool_result_max_bytes` config value (default `1 << 20` = 1 MiB; the same cap the orchestrator's `tools.TruncateOutput` enforces on every tool's output). No separate ExternalAgent-only output-cap config is introduced. Operators raise the cap globally via `tool_result_max_bytes` and gain the increase across ExternalAgent and all other tools.

When the cap is reached: (a) further bytes from the child MUST be drained into `io.Discard` so the child does not block on a full pipe; (b) the driver MUST send `control.cancel_signal` to the child process group and proceed with the standard cancel → grace → SIGKILL teardown (unless `external_agents.driver.kill_on_output_cap: false` is set, in which case the child runs to natural completion while the buffer remains at the cap); (c) the result MUST be marked `Truncated: true`. On normal exit OR truncation OR non-zero exit, the buffer MUST be parsed per `output.format` and rendered as a single tool_result text. The tool_result MUST append exactly ONE `[... truncated N bytes]` marker when truncated.

Truncation accounting is done at byte-receipt time, not after collection completes. This guarantees memory bounded by the cap regardless of child output rate.

Because the driver's cap equals the orchestrator's cap, the orchestrator's `tools.TruncateOutput` (`orchestrator.go:226`) is a no-op on ExternalAgent results — operators see exactly one truncation marker, never two.

`output.format` semantics:

- `text`: return stdout verbatim, stripping ANSI escape sequences.
- `jsonl`: parse each line as JSON; if `output.parser.assistant_text` is set, apply the JSONPath expression to each line and concatenate the extracted strings; else pretty-print all parsed objects as a JSON array.
- `streaming-json`: same as `jsonl` but tolerate a partial trailing line without erroring.
- `sse`: parse `data:` event lines, apply same JSONPath logic to the JSON payload of each event.

`output.stderr` semantics:

- `separate`: append `\n<stderr>\n...\n</stderr>` to the tool_result text when stderr is non-empty.
- `merge`: interleave stderr lines into stdout in receipt order before parsing.
- `ignore`: drop stderr entirely.

#### Scenario: Plain text output

- **WHEN** an adapter declares `output.format: text` and the child writes "Hello\nWorld\n" to stdout
- **THEN** the tool_result text is `"Hello\nWorld\n"` (ANSI stripped if present)

#### Scenario: JSONL with parser extraction

- **WHEN** an adapter declares `output.format: jsonl` and `output.parser.assistant_text: $.delta.text`
- **AND** the child writes lines `{"delta":{"text":"Hi"}}` and `{"delta":{"text":" there"}}`
- **THEN** the tool_result text is `"Hi there"`

#### Scenario: Output cap triggers child cancel (default)

- **WHEN** the child produces 8 MiB of stdout with `kill_on_output_cap: true` (the default) and `tool_result_max_bytes: 1048576`
- **THEN** the captured buffer holds the first 1 MiB; the child is sent `cancel_signal` then SIGKILL on grace expiry; the tool_result text ends with `[... truncated 1048576 bytes]`

#### Scenario: Output cap with kill disabled lets child finish

- **WHEN** the child produces 8 MiB of stdout with `kill_on_output_cap: false` configured and `tool_result_max_bytes: 1048576`
- **THEN** the captured buffer holds the first 1 MiB; the child continues to natural exit while the driver discards excess bytes; the tool_result text ends with `[... truncated 1048576 bytes]`

#### Scenario: Single truncation marker (no double truncation)

- **WHEN** an ExternalAgent call produces output exceeding `tool_result_max_bytes`
- **THEN** the tool_result text contains exactly ONE `[... truncated N bytes]` marker (from the driver), NOT a second marker from the orchestrator's `tools.TruncateOutput` — because the driver's cap equals the orchestrator's cap, the orchestrator's truncation is a no-op

#### Scenario: Raising tool_result_max_bytes raises ExternalAgent budget

- **WHEN** an operator sets `tool_result_max_bytes: 4194304` (4 MiB) in config and restarts
- **THEN** ExternalAgent's effective output cap is also 4 MiB; the driver collects up to 4 MiB before truncating

#### Scenario: Raw output dump on truncate

- **WHEN** the output is truncated
- **THEN** the driver writes the captured raw stdout+stderr to `os.TempDir()/workhorse-extagent-<session_id>-<call_id>.log` with mode `0600`
- **AND** the tool_result text appends a footer line `[raw output dump: <path>]`

#### Scenario: Raw output dump on non-zero exit

- **WHEN** the child exits with a non-zero exit code
- **THEN** the driver writes the captured raw stdout+stderr to a temp file with mode `0600`
- **AND** the tool_result text appends `[raw output dump: <path>]`

#### Scenario: Happy path skips raw dump

- **WHEN** the child exits zero without truncation
- **THEN** no temp file is written

#### Scenario: stderr separate

- **WHEN** an adapter declares `output.stderr: separate` and the child writes to both streams
- **THEN** the tool_result text ends with `\n<stderr>\n...\n</stderr>` containing the captured stderr

### Requirement: First-invocation approval for untrusted adapters

The system SHALL gate the first `ExternalAgent` invocation against any adapter with `security.trusted: false` (which includes all `provenance.source != builtin` adapters by default) through a permission prompt, EXCEPT for adapters that were just published through the `adapter-generation` capability's approval flow in the same session — those adapters are recorded in the per-session approved-set at publish time, so the standard first-invocation prompt does NOT fire for them in the originating session. Adapters published in one session continue to prompt on first use in any other session. Gating is performed INSIDE the tool's `Run` method, NOT through the orchestrator's standard `checkPermissions` flow. Implementation:

- The tool's `extractResource("ExternalAgent", ...)` returns the empty string and the tool implements the marker interface `tools.InternalGated` (returns `true`) that causes `internal/agent/loop.go` to bypass `Permissions.Check` for this tool. (The alternative `permission.Manager.AllowSession` path documented in `add-external-agent-tool` design.md D21 is NOT taken — D21 is locked to the `InternalGated` path so both `ExternalAgent` and `agent_setup` share one mechanism.)
- Inside `Tool.Run`, before sub-process start: if `adapter.Security.Trusted == false` and the per-session approved-set does not contain `agent_name`, the tool calls `Host.PermissionGate.Prompt(ctx, sessionID, "ExternalAgent", agent_name, ...)` and records the result in the per-session map on approval.
- The `adapter-generation` capability's approval handler MUST write the just-published `agent_name` into the same per-session approved-set BEFORE returning success on `POST /sessions/{id}/approvals/{approval_id}`. The approved-set is held on `Host` (see `add-external-agent-tool` D21) and exposed to the approval handler via a thin `Host.MarkApproved(sessionID, agentName)` method added for this purpose.

Builtin adapters (`security.trusted: true`) bypass the gate entirely. Approval scope is the current session only; subsequent invocations in the same session do not re-prompt. A new session re-prompts on first use.

#### Scenario: Builtin adapter not gated

- **WHEN** the model invokes the `claude-code` builtin adapter for the first time in a session
- **AND** the adapter has `provenance.source: builtin` and `security.trusted: true`
- **THEN** no permission prompt is raised; the call proceeds

#### Scenario: User-yaml adapter gated on first use

- **WHEN** the model invokes an adapter with `provenance.source: user_yaml` and `security.trusted: false` for the first time in a session
- **THEN** a permission prompt is raised before the sub-process starts
- **AND** the call proceeds only after the user approves

#### Scenario: Second invocation in same session not gated

- **WHEN** the model invokes the same untrusted adapter a second time in the same session (after a prior approval)
- **THEN** no permission prompt is raised; the call proceeds

#### Scenario: Fresh session re-prompts

- **WHEN** the user creates a new session and the model invokes the same previously-approved untrusted adapter
- **THEN** the permission prompt is raised again on first use of that adapter in the new session

#### Scenario: LLM-generated adapter approved via agent_setup not double-gated in originating session

- **WHEN** an adapter is generated and approved via `agent_setup` in session A (the publish step calls `Host.MarkApproved(sessionA, agent_name)`)
- **AND** the model in session A then invokes `ExternalAgent` against the new adapter
- **THEN** no additional permission prompt is raised in session A; the call proceeds directly to sub-process start

#### Scenario: Same LLM-generated adapter still gated in other sessions

- **WHEN** session B is created after the adapter is published via approval in session A
- **AND** the model in session B invokes `ExternalAgent` against the new adapter for the first time
- **THEN** the standard first-invocation approval prompt IS raised in session B (per-session approved-set is not shared across sessions)

### Requirement: ExternalAgent supports parallel invocation

The `ExternalAgent` tool's `CanRunInParallel()` method MUST return `true`. Each invocation spawns an independent sub-process with isolated I/O, memory buffer, process group, and context. The orchestrator MAY fan out multiple concurrent `ExternalAgent` calls within a single model turn, mirroring `Dispatch` semantics.

#### Scenario: Concurrent invocations are independent

- **WHEN** the model emits two `ExternalAgent` tool_use blocks in a single turn (one against `claude-code`, one against `aider`)
- **THEN** the orchestrator dispatches both concurrently; both children run with separate process groups and separate output buffers; the two tool_results return independently

### Requirement: Session id surfaced for resumable adapters

When an adapter declares `session.supports_resume: true` AND its `output.parser` block includes the optional `session_id_path` JSONPath, the system SHALL extract the session id from the captured output and append a single `[SESSION_ID: <id>]` footer to the tool_result text. The model can read this value from a prior tool_result and supply it to a subsequent `ExternalAgent` invocation via `resume_session_id`. When `session_id_path` is unset or yields no value, no footer is appended.

#### Scenario: session_id extracted and surfaced

- **WHEN** an adapter declares `session.supports_resume: true` and `output.parser.session_id_path: $.session_id`
- **AND** the captured stdout contains a JSON line `{"session_id": "S123", "delta": {"text": "hi"}}`
- **THEN** the rendered tool_result text ends with the line `[SESSION_ID: S123]`

#### Scenario: missing session_id silently omitted

- **WHEN** `session_id_path` is set but the captured output contains no value at the path
- **THEN** the tool_result has no `[SESSION_ID: ...]` footer; a debug log line records the missing extraction

#### Scenario: non-resumable adapter does not surface session_id

- **WHEN** an adapter declares `session.supports_resume: false`
- **THEN** any value the parser extracts at `session_id_path` is discarded and the tool_result has no footer

### Requirement: Tool DefaultTimeout reflects loaded adapters

The `ExternalAgent` tool's `DefaultTimeout()` method MUST return the maximum `control.max_timeout_sec` across all currently-healthy `sub_agent` adapters in the session's registry snapshot, plus a 30 second grace period. When the registry contains no healthy `sub_agent` adapters (an edge case since the tool wouldn't be registered then), the method MUST return a 3630 second fallback. This ensures the orchestrator's `context.WithTimeout` wrap (`internal/agent/orchestrator.go`) never fires before the adapter's internal `[TIMEOUT]` teardown has a chance to run.

#### Scenario: DefaultTimeout reflects max adapter timeout

- **WHEN** the registry contains two healthy `sub_agent` adapters with `control.max_timeout_sec` of 1800 and 3600 respectively
- **THEN** `Tool.DefaultTimeout()` returns `3600s + 30s = 3630s`

#### Scenario: Internal timeout fires before orchestrator backstop

- **WHEN** an invocation's effective internal timeout is 600s and the orchestrator's backstop is 3630s
- **THEN** the internal timeout fires first; the tool_result text is prefixed `[TIMEOUT]` (not `[CANCELLED]`)

### Requirement: Per-invocation structured log line

For every `ExternalAgent` invocation that progresses past adapter-name validation, the system SHALL emit a single structured log line on completion (success, timeout, cancel, or error) at `info` level with fields: `adapter`, `session_id`, `call_id`, `duration_ms`, `exit_code`, `cancelled` (bool), `timed_out` (bool), `truncated_bytes` (0 if not truncated), `prompt_chars`. The line gives operators a single grep-target for usage and failure patterns without requiring a metrics backend.

#### Scenario: Successful invocation logged

- **WHEN** an `ExternalAgent` call against a healthy adapter completes successfully
- **THEN** exactly one log line at info level named `external_agent.invoke` is emitted with the fields above populated; `cancelled` and `timed_out` are false; `truncated_bytes` is 0; `exit_code` is the child's exit code

#### Scenario: Failed invocation logged with reason

- **WHEN** an invocation times out
- **THEN** the same log line is emitted with `timed_out: true` and `duration_ms` ≥ effective timeout

### Requirement: Failed adapter loads do not block server startup

The system SHALL load adapters defensively: parse failures, schema-validation failures, filename-stem mismatches, and unresolvable binaries MUST cause the affected adapter to be skipped with a structured log line, but MUST NOT prevent the server from starting or other adapters from loading.

#### Scenario: One bad YAML does not break the registry

- **WHEN** `~/.workhorse-agent/external-agents/` contains three adapter files and one is unparseable YAML
- **THEN** the server starts successfully, the two valid adapters are registered, the malformed file is logged with its parse error, and `ExternalAgent` exposes the two valid adapters

#### Scenario: Server starts with empty registry

- **WHEN** no on-disk adapters exist and no builtin binaries are on `PATH`
- **THEN** the server starts successfully, the registry is empty, the `ExternalAgent` tool is not registered, and existing tool surfaces (`Bash`, `Read`, `Dispatch`, etc.) are unaffected

