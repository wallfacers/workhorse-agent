## Context

The `interrupted` state of an assistant message currently lives only in two places:

1. **SSE events table** (`events`): the `interrupted` SSE event is persisted by `EmitNow()` → `assignIdx()` → `store.AppendEvent()`, but events are audit-log entries keyed by `idx` — they are not queryable as message metadata.
2. **Client-side React runtime** (`events.ts`): the SSE listener sets `msg.interrupted = true` in-memory, which is lost on session eviction.

When a session is evicted (project switch, app restart) and later rehydrated via `GET /v1/sessions/{id}/history` → `agentSessionHistory()` → `coerceHistory()`, the `interrupted` flag is absent from the response because `historyMessage` (the Go wire type) has no such field, and `store.Message` (the SQLite row type) has no such column.

This breaks the `sse-event-display` spec requirement: "The interrupted state SHALL persist in the message list and across session switches."

### Current data flow (broken)

```
consumeProviderStream (assistant turn)
  └─ AppendMessage(role=assistant, ...)
       └─ store.Message{ID: idgen.NewULID(), ...}
       └─ ID is NOT returned to caller ✗

finishCancelledTurn()
  ├─ AppendMessage (cancelled tool_results)  → messages table (persisted ✓)
  └─ EmitNow("interrupted", {})              → events table (persisted ✓)
                                             → SSE → events.ts → React state (volatile ✗)

Rehydration:
  GET /v1/sessions/{id}/history
  → store.ListMessages()
  → buildHistory(messages)  — no Interrupted field
  → JSON response            — no interrupted field
  → coerceHistory()          — as ChatMessage[], pass-through
  → ChatMessage.interrupted? — undefined ✗
```

### Target data flow (fixed)

```
consumeProviderStream (assistant turn, loop.go:557)
  └─ AppendMessage(role=assistant, ...)
       └─ Session.lastAssistantMsgID = row.ID  ← 🆕

finishCancelledTurn()
  ├─ AppendMessage (cancelled tool_results)            → messages table
  ├─ msgID = Session.LastAssistantMessageID()          ← 🆕
  ├─ Session.MarkMessageInterrupted(msgID)             ← 🆕 UPDATE messages SET interrupted=1 WHERE id=?
  └─ EmitNow("interrupted", {message_id: msgID})       ← 🆕 payload 含 message_id

Rehydration:
  GET /v1/sessions/{id}/history
  → store.ListMessages()        — reads interrupted column
  → buildHistory(messages)      — includes Interrupted in wire type
  → JSON response               — { messages: [{ ..., interrupted: true }] }
  → coerceHistory()             — pass-through (as ChatMessage[])
  → ChatMessage.interrupted?    — true ✓ (interface 已声明)
```

**TypeScript 零改动原理**：`coerceHistory`（`SessionProvider.tsx:140-145`）是纯 `as` 类型断言透传 — 只要 Go API 返回的 JSON 对象包含 `interrupted` 字段，运行时就会自动映射到 `ChatMessage.interrupted?: boolean`。Rust bridge 只透传 SSE 事件，history 走的是 Go HTTP API 直接返回 `Value`，同样零改动。

## Goals / Non-Goals

**Goals:**
- Persist the `interrupted` flag in the `messages` table so it survives session eviction and server restart
- Include `interrupted` in the `GET /v1/sessions/{id}/history` response
- Make the TS consumer work without changes (pass-through)
- Maintain backward compatibility: clients that don't understand `interrupted` silently ignore it

**Non-Goals:**
- Changing the store event-based interruption semantics (the events table already has the `interrupted` event; this change adds message-level metadata)
- Adding `interrupted` to live SSE events (already works)
- Changing the `ChatMessage` TypeScript type (already has `interrupted?: boolean`)
- Adding any new HTTP endpoints
- Changing the compact/rewrite path (compaction rewrites messages — the new column is included in the INSERT but the flag on rewritten messages stays 0, which is correct because compacted turns are not interrupted)

## Decisions

### D1: `interrupted` at the Message level, not Part level

**Choice**: `store.Message.Interrupted bool` — the flag lives on the entire message, not on individual `parts[]`.

**Why**: Interruption is a turn-level event. The SSE `interrupted` event marks the entire assistant turn, not a specific content block within it. Placing the flag on the message avoids ambiguity about which part to flag (the text part? the reasoning part? the last tool_call?).

