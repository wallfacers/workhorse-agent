## 0. Prerequisite confirmation

- [ ] 0.1 Confirm `add-external-agent-tool` has been archived (its tasks marked complete) before any work in this change starts; do not begin implementation if Change 1's adapter schema or smoke runner is still in flux
- [ ] 0.2 Re-read Change 1's design.md decisions before authoring code in this change — load-bearing for integration points:
    - D4 (adapter schema): governs what WriteAdapterDraft writes; tasks 3.x build on it
    - D9 (envfilter reuse via `bash.Filter`/`bash.LogDropped`): generator's Bash inspector and smoke sandbox both rely on it; task 4.4 builds on it
    - D11 (output cap + raw-output dump): smoke runner reuses the same driver; task 6.7 reuses
    - D12 (JSONPath subset + error semantics): generator's output-format check in task 6.7 must produce paths matching the schema's grammar
    - D13 (smoke sandbox): draft smoke runs through the same path; task 6.7 reuses
    - D17 (first-invocation approval): the "Approved adapter skips first-invocation approval in originating session" requirement modifies its behavior; task 10.4 implements
    - D20 (orchestrator timeout layering): `agent_setup`'s `DefaultTimeout()` must return generously to cover information-collection + subagent + smoke; task 6.2 sets this
    - D21 (permission gating internal to Run): `agent_setup` likely needs the same `InternalGated` marker pattern; task 6.1 considers
- [ ] 0.3 After `add-external-agent-tool` archives, flesh out the formal `MODIFIED Requirements` blocks in `specs/adapter-generation/spec.md` against `openspec/specs/external-agents/spec.md` for the two cross-capability interactions documented in spec.md (pre-rejection intercept; first-invocation-approval skip in originating session). Re-run `openspec validate` after

## 1. Configuration

- [ ] 1.1 Add `external_agents.generation.enabled` (default `true`), `external_agents.generation.approval_timeout_sec` (default `300`), `external_agents.generation.implicit_trigger_enabled` (default `true`), `external_agents.generation.allowed_models` (default `[]`) to the config schema in `internal/config`
- [ ] 1.2 Wire defaults so all keys are optional; existing configs must not require edits
- [ ] 1.3 Config-validation tests for each new key including type checks and the "empty allowed_models means any" semantics

## 2. Drafts directory and pathguard scope

- [ ] 2.1 Add a `DraftScope` mode to `internal/tools/pathguard` containing root at `<profileDir>/external-agents/.drafts/`; reuse the existing symlink resolution + `O_NOFOLLOW` semantics
- [ ] 2.2 Update the registry loader from `add-external-agent-tool` to explicitly skip any subdirectory whose name starts with `.` (one-line guard plus comment naming Change 2 as the reason)
- [ ] 2.3 Tests asserting DraftScope rejects writes outside `.drafts/` and symlinks whose targets escape; tests asserting the loader does not pick up files under `.drafts/` even when they are valid YAML
- [ ] 2.4 Tests asserting the drafts dir is created with mode `0700` on first `WriteAdapterDraft` use

## 3. WriteAdapterDraft tool

- [ ] 3.1 Create `internal/tools/extagent/drafttool/write.go` (note: package name `drafttool` — distinct from `internal/extagent/draft/` which holds publish/rename logic. Avoiding two `draft` packages keeps imports unambiguous) implementing the `WriteAdapterDraft` tool with input schema `{path: string, content: string}`. Schema must conform to Change 1's adapter schema (D4) so the tool can pre-validate before writing; reject any `path` not matching `<profileDir>/external-agents/.drafts/<safe-name>.yaml` where `safe-name` matches `^[a-z0-9][a-z0-9_-]{0,63}$`
- [ ] 3.2 Atomic write (temp file + rename) with mode `0600`. Drafts directory mode `0700` (consistent with Change 1 D22 single-rationale policy)
- [ ] 3.3 Do NOT register the tool with the global tool registry; instead expose it as an "agent-type-scoped tool" that is only added to a session's surface when the session's agent type is `adapter-generator`
- [ ] 3.4 Tests: valid path accepted, live-dir path rejected, traversal rejected, invalid name regex rejected, atomic write semantics

## 4. adapter-generator agent type

