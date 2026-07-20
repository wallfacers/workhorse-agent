package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

// fakeStore is a minimal store.Store stub: only AppendEvent is non-trivial,
// and it assigns idx starting at 100 so tests can tell store-assigned idx
// apart from the in-memory counter starting at 1.
type fakeStore struct {
	mu       sync.Mutex
	next     int64
	events   []*store.Event
	messages []*store.Message
}

func (f *fakeStore) AppendEvent(_ context.Context, e *store.Event) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next == 0 {
		f.next = 100
	}
	idx := f.next
	f.next++
	e.Idx = idx
	f.events = append(f.events, e)
	return idx, nil
}
func (f *fakeStore) CreateSession(context.Context, *store.Session) error { return nil }
func (f *fakeStore) GetSession(context.Context, string) (*store.Session, error) {
	return nil, store.ErrNotFound
}
func (f *fakeStore) ListSessions(context.Context, bool) ([]*store.Session, error) { return nil, nil }
func (f *fakeStore) ListSessionsByWorkdir(context.Context, string) ([]*store.SessionSummary, error) {
	return nil, nil
}
func (f *fakeStore) ListAllSessions(context.Context) ([]*store.SessionSummary, error) {
	return nil, nil
}
func (f *fakeStore) ListProjects(context.Context) ([]*store.Project, error)   { return nil, nil }
func (f *fakeStore) UpdateSession(context.Context, *store.Session) error      { return nil }
func (f *fakeStore) UpdateSessionTitle(context.Context, string, string) error { return nil }
func (f *fakeStore) CountMessages(context.Context, string) (int, error)       { return 0, nil }
func (f *fakeStore) DeleteSession(context.Context, string) error              { return nil }
func (f *fakeStore) PurgeSession(context.Context, string) error               { return nil }
func (f *fakeStore) CountActiveSessions(context.Context) (int, error)         { return 0, nil }
func (f *fakeStore) AppendMessage(_ context.Context, m *store.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, m)
	return nil
}
func (f *fakeStore) ListMessages(context.Context, string) ([]*store.Message, error) { return nil, nil }
func (f *fakeStore) MarkMessageInterrupted(context.Context, string) error           { return nil }
func (f *fakeStore) ReplaceMessages(_ context.Context, _ string, msgs []*store.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append([]*store.Message(nil), msgs...)
	return nil
}
func (f *fakeStore) EventsAfter(context.Context, string, int64, int64) ([]*store.Event, error) {
	return nil, nil
}
func (f *fakeStore) MaxEventIdx(context.Context, string) (int64, error)    { return 0, nil }
func (f *fakeStore) AppendToolCall(context.Context, *store.ToolCall) error { return nil }
func (f *fakeStore) UpdateToolCall(context.Context, *store.ToolCall) error { return nil }
func (f *fakeStore) ListToolCalls(context.Context, string) ([]*store.ToolCall, error) {
	return nil, nil
}
func (f *fakeStore) SavePermission(context.Context, *store.Permission) error { return nil }
func (f *fakeStore) ListPermissions(context.Context, string) ([]*store.Permission, error) {
	return nil, nil
}
func (f *fakeStore) DeletePermission(context.Context, string) error { return nil }
func (f *fakeStore) GetDelegation(context.Context, string) (*store.Delegation, error) {
	return nil, store.ErrNotFound
}
func (f *fakeStore) ListDelegations(context.Context, string) ([]*store.Delegation, error) {
	return nil, nil
}
func (f *fakeStore) CreateDelegation(context.Context, *store.Delegation) error { return nil }
func (f *fakeStore) CountRunningDelegations(context.Context) (int, error)      { return 0, nil }
func (f *fakeStore) CompleteDelegation(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeStore) FailDelegation(context.Context, string, string, string) error {
	return nil
}
func (f *fakeStore) ClaimPendingNotifications(context.Context, string) ([]*store.Delegation, error) {
	return nil, nil
}
func (f *fakeStore) ReapRunningDelegations(context.Context) error          { return nil }
func (f *fakeStore) CreateSchedule(context.Context, *store.Schedule) error { return nil }
func (f *fakeStore) GetSchedule(context.Context, string) (*store.Schedule, error) {
	return nil, store.ErrNotFound
}
func (f *fakeStore) ListSchedules(context.Context) ([]*store.Schedule, error) {
	return nil, nil
}
func (f *fakeStore) DeleteSchedule(context.Context, string) error              { return nil }
func (f *fakeStore) TouchScheduleRun(context.Context, string, time.Time) error { return nil }
func (f *fakeStore) CreateScheduleRun(context.Context, *store.ScheduleRun) (int64, error) {
	return 0, nil
}
func (f *fakeStore) FinishScheduleRun(context.Context, int64, store.ScheduleRunStatus, string, string) error {
	return nil
}
func (f *fakeStore) ListScheduleRuns(context.Context, string, int) ([]*store.ScheduleRun, error) {
	return nil, nil
}
func (f *fakeStore) PruneScheduleRuns(context.Context, string, int) error { return nil }
func (f *fakeStore) Close() error                                         { return nil }

