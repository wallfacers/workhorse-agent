package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
)

// Runner is what the manager hands off to internal/agent to drive a session.
// The manager holds it only as an opaque handle; the cancel func cascades to
// every goroutine the agent loop spawned (provider HTTP, tool execution,
// child sessions).
type Runner interface {
	// Run blocks for the lifetime of the session goroutine. It returns when
	// ctx is cancelled or Stop is called.
	Run(ctx context.Context)
}

// RunnerFactory wires an agent loop to a Session. Manager calls it once per
// new session and keeps the returned Runner.
type RunnerFactory func(*Session) Runner

// ErrTooManyConcurrent is returned by CreateSession when the active session
// count is already at sessions.max_concurrent.
var ErrTooManyConcurrent = errors.New("session: max concurrent sessions reached")

// ErrNotFound is the manager-level alias for missing sessions. The HTTP layer
// translates it to 404.
var ErrNotFound = errors.New("session: not found")

// Manager owns the lifecycle of every active session: creation (with
// max_concurrent enforcement), lookup, listing, and deletion with cancel
// cascade. It is the only object the HTTP layer talks to for session CRUD.
type Manager struct {
	store         store.Store
	runnerFactory RunnerFactory
	maxConcurrent int

	mu       sync.Mutex
	sessions map[string]*activeSession
}

type activeSession struct {
	sess   *Session
	cancel context.CancelFunc
	done   chan struct{}
}

// ManagerOptions bundles construction args so callers don't pass five
// positional parameters.
type ManagerOptions struct {
	Store         store.Store
	RunnerFactory RunnerFactory
	MaxConcurrent int
}

// NewManager builds a Manager. The store may be nil for unit tests that don't
// exercise persistence (ephemeral-only sessions). RunnerFactory may also be
// nil; CreateSession will then return a started session whose goroutine never
// runs — useful for state-machine unit tests.
func NewManager(opts ManagerOptions) *Manager {
	return &Manager{
		store:         opts.Store,
		runnerFactory: opts.RunnerFactory,
		maxConcurrent: opts.MaxConcurrent,
		sessions:      map[string]*activeSession{},
	}
}

// CreateSession instantiates a Session, persists it (unless ephemeral), starts
// its agent goroutine, and tracks it under the manager. Returns
// ErrTooManyConcurrent when the cap is reached.
func (m *Manager) CreateSession(ctx context.Context, opts Options) (*Session, error) {
	if opts.Store == nil {
		opts.Store = m.store
	}
	sess := New(opts)

	m.mu.Lock()
	if m.maxConcurrent > 0 && len(m.sessions) >= m.maxConcurrent {
		m.mu.Unlock()
		return nil, ErrTooManyConcurrent
	}
	// Reserve the slot under the lock so concurrent CreateSession calls don't
	// race past the cap. We finalise the active-session entry after the
	// (cheap) persistence write.
	m.sessions[sess.ID] = nil
	m.mu.Unlock()

	if !sess.Ephemeral && m.store != nil {
		if err := m.persistNew(ctx, sess); err != nil {
			m.mu.Lock()
			delete(m.sessions, sess.ID)
			m.mu.Unlock()
			return nil, err
		}
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	active := &activeSession{sess: sess, cancel: cancel, done: done}

	m.mu.Lock()
	m.sessions[sess.ID] = active
	m.mu.Unlock()

	if m.runnerFactory != nil {
		runner := m.runnerFactory(sess)
		go func() {
			defer close(done)
			runner.Run(runCtx)
		}()
	} else {
		close(done)
	}

	return sess, nil
}

func (m *Manager) persistNew(ctx context.Context, s *Session) error {
	envJSON, err := json.Marshal(s.Env)
	if err != nil {
		return fmt.Errorf("session: marshal env: %w", err)
	}
	row := &store.Session{
		ID:        s.ID,
		ParentID:  s.ParentID,
		State:     store.SessionState(s.State()),
		Workdir:   s.Workdir,
		EnvJSON:   string(envJSON),
		AgentType: s.AgentType,
		Model:     s.Model,
		Ephemeral: false,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.CreatedAt,
	}
	if err := m.store.CreateSession(ctx, row); err != nil {
		return fmt.Errorf("session: persist: %w", err)
	}
	return nil
}

// GetSession returns the live in-memory session. For sessions that were
// previously persisted but not loaded yet, the caller must hydrate via the
// store directly (Group 9 will).
func (m *Manager) GetSession(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	active, ok := m.sessions[id]
	if !ok || active == nil {
		return nil, ErrNotFound
	}
	return active.sess, nil
}

// ListSessions returns a snapshot of every live session. Order is unspecified.
func (m *Manager) ListSessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, a := range m.sessions {
		if a != nil {
			out = append(out, a.sess)
		}
	}
	return out
}