- [ ] 4.1 Author `internal/extagent/builtins/agents/adapter-generator.yaml` defining the agent type with a placeholder `allowed_tools` list (the actual enforcement happens in code; the YAML is documentation)
- [ ] 4.2 In `internal/coord/loader.go` (or the appropriate seam), inject the code-side enforcement: when an agent type's `name` is `adapter-generator`, its effective tool surface is fixed to `[Bash, Read, WriteAdapterDraft]` regardless of YAML content
- [ ] 4.3 Add `denied_tools` enforcement so the subagent cannot call `Dispatch`, `ExternalAgent`, `agent_setup`, `Write`, `Edit`
- [ ] 4.4 Generator subagent's `Bash` command inspector: implement a generator-scope hook running BEFORE the dangerous-command guard. The inspector accepts ONLY commands matching the expanded regex set from spec.md §"Information collection scope and exact command grammar":
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
    Reject ANY input containing shell metacharacters (`;`, `|`, `&`, `&&`, `||`, `>`, `<`, `>>`, `<<`, backticks, `$(...)`, newlines). For path-taking patterns (`readlink`, `cat`, `head`, `ls`, `file`), validate the path resolves under either the binary's install prefix (`dirname $(which <bin>)/..`) OR a standard system documentation root (`/usr/share/man/`, `/usr/share/doc/`), and passes standard pathguard checks
- [ ] 4.5 Tests: (a) tampered YAML cannot add tools — generator's effective tool surface stays `[Bash, Read, WriteAdapterDraft]`; (b) Dispatch / ExternalAgent / Write are rejected if attempted; (c) every regex-allowed Bash command proceeds (test each pattern); (d) every metacharacter-bearing command rejected (test each metachar individually); (e) `cat /etc/passwd` rejected when binary install prefix is `/usr/local/bin/`; (f) `cat <install_prefix>/README` allowed; (g) `cat /usr/share/doc/git/README` allowed; (h) `readlink -f $(which gemini)` rejected (command substitution) — generator must split into `which gemini` + separate `readlink -f /abs/path`; (i) `git --version` allowed even when analyzing `gemini` (cross-binary probes are harmless)

## 5. AdapterGeneration prompt template

- [ ] 5.1 Add `AdapterGeneration` template to `internal/prompt/builtins.go` with placeholders `{SchemaJSON, BinaryName, BinaryPath, HelpOutput, VersionOutput, ManOutput, ReadmeOutput, DescriptionHint, Examples}`
- [ ] 5.2 Include in the template: the full adapter JSON Schema (Change 1 D4), the three builtin adapter YAMLs as few-shot examples, explicit field-by-field reasoning instructions including:
    - `prompt_via` detection (counterweight the 2:1 arg-vs-stdin builtin ratio — explicitly instruct LLM to detect stdin conventions)
    - JSONPath restricted-grammar reminder (Change 1 D12)
    - Absolute binary path requirement (use resolved `which` output, not alias — handles the `python` vs `python3` case)
    - cli_tool refusal clause (refuse when no prompt-passing convention)
    - "call WriteAdapterDraft as your final action" closing
- [ ] 5.3 Include in the template a few-shot disclaimer: `"The embedded examples below are snapshots from when this binary was built. Tools evolve — always prefer behavior observed in the actual --help output above over what the examples suggest. If --help contradicts an example, follow --help."` Placed between the schema and the examples so it conditions interpretation
- [ ] 5.4 Tests: template renders byte-stably across calls; empty optional inputs collapse cleanly; non-empty inputs produce well-formed sections; the few-shot disclaimer is always present even when no examples are interpolated

## 6. agent_setup tool