func TestNew_DefaultsAndIDFormat(t *testing.T) {
	s := New(Options{})
	if s.State() != StateIdle {
		t.Fatalf("new session must start in Idle, got %q", s.State())
	}
	if len(s.ID) != 26 {
		t.Fatalf("session ID expected 26 chars (ULID), got %d (%q)", len(s.ID), s.ID)
	}
	if cap(s.Inbox) == 0 || cap(s.Outbox) == 0 {
		t.Fatalf("channels must be buffered (got inbox=%d outbox=%d)", cap(s.Inbox), cap(s.Outbox))
	}
}

func TestTransition_AllowedAndRejected(t *testing.T) {
	cases := []struct {
		name string
		from State
		to   State
		ok   bool
	}{
		{"idle→thinking", StateIdle, StateThinking, true},
		{"thinking→awaitPerm", StateThinking, StateAwaitPerm, true},
		{"thinking→executing", StateThinking, StateExecuting, true},
		{"thinking→compacting", StateThinking, StateCompacting, true},
		{"executing→thinking", StateExecuting, StateThinking, true},
		{"compacting→idle", StateCompacting, StateIdle, true},
		{"cancelled→idle", StateCancelled, StateIdle, true},
		{"idle→executing forbidden", StateIdle, StateExecuting, false},
		{"executing→idle", StateExecuting, StateIdle, true},
		{"idle→awaitPerm forbidden", StateIdle, StateAwaitPerm, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{})
			// Set the from-state directly via ForceTransition for cases that
			// can't be reached from Idle.
			if tc.from != StateIdle {
				// drive there via a legal path
				switch tc.from {
				case StateThinking:
					mustTransition(t, s, StateIdle, StateThinking)
				case StateExecuting:
					mustTransition(t, s, StateIdle, StateThinking)
					mustTransition(t, s, StateThinking, StateExecuting)
				case StateCompacting:
					mustTransition(t, s, StateIdle, StateThinking)
					mustTransition(t, s, StateThinking, StateCompacting)
				case StateCancelled:
					mustTransition(t, s, StateIdle, StateCancelled)
				case StateAwaitPerm:
					mustTransition(t, s, StateIdle, StateThinking)
					mustTransition(t, s, StateThinking, StateAwaitPerm)
				}
			}
			err := s.Transition(tc.from, tc.to)
			if tc.ok && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected failure, got success")
			}
			if !tc.ok && !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("expected ErrInvalidTransition, got %v", err)
			}
		})
	}
}

func mustTransition(t *testing.T, s *Session, from, to State) {
	t.Helper()
	if err := s.Transition(from, to); err != nil {
		t.Fatalf("setup transition %s→%s: %v", from, to, err)
	}
}

func TestPendingToolUses_TrackAndDrain(t *testing.T) {
	s := New(Options{})
	s.MarkToolUsePending("u1", "Read", json.RawMessage(`{"path":"a"}`))
	s.MarkToolUsePending("u2", "Bash", json.RawMessage(`{"cmd":"ls"}`))
	s.ClearToolUsePending("u1")

	drained := s.DrainPendingToolUses()
	if len(drained) != 1 || drained[0].ID != "u2" {
		t.Fatalf("expected only u2 after clear+drain, got %+v", drained)
	}
	if got := s.DrainPendingToolUses(); got != nil {
		t.Fatalf("second drain must be empty, got %+v", got)
	}
}

