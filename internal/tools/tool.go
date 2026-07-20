// Package tools defines the Tool interface and the small support types
// (Env, Result, ContextModifier) every concrete tool implementation —
// builtin or MCP-adapted — has to honour. Concrete tools live in
// internal/tools/{read,write,edit,grep,bash,dispatch,loadskill,...}.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// ToolSearchName is the canonical name of the built-in ToolSearch tool. It is
// referenced from several packages (the tool itself, the agent loop's
// deferral filter, and the session's discovered-set reconstruction) so the
// constant lives here to avoid an import cycle.
const ToolSearchName = "ToolSearch"

// Deferrable is an optional interface a Tool may implement to participate in
// tool search. A tool whose ShouldDefer() returns true is "deferred": its full
// schema is withheld from the model's tool list until ToolSearch surfaces it.
// Tools that do not implement Deferrable are never deferred (always loaded
// upfront).
type Deferrable interface {
	ShouldDefer() bool
}

// ToolInfo is the minimal description of a deferred tool exposed to ToolSearch
// for keyword scoring and schema rendering.
type ToolInfo struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolCatalog is the read-only view the agent loop hands to ToolSearch (via
// Env.ToolCatalog) so it can score and render the session's currently
// deferred-and-allowed tools. The set it returns MUST match what the loop
// withheld from the model's tool list this turn.
type ToolCatalog interface {
	DeferredTools() []ToolInfo
}

// Tool is the interface the orchestrator drives. Methods are read at
// registration time (Name / Description / InputSchema / IsReadOnly /
// CanRunInParallel / DefaultTimeout) and per-call (Run).
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	// IsReadOnly is true when the tool produces no state changes —
	// orchestrator may run multiple read-only calls concurrently.
	IsReadOnly() bool
	// CanRunInParallel is true when concurrent execution with siblings is
	// safe. Read-only implies this; writers may opt in if they namespace
	// their writes (Dispatch and LoadSkill do).
	CanRunInParallel() bool
	// DefaultTimeout is the lowest-priority timeout source for the
	// orchestrator's context.WithTimeout wrapper. Returns 0 to mean "inherit
	// tools.default_timeout_seconds".
	DefaultTimeout() time.Duration
	// Run executes one tool call. The orchestrator owns ctx and will cancel
	// on session cancel or timeout; tools are expected to honour it.
	Run(ctx context.Context, env *Env, input json.RawMessage) (*Result, error)
}

// Env is the per-call environment handed to a tool. Tools must NOT mutate any
// field — the orchestrator may reuse the struct across concurrent calls.
type Env struct {
	SessionID string
	Workdir   string
	// Env is the filtered process environment Bash and MCP child processes
	// inherit. It has already passed through internal/tools/bash/envfilter.
	Env    map[string]string
	Logger *slog.Logger
	// ExtAgentRegistry holds the per-session external agent registry.
	// Typed as any to avoid import cycles; the ExternalAgent tool
	// type-asserts this to *extagent.Registry.
	ExtAgentRegistry any
	// TaskList holds the per-session task list store. Typed as any to avoid
	// import cycles; the TodoWrite tool type-asserts this to *tasklist.Store.
	TaskList any
	// InstructionResolver handles proximity injection for the Read tool.
	// Typed as any to avoid import cycles; the Read tool type-asserts this
	// to *instructions.Resolver.
	InstructionResolver any
	// ToolCatalog exposes the session's currently deferred tools to the
	// ToolSearch tool. Typed as any to avoid import cycles; ToolSearch
	// type-asserts this to ToolCatalog. nil when tool search is inactive.
	ToolCatalog any
	// Delegations holds the process-wide delegation manager. Typed as any to
	// avoid import cycles; the delegate/delegation_read/delegation_list tools
	// type-assert this to *delegation.Manager.
	Delegations any
	// Schedules holds the store for schedule management. Typed as any to avoid
	// import cycles; the schedule_* tools type-assert this to store.Store.
	Schedules any
}

// Result is the outcome of one tool call. Output is the canonical string the
// agent loop feeds back to the LLM (already truncated). IsError marks the
// result as a failure that should be reported via tool_result.is_error = true.
//
// Modifier, when non-nil, mutates session state *after* the result reaches
// the LLM (deferred so a same-batch sibling tool's view of allowed_tools
// isn't disturbed mid-batch).
type Result struct {
	Output   string
	IsError  bool
	Modifier ContextModifier
}

// ErrorResultJSON builds a tool Result carrying a structured {"error": msg}
// JSON envelope with IsError set. Tools that emit machine-readable errors
// (TodoWrite, memory_*, session_search) share this single constructor so the
// envelope shape stays consistent across them.
func ErrorResultJSON(msg string) *Result {
	return &Result{Output: fmt.Sprintf(`{"error":%q}`, msg), IsError: true}
}

// ContextModifier mutates session-level state when a tool wants to e.g.
// restrict the AllowedTools set (LoadSkill). The session manager implements
// ModifierTarget and applies modifiers after the tool batch settles.
type ContextModifier interface {
	Apply(ModifierTarget) error
}

// ModifierTarget is the small interface the session manager exposes to
// ContextModifier implementations.
type ModifierTarget interface {
	SetAllowedTools(tools []string)
	// MarkToolsDiscovered records that the named deferred tools have been
	// surfaced via ToolSearch and should appear with full schema in every
	// subsequent turn of this session.
	MarkToolsDiscovered(names []string)
}