- [ ] 6.1 Create `internal/tools/agentsetup/tool.go` with `Tool{Host}` where `Host` bundles registry, dispatch handle, draft writer, smoke runner, approval manager, command inspector. The tool implements the `tools.InternalGated` marker interface so the orchestrator skips `Permissions.Check` (we gate internally — Change 1 D21 pattern)
- [ ] 6.2 Implement `DefaultTimeout()` returning a generous value (e.g. 600s) covering information collection + subagent run + smoke; this sits within the orchestrator's backstop. Document the choice inline. (Cross-reference Change 1 D20)
- [ ] 6.3 Implement input schema `{binary, description_hint?, regenerate?, model?}`; reject unknown fields
- [ ] 6.4 Implement preflight per spec §"agent_setup binary preflight": resolve path, reject missing, reject already-registered without regenerate, reject sensitive paths (`/proc/`, `/sys/`, `/dev/`), reject system shells (`bash`, `sh`, `zsh`, `dash`, `csh`, `fish`, `ash`, `ksh`, `tcsh`) per design.md G13
- [ ] 6.5 Implement metadata collection: run `which`, `<bin> --help` (mandatory tier); attempt `<bin> --version` but tolerate failure (empty `tool_version` if probe fails — design.md G3); optionally `man <bin>` and README if found; cap each output length per design.md §G3
- [ ] 6.6 Render the `AdapterGeneration` prompt with collected metadata; build the initial user message for the subagent
- [ ] 6.7 Invoke `Dispatch` with `agent_type: adapter-generator`, the rendered prompt, and the inherited (or overridden) model; capture the subagent's final draft path
- [ ] 6.8 On subagent success: (a) parse and schema-validate the draft (reject on failure with a clear tool_result); (b) run dangerous-command pattern scan on every `invocation.extra_args` and `invocation.env_override` key+value string per spec §"Dangerous-command scan on generated argument and env strings" (reject on hit); (c) run smoke on the draft via Change 1's smoke runner (D13 sandbox) with `provenance.source: llm_generated` recorded; (d) for `output.format ∈ {jsonl, streaming-json, sse}`, run the additional output-format validation per spec §"Sandbox smoke test on draft" (failure marks smoke as `output_format_mismatch` but does NOT block approval); (e) generate the diff against prior YAML (when `regenerate: true` and an existing adapter exists) using unified diff format (G6); (f) emit `adapter_approval_request` SSE event with the full payload (draft_yaml, prior_yaml, diff_against_prior, smoke_result, provenance); (g) register the pending approval in the approval manager
- [ ] 6.8a Retry hygiene: BEFORE invoking the generator subagent (task 6.7), if a draft file for the same `name` already exists in `.drafts/` from a prior failed attempt, delete it. This prevents stale partial drafts from a failed run leaking into a fresh approval. Test: simulate a generator failure mid-write, retry, assert the second run's draft is the complete new content (not a merge of partial + new)
- [ ] 6.9 On subagent failure (cli_tool refusal, panic, timeout): return a clear tool_result per spec §"Failure modes — explicit, non-mysterious"
- [ ] 6.10 Tests covering each branch: happy path; preflight rejections (missing binary, already-registered, sensitive path, system shell); schema failure; dangerous-command scan rejection (extra_args, env_override key, env_override value); smoke pass with format-check pass; smoke pass with format-check fail (still surfaces approval); smoke fail substring; cli_tool refusal; absolute-binary-path verification (alias `python3` → adapter uses `/usr/local/bin/python3`); empty `tool_version` recorded when probe fails

## 7. Approval manager (foundational state + lifecycle)

- [ ] 7.1 Create `internal/extagent/approval/manager.go` with `Manager` holding `map[approval_id]*PendingApproval` guarded by a mutex; entries include draft path, draft YAML, prior YAML (when regenerate), unified diff, smoke result, expires_at, session id, agent_name (for dedup clear-on-approve interaction with implicit trigger)
- [ ] 7.2 Implement `Register(pending) approval_id` that ULID-generates an id and schedules an `AfterFunc` to expire the entry
- [ ] 7.3 Define the `Decide(approval_id, decision, edited_yaml?) error` interface signature here (do NOT couple to publish logic yet — that lives in section 8). On `edit`, this method only re-validates schema + re-runs dangerous-command scan + re-runs smoke; on `approve`, it invokes the publish hook (section 8 dependency-injected). On `reject`, it purges in-memory state and deletes draft file
- [ ] 7.4 Implement `Expire(approval_id)` removing the entry, deleting the draft, emitting `adapter_approval_expired`
- [ ] 7.5 Define hook interfaces the manager calls to: (a) publish (`type Publisher interface { Publish(draft, ...) error }`), (b) inject into current session's registry (`type RegistryInjector interface { Inject(sessionID string, adapter *Adapter) }`), (c) clear implicit-trigger dedup (`type DedupClearer interface { ClearImplicitTriggerDedup(sessionID, agentName string) }`). All optional (nil-safe) so tests can mock independently
- [ ] 7.6 Tests: register + expire, register + reject, register + decide-approve invokes Publisher then RegistryInjector then DedupClearer in order, register + decide-edit re-runs validation but does NOT invoke Publisher (left for a subsequent approve), decide after expiry returns 404-equivalent error

## 8. Atomic publish, genmeta sibling, registry injection (foundational publish layer)

