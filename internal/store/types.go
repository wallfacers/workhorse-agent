// Package store defines the workhorse-agent persistence boundary: a small set of
// value types and a Store interface that every concrete backend
// (currently only SQLite via modernc.org/sqlite) must implement.
package store

import "time"

// SessionState matches the six-state machine described in the session-
// management spec. The string values are the on-disk encoding; callers must
// not invent new values without updating the spec first.
type SessionState string

const (
	SessionStateIdle       SessionState = "idle"
	SessionStateThinking   SessionState = "thinking"
	SessionStateAwaitPerm  SessionState = "await_perm"
	SessionStateExecuting  SessionState = "executing"
	SessionStateCompacting SessionState = "compacting"
	SessionStateCancelled  SessionState = "cancelled"
)

// Session is the persisted form of an agent conversation. ID and ParentID are
// ULIDs (top-level agent has ParentID == ""). EnvJSON holds a JSON-encoded
// map[string]string and is filtered through internal/tools/bash/envfilter
// before any child process inherits it.
type Session struct {
	ID        string
	ParentID  string
	State     SessionState
	Workdir   string
	EnvJSON   string
	AgentType string
	Model     string
	Provider  string
	Title     string
	// Instructions is the optional per-session extra instruction text the
	// creator supplied; it joins the system prompt's dynamic Instructions
	// segment. MetadataJSON is an opaque caller-supplied string→string map.
	Instructions string
	MetadataJSON string
	Ephemeral    bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// SessionSummary is a session plus the two aggregates the project-scoped
// listing needs (add-project-sessions SessionMeta). MessageCount and
// LastMessagePreview are derived from the messages table; the live idle|running
// status is overlaid by the manager, not stored here.
type SessionSummary struct {
	Session
	MessageCount       int
	LastMessagePreview string
}

// Project is a distinct workdir that has at least one non-deleted session.
type Project struct {
	Path         string
	SessionCount int
	UpdatedAt    time.Time
}

// Message represents one turn in a session's conversation. Content is the
// JSON-serialised []ContentBlock owned by internal/provider.
type Message struct {
	ID          string
	SessionID   string
	Role        string
	ContentJSON string
	TokenCount  int
	StopReason  string
	Interrupted bool
	CreatedAt   time.Time
}

// Event is the unit emitted to SSE clients. Idx is the int64 monotonically
// increasing primary key (AUTOINCREMENT) that doubles as the SSE `id:` field
// for Last-Event-ID resumption. Type is one of the 11 ServerEvent names.
type Event struct {
	Idx         int64
	SessionID   string
	Type        string
	PayloadJSON string
	CreatedAt   time.Time
}

// ToolCall captures one tool invocation. ID is the tool_use_id reported by the
// provider (or generated locally for client-initiated calls).
type ToolCall struct {
	ID         string
	SessionID  string
	MessageID  string
	Tool       string
	InputJSON  string
	OutputJSON string
	IsError    bool
	StartedAt  time.Time
	FinishedAt *time.Time
}

// PermissionDecision enumerates the five allowed user responses defined by the
// permission-control spec.
type PermissionDecision string

const (
	DecisionAllowOnce      PermissionDecision = "allow_once"
	DecisionAllowSession   PermissionDecision = "allow_session"
	DecisionAllowPermanent PermissionDecision = "allow_permanent"
	DecisionDeny           PermissionDecision = "deny"
	DecisionDenyPermanent  PermissionDecision = "deny_permanent"
)

// PermissionScope says where the rule lives. Once/Session rules are only kept
// in memory; Permanent rules are persisted (SessionID="" means global).
type PermissionScope string

const (
	ScopeOnce      PermissionScope = "once"
	ScopeSession   PermissionScope = "session"
	ScopePermanent PermissionScope = "permanent"
)

// Permission is one matched rule. Pattern is a glob over the tool's natural
// resource (file path for Read/Edit/Write, command string for Bash, etc.).
type Permission struct {
	ID        string
	SessionID string
	Tool      string
	Pattern   string
	Decision  PermissionDecision
	Scope     PermissionScope
	CreatedAt time.Time
}

// DelegationStatus is the lifecycle state of a background delegation.
type DelegationStatus string

const (
	DelegationRunning  DelegationStatus = "running"
	DelegationComplete DelegationStatus = "complete"
	DelegationError    DelegationStatus = "error"
)

// Delegation is one background read-only sub-agent task (001-agent-orchestration
// US1). Result holds the sub-agent's final assistant text on success, or the
// error detail when Status == error. NotifiedAt drives exactly-once completion
// notice injection: nil means pending, a non-nil value means the notice has
// already been appended to the parent session's history.
type Delegation struct {
	ID          string
	SessionID   string
	Description string
	Prompt      string
	Workdir     string
	Status      DelegationStatus
	Title       string
	Summary     string
	Result      string
	Error       string
	StartedAt   time.Time
	CompletedAt *time.Time
	NotifiedAt  *time.Time
}

// Schedule is one self-serve automation plan (001-agent-orchestration US3).
// Exactly one of Cron (five-field repeating expression) or RunAt (one-shot
// instant) is set. Enabled flips to false after a one-shot fires once (FR-019).
// LastRunAt is the same-minute de-dupe key the scheduler tick checks.
type Schedule struct {
	ID          string
	Name        string
	Instruction string
	Cron        string
	RunAt       *time.Time
	Workdir     string
	Enabled     bool
	CreatedAt   time.Time
	LastRunAt   *time.Time
}

// ScheduleRunStatus is the lifecycle state of one schedule execution.
type ScheduleRunStatus string

const (
	ScheduleRunRunning  ScheduleRunStatus = "running"
	ScheduleRunComplete ScheduleRunStatus = "complete"
	ScheduleRunError    ScheduleRunStatus = "error"
)

// ScheduleRun is one triggered execution of a Schedule (001-agent-orchestration
// US3). SessionID is the persisted unattended session the run created (replayable
// via the history API); OutputTail holds the final assistant text truncated to
// 64 KiB.
type ScheduleRun struct {
	ID          int64
	ScheduleID  string
	SessionID   string
	StartedAt   time.Time
	CompletedAt *time.Time
	Status      ScheduleRunStatus
	OutputTail  string
	Error       string
}
