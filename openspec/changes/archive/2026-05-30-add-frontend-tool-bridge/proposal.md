## Why

workhorse-agent's tools all execute server-side (Bash, Read, Write, Dispatch,
MCP adapters). A companion desktop client (workhorse-assistant, a Tauri app)
wants the model to **operate its UI** — open tabs, read a button's state, click
controls — by exposing those UI capabilities as ordinary agent tools.

The UI cannot be compiled into Go: it lives in the renderer and changes per
release. So the agent needs a tool class whose execution is **delegated to the
client**: when the model calls such a tool, the agent emits a `tool_use` toward
the client over the existing SSE stream, the client runs it in the browser, and
posts the result back. This is structurally identical to the existing
**permission-control** round-trip (loop blocks, emits an event, awaits a client
POST), and reuses the orchestrator's existing timeout/cancel/panic handling.

The client side (renderer registries + Rust transport) is already built in the
workhorse-assistant repo against a fixed wire contract; this change implements
the agent half.

## What Changes

- **Frontend (proxy) tool class**: a `tools.Tool` whose `Run()` does not execute
  locally — it emits a `frontend_tool_use` server event and blocks on the
  matching client `frontend_tool_result`, correlated by a per-call id, honouring
  the orchestrator's `ctx` (so timeout/cancel synthesise an `is_error` result
  exactly as for any tool).
- **Per-session dynamic registration**: a new client message
  `publish_frontend_tools` carries a catalog of `{name, description,
  inputSchema, outputSchema, parallelSafety}`; the agent registers those tools
  into **that session's** tool surface only. Re-publishing replaces the set.
  Names colliding with a server-side tool are rejected (server-side retained)
  and reported back via a `frontend_tools_published` event.
- **New protocol messages/events** on the existing `POST|GET /v1/sessions/{id}/stream`:
  client→`publish_frontend_tools`, `frontend_tool_result`; server→
  `frontend_tool_use`, `frontend_tools_published`. The POST/state table accepts
  `frontend_tool_result` while `Executing`.
- **Session plumbing**: a per-session frontend `Bridge` (emit + pending-result
  correlation), mirroring `PermissionAnswers`.

## Capabilities

### New Capabilities
- `frontend-tools`: the proxy tool class + per-session catalog registration that
  lets a client expose UI actions/state as agent tools and resolve their results.

### Modified Capabilities
- `api-protocol`: adds two client message types and two server event types, and
  extends the POST/state acceptance table to admit `frontend_tool_result` while
  the session is `Executing`.

## Impact

- **New package** `internal/tools/frontend` (Bridge + proxy Tool).
- **`internal/session`**: a `Frontend *Bridge` handle + the new `ClientMessageType`
  constants and payload structs (mirrors `PermissionAnswers`).
- **`internal/api`**: `stream_post.go` routing + `stateAccepts`; protocol package
  client-message decode + event-type constants.
- **`internal/agent/loop.go`**: own/construct the per-session bridge; on
  `publish_frontend_tools`, (re)register tools into the session registry.
- **Cross-repo**: the workhorse-assistant Rust bridge + `contract.md` align to the
  real `/v1/sessions/{id}/stream` protocol and these message/event names
  (separate repo, coordinated; this change fixes the wire names).
- **License boundary preserved**: the two repos stay separate processes over
  HTTP — workhorse-agent remains AGPL-3.0, the client is unaffected.
