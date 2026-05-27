## Context

`workhorse-agent` is a local single-user, multi-session AI agent server (Go 1.22+, `modernc.org/sqlite`, single binary). The current tool surface (commit `dda4981` HEAD) is:

| Tool         | Where                                              | Purpose                                          |
|--------------|----------------------------------------------------|--------------------------------------------------|
| `Bash`       | `internal/tools/bash/`                             | One-shot shell command with envfilter + danger guard |
| `Read/Write/Edit/Glob/Grep` | `internal/tools/builtin/`                | Workdir-scoped filesystem ops via `pathguard`     |
| `Dispatch`   | `internal/tools/dispatch/`                         | Spawn a child Session with a different `agent_type`; streams or blocks |
| `LoadSkill`  | `internal/skills/loadtool.go`                      | Pull a skill's full body into the conversation    |
| MCP-attached | `internal/mcp/`                                    | Tools surfaced from configured MCP servers       |

What's missing is a way to treat *another binary on the box* — `claude`, `codex`, `aider`, `pandoc`, `ffmpeg` — as something the agent can hand work off to with structure: known prompt-passing convention, known output format, known cancel/timeout semantics, known capability set. Today the model must invent that convention per turn via `Bash`, with no streaming or session reuse.

Hermes solves the equivalent problem via its `acp_adapter` (Agent Communication Protocol) and `terminal` toolset, but that ecosystem assumes a Python plugin per tool. We want the same outcome with a YAML manifest per tool and a single Go-side driver.

The existing `internal/tools/dispatch/Host` pattern (commit `internal/tools/dispatch/dispatch.go:24-30`) is the closest analog: a stateless tool whose runtime dependencies (manager, loader, depth cap) are bundled into an injected `Host`. We follow the same pattern.

`internal/skills/loader.go` is the closest analog for the registry: filesystem-scan + hot-reload on session start, no fsnotify. We mirror it.

`internal/tools/bash/envfilter.go` is the canonical env stripper. Its public API is `bash.Filter([]string) (kept, dropped []string)`, `bash.FilterMap(map[string]string) (kept, dropped)`, and `bash.LogDropped(logger, dropped)`. We reuse those verbatim — the sub-agent inherits no broader privilege than a bash command would.

`internal/prompt/builtins.go` exposes `BuildSystemPrompt(base string) string` which renders `{{.BasePrompt}}\n\n{{cancellednote}}`. The `add-memory-l1-l2` change (in review) currently prepends the memory block at the agent-loop call site (`internal/agent/loop.go:370-372`) BEFORE calling `BuildSystemPrompt`, rather than via a template slot. We follow the same pattern for the environment block — see D15. Coordinating on a single composition pattern is load-bearing for prompt-cache stability and avoids two divergent conventions in one template.

## Goals / Non-Goals

**Goals:**

- Let the agent invoke a `sub_agent`-class external CLI (e.g. `claude --prompt '...'`) through a typed tool surface, with cancellation, timeout, env isolation, and structured output collection.
- Make `cli_tool`-class binaries (e.g. `pandoc`, `playwright`) usable *without* per-tool adapter code — they appear as a list in the system prompt's `<environment>` block and the model reaches them via the existing `Bash` tool.
- Extensible by *dropping a YAML file*: no Go code change, no central config edit. New file → next session sees it.
- Ship 3 builtin adapters embedded in the binary so the system is useful out of the box.
- Smoke-test adapters before exposing them; cache results to avoid blocking every session start.
- Preserve every current invariant: bind 127.0.0.1, envfilter rules, pathguard, ULID identifiers, no CGO, modernc.org/sqlite only.

**Non-Goals:**

- LLM-driven generation of adapters from `--help` output — that is a separate change (`add-llm-adapter-generator`) layered on top of this one.
- Streaming sub-agent output back to the parent SSE channel — MVP collects the full child stdout/stderr until exit and emits one `tool_result`. Stream-forwarding requires SSE-in-tool-result semantics that don't exist today and would change the `agent-loop` spec.
- Auto-installing missing binaries — out of scope. The user installs `claude` themselves.
- Cross-machine adapter sync, adapter marketplaces, registry signing — out of scope.
- Per-session adapter overrides (e.g. "use claude only in this conversation") — out of scope; the adapter set is profile-wide.
- Hot-reload of adapters mid-session via fsnotify — explicitly rejected (D7). Re-scan happens at session creation, identical to skills.

## Decisions

### D0: Single capability `external-agents` + separate `environment-detection`, not one mega-capability

**Choice**: Two capabilities. `external-agents` covers everything adapter-related: schema, validation, registry, hot-reload, `ExternalAgent` tool, sub-process driver, smoke test, builtin adapters. `environment-detection` covers PATH scanning, the `<environment>` system-prompt block format, and the `{{.Environment}}` template slot.

**Why**: PATH scanning is genuinely independent. The same `<environment>` block will eventually be useful for non-adapter features (e.g. detecting that `git` is installed before suggesting a git workflow). Bundling it inside `external-agents` would couple unrelated requirements and make the spec hard to evolve.

**Alternatives considered**:
- *One capability `external-agents` that includes PATH scan*. Rejected — couples the model's general environment-awareness to the adapter system. Future "detect that there's a Postgres on localhost" work would have nowhere clean to live.
- *Three capabilities (schema / registry / tool)*. Rejected — they share too much surface; splitting them three ways forces cross-references in scenarios.

### D1: Two adapter classes, `sub_agent` and `cli_tool`, not one unified type

**Choice**: Every adapter MUST declare `class: sub_agent | cli_tool`. The two are routed differently:

