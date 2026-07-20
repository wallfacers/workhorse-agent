package store

import (
	"context"
	"errors"
	"time"
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
	// ListAllSessions returns non-deleted sessions across ALL projects, each with
	// its message-count and last-message preview, newest-updated first. Backs the
	// cross-project session-management view (GET /v1/sessions with no workdir).
	ListAllSessions(ctx context.Context) ([]*SessionSummary, error)
	// ListProjects returns the distinct workdirs that have at least one
	// non-deleted session, with per-project session counts.
	ListProjects(ctx context.Context) ([]*Project, error)
	UpdateSession(ctx context.Context, s *Session) error
	// UpdateSessionTitle updates only the title and updated_at for a session
	// without requiring a full-row rebuild. Returns ErrNotFound if the session
	// does not exist.
	UpdateSessionTitle(ctx context.Context, id, title string) error
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
	// CountMessages returns the number of messages for a session.
	CountMessages(ctx context.Context, sessionID string) (int, error)
	// MarkMessageInterrupted sets interrupted=1 on a message by its ULID
	// primary key. No-op (no error) when the row does not exist.
	MarkMessageInterrupted(ctx context.Context, messageID string) error

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

	// --- Delegation CRUD (001-agent-orchestration US1) ---
	CreateDelegation(ctx context.Context, d *Delegation) error
	GetDelegation(ctx context.Context, id string) (*Delegation, error)
	ListDelegations(ctx context.Context, sessionID string) ([]*Delegation, error)
	// CountRunningDelegations returns the global count of status='running'
	// delegations; used to enforce the fixed concurrency cap across sessions.
	CountRunningDelegations(ctx context.Context) (int, error)
	// CompleteDelegation transitions a delegation to 'complete', recording the
	// derived title/summary, full result, and completion timestamp.
	CompleteDelegation(ctx context.Context, id, title, summary, result string) error
	// FailDelegation transitions a delegation to 'error', recording the reason
	// and (optionally) a partial result detail.
	FailDelegation(ctx context.Context, id, errMsg, result string) error
	// ClaimPendingNotifications atomically selects every finished-but-unnotified
	// delegation for the session and marks notified_at within one transaction,
	// returning the claimed set. notified_at is set BEFORE the notice is injected
	// into history, so a crash between claim and inject loses one notification
	// rather than duplicating it (prefer-dropping-over-duplication).
	ClaimPendingNotifications(ctx context.Context, sessionID string) ([]*Delegation, error)
	// ReapRunningDelegations marks every still-running delegation as failed with
	// error "server restarted". Called at startup so delegations orphaned by a
	// server restart never stay 'running' forever.
	ReapRunningDelegations(ctx context.Context) error

	// --- Schedule CRUD (001-agent-orchestration US3) ---
	CreateSchedule(ctx context.Context, s *Schedule) error
	GetSchedule(ctx context.Context, id string) (*Schedule, error)
	ListSchedules(ctx context.Context) ([]*Schedule, error)
	// DeleteSchedule removes a schedule and cascades its run log in one
	// transaction. Returns ErrNotFound if the schedule does not exist.
	DeleteSchedule(ctx context.Context, id string) error
	// TouchScheduleRun stamps last_run_at; for a one-shot schedule (run_at set)
	// it also flips enabled to 0 so it never fires again (FR-019).
	TouchScheduleRun(ctx context.Context, id string, at time.Time) error
	// CreateScheduleRun inserts a running run row, prunes the plan to the most
	// recent 20 runs in the same transaction, and returns the new run id.
	CreateScheduleRun(ctx context.Context, r *ScheduleRun) (int64, error)
	// FinishScheduleRun records the terminal status, output tail, error, and
	// completion timestamp for a run.
	FinishScheduleRun(ctx context.Context, id int64, status ScheduleRunStatus, outputTail, errMsg string) error
	// ListScheduleRuns returns the most recent runs for a plan (limit clamped
	// to 1..20; <=0 means the default of 5).
	ListScheduleRuns(ctx context.Context, scheduleID string, limit int) ([]*ScheduleRun, error)
	// PruneScheduleRuns deletes all but the most recent `keep` runs for a plan.
	PruneScheduleRuns(ctx context.Context, scheduleID string, keep int) error

	// Close releases the underlying handle. Calling Close more than once is
	// safe; the second call is a no-op.
	Close() error
}
