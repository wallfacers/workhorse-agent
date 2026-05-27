## 1. Preflight and configuration

- [ ] 1.1 Add `external_agents.dir` (default `<profileDir>/external-agents/`), `external_agents.smoke_test.cache_ttl` (default `168h`), `external_agents.pathscan.cache_ttl` (default `24h`), `external_agents.pathscan.extra` (default `[]`), `external_agents.pathscan.disabled` (default `[]`), `external_agents.driver.kill_on_output_cap` (default `true`) to the config schema in `internal/config`; wire defaults so all keys are optional. Coordinate with `add-memory-l1-l2` and Change 2 — all three changes add fields to the `Config` struct in `internal/config/config.go`; cherry-pick order matters. NOTE: do NOT add a separate `external_agents.driver.output_cap_bytes` key — the driver reuses the existing `tool_result_max_bytes` config to avoid double truncation (see design.md D11)
- [ ] 1.2 Add config-validation tests covering each new key: type checks, default fallthrough, mutually-exclusive cases (disabled wins over extra)
- [ ] 1.3 Document at the agent-loop call site (`internal/agent/loop.go` near the existing memory prepend at L370-372) the composition order: `environment → memory → base prompt`, joined by `"\n\n"` only between non-empty pieces; reference `add-memory-l1-l2` and this change as load-bearing for prompt-cache stability. Do NOT add a template slot — composition stays at the Go call site (see design.md D15)

## 2. Adapter schema definition

- [ ] 2.1 Author `internal/extagent/schema/adapter.schema.json` covering identity, invocation, session, output, control, security, smoke_test, description, usage_hints, provenance — as drafted in design.md §D4 (the `capabilities` field is intentionally absent — see D4 "Note on removed field"); embed via `//go:embed`
- [ ] 2.2 Include in the JSON Schema a pattern check on `output.parser.*` values enforcing the restricted JSONPath grammar: `^\$(\.[A-Za-z_][A-Za-z0-9_]*|\[(-?[0-9]+|\*)\])*$` — out-of-grammar paths fail validation at load time, eliminating runtime malformed-path handling
- [ ] 2.3 Add Go structs in `internal/extagent/types.go` mirroring the schema (use `yaml:"..."` tags; nested structs for `Invocation`, `Session`, `Output`, `Control`, `Security`, `SmokeTest`, `Provenance`)
- [ ] 2.4 Write `internal/extagent/parser.go` with `Parse(raw []byte) (*Adapter, error)` that runs yaml unmarshal → JSON-Schema validation via `github.com/santhosh-tekuri/jsonschema/v6` → custom invariants (filename-stem-matches-name is checked at the loader level, not here)
- [ ] 2.5 Write parser tests covering every required-field combination, every enum value, malformed YAML, the class-conditional required blocks (sub_agent vs cli_tool), and out-of-grammar JSONPath rejection
- [ ] 2.6 Author golden-file fixtures for the three builtin adapters (`claude-code.yaml`, `codex.yaml`, `aider.yaml`); commit them under `internal/extagent/builtins/`
- [ ] 2.7 Implement the hand-rolled JSONPath subset parser at `internal/extagent/jsonpath/parser.go` (~80 lines): supports `$`, `.identifier`, `[integer]`, `[*]`. Compile-time error on out-of-grammar input; runtime extraction tolerates null/undefined (returns empty + debug log) and non-string values (coerces via `%v`). Tests covering each grammar element and each error mode

## 3. internal/extagent loader and registry

- [ ] 3.1 Create `internal/extagent/loader.go` with `Loader.Load(dir string) (*Snapshot, error)` returning a registry snapshot that merges embedded builtins first then on-disk files (overriding by `name`)
- [ ] 3.2 Implement filename-stem-vs-name check in the loader; mismatch records a structured log line and skips the file without affecting other adapters. Also enforce filename stem matches `^[a-z0-9][a-z0-9_-]{0,63}$` (lowercase kebab-case); uppercase or unsafe-character filenames are rejected with an error log. Tests covering: `Claude-Code.yaml` rejected (uppercase); `claude code.yaml` rejected (space); `claude-code.yaml` accepted
- [ ] 3.3 Implement defensive load: parse failure, schema failure, binary resolution failure → skip with log, continue with rest
- [ ] 3.4 Implement binary resolution via `exec.LookPath` for non-absolute paths, `os.Stat` + mode check for absolute; adapter retains its YAML data but is marked `BinaryMissing` state
- [ ] 3.5 Create `Registry` type holding the snapshot (`map[string]*Adapter`); expose `Healthy() []*Adapter` filtering out BinaryMissing and smoke-failed
- [ ] 3.6 Unit tests: builtin-only registry, on-disk override of builtin, mixed valid/invalid files, empty dir creates dir with mode 0700

