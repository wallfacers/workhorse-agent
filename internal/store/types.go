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
	Title     string
	Ephemeral bool
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
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
