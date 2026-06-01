package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get*/Update*/Delete* methods when no row matches
// the provided primary key.
var ErrNotFound = errors.New("store: not found")

// Store is the database-agnostic persistence boundary. All methods are
// context-aware and safe for concurrent calls; the underlying SQLite backend
// uses BEGIN IMMEDIATE for writes and busy_timeout for contention.
type Store interface {
	// --- Session CRUD ---
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	ListSessions(ctx context.Context, includeDeleted bool) ([]*Session, error)
	// ListSessionsByWorkdir returns non-deleted sessions for one project path,
	// each with its message-count and last-message preview, newest-updated first.
	ListSessionsByWorkdir(ctx context.Context, workdir string) ([]*SessionSummary, error)
	// ListProjects returns the distinct workdirs that have at least one
	// non-deleted session, with per-project session counts.
	ListProjects(ctx context.Context) ([]*Project, error)
	UpdateSession(ctx context.Context, s *Session) error
	DeleteSession(ctx context.Context, id string) error
	// PurgeSession hard-deletes a session and cascades to its messages, events,
	// and tool_calls (the "delete also removes the transcript" contract).
	// Returns ErrNotFound if no such row exists.
	PurgeSession(ctx context.Context, id string) error
	// CountActiveSessions returns the count of sessions whose DeletedAt is
	// NULL. Used to enforce sessions.max_concurrent.
	CountActiveSessions(ctx context.Context) (int, error)

	// --- Message CRUD ---
	AppendMessage(ctx context.Context, m *Message) error
	ListMessages(ctx context.Context, sessionID string) ([]*Message, error)
	// ReplaceMessages atomically swaps a session's whole transcript (compaction
	// rewrite). Passing an empty slice clears the transcript.
	ReplaceMessages(ctx context.Context, sessionID string, msgs []*Message) error

	// --- Event append + incremental query ---
	// AppendEvent assigns the next idx and returns it. Callers should treat
	// the returned idx as the SSE `id:` value.
	AppendEvent(ctx context.Context, e *Event) (int64, error)
	// EventsAfter returns events whose Idx satisfies (lastIdx, snapshot].
	// snapshot==0 means "no upper bound — read up to the current tail".
	// Used by GET /v1/sessions/{id}/stream resumption: callers take a snapshot
	// under the session write lock, replay (lastIdx, snapshot], then switch
	// to the live channel for idx > snapshot.
	EventsAfter(ctx context.Context, sessionID string, lastIdx, snapshot int64) ([]*Event, error)
	// MaxEventIdx returns the highest Idx for a session, or 0 if none.
	MaxEventIdx(ctx context.Context, sessionID string) (int64, error)

	// --- ToolCall CRUD ---
	AppendToolCall(ctx context.Context, t *ToolCall) error
	UpdateToolCall(ctx context.Context, t *ToolCall) error
	ListToolCalls(ctx context.Context, sessionID string) ([]*ToolCall, error)

	// --- Permission CRUD ---
	// SavePermission persists a rule. Scope=ScopeOnce/ScopeSession callers
	// may keep the rule in memory instead; SQLite is only the home for
	// ScopePermanent rules.
	SavePermission(ctx context.Context, p *Permission) error
	// ListPermissions returns rules applicable to sessionID: all permanent
	// rules (SessionID="") plus rules scoped to this session.
	ListPermissions(ctx context.Context, sessionID string) ([]*Permission, error)
	DeletePermission(ctx context.Context, id string) error

	// Close releases the underlying handle. Calling Close more than once is
	// safe; the second call is a no-op.
	Close() error
}
