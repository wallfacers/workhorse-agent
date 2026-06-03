## Why

The `interrupted` flag on assistant messages is currently a client-side-only concept: the SSE `interrupted` event sets it in React runtime memory, but when a session is evicted and later rehydrated via `GET /v1/sessions/{id}/history`, the flag is lost. Users see interrupted messages disappear after switching projects or restarting the app. The workhorse-assistant `sse-event-display` spec already requires "The interrupted state SHALL persist in the message list and across session switches" — this change makes that contract actually work by persisting the flag in the sidecar.

## What Changes

- **Persist `interrupted` on messages**: Add `interrupted INTEGER NOT NULL DEFAULT 0` column to the `messages` table via v5 migration
- **Store layer**: `store.Message` gains `Interrupted bool`; new `MarkMessageInterrupted()` method sets the flag on the last assistant message for a session
- **Agent loop**: `finishCancelledTurn()` calls `MarkMessageInterrupted()` after emitting the `interrupted` SSE event, so the flag survives restarts
- **History wire type**: `historyMessage` gains `Interrupted bool`; `buildHistory()` reads it from the stored message and includes it in the response
- **Session persistence**: `AppendMessage()` ensures all messages are persisted with `Interrupted: false` by default

## Capabilities

### New Capabilities

None. This is a bug fix that completes the existing `sse-event-display` spec requirement.

### Modified Capabilities

- `api-protocol`: `HistoryMessage` wire type gains an optional `interrupted: boolean` field at the message level (not per-part). Consumers already tolerate missing fields.
- `session-management`: The `interrupted` turn lifecycle step SHALL persist the interrupted flag to the last assistant message in the transcript, so the flag survives session rehydration.

## Impact

- **Store types**: `internal/store/types.go` — `Message` struct gains `Interrupted bool`
- **Store interface**: `internal/store/store.go` — new `MarkMessageInterrupted(ctx, messageID string) error`
- **SQLite migrations**: `internal/store/sqlite/migrations.go` — v5 adds `interrupted` column
- **SQLite CRUD**: `internal/store/sqlite/crud.go` — all Message CRUD updated to read/write `interrupted`; new `MarkMessageInterrupted` implementation (`UPDATE messages SET interrupted=1 WHERE id=?`)
- **Session**: `internal/session/session.go` — `Session` gains `lastAssistantMsgID` field + `LastAssistantMessageID()` getter + `MarkMessageInterrupted()` convenience method; `AppendMessage` records the ID internally when role is assistant
- **Agent loop**: `internal/agent/loop.go` — `finishCancelledTurn` calls `Session.MarkMessageInterrupted()` and includes `message_id` in the `interrupted` SSE event payload
- **API**: `internal/api/sessions.go` — `historyMessage` gains `Interrupted`; `buildHistory` reads and passes it through
- **Wire format**: Response from `GET /v1/sessions/{id}/history` gains `interrupted` field on each message — backward-compatible (clients tolerate missing fields per spec)
- **SSE event**: `interrupted` event payload gains `message_id` field — backward-compatible (consumers ignore unknown fields)
- **Rust bridge**: No changes (`session_history()` returns `Value` verbatim)
- **TypeScript**: No changes (`ChatMessage.interrupted?` already exists; `coerceHistory` is pass-through)
