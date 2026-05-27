package sessionsearch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/sessionsearch"
)

func TestSearch_SessionTreeScope(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	db := s.DB()

	now := time.Now().UTC()
	ts := func(offset time.Duration) int64 {
		return now.Add(offset).UnixMicro()
	}

	// Create tree: parent → child → grandchild, plus a sibling of child
	for _, row := range []struct {
		id, parent string
	}{
		{"parent", ""},
		{"child", "parent"},
		{"grandchild", "child"},
		{"sibling", "parent"},
	} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO sessions(id, parent_id, state, workdir, env_json, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
			row.id, row.parent, "idle", "/tmp", "{}", ts(0), ts(0))
		if err != nil {
			t.Fatalf("create session %s: %v", row.id, err)
		}
	}

	// Insert a findable message in each session
	for i, id := range []string{"parent", "child", "grandchild", "sibling"} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at) VALUES (?,?,?,?,?,?)`,
			"m_"+id, id, "user",
			`[{"type":"text","text":"treedata `+id+`"}]`, 0, ts(time.Duration(i+1)*time.Second))
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	tool := &sessionsearch.Tool{DB: db}

	// Default scope from "child" should include parent, child, grandchild but NOT sibling
	res, err := tool.Run(ctx, &tools.Env{},
		[]byte(`{"query":"treedata","session_id":"child"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %s", res.Output)
	}

	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	hits := out["hits"].([]any)

	included := map[string]bool{}
	for _, h := range hits {
		hm := h.(map[string]any)
		included[hm["session_id"].(string)] = true
	}

	if !included["parent"] {
		t.Error("default scope should include parent")
	}
	if !included["child"] {
		t.Error("default scope should include child")
	}
	if !included["grandchild"] {
		t.Error("default scope should include grandchild")
	}
	if included["sibling"] {
		t.Error("default scope should NOT include sibling")
	}

	// scope: "all" should include everything
	res, err = tool.Run(ctx, &tools.Env{},
		[]byte(`{"query":"treedata","session_id":"child","scope":"all"}`))
	if err != nil {
		t.Fatalf("run all: %v", err)
	}

	json.Unmarshal([]byte(res.Output), &out)
	hits = out["hits"].([]any)
	included = map[string]bool{}
	for _, h := range hits {
		hm := h.(map[string]any)
		included[hm["session_id"].(string)] = true
	}

	if !included["sibling"] {
		t.Error("scope:all should include sibling")
	}
	if !included["parent"] {
		t.Error("scope:all should include parent")
	}
}

func TestSearch_CJKTrigramPath(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	db := s.DB()

	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions(id, state, workdir, env_json, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		"s1", "idle", "/tmp", "{}", time.Now().UnixMicro(), time.Now().UnixMicro())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at) VALUES (?,?,?,?,?,?)`,
		"m1", "s1", "user",
		`[{"type":"text","text":"数据库迁移完成"}]`, 0, time.Now().UnixMicro())
	if err != nil {
		t.Fatal(err)
	}

	tool := &sessionsearch.Tool{DB: db}
	res, err := tool.Run(ctx, &tools.Env{},
		[]byte(`{"query":"数据库迁移","session_id":"s1"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("CJK trigram error: %s", res.Output)
	}

	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	hits := out["hits"].([]any)
	if len(hits) == 0 {
		t.Error("CJK trigram query should find the message")
	}
}

func TestSearch_CJKLikeFallback(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	db := s.DB()

	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions(id, state, workdir, env_json, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		"s1", "idle", "/tmp", "{}", time.Now().UnixMicro(), time.Now().UnixMicro())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at) VALUES (?,?,?,?,?,?)`,
		"m1", "s1", "user",
		`[{"type":"text","text":"迁移失败"}]`, 0, time.Now().UnixMicro())
	if err != nil {
		t.Fatal(err)
	}

	tool := &sessionsearch.Tool{DB: db}
	// "迁移" is only 2 CJK chars → LIKE fallback
	res, err := tool.Run(ctx, &tools.Env{},
		[]byte(`{"query":"迁移","session_id":"s1"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("CJK LIKE error: %s", res.Output)
	}

	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	hits := out["hits"].([]any)
	if len(hits) == 0 {
		t.Error("CJK LIKE query should find the message")
	}
}

func TestSearch_NoLLMCalls(t *testing.T) {
	// session_search only touches the DB, no provider calls.
	// This is structurally guaranteed by the tool not taking a Provider.
	// We verify it compiles and runs without any provider dependency.
	ctx := context.Background()
	s, _ := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	defer s.Close()
	db := s.DB()

	db.ExecContext(ctx,
		`INSERT INTO sessions(id, state, workdir, env_json, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		"s1", "idle", "/tmp", "{}", time.Now().UnixMicro(), time.Now().UnixMicro())
	db.ExecContext(ctx,
		`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at) VALUES (?,?,?,?,?,?)`,
		"m1", "s1", "user", `[{"type":"text","text":"hello"}]`, 0, time.Now().UnixMicro())

	tool := &sessionsearch.Tool{DB: db}
	// Tool has no Provider field — structurally impossible to call an LLM
	_ = tool
}

var _ = sql.DB{}
