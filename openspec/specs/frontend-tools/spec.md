# frontend-tools Specification

## Purpose
TBD - created by archiving change add-frontend-tool-bridge. Update Purpose after archive.
## Requirements
### Requirement: Proxy frontend tool class

The agent SHALL support a tool class whose `Run` does not execute in-process but
instead emits a `frontend_tool_use` server event toward the client and suspends
until a matching `frontend_tool_result` arrives, correlated by a per-call id.
The class SHALL honour the orchestrator's `context` so existing
timeout/cancel/panic handling produces an `is_error` result without bespoke
logic.

#### Scenario: Model invokes a frontend tool

- **WHEN** the model emits a `tool_use` for a registered frontend tool
- **THEN** the tool's `Run` emits a `frontend_tool_use` event carrying a freshly
  minted correlation id, the tool name, and the input
- **AND** `Run` suspends until a `frontend_tool_result` with the same id arrives

#### Scenario: Frontend tool result resumes the turn

- **WHEN** a `frontend_tool_result` with a matching id arrives carrying `{ok:true, value}`
- **THEN** `Run` returns a `tools.Result` whose `Output` is the serialized `value`
  and `IsError` is false
- **AND** the agent loop appends the tool_result and continues the turn

#### Scenario: Frontend tool result carries an error envelope

- **WHEN** a `frontend_tool_result` carries `{ok:false, error:{kind, message}}`
- **THEN** `Run` returns a `tools.Result` with `IsError` true and `Output` set to
  the error message

#### Scenario: No result before timeout

- **WHEN** no matching `frontend_tool_result` arrives before the orchestrator's
  per-tool timeout (or the turn is cancelled)
- **THEN** the orchestrator's existing handling yields an `is_error` tool_result
  and the turn never hangs
- **AND** the tool's pending correlation entry is removed so it does not leak

#### Scenario: Parallel-safety is derived from the catalog

- **WHEN** a frontend tool's catalog entry declares `parallelSafety:"safe"`
- **THEN** the tool reports `CanRunInParallel()` true, so the orchestrator may
  batch it concurrently with other safe tools
- **AND** an `"unsafe"` entry (or one that omits `parallelSafety`, which defaults
  to `"unsafe"`) reports `CanRunInParallel()` false, so the orchestrator runs it
  in its own serial batch
- **AND** `IsReadOnly()` is always false regardless of `parallelSafety` (the
  agent cannot verify client-side side effects; only `CanRunInParallel()` drives
  batching)

### Requirement: Per-session frontend tool registration

The agent SHALL accept a tool catalog from the client and register those tools
into **only that session's** tool surface, without recompiling and without
affecting other sessions. Re-publishing SHALL replace the session's frontend
tool set.

#### Scenario: Register a catalog

- **WHEN** the client publishes a catalog of `{name, description, inputSchema, outputSchema, parallelSafety}` entries
- **THEN** each non-colliding entry becomes a tool callable by the model in that
  session, with `inputSchema` advertised as its parameters
- **AND** the agent emits a `frontend_tools_published` event listing the
  registered names

#### Scenario: Name collides with a server-side tool

- **WHEN** a catalog entry's name matches an existing server-side tool in that session
- **THEN** that entry is rejected, the authoritative server-side tool is retained,
  and the rejection (name + reason) is reported in `frontend_tools_published`
- **AND** non-colliding entries in the same catalog still register

#### Scenario: Re-publishing replaces the prior set

- **WHEN** the client publishes a new catalog for a session that already has one
- **THEN** the previously registered frontend tools are unregistered and the new
  set takes their place
- **AND** a frontend tool removed by the new catalog is no longer callable

#### Scenario: Catalogs are scoped to the session

- **WHEN** two sessions publish different catalogs
- **THEN** each session's model sees only its own session's frontend tools
