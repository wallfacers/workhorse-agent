package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

const hydID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func persistedStore(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC()
	if err := st.CreateSession(ctx, &store.Session{
		ID: hydID, State: store.SessionStateIdle, Workdir: "/tmp/p",
		EnvJSON: `{"K":"V"}`, Model: "anthropic:x", Title: "old chat",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i, m := range []struct{ id, role, text string }{
		{"m1", "user", "earlier question"},
		{"m2", "assistant", "earlier answer"},
	} {
		if err := st.AppendMessage(ctx, &store.Message{
			ID: m.id, SessionID: hydID, Role: m.role,
			ContentJSON: `[{"type":"text","text":"` + m.text + `"}]`,
			CreatedAt:   now.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	return st
}

func TestManager_HydratesPersistedSession(t *testing.T) {
	ctx := context.Background()
	st := persistedStore(t)
	runner := &fakeRunner{}
	m := NewManager(ManagerOptions{Store: st, RunnerFactory: func(*Session) Runner { return runner }})

	// Precondition: not live in memory.
	if _, err := m.GetSession(hydID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("precondition: want not-live, got %v", err)
	}

	sess, err := m.GetOrHydrate(ctx, hydID)
	if err != nil {
		t.Fatalf("GetOrHydrate: %v", err)
	}
	if sess.ID != hydID || sess.Workdir != "/tmp/p" || sess.Env["K"] != "V" {
		t.Errorf("metadata not restored: id=%q workdir=%q env=%v", sess.ID, sess.Workdir, sess.Env)
	}
	if sess.State() != StateIdle {
		t.Errorf("hydrated state = %v, want idle", sess.State())
	}

	// T4: the model context is rebuilt from the persisted transcript.
	hist := sess.History()
	if len(hist) != 2 {
		t.Fatalf("restored history len = %d, want 2", len(hist))
	}
	if hist[0].Role != provider.RoleUser || hist[0].Content[0].Text != "earlier question" {
		t.Errorf("history[0] wrong: %+v", hist[0])
	}
	if hist[1].Content[0].Text != "earlier answer" {
		t.Errorf("history[1] wrong: %+v", hist[1])
	}

	// Now live and idempotent: a second hydrate returns the same pointer.
	got, err := m.GetSession(hydID)
	if err != nil || got != sess {
		t.Fatalf("after hydrate GetSession should return same live session: got=%p err=%v", got, err)
	}
	again, err := m.GetOrHydrate(ctx, hydID)
	if err != nil || again != sess {
		t.Errorf("re-hydrate must return the existing live session")
	}

	// Hydration must NOT re-persist the transcript (ids unchanged).
	msgs, _ := st.ListMessages(ctx, hydID)
	if len(msgs) != 2 || msgs[0].ID != "m1" || msgs[1].ID != "m2" {
		t.Errorf("hydration rewrote the transcript: %+v", msgs)
	}

	// Runner goroutine started.
	deadline := time.Now().Add(time.Second)
	for !runner.started.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !runner.started.Load() {
		t.Error("hydrated session runner never started")
	}
}

// TestPersistTitle_PreservesFields locks the fix for the partial-row clobber
// bug: persisting a title must not blank workdir/model/etc. (UpdateSession
// overwrites every column).
func TestPersistTitle_PreservesFields(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := st.CreateSession(ctx, &store.Session{
		ID: id, State: store.SessionStateIdle, Workdir: "/keep/me",
		EnvJSON: `{"K":"V"}`, Model: "anthropic:keep", AgentType: "coder",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sess := New(Options{
		Workdir: "/keep/me", Env: map[string]string{"K": "V"},
		Model: "anthropic:keep", AgentType: "coder", Store: st,
	})
	sess.ID = id
	sess.CreatedAt = now
	sess.SetTitle("derived title")
	sess.PersistTitle(ctx)

	got, err := st.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "derived title" {
		t.Errorf("title not persisted: %q", got.Title)
	}
	if got.Workdir != "/keep/me" {
		t.Errorf("workdir clobbered: %q", got.Workdir)
	}
	if got.Model != "anthropic:keep" {
		t.Errorf("model clobbered: %q", got.Model)
	}
	if got.AgentType != "coder" {
		t.Errorf("agent_type clobbered: %q", got.AgentType)
	}
}

func TestManager_GetOrHydrate_MissingAndDeleted(t *testing.T) {
	ctx := context.Background()
	st := persistedStore(t)
	m := NewManager(ManagerOptions{Store: st, RunnerFactory: func(*Session) Runner { return &fakeRunner{} }})

	if _, err := m.GetOrHydrate(ctx, "01ZZZZZZZZZZZZZZZZZZZZZZZZ"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id: want ErrNotFound, got %v", err)
	}

	// Soft-deleted sessions are not hydratable.
	if err := st.DeleteSession(ctx, hydID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := m.GetOrHydrate(ctx, hydID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted id: want ErrNotFound, got %v", err)
	}
}

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