- `sub_agent` adapters are loaded into the registry, presented to the model only via the `ExternalAgent` tool's enum, and invoked through the sub-process driver with full lifecycle management.
- `cli_tool` adapters (if a YAML exists for them at all) and PATH-scan-discovered binaries are surfaced *only* in the `<environment>` block — the model uses `Bash` to call them. They are NOT registered as tools; the `ExternalAgent` tool refuses them.

**Why**: A `pandoc` adapter does not need streaming output parsing, cancellation grace periods, or smoke tests. Forcing it through the sub-agent driver would be wasteful and would multiply the number of "tools" the model sees (each one its own tool name). Conversely, `claude-code` needs all of that machinery — calling it via `Bash` loses streaming output and clean cancellation.

**Alternatives considered**:
- *One class, optional fields*. Rejected — the `ExternalAgent` tool's behavior changes fundamentally based on whether streaming/resume is in play; encoding that as "field present or not" hides the contract.
- *Three classes (`sub_agent`, `cli_tool`, `service`)*. Rejected — YAGNI; we can add a third class later when the need is concrete.

### D2: Adapter location is `~/.workhorse-agent/external-agents/<name>.yaml`, NOT under `agents/` or `skills/`

**Choice**: New directory under the profile root, parallel to existing `agents/` and `skills/`. One YAML per adapter; filename stem MUST equal `name:` field.

**Why**: 
- `agents/` holds `agent_type` definitions consumed by `Dispatch` (child sessions running our own loop). Conceptually different — an `agent_type` is "a way I drive my own loop", an external agent is "a binary I hand off to".
- `skills/` holds prompt fragments loaded on demand. Even more different.
- Co-locating adapters with one of the above would muddle directory semantics and complicate the loader.

**Alternatives considered**:
- *`~/.workhorse-agent/agents/external/`* (sub-directory). Rejected — the loader for `agents/` would either ignore the subdir (confusing) or have to special-case it (couples the two loaders).
- *Single file `~/.workhorse-agent/external-agents.yaml`*. Rejected — editing one file is harder than dropping a new file; conflicts on adapter add/remove are worse; the LLM generator (Change 2) writes one file per generated adapter.

### D3: Filename stem is the adapter identity, NOT the `name:` field — but the two MUST match

**Choice**: The loader uses the filename stem to detect duplicate adapters and to allow user overrides of builtin adapters (drop `claude-code.yaml` on disk to override the embedded one). At load time, the loader asserts `filename_stem == name:`; mismatch is a hard error.

**Why**: Filenames are easier to grep, list, and ENT-style override. Requiring equality prevents the surprising "I renamed the file but it still loads as the old name" trap.

### D4: Complete adapter schema (see appendix), validated by JSON Schema co-shipped with the binary

**Choice**: Full schema as drafted in the explore session. Fields are organized by concern: identity, invocation, session, output, control, capabilities, security, smoke_test, description, usage_hints, provenance. JSON Schema lives at `internal/extagent/schema/adapter.schema.json` and is `//go:embed`-ed.

| Block         | Required? | Notes                                                        |
|---------------|-----------|--------------------------------------------------------------|
| `name`        | yes       | kebab-case, MUST match filename stem                          |
| `binary`      | yes       | resolved via `exec.LookPath`; absolute path also accepted     |
| `class`       | yes       | `sub_agent` or `cli_tool`                                     |
| `invocation`  | yes for sub_agent | `prompt_via`, `prompt_arg`, `extra_args`, `cwd`, `env_passthrough`, `env_override` |
| `session`     | no        | `supports_resume`, `resume_flag`, `session_id_arg`             |
| `output`      | yes for sub_agent | `format` ∈ {`text`, `jsonl`, `streaming-json`, `sse`}; `stderr` ∈ {`separate`, `merge`, `ignore`}; optional `parser` (JSONPath hints) |
| `control`     | yes for sub_agent | `cancel_signal`, `cancel_grace_sec`, `default_timeout_sec`, `max_timeout_sec` |
| `security`    | yes       | `network` ∈ {`allowed`,`restricted`,`none`}, `filesystem` ∈ {`workdir`,`full`}, `trusted` (bool) |
| `smoke_test`  | yes for sub_agent | `prompt`, `expected_substring`, `timeout_sec`              |
| `description` | yes       | one-paragraph summary: WHAT this binary is. Human + LLM readable. |
| `usage_hints` | no        | WHEN the orchestrator should pick this over alternatives. Distinct from `description` — `description` answers "what" while `usage_hints` answers "why pick this now". Injected into the `ExternalAgent` tool's description so the model can choose `agent_name` informedly. |
| `provenance`  | yes       | `source` ∈ {`builtin`,`user_yaml`,`llm_generated`}; LLM-generated entries also carry `generated_by`, `generated_at`, `tool_version`, `reviewed_by` |

**Note on removed field**: an earlier schema draft included `capabilities: [string]` ("hints for the orchestrator's choice"). Removed — no consumer in MVP, and adding fields later is cheap. If future orchestrator-side selector logic needs structured capability tags, add the field at that time.

**Why complete (not minimal)**:
- `session` distinguishes `claude-code` (resumable) from `codex` (not) — important for orchestration semantics.
- `output.format` is the difference between rich integration and `tail -f`.
- `usage_hints` injects into the tool's description so the model picks the right `agent_name` (parallels how Dispatch tool descriptions list `agent_type`s).
- `provenance` is load-bearing for Change 2 (the LLM generator must mark its output).

