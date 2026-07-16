package sqlite

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

// v3SessionTitle adds the per-session display title (derived from the first
// user message, renamable via PATCH) and an index over workdir so the
// project-scoped session listing (add-project-sessions) stays cheap.
var v3SessionTitle = []string{
	`ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_workdir ON sessions(workdir) WHERE deleted_at IS NULL`,
}

var v3SessionTitleDown = []string{
	`DROP INDEX IF EXISTS idx_sessions_workdir`,
	// SQLite cannot DROP COLUMN before 3.35; the column is left in place on
	// downgrade. The schema_version bump is what gates re-application.
}

// v4ProviderAndStopReason persists the provider name and per-message stop_reason
// so that hydrated sessions restore accurate turn-boundary metadata and provider
// identity without relying on heuristics or defaults.
var v4ProviderAndStopReason = []string{
	`ALTER TABLE sessions ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE messages ADD COLUMN stop_reason TEXT NOT NULL DEFAULT ''`,
}

// v5InterruptedFlag persists the interrupted state on assistant messages so that
// rehydrated chat history preserves the "(已中断)" marker across session switches
// and server restarts.
var v5InterruptedFlag = []string{
	`ALTER TABLE messages ADD COLUMN interrupted INTEGER NOT NULL DEFAULT 0`,
}

// v6SessionCustomization carries the per-session instructions text and the
// caller-supplied opaque metadata map (support-dataweave-headless-integration).
var v6SessionCustomization = []string{
	`ALTER TABLE sessions ADD COLUMN instructions TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE sessions ADD COLUMN metadata_json TEXT NOT NULL DEFAULT ''`,
}

// v7Memory creates the per-entry memory store (redesign-memory-layered-curation
// D1/D6): the memory_entries table, its FTS5 mirror with sync triggers, and the
// single-row curation leader-lease table.
//
// Unlike messages_fts (which extracts text out of a JSON column), the memory FTS
// columns (name, trigger, content) are plain text, so the triggers index them
// directly with no extract_text() call. All timestamps are INTEGER unix
// microseconds, consistent with the rest of the schema.
var v7Memory = []string{
	`CREATE TABLE IF NOT EXISTS memory_entries (
		id                TEXT    PRIMARY KEY,
		name              TEXT    NOT NULL UNIQUE,
		trigger           TEXT    NOT NULL DEFAULT '',
		content           TEXT    NOT NULL DEFAULT '',
		pinned            INTEGER NOT NULL DEFAULT 0,
		durability        TEXT    NOT NULL DEFAULT 'volatile',
		category          TEXT    NOT NULL DEFAULT '',
		hit_count         INTEGER NOT NULL DEFAULT 0,
		last_used_at      INTEGER,
		created_at        INTEGER NOT NULL,
		updated_at        INTEGER NOT NULL,
		char_count        INTEGER NOT NULL DEFAULT 0,
		source_session_id TEXT    NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_memory_pinned ON memory_entries(pinned)`,

	`CREATE VIRTUAL TABLE IF NOT EXISTS memory_entries_fts USING fts5(
		name,
		trigger,
		content,
		tokenize='trigram'
	)`,
	`CREATE TRIGGER IF NOT EXISTS memory_entries_fts_ai AFTER INSERT ON memory_entries BEGIN
		INSERT INTO memory_entries_fts(rowid, name, trigger, content)
		VALUES (new.rowid, new.name, new.trigger, new.content);
	END`,
	`CREATE TRIGGER IF NOT EXISTS memory_entries_fts_ad AFTER DELETE ON memory_entries BEGIN
		DELETE FROM memory_entries_fts WHERE rowid = old.rowid;
	END`,
	`CREATE TRIGGER IF NOT EXISTS memory_entries_fts_au AFTER UPDATE ON memory_entries BEGIN
		DELETE FROM memory_entries_fts WHERE rowid = old.rowid;
		INSERT INTO memory_entries_fts(rowid, name, trigger, content)
		VALUES (new.rowid, new.name, new.trigger, new.content);
	END`,

	`CREATE TABLE IF NOT EXISTS memory_curation_lease (
		id           INTEGER PRIMARY KEY CHECK (id = 1),
		holder       TEXT    NOT NULL DEFAULT '',
		expires_at   INTEGER NOT NULL DEFAULT 0,
		heartbeat_at INTEGER NOT NULL DEFAULT 0
	)`,
	`INSERT OR IGNORE INTO memory_curation_lease(id, holder, expires_at, heartbeat_at)
		VALUES (1, '', 0, 0)`,
}