(Numbered AFTER 7 so the approval manager has a Publisher to wire into. Numbered BEFORE 9 (HTTP layer) so the API hands off to a working publish pipeline.)

- [ ] 8.1 Create `internal/extagent/draft/publish.go` exposing `Publish(draftPath, liveDir, genmeta GenmetaPayload) error` that: (a) re-validates the YAML on disk one final time; (b) atomically renames the draft to `<liveDir>/<name>.yaml` (same-filesystem rename — guaranteed since drafts dir is a subdir of live dir); (c) writes `<liveDir>/<name>.genmeta` with mode `0600` containing the rendered prompt, captured `--help`/`--version`/`man` outputs, model id, generation timestamp
- [ ] 8.2 Implement `RegistryInjector` impl: locks the session's registry snapshot, adds the new adapter, releases. Snapshot is now mutable via this single chokepoint
- [ ] 8.3 Tests: rename within same filesystem succeeds atomically; genmeta written with correct mode and content; publish is idempotent under concurrent calls (second call returns a clean error rather than corrupting); registry injection makes adapter visible to subsequent ExternalAgent calls in the same session

## 9. HTTP approval endpoint and SSE events (delivery layer)

(Numbered LAST because it depends on 7 + 8 being wired.)

- [ ] 9.1 Add handler for `POST /sessions/{id}/approvals/{approval_id}` in `internal/api/`; accept body `{decision: approve|reject|edit, edited_yaml?: string}`; require existing bearer-token auth via constant-time compare
- [ ] 9.2 Add SSE event types `adapter_approval_request`, `adapter_approval_resolved`, `adapter_approval_expired` to the API protocol; document payload shapes — the request event carries `draft_yaml`, `prior_yaml`, `diff_against_prior` (unified diff format), `smoke_result`, `provenance`, `expires_at`
- [ ] 9.3 Wire approval manager into the API surface so events are emitted on the session's SSE channel
- [ ] 9.4 Wire the manager's Publisher/RegistryInjector/DedupClearer hooks to the impls from section 8 and section 10 respectively
- [ ] 9.5 Tests: end-to-end approve via HTTP (assert draft moved, .genmeta written, registry injected, hot-reload picks up adapter on next session, dedup cleared); reject (assert draft deleted, dedup transitions to `unavailable`); edit valid (assert re-validation + re-smoke); edit invalid returns 422 keeping approval open; unauthenticated request returns 401; expired approval returns 404

## 10. Implicit trigger — Plan A (see design.md G8)