**Why no dangerous-command scan of `extra_args` / `env_override` strings in this change**: adapter YAML in Change 1 originates from either embedded builtins (developer-reviewed) or hand-written user files (user-reviewed). Neither is untrusted input. The LLM-generated adapter case is introduced by `add-llm-adapter-generator`; the dangerous-pattern scan of generated argument and env strings is a requirement on THAT change, not this one. (An earlier draft of this design incorrectly claimed the scan happened here — corrected.)

**Alternative**: Minimal schema (~10 fields) with everything else inferred. Rejected — inference requires per-binary heuristics that are themselves the thing we're trying to avoid hard-coding.

### D5a: `ExternalAgent.CanRunInParallel()` returns `true`, mirroring `Dispatch`

**Choice**: The `Tool.CanRunInParallel()` interface method returns `true`. Each `ExternalAgent` invocation spawns an independent sub-process with its own stdout/stderr pipes, memory buffer, process group, and context — there is no shared mutable state between concurrent calls. This matches `Dispatch` (`internal/tools/dispatch/dispatch.go:94`).

**Implication**: in a single LLM turn the orchestrator may dispatch multiple `ExternalAgent` calls concurrently — e.g. 3 parallel `claude` invocations against different prompts. Each is a separately-billed API call against the underlying provider. This is the same risk profile `Dispatch` already carries; we accept it for the same reasons (parallelism is a meaningful UX win for fan-out tasks; the model is the appropriate gatekeeper).

### D5: Single `ExternalAgent` tool with `agent_name` enum, NOT one tool per adapter

**Choice**: The tool surface is one tool named `ExternalAgent`. Its input schema's `agent_name` field is dynamically populated with the names of all loaded `sub_agent`-class adapters. Its description lists each adapter and its `usage_hints`.

```jsonc
{
  "name": "ExternalAgent",
  "input_schema": {
    "type": "object",
    "required": ["agent_name", "prompt"],
    "properties": {
      "agent_name": { "type": "string", "enum": ["claude-code","codex","aider"] },
      "prompt": { "type": "string" },
      "inputs": { "type": "object", "additionalProperties": true },
      "timeout_sec": { "type": "integer", "minimum": 1 },
      "resume_session_id": { "type": "string" }
    }
  }
}
```

**Why**: Mirrors how `Dispatch` exposes `agent_type` as an enum. Avoids polluting the tool namespace (10 adapters → 1 tool, not 10). The model's prompt-cache key is unaffected by adapter add/remove if we re-render the enum stably-sorted.

**Alternative**: One tool per adapter (`ClaudeCode`, `Codex`, …). Rejected — N+1 tool registrations per added adapter; the model's tool-choice surface grows linearly; tool description becomes redundant.

### D6: `ExternalAgent` is exposed only when ≥1 `sub_agent` adapter loads successfully

**Choice**: If the registry resolves zero healthy `sub_agent` adapters at session start, the `ExternalAgent` tool is not registered for that session. Loading 1+ enables it.

**Why**: A tool with an empty enum is a paper cut for the model and wastes prompt tokens. The system-prompt's `<environment>` block still lists `cli_tool`s independently.

### D7: Hot-reload happens at session start, NOT via fsnotify

**Choice**: The registry rescans `~/.workhorse-agent/external-agents/` exactly once per session-creation event. Edits during a live session do not propagate until a new session begins. This mirrors how `internal/skills/loader.go` behaves today.

**Why**:
- Consistency with the existing skill-loader is more important than freshness — operators already know "to pick up new skills, start a new session".
- fsnotify on Linux has well-known edge cases (editor swap files, fsync ordering) that we'd have to special-case.
- Mid-session adapter changes can break a running `ExternalAgent` invocation in a half-defined way (which version of the adapter governs the running child?). Rescanning at session start eliminates the ambiguity by construction.

**Alternative**: fsnotify with debounced reload. Rejected on simplicity grounds; revisit if user feedback demands it.

### D8: Builtin adapters are `//go:embed`-ed and seed the registry only when no on-disk override exists

**Choice**: `internal/extagent/builtins/` ships `claude-code.yaml`, `codex.yaml`, `aider.yaml` embedded into the binary. On registry construction, the loader merges in this order:

1. Embedded builtins (with `provenance.source: builtin`).
2. On-disk adapters under `~/.workhorse-agent/external-agents/*.yaml`, overriding any builtin with the same `name`.

**Why**: User can override a builtin by dropping a same-named file. No flag, no escape hatch needed. Builtins guarantee the system is useful immediately after install.

**Alternative**: Builtins are *examples only* and the user must copy them to enable. Rejected — friction without benefit; the builtin set is small and curated.

### D9: `ExternalAgent` reuses `internal/tools/bash/envfilter.go` — filter is applied LAST as a safety net

**Choice**: The sub-process driver computes the child env in the same shape `internal/tools/bash/bash.go:91-100` does — merge all sources first, then apply `bash.Filter()` as a non-bypassable safety net. Concretely:

1. Start from the parent process env as a `[]string` (`os.Environ()`).
2. Project to a map keyed by name; KEEP ONLY the names listed in `invocation.env_passthrough` (allowlist applied against parent env).
3. Layer in `invocation.env_override` (verbatim key/value pairs from YAML).
4. Always inject `PATH` and `HOME` if not already present (without these many binaries fail to start).
5. Re-serialize the map to `[]string` and call `bash.Filter(envSlice)` → `(kept, dropped)`. **This step is unconditional and applies to the merged env including `env_override` values**, ensuring no source — not even the YAML-declared override — can re-introduce a denied variable like `LD_PRELOAD`, `LD_LIBRARY_PATH`, `DYLD_*`, `PYTHONPATH`, or a `NODE_OPTIONS` value containing `--require`/`--import`/`--inspect`/etc.
6. Call `bash.LogDropped(logger, dropped)` for the audit trail (identical to Bash).
7. Assign `kept` to `exec.Cmd.Env`.