## 4. PATH scan and environment detection

- [ ] 4.1 Create `internal/extagent/pathscan/builtins.go` with the embedded allowlist — `git`, `gh`, `jq`, `yq`, `curl`, `wget`, `rg`, `fd`, `pandoc`, `libreoffice`, `soffice`, `ffmpeg`, `convert`, `magick`, `identify`, `yt-dlp`, `playwright`, `chromium`, `chrome`, `firefox`, `python3`, `node`, `npm`, `pnpm`, `yarn`, `deno`, `bun`, `go`, `cargo`, `rustc`, `docker`, `podman`, `kubectl`, `terraform`, `ansible`, `asciidoctor`, `marp` (NOTE: `imagemagick` is intentionally absent — it is a package name, the binaries are `convert`/`magick`/`identify`); function `Allowlist()` returning the union with `extra` minus `disabled`
- [ ] 4.2 Emit a `pathscan.large name_count=N` warn-level log line when the resolved (union, post-disable) set size exceeds 80 — no truncation, just observability
- [ ] 4.3 Implement `pathscan.Scan(allowlist []string)` running `exec.LookPath` per entry in parallel (workers = min(NumCPU, 8)); collect resolved absolute paths
- [ ] 4.4 Implement `pathscan.Version(bin string)` invoking `<bin> --version` with 2s timeout via `context.WithTimeout`; tolerate failure (return empty string + debug log)
- [ ] 4.5 Implement disk cache at `<profileDir>/cache/pathscan.json` (mode `0600`) schema `{scanned_at, extra_fingerprint, disabled_fingerprint, entries: [{name, path, version}]}`; helpers `LoadCache(...)` / `WriteCache(...)` using temp-file + rename for atomic write (see design.md D23)
- [ ] 4.6 Implement cache invalidation logic: missing file, TTL expired, or fingerprint mismatch → re-scan; otherwise reuse
- [ ] 4.7 Unit tests: cache hit path, TTL-expired re-scan, fingerprint-change re-scan, version probe timeout, atomic write under simulated interrupt

## 5. internal/prompt EnvironmentBlock helper

- [ ] 5.1 Add `EnvironmentBlock(input EnvironmentInput) string` to `internal/prompt`; `EnvironmentInput` carries OS, shell, cwd, []CLITool, []SubAgentHint
- [ ] 5.2 Render `<environment>` block per design.md §D15 with stably-sorted entries inside each section; empty input → empty string output
- [ ] 5.3 Update the agent-loop call site (`internal/agent/loop.go`, near the existing memory prepend at L370-372) to also prepend the environment block via the composition rule `environment → memory → base` joined by `"\n\n"` only between non-empty pieces. Do NOT modify `BuildSystemPrompt`'s signature; do NOT add a template slot
- [ ] 5.4 Tests covering `EnvironmentBlock`: empty input produces empty output; non-empty input is byte-stable across calls; sorting within sections is stable
- [ ] 5.5 Tests covering the loop call-site composition: env empty + mem non-empty (one joiner, no spurious blank), env non-empty + mem empty (one joiner), both empty (base unchanged), both non-empty (two joiners, fixed order)

## 6. Sub-process driver

