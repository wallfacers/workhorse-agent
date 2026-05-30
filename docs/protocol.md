# workhorse-agent protocol reference

This document is the wire-level contract for the workhorse-agent HTTP API.
It is the source of truth for anyone writing a client (browser UI, CLI,
another service) against `workhorse-agent serve`. The canonical event and
error enums are in `internal/api/protocol/protocol.go`; the requirements
behind them live in
`openspec/changes/init-workhorse-agent-mvp/specs/api-protocol/spec.md`.

## Overview

- Single HTTP server, default bind `127.0.0.1:7821`.
- One conversation = one **session**, identified by a 26-character ULID.
- A session exposes two operations on the same path
  `/v1/sessions/{id}/stream`:
  - `POST` submits one **ClientEvent** (a JSON object with a `type`
    discriminator) and returns `202 Accepted`.
  - `GET` opens a long-lived **SSE** stream of **ServerEvents** (one per SSE
    frame).
- A side set of REST endpoints handles session CRUD, cancel, manual
  compaction, health, and debug event replay.
- Order is global per session: every ServerEvent is assigned a monotonically
  increasing `int64` index (`idx`) at the moment it is persisted, and that
  same `idx` becomes the SSE frame's `id:` field.

## Relation to MCP 2025-11-25

workhorse-agent borrows the **transport model** of MCP 2025-11-25 Streamable
HTTP:

- Single endpoint that multiplexes a POST submission channel and a GET SSE
  channel.
- SSE `id:` field plus `Last-Event-ID` for resumable streams.
- Mandatory `Origin` validation and a default localhost bind for DNS
  rebinding defence.

It is **not** an MCP server. The message envelope is an application-level
ClientEvent / ServerEvent JSON object, not a JSON-RPC 2.0 request. A generic
MCP client that POSTs `{"jsonrpc":"2.0","method":"initialize",...}` to this
endpoint receives `400 Bad Request` with
`{"code":"unknown_message_type","message":"..."}` and no side effects.

## REST endpoint table

| Method | Path                                       | Purpose                                                       |
|--------|--------------------------------------------|---------------------------------------------------------------|
| POST   | `/v1/sessions`                             | Create a session.                                             |
| GET    | `/v1/sessions`                             | List sessions.                                                |
| GET    | `/v1/sessions/{id}`                        | Session detail.                                               |
| DELETE | `/v1/sessions/{id}`                        | Destroy a session (cancels in-flight work first).             |
| POST   | `/v1/sessions/{id}/cancel`                 | Interrupt the current turn.                                   |
| POST   | `/v1/sessions/{id}/compact`                | Trigger context compaction.                                   |
| POST   | `/v1/sessions/{id}/stream`                 | Submit one ClientEvent.                                       |
| GET    | `/v1/sessions/{id}/stream`                 | Subscribe to the SSE event stream.                            |
| GET    | `/debug/sessions/{id}/events?since=N`      | NDJSON replay of events with `idx > N`. Requires `debug.enabled`. |
| GET    | `/health`                                  | Liveness probe. Returns `{ok, version, uptime_sec, sessions_active}`. |
| GET    | `/ui`                                      | Embedded reference web UI (static asset).                     |

`POST /v1/sessions` returns `201 Created` with a JSON body whose `id` matches
`^[0-9A-HJKMNP-TV-Z]{26}$` (Crockford Base32 ULID).

## HTTP method and content negotiation

The `/v1/sessions/{id}/stream` endpoint enforces strict negotiation:

| Condition                                                        | Status | Notes                                  |
|------------------------------------------------------------------|--------|----------------------------------------|
| Method not in `GET`, `POST`                                      | 405    | Response includes `Allow: GET, POST`.  |
| `POST` without `Content-Type: application/json`                  | 415    | `unsupported_media_type`.              |
| `GET` whose `Accept` is set and does not include `text/event-stream` or `*/*` | 406 | `not_acceptable`. |
| `POST` body exceeds `server.max_request_body_bytes` (default 1 MiB) | 413 | `{"code":"request_too_large","limit":<bytes>}`. |
| Session not found                                                | 404    | All `/v1/sessions/{id}/*` paths.       |
| Session in a state that rejects this POST type                   | 409    | See "POST and session state" below.    |

