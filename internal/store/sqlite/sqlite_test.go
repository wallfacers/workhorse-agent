package sqlite_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_AppliesMigrations(t *testing.T) {
	s := newTestStore(t)
	// listing on an empty database should return no rows, not an error.
	sessions, err := s.ListSessions(context.Background(), false)
	if err != nil {
		t.Fatalf("ListSessions on fresh db: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("fresh db should be empty, got %d sessions", len(sessions))
	}
}

func TestSession_CRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	sess := &store.Session{
		ID:        "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		State:     store.SessionStateIdle,
		Workdir:   "/tmp/x",
		EnvJSON:   `{"FOO":"bar"}`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.State != store.SessionStateIdle || got.Workdir != "/tmp/x" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("created_at: got %v want %v", got.CreatedAt, now)
	}

	// Update
	sess.State = store.SessionStateThinking
	sess.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateSession(ctx, sess); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	got, _ = s.GetSession(ctx, sess.ID)
	if got.State != store.SessionStateThinking {
		t.Errorf("update did not stick: %v", got.State)
	}

	// Soft delete
	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	got, _ = s.GetSession(ctx, sess.ID)
	if got.DeletedAt == nil {
		t.Error("DeletedAt should be set after soft delete")
	}

	// Excluded from default listing
	active, _ := s.ListSessions(ctx, false)
	if len(active) != 0 {
		t.Errorf("deleted session leaked into active list: %d", len(active))
	}
	all, _ := s.ListSessions(ctx, true)
	if len(all) != 1 {
		t.Errorf("includeDeleted list should be 1, got %d", len(all))
	}

	// Idempotent re-delete
	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Errorf("re-delete should be idempotent, got %v", err)
	}

	// Missing id
	_, err = s.GetSession(ctx, "missing-id")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing id, got %v", err)
	}
}

func TestCountActiveSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()
	for i, id := range []string{"a", "b", "c"} {
		sess := &store.Session{
			ID:        id,
			State:     store.SessionStateIdle,
			Workdir:   "/",
			EnvJSON:   "{}",
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			UpdatedAt: now,
		}
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteSession(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountActiveSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("active count: got %d, want 2 (b is deleted)", n)
	}
}