// v7MemoryDown reverses the v7 migration. Order is safe: drop the triggers and
// FTS mirror before the base table, then the standalone lease table.
var v7MemoryDown = []string{
	`DROP TRIGGER IF EXISTS memory_entries_fts_au`,
	`DROP TRIGGER IF EXISTS memory_entries_fts_ad`,
	`DROP TRIGGER IF EXISTS memory_entries_fts_ai`,
	`DROP TABLE IF EXISTS memory_entries_fts`,
	`DROP TABLE IF EXISTS memory_curation_lease`,
	`DROP TABLE IF EXISTS memory_entries`,
}

// v8MemoryHybrid extends the memory store for hybrid retrieval
// (memory-hybrid-retrieval-locomo). It adds provenance/temporal columns to
// memory_entries and two side tables kept out of the FTS-mirrored base table:
// memory_embeddings (one float32 vector BLOB per entry, rebuildable on model
// change) and memory_entities (normalized entity -> entry index for the
// entity-match retrieval signal). All timestamps remain INTEGER unix micros.
//
// event_date is nullable: the unix-micros instant the remembered fact occurred
// (distinct from created_at, when it was recorded). fact_source records
// provenance ('' | user | agent | extraction).
var v8MemoryHybrid = []string{
	`ALTER TABLE memory_entries ADD COLUMN event_date INTEGER`,
	`ALTER TABLE memory_entries ADD COLUMN fact_source TEXT NOT NULL DEFAULT ''`,

	`CREATE TABLE IF NOT EXISTS memory_embeddings (
		entry_name TEXT    PRIMARY KEY,
		model      TEXT    NOT NULL DEFAULT '',
		dims       INTEGER NOT NULL DEFAULT 0,
		vec        BLOB    NOT NULL,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS memory_entities (
		entry_name  TEXT NOT NULL,
		entity_norm TEXT NOT NULL,
		entity_raw  TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (entry_name, entity_norm)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_memory_entities_norm ON memory_entities(entity_norm)`,
}

// v8MemoryHybridDown reverses v8. SQLite (modernc) supports DROP COLUMN, so the
// added columns are removed after the side tables.
var v8MemoryHybridDown = []string{
	`DROP INDEX IF EXISTS idx_memory_entities_norm`,
	`DROP TABLE IF EXISTS memory_entities`,
	`DROP TABLE IF EXISTS memory_embeddings`,
	`ALTER TABLE memory_entries DROP COLUMN fact_source`,
	`ALTER TABLE memory_entries DROP COLUMN event_date`,
}

// migrationsByVersion is the ordered list of all migrations. Each entry is
// applied inside its own transaction; schema_version is bumped per step.
var migrationsByVersion = []Migration{
	{Version: 1, Up: v1Schema, Down: nil},
	{Version: 2, Up: append(v2MemoryFTS, v2Backfill...), Down: v2MemoryFTSDown},
	{Version: 3, Up: v3SessionTitle, Down: v3SessionTitleDown},
	{Version: 4, Up: v4ProviderAndStopReason, Down: nil},
	{Version: 5, Up: v5InterruptedFlag, Down: nil},
	{Version: 6, Up: v6SessionCustomization, Down: nil},
	{Version: 7, Up: v7Memory, Down: v7MemoryDown},
	{Version: 8, Up: v8MemoryHybrid, Down: v8MemoryHybridDown},
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
	var version *int
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_version`).Scan(&version)
	if err != nil {
		// Fresh database: schema_version table doesn't exist yet, or
		// table exists but is empty (NULL → Scan to *int yields nil value, no error).
		// Any other error (corruption, I/O) should propagate.
		if isTableNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}

func truncateStmt(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func isTableNotExist(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no such table")
}