- [ ] 6.1 Create `internal/extagent/driver/driver.go` exposing `Driver.Run(ctx, adapter, prompt, opts) (Result, error)` and `Result{Stdout, Stderr, ExitCode, Cancelled, TimedOut, Truncated, TruncatedAtBytes, RawDumpPath}`
- [ ] 6.2 Implement env composition in the exact order from design.md D9 — filter LAST as a safety net so `env_override` cannot re-introduce denied variables: `os.Environ()` projected to map → restrict to `invocation.env_passthrough` allowlist → layer `invocation.env_override` (verbatim from YAML) → inject `PATH`/`HOME` if absent → re-serialize to `[]string` → `bash.Filter(envSlice)` ONCE on the merged result → `bash.LogDropped(logger, dropped)` → assign `kept` to `exec.Cmd.Env`. Matches `internal/tools/bash/bash.go:91-100` ordering. Test that `env_override: {LD_PRELOAD: /tmp/evil.so}` is stripped (the canonical bypass case)
- [ ] 6.3 Implement argv composition per `invocation.prompt_via` (`arg` / `stdin` / `file`); for `file`, create temp file UNDER `os.TempDir()` (NOT the workdir) with mode `0600`, write prompt, defer `os.Remove` regardless of outcome
- [ ] 6.4 Set `SysProcAttr.Setpgid = true` so the child runs in its own process group; child's process-group id is recorded for kill semantics
- [ ] 6.5 Implement cancellation: on `ctx.Done()`, send `control.cancel_signal` to `-pgid`, wait up to `control.cancel_grace_sec`, then SIGKILL `-pgid`; mark `Result.Cancelled = true`
- [ ] 6.6 Implement timeout via `context.WithTimeout(parentCtx, effective)`; on timeout, run identical teardown and mark `Result.TimedOut = true`
- [ ] 6.7 Implement streaming output collection: read stdout/stderr concurrently into a memory buffer counting bytes; cap at the existing global `tool_result_max_bytes` config (default 1 MiB — same value the orchestrator's `tools.TruncateOutput` enforces, so the orchestrator becomes a no-op for ExternalAgent output and there's no double truncation). On cap-hit: drain further bytes into `io.Discard`; if `external_agents.driver.kill_on_output_cap` (default true), trigger the cancel-grace-SIGKILL sequence; mark `Result.Truncated = true`. Truncated tool_result text appends exactly ONE `[... truncated N bytes]` marker. Cross-reference design.md D11
- [ ] 6.8 Implement raw-output debug dump: when `Result.Truncated || Result.ExitCode != 0`, write the captured raw stdout+stderr to `os.TempDir()/workhorse-extagent-<session_id>-<call_id>.log` with mode `0600`; set `Result.RawDumpPath`; tool_result text appends `[raw output dump: <path>]`. Happy-path (no truncate, exit 0) skips the dump
- [ ] 6.9 Implement output parsing per `output.format`: `text` (ANSI strip), `jsonl` (per-line JSON + optional JSONPath via the parser from task 2.7), `streaming-json` (tolerate trailing partial), `sse` (parse `data:` events). For JSONPath failures (null/undefined extract, non-string value, line-parse error): log at debug level and continue with next chunk; never abort the call (see design.md D12). If the adapter declares `output.parser.session_id_path` AND `session.supports_resume: true`, extract the value (first non-empty match wins) and append a single `[SESSION_ID: <id>]` footer line to the tool_result text. When the path is unset OR yields no value OR adapter is non-resumable, no footer is added
- [ ] 6.10 Implement `output.stderr` handling: `separate` (append delimited block), `merge` (interleave by receipt order), `ignore` (drop)
- [ ] 6.11 Emit per-invocation structured log line `external_agent.invoke` at info level with fields: `adapter`, `session_id`, `call_id`, `duration_ms`, `exit_code`, `cancelled`, `timed_out`, `truncated_bytes`, `prompt_chars` — single grep target for usage and failure patterns
- [ ] 6.12 Unit tests with fake binaries (a Go test helper that compiles a tiny child binary on demand): graceful cancel, hard kill on grace timeout, wall-clock timeout, output cap with kill-on-cap true, output cap with kill-on-cap false, raw dump on truncate, raw dump on non-zero exit, no dump on happy path, JSONPath null/non-string handling, stderr-separate, env audit log line presence, **env bypass guard** — adapter declaring `env_override: {LD_PRELOAD: ...}` MUST NOT see it reach the child, **env bypass guard NODE_OPTIONS** — adapter declaring `env_override: {NODE_OPTIONS: --require x}` MUST NOT see it reach the child, **single truncation marker** — output exceeding `tool_result_max_bytes` MUST contain exactly one `[... truncated N bytes]` marker (no orchestrator-added second marker)

## 7. Smoke test runner