**Alternative considered**: Add `interrupted` as a part type (like `{ type: "interrupted" }`). Rejected because: (a) it would change the parts array shape, requiring TS changes to filter/ignore it; (b) it doesn't represent a content block — it's metadata about the message; (c) it complicates the reasoning-end/tool-call-done flow which appends parts sequentially.

### D2: `MarkMessageInterrupted` by message ID, not session ID

**Choice**: Store method `MarkMessageInterrupted(ctx, messageID) error` that sets `interrupted=1` on a specific message by its ULID primary key:

```sql
UPDATE messages SET interrupted=1 WHERE id=?
```

**Why**: ULIDs are globally unique, so no `session_id` needed for disambiguation. A primary-key lookup is the fastest possible SQL operation. The caller (Session/Loop) has the exact message ID in hand (see D6).

**Alternative considered**: `MarkMessageInterrupted(ctx, sessionID)` with `ORDER BY created_at DESC LIMIT 1`. Rejected because: (a) fragile if timestamps collide within the same session; (b) `WHERE id=?` is simpler and faster; (c) with D6, the message ID is available — using a heuristic when you have the exact key is an anti-pattern.

### D3: SQL representation as INTEGER

**Choice**: `interrupted INTEGER NOT NULL DEFAULT 0` (SQLite boolean convention).

**Why**: Consistent with existing columns (`ephemeral`, `is_error`). SQLite has no native boolean type; INTEGER 0/1 is the project convention.

### D4: No Rust bridge or TypeScript changes

**Choice**: The Rust `session_history()` method returns `Value` (serde_json) which passes through the JSON response verbatim. The TS `coerceHistory()` is also a pass-through (`as ChatMessage[]`). The new `interrupted` field in Go's JSON response automatically appears at runtime.

**Why**: `ChatMessage.interrupted?: boolean` already exists in `types.ts`. No deserialization step in either Rust or TS strips unknown fields — the field flows from Go → HTTP JSON → Rust `Value` → Tauri invoke → TS `unknown` → `as ChatMessage[]` untouched. Zero changes on the assistant side.

### D5: Migration strategy

**Choice**: v5 migration with `ALTER TABLE messages ADD COLUMN interrupted INTEGER NOT NULL DEFAULT 0`.

**Why**: SQLite `ALTER TABLE ADD COLUMN` with a `DEFAULT` value is instantaneous (no table rewrite) and backward-compatible — existing rows get `0` (not interrupted), which is correct. Follows the pattern of v4 (`ALTER TABLE messages ADD COLUMN stop_reason`).

### D6: Last assistant message ID tracked on Session

**Choice**: `Session` gains a `lastAssistantMsgID string` field, set inside `AppendMessage` (under the existing mutex) when `m.Role == provider.RoleAssistant`. Exposed via `LastAssistantMessageID() string` getter.

**Why `AppendMessage`** (session.go:481) generates the message ULID internally — callers never see it. Changing `AppendMessage`'s return signature would require updating 6+ call sites across loop.go, most of which don't need the ID. Instead, Session records the most recent assistant message ID internally and exposes it on demand.

**Why not on Loop**: Putting the field on Loop requires every caller of `AppendMessage` to remember to capture the return value. Session-level tracking is automatic — it cannot be forgotten.

```go
// In AppendMessage (session.go), inside the mutex-locked section:
row := &store.Message{ID: idgen.NewULID(), ...}
if m.Role == provider.RoleAssistant {
    s.lastAssistantMsgID = row.ID
}

// Getter:
func (s *Session) LastAssistantMessageID() string {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.lastAssistantMsgID
}
```

**Note**: `consumeProviderStream` appends assistant messages both incrementally (per-block in stream) and as a final batch (line 557). The per-block appends are buffered in `blocks` and flushed once at the end. So `lastAssistantMsgID` reflects the final assistant message — which is the one that should be marked interrupted.

### D7: Messages table first UPDATE — concurrency safety

**Choice**: `MarkMessageInterrupted` is the first UPDATE on the `messages` table. All prior operations are INSERT (`AppendMessage`, `ReplaceMessages`) or DELETE (`ReplaceMessages`, `PurgeSession`).

**Why it's safe**:

