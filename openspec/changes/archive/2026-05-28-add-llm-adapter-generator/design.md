## Context

`add-external-agent-tool` (sibling change, must merge first) establishes:

- A typed adapter schema at `internal/extagent/schema/adapter.schema.json`
- A registry that hot-reloads `~/.workhorse-agent/external-agents/*.yaml` on each session start
- A sub-process driver with cancel/timeout/envfilter
- A smoke-test runner that sandboxes a candidate adapter in a temp cwd with a minimised env
- Three builtin adapters (`claude-code`, `codex`, `aider`) as embedded YAML
- The `ExternalAgent` tool, enabled only when ≥1 healthy `sub_agent` adapter loads
- A first-invocation approval gate for `security.trusted: false` adapters

This change layers an **authoring loop** on top of that. The goal stated in the explore session: *"用户说'我装了 gemini，帮我接入'，系统点一下就能用"*. Two trigger surfaces are needed:

- **Explicit** (`workhorse-agent setup-agent gemini` from a terminal, or `agent_setup` tool call from the model when the user types it as an instruction)
- **Implicit** (the model emits `ExternalAgent {agent_name: "gemini", ...}`, the enum doesn't have `gemini`, but `gemini` resolves on PATH → loop synthesises `agent_setup`)

The generator itself is best modeled as a **`Dispatch`-driven subagent**, not as inline orchestrator code. That cleanly:

- Reuses the existing dispatch infrastructure (`internal/tools/dispatch/`, `internal/coord/loader.go`)
- Confines tool privileges via the existing `allowed_tools` / `denied_tools` mechanism
- Lets the subagent run its own loop (it may need multiple turns of `<bin> --help` → think → `man <bin>` → think → write draft)
- Keeps the meta-work out of the parent session's transcript by default (subagent events stream in wrapped form)

The drafting/publishing flow needs two filesystem locations distinct from the live adapter dir, so the registry never sees half-baked output:

- `<profileDir>/external-agents/.drafts/` — generator writes here (subagent has `WriteAdapterDraft` tool restricted to this prefix)
- `<profileDir>/external-agents/<name>.yaml` — final destination, atomic `rename(2)` after approval

The loader from Change 1 already ignores non-`.yaml` siblings (`.smoke` files); it must also explicitly skip the `.drafts/` subdir.

The approval UX needs to work in two modes: HTTP (programmatic, with a new SSE event + approval endpoint) and CLI (interactive prompt with `y/n/edit`). Both end in the same disk operation: write approved YAML to live dir → registry hot-reloads on next session.

## Goals / Non-Goals

**Goals:**

- A single user gesture ("set up gemini") produces a working, smoke-tested adapter file on disk, with zero hand-editing in the happy path.
- The generation flow is auditable: every generated adapter carries `provenance.generated_by`, `generated_at`, `tool_version`, `reviewed_by` and the raw `--help` output it was generated from (stored as a sibling `.genmeta` file).
- The flow cannot publish an adapter that fails schema validation or fails smoke test — those failures surface to the user, not to the live registry.
- The generator subagent operates under strict tool restrictions (allowlist enforced in Go code, not YAML).
- Implicit trigger is opt-out (default on), opt-in explicit triggering is always available via CLI or tool call.
- Reuse the schema, driver, smoke runner, and approval gate from Change 1 verbatim — no parallel implementations.

**Non-Goals:**

- Auto-regeneration on CLI version change — drift is detected and logged but never acted on without user gesture. (Avoids the surprise of "I upgraded `claude` and my adapter mysteriously rewrote itself.")
- Generation for `cli_tool`-class adapters — PATH scan already surfaces them; an LLM-generated adapter would add no value for binaries that are invoked through `Bash` anyway.
- Cross-machine adapter portability — generation runs against a specific binary on a specific machine; the resulting YAML is intentionally not portable (it may reference an absolute path).
- Generation from natural-language description alone (without an installed binary) — refused; the binary must be present so the generator can ground in real `--help` text.
- Persisting pending approvals across server restart — restarts cancel pending approvals (in-memory only); user re-triggers.
- "Marketplace" of community-generated adapters — out of scope; provenance metadata supports a future such system but no central service is built here.

## Decisions

### G0: Generator is a `Dispatch`-driven subagent type, not inline orchestrator code

**Choice**: A new builtin agent type `adapter-generator` lives at `internal/extagent/builtins/agents/adapter-generator.yaml` (embedded). Its definition:

- `system_prompt`: the `AdapterGeneration` prompt template from `internal/prompt/builtins.go`, rendered with the schema + collected metadata + few-shot examples.
- `allowed_tools`: `[Bash, Read, WriteAdapterDraft]` — hardcoded in Go code, NOT in the YAML (defense against the YAML being edited to add tools).
- `denied_tools`: `[Write, Edit, ExternalAgent, agent_setup, Dispatch]` — defensive blocklist preventing chains, recursion, or filesystem writes outside `.drafts/`.
- `provider`/`model`: inherits the parent session's defaults; can be overridden via `agent_setup` input.

`agent_setup` invokes `Dispatch` with `agent_type: adapter-generator` and seeds the first user message with the collected metadata + a templated instruction.

**Why**:
- Reuses every guarantee already proven in Change 1's predecessor work: per-agent tool restriction, depth cap, child-session lifecycle, panic recovery.
- Lets the generator take however many turns it needs without polluting the parent session's transcript.
- The agent type's restrictions live in code, so a tampered YAML can't escalate privileges (cf. Change 1's defense-in-depth posture on `dangerous-command` patterns).

**Alternative**: Inline generator code path in `agent_setup` that single-shots an LLM call with a giant prompt. Rejected — single-shot can't recover from "help output ambiguous, need to read man page"; multi-turn reasoning belongs in a loop, not a function.

### G1: `WriteAdapterDraft` is a new internal-only tool, not exposed to user-facing sessions

**Choice**: `internal/tools/extagent/draft/write.go` registers a tool `WriteAdapterDraft` that ONLY accepts paths matching `<profileDir>/external-agents/.drafts/<safe-name>.yaml`. The tool is NEVER added to `internal/tools/registry.go`'s default surface; it is injected into agent-type-specific tool surfaces only when the agent type is `adapter-generator`.

The pathguard package gets a new scope mode `DraftScope` (cf. the `MemoryScope` from `add-memory-l1-l2` Decision D3-ish) restricting to the drafts directory.

**Why**:
- Even if the `adapter-generator` agent type's `allowed_tools` list were tampered with, the tool itself wouldn't exist in the user-session registry to be added.
- The path restriction is enforced by pathguard, the project's "single chokepoint for filesystem access" per `CLAUDE.md`.

**Alternative**: Reuse `Write` with pathguard scope. Rejected — `Write` is widely allowlisted; coupling to that surface means any future privilege escalation in `Write` would automatically extend to drafts. A dedicated tool keeps the blast radius tight.

### G2: Drafts directory `<profileDir>/external-agents/.drafts/` is explicitly skipped by the registry loader

**Choice**: Update the loader from Change 1: when scanning `<profileDir>/external-agents/`, skip any subdirectory whose name starts with `.` (i.e. dotfiles convention). The loader already only processes `*.yaml` files in the top level, so this is a one-line `if isDir { continue }` plus a comment.

**Why**: Drafts must be invisible to the registry until atomically published via `rename(2)`. Hidden-dir convention is the simplest safe boundary.

**Alternative**: A separate top-level drafts dir like `<profileDir>/external-agent-drafts/`. Rejected — splits a single logical concept across two paths; harder to clean up; harder to debug.

### G3: Information collection — tiered, all read-only, time-bounded, version-failure tolerant

**Choice**: The generator subagent collects metadata in tiers, each with a hard time bound (enforced by the `Bash` tool's existing per-call timeout, not by the generator):

- **Tier 1 (mandatory)**: `which <bin>` → resolve absolute path. `<bin> --help` → primary signal. `<bin> --version` is attempted but **failure-tolerant** (see below).
- **Tier 2 (best-effort)**: `man <bin>` (if `man` is installed). Capped at 10000 chars in the prompt context.
- **Tier 3 (opt-in)**: README files found near the binary's install location (e.g. `dirname $(which <bin>)/../README*`). Capped at 5000 chars.

No internet access. No `<bin> --json-schema` or other speculative invocations. The generator's `Bash` calls are constrained by a generator-scoped command inspector (see G13 for the exact allowed-pattern regex) — not by the general dangerous-command guard.

**`--version` failure tolerance**: many CLIs are interactive when stdin is a TTY, or require a subcommand to print version, or simply exit non-zero from `--version`. When `<bin> --version` fails (non-zero exit, timeout, empty stdout), the generator MUST NOT guess a version string from `--help` text or fabricate one. The resulting `provenance.tool_version` field is set to the empty string. Drift detection (G10) handles empty `tool_version` by skipping the comparison (no false drift alerts). Generator output also includes a comment in the YAML noting that the version probe failed.

**Why**:
- Grounds the LLM in actual output of the actual binary on the actual machine.
- Avoids the failure mode where the LLM hallucinates flags that the CLI doesn't have.
- Read-only invocations limit blast radius (the binary may be malicious, but `--help` is a near-universal benign call).
- Empty `tool_version` is honest; a guessed string would cause spurious drift events on every restart.

**Alternative**: Fetch from a web `claude-code --docs` URL. Rejected — adds a network dependency, and the LLM has every incentive to invent URLs.

### G4: Generation prompt is a new template `AdapterGeneration` in `internal/prompt/builtins.go`

**Choice**: Template inputs: `{SchemaJSON, BinaryName, BinaryPath, HelpOutput, VersionOutput, ManOutput?, ReadmeOutput?, DescriptionHint?, Examples}`. The template renders:

1. The full JSON Schema for context.
2. The collected `--help` / `--version` / man content with clearly-delimited fences.
3. Few-shot examples: the three builtin adapter YAMLs (`claude-code`, `codex`, `aider`) verbatim.
4. Explicit instructions on field-by-field reasoning, including:
   - **`prompt_via` detection**: "Look for `--prompt`, `--message`, `--input`, `--ask` flags in `--help`. If present, use `prompt_via: arg`. If the CLI documentation says it reads from stdin, OR shows no prompt-passing flag but is interactive in normal use, use `prompt_via: stdin`. Many Unix tools follow the stdin convention — do not default to `arg` just because the built-in examples do."
   - **JSONPath restriction**: "The `output.parser.*` values must match the restricted grammar from the schema. Do not use `$..` (recursive) or filters."
   - **Absolute binary path**: "Set `binary` to the absolute path from `which <bin>` output, not the alias name. This protects against PATH changes on the user's machine."
5. **Few-shot disclaimer**: "The embedded examples below are snapshots from when this binary was built. Tools evolve — always prefer behavior observed in the actual `--help` output above over what the examples suggest. If `--help` contradicts an example, follow `--help`."
6. The required output format: a single YAML document inside a fenced block, then call `WriteAdapterDraft` with that content.
7. **cli_tool refusal clause**: "If `--help` shows no prompt-passing convention (no `--prompt`/`--message`/`--input`/`--ask` flag, no stdin-based interactive use), do NOT write a draft. Return a text response classifying this as a `cli_tool` and recommending the user add it to `external_agents.pathscan.extra`."

**Why**: Template renders identically across calls (byte-stable for cache hits on the same machine). Few-shot examples are the strongest signal for getting the JSONPath in `output.parser` right. The 2-out-of-3 `prompt_via: arg` ratio in builtins would bias the LLM if not counteracted by the explicit stdin-detection instruction.

**Alternative**: Conversational prompting ("ask the user for each field"). Rejected — turns a 1-gesture flow into a 20-question interview.

### G5: Smoke test happens *before* approval, on the draft file

**Choice**: After the generator subagent emits a draft, `agent_setup`:

1. Parses & schema-validates the draft (rejects → return parse error to the user, no approval prompt).
2. Runs the existing smoke runner from Change 1 with the draft as the candidate adapter, sandbox cwd under `os.TempDir()`, minimal env. Smoke output is captured.
3. Composes the approval payload: the draft YAML, the smoke result (pass/fail + captured output), the binary path, the model id that generated it, the version of the binary at generation time.
4. Emits `adapter_approval_request` SSE event (HTTP path) or interactive prompt (CLI path).

**Why**:
- The user should know at approval time whether the adapter actually works, not find out post-merge.
- Reuses Change 1's smoke infrastructure unchanged — no parallel implementation.
- Smoke failure does NOT auto-reject; the user may want to approve a fail-but-close adapter and fix it manually. (Approval payload distinguishes "smoke passed" vs "smoke failed but you can still approve".)

**Alternative**: Skip smoke pre-approval; rely on Change 1's smoke at load time. Rejected — adds a "approval → file lands → load → smoke fails → user must re-approve" cycle. Bad UX.

### G6: Approval payload structure and diff format

**Choice**: The `adapter_approval_request` event carries:

```jsonc
{
  "approval_id": "01HXXX...",
  "type": "adapter_generation",
  "binary": "/usr/local/bin/gemini",
  "draft_yaml": "name: gemini\nbinary: gemini\n...",
  "prior_yaml": null,                   // populated on regenerate; full prior YAML for inspection
  "diff_against_prior": null,           // populated on regenerate; unified diff (RFC 8174-ish format) between prior and draft
  "smoke_result": {
    "passed": true,
    "stdout": "WORKHORSE_SMOKE_OK\n",
    "stderr": "",
    "exit_code": 0,
    "duration_ms": 1240
  },
  "provenance": {
    "generated_by": "claude-opus-4-7",
    "generated_at": "2026-05-27T11:23:00Z",
    "tool_version": "0.4.1"
  },
  "expires_at": "2026-05-27T11:28:00Z"
}
```

**Diff format**: when present, `diff_against_prior` is a **unified diff** (output of `diff -u prior.yaml draft.yaml` semantics, generated by `github.com/sergi/go-diff` or equivalent vendored library — confirm at implementation; no new dependency added). Format chosen because: (a) human-readable in any terminal; (b) common in code review tools; (c) deterministic from the two source files. Both `prior_yaml` and `diff_against_prior` are included so the consumer can re-derive the diff with its own tooling if needed.

The endpoint `POST /sessions/{id}/approvals/{approval_id}` accepts:

```jsonc
{ "decision": "approve" | "reject" | "edit", "edited_yaml": "..." }
```

`edit` mode: client may submit an edited YAML; server re-runs schema validation + smoke before publishing.

**Why**:
- Full draft visible — the user sees exactly what is about to be written.
- Smoke result lets the user spot issues before approving.
- `edit` mode is the safety valve for the LLM-got-it-mostly-right case.
- Including both `prior_yaml` and `diff_against_prior` removes ambiguity about what the diff is against.

### G7: Approval timeout = 5 minutes (configurable), auto-reject on expiry

**Choice**: `external_agents.generation.approval_timeout_sec` (default `300`). On expiry, the pending approval is removed from in-memory state, the draft file is deleted, and an `adapter_approval_expired` event is emitted. The user must re-trigger `agent_setup` to retry.

**Why**:
- Bounded in-memory state.
- Doesn't leave drafts littering the disk indefinitely.
- 5 minutes is long enough for a distracted user to come back; longer windows accumulate junk.

### G8: Implicit trigger — Plan A (informative tool_result, user-mediated retry)

**Choice**: When the model emits `ExternalAgent {agent_name: X, ...rest}` where X is not in the enum but a binary named X resolves on `PATH`, AND `implicit_trigger_enabled` is true (default), AND no synth for X has already been attempted this session, the agent loop performs this exact sequence:

1. **Detection** (in the tool-call interceptor, a new hook in `internal/agent/loop.go` running BEFORE tool dispatch for the `ExternalAgent` tool):
   - Recognize unknown `agent_name`.
   - `exec.LookPath(agent_name)` to confirm a binary exists.
   - Check the per-session synth-dedup set; if X is already there, skip to step 5b.
2. **Synchronous in-loop setup**: the interceptor calls the same internal entrypoint that `agent_setup`'s tool handler uses, providing `binary: X`. This entrypoint:
   - Runs the `adapter-generator` subagent (this is a separate Dispatch — yes, the interceptor blocks the original tool call for the duration of the subagent run, typically 5-15s).
   - Validates and smoke-tests the resulting draft.
   - Registers a pending approval with the approval manager and emits the `adapter_approval_request` SSE event to the user's stream.
3. **Mark dedup**: add X to the per-session synth-dedup set with state `pending` (so a second unknown-X call in the same session doesn't re-trigger).
4. **Return informative tool_result for the ORIGINAL `ExternalAgent` tool_use**: NO synthesized tool_use is added to the conversation. The original `ExternalAgent` call's tool_result text reads:

```
Adapter for 'X' was not registered. Detected '/usr/local/bin/X' on PATH and initiated
automatic setup. Approval request <approval_id> has been sent to the user via SSE.

Your original call's parameters were:
{
  "agent_name": "X",
  "prompt": "...",
  "resume_session_id": "..."   // if you set one
}

Once the user approves (which will arrive as an out-of-band action, not in this
conversation), you can retry your call by emitting the same ExternalAgent tool_use
with the same parameters. If the user rejects, the adapter will not become available
and you should choose a different approach.
```

5. **Dedup-state transitions**:
   - 5a. If the user **approves**: approval manager clears the dedup entry for X. The model's next retry of `ExternalAgent {agent_name: X, ...}` succeeds because (i) the new adapter is loaded into the session via hot-reload on next session start OR is injected into the current session's registry by the approval handler (see implementation note below), and (ii) the dedup no longer blocks it.
   - 5b. If the user **rejects** or the approval **expires**: the dedup entry for X remains, but transitions to state `unavailable`. A subsequent retry by the model returns: `"Adapter setup for 'X' was rejected/expired this session. Use a different approach or ask the user to manually configure."`

Disabled via `external_agents.generation.implicit_trigger_enabled: false`.

**Implementation note on live-registry injection**: when an approval is published mid-session, the approval handler also adds the new adapter to the current session's in-memory registry snapshot (Change 1's snapshots are by-value but the approval handler holds a reference). This means the model can retry on the very next turn without waiting for a new session — important UX. The next session start also picks it up via Change 1's hot-reload, as usual.

**Why Plan A (not the "synthesized tool_use" alternative)**:
- The agent-loop spec mandates that tool_use originates from the model. Framework-synthesised tool_use violates that semantic and creates surprising history entries.
- Plan A's "user mediates retry" pattern is explicit and inspectable; the model can react sensibly (paraphrase to the user, wait for ack).
- The `resume_session_id` (and any other call parameters) are preserved in the tool_result text and in the model's own assistant block — the model can re-emit them verbatim on retry. Solves the otherwise-lost-context bug.

**Risks** → Mitigation:
- *Loop / oscillation* (model retries before approval arrives): dedup-state `pending` causes second tries within the same session to short-circuit with `"setup pending; wait for user"` instead of re-triggering generation.
- *Approval friction* (user gets a popup mid-task without asking): the popup is the SSE event the user's frontend renders; if the user finds it disruptive they can disable implicit trigger via config.
- *Model doesn't understand "wait for out-of-band approval"*: the tool_result text is explicit. Few-shot examples in the orchestrator's system prompt (a Change 1 follow-up) can reinforce this pattern.

**Alternative — synthesized tool_use**: Rejected. Would violate the agent-loop spec and force the model to react to history entries it didn't author.

### G9: Generated adapter publication is atomic via `rename(2)`

**Choice**: After approval:

1. Validate the (possibly edited) YAML again.
2. Write the final YAML to `<profileDir>/external-agents/.drafts/<name>.yaml.tmp` (or directly to the approved name in `.drafts/`).
3. Use `os.Rename(.drafts/<name>.yaml, <profileDir>/external-agents/<name>.yaml)` — on the same filesystem, this is atomic.
4. Write a sibling `<name>.genmeta` file under the live dir capturing the original `--help` output, the prompt, the model id, and timestamps (for audit and future re-generation).
5. The next session start sees the new adapter via Change 1's hot-reload.

**Why**:
- No window where a partial YAML exists in the live dir.
- Reusing `rename(2)` keeps the operation crash-safe.
- `.genmeta` is the audit trail; the loader from Change 1 explicitly ignores non-`.yaml` siblings (already covered by its scope).

### G10: Drift detection at server startup, log-only

**Choice**: On server startup, after the registry loads, iterate every adapter with `provenance.source: llm_generated`. For each: run `<binary> --version`, compare to `provenance.tool_version`. If different:

- Emit a structured log line: `external_agent.drift name=gemini was=0.4.1 now=0.5.0 adapter_path=...`.
- Surface in `GET /diagnostics` under `external_agents.drift: [{name, was, now}]`.
- Do NOT regenerate, do NOT mark unhealthy, do NOT change loaded behavior.

**Why**:
- "Auto-regenerate" surprises the user with quiet rewrites.
- The user knows that a CLI just upgraded; they can `workhorse-agent setup-agent gemini --regenerate` if they want fresh metadata.
- A log line + diagnostics endpoint is enough for an attentive operator.

**Alternative**: Hard-fail the adapter on version mismatch. Rejected — CLIs often add flags without removing old ones; existing adapter likely still works.

### G11: Models — same as session default, overridable per-call

**Choice**: The `adapter-generator` subagent inherits the parent session's provider/model unless `agent_setup` passes overrides. Add `external_agents.generation.allowed_models: [...]` (default empty = "any model the user's provider exposes") as a guardrail against picking a model that's too small.

**Why**:
- The user paid for one model; generation should use it.
- Allowed-models config is a safety net for a future "small fast model" gateway that the user might not want generating adapters.

### G12: Generator command inspector — exact allow-list regex (read-only probe principle)

**Choice**: The generator subagent's `Bash` tool invocations go through a dedicated command inspector (registered as a generator-scope hook on top of the existing dangerous-command guard). The principle underlying the allowlist is **read-only probes of a binary's documentation/identity** — no execution of the binary's primary functionality, no filesystem mutation, no network. The accepted patterns:

```
^which\s+\S+$                                  # PATH lookup
^type\s+\S+$                                   # shell builtin equivalent
^command\s+-v\s+\S+$                           # POSIX equivalent
^readlink\s+(-f\s+)?\S+$                       # resolve symlinks (e.g. resolve `which` output)
^file\s+\S+$                                   # binary type detection
^ls\s+(-l\s+)?\S+$                             # list install-prefix dir contents (path-checked)
^man\s+\S+$                                    # man page
^cat\s+\S+$                                    # README/CHANGELOG (path-checked)
^head(\s+-n\s+\d+)?\s+\S+$                     # truncated read of large doc files
^\S+\s+(--help|-h|help|-\?)(\s.*)?$            # <bin> --help / -h / help [subcmd] / -?
^\S+\s+(--version|-V|version)$                 # <bin> --version variants
```

The inspector rejects ANY input containing shell metacharacters (`;`, `|`, `&`, `&&`, `||`, `>`, `<`, `>>`, `<<`, backticks, `$(...)`, newlines). The inspector runs BEFORE the dangerous-command guard so generator-scope rejection is more specific than the default scope.

For the path-taking forms (`readlink`, `cat`, `head`, `ls`, `file`), the inspector additionally requires the path to (a) resolve, (b) be under the install prefix of the binary being analyzed (e.g. `dirname $(which <bin>)/..`) OR be a standard system documentation root (`/usr/share/man/`, `/usr/share/doc/`), and (c) pass the standard pathguard sanity checks. This prevents the generator from `cat /etc/shadow`-ing via prompt injection.

**The `<bin>` slot in patterns is intentionally any non-whitespace token, not pinned to the target binary**: the generator may need to compare two CLIs (e.g. run `git --version` while analyzing `gh`). Read-only probes against any binary are harmless and waste only API tokens.

**Why a curated allowlist not a free-form principle filter**: "read-only" is hard to enforce mechanically (`git status` is read-only but `git checkout` mutates working tree; `find -delete` is destructive). An enumerated list of well-understood patterns is auditable and tight. Operators adding adapters for genuinely novel CLIs that need probes outside this list can either (a) generate manually and skip the generator, or (b) propose extensions in a follow-up change.

**Why not lean on the existing dangerous-command guard alone**: that guard catches the 8 well-known harmful patterns (`rm -rf /` etc.) but lets through almost anything else. The generator's job is narrow; the inspector should be correspondingly narrow.

**Coverage check against typical needs**: `which gemini` (yes), `gemini --version` (yes), `gemini --help` (yes), `gemini chat --help` (yes — subcommand help), `man gemini` (yes), `readlink -f $(which gemini)` — wait, this contains `$(...)` and is REJECTED. The generator must instead run `which gemini` first (capture stdout), then `readlink -f /the/absolute/path` as a separate call. This is intentional: command-substitution is dangerous to allow in any form.

### G13: System shell refusal

**Choice**: `agent_setup` MUST refuse — at preflight, before invoking the generator — to generate an adapter for any binary whose resolved absolute path's basename is in the set `{bash, sh, zsh, dash, csh, fish, ash, ksh, tcsh}`. The refusal is a clean tool_result: `"Refusing to generate an adapter for shell '<name>'. Shells are not appropriate as sub_agents (they accept arbitrary input and have unlimited capability). If you need shell access, use the Bash tool directly."`

**Why**: An adapter for `bash` would be a back-door equivalent of the dangerous-command guard's worst-case (unfiltered shell). The `cli_tool` refusal clause (G4) might happen to catch this case, but it relies on the generator's reasoning; an explicit pre-check is more reliable.

### G14: Smoke output format validation for structured-format adapters

**Choice**: After the regular smoke test (`expected_substring` check) passes for a draft whose `output.format` is `jsonl`, `streaming-json`, or `sse`, an additional check MUST be run:

- For `jsonl`: every non-empty line in captured stdout must parse as valid JSON. At least one line must contain a value at the JSONPath in `output.parser.assistant_text` (if specified).
- For `streaming-json`: same as `jsonl` but tolerate a trailing partial line.
- For `sse`: at least one `data: ...` event must be present and its payload must parse as JSON.

A draft that passes the substring check but fails this format check is recorded as smoke-failed with reason `output_format_mismatch`. The approval payload still surfaces the draft to the user (per G5, smoke fail does not block approval), but with a clearer error reason in the smoke result.

**Why**: Catches the "CLI silently ignored a hallucinated flag" case. If the LLM invented `--stream-json` and the CLI just ran in default mode producing plain text, the substring `WORKHORSE_SMOKE_OK` might still appear (CLIs often echo their input or print confirmations). The format check catches the disconnect between declared and actual output structure.

**Alternative**: Only do the substring check. Rejected — the failure mode (production calls return empty extracts because format mismatch) is exactly the silent-bug class we want to catch pre-approval.

### G15: Failure modes — explicit, non-mysterious

**Choice**: Every failure mode in `agent_setup` returns a tool_result text naming the failure and the suggested user action:

| Failure                                             | Tool result text                                                                                    |
|-----------------------------------------------------|----------------------------------------------------------------------------------------------------|
| Binary not on PATH                                  | `Cannot set up <name>: binary not found on PATH. Install it first.`                                |
| `<bin> --help` exits non-zero or produces nothing   | `Cannot generate adapter: <bin> --help returned no usable content. Add manually under .../<name>.yaml.` |
| Schema validation on generated YAML fails           | `Generated adapter failed schema: <error>. Try regenerating, or write manually.`                   |
| Smoke test fails                                    | Surface to approval anyway (G5) with `smoke_result.passed: false`.                                 |
| Approval expires                                    | `Adapter approval for <name> expired without decision. Re-run setup if still desired.`             |
| User rejects                                        | `Adapter setup for <name> rejected by user.`                                                       |
| Generator subagent panics                           | `Adapter generation failed internally (logged). Retry, or write manually.`                         |

**Why**: Mystery failures are the worst class of bug. Each surface is a one-line clarification the model can summarise to the user.

## Risks / Trade-offs

- **[Risk] LLM hallucinates a flag that doesn't exist; smoke test passes by coincidence (e.g. `--prompt` falls back to `--input` silently)** → Mitigation: smoke test requires `expected_substring`, which the generator picks; if the substring doesn't appear, smoke fails and the user sees the failure pre-approval. We also store the raw `--help` in `.genmeta` so an operator can audit.

- **[Risk] Generator subagent privilege escalation via prompt injection in `--help` text** (e.g. `--help` output contains text that tries to convince the LLM to run dangerous Bash) → Mitigation: (a) generator's `Bash` invocations go through the dedicated generator-scope command inspector (G12) which allows only narrow help/version/cat-of-install-prefix patterns; (b) generator's `Write` is denied; only `WriteAdapterDraft` (restricted path) is allowed; (c) generator cannot `Dispatch` further (cycle prevention); (d) post-generation, every `extra_args` and `env_override` string value in the draft is scanned for dangerous-command patterns; (e) preflight refuses to generate adapters for system shells (G13).

- **[Risk] Drift between machines: same adapter generated against `claude` v0.4.1 on one box, used against v0.5.0 on another** → Mitigation: `provenance.tool_version` lets drift detection surface this. Adapters are explicitly machine-local artifacts (Non-Goal: portability).

- **[Risk] Approval-popup-fatigue: every implicit trigger asks for approval** → Mitigation: 5-minute timeout means stale popups disappear; user can disable implicit trigger; user can pre-create the adapter explicitly to skip the popup later.

- **[Risk] Generator produces a working-but-non-idiomatic adapter (e.g. uses `text` output when `streaming-json` is available)** → Mitigation: few-shot examples bias toward the richer format; the audit `.genmeta` lets a future operator regenerate manually. Acceptable trade-off for the gesture-economy goal.

- **[Risk] Few-shot examples in the prompt go stale as the actual CLIs evolve** → Mitigation: G4's explicit disclaimer ("prefer behavior observed in actual --help output over what the examples suggest") instructs the LLM to weight live data over frozen examples. Smoke test (G15 format validation) catches drift. Long-term, a CI job could re-render the embedded examples from regenerated adapters; out of scope for MVP.

- **[Risk] All filesystem artifacts (adapters, .smoke, .genmeta, draft files, cache) carry secret-bearing references (env passthrough names, captured URLs with tokens)** → Mitigation: every file uses mode `0600`, every directory uses `0700`. Single-user system per CLAUDE.md but conservative modes protect against accidental file sharing (e.g. rsync to multi-user host).

- **[Risk] `rename(2)` across filesystems fails (e.g. profileDir on tmpfs vs drafts on overlay)** → Mitigation: drafts dir is a SUBDIRECTORY of the live dir, guaranteeing same filesystem. Documented in code.

- **[Risk] Implicit trigger races with another in-flight setup for the same binary (model retries fast)** → Mitigation: in-memory dedup keyed by binary name; a second synth for the same `name` while the first is pending returns the pending approval id without starting a second generation.

## Migration Plan

This change is purely additive on top of `add-external-agent-tool`. Deployment sequence:

1. `add-external-agent-tool` merges and ships first.
2. `add-llm-adapter-generator` merges next. Server start:
   - New config keys take their defaults; nothing breaks.
   - `<profileDir>/external-agents/.drafts/` is created with mode `0700` on first session.
   - The `agent_setup` tool is registered on every session by default. The `adapter-generator` agent type is loaded from the embedded builtin.
   - Implicit trigger is enabled by default; user can disable in config.
3. Rollback: revert the binary. Any in-flight approvals are lost (in-memory only). Any drafts in `.drafts/` are orphaned (harmless — older binary ignores the dir). Any approved/published adapters continue to work under the older binary because their YAML is identical to a hand-written one.

## Open Questions

1. **Approval UI for the CLI path**: should `workhorse-agent setup-agent <bin>` block the terminal awaiting `y/n`, or print the approval_id and require a separate `workhorse-agent approve <id>` call? **Default for MVP**: blocking interactive prompt (TTY check); fall back to printing `approval_id` when stdin is non-TTY (scripted usage).

2. **Editing a draft mid-approval**: should the `edit` decision re-run the generator subagent on the edited YAML to "lint" it, or treat the user's edit as authoritative and only run schema + smoke? **Default for MVP**: trust user edit; only schema + smoke.

3. **`.genmeta` privacy**: the raw `--help` output may include the user's username, tokens in URL examples, etc. **Default for MVP**: write `.genmeta` with mode `0600` (same as `.smoke`); document that `.genmeta` is not portable. Revisit if anyone wants to share adapters.

4. **Generation cost accounting**: should generation token usage be attributed to the session that triggered it, a special "system" session, or its own session id? **Default for MVP**: own session id (child session under Dispatch), per existing dispatch semantics. The user sees the cost in their session list.

5. **Re-running smoke after approval but before publish**: G9 only validates schema before `rename(2)`. Should it also re-run smoke (a second time) post-approval? **Default for MVP**: no — the smoke we ran pre-approval is the one the user looked at. Re-smoking adds latency without new information unless the user edited the draft.

6. **Auto-detect "this is actually a `cli_tool` not a `sub_agent`"**: if the generator looks at `--help` and the binary clearly doesn't take a prompt-like input (e.g. `ls`), should it refuse to generate a `sub_agent` adapter and instead suggest "this is a cli_tool, just add it to `pathscan.extra`"? **Default for MVP**: yes — the generator's instructions include a "if no prompt-passing convention is detectable, return a tool_result explaining cli_tool is the right fit and do not WriteAdapterDraft" clause.
