package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRunner records context cancellation and waits up to a configurable
// "drain" delay before its Run method returns. Used to verify cancel cascade
// and Manager.DeleteSession draining.
type fakeRunner struct {
	started     atomic.Bool
	ctxObserved atomic.Bool
	drain       time.Duration
}

func (r *fakeRunner) Run(ctx context.Context) {
	r.started.Store(true)
	<-ctx.Done()
	r.ctxObserved.Store(true)
	if r.drain > 0 {
		time.Sleep(r.drain)
	}
}

func TestManager_CreateAndGetEphemeral(t *testing.T) {
	runner := &fakeRunner{}
	m := NewManager(ManagerOptions{
		RunnerFactory: func(*Session) Runner { return runner },
	})

	sess, err := m.CreateSession(context.Background(), Options{Ephemeral: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.State() != StateIdle {
		t.Fatalf("new session must start Idle, got %q", sess.State())
	}
	// Give the runner goroutine a moment to start.
	deadline := time.Now().Add(time.Second)
	for !runner.started.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !runner.started.Load() {
		t.Fatal("runner.Run never started")
	}
	got, err := m.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != sess {
		t.Fatalf("GetSession returned different pointer")
	}
	if m.CountActive() != 1 {
		t.Fatalf("CountActive: want 1, got %d", m.CountActive())
	}
}

func TestManager_GetMissing(t *testing.T) {
	m := NewManager(ManagerOptions{})
	if _, err := m.GetSession("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestManager_MaxConcurrent(t *testing.T) {
	m := NewManager(ManagerOptions{MaxConcurrent: 2})
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := m.CreateSession(ctx, Options{Ephemeral: true}); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	if _, err := m.CreateSession(ctx, Options{Ephemeral: true}); !errors.Is(err, ErrTooManyConcurrent) {
		t.Fatalf("expected ErrTooManyConcurrent, got %v", err)
	}
}

func TestManager_Cancel_TriggersRunnerContext(t *testing.T) {
	runner := &fakeRunner{}
	m := NewManager(ManagerOptions{
		RunnerFactory: func(*Session) Runner { return runner },
	})
	sess, err := m.CreateSession(context.Background(), Options{Ephemeral: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Wait for runner to start.
	waitFor(t, func() bool { return runner.started.Load() }, time.Second)
	if runner.ctxObserved.Load() {
		t.Fatal("runner observed cancel before Cancel call")
	}
	if err := m.Cancel(sess.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, runner.ctxObserved.Load, time.Second)

	// Repeat Cancel must be a no-op (idempotent).
	if err := m.Cancel(sess.ID); err != nil {
		t.Fatalf("cancel #2: %v", err)
	}
	if err := m.Cancel("unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown: want ErrNotFound, got %v", err)
	}
}

func TestManager_DeleteSession_WaitsForRunner(t *testing.T) {
	runner := &fakeRunner{drain: 50 * time.Millisecond}
	m := NewManager(ManagerOptions{
		RunnerFactory: func(*Session) Runner { return runner },
	})
	sess, err := m.CreateSession(context.Background(), Options{Ephemeral: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitFor(t, func() bool { return runner.started.Load() }, time.Second)

	start := time.Now()
	if err := m.DeleteSession(context.Background(), sess.ID, time.Second); err != nil {
		t.Fatalf("delete: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("delete returned too early (%s); runner drain not respected", elapsed)
	}
	if _, err := m.GetSession(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session must be gone after delete, got %v", err)
	}
}

func TestManager_DeleteSession_HardTimeout(t *testing.T) {
	// A runner that ignores ctx.Done and runs forever — DeleteSession must
	// still return within the drain timeout and remove the entry.
	wedged := make(chan struct{})
	m := NewManager(ManagerOptions{
		RunnerFactory: func(*Session) Runner {
			return runnerFunc(func(ctx context.Context) {
				<-wedged
			})
		},
	})
	defer close(wedged)
	sess, err := m.CreateSession(context.Background(), Options{Ephemeral: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	start := time.Now()
	if err := m.DeleteSession(context.Background(), sess.ID, 50*time.Millisecond); err != nil {
		t.Fatalf("delete: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("delete blocked past drain timeout: %s", elapsed)
	}
	if _, err := m.GetSession(sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session must be removed even on wedge: %v", err)
	}
}

func TestManager_Shutdown_CancelsAll(t *testing.T) {
	const N = 3
	runners := make([]*fakeRunner, N)
	for i := range runners {
		runners[i] = &fakeRunner{}
	}
	idx := atomic.Int32{}
	m := NewManager(ManagerOptions{
		RunnerFactory: func(*Session) Runner {
			i := idx.Add(1) - 1
			return runners[i]
		},
	})
	for i := 0; i < N; i++ {
		if _, err := m.CreateSession(context.Background(), Options{Ephemeral: true}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	for _, r := range runners {
		waitFor(t, func() bool { return r.started.Load() }, time.Second)
	}
	m.Shutdown(time.Second)
	for i, r := range runners {
		if !r.ctxObserved.Load() {
			t.Fatalf("runner #%d never observed shutdown cancel", i)
		}
	}
}

type runnerFunc func(context.Context)

func (f runnerFunc) Run(ctx context.Context) { f(ctx) }

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