- [ ] 7.1 Create `internal/extagent/smoke/runner.go` exposing `Run(adapter) (SmokeResult, error)`; sandbox cwd is a fresh `os.MkdirTemp` with mode `0700` that is removed via deferred `os.RemoveAll` (runs on panic / timeout / normal exit)
- [ ] 7.2 Compute sandbox env: `bash.Filter(os.Environ())` → restrict to `env_passthrough` → layer `env_override` → strip to minimum allowlist (`PATH`, `HOME`, `USER`, `LANG`) for the smoke run only
- [ ] 7.3 Invoke the driver with the smoke prompt and `smoke_test.timeout_sec`; reuse the same parsing pipeline so output extraction matches production invocation
- [ ] 7.4 Assert the parsed output contains `smoke_test.expected_substring`; on success write sibling `<name>.smoke` JSON `{passed: true, ran_at, output_hash}` (mode `0600`); on failure write `{passed: false, ran_at, error}` (mode `0600`); both writes use the temp-file + rename pattern (see design.md D23)
- [ ] 7.5 Implement cache-hit logic: skip re-run when sibling `.smoke` exists with `passed: true`, mtime newer than adapter file mtime, and `ran_at` within `cache_ttl`. Treat malformed `.smoke` JSON as cache-miss (re-run)
- [ ] 7.6 Wire smoke runner into the loader: after schema validation and binary resolution, run smoke (or read cache) for each `sub_agent`-class adapter. **Concurrency guard**: take an exclusive advisory file lock on a sibling `<name>.smoke.lock` file (via `golang.org/x/sys/unix` flock on Unix, `LockFileEx` on Windows) before running smoke; wait up to 30s if held; on lock-hold release re-read the `.smoke` cache (the other process likely just wrote it). This prevents two concurrent session creations from both running smoke on a cold cache and double-billing API calls
- [ ] 7.7 Unit tests with fake binaries: smoke pass, smoke fail (wrong substring), smoke timeout, cache hit path, cache invalidation on adapter mtime, cache invalidation on TTL expiry, atomic write under simulated interrupt, malformed cache treated as miss, **concurrent-smoke flock** — two goroutines simultaneously call smoke for the same cold-cache adapter; only ONE actually executes the child, the second re-reads the cache after the lock releases

## 8. ExternalAgent tool

- [ ] 8.1 Create `internal/tools/extagent/tool.go` with `Tool{Host}` and `Host{Registry, PermissionGate}`; tool name `ExternalAgent`
- [ ] 8.2 Generate input schema dynamically: `agent_name` enum from healthy `sub_agent` adapters (alphabetized for stability); `prompt`, `inputs`, `timeout_sec`, `resume_session_id` fields. **Cache the rendered JSON schema** at tool construction time (registry is per-session-fixed per D7); subsequent `InputSchema()` calls return the cached bytes — avoids ~2 KB of allocation per LLM turn over a long session
- [ ] 8.2b Implement `CanRunInParallel() bool { return true }` (mirrors Dispatch — D5a). Each invocation owns isolated I/O, buffer, process group, context. Test: two concurrent invocations against different adapters complete independently
- [ ] 8.3 Generate tool description listing each healthy `sub_agent` with its `description` and `usage_hints`
- [ ] 8.4 Implement `DefaultTimeout()` returning `max(adapter.control.max_timeout_sec for healthy sub_agent adapters) + 30s` with a 3630s fallback for empty registry — guarantees orchestrator backstop always sits beyond adapter internal deadline (see design.md D20)
- [ ] 8.5 Implement the `tools.InternalGated` marker interface returning `true` so `internal/agent/loop.go`'s `checkPermissions` bypasses `Permissions.Check` for this tool. `extractResource("ExternalAgent", ...)` returns "" (see design.md D21). **Implementation alternative to consider during code review** (D21 documents both): if you prefer extending `permission.Manager` with a public `AllowSession(sessionID, tool, resource)` method and pre-populating builtin adapter rules at session start, that achieves the same observable outcome but adds `"agent_name"` to `extractResource`'s key list. Pick one; record the choice in the PR description
- [ ] 8.6 Implement `Run`: validate `agent_name` against enum → fetch adapter → check health (binary present, smoke passed) → if `security.trusted: false` and not yet approved in this session's approved-set, call `Host.PermissionGate.Prompt(ctx, sessionID, "ExternalAgent", agent_name)` and record approval → clamp `timeout_sec` to `[1, control.max_timeout_sec]` defaulting to `control.default_timeout_sec` → invoke driver → format result text
- [ ] 8.7 Implement resume gating: if `resume_session_id` provided but adapter has `session.supports_resume: false`, return error before sub-process start
- [ ] 8.8 Add `Host.PermissionGate.Prompt(...)` thin wrapper around the same prompt callback the permission Manager uses — no registry rule consultation, just the prompt. Track per-session approved adapter names in `Host` keyed by `(sessionID, agent_name)`
- [ ] 8.9 Wire registration: tool is added to the session's tool surface only when at least one healthy `sub_agent` adapter exists at session creation; absent otherwise. `loop.go` must recognize the `InternalGated` interface and skip its permission check
- [ ] 8.10 Tests: unknown agent_name rejected, binary-missing adapter rejected, smoke-failed adapter rejected, builtin-trusted-no-prompt, untrusted-first-call-prompted-and-approved, untrusted-second-call-same-session-no-prompt, new-session-re-prompts, resume against non-resumable rejected, timeout enforcement, cancellation produces `[CANCELLED]` prefix, internal `[TIMEOUT]` fires before orchestrator backstop, `loop.go` bypasses `Permissions.Check` for this tool

