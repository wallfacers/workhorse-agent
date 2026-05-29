## ADDED Requirements

### Requirement: Frontend-tool client messages

`POST /v1/sessions/{id}/stream` SHALL accept two additional `ClientMessage`
types carrying the frontend-tool round-trip, decoded by the protocol package
like the existing types.

#### Scenario: publish_frontend_tools accepted while Idle

- **WHEN** a `publish_frontend_tools` message arrives while the session is `Idle`
- **THEN** the server accepts it (202) and registers the catalog for the session

#### Scenario: publish_frontend_tools rejected outside Idle

- **WHEN** a `publish_frontend_tools` message arrives while the session is not `Idle`
- **THEN** the server responds `409` with a `session_busy`-shaped body
- **AND** mirrors an `error` event to the SSE stream (matching the compact/POST
  conflict rule)

#### Scenario: frontend_tool_result accepted while Executing

- **WHEN** a `frontend_tool_result` message arrives while the session is `Executing`
- **THEN** the server accepts it (202) and routes it to the session's frontend
  bridge, resolving the matching suspended tool call

#### Scenario: frontend_tool_result for an unknown id is inert

- **WHEN** a `frontend_tool_result` carries an id with no suspended call (e.g. it
  already timed out)
- **THEN** the server accepts it (202) and drops it without error

### Requirement: Frontend-tool server events

The agent SHALL emit two additional server events through the session Outbox →
SSE path, wrapped with the standard `{type, idx, session_id}` envelope so
ordering and Last-Event-ID replay behave like every other event.

#### Scenario: frontend_tool_use emitted on invocation

- **WHEN** a frontend tool's `Run` begins
- **THEN** a `frontend_tool_use` event is emitted with `{tool_use_id, name, input}`

#### Scenario: frontend_tools_published emitted after registration

- **WHEN** a `publish_frontend_tools` message is processed
- **THEN** a `frontend_tools_published` event is emitted with
  `{registered:[...], rejected:[{name, reason}]}`