## Streamable HTTP transport

### POST: submit one ClientEvent

```
POST /v1/sessions/{id}/stream
Content-Type: application/json

{"type":"user_message","content":"hello"}
```

On success the server returns `202 Accepted` with no body. Effects of the
message become observable as ServerEvents on the GET stream.

### GET: subscribe to SSE

```
GET /v1/sessions/{id}/stream
Accept: text/event-stream
Last-Event-ID: 42        (optional)
```

Response headers:

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
```

Each event is one SSE frame:

```
id: <idx>
event: <type>
data: <compact-single-line-json>

```

(blank line is the SSE frame terminator). `data` is always a single line of
JSON; values that contain newlines use the JSON escape `\n`, never a literal
newline. Every 25 seconds (configurable via `server.sse_keepalive_seconds`)
the server emits a comment frame as a heartbeat:

```
: keep-alive

```

### Single active GET per session

At most one GET SSE stream per session is connected at any time. When a
second GET arrives the server takes a session-level write lock and:

1. Writes `: superseded\n\n` to the old response.
2. Closes the old response writer.
3. Hands control to the new handler.

Events generated during the handover are still written to the events table.
The new client reaches them via `Last-Event-ID` replay (see below).

### Last-Event-ID resumption

The client may send `Last-Event-ID: N` as a header **or** `?last_event_id=N`
as a query parameter (for environments such as `curl -N` that cannot set
headers). If both are present, the header wins.

Replay is atomic:

1. Under the session write lock, snapshot `max_idx = MAX(events.idx)`.
2. Stream events where `Last-Event-ID < idx <= max_idx` in order.
3. Release the lock and switch to live tail from the outbox channel.

Any events the loop produces during replay are written to the events table
normally and delivered after the replay batch finishes, so the client sees
no duplicates and no gaps.

### Client disconnect

When `r.Context().Done()` fires the SSE writer stops, the stream slot is
released so a future GET can take over, but the session goroutine keeps
running. Subsequent events accumulate in the events table.

### Interrupt and outbox flush

Receiving a `{"type":"interrupt"}` POST does:

1. Cancel the session context immediately.
2. Drain the outbox channel — events buffered for the cancelled turn are
   discarded from the live stream (but **not** deleted from the events
   table; a client that reconnects with `Last-Event-ID` still observes
   them).
3. Emit one `interrupted` event as the terminal event of the turn.

## ClientEvent types

Five `type` values are accepted; anything else returns `400 Bad Request`
with `{"code":"unknown_message_type","message":"..."}` on the POST and is
also surfaced on the SSE stream so a one-channel client notices.

| `type`                | Payload fields                                                                                                                | Notes                                          |
|-----------------------|-------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------|
| `user_message`        | `content: string`, `attachments?: []`                                                                                          | Begin a new turn.                              |
| `permission_decision` | `request_id: string`, `decision: "allow_once" \| "allow_session" \| "allow_permanent" \| "deny" \| "deny_permanent"`           | Reply to a pending `permission_request`.       |
| `interrupt`           | (none)                                                                                                                         | Cancel the current turn.                       |
| `ping`                | (none)                                                                                                                         | Heartbeat. Produces a `pong` ServerEvent.      |
| `context_update`      | `workdir?: string`, `files?: []`                                                                                               | Metadata-only update; does not start a turn.   |

### POST and session state

The state machine has six states. Which client types are accepted depends
on the current state.

| State        | Accepted POST types                                | Otherwise          |
|--------------|----------------------------------------------------|--------------------|
| `Idle`       | all five                                           | —                  |
| `Thinking`   | `interrupt`, `ping`                                | 409 `session_busy` |
| `AwaitPerm`  | `permission_decision` (matching `request_id`), `interrupt`, `ping` | 409 `session_busy` |
| `Executing`  | `interrupt`, `ping`                                | 409 `session_busy` |
| `Compacting` | `interrupt`, `ping`                                | 409 `session_busy` |
| `Cancelled`  | `interrupt` (idempotent → 202), `ping`             | 409 `session_busy` |

On a 409 the server also emits an `error` event with the same `code` and a
`details.state` field so SSE-only clients observe the rejection.

## ServerEvent types

Twenty event types are defined (see `protocol.AllServerEventTypes`). The
eleven core event types are listed below; capability-specific events
(adapter approval, frontend tools, task update) are documented in their
respective specs. All events carry `idx` (int64) and `session_id` (ULID) in
addition to the type-specific payload listed below.

### Core events

| `event`                | Payload fields                                                                            | Meaning                                                    |
|------------------------|-------------------------------------------------------------------------------------------|------------------------------------------------------------|
| `assistant_text_delta` | `delta: string`                                                                            | One token chunk of assistant text.                         |
| `assistant_text_done`  | `message_id: string`                                                                       | End of a text segment.                                     |
| `reasoning_start`      | `block_index: int`, `type: "thinking" \| "redacted"`                                        | A thinking block begins. `type` distinguishes regular and redacted thinking. |
| `reasoning_delta`      | `block_index: int`, `delta: string`                                                         | Thinking text increment (regular thinking only).           |
| `reasoning_end`        | `block_index: int`                                                                         | Thinking block ends.                                       |
| `tool_call_start`      | `id: string`, `tool: string`, `input: object`                                              | Tool invocation begins.                                    |
| `tool_call_done`       | `id: string`, `output: any`, `ok: bool`, `took_ms: int`                                    | Tool invocation finishes.                                  |
| `permission_request`   | `request_id: string`, `tool: string`, `input: object`, `pattern?: string`                  | Block waiting for a `permission_decision` reply.           |
| `subagent_event`       | `parent_id: string`, `child_id: string`, `event: <wrapped event>`                          | Streaming event from a dispatched sub-session.             |
| `compaction`           | `before_tokens: int`, `after_tokens: int`, `kept_recent: int`                              | Automatic or manual context compaction completed.          |
| `provider_retry`       | `attempt: int`, `after_ms: int`                                                            | Provider returned a retryable error; backing off.          |
| `error`                | `code: string`, `message: string`, `recoverable: bool`, `details: object`                  | See error table below.                                     |
| `interrupted`          | (none beyond envelope)                                                                     | Terminal event after an `interrupt` or graceful shutdown.  |
| `pong`                 | (none beyond envelope)                                                                     | Reply to a `ping`.                                         |

### Reasoning events and signature handling

The `reasoning_start` / `reasoning_delta` / `reasoning_end` events expose the
model's extended thinking to the client for real-time display (e.g. collapsible
sections). Key invariants:

- **`reasoning_start.type`** distinguishes `"thinking"` (regular, carries text
  deltas) from `"redacted"` (opaque, no deltas emitted).
- **Signature is never exposed**: the thinking block's cryptographic signature
  is persisted server-side and included in Anthropic API round-trips, but it
  **never** appears in any SSE payload.
- **Redacted data is never exposed**: `redacted_thinking` blocks carry opaque
  Anthropic-encrypted data that is also never sent to the client.

### Event ordering

Events are appended through a single session-level mutex; the SSE writer
sees them in strictly increasing `idx` order. The `idx` value is allocated
inside the same SQLite transaction that inserts the row (table primary key
`INTEGER PRIMARY KEY AUTOINCREMENT`), so the events table and the SSE
stream agree on every value.

## Error event schema

The `error` ServerEvent always uses this shape:

```json
{
  "type": "error",
  "idx": 42,
  "session_id": "01HZ...",
  "code": "session_busy",
  "message": "session is currently busy",
  "recoverable": true,
  "details": { "state": "Compacting" }
}
```

`details` is always present (empty object when no fields apply) so the
schema is stable.

The full enum of 14 error codes:

| `code`                              | Trigger                                                                  | `recoverable` | `details` fields                                      |
|-------------------------------------|--------------------------------------------------------------------------|---------------|-------------------------------------------------------|
| `session_busy`                      | POST rejected because session is in Thinking/Executing/Compacting/etc.   | true          | `{ "state": "<current>" }`                            |
| `unknown_message_type`              | POST body has a `type` outside the five-value enum.                      | false         | `{ "received_type": "<what client sent>" }`           |
| `history_token_limit`               | History exceeds `agent.max_history_tokens`.                              | false         | `{ "limit": <int>, "current": <int> }`                |
| `tool_not_allowed`                  | LLM tried to invoke a tool outside the allowed set.                      | true          | `{ "tool": "<name>" }`                                |
| `permission_denied`                 | Permission rule denied or user replied `deny`.                           | true          | `{ "tool": "<name>", "pattern": "<rule>" }`           |
| `provider_auth_failed`              | Provider returned 401.                                                   | false         | `{ "provider": "anthropic" }`                         |
| `provider_invalid_request`          | Provider returned 400.                                                   | false         | `{ "provider": "...", "upstream_message": "..." }`    |
| `provider_context_length_exceeded`  | Provider rejected an over-long request.                                  | false         | `{ "provider": "...", "tokens": <int> }`              |
| `provider_insufficient_quota`       | Provider quota exhausted.                                                | false         | `{ "provider": "openai" }`                            |
| `provider_unrecoverable`            | Any other non-retryable provider error.                                  | false         | `{ "provider": "...", "upstream_code": "..." }`       |
| `cancel_timeout`                    | Cancel drain exceeded `agent.cancel_drain_timeout_seconds`.              | true          | `{ "phase": "<name>", "elapsed_ms": <int> }`          |
| `internal_panic`                    | Session goroutine recovered from a panic.                                | true          | `{}` (stack trace logged, not sent).                  |
| `server_shutdown`                   | Graceful shutdown in progress.                                           | false         | `{}`                                                  |
| `request_too_large`                 | POST body exceeded `server.max_request_body_bytes`.                      | true          | `{ "limit": <bytes> }`                                |

`recoverable: true` means the session is still usable for new
`user_message` POSTs. `recoverable: false` typically means the client
should `DELETE` the session and create a new one after fixing the
underlying problem.

## Graceful shutdown

When the process receives `SIGTERM` or `SIGINT`, the server runs these
seven steps in order:

1. Stop accepting new HTTP connections (existing SSE streams stay open).
2. Cancel every session in Thinking / Executing / AwaitPerm / Compacting,
   running the same flow as a user `interrupt` (synthesise a cancelled
   `tool_result`, emit `interrupted`).
3. Wait for the cancelled `tool_result` rows and the `interrupted` events
   to land in the events table and the outbox channel.
4. On each active GET SSE stream, emit
   `error{code:"server_shutdown", recoverable:false}` and flush, then close
   the response writer.
5. Wait for all session goroutines to exit, bounded by
   `server.graceful_shutdown_timeout_seconds` (default 30).
6. Close the SQLite connection and shut down the MCP host.
7. Exit 0 if the deadline held, exit 1 otherwise.

**Invariant.** The cancelled `tool_result` and `interrupted` events for
each session reach the client *before* the `error{server_shutdown}` event.
Ephemeral sessions (no persistent history) are no exception; their
cancellation events are delivered over the still-open SSE stream before
that stream is closed in step 4.

## Auth

Bearer token authentication is optional, controlled by `auth.enabled` and
`auth.bearer_token` in `config.yaml`. When enabled:

- All `/v1/*` and `/debug/*` endpoints require
  `Authorization: Bearer <token>`.
- `/health` and `/ui` are exempt.
- Missing or wrong tokens return `401 Unauthorized` with
  `{"code":"auth_required"}` or `{"code":"invalid_token"}` respectively.
- The comparison uses `crypto/subtle.ConstantTimeCompare`. The token value
  is never written to logs, traces, or error messages.

## Body size limit

Every POST endpoint wraps the request body in `http.MaxBytesReader` at
`server.max_request_body_bytes` (default 1 MiB / 1048576 bytes). Exceeding
the limit returns `413 Payload Too Large` with
`{"code":"request_too_large","limit":<bytes>}`. Future attachment support
will use a dedicated chunked upload endpoint rather than enlarging this
limit.