**Why filter LAST**: An earlier draft of this decision applied the filter BEFORE layering `env_override`. That ordering allowed a malicious or buggy `env_override: {LD_PRELOAD: /tmp/evil.so}` to bypass the filter — every sub-agent invocation would inherit the dangerous variable. The post-merge filter eliminates the bypass by treating filter rules as absolute regardless of source. This matches the Bash tool's discipline exactly (`bash.go:97-98`: merge first, filter once).

**Why**: Sub-agents inherit no broader privilege than a Bash command would. The envfilter rules are the project's "single chokepoint" per `CLAUDE.md`; any tightening of those rules automatically tightens both the Bash and ExternalAgent surfaces. Reusing `LogDropped` means an operator who already greps for `env.dropped` log lines (from Bash) sees them for ExternalAgent too — no separate observability layer needed.

**Alternative considered**: at adapter validation time, reject any `env_override` whose keys appear in the envfilter deny set. Rejected — couples validation to the deny list; if envfilter adds a new denied name (say `LD_NEW_DANGER`), every previously-loaded adapter would need re-validation. Post-merge filter handles this automatically.

**Alternative considered**: per-adapter custom envfilter rules. Rejected — multiplies the security surface and contradicts the "single chokepoint" pattern.

### D10: Cancellation: `SIGINT` (configurable) → grace period → `SIGKILL`

**Choice**: When the parent agent cancels the `ExternalAgent` tool call (user cancel, session timeout, panic recover), the driver:

1. Sends `control.cancel_signal` (default `SIGINT`) to the entire process group of the child.
2. Waits up to `control.cancel_grace_sec` (default 5s) for the child to exit.
3. If still alive, sends `SIGKILL` to the process group.
4. Returns a `tool_result` with the existing `[CANCELLED]` marker (`internal/prompt/builtins.go:8`) so the model knows.

**Why**: Mirrors the Bash tool's existing teardown (`internal/tools/bash/`). Process-group kill is the only reliable way to stop a child that has forked further. `[CANCELLED]` marker integrates with the existing CancelledNote (`internal/prompt/builtins.go:14-17`).

### D11: Output collection — single cap at the existing `ToolResultMaxBytes`, no double truncation

**Choice**: The driver reads stdout (and stderr per `output.stderr`) from the child concurrently, counting bytes as they accumulate in a memory buffer. The cap is the existing global `tool_result_max_bytes` config key (`internal/config/config.go:76`, default `1 << 20` = 1 MiB) — the same cap the orchestrator's `tools.TruncateOutput` already enforces on every tool's output (`orchestrator.go:226`). No separate `external_agents.driver.output_cap_bytes` config is introduced; bumping the global cap raises ExternalAgent's effective output budget along with every other tool's.

On reaching the cap:

1. Stop reading further bytes (drain into `io.Discard` to prevent the child from blocking on a full pipe).
2. Send `control.cancel_signal` to the child process group, then SIGKILL after `cancel_grace_sec` — the same teardown as user cancel (D10).
3. Mark the result `Truncated: true` with `TruncatedAtBytes: <cap>`.
4. Append a single `[... truncated N bytes]` marker to the rendered tool_result text.

Because the driver's cap equals the orchestrator's cap, the orchestrator's `TruncateOutput` call is a no-op on ExternalAgent output — the operator sees ONE truncation marker, not two. (If a future operator raises `tool_result_max_bytes` without restarting, an in-flight ExternalAgent call uses whatever value was loaded at session start; this is consistent with how the orchestrator's cap reads the same field.)

Stream-forwarding into the parent SSE channel would require new event types and changes to the `agent-loop` spec; defer to a follow-up change. The single cap protects the SQLite event store and the LLM's context window.

**Why no separate config**: an earlier draft of this decision introduced `external_agents.driver.output_cap_bytes` (default 4 MiB) alongside `ToolResultMaxBytes` (default 1 MiB). The driver capped at 4 MiB, then the orchestrator truncated again to 1 MiB. Operators saw two truncation markers and a smaller-than-advertised cap. Removing the separate config is the cleanest fix; operators who genuinely want a 4 MiB budget bump `tool_result_max_bytes` and get it everywhere (a uniform lever beats per-tool knobs).

**Cap-then-kill rationale**: a sub-agent that has produced the full cap of bytes and is still running is almost certainly stuck in a loop or pathologically verbose. Letting it continue costs API tokens (claude-code, codex, etc. are billed) for output that will be thrown away. Configurable via `external_agents.driver.kill_on_output_cap` (default `true`) for the rare adapter where it's not appropriate.

**Raw-output debug dump**: when either `Truncated: true` OR `ExitCode != 0`, the driver writes the captured raw stdout+stderr to a unique temp file under `os.TempDir()/workhorse-extagent-<session_id>-<call_id>.log` with mode `0600`. The tool_result text appends a footer `[raw output dump: <path>]` so an operator can inspect what the parser saw. Happy-path calls (no truncate, exit 0) skip the dump entirely — zero extra I/O. Files are NOT cleaned up by the driver (the session may need them); operator's standard `/tmp` rotation handles them.

**Alternative**: Stream chunks as multiple `tool_result` blocks. Rejected — current `agent-loop` spec doesn't permit multiple `tool_result`s for a single `tool_use` id.

### D12: Output parsing per declared `output.format` + restricted JSONPath subset

**Choice**:

