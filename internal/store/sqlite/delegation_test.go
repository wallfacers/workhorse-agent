package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func newDelegationStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedDelegation(t *testing.T, s store.Store, id, sess string, status store.DelegationStatus, started time.Time) {
	t.Helper()
	if err := s.CreateDelegation(context.Background(), &store.Delegation{
		ID:          id,
		SessionID:   sess,
		Description: id + " task",
		Prompt:      "do " + id,
		Workdir:     "/repo",
		Status:      status,
		StartedAt:   started,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestDelegation_CreateGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newDelegationStore(t)
	started := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	if err := s.CreateDelegation(ctx, &store.Delegation{
		ID: "brisk-amber-fox", SessionID: "sess1",
		Description: "Research auth", Prompt: "do it", Workdir: "/repo",
		Status: store.DelegationRunning, StartedAt: started,
		Title: "t", Summary: "s", Result: "r",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetDelegation(ctx, "brisk-amber-fox")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.DelegationRunning || got.Description != "Research auth" ||
		got.Prompt != "do it" || got.Title != "t" || got.Result != "r" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.StartedAt.UnixMicro() != started.UnixMicro() {
		t.Fatalf("started_at: got %v want %v", got.StartedAt, started)
	}
	if got.CompletedAt != nil || got.NotifiedAt != nil {
		t.Fatalf("expected nil completed/notified, got %v %v", got.CompletedAt, got.NotifiedAt)
	}
	if _, err := s.GetDelegation(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id: got %v want ErrNotFound", err)
	}
}

func TestDelegation_ListOrdersByStartedDesc(t *testing.T) {
	ctx := context.Background()
	s := newDelegationStore(t)
	t0 := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	seedDelegation(t, s, "a", "sess1", store.DelegationComplete, t0)
	seedDelegation(t, s, "b", "sess1", store.DelegationRunning, t0.Add(time.Minute))
	seedDelegation(t, s, "c", "sess2", store.DelegationComplete, t0.Add(2*time.Minute))

	got, err := s.ListDelegations(ctx, "sess1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 delegations for sess1, got %d", len(got))
	}
	if got[0].ID != "b" || got[1].ID != "a" {
		t.Fatalf("order wrong: got %s %s, want b a", got[0].ID, got[1].ID)
	}
}

func TestDelegation_CompleteAndFail(t *testing.T) {
	ctx := context.Background()
	s := newDelegationStore(t)
	seedDelegation(t, s, "ok", "sess1", store.DelegationRunning, time.Now().UTC())
	seedDelegation(t, s, "bad", "sess1", store.DelegationRunning, time.Now().UTC())

	if err := s.CompleteDelegation(ctx, "ok", "Auth Flow", "summary here", "full result text"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err := s.GetDelegation(ctx, "ok")
	if err != nil {
		t.Fatalf("get ok: %v", err)
	}
	if got.Status != store.DelegationComplete || got.Title != "Auth Flow" ||
		got.Summary != "summary here" || got.Result != "full result text" || got.CompletedAt == nil {
		t.Fatalf("complete fields wrong: %+v", got)
	}

	if err := s.FailDelegation(ctx, "bad", "model timeout", "partial detail"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	bad, err := s.GetDelegation(ctx, "bad")
	if err != nil {
		t.Fatalf("get bad: %v", err)
	}
	if bad.Status != store.DelegationError || bad.Error != "model timeout" ||
		bad.Result != "partial detail" || bad.CompletedAt == nil {
		t.Fatalf("fail fields wrong: %+v", bad)
	}

	if err := s.CompleteDelegation(ctx, "missing", "", "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("complete missing: got %v want ErrNotFound", err)
	}
}

func TestDelegation_CountRunning(t *testing.T) {
	ctx := context.Background()
	s := newDelegationStore(t)
	t0 := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	seedDelegation(t, s, "r1", "sess1", store.DelegationRunning, t0)
	seedDelegation(t, s, "r2", "sess1", store.DelegationRunning, t0.Add(time.Second))
	seedDelegation(t, s, "c1", "sess1", store.DelegationComplete, t0.Add(2*time.Second))

	n, err := s.CountRunningDelegations(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("running count: got %d want 2", n)
	}
}

func TestDelegation_ClaimPendingExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newDelegationStore(t)
	t0 := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)

	// sess1: one complete + one failed => both pending; one running => not pending.
	seedDelegation(t, s, "d1", "sess1", store.DelegationRunning, t0)
	seedDelegation(t, s, "d2", "sess1", store.DelegationRunning, t0.Add(time.Second))
	if err := s.CompleteDelegation(ctx, "d1", "t1", "s1", "r1"); err != nil {
		t.Fatalf("complete d1: %v", err)
	}
	if err := s.FailDelegation(ctx, "d2", "boom", "rd"); err != nil {
		t.Fatalf("fail d2: %v", err)
	}
	seedDelegation(t, s, "d3", "sess1", store.DelegationRunning, t0.Add(2*time.Second)) // stays running
	// sess2: one complete, must NOT appear in sess1's claim.
	seedDelegation(t, s, "x1", "sess2", store.DelegationRunning, t0.Add(3*time.Second))
	if err := s.CompleteDelegation(ctx, "x1", "tx", "sx", "rx"); err != nil {
		t.Fatalf("complete x1: %v", err)
	}

	first, err := s.ClaimPendingNotifications(ctx, "sess1")
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("claim 1: want 2 pending, got %d", len(first))
	}
	if first[0].ID != "d1" || first[1].ID != "d2" {
		t.Fatalf("claim order: got %s %s, want d1 d2", first[0].ID, first[1].ID)
	}

	// notified_at stamped => second claim returns nothing (exactly-once).
	second, err := s.ClaimPendingNotifications(ctx, "sess1")
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("claim 2: want 0, got %d (not exactly-once)", len(second))
	}

	// sess2's delegation still pending for its own session.
	cross, err := s.ClaimPendingNotifications(ctx, "sess2")
	if err != nil {
		t.Fatalf("claim sess2: %v", err)
	}
	if len(cross) != 1 || cross[0].ID != "x1" {
		t.Fatalf("claim sess2: got %+v", cross)
	}
}

func TestDelegation_ReapRunning(t *testing.T) {
	ctx := context.Background()
	s := newDelegationStore(t)
	t0 := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	seedDelegation(t, s, "live1", "sess1", store.DelegationRunning, t0)
	seedDelegation(t, s, "live2", "sess2", store.DelegationRunning, t0.Add(time.Second))
	seedDelegation(t, s, "done", "sess1", store.DelegationComplete, t0.Add(2*time.Second))

	if err := s.ReapRunningDelegations(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	for _, id := range []string{"live1", "live2"} {
		got, err := s.GetDelegation(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != store.DelegationError || got.Error != "server restarted" || got.CompletedAt == nil {
			t.Fatalf("%s not reaped: %+v", id, got)
		}
	}
	// Already-complete delegation is untouched.
	done, err := s.GetDelegation(ctx, "done")
	if err != nil {
		t.Fatalf("get done: %v", err)
	}
	if done.Status != store.DelegationComplete {
		t.Fatalf("done status changed by reap: %+v", done)
	}
	n, err := s.CountRunningDelegations(ctx)
	if err != nil {
		t.Fatalf("count after reap: %v", err)
	}
	if n != 0 {
		t.Fatalf("running count after reap: got %d want 0", n)
	}
}