## 9. Builtin adapters

- [ ] 9.1 Author `internal/extagent/builtins/claude-code.yaml`: `prompt_via: arg`, `prompt_arg: --prompt`, `output.format: streaming-json` with JSONPath parser, `session.supports_resume: true`, smoke prompt + expected substring, `security.trusted: true`
- [ ] 9.2 Author `internal/extagent/builtins/codex.yaml`: best-current settings for the OpenAI Codex CLI; resume support per the actual CLI's capability; smoke prompt
- [ ] 9.3 Author `internal/extagent/builtins/aider.yaml`: `prompt_via: stdin` (aider reads from stdin in non-tty mode); resume gated per CLI capability; smoke prompt
- [ ] 9.4 Embed via `//go:embed *.yaml` and load via the registry seed step
- [ ] 9.5 Smoke-test each builtin manually against the actual CLI on a dev machine before merging (this verifies the YAML is correct against the real binary, not just self-consistent)

## 10. Wiring into session lifecycle

- [ ] 10.1 Identify the session-creation seam in `internal/session/`; arrange for the registry snapshot and PATH-scan snapshot to be computed there exactly once per session, threaded through to system-prompt construction
- [ ] 10.2 Surface the snapshots to the tool registry construction so `ExternalAgent` sees the right enum for this session
- [ ] 10.3 Ensure that mid-session writes to the adapter directory do not affect the live session's registry or system prompt (the snapshot is by-value, not by-reference to a mutable map)
- [ ] 10.4 Integration test: create session A, observe its `<environment>` block; drop a new adapter file; create session B, observe its `<environment>` block includes the new adapter; session A's snapshot is unchanged

## 11. Documentation and CLAUDE.md updates

- [ ] 11.1 Add a new section to `CLAUDE.md` titled "External agents" covering: adapter location, schema link, sub_agent vs cli_tool, how to add a new adapter manually, security model (trusted vs untrusted), smoke test cache, PATH allowlist extension
- [ ] 11.2 Add an example walkthrough to `openspec/AGENTS.md` (or wherever project guidance lives): "How to register a new sub-agent CLI"
- [ ] 11.3 Document the `external_agents.*` config keys in the config schema doc and in any `--help`/`workhorse-agent config explain` output

## 12. Cross-change coordination

- [ ] 12.1 Confirm with the `add-memory-l1-l2` author that the loop-call-site composition order `environment → memory → base` (joined by `"\n\n"` only between non-empty pieces) is mutually agreed; capture in both designs and code comments at `internal/agent/loop.go`
- [ ] 12.2 Coordinate `internal/config/config.go` struct changes with `add-memory-l1-l2` (`MemoryConfig`) and Change 2 (`ExternalAgents.Generation`) — all three changes add top-level fields. Land in cherry-pick-friendly order
- [ ] 12.3 Make `add-llm-adapter-generator` (the follow-up change) declared as depending on this change in its proposal

## 13. End-to-end integration tests

- [ ] 13.1 E2E: build a fake binary in test setup that responds to `--prompt "X"` with `WORKHORSE_SMOKE_OK` (smoke) and to other prompts with predictable output; install it on `PATH`; drop an adapter YAML pointing at it; start the server; create a session; observe `<environment>` block; invoke `ExternalAgent`; assert `tool_result` text contains expected output and the event log contains the structured `external_agent.invoke` log line with correct fields
- [ ] 13.2 E2E: two concurrent sessions both invoke `ExternalAgent` against the same adapter; assert no shared state corruption; assert per-session permission approval (untrusted adapter: session A's approval does NOT carry to session B)
- [ ] 13.3 E2E: invoke `ExternalAgent` with a prompt that produces >4 MiB of output; assert `Truncated` flag, `[... truncated N bytes]` suffix, raw-dump tempfile created, kill-on-cap fired (default config)
- [ ] 13.4 E2E: cancellation mid-invocation (parent context cancel); assert `[CANCELLED]` prefix, child process gone (process-group reaped), no orphans
- [ ] 13.5 E2E: timeout via per-call `timeout_sec`; assert `[TIMEOUT]` prefix (not `[CANCELLED]`), orchestrator backstop did NOT fire first