func TestEmit_PopulatesEventAndAssignsIdx(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if err := s.Emit(ctx, "assistant_text_delta", map[string]any{"delta": "hi"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := s.Emit(ctx, "assistant_text_done", map[string]any{"message_id": "m1"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	e1 := <-s.Outbox
	e2 := <-s.Outbox
	if e1.Idx != 1 || e2.Idx != 2 {
		t.Fatalf("idx must increment monotonically, got %d / %d", e1.Idx, e2.Idx)
	}
	if e1.SessionID != s.ID {
		t.Fatalf("session_id missing: got %q want %q", e1.SessionID, s.ID)
	}
	if e1.Type != "assistant_text_delta" {
		t.Fatalf("type: got %q", e1.Type)
	}
}

func TestEmit_StoreBackedIdxOverridesCounter(t *testing.T) {
	fs := &fakeStore{}
	s := New(Options{Store: fs})
	if err := s.Emit(context.Background(), "assistant_text_delta", map[string]any{"delta": "x"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := s.Emit(context.Background(), "assistant_text_done", map[string]any{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	e1 := <-s.Outbox
	e2 := <-s.Outbox
	if e1.Idx != 100 || e2.Idx != 101 {
		t.Fatalf("expected store-assigned idx 100/101, got %d/%d", e1.Idx, e2.Idx)
	}
	if len(fs.events) != 2 {
		t.Fatalf("expected 2 events persisted, got %d", len(fs.events))
	}
	if fs.events[0].Type != "assistant_text_delta" || fs.events[1].Type != "assistant_text_done" {
		t.Fatalf("persisted types wrong: %s %s", fs.events[0].Type, fs.events[1].Type)
	}
}

func TestEmit_EphemeralBypassesStore(t *testing.T) {
	fs := &fakeStore{}
	s := New(Options{Store: fs, Ephemeral: true})
	if err := s.Emit(context.Background(), "x", nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	e := <-s.Outbox
	if e.Idx != 1 {
		t.Fatalf("ephemeral session must use in-memory counter, got idx=%d", e.Idx)
	}
	if len(fs.events) != 0 {
		t.Fatalf("ephemeral must not persist, got %d events", len(fs.events))
	}
}

func TestEvent_MarshalJSON_FlattensPayload(t *testing.T) {
	e := Event{
		Type:      "tool_call_done",
		SessionID: "01HFOO",
		Idx:       7,
		Payload: map[string]any{
			"id":      "u1",
			"output":  "ok",
			"took_ms": 12,
		},
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["type"] != "tool_call_done" || decoded["idx"].(float64) != 7 ||
		decoded["session_id"] != "01HFOO" || decoded["output"] != "ok" {
		t.Fatalf("flattened JSON missing fields: %s", raw)
	}
}

func TestAllowedTools_RoundTrip(t *testing.T) {
	s := New(Options{AllowedTools: []string{"Read", "Bash"}})
	got := s.AllowedTools()
	if len(got) != 2 || got[0] != "Read" || got[1] != "Bash" {
		t.Fatalf("AllowedTools roundtrip failed: %+v", got)
	}
	s.SetAllowedTools([]string{"Read"})
	got = s.AllowedTools()
	if len(got) != 1 || got[0] != "Read" {
		t.Fatalf("after SetAllowedTools: %+v", got)
	}
}

func TestHistory_AppendAndReplace(t *testing.T) {
	s := New(Options{})
	s.AppendMessage(context.Background(), provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}})
	s.AppendMessage(context.Background(), provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hello"}}})
	hist := s.History()
	if len(hist) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(hist))
	}
	s.ReplaceHistory(context.Background(), []provider.Message{
		{Role: provider.RoleSystem, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "summary"}}},
	})
	hist = s.History()
	if len(hist) != 1 || hist[0].Role != provider.RoleSystem {
		t.Fatalf("after ReplaceHistory: %+v", hist)
	}
}

func TestEmit_RespectsContextCancel(t *testing.T) {
	s := New(Options{OutboxBuffer: 1})
	// Fill the outbox so the next Emit blocks.
	_ = s.Emit(context.Background(), "x", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if err := s.Emit(ctx, "y", nil); err == nil {
		t.Fatal("expected cancel error from blocked Emit")
	}
	// Wait a beat to make sure no goroutine is leaked.
	time.Sleep(5 * time.Millisecond)
}
