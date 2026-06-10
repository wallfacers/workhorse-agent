package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
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
	workdir, err := ValidateWorkdir(opts.Workdir)
	if err != nil {
		return nil, err
	}
	opts.Workdir = workdir

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
	metadataJSON := ""
	if len(s.Metadata) > 0 {
		raw, err := json.Marshal(s.Metadata)
		if err != nil {
			return fmt.Errorf("session: marshal metadata: %w", err)
		}
		metadataJSON = string(raw)
	}
	row := &store.Session{
		ID:           s.ID,
		ParentID:     s.ParentID,
		State:        store.SessionState(s.State()),
		Workdir:      s.Workdir,
		EnvJSON:      string(envJSON),
		AgentType:    s.AgentType,
		Model:        s.Model,
		Provider:     s.ProviderName,
		Title:        s.Title(),
		Instructions: s.Instructions,
		MetadataJSON: metadataJSON,
		Ephemeral:    false,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.CreatedAt,
	}
	if err := m.store.CreateSession(ctx, row); err != nil {
		return fmt.Errorf("session: persist: %w", err)
	}
	return nil
}

// GetOrHydrate returns the live session, hydrating it from the store if it was
// persisted but not currently loaded (e.g. after a restart, or a session the
// user switched away from). The whole operation is done under m.mu: the store
// already serialises access via a single connection, so holding the lock across
// the (fast, local) reads keeps hydration race-free without a placeholder slot.
// Returns ErrNotFound for unknown or soft-deleted sessions.
func (m *Manager) GetOrHydrate(ctx context.Context, id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if a, ok := m.sessions[id]; ok {
		if a == nil {
			// A concurrent CreateSession reserved this freshly-minted id; an
			// existing session's id never collides with it.
			return nil, ErrNotFound
		}
		return a.sess, nil
	}
	if m.store == nil {
		return nil, ErrNotFound
	}

	if m.maxConcurrent > 0 && len(m.sessions) >= m.maxConcurrent {
		return nil, ErrTooManyConcurrent
	}

	row, err := m.store.GetSession(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if row.DeletedAt != nil {
		return nil, ErrNotFound
	}

	sess, err := m.buildHydrated(ctx, row)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.sessions[id] = &activeSession{sess: sess, cancel: cancel, done: done}
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

// buildHydrated reconstructs an idle Session from its persisted row and
// transcript. The stored model and provider are preserved.
func (m *Manager) buildHydrated(ctx context.Context, row *store.Session) (*Session, error) {
	env := map[string]string{}
	if row.EnvJSON != "" {
		_ = json.Unmarshal([]byte(row.EnvJSON), &env)
	}
	metadata := map[string]string{}
	if row.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(row.MetadataJSON), &metadata)
	}
	sess := New(Options{
		Workdir:      row.Workdir,
		Env:          env,
		Model:        row.Model,
		ProviderName: row.Provider,
		AgentType:    row.AgentType,
		ParentID:     row.ParentID,
		Instructions: row.Instructions,
		Metadata:     metadata,
		Store:        m.store,
	})
	sess.ID = row.ID
	sess.CreatedAt = row.CreatedAt
	sess.SetTitle(row.Title)

	msgs, err := m.store.ListMessages(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("session: hydrate transcript: %w", err)
	}
	hist := make([]provider.Message, 0, len(msgs))
	for _, mm := range msgs {
		blocks, err := unmarshalContent(mm.ContentJSON)
		if err != nil {
			return nil, fmt.Errorf("session: hydrate decode message %s: %w", mm.ID, err)
		}
		hist = append(hist, provider.Message{
			Role:       provider.Role(mm.Role),
			Content:    blocks,
			StopReason: mm.StopReason,
		})
	}
	sess.RestoreHistory(hist)
	return sess, nil
}

// GetSession returns the live in-memory session. Sessions that were persisted
// but not yet loaded are hydrated on demand via GetOrHydrate (used by the
// stream handlers); GetSession itself stays live-only so read paths don't spin
// up a runner.
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

// DeleteSession removes a session and its transcript. For a live session it
// cancels the agent loop and waits up to drainTimeout for the goroutine to
// exit; then (for any persisted session, live or not) it hard-deletes the row
// and cascades to messages/events/tool_calls. Returns ErrNotFound only when the
// session is neither live nor persisted.
func (m *Manager) DeleteSession(ctx context.Context, id string, drainTimeout time.Duration) error {
	m.mu.Lock()
	active, ok := m.sessions[id]
	wasLive := ok && active != nil
	if wasLive {
		delete(m.sessions, id)
	}
	// Soft-delete the store row while holding the lock so a concurrent
	// GetOrHydrate sees DeletedAt != nil and refuses to hydrate.
	if m.store != nil {
		_ = m.store.DeleteSession(ctx, id)
	}
	m.mu.Unlock()

	if wasLive {
		active.cancel()
		if drainTimeout <= 0 {
			drainTimeout = 5 * time.Second
		}
		timer := time.NewTimer(drainTimeout)
		select {
		case <-active.done:
			timer.Stop()
		case <-timer.C:
		}
	}

	if m.store == nil {
		if !wasLive {
			return ErrNotFound
		}
		return nil
	}

	// Hard delete + cascade (add-project-sessions D6). An ephemeral live session
	// was never persisted, so a missing row there is expected, not an error.
	if err := m.store.PurgeSession(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if wasLive {
				return nil
			}
			return ErrNotFound
		}
		return fmt.Errorf("session: store purge: %w", err)
	}
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
		case <-time.NewTimer(remaining).C:
			return
		}
	}
}
