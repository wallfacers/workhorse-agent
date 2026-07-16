package memorytool_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/memorytool"
)

func testStore(t *testing.T) (*memory.EntryStore, *sql.DB) {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return memory.NewEntryStore(s.DB()), s.DB()
}

func testEnv() *tools.Env { return &tools.Env{SessionID: "sess-1", Workdir: "/tmp"} }

func budgets() memory.Budgets { return memory.DefaultBudgets() }

func run(t *testing.T, tool tools.Tool, in string) map[string]any {
	t.Helper()
	res, err := tool.Run(context.Background(), testEnv(), json.RawMessage(in))
	if err != nil {
		t.Fatalf("%s run: %v", tool.Name(), err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		// Non-JSON output (e.g. LoadMemory returns raw content).
		return map[string]any{"__raw": res.Output, "__is_error": res.IsError}
	}
	out["__is_error"] = res.IsError
	return out
}

// ---- memory_write -----------------------------------------------------------

func TestWrite_UpsertSingle(t *testing.T) {
	es, _ := testStore(t)
	w := &memorytool.Write{Store: es, Budgets: budgets()}

	out := run(t, w, `{"name":"alpha","trigger":"when greeting","content":"hello"}`)
	if out["__is_error"] == true {
		t.Fatalf("unexpected error: %v", out)
	}
	if out["accepted"] != true || out["next_session_effective"] != true {
		t.Fatalf("unexpected response: %v", out)
	}
	if out["char_count"].(float64) != 5 {
		t.Fatalf("char_count = %v, want 5", out["char_count"])
	}

	e, err := es.GetByName(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.Content != "hello" || e.Trigger != "when greeting" || e.Durability != "volatile" {
		t.Fatalf("stored entry mismatch: %+v", e)
	}
}

func TestWrite_RejectsArrayInput(t *testing.T) {
	es, _ := testStore(t)
	w := &memorytool.Write{Store: es, Budgets: budgets()}

	out := run(t, w, `[{"name":"a","content":"x"}]`)
	if out["__is_error"] != true {
		t.Fatalf("expected error for array input, got %v", out)
	}
	if !strings.Contains(out["error"].(string), "single entry") {
		t.Fatalf("expected 'single entry' message, got %q", out["error"])
	}
	if n, _ := es.Count(context.Background()); n != 0 {
		t.Fatalf("store should be empty, has %d", n)
	}
}

func TestWrite_AppendConcatenates(t *testing.T) {
	es, _ := testStore(t)
	w := &memorytool.Write{Store: es, Budgets: budgets()}

	run(t, w, `{"name":"log","content":"line1"}`)
	out := run(t, w, `{"name":"log","content":"line2","mode":"append"}`)
	if out["__is_error"] == true {
		t.Fatalf("append error: %v", out)
	}
	e, _ := es.GetByName(context.Background(), "log")
	if e.Content != "line1\nline2" {
		t.Fatalf("append content = %q, want %q", e.Content, "line1\nline2")
	}
}

func TestWrite_AppendOverLimitRejected(t *testing.T) {
	es, _ := testStore(t)
	b := budgets()
	b.EntryContentChars = 8
	w := &memorytool.Write{Store: es, Budgets: b}

	run(t, w, `{"name":"log","content":"12345"}`) // 5 chars, fits
	out := run(t, w, `{"name":"log","content":"6789","mode":"append"}`)
	if out["__is_error"] != true {
		t.Fatalf("expected over-limit error, got %v", out)
	}
	if out["code"] != "memory_too_large" {
		t.Fatalf("expected code memory_too_large, got %v", out["code"])
	}
	// Existing entry left byte-identical.
	e, _ := es.GetByName(context.Background(), "log")
	if e.Content != "12345" {
		t.Fatalf("entry mutated on rejected append: %q", e.Content)
	}
}

func TestWrite_TriggerNewlineRejected(t *testing.T) {
	es, _ := testStore(t)
	w := &memorytool.Write{Store: es, Budgets: budgets()}

	out := run(t, w, `{"name":"a","trigger":"line1\nline2","content":"x"}`)
	if out["__is_error"] != true || out["code"] != "trigger_invalid" {
		t.Fatalf("expected trigger_invalid, got %v", out)
	}
	if n, _ := es.Count(context.Background()); n != 0 {
		t.Fatalf("store should be unchanged, has %d", n)
	}
}

func TestWrite_PinnedOverBudgetRejected(t *testing.T) {
	es, _ := testStore(t)
	b := budgets()
	b.PinnedChars = 5
	w := &memorytool.Write{Store: es, Budgets: b}

	out := run(t, w, `{"name":"p","content":"123456","pinned":true}`) // 6 > 5
	if out["__is_error"] != true || out["code"] != "pinned_budget_exceeded" {
		t.Fatalf("expected pinned_budget_exceeded, got %v", out)
	}
	if n, _ := es.Count(context.Background()); n != 0 {
		t.Fatalf("store should be unchanged, has %d", n)
	}
}

func TestWrite_InvalidDurabilityRejected(t *testing.T) {
	es, _ := testStore(t)
	w := &memorytool.Write{Store: es, Budgets: budgets()}
	out := run(t, w, `{"name":"a","content":"x","durability":"forever"}`)
	if out["__is_error"] != true {
		t.Fatalf("expected error for bad durability, got %v", out)
	}
}

// ---- memory_read ------------------------------------------------------------

func TestRead_HitReturnsFields(t *testing.T) {
	es, _ := testStore(t)
	must := func(err error) {
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(es.Upsert(context.Background(), &memory.Entry{
		Name: "alpha", Trigger: "trig", Content: "body", Pinned: true,
		Durability: "evergreen", Category: "user", CharCount: 4,
	}))

	r := &memorytool.Read{Store: es}
	out := run(t, r, `{"name":"alpha"}`)
	if out["__is_error"] == true {
		t.Fatalf("read error: %v", out)
	}
	if out["content"] != "body" || out["trigger"] != "trig" || out["pinned"] != true ||
		out["durability"] != "evergreen" || out["category"] != "user" {
		t.Fatalf("read fields mismatch: %v", out)
	}
	if out["hit_count"].(float64) != 0 {
		t.Fatalf("hit_count = %v, want 0", out["hit_count"])
	}
}

func TestRead_DoesNotBumpUsage(t *testing.T) {
	es, _ := testStore(t)
	_ = es.Upsert(context.Background(), &memory.Entry{Name: "a", Content: "x", CharCount: 1})

	r := &memorytool.Read{Store: es}
	run(t, r, `{"name":"a"}`)
	run(t, r, `{"name":"a"}`)

	e, _ := es.GetByName(context.Background(), "a")
	if e.HitCount != 0 || e.LastUsedAt != nil {
		t.Fatalf("memory_read must not bump usage: hit=%d last=%v", e.HitCount, e.LastUsedAt)
	}
}

func TestRead_NotFound(t *testing.T) {
	es, _ := testStore(t)
	r := &memorytool.Read{Store: es}
	out := run(t, r, `{"name":"nope"}`)
	if out["__is_error"] != true || out["code"] != "not_found" {
		t.Fatalf("expected not_found, got %v", out)
	}
}

// ---- LoadMemory -------------------------------------------------------------

func TestLoadMemory_ReturnsContentAndBumps(t *testing.T) {
	es, _ := testStore(t)
	_ = es.Upsert(context.Background(), &memory.Entry{Name: "a", Content: "full body here", CharCount: 14})

	usage := memory.NewUsageLogger(es, 16)
	l := memorytool.NewLoadMemory(es, usage)

	res, err := l.Run(context.Background(), testEnv(), json.RawMessage(`{"name":"a"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "full body here" {
		t.Fatalf("content = %q", res.Output)
	}

	usage.Close() // flush the usage bump
	e, _ := es.GetByName(context.Background(), "a")
	if e.HitCount != 1 {
		t.Fatalf("hit_count = %d, want 1", e.HitCount)
	}
	if e.LastUsedAt == nil {
		t.Fatal("last_used_at should be set after LoadMemory")
	}
}

func TestLoadMemory_NotFound(t *testing.T) {
	es, _ := testStore(t)
	l := memorytool.NewLoadMemory(es, nil)
	out := run(t, l, `{"name":"missing"}`)
	if out["__is_error"] != true || out["code"] != "not_found" {
		t.Fatalf("expected not_found, got %v", out)
	}
}

func TestLoadMemory_NilUsageBestEffort(t *testing.T) {
	es, _ := testStore(t)
	_ = es.Upsert(context.Background(), &memory.Entry{Name: "a", Content: "c", CharCount: 1})
	// A nil usage logger models a failed/absent recorder: the load must still
	// succeed and return content (best-effort usage semantics).
	l := memorytool.NewLoadMemory(es, nil)
	res, err := l.Run(context.Background(), testEnv(), json.RawMessage(`{"name":"a"}`))
	if err != nil || res.IsError || res.Output != "c" {
		t.Fatalf("load with nil usage should still succeed: err=%v res=%+v", err, res)
	}
}

// ---- MemorySearch -----------------------------------------------------------

func seedSearch(t *testing.T, es *memory.EntryStore) {
	t.Helper()
	entries := []*memory.Entry{
		{Name: "deploy", Trigger: "when deploying", Content: "run the deployment pipeline carefully", CharCount: 37},
		{Name: "中文笔记", Trigger: "中文触发", Content: "这是一段关于数据中台的中文记忆内容", CharCount: 17},
		{Name: "golang", Trigger: "writing go code", Content: "prefer table-driven tests and small interfaces", CharCount: 47},
	}
	for _, e := range entries {
		if err := es.Upsert(context.Background(), e); err != nil {
			t.Fatalf("seed %s: %v", e.Name, err)
		}
	}
}

func TestMemorySearch_EnglishMatch(t *testing.T) {
	es, db := testStore(t)
	seedSearch(t, es)
	s := &memorytool.MemorySearch{DB: db}

	out := run(t, s, `{"query":"deployment"}`)
	matches := out["matches"].([]any)
	if len(matches) == 0 {
		t.Fatalf("expected at least one match, got %v", out)
	}
	first := matches[0].(map[string]any)
	if first["name"] != "deploy" {
		t.Fatalf("expected deploy match, got %v", first)
	}
}

func TestMemorySearch_CJKFallback(t *testing.T) {
	es, db := testStore(t)
	seedSearch(t, es)
	s := &memorytool.MemorySearch{DB: db}

	out := run(t, s, `{"query":"数据中台"}`)
	matches := out["matches"].([]any)
	if len(matches) == 0 {
		t.Fatalf("expected CJK match, got %v", out)
	}
	found := false
	for _, m := range matches {
		if m.(map[string]any)["name"] == "中文笔记" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 中文笔记 in matches, got %v", matches)
	}
}

func TestMemorySearch_LimitTruncates(t *testing.T) {
	es, db := testStore(t)
	for i := 0; i < 5; i++ {
		_ = es.Upsert(context.Background(), &memory.Entry{
			Name:      "note" + string(rune('a'+i)),
			Content:   "common shared keyword content",
			CharCount: 29,
		})
	}
	s := &memorytool.MemorySearch{DB: db}
	out := run(t, s, `{"query":"keyword","limit":2}`)
	matches := out["matches"].([]any)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches with limit 2, got %d", len(matches))
	}
	if out["truncated"] != true {
		t.Fatalf("expected truncated=true, got %v", out["truncated"])
	}
}

func TestMemorySearch_DoesNotBump(t *testing.T) {
	es, db := testStore(t)
	seedSearch(t, es)
	s := &memorytool.MemorySearch{DB: db}
	run(t, s, `{"query":"deployment"}`)

	e, _ := es.GetByName(context.Background(), "deploy")
	if e.HitCount != 0 {
		t.Fatalf("MemorySearch must not bump usage, hit=%d", e.HitCount)
	}
}

// ---- memory_delete ----------------------------------------------------------

func TestDelete_RemovesEntryAndFTS(t *testing.T) {
	es, db := testStore(t)
	_ = es.Upsert(context.Background(), &memory.Entry{Name: "gone", Content: "deletable content", CharCount: 17})

	d := &memorytool.Delete{Store: es}
	out := run(t, d, `{"name":"gone"}`)
	if out["__is_error"] == true || out["deleted"] != true {
		t.Fatalf("delete failed: %v", out)
	}

	if _, err := es.GetByName(context.Background(), "gone"); err == nil {
		t.Fatal("entry should be gone")
	}
	// FTS mirror removed by trigger.
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memory_entries_fts WHERE memory_entries_fts MATCH ?`, "deletable").Scan(&n); err != nil {
		t.Fatalf("fts count: %v", err)
	}
	if n != 0 {
		t.Fatalf("fts row should be gone, found %d", n)
	}
}

func TestDelete_NotFound(t *testing.T) {
	es, _ := testStore(t)
	d := &memorytool.Delete{Store: es}
	out := run(t, d, `{"name":"missing"}`)
	if out["__is_error"] != true || out["code"] != "not_found" {
		t.Fatalf("expected not_found, got %v", out)
	}
}

// ---- memory_merge -----------------------------------------------------------

func TestMerge_Atomic(t *testing.T) {
	es, _ := testStore(t)
	_ = es.Upsert(context.Background(), &memory.Entry{Name: "a", Content: "aaa", CharCount: 3})
	_ = es.Upsert(context.Background(), &memory.Entry{Name: "b", Content: "bbb", CharCount: 3})

	m := &memorytool.Merge{Store: es, Budgets: budgets()}
	out := run(t, m, `{"names":["a","b"],"into":{"name":"merged","content":"combined","trigger":"merged trig"}}`)
	if out["__is_error"] == true || out["merged"] != true {
		t.Fatalf("merge failed: %v", out)
	}
	if out["into"] != "merged" {
		t.Fatalf("into = %v", out["into"])
	}

	ctx := context.Background()
	if _, err := es.GetByName(ctx, "a"); err == nil {
		t.Fatal("source a should be gone")
	}
	if _, err := es.GetByName(ctx, "b"); err == nil {
		t.Fatal("source b should be gone")
	}
	merged, err := es.GetByName(ctx, "merged")
	if err != nil || merged.Content != "combined" {
		t.Fatalf("merged entry missing/wrong: %+v err=%v", merged, err)
	}
}

func TestMerge_IntoOverLimitRejected(t *testing.T) {
	es, _ := testStore(t)
	_ = es.Upsert(context.Background(), &memory.Entry{Name: "a", Content: "aaa", CharCount: 3})
	b := budgets()
	b.EntryContentChars = 3
	m := &memorytool.Merge{Store: es, Budgets: b}

	out := run(t, m, `{"names":["a"],"into":{"name":"merged","content":"way too long"}}`)
	if out["__is_error"] != true || out["code"] != "memory_too_large" {
		t.Fatalf("expected memory_too_large, got %v", out)
	}
	// Source untouched.
	if _, err := es.GetByName(context.Background(), "a"); err != nil {
		t.Fatalf("source a should survive a rejected merge: %v", err)
	}
}

func TestMerge_PinnedOverBudgetRejected(t *testing.T) {
	es, _ := testStore(t)
	_ = es.Upsert(context.Background(), &memory.Entry{Name: "a", Content: "aaa", CharCount: 3})
	b := budgets()
	b.PinnedChars = 5
	m := &memorytool.Merge{Store: es, Budgets: b}

	// Merging into a pinned entry whose content exceeds the pinned budget must be
	// rejected exactly like a pinned memory_write — no back door to an over-budget
	// pinned region.
	out := run(t, m, `{"names":["a"],"into":{"name":"merged","content":"too many pinned chars","pinned":true}}`)
	if out["__is_error"] != true || out["code"] != "pinned_budget_exceeded" {
		t.Fatalf("expected pinned_budget_exceeded, got %v", out)
	}
	if _, err := es.GetByName(context.Background(), "a"); err != nil {
		t.Fatalf("source a should survive a rejected merge: %v", err)
	}
}

func TestMerge_RejectsEmptyNames(t *testing.T) {
	es, _ := testStore(t)
	m := &memorytool.Merge{Store: es, Budgets: budgets()}
	out := run(t, m, `{"names":[],"into":{"name":"x","content":"y"}}`)
	if out["__is_error"] != true {
		t.Fatalf("expected error for empty names, got %v", out)
	}
}

func TestMemorySearch_HybridRendersTimestamps(t *testing.T) {
	es, db := testStore(t)
	ctx := context.Background()
	ev := time.Date(2019, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := es.Upsert(ctx, &memory.Entry{
		Name: "moved", Trigger: "origin", Content: "moved from Sweden", CharCount: 17, EventDate: &ev,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	vs := memory.NewVectorStore(db)
	s := &memorytool.MemorySearch{DB: db, Retriever: memory.NewRetriever(es, vs, nil)}

	out := run(t, s, `{"query":"Sweden","top_k":5}`)
	matches := out["matches"].([]any)
	if len(matches) == 0 {
		t.Fatalf("expected a match, got %v", out)
	}
	snip := matches[0].(map[string]any)["snippet"].(string)
	if !strings.Contains(snip, "[event: 2019-05-01]") {
		t.Fatalf("expected event marker in snippet, got %q", snip)
	}
	if !strings.Contains(snip, "[recorded:") {
		t.Fatalf("expected recorded marker in snippet, got %q", snip)
	}
}

func TestMemorySearch_TopKCap(t *testing.T) {
	es, db := testStore(t)
	vs := memory.NewVectorStore(db)
	s := &memorytool.MemorySearch{DB: db, Retriever: memory.NewRetriever(es, vs, nil)}
	out := run(t, s, `{"query":"x","top_k":200}`)
	if out["top_k_capped"] != true {
		t.Fatalf("expected top_k_capped true, got %v", out["top_k_capped"])
	}
}
