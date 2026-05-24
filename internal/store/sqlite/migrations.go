package sqlite

import (
	"context"
	"fmt"
)

// schema is the v1 schema for dataagent state. Tables map to the five domain
// types in internal/store (sessions, messages, events, tool_calls,
// permissions). All timestamps are stored as INTEGER unix microseconds — the
// finest resolution we expect to need, and easy to round-trip via time.Time.
//
// events.idx is INTEGER PRIMARY KEY AUTOINCREMENT so the value strictly
// increases per row across the whole database. That property is what makes
// it a safe SSE Last-Event-ID anchor.
//
// Migration policy: this file ships v1. Future versions add migrations under
// `migrationsByVersion` keyed by the target version; migrate() applies each
// one inside a transaction and bumps the schema_version row.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		id          TEXT    PRIMARY KEY,
		parent_id   TEXT    NOT NULL DEFAULT '',
		state       TEXT    NOT NULL,
		workdir     TEXT    NOT NULL,
		env_json    TEXT    NOT NULL DEFAULT '{}',
		agent_type  TEXT    NOT NULL DEFAULT '',
		model       TEXT    NOT NULL DEFAULT '',
		ephemeral   INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL,
		deleted_at  INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_active ON sessions(deleted_at) WHERE deleted_at IS NULL`,

	`CREATE TABLE IF NOT EXISTS messages (
		id           TEXT    PRIMARY KEY,
		session_id   TEXT    NOT NULL,
		role         TEXT    NOT NULL,
		content_json TEXT    NOT NULL,
		token_count  INTEGER NOT NULL DEFAULT 0,
		created_at   INTEGER NOT NULL,
		FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_session_time ON messages(session_id, created_at)`,

	`CREATE TABLE IF NOT EXISTS events (
		idx          INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id   TEXT    NOT NULL,
		type         TEXT    NOT NULL,
		payload_json TEXT    NOT NULL,
		created_at   INTEGER NOT NULL,
		FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_session_idx ON events(session_id, idx)`,

	`CREATE TABLE IF NOT EXISTS tool_calls (
		id          TEXT    PRIMARY KEY,
		session_id  TEXT    NOT NULL,
		message_id  TEXT    NOT NULL,
		tool        TEXT    NOT NULL,
		input_json  TEXT    NOT NULL,
		output_json TEXT    NOT NULL DEFAULT '',
		is_error    INTEGER NOT NULL DEFAULT 0,
		started_at  INTEGER NOT NULL,
		finished_at INTEGER,
		FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id, started_at)`,

	`CREATE TABLE IF NOT EXISTS permissions (
		id         TEXT    PRIMARY KEY,
		session_id TEXT    NOT NULL DEFAULT '',
		tool       TEXT    NOT NULL,
		pattern    TEXT    NOT NULL,
		decision   TEXT    NOT NULL,
		scope      TEXT    NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_permissions_tool ON permissions(tool, scope)`,
	`CREATE INDEX IF NOT EXISTS idx_permissions_session ON permissions(session_id, scope)`,
}

const currentSchemaVersion = 1

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range schema {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: migration step failed: %s\n  %w", stmt, err)
		}
	}

	// Record the schema version we just installed. ON CONFLICT IGNORE lets
	// us re-run migrations safely; future versions will use INSERT OR REPLACE
	// in a dedicated migration block.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_version(version) VALUES (?)`,
		currentSchemaVersion); err != nil {
		return fmt.Errorf("sqlite: record schema version: %w", err)
	}

	return tx.Commit()
}