func TestMessage_AppendList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()
	mustCreateSession(t, s, "sess1")

	for i, role := range []string{"user", "assistant", "tool"} {
		m := &store.Message{
			ID:          mkID("msg", i),
			SessionID:   "sess1",
			Role:        role,
			ContentJSON: `[{"type":"text","text":"hi"}]`,
			TokenCount:  i + 1,
			CreatedAt:   now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := s.AppendMessage(ctx, m); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	got, err := s.ListMessages(ctx, "sess1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if got[0].Role != "user" || got[2].Role != "tool" {
		t.Errorf("ordering broken: %+v", roles(got))
	}
}

func TestEvent_AppendIncremental(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustCreateSession(t, s, "sess1")

	// 5 events
	for i := 0; i < 5; i++ {
		_, err := s.AppendEvent(ctx, &store.Event{
			SessionID:   "sess1",
			Type:        "assistant_text_delta",
			PayloadJSON: `{"delta":"hello"}`,
		})
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	max, err := s.MaxEventIdx(ctx, "sess1")
	if err != nil || max != 5 {
		t.Fatalf("MaxEventIdx: max=%d err=%v", max, err)
	}

	// Replay (lastIdx=2, snapshot=4) should yield idx 3 and 4 only.
	got, err := s.EventsAfter(ctx, "sess1", 2, 4)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(got) != 2 || got[0].Idx != 3 || got[1].Idx != 4 {
		t.Errorf("replay window broken: %+v", idxs(got))
	}

	// Snapshot=0 should read up to current tail.
	got, _ = s.EventsAfter(ctx, "sess1", 2, 0)
	if len(got) != 3 || got[2].Idx != 5 {
		t.Errorf("unbounded replay broken: %+v", idxs(got))
	}

	// MaxEventIdx returns 0 for sessions with no events.
	mustCreateSession(t, s, "sess2")
	max, err = s.MaxEventIdx(ctx, "sess2")
	if err != nil || max != 0 {
		t.Errorf("expected MaxEventIdx=0 for empty session, got %d err=%v", max, err)
	}
}

func TestEvent_GlobalAutoIncrement(t *testing.T) {
	// The spec relies on events.idx being monotonic across the whole database
	// so the SSE id: cursor is globally unique. Two sessions writing concurrently
	// must never collide.
	ctx := context.Background()
	s := newTestStore(t)
	mustCreateSession(t, s, "a")
	mustCreateSession(t, s, "b")

	a1, _ := s.AppendEvent(ctx, &store.Event{SessionID: "a", Type: "x", PayloadJSON: "{}"})
	b1, _ := s.AppendEvent(ctx, &store.Event{SessionID: "b", Type: "x", PayloadJSON: "{}"})
	a2, _ := s.AppendEvent(ctx, &store.Event{SessionID: "a", Type: "x", PayloadJSON: "{}"})
	if a1 >= b1 || b1 >= a2 {
		t.Errorf("idx not globally monotonic: a1=%d b1=%d a2=%d", a1, b1, a2)
	}
}

func TestToolCall_AppendUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustCreateSession(t, s, "sess1")
	now := time.Now().UTC()

	tc := &store.ToolCall{
		ID:        "tu_1",
		SessionID: "sess1",
		MessageID: "msg_1",
		Tool:      "Read",
		InputJSON: `{"path":"a"}`,
		StartedAt: now,
	}
	if err := s.AppendToolCall(ctx, tc); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	finished := now.Add(time.Second)
	tc.OutputJSON = `{"output":"file contents"}`
	tc.FinishedAt = &finished
	if err := s.UpdateToolCall(ctx, tc); err != nil {
		t.Fatalf("UpdateToolCall: %v", err)
	}

	got, _ := s.ListToolCalls(ctx, "sess1")
	if len(got) != 1 || got[0].OutputJSON == "" || got[0].FinishedAt == nil {
		t.Errorf("update did not stick: %+v", got)
	}

	// Updating an unknown tool_use_id should return ErrNotFound.
	if err := s.UpdateToolCall(ctx, &store.ToolCall{ID: "nope"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown tool call, got %v", err)
	}
}

func TestPermission_PermanentVsSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()

	// Permanent: session_id = ""
	must(t, s.SavePermission(ctx, &store.Permission{
		ID:        "p1",
		Tool:      "Read",
		Pattern:   "**/*.md",
		Decision:  store.DecisionAllowPermanent,
		Scope:     store.ScopePermanent,
		CreatedAt: now,
	}))
	// Session-scoped rules for two different sessions
	must(t, s.SavePermission(ctx, &store.Permission{
		ID:        "p2",
		SessionID: "sessA",
		Tool:      "Bash",
		Pattern:   "ls *",
		Decision:  store.DecisionAllowSession,
		Scope:     store.ScopeSession,
		CreatedAt: now,
	}))
	must(t, s.SavePermission(ctx, &store.Permission{
		ID:        "p3",
		SessionID: "sessB",
		Tool:      "Bash",
		Pattern:   "echo *",
		Decision:  store.DecisionAllowSession,
		Scope:     store.ScopeSession,
		CreatedAt: now,
	}))

	got, err := s.ListPermissions(ctx, "sessA")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sessA should see permanent + own session rule, got %d", len(got))
	}
	for _, p := range got {
		if p.ID == "p3" {
			t.Errorf("sessA should not see sessB's rule")
		}
	}

	// Delete the permanent rule; sessA should now only see its own.
	if err := s.DeletePermission(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListPermissions(ctx, "sessA")
	if len(got) != 1 || got[0].ID != "p2" {
		t.Errorf("after delete, got %+v", got)
	}
}

func TestConcurrentAppend_NoDuplicateIdx(t *testing.T) {
	// 50 goroutines each appending 20 events. We expect 1000 distinct idx
	// values, no errors, no duplicates. Catches any "SetMaxOpenConns(1)"
	// regression.
	ctx := context.Background()
	s := newTestStore(t)
	mustCreateSession(t, s, "sess1")

	const goroutines = 50
	const each = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	idxCh := make(chan int64, goroutines*each)
	errCh := make(chan error, goroutines*each)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				idx, err := s.AppendEvent(ctx, &store.Event{
					SessionID:   "sess1",
					Type:        "x",
					PayloadJSON: "{}",
				})
				if err != nil {
					errCh <- err
					return
				}
				idxCh <- idx
			}
		}()
	}
	wg.Wait()
	close(idxCh)
	close(errCh)

	for err := range errCh {
		t.Fatalf("append error: %v", err)
	}
	seen := map[int64]bool{}
	for idx := range idxCh {
		if seen[idx] {
			t.Errorf("duplicate idx: %d", idx)
		}
		seen[idx] = true
	}
	if len(seen) != goroutines*each {
		t.Errorf("got %d distinct idx, want %d", len(seen), goroutines*each)
	}
}

// helpers ----

func mustCreateSession(t *testing.T, s *sqlite.Store, id string) {
	t.Helper()
	must(t, s.CreateSession(context.Background(), &store.Session{
		ID:        id,
		State:     store.SessionStateIdle,
		Workdir:   "/",
		EnvJSON:   "{}",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func mkID(prefix string, i int) string {
	return prefix + "_" + string(rune('a'+i))
}

func roles(ms []*store.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Role
	}
	return out
}

func idxs(es []*store.Event) []int64 {
	out := make([]int64, len(es))
	for i, e := range es {
		out[i] = e.Idx
	}
	return out
}
