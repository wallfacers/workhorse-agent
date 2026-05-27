package sessionsearch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/sessionsearch"
)

func setupDB(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCreateSession(t *testing.T, s *sqlite.Store, id, parentID string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO sessions(id, parent_id, state, workdir, env_json, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		id, parentID, "idle", "/tmp", "{}", now.UnixMicro(), now.UnixMicro())
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func mustInsertMessage(t *testing.T, db *sql.DB, id, sessionID, role, contentJSON string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at) VALUES (?,?,?,?,?,?)`,
		id, sessionID, role, contentJSON, 0, time.Now().UnixMicro())
	if err != nil {
		t.Fatalf("insert message %s: %v", id, err)
	}
}

func TestCJK_Classification(t *testing.T) {
	tests := []struct {
		name string
		r    rune
		want bool
	}{
		{"CJK basic", 0x4E00, true},
		{"CJK end", 0x9FFF, true},
		{"Ext A start", 0x3400, true},
		{"Hiragana", 0x3041, true},
		{"Katakana", 0x30A1, true},
		{"Hangul", 0xAC00, true},
		{"Jamo", 0x1100, true},
		{"ASCII a", 'a', false},
		{"digit 5", '5', false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionsearch.IsCJK(tt.r); got != tt.want {
				t.Errorf("isCJK(%U) = %v, want %v", tt.r, got, tt.want)
			}
		})
	}
}

func TestTokenizer_Basic(t *testing.T) {
	runs := sessionsearch.Tokenize("hello world")
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	if runs[0].Kind != "ascii" || runs[0].Text != "hello" {
		t.Errorf("run[0]: %+v", runs[0])
	}
	if runs[1].Kind != "ws" {
		t.Errorf("run[1]: %+v", runs[1])
	}
	if runs[2].Kind != "ascii" || runs[2].Text != "world" {
		t.Errorf("run[2]: %+v", runs[2])
	}
}

func TestTrigrams(t *testing.T) {
	got := sessionsearch.Trigrams("数据库迁移")
	want := []string{"数据库", "据库迁", "库迁移"}
	if len(got) != len(want) {
		t.Fatalf("trigrams: got %v, want %v", got, want)
	}
	for i, g := range got {
		if g != want[i] {
			t.Errorf("trigram[%d]: got %q, want %q", i, g, want[i])
		}
	}
}

func TestBuildPlan_PureASCII(t *testing.T) {
	expr, ok := sessionsearch.BuildPlan("hello world")
	if !ok {
		t.Fatal("expected ok=true for pure ASCII")
	}
	if expr != "hello AND world" {
		t.Errorf("got %q", expr)
	}
}

func TestBuildPlan_LongCJK(t *testing.T) {
	expr, ok := sessionsearch.BuildPlan("数据库迁移")
	if !ok {
		t.Fatal("expected ok=true for long CJK")
	}
	if expr == "" {
		t.Error("expected non-empty expression")
	}
}

func TestBuildPlan_ShortCJK_Fallback(t *testing.T) {
	_, ok := sessionsearch.BuildPlan("迁移")
	if ok {
		t.Error("short CJK should trigger fallback")
	}
}

func TestBuildPlan_MixedASCIIShortCJK_Fallback(t *testing.T) {
	_, ok := sessionsearch.BuildPlan("sqlite 迁移")
	if ok {
		t.Error("mixed ASCII + short CJK should fallback")
	}
}

func TestSearch_ASCIIQuery(t *testing.T) {
	s := setupDB(t)
	db := s.DB()
	mustCreateSession(t, s, "sess1", "")
	mustInsertMessage(t, db, "m1", "sess1", "user", `[{"type":"text","text":"hello world from test"}]`)

	tool := &sessionsearch.Tool{DB: db}
	res, err := tool.Run(context.Background(), &tools.Env{},
		[]byte(`{"query":"hello","session_id":"sess1"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}

	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	hits := out["hits"].([]any)
	if len(hits) == 0 {
		t.Error("expected at least one hit")
	}
}

func TestSearch_SoftDeletedExcluded(t *testing.T) {
	s := setupDB(t)
	db := s.DB()
	mustCreateSession(t, s, "alive", "")
	mustCreateSession(t, s, "dead", "")
	mustInsertMessage(t, db, "m1", "alive", "user", `[{"type":"text","text":"findme"}]`)
	mustInsertMessage(t, db, "m2", "dead", "user", `[{"type":"text","text":"findme hidden"}]`)

	now := time.Now().UTC()
	db.ExecContext(context.Background(),
		"UPDATE sessions SET deleted_at = ? WHERE id = ?", now.UnixMicro(), "dead")

	tool := &sessionsearch.Tool{DB: db}
	res, _ := tool.Run(context.Background(), &tools.Env{},
		[]byte(`{"query":"findme","session_id":"alive","scope":"all"}`))

	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	hits := out["hits"].([]any)
	for _, h := range hits {
		hm := h.(map[string]any)
		if hm["session_id"] == "dead" {
			t.Error("soft-deleted session should not appear in results")
		}
	}
}

func TestSearch_TruncatedFlag(t *testing.T) {
	s := setupDB(t)
	db := s.DB()
	mustCreateSession(t, s, "sess1", "")

	for i := 0; i < 15; i++ {
		mustInsertMessage(t, db, fmt.Sprintf("m%d", i), "sess1", "user",
			`[{"type":"text","text":"uniqueKeyword find"}]`)
	}

	tool := &sessionsearch.Tool{DB: db}
	res, _ := tool.Run(context.Background(), &tools.Env{},
		[]byte(`{"query":"uniqueKeyword","session_id":"sess1","limit":10}`))

	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	if out["truncated"] != true {
		t.Error("expected truncated=true")
	}
}
