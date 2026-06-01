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

func TestListSessionsByWorkdir(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()
	mk := func(id, workdir string, upd time.Duration) {
		if err := s.CreateSession(ctx, &store.Session{
			ID: id, State: store.SessionStateIdle, Workdir: workdir, EnvJSON: "{}",
			Title: "t-" + id, CreatedAt: now, UpdatedAt: now.Add(upd),
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("s1", "/a", 2*time.Second)
	mk("s2", "/a", 1*time.Second)
	mk("s3", "/b", 0)

	// s1 has two messages; the latest text drives the preview.
	for i, txt := range []string{"first", "latest one"} {
		if err := s.AppendMessage(ctx, &store.Message{
			ID: mkID("s1m", i), SessionID: "s1", Role: "user",
			ContentJSON: `[{"type":"text","text":"` + txt + `"}]`,
			CreatedAt:   now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got, err := s.ListSessionsByWorkdir(ctx, "/a")
	if err != nil {
		t.Fatalf("ListSessionsByWorkdir: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions for /a, got %d", len(got))
	}
	// Ordered by updated_at DESC → s1 first.
	if got[0].ID != "s1" {
		t.Errorf("order: got[0]=%s want s1", got[0].ID)
	}
	if got[0].MessageCount != 2 {
		t.Errorf("s1 MessageCount = %d, want 2", got[0].MessageCount)
	}
	if got[0].LastMessagePreview != "latest one" {
		t.Errorf("s1 LastMessagePreview = %q, want %q", got[0].LastMessagePreview, "latest one")
	}
	if got[0].Title != "t-s1" {
		t.Errorf("s1 Title = %q", got[0].Title)
	}
	if got[1].MessageCount != 0 || got[1].LastMessagePreview != "" {
		t.Errorf("s2 should have no messages: count=%d preview=%q", got[1].MessageCount, got[1].LastMessagePreview)
	}
}

func TestListProjects(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()
	for _, row := range []struct {
		id, workdir string
	}{{"s1", "/a"}, {"s2", "/a"}, {"s3", "/b"}} {
		if err := s.CreateSession(ctx, &store.Session{
			ID: row.id, State: store.SessionStateIdle, Workdir: row.workdir,
			EnvJSON: "{}", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// A deleted session must not keep a project alive on its own.
	if err := s.PurgeSession(ctx, "s3"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	got, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 project after purging /b's only session, got %d (%+v)", len(got), got)
	}
	if got[0].Path != "/a" || got[0].SessionCount != 2 {
		t.Errorf("project = %+v, want {/a, 2}", got[0])
	}
}

func TestSession_PurgeCascades(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()
	mustCreateSession(t, s, "s1")

	if err := s.AppendMessage(ctx, &store.Message{ID: "m1", SessionID: "s1", Role: "user", ContentJSON: `[{"type":"text","text":"hi"}]`, CreatedAt: now}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, err := s.AppendEvent(ctx, &store.Event{SessionID: "s1", Type: "assistant_text_delta", PayloadJSON: `{}`}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := s.AppendToolCall(ctx, &store.ToolCall{ID: "tc1", SessionID: "s1", MessageID: "m1", Tool: "Bash", InputJSON: `{}`, StartedAt: now}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	if err := s.PurgeSession(ctx, "s1"); err != nil {
		t.Fatalf("PurgeSession: %v", err)
	}

	if _, err := s.GetSession(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session row should be gone, got err=%v", err)
	}
	if msgs, _ := s.ListMessages(ctx, "s1"); len(msgs) != 0 {
		t.Errorf("messages should cascade-delete, got %d", len(msgs))
	}
	for _, tbl := range []string{"events", "tool_calls", "messages_fts"} {
		var n int
		q := "SELECT count(*) FROM " + tbl
		if tbl != "messages_fts" {
			q += " WHERE session_id = 's1'"
		}
		if err := s.DB().QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("%s rows should be 0 after purge, got %d", tbl, n)
		}
	}

	// Purging a missing session is a no-op error per the interface contract.
	if err := s.PurgeSession(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("purge missing: want ErrNotFound, got %v", err)
	}
}

func TestSession_Title(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	sess := &store.Session{
		ID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", State: store.SessionStateIdle,
		Workdir: "/tmp/x", EnvJSON: "{}", Title: "first title",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got.Title != "first title" {
		t.Errorf("title round-trip: got %q want %q", got.Title, "first title")
	}

	sess.Title = "renamed"
	sess.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateSession(ctx, sess); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	got, _ = s.GetSession(ctx, sess.ID)
	if got.Title != "renamed" {
		t.Errorf("title update did not stick: %q", got.Title)
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

// TestMessage_AppendPopulatesFTS locks the contract that persisting a message
// (the now-live AppendMessage write path) populates messages_fts so
// session_search works on real sessions (add-project-sessions 0.4). It guards
// against either the trigger or the write path silently regressing.
func TestMessage_AppendPopulatesFTS(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustCreateSession(t, s, "sess1")

	if err := s.AppendMessage(ctx, &store.Message{
		ID:          mkID("msg", 0),
		SessionID:   "sess1",
		Role:        "user",
		ContentJSON: `[{"type":"text","text":"findmexyz needle"}]`,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, "findmexyz").Scan(&n); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if n != 1 {
		t.Fatalf("messages_fts row count = %d, want 1 (trigger did not fire on AppendMessage)", n)
	}
}

func TestMessage_Replace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()
	mustCreateSession(t, s, "sess1")

	for i, role := range []string{"user", "assistant", "tool"} {
		if err := s.AppendMessage(ctx, &store.Message{
			ID:          mkID("old", i),
			SessionID:   "sess1",
			Role:        role,
			ContentJSON: `[{"type":"text","text":"old"}]`,
			CreatedAt:   now.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	// Replace the 3 messages with 2 fresh ones (the compaction rewrite path).
	repl := []*store.Message{
		{ID: mkID("new", 0), SessionID: "sess1", Role: "user", ContentJSON: `[{"type":"text","text":"summary"}]`, CreatedAt: now.Add(10 * time.Millisecond)},
		{ID: mkID("new", 1), SessionID: "sess1", Role: "assistant", ContentJSON: `[{"type":"text","text":"ok"}]`, CreatedAt: now.Add(11 * time.Millisecond)},
	}
	if err := s.ReplaceMessages(ctx, "sess1", repl); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	got, err := s.ListMessages(ctx, "sess1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("after replace got %d messages, want 2", len(got))
	}
	if got[0].ID != mkID("new", 0) || got[1].Role != "assistant" {
		t.Errorf("replace content wrong: %+v", roles(got))
	}

	// Replacing with nothing clears the transcript.
	if err := s.ReplaceMessages(ctx, "sess1", nil); err != nil {
		t.Fatalf("ReplaceMessages(nil): %v", err)
	}
	got, _ = s.ListMessages(ctx, "sess1")
	if len(got) != 0 {
		t.Fatalf("after empty replace got %d, want 0", len(got))
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

func TestPermission_InsertOrReplace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.SavePermission(ctx, &store.Permission{
		ID:        "perm-abc",
		Tool:      "Bash",
		Pattern:   "git *",
		Decision:  store.DecisionAllowPermanent,
		Scope:     store.ScopePermanent,
		CreatedAt: now,
	}))

	// Save same ID with different decision — should replace, not error.
	must(t, s.SavePermission(ctx, &store.Permission{
		ID:        "perm-abc",
		Tool:      "Bash",
		Pattern:   "git *",
		Decision:  store.DecisionDenyPermanent,
		Scope:     store.ScopePermanent,
		CreatedAt: now.Add(time.Hour),
	}))

	got, err := s.ListPermissions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after replace, want 1 row, got %d", len(got))
	}
	if got[0].Decision != store.DecisionDenyPermanent {
		t.Errorf("decision not updated: got %q", got[0].Decision)
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
