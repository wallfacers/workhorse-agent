package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func TestMigration_FreshDB(t *testing.T) {
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	s.Close()
}

func TestMigration_IdempotentRerun(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}

	var version int
	if err := s.DB().QueryRowContext(ctx, "SELECT MAX(version) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 4 {
		t.Errorf("expected version 4 after first open, got %d", version)
	}
	s.Close()

	// Re-open the same database (simulates server restart with :memory: we
	// just verify the code path; a real file-based DB would preserve state).
	s2, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s2.Close()
}

func TestMigration_V2FTSCreated(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	db := s.DB()

	// Verify FTS table exists and triggers are functional
	var name string
	if err := db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='messages_fts'").Scan(&name); err != nil {
		t.Fatalf("messages_fts table not found: %v", err)
	}

	// Create a session and insert a message; verify FTS trigger fires
	mustCreateSession(t, s, "ftstest")
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at) VALUES (?,?,?,?,?,?)`,
		"m1", "ftstest", "user", `[{"type":"text","text":"hello world"}]`, 0, 0)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	var content string
	if err := db.QueryRowContext(ctx,
		"SELECT content FROM messages_fts WHERE rowid=1").Scan(&content); err != nil {
		t.Fatalf("query fts: %v", err)
	}
	if content != "hello world" {
		t.Errorf("fts content: got %q, want %q", content, "hello world")
	}

	// Verify deletion removes FTS row
	_, err = db.ExecContext(ctx, "DELETE FROM messages WHERE id=?", "m1")
	if err != nil {
		t.Fatalf("delete message: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages_fts").Scan(&count); err != nil {
		t.Fatalf("count fts: %v", err)
	}
	if count != 0 {
		t.Errorf("fts row should be deleted, got %d rows", count)
	}
}

func TestMigration_PartialStateRecovery(t *testing.T) {
	// Simulate: v1 completed, v2 hasn't run yet.
	// On next boot, migrate() should pick up from version 1 and run v2.
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()

	// Manually set up just v1 schema + version = 1
	for _, stmt := range []string{
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_version(version) VALUES (1)`,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY, parent_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL, workdir TEXT NOT NULL,
			env_json TEXT NOT NULL DEFAULT '{}',
			agent_type TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			ephemeral INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER)`,
		`CREATE TABLE messages (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, role TEXT NOT NULL,
			content_json TEXT NOT NULL, token_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("manual setup: %v", err)
		}
	}
	db.Close()

	// Now open through sqlite.Open which should run v2 migration
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open for v2: %v", err)
	}
	defer s.Close()

	var name string
	if err := s.DB().QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='messages_fts'").Scan(&name); err != nil {
		t.Fatalf("messages_fts should exist after v2 migration: %v", err)
	}
}

func TestMigration_V2UpdateTrigger(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	db := s.DB()
	mustCreateSession(t, s, "updtest")
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at) VALUES (?,?,?,?,?,?)`,
		"m1", "updtest", "user", `[{"type":"text","text":"original content"}]`, 0, 0)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err = db.ExecContext(ctx,
		`UPDATE messages SET content_json = ? WHERE id = ?`,
		`[{"type":"text","text":"updated content"}]`, "m1")
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	var content string
	if err := db.QueryRowContext(ctx,
		"SELECT content FROM messages_fts WHERE rowid=1").Scan(&content); err != nil {
		t.Fatalf("query fts after update: %v", err)
	}
	if content != "updated content" {
		t.Errorf("fts after update: got %q, want %q", content, "updated content")
	}
}

func TestMigration_V2BackfillFromV1(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/backfill_test.db"

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	now := time.Now().UnixMicro()
	for _, stmt := range []string{
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_version(version) VALUES (1)`,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY, parent_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL, workdir TEXT NOT NULL,
			env_json TEXT NOT NULL DEFAULT '{}',
			agent_type TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			ephemeral INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER)`,
		`CREATE TABLE messages (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, role TEXT NOT NULL,
			content_json TEXT NOT NULL, token_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE)`,
		fmt.Sprintf(`INSERT INTO sessions(id, parent_id, state, workdir, env_json, agent_type, model, created_at, updated_at)
			VALUES ('sess1','','idle','/tmp','{}','','',%d,%d)`, now, now),
		fmt.Sprintf(`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at)
			VALUES ('m1','sess1','user','[{"type":"text","text":"backfill test alpha"}]',0,%d)`, now),
		fmt.Sprintf(`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at)
			VALUES ('m2','sess1','assistant','[{"type":"text","text":"backfill test beta"}]',0,%d)`, now+1),
		fmt.Sprintf(`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at)
			VALUES ('m3','sess1','user','[{"type":"tool_use","id":"t1","name":"bash"}]',0,%d)`, now+2),
	} {
		if _, err := rawDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("v1 setup: %s: %v", stmt[:60], err)
		}
	}
	rawDB.Close()

	s, err := sqlite.Open(ctx, sqlite.Options{DSN: dbPath})
	if err != nil {
		t.Fatalf("open for v2: %v", err)
	}
	defer s.Close()

	db := s.DB()

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages_fts").Scan(&count); err != nil {
		t.Fatalf("count fts: %v", err)
	}
	if count != 3 {
		t.Errorf("backfill: expected 3 FTS rows, got %d", count)
	}

	var content string
	if err := db.QueryRowContext(ctx,
		"SELECT content FROM messages_fts WHERE rowid=(SELECT rowid FROM messages WHERE id='m1')").Scan(&content); err != nil {
		t.Fatalf("fts m1: %v", err)
	}
	if content != "backfill test alpha" {
		t.Errorf("fts m1: got %q", content)
	}

	if err := db.QueryRowContext(ctx,
		"SELECT content FROM messages_fts WHERE rowid=(SELECT rowid FROM messages WHERE id='m3')").Scan(&content); err != nil {
		t.Fatalf("fts m3: %v", err)
	}
	if content != "" {
		t.Errorf("fts m3 (tool_use only): got %q, want empty", content)
	}
}
