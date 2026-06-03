## 1. Store types — add Interrupted to Message

- [x] 1.1 `internal/store/types.go`: Add `Interrupted bool` field to `Message` struct

## 2. Store interface — add MarkMessageInterrupted

- [x] 2.1 `internal/store/store.go`: Add `MarkMessageInterrupted(ctx context.Context, messageID string) error` to `Store` interface

## 3. SQLite migration — v5 add interrupted column

- [x] 3.1 `internal/store/sqlite/migrations.go`: Add v5 migration entry with `ALTER TABLE messages ADD COLUMN interrupted INTEGER NOT NULL DEFAULT 0`
- [x] 3.2 `internal/store/sqlite/migrations.go`: Register v5 in `migrationsByVersion` slice

## 4. SQLite CRUD — read/write interrupted column + MarkMessageInterrupted

- [x] 4.1 `internal/store/sqlite/crud.go` `AppendMessage`: Add `interrupted` to INSERT columns and VALUES
- [x] 4.2 `internal/store/sqlite/crud.go` `ReplaceMessages`: Add `interrupted` to INSERT columns and VALUES
- [x] 4.3 `internal/store/sqlite/crud.go` `ListMessages`: Add `interrupted` to SELECT and Scan
- [x] 4.4 `internal/store/sqlite/crud.go`: Implement `MarkMessageInterrupted()` — `UPDATE messages SET interrupted=1 WHERE id=?`

## 5. Session — track last assistant message ID

- [x] 5.1 `internal/session/session.go`: Add `lastAssistantMsgID string` field to `Session` struct (guarded by `mu`)
- [x] 5.2 `internal/session/session.go` `AppendMessage`: Set `s.lastAssistantMsgID = row.ID` when `m.Role == provider.RoleAssistant` (inside existing mutex-locked section)
- [x] 5.3 `internal/session/session.go`: Add `LastAssistantMessageID() string` getter
- [x] 5.4 `internal/session/session.go`: Add `MarkMessageInterrupted(ctx context.Context) error` method that delegates to `s.store.MarkMessageInterrupted(ctx, s.lastAssistantMsgID)` — no-op when store is nil or Ephemeral or `lastAssistantMsgID` is empty

## 6. Agent loop — call MarkMessageInterrupted in cancel path

- [x] 6.1 `internal/agent/loop.go` `finishCancelledTurn`: After `DrainPendingToolUses` + synthetic `AppendMessage`, call `l.Session.MarkMessageInterrupted(context.Background())`
- [x] 6.2 `internal/agent/loop.go` `finishCancelledTurn`: Include `"message_id"` in `EmitNow("interrupted", ...)` payload (read from `l.Session.LastAssistantMessageID()`)

## 7. History wire type — include interrupted in response

- [x] 7.1 `internal/api/sessions.go` `historyMessage` struct: Add `Interrupted bool \`json:"interrupted,omitempty"\`` field
- [x] 7.2 `internal/api/sessions.go` `buildHistory`: Read `Interrupted` from each `store.Message` and write to `historyMessage`

## 8. Tests — verify end-to-end persistence

- [x] 8.1 `internal/agent/loop_test.go`: Verify that after `finishCancelledTurn()`, the last assistant message in store has `Interrupted: true`
- [x] 8.2 `internal/store/sqlite/sqlite_test.go`: Verify `MarkMessageInterrupted` sets the flag on the correct message by ID
- [x] 8.3 Verify `go test ./...` passes with no regressions