- `text` → return raw stdout verbatim (post-strip ANSI).
- `jsonl` → parse each line as JSON; if `parser.assistant_text` (JSONPath) extracts a value, concatenate; else return pretty-printed array.
- `streaming-json` → same as `jsonl` but tolerate a partial trailing line without erroring.
- `sse` → parse `data: ...` events; same JSONPath extraction against the JSON payload of each event.

`stderr` per `output.stderr` ∈ {`separate` (return as `<stderr>...</stderr>` suffix), `merge` (interleave with stdout in receipt order), `ignore` (drop)}.

**JSONPath subset, hand-rolled**: rather than pull a third-party library whose feature set may grow over time, we ship a ~80-line custom parser supporting EXACTLY this grammar:

```
path     := "$" segment*
segment  := "." identifier | "[" (integer | "*") "]"
identifier := [A-Za-z_][A-Za-z0-9_]*
integer  := -?[0-9]+
```

Valid: `$.delta.text`, `$.output[0].content`, `$.choices[*].message.content`, `$[0].text`.

Explicitly NOT supported: filter expressions (`?(...)`), recursive descent (`..`), bracket-string keys (`["foo"]`), slicing (`[1:3]`). An adapter declaring an out-of-grammar path is rejected at schema-validation time (the schema's pattern check covers it) — implementers do not need to handle malformed paths at runtime.

**Error behavior at extraction time**:

- Path evaluates to `null` or undefined on a given chunk → that chunk contributes the empty string to the concatenation; debug log line.
- Path evaluates to a non-string (e.g. object, array, number) → coerce via `fmt.Sprintf("%v", v)`; debug log line warning of unexpected type.
- JSON line fails to parse (for `jsonl` / `streaming-json` / `sse`) → debug log line and continue with next line.

These behaviors mean a single malformed chunk never aborts the tool call. The smoke test (D13) catches systematic parser misconfiguration.

**Why**: Covers Claude Code (`streaming-json`), Codex (`text`), and a future MCP-bridged variant (`sse`). Restricted grammar prevents LLM-generated adapters from emitting filter expressions whose semantics are subtle and library-specific. No external dependency, predictable behavior.

**Risks** → Mitigation:
- LLM-generated adapters might produce a path that compiles but extracts the wrong field. Mitigated by smoke test (D13) running the actual binary; if `expected_substring` is not in the extracted text, smoke fails and the adapter is not usable.
- Structured-format adapters whose smoke output is "valid expected_substring" but produced by a coincidentally lenient CLI (CLI silently ignored a hallucinated flag) are caught by the supplemental check from Change 2 G14 (structured-format adapters must also parse as the declared format).

### D13: Smoke test runs at first load and again after adapter-file mtime change; result cached in sibling `.smoke` file

**Choice**: On first load of a `sub_agent` adapter (and on any subsequent load where the adapter file's mtime is newer than the cached `.smoke` file's mtime), the registry runs the adapter with `smoke_test.prompt` in a sandbox:

- Empty `cwd` under `os.TempDir()` (auto-removed).
- env computed per D9, then *additionally* stripped to a minimum allowlist (`PATH`, `HOME`, `USER`, `LANG`) for the smoke test specifically.
- Hard wall-clock timeout = `smoke_test.timeout_sec` (default 60s).
- Output parsed per `output.format`; result MUST contain `smoke_test.expected_substring`.

On success: write `<name>.smoke` JSON `{passed: true, ran_at, output_hash}`. On failure: write `{passed: false, ran_at, error}` and emit a structured log; the adapter is loaded but `ExternalAgent` rejects invocations against it with a clear error.

**Cache TTL**: configurable via `external_agents.smoke_test.cache_ttl` (default `168h` / 7 days). Beyond TTL, re-run on next session start even without mtime change.

**Why**:
- Catches "claude binary moved" or "the user upgraded to a breaking version" without requiring per-session re-runs.
- Sandbox prevents a malformed adapter from doing damage during the test itself.
- Sibling `.smoke` file is greppable, debuggable, and removable (delete file → force re-run).

**Alternatives considered**:
- *Run smoke test on every session start*. Rejected — 3 adapters × ~5s each = 15s session-start latency; unacceptable.
- *Don't smoke-test at all, fail at invocation*. Rejected — the user's first realization that an adapter is broken should not be "the agent already started doing my task". Smoke at load is the prophylactic.

### D14: `cli_tool` discovery via curated PATH allowlist, NOT wholesale `/usr/bin` scan

**Choice**: A builtin allowlist of "binaries likely useful to a task agent" is shipped in `internal/extagent/pathscan/builtins.go`:

```
git, gh, jq, yq, curl, wget, rg, fd, fzf,
pandoc, asciidoctor, marp, libreoffice, soffice,
ffmpeg, convert, magick, identify, yt-dlp,
playwright, chromium, chrome, firefox,
python3, node, npm, pnpm, yarn, deno, bun, go, cargo, rustc,
docker, podman, kubectl, terraform, ansible
```

(Note: an earlier draft listed `imagemagick` — that is the package name, not a binary. The actual binaries shipped by ImageMagick are `convert`, `magick`, `identify`, `mogrify`. Use those instead.)

For each entry: `exec.LookPath()` to confirm presence, optional `<bin> --version` (with 2s timeout) for version string. Detected entries land in the `<environment>` block as one line each (`name @ path (version)`).

User extensions via `external_agents.pathscan.extra: [foo, bar]`; user exclusions via `external_agents.pathscan.disabled: [docker]`.

**Why**: Scanning `/usr/bin` wholesale (200-2000 entries) would bloat the system prompt and surface irrelevant binaries (`mkfs`, `dd`, `chsh`). A curated list keeps the block small and signal-dense. User extensibility covers the long tail.

**Alternative**: No PATH scan; the model finds tools via `which` ad hoc. Rejected — costs a tool round-trip per question; the model often won't think to check; misses obvious wins like "you have `pandoc` installed".

### D15: `<environment>` block — Go-level prepending, parallel to memory

**Choice**: `internal/prompt.EnvironmentBlock(input EnvironmentInput) string` renders the block (or empty string if no detected tools and no sub_agents). The agent loop call site (`internal/agent/loop.go` near the existing memory prepend) composes:

```go
base := l.SystemPromptBase
if mem := memory.Block(l.Session.MemorySnapshot); mem != "" {
    base = mem + "\n\n" + base
}
if envb := prompt.EnvironmentBlock(l.Session.EnvSnapshot); envb != "" {
    base = envb + "\n\n" + base
}
req.System = prompt.BuildSystemPrompt(base)
```

Order: `environment → memory → base prompt`. Rationale for this exact order: environment is the most stable across sessions on the same machine (changes on tool install/upgrade only), memory is next-most-stable (changes on user `memory_write`), base prompt is most variable (per-session/per-agent-type). Stable prefixes maximise prompt-cache hit rate.

`BuildSystemPrompt`'s signature does NOT change — it still takes a single `base string`. No template slot is added (this is a deliberate revision from an earlier draft of this design which proposed a `{{.Environment}}` slot; that approach was rejected to keep the composition pattern consistent with `add-memory-l1-l2`'s Go-level prepending).

