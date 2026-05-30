package tasklist_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/tasklist"
)

func run(t *testing.T, store *tasklist.Store, body string) *tools.Result {
	t.Helper()
	res, err := tasklist.TodoWrite{}.Run(context.Background(), &tools.Env{TaskList: store}, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	return res
}

func decodeTasks(t *testing.T, res *tools.Result) []tasklist.Task {
	t.Helper()
	var out struct {
		Tasks []tasklist.Task `json:"tasks"`
		Count int             `json:"count"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("decode output %q: %v", res.Output, err)
	}
	return out.Tasks
}

// Scenario: 创建任务 — a submitted task lands as pending with an id.
func TestTodoWrite_Create(t *testing.T) {
	store := tasklist.NewStore(nil)
	res := run(t, store, `{"tasks":[{"subject":"Add retry to client"}]}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	got := decodeTasks(t, res)
	if len(got) != 1 || got[0].ID != 1 || got[0].Subject != "Add retry to client" || got[0].Status != tasklist.StatusPending {
		t.Fatalf("unexpected task: %+v", got)
	}
	if snap := store.Snapshot(); len(snap) != 1 || snap[0].Status != tasklist.StatusPending {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
}

// Scenario: 状态流转 — pending -> in_progress -> completed reflected each call.
func TestTodoWrite_StateMachine(t *testing.T) {
	store := tasklist.NewStore(nil)
	run(t, store, `{"tasks":[{"subject":"Build feature","status":"pending"}]}`)
	run(t, store, `{"tasks":[{"subject":"Build feature","status":"in_progress","active_form":"Building feature"}]}`)
	res := run(t, store, `{"tasks":[{"subject":"Build feature","status":"completed"}]}`)
	got := decodeTasks(t, res)
	if len(got) != 1 || got[0].Status != tasklist.StatusCompleted {
		t.Fatalf("expected completed, got %+v", got)
	}
	if snap := store.Snapshot(); snap[0].Status != tasklist.StatusCompleted {
		t.Fatalf("snapshot not completed: %+v", snap)
	}
}

// Scenario: 非法状态被拒绝 — invalid status is rejected and the list is unchanged.
func TestTodoWrite_RejectInvalidStatus(t *testing.T) {
	store := tasklist.NewStore(nil)
	run(t, store, `{"tasks":[{"subject":"Keep me","status":"in_progress"}]}`)
	res := run(t, store, `{"tasks":[{"subject":"Keep me","status":"done"}]}`)
	if !res.IsError {
		t.Fatalf("expected is_error for undefined status, got %s", res.Output)
	}
	snap := store.Snapshot()
	if len(snap) != 1 || snap[0].Status != tasklist.StatusInProgress {
		t.Fatalf("rejected submission must leave prior list intact, got %+v", snap)
	}
}

func TestTodoWrite_RejectEmptySubject(t *testing.T) {
	store := tasklist.NewStore(nil)
	res := run(t, store, `{"tasks":[{"subject":""}]}`)
	if !res.IsError {
		t.Fatalf("expected is_error for empty subject, got %s", res.Output)
	}
	if snap := store.Snapshot(); len(snap) != 0 {
		t.Fatalf("store should stay empty, got %+v", snap)
	}
}

// Whole-table overwrite: a later submission replaces the entire list.
func TestTodoWrite_WholeTableOverwrite(t *testing.T) {
	store := tasklist.NewStore(nil)
	run(t, store, `{"tasks":[{"subject":"A"},{"subject":"B"},{"subject":"C"}]}`)
	run(t, store, `{"tasks":[{"subject":"Only one"}]}`)
	snap := store.Snapshot()
	if len(snap) != 1 || snap[0].Subject != "Only one" || snap[0].ID != 1 {
		t.Fatalf("expected full replacement, got %+v", snap)
	}
}

func TestTodoWrite_Unavailable(t *testing.T) {
	res, err := tasklist.TodoWrite{}.Run(context.Background(), &tools.Env{}, json.RawMessage(`{"tasks":[]}`))
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected is_error when no store bound, got %s", res.Output)
	}
}

// Scenario: 状态变更广播 — the emit hook fires with the latest snapshot.
func TestTodoWrite_BroadcastsOnChange(t *testing.T) {
	var mu sync.Mutex
	var calls [][]tasklist.Task
	store := tasklist.NewStore(func(_ context.Context, tasks []tasklist.Task) {
		mu.Lock()
		calls = append(calls, tasks)
		mu.Unlock()
	})
	run(t, store, `{"tasks":[{"subject":"X","status":"pending"}]}`)
	run(t, store, `{"tasks":[{"subject":"X","status":"in_progress"}]}`)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected 2 broadcasts, got %d", len(calls))
	}
	if calls[1][0].Status != tasklist.StatusInProgress {
		t.Fatalf("last broadcast should carry in_progress, got %+v", calls[1])
	}
}

// A rejected submission must NOT broadcast (list unchanged).
func TestTodoWrite_NoBroadcastOnReject(t *testing.T) {
	n := 0
	store := tasklist.NewStore(func(_ context.Context, _ []tasklist.Task) { n++ })
	run(t, store, `{"tasks":[{"subject":"ok"}]}`)
	run(t, store, `{"tasks":[{"subject":"bad","status":"nope"}]}`)
	if n != 1 {
		t.Fatalf("expected exactly 1 broadcast (reject must not emit), got %d", n)
	}
}

// Scenario: 会话隔离 — each session owns an independent store.
func TestTodoWrite_SessionIsolation(t *testing.T) {
	a := tasklist.NewStore(nil)
	b := tasklist.NewStore(nil)
	run(t, a, `{"tasks":[{"subject":"A-only"}]}`)
	run(t, b, `{"tasks":[{"subject":"B-1"},{"subject":"B-2"}]}`)
	if sa := a.Snapshot(); len(sa) != 1 || sa[0].Subject != "A-only" {
		t.Fatalf("store A leaked: %+v", sa)
	}
	if sb := b.Snapshot(); len(sb) != 2 {
		t.Fatalf("store B leaked: %+v", sb)
	}
}

// Snapshot returns a copy — mutating it must not affect the store.
func TestStore_SnapshotIsCopy(t *testing.T) {
	store := tasklist.NewStore(nil)
	run(t, store, `{"tasks":[{"subject":"orig"}]}`)
	snap := store.Snapshot()
	snap[0].Subject = "mutated"
	if store.Snapshot()[0].Subject != "orig" {
		t.Fatal("Snapshot must return a defensive copy")
	}
}

// Scenario: 工具受 AllowedTools 门控 — when AllowedTools excludes TodoWrite its
// schema is filtered out of the LLM-facing set; when included, it is exposed.
// (Registry.Filtered is the single gating path shared with memory_*/session_search.)
func TestTodoWrite_AllowedToolsGating(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(tasklist.TodoWrite{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	has := func(list []tools.Tool) bool {
		for _, x := range list {
			if x.Name() == "TodoWrite" {
				return true
			}
		}
		return false
	}
	if has(reg.Filtered([]string{"Read", "Bash"})) {
		t.Error("TodoWrite must be hidden when not in AllowedTools")
	}
	if !has(reg.Filtered([]string{"Read", "TodoWrite"})) {
		t.Error("TodoWrite must be exposed when listed in AllowedTools")
	}
	if !has(reg.Filtered(nil)) {
		t.Error("empty AllowedTools means all tools, TodoWrite included")
	}
}
