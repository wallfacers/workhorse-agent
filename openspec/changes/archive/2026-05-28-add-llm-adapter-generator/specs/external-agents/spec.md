## MODIFIED Requirements

<!-- These blocks modify requirements introduced by add-external-agent-tool's
     `external-agents` capability. Each requirement body below is the full
     post-edit content (copy of the original requirement with the changes
     described in design.md G8/G9/G10 and spec.md §"Implicit trigger" /
     §"Approved adapter skips first-invocation approval" applied).

     Cross-reference: add-external-agent-tool/specs/external-agents/spec.md
     contains the originals. After both changes archive, the merged content
     in openspec/specs/external-agents/spec.md reflects the post-edit form. -->

### Requirement: ExternalAgent invocation against unknown adapter

The system SHALL reject `ExternalAgent` calls whose `agent_name` does not appear in the current session's enum, EXCEPT when the LLM-driven adapter-generation flow from the `adapter-generation` capability intercepts the call. The standard rejection path applies when any of the following holds: (a) `external_agents.generation.implicit_trigger_enabled` is `false`, OR (b) no binary matching `agent_name` resolves on `PATH` via `exec.LookPath`, OR (c) the per-session adapter-generation dedup map records `agent_name` in state `unavailable` (i.e. a prior generation in this session was rejected or expired). When the intercept fires (binary resolves, intercept is enabled, no `unavailable` dedup entry), the tool MUST NOT emit the standard "unknown agent" error; instead the intercept produces a tool_result naming the new approval_id and repeating the model's call parameters verbatim (per the `adapter-generation` Plan A requirement). In every case the rejection or intercept MUST occur before any sub-process is started.

#### Scenario: Unknown agent_name rejected (no intercept eligible)

- **WHEN** the model emits `ExternalAgent` with `agent_name: "gemini"` but `gemini` is not in the session's enum
- **AND** either `implicit_trigger_enabled` is false, OR no `gemini` binary resolves on PATH, OR the dedup map shows `gemini → unavailable`
- **THEN** no sub-process is started and the tool returns the standard error tool_result naming `gemini` and listing the available agents

#### Scenario: Unknown agent_name intercepted by Plan A

- **WHEN** the model emits `ExternalAgent` with `agent_name: "gemini"`, `gemini` resolves via `exec.LookPath`, `implicit_trigger_enabled` is true, and the dedup map has no entry for `gemini`
- **THEN** no sub-process is started, the adapter-generation flow runs synchronously, and the tool returns an intercept tool_result (NOT the standard error) naming the new approval_id; the dedup map records `gemini → pending`

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