Rendered block format:

```
<environment>
os: linux (6.6.87.2-microsoft-standard-WSL2)
shell: bash
cwd: /home/wallfacers/project/workhorse-agent

cli_tools (invoke via Bash):
- git @ /usr/bin/git (2.43.0)
- pandoc @ /usr/bin/pandoc (3.1.11)
- ffmpeg @ /usr/bin/ffmpeg (6.0)
- playwright @ /usr/local/bin/playwright (1.45.0)
...

sub_agents (invoke via ExternalAgent tool):
- claude-code: Anthropic's official coding agent (resumable)
- codex: OpenAI Codex review CLI
</environment>
```

**Why sectioned**: model can scan. `os` / `shell` / `cwd` echo what `Bash` already exposes but consolidates per turn so the model doesn't need a sniffing call. Section headers are stable, alphabetized within section for cache stability.

**No size cap on the block**: an earlier review proposal suggested truncating at 40+ entries to avoid context bloat. Rejected — truncation hides tools from the model, which then forgets they exist. The block stays under ~1500-2000 tokens even on a fully-loaded dev box (≈1% of a 200k context); prompt cache amortises the cost across turns. We DO log a warning at 80+ detected entries so an operator notices unusual growth: `pathscan.large name_count=N` at warn level.

**Cache discipline**: The `<environment>` block is part of the system prompt prefix that Anthropic prompt-cache keys on. We snapshot it at session start (same pattern as memory snapshot); discovered changes mid-session do NOT mutate the snapshot. Re-detection at the next session start.

### D16: Failed adapter loads do NOT block server startup

**Choice**: Adapter YAML parse failures, schema validation failures, and unresolvable `binary:` paths cause the affected adapter to be skipped (with a structured log line) but do not prevent server start or other adapters from loading. The skipped adapter is absent from the registry; the `ExternalAgent` tool enum reflects only what loaded.

**Why**: One bad YAML in `~/.workhorse-agent/external-agents/` should not turn the box into a brick. The user's other work continues.

**Alternative**: Fail-fast on any adapter error. Rejected — too brittle; the LLM generator (Change 2) is going to occasionally produce malformed YAML before approval, and we want the server to keep running.

### D17: First-invocation approval for `security.trusted: false` adapters

**Choice**: Every adapter loaded with `provenance.source != builtin` defaults to `security.trusted: false`. The first time `ExternalAgent` is asked to invoke such an adapter in a session, the call goes through the existing `permission` package (`internal/permission/`) requiring a permission decision. Once approved, subsequent invocations in the same session do not re-prompt. Approval is NOT persisted across sessions (every fresh session re-prompts on first use).

**Why**:
- Mirrors the existing Bash dangerous-command permission flow.
- Catches the LLM-generator case: an adapter that just got written by an LLM should not silently start running.
- Per-session approval (not durable) keeps the user in control without nagging mid-session.

**Alternative**: Persistent approval via a `~/.workhorse-agent/external-agents/<name>.approved` marker. Rejected for MVP — let's see if per-session is too noisy first. Trivial to add later.

### D18: `ExternalAgent` tool's `resume_session_id` parameter is opt-in and adapter-gated

**Choice**: The `ExternalAgent` input schema includes `resume_session_id?: string`. The tool only honours it when the target adapter has `session.supports_resume: true`. If the adapter does not support resume and a session id is supplied, the tool returns an error before invoking.

**Why**: Surfaces resume semantics at the tool level so the model can attempt it; gates by adapter capability so the model can't fake-resume against `codex`.

### D19: No persistence of external-agent invocations beyond the existing event log

**Choice**: Each `ExternalAgent` call is one `tool_use` / `tool_result` pair in the existing `messages` table, identical to any other tool. The sub-process's intermediate output is NOT stored separately; the final collected output (post-parse) goes into the `tool_result` text field, subject to the existing 4 MiB cap. Note that the raw-output debug dump described in D11 (only on truncate/non-zero exit) is a tempfile, NOT a SQLite row; it is meant for operator debugging and is not part of the durable event log.