// CountActive returns the live session count for sessions.max_concurrent
// monitoring.
func (m *Manager) CountActive() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.sessions {
		if a != nil {
			n++
		}
	}
	return n
}

// RequestCompact signals the session's agent loop that POST /compact was
// invoked. Coalesces if a request is already pending (channel buffer is 1).
func (m *Manager) RequestCompact(id string) error {
	m.mu.Lock()
	active, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok || active == nil {
		return ErrNotFound
	}
	select {
	case active.sess.CompactRequest <- struct{}{}:
	default:
	}
	return nil
}

// Cancel triggers the session's context cancel — the agent loop sees ctx.Done
// and runs its drain path (synthesise cancelled tool_results, emit
// `interrupted`, transition to Idle). The session is *not* removed; it can
// receive further user_messages.
//
// Cancel is idempotent: repeated calls are no-ops.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	active, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok || active == nil {
		return ErrNotFound
	}
	active.cancel()
	return nil
}

// DeleteSession cancels the agent loop, waits up to drainTimeout for it to
// exit, then removes the entry from the manager and (for non-ephemeral)
// marks deleted_at in the store.
func (m *Manager) DeleteSession(ctx context.Context, id string, drainTimeout time.Duration) error {
	m.mu.Lock()
	active, ok := m.sessions[id]
	if !ok || active == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	active.cancel()
	if drainTimeout <= 0 {
		drainTimeout = 5 * time.Second
	}
	select {
	case <-active.done:
	case <-time.After(drainTimeout):
		// Goroutine wedged past timeout. We've already removed the entry; let
		// the goroutine leak — callers (e.g. graceful shutdown) get a clean
		// state from the manager's perspective.
	}

	if !active.sess.Ephemeral && m.store != nil {
		row := &store.Session{
			ID:        active.sess.ID,
			ParentID:  active.sess.ParentID,
			State:     store.SessionState(active.sess.State()),
			Workdir:   active.sess.Workdir,
			EnvJSON:   "{}",
			AgentType: active.sess.AgentType,
			Model:     active.sess.Model,
			Ephemeral: false,
			CreatedAt: active.sess.CreatedAt,
			UpdatedAt: time.Now().UTC(),
		}
		// Best-effort: re-marshal env in case it changed.
		if env, err := json.Marshal(active.sess.Env); err == nil {
			row.EnvJSON = string(env)
		}
		if err := m.store.DeleteSession(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("session: store delete: %w", err)
		}
		// Update final state before the soft delete already wrote deleted_at.
		_ = m.store.UpdateSession(ctx, row)
	}

	// Drain the channels so any pending goroutine that races a final send
	// doesn't block on a closed-but-buffered channel. We don't close them —
	// the goroutine that owns them already exited.
	return nil
}

// Shutdown cancels every session and waits for their goroutines to exit, up
// to drainTimeout total. Used by graceful HTTP shutdown.
func (m *Manager) Shutdown(drainTimeout time.Duration) {
	m.mu.Lock()
	actives := make([]*activeSession, 0, len(m.sessions))
	for _, a := range m.sessions {
		if a != nil {
			actives = append(actives, a)
		}
	}
	m.mu.Unlock()

	for _, a := range actives {
		a.cancel()
	}
	deadline := time.Now().Add(drainTimeout)
	for _, a := range actives {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		select {
		case <-a.done:
		case <-time.After(remaining):
			return
		}
	}
}
