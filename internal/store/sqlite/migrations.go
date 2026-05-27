package sqlite

import (
	"context"
	"fmt"
	"log/slog"
)

// Migration represents a single schema migration step. Up contains the
// statements applied in order inside a single transaction; Down contains
// statements to reverse the migration (applied in reverse order).
type Migration struct {
	Version int
	Up      []string
	Down    []string
}

// v1Schema is the initial schema. Tables map to the five domain types in
// internal/store (sessions, messages, events, tool_calls, permissions). All
// timestamps are stored as INTEGER unix microseconds — the finest resolution
// we expect to need, and easy to round-trip via time.Time.
//
// events.idx is INTEGER PRIMARY KEY AUTOINCREMENT so the value strictly
// increases per row across the whole database. That property is what makes
// it a safe SSE Last-Event-ID anchor.
var v1Schema = []string{
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

// v2MemoryFTS creates the FTS5 virtual table over messages.content_json
// with triggers to keep it in sync, and backfills existing rows.
var v2MemoryFTS = []string{
	`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
		content,
		tokenize='trigram'
	)`,
	`CREATE TRIGGER IF NOT EXISTS messages_fts_ai AFTER INSERT ON messages BEGIN
		INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, extract_text(new.content_json));
	END`,
	`CREATE TRIGGER IF NOT EXISTS messages_fts_ad AFTER DELETE ON messages BEGIN
		DELETE FROM messages_fts WHERE rowid = old.rowid;
	END`,
	`CREATE TRIGGER IF NOT EXISTS messages_fts_au AFTER UPDATE ON messages BEGIN
		DELETE FROM messages_fts WHERE rowid = old.rowid;
		INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, extract_text(new.content_json));
	END`,
}

// v2Backfill populates messages_fts from existing messages rows.
var v2Backfill = []string{
	`INSERT INTO messages_fts(rowid, content) SELECT rowid, extract_text(content_json) FROM messages`,
}

// v2MemoryFTSDown reverses the v2 migration.
var v2MemoryFTSDown = []string{
	`DROP TRIGGER IF EXISTS messages_fts_au`,
	`DROP TRIGGER IF EXISTS messages_fts_ad`,
	`DROP TRIGGER IF EXISTS messages_fts_ai`,
	`DROP TABLE IF EXISTS messages_fts`,
}

// migrationsByVersion is the ordered list of all migrations. Each entry is
// applied inside its own transaction; schema_version is bumped per step.
var migrationsByVersion = []Migration{
	{Version: 1, Up: v1Schema, Down: nil},
	{Version: 2, Up: append(v2MemoryFTS, v2Backfill...), Down: v2MemoryFTSDown},
}

func (s *Store) migrate(ctx context.Context) error {
	// Apply each migration in version order, each in its own transaction.
	for _, m := range migrationsByVersion {
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration checks whether migration m has already been applied and, if
// not, executes its Up statements inside a single transaction, then bumps
// schema_version.
func (s *Store) applyMigration(ctx context.Context, m Migration) error {
	current, err := s.readSchemaVersion(ctx)
	if err != nil {
		return err
	}
	if current >= m.Version {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin migration v%d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, stmt := range m.Up {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: migration v%d step %d failed: %s\n  %w", m.Version, i+1, truncateStmt(stmt), err)
		}
	}

	if m.Version == 2 {
		slog.Info("sqlite: migration v2 backfill complete")
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO schema_version(version) VALUES (?)`, m.Version); err != nil {
		return fmt.Errorf("sqlite: record schema version v%d: %w", m.Version, err)
	}

	return tx.Commit()
}

// readSchemaVersion returns the current schema version, or 0 if the table
// does not yet exist (fresh database) or is empty.
func (s *Store) readSchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_version`).Scan(&version)
	if err != nil {
		// MAX on an empty table returns NULL → sql.Scan to int fails.
		// A missing table means v1 hasn't run yet — return 0 so it runs.
		return 0, nil
	}
	return version, nil
}

func truncateStmt(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