1. **Single writer per session**: `Session.mu` serializes all `AppendMessage` and `MarkMessageInterrupted` calls — no two goroutines write to the same session's messages concurrently.
2. **Cancel path is synchronous**: `finishCancelledTurn` runs on the agent loop goroutine. By the time it calls `MarkMessageInterrupted`, all in-flight `AppendMessage` calls from `consumeProviderStream` have returned (the drain-budget wait in `runTurnSafe` ensures this).
3. **Targeted row update**: `UPDATE ... WHERE id=?` affects exactly one row by primary key — no table scan, no range lock, no deadlock risk.

**Alternative considered**: DELETE + re-INSERT to avoid introducing UPDATE. Rejected because: (a) it changes the message ID (ULID), breaking any external references; (b) it's semantically a state mutation, not a replacement; (c) two writes vs one.

### D8: Timing within finishCancelledTurn

**Choice**: `MarkMessageInterrupted` is called AFTER `DrainPendingToolUses` + synthetic `AppendMessage`, BEFORE `ForceTransition(Cancelled)` and `EmitNow("interrupted")`.

```
finishCancelledTurn():
  1. pending = DrainPendingToolUses()
  2. if pending: AppendMessage(synthetic user message with cancelled tool_results)
  3. msgID = LastAssistantMessageID()
  4. if msgID != "": MarkMessageInterrupted(msgID)       ← 🆕
  5. ForceTransition(Cancelled)
  6. EmitNow("interrupted", {message_id: msgID})         ← 🆕 payload
```

**Why**:

1. **Drain first**: Synthetic cancelled tool_results must be appended before marking — the synthetic user message is NOT the message being marked.
2. **Mark before emit**: Persisting the flag BEFORE emitting the SSE event ensures the database is consistent before any client reads history (a client might trigger a rehydration on receiving the SSE event).
3. **Transition last**: State transitions should happen after data mutations — if a crash occurs between step 4 and step 5, the session has `interrupted=1` on the message but is still in its pre-Cancelled state. This is consistent and recoverable (next startup rehydrates correctly, and the state machine will complete the transition).

### D9: Interrupted SSE event payload includes message_id

**Choice**: The `interrupted` SSE event payload (currently `map[string]any{}`) gains a `message_id` field.

**Why**: The cost is one map entry. The benefit is that SSE consumers (including the TS `events.ts` handler) can correlate the event with a specific message without heuristics. Currently `events.ts` falls back to "the last assistant message by index" when `assistantId` is empty — with `message_id`, it could do an exact match. This is not required for the fix but is trivially cheap to include since the ID is already in hand at the emit site.

**Note**: The `message_id` in the `interrupted` event payload is the **STORE_ID** (from `Session.lastAssistantMsgID`), which differs from the `message_id` in `assistant_text_done` (which is a separately generated ULID at loop.go:568). The TS frontend does not use the `interrupted` event's `message_id` for matching — it uses `scratch.assistantId` from `assistant_text_done`. The field is informational for future use.

## Risks / Trade-offs

- **Race condition: cancel while still streaming blocks**: If the agent loop is still appending blocks when cancel fires, `lastAssistantMsgID` might point to a partial message that hasn't received its final `assistant_text_done`/`tool_call_done` block. → **Mitigation**: `finishCancelledTurn()` is called AFTER the drain-budget wait in `runTurnSafe()`. By that point, `consumeProviderStream` has either completed normally or been aborted by ctx cancellation, and all its `AppendMessage` calls have returned. The `lastAssistantMsgID` at this point is the final assistant message of the turn.

- **Compaction wipes interrupted flag**: `ReplaceMessages` rewrites the entire transcript with new message IDs. → **Mitigation**: This is correct behavior. After compaction, the pre-compaction turn is no longer visible; the user sees the compacted summary. The `interrupted` flag only matters for the currently visible turn.

- **Backward compatibility**: Older clients ignore unknown JSON fields. → **No risk**.

## Migration Plan

1. **Deploy**: Ship new binary with v5 migration — applied automatically on startup via `migrate()`
2. **Rollback**: `Down: nil` (like v3, v4). Deploy previous binary — the `interrupted` column remains but is harmless (not read). Or `ALTER TABLE messages DROP COLUMN interrupted` on SQLite ≥ 3.35.
3. **Data integrity**: Existing rows get `interrupted = 0` from DEFAULT — correct, since no pre-existing message was marked interrupted.

## Open Questions

None.