- [ ] 10.1 Add a tool-call interceptor hook in `internal/agent/loop.go` that runs BEFORE tool dispatch for `ExternalAgent` tool_use blocks; check whether `agent_name` is in the current session's enum
- [ ] 10.2 Implement per-session dedup state machine (`pending` / `unavailable`) keyed by `agent_name`. The map is owned by the session manager so it survives across turns within a session but resets on new session creation
- [ ] 10.3 When agent_name NOT in enum AND binary resolves on PATH AND `implicit_trigger_enabled` AND no dedup entry for this name yet: (a) add dedup entry as `pending`; (b) call the same internal entrypoint as `agent_setup.Run` synchronously (blocks the tool call for 5-15s); (c) on successful registration of a pending approval, return a tool_result for the ORIGINAL ExternalAgent tool_use whose text names the approval_id and repeats the model's full call parameters (including `prompt`, `inputs`, `timeout_sec`, `resume_session_id`) VERBATIM so the model can re-emit them. NO synthesized tool_use is added to the conversation history. The tool_result text MUST begin with the marker substring `Adapter for '<name>' was not registered` so a compaction summarizer can detect and preserve the parameters
- [ ] 10.4 Implement the approval-handler hook that, on `approve`, clears the dedup entry for that agent_name (so the model's next retry succeeds). Approval-published adapter is injected into the current session's registry (section 8.2). The injected adapter does NOT re-prompt for first-invocation approval IN THE SAME SESSION (cross-cutting modification of Change 1 D17). Other sessions still gate normally
- [ ] 10.5 On reject or expire: dedup entry transitions to `unavailable`; subsequent retries return `"Adapter setup for '<name>' was rejected/expired this session. Use a different approach."`; do NOT re-trigger generation
- [ ] 10.6 When dedup shows `pending` and the model retries before approval: return `"Adapter setup for '<name>' is pending user approval (approval_id <id>). Wait for the user or use a different approach."` — do NOT start a second generation
- [ ] 10.7 Honor the `implicit_trigger_enabled` config; when disabled, fall through to Change 1's standard unknown-agent rejection
- [ ] 10.8 Implement the `DedupClearer` interface (declared in 7.5) that the approval manager calls on approve
- [ ] 10.9 Tests: (a) unknown name with installed binary triggers Plan A flow (no synth tool_use, SSE event emitted, tool_result text preserves call parameters); (b) unknown name without binary returns standard error; (c) dedup blocks duplicate generation while `pending`; (d) approve clears dedup and registry-injects (model's retry succeeds); (e) reject keeps dedup as `unavailable` (model's retry returns reject message); (f) expire same; (g) opt-out via config; (h) resume_session_id preserved in the tool_result text and honored on retry when adapter declares `session.supports_resume: true`

## 11. Drift detection at startup

- [ ] 11.1 Create `internal/extagent/regen/drift.go` exposing `Check(registry) []DriftEntry`; for each adapter with `provenance.source: llm_generated`, run `<binary> --version` with a 2 second timeout and compare to `provenance.tool_version`. **Skip the comparison when `provenance.tool_version` is empty** (G3 allows empty when probe failed at generation time — comparison would always falsely report drift)
- [ ] 11.2 Wire into server startup after the registry loads; emit a structured log line per drift entry
- [ ] 11.3 Surface drift entries via `GET /diagnostics` under `external_agents.drift`
- [ ] 11.4 Tests: matching version → no drift; mismatched version → entry created; empty stored version → skipped (no entry); binary missing entirely → log only (no entry, since adapter is already marked binary-missing by Change 1's loader)

## 12. CLI subcommand

- [ ] 12.1 Add `workhorse-agent setup-agent <binary> [--description-hint <text>] [--regenerate] [--model <id>]` to `cmd/workhorse-agent/`
- [ ] 12.2 Detect TTY on stdin; if TTY, prompt `[a]pprove / [r]eject / [e]dit` interactively
- [ ] 12.3 If non-TTY, print `approval_id=<id>` and exit zero; user runs `workhorse-agent approve <approval_id>` to complete
- [ ] 12.4 Implement `workhorse-agent approve <approval_id> [--decision approve|reject|edit] [--file <edited.yaml>]` companion subcommand. Symmetric TTY detection: when stdin is a TTY AND no `--decision` provided, fetch approval payload, render it (draft + smoke + diff if present), prompt `[a]pprove / [r]eject / [e]dit` interactively. When stdin is non-TTY OR `--decision` provided, act non-interactively (error if `--decision edit` without `--file`)
- [ ] 12.5 Tests: setup-agent TTY happy path; setup-agent non-TTY id-print path; setup-agent preflight-failure exit non-zero; approve TTY interactive path (with mocked stdin "a"); approve non-TTY without `--decision` exits non-zero with instructive message; approve non-TTY with `--decision approve` succeeds; approve `--decision edit --file <yaml>` re-validates and publishes; approve `--decision edit` without `--file` errors

## 13. Documentation

- [ ] 13.1 Add a "LLM-generated adapters" section to `CLAUDE.md` covering: how generation is triggered (explicit + implicit), the approval flow, the `.drafts/` and `.genmeta` files, drift detection, and the security model
- [ ] 13.2 Update the "External agents" section from `add-external-agent-tool` to mention this change as the recommended way to add new adapters once `agent_setup` ships
- [ ] 13.3 Document the new SSE events and approval endpoint in the API protocol spec

## 14. Integration and end-to-end

- [ ] 14.1 End-to-end test on a fake binary: install a tiny test binary in a temp dir with predictable `--help` output; run `agent_setup`; assert subagent runs; assert draft validates and smokes; assert approval payload is well-formed; approve via HTTP; assert next session can invoke the adapter
- [ ] 14.2 End-to-end test for implicit trigger: model emits `ExternalAgent {agent_name: fake-bin}`; observe synthesised `agent_setup`; complete approval; observe model can now successfully invoke the adapter
- [ ] 14.3 End-to-end test for the cli_tool refusal path: install a fake binary whose `--help` shows no prompt-passing convention; run `agent_setup`; observe refusal and no draft written
- [ ] 14.4 Soak test: 50 consecutive `agent_setup` calls against different fake binaries to detect leaks (pending-approval goroutine cleanup, draft directory cleanliness)