**Why**: Preserves the existing event-log invariants (`events.idx` monotonicity, message immutability). The user can grep historical sub-agent output via the FTS index that `add-memory-l1-l2` is adding. Historical `agent_name` values may reference adapters that have since been removed; this is a known cross-cutting note (the FTS search will find them, the registry won't) and requires no action.

### D20: Orchestrator timeout layering — `ExternalAgent.DefaultTimeout()` returns the max adapter deadline

**Choice**: The orchestrator wraps every tool call in `context.WithTimeout` whose duration follows the priority chain `Tool.DefaultTimeout() → PerToolTimeouts[name] → Orchestrator.DefaultTimeout → 120s default` (see `internal/agent/orchestrator.go:233-247`). If `ExternalAgent.DefaultTimeout()` returned `0` (like `Dispatch` does today), the orchestrator's 120s default would kill the tool's context **before** the adapter's internal `control.default_timeout_sec` (often 600) had a chance to fire — breaking the `[TIMEOUT]` prefix and the orderly cancel sequence.

The tool's `DefaultTimeout()` is therefore computed at session start from the loaded registry:

```go
func (t *Tool) DefaultTimeout() time.Duration {
    max := time.Duration(0)
    for _, a := range t.Host.Registry.Healthy() {
        if a.Class != ClassSubAgent { continue }
        if d := time.Duration(a.Control.MaxTimeoutSec) * time.Second; d > max {
            max = d
        }
    }
    if max == 0 { return 3600 * time.Second }   // empty registry fallback
    return max + 30 * time.Second                // grace for internal teardown
}
```

This guarantees the orchestrator's deadline always sits BEYOND the worst-case adapter deadline by 30s, so the adapter's internal timeout (`[TIMEOUT]` path) always fires first. Per-call clamping (`min(invocation.timeout_sec, control.max_timeout_sec)`) continues to govern the actual budget per the tool's input schema.

**Why**: The orchestrator's `context.WithTimeout` is a backstop, not the primary deadline. The adapter knows its own bounds.

**Alternative**: Add a config key `tools.external_agent.timeout_seconds` requiring the operator to tune. Rejected — adapters declare their own bounds; making the operator re-declare them in config invites drift.

### D21: Permission integration — gating handled inside `Tool.Run`, NOT via `extractResource`

**Choice**: The existing `internal/agent/loop.go` permission flow calls `extractResource(toolName, input)` (`loop.go:642-650`) to derive a resource string from a fixed list of keys `[path, file_path, command, pattern, glob]`, then `Permissions.Check(ctx, sessionID, tool, resource)`. The current permission manager (`internal/permission/manager.go`) has no per-adapter trust concept and no public `PreApprove`/`AllowSession` API (`addSession` at L201 is private).

Rather than extending the permission manager, `ExternalAgent` handles its own per-adapter gating:

- `extractResource(\"ExternalAgent\", input)` returns the empty string (no new key added to the lookup list). With an empty resource, the standard `checkPermissions` flow either matches a wildcard rule or falls through. In the default config there is no rule, so the prompt callback would fire — but for ExternalAgent we route around this by registering the tool with a marker interface that `loop.go`'s permission preprocessor recognizes and skips. Implementation: a new interface `tools.InternalGated` returning `true`; `loop.go`'s `checkPermissions` (`loop.go:600`) checks the interface and bypasses `Permissions.Check` for those tools.
- Inside `ExternalAgent.Tool.Run`, after agent_name validation and before sub-process start, the tool consults a per-session map of previously-approved adapter names. If the adapter's `security.trusted: false` and not yet approved in this session, the tool calls `Host.PermissionGate.Prompt(...)` (a new thin wrapper that does the same prompt-callback dance as Manager.Check, but without registry-rule consultation). On approval, the per-session map records the adapter so subsequent calls in the same session skip the prompt.
- Builtin adapters (`security.trusted: true`) bypass the gate entirely — no prompt, ever.

This mirrors how `Dispatch` handles its own semantics internally (child-session lifecycle) without forcing the permission layer to understand them.

**Why**: Avoids leaking adapter-registry knowledge into the orchestrator's permission flow. Keeps `extractResource` simple. Matches the existing pattern of "tools with internal semantics keep them internal".

**Alternative considered: pre-populate `AllowSession` rules for builtin adapters at session start**. This would require adding a public `AllowSession(sessionID, tool, resource string)` method to `permission.Manager` (since `addSession` is currently private) AND requires `extractResource` to include `"agent_name"` in its lookup list so the resource passed to `Permissions.Check` matches the prefilled rule. It is a reasonable approach and would not require the `InternalGated` interface. We rejected it for two reasons:
1. The `AllowSession` rule applies cleanly to builtins (always trusted) but the untrusted-adapter prompt flow still has to live somewhere — either inside Run (so we're doing both) or via a one-shot prompt that `Permissions.Check` triggers (which conflates "approve this resource" with "approve this adapter").
2. The `InternalGated` interface is a single new symbol; the `AllowSession` path adds a public Manager method plus an extra entry in `extractResource`'s key list, expanding two surfaces.

Both approaches achieve the same observable outcome (builtins skip prompts, non-builtins prompt once per session). Pick whichever the implementer finds cleaner during code review — record the choice as a follow-up in tasks.md.

### D22: Filesystem permission policy — 0600 for files, 0700 for directories, single rationale

**Choice**: Every adapter-related on-disk artifact uses mode `0600`; every directory uses `0700`. This applies to:

- `<profileDir>/external-agents/` (dir)
- `<profileDir>/external-agents/<name>.yaml` (file)
- `<profileDir>/external-agents/<name>.smoke` (file)
- `<profileDir>/external-agents/.drafts/` (dir, introduced by Change 2)
- `<profileDir>/external-agents/<name>.genmeta` (file, Change 2)
- `<profileDir>/cache/pathscan.json` (file)

**Rationale**: adapter YAMLs may reference secret-bearing environment variable names in `env_passthrough` (e.g. `[ANTHROPIC_API_KEY, OPENAI_API_KEY]`); collected `.genmeta` files may contain raw `--help` output that includes example URLs with embedded tokens; smoke caches may include captured CLI output. Even though `workhorse-agent` is a single-user local server (per CLAUDE.md), the conservative file mode protects against accidental file sharing (e.g. an `rsync` to a multi-user host) and matches the project's posture for sensitive files (the memory snapshots in `add-memory-l1-l2` use the same modes).

### D23: Atomic writes for non-YAML adapter sidecar files

**Choice**: Every adapter-sidecar file write (`<name>.smoke`, `<name>.genmeta`, `cache/pathscan.json`) uses the temp-file + rename pattern: write to `<file>.tmp` then `os.Rename(.tmp, <file>)`. Reads tolerate partial files (a JSON parse error logs and treats the cache as missing — re-run). This is the same atomicity discipline `add-memory-l1-l2` uses for memory writes.

**Why**: A server restart in the middle of a `.smoke` write would otherwise leave a corrupted JSON file that the next loader would either fail to parse (treating it as failed smoke) or worse, silently misinterpret. Same applies to all sidecars. Rename is atomic within the same filesystem; sidecars are siblings of the source file, guaranteeing same FS.

## Risks / Trade-offs

- **[Risk] LLM-supplied JSONPath in `output.parser` is wrong** → Mitigation: smoke test (D13) confirms the parser extracts a non-empty string matching `expected_substring`. Failure at smoke time marks the adapter unusable rather than letting bad parsing reach production calls.

- **[Risk] Sub-agent process leaks** (child of cancelled call survives) → Mitigation: process-group kill (D10), monitored by a goroutine with deferred cleanup that runs even on parent-loop panic recovery (CLAUDE.md mandates this).

- **[Risk] User installs a malicious `aider` in `~/bin`** → Mitigation: same as today's Bash tool — we cannot defend against user-installed binaries doing harm. We DO defend against the *adapter file* being malicious for the Change 2 case (LLM-generated YAML) via dangerous-command scan of generated `extra_args` and `env_override` strings, which is a requirement on Change 2. For Change 1 (builtin + hand-written user YAML), we rely on the smoke sandbox, envfilter, and first-invocation approval for non-builtin adapters.

- **[Risk] Builtin adapters drift from the actual `claude` CLI's flags as it evolves** → Mitigation: smoke test catches breakage on the next session start after a CLI upgrade. User can override via on-disk file (D8). Long-term: Change 2 can re-generate.

- **[Risk] PATH scan adds 1-3s to session-start time (50 `LookPath` + `--version` calls)** → Mitigation: parallelize LookPath (cheap, syscall-only); cap `--version` parallelism at 8; cache the result per cold-boot (`<profileDir>/cache/pathscan.json`) with 24h TTL — only re-scan if cache stale or user changes `pathscan.extra`/`pathscan.disabled`.

- **[Risk] Smoke tests cost real API tokens** (claude-code's smoke prompt → Anthropic billing) → Mitigation: smoke prompts are minimal ("Reply with exactly: WORKHORSE_SMOKE_OK"); cached 7d; the user knew they installed a billed binary. Document this in the embedded builtin YAMLs.

- **[Risk] The `<environment>` block changes between sessions and busts prompt cache "more often than needed"** → Mitigation: snapshot per session (D15); the rate-limiting factor is session-creation, not turn. Cache invalidation across sessions is expected.

- **[Risk] Tool name `ExternalAgent` collides with a user's MCP-attached tool of the same name** → Mitigation: name is hardcoded; document as reserved. Trivial to make configurable later.

## Migration Plan

This change is purely additive — no existing behavior changes. Deployment order:

1. Merge change, ship binary.
2. New install: `~/.workhorse-agent/external-agents/` is auto-created at first session; builtin adapters seed the registry; `ExternalAgent` tool is exposed if any of `claude` / `codex` / `aider` are on `PATH`.
3. Existing install: same as #2 on first server start after upgrade. No data migration; no SQLite migration; no config migration. If the user has no `claude`/`codex`/`aider` installed, `ExternalAgent` is simply not exposed.
4. Rollback: revert the binary. Adapter files remain on disk and are ignored by the older binary. No data loss.

## Open Questions

1. Should the builtin allowlist for PATH scan (D14) be embedded data or a sibling YAML the user can edit without rebuilding? **Default for MVP**: embedded data; user extensions via config. Revisit if the list grows past ~50 entries.

2. Should `ExternalAgent` accept a `workdir` override (e.g. let the model run `claude` on a *different* repo than the parent session's workdir)? **Default for MVP**: no — inherit parent workdir verbatim. Defer until a concrete use case demands it.

3. Should we publish a JSON Schema URL for adapters so external editors can validate? **Default for MVP**: ship the schema as a `//go:embed` resource and expose it via a (future) `workhorse-agent adapter validate <file>` CLI subcommand. Public hosting can wait.

4. **API protocol versioning** (deferred to a separate consolidation change): this change introduces no new SSE event types of its own (it reuses the existing tool_use/tool_result/permission_request envelopes). Change 2 introduces three new event types (`adapter_approval_request`, `adapter_approval_resolved`, `adapter_approval_expired`). Together with `add-memory-l1-l2`'s additions, the SSE protocol will accumulate several new event types across three concurrent changes. A follow-up change should consolidate them into a versioned API protocol spec; nothing in this change blocks that consolidation.
