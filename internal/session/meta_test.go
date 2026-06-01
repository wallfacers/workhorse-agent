package session

import "testing"

func TestSessionStatusProjection(t *testing.T) {
	s := New(Options{})
	if got := s.Status(); got != "idle" {
		t.Fatalf("new session Status = %q, want idle", got)
	}

	cases := []struct {
		to   State
		want string
	}{
		{StateThinking, "running"},
		{StateAwaitPerm, "running"},
		{StateExecuting, "running"},
	}
	// Drive Idle→Thinking→AwaitPerm→Executing and check each is "running".
	mustTransition(t, s, StateIdle, StateThinking)
	if got := s.Status(); got != "running" {
		t.Errorf("Thinking Status = %q, want running", got)
	}
	mustTransition(t, s, StateThinking, StateAwaitPerm)
	if got := s.Status(); got != "running" {
		t.Errorf("AwaitPerm Status = %q, want running", got)
	}
	mustTransition(t, s, StateAwaitPerm, StateExecuting)
	if got := s.Status(); got != "running" {
		t.Errorf("Executing Status = %q, want running", got)
	}
	_ = cases

	// Cancelled projects back to idle.
	mustTransition(t, s, StateExecuting, StateCancelled)
	if got := s.Status(); got != "idle" {
		t.Errorf("Cancelled Status = %q, want idle", got)
	}
}

func TestSessionTitle(t *testing.T) {
	s := New(Options{})
	if got := s.Title(); got != "" {
		t.Fatalf("new session Title = %q, want empty", got)
	}
	s.SetTitle("重构登录流程")
	if got := s.Title(); got != "重构登录流程" {
		t.Errorf("Title after SetTitle = %q", got)
	}
}
