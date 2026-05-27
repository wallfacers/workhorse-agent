// Package session owns the runtime representation of one conversation: state
// machine, history, inbox/outbox channels, and the per-session cancel func.
// The state machine has six states (Idle, Thinking, AwaitPerm, Executing,
// Compacting, Cancelled) with strict transitions enforced by Transition().
//
// The agent loop in internal/agent drives a Session by reading Inbox messages,
// mutating history, and emitting Events to Outbox. The HTTP layer in Group 9
// drains Outbox into SSE.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/idgen"
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

// State re-exports store.SessionState so callers don't have to import store
// just to compare against the six string constants.
type State = store.SessionState

const (
	StateIdle       = store.SessionStateIdle
	StateThinking   = store.SessionStateThinking
	StateAwaitPerm  = store.SessionStateAwaitPerm
	StateExecuting  = store.SessionStateExecuting
	StateCompacting = store.SessionStateCompacting
	StateCancelled  = store.SessionStateCancelled
)

// ErrInvalidTransition is returned by Transition when the requested state move
// is not in the allow-list. Callers typically log and ignore — the spec treats
// invalid transitions as developer bugs, not runtime conditions.
var ErrInvalidTransition = errors.New("session: invalid state transition")

// allowedTransitions is the closed set of legal moves from the
// session-management spec. Cancelled is reachable from any state (interrupt /
// panic), which is encoded by adding it to every from-set rather than a
// wildcard so the table remains greppable.
// allowedTransitions extends the six-state spec table with two pragmatic edges
// not literally enumerated there but implied by the agent loop's behaviour:
//
//   - Idle → Compacting: manual `POST /v1/sessions/{id}/compact` while idle.
//   - Compacting → Thinking: in-turn compaction returns control to the same
//     thinking turn rather than ending it (the agent-loop spec calls
//     compaction "before each LLM call", which only makes sense mid-turn).
//
// Both edges are documented in design.md; without them the loop would deadlock
// or wrongly drop the user's turn.
var allowedTransitions = map[State]map[State]struct{}{
	StateIdle: {
		StateThinking:   {},
		StateCompacting: {},
		StateCancelled:  {},
	},
	StateThinking: {
		StateAwaitPerm:  {},
		StateExecuting:  {},
		StateCompacting: {},
		StateIdle:       {},
		StateCancelled:  {},
	},
	StateAwaitPerm: {
		StateExecuting: {},
		StateThinking:  {},
		StateCancelled: {},
		StateIdle:      {},
	},
	StateExecuting: {
		StateThinking:  {},
		StateCancelled: {},
		StateIdle:      {},
	},
	StateCompacting: {
		StateIdle:      {},
		StateThinking:  {},
		StateCancelled: {},
	},
	StateCancelled: {
		StateIdle: {},
	},
}

// ClientMessageType enumerates the five Client → Server message types defined
// by the api-protocol spec. The HTTP layer parses incoming JSON, validates the
// type, and pushes a ClientMessage into Session.Inbox.
type ClientMessageType string

const (
	ClientUserMessage        ClientMessageType = "user_message"
	ClientPermissionDecision ClientMessageType = "permission_decision"
	ClientInterrupt          ClientMessageType = "interrupt"
	ClientPing               ClientMessageType = "ping"
	ClientContextUpdate      ClientMessageType = "context_update"
)

// ClientMessage is the parsed form of one Client → Server message. Payload
// holds the type-specific fields; the agent loop type-asserts based on Type.
type ClientMessage struct {
	Type    ClientMessageType
	Payload json.RawMessage
}

// UserMessagePayload is the schema for ClientUserMessage.Payload.
type UserMessagePayload struct {
	Content string `json:"content"`
}

// PermissionDecisionPayload is the schema for ClientPermissionDecision.Payload.
type PermissionDecisionPayload struct {
	RequestID string                   `json:"request_id"`
	Decision  store.PermissionDecision `json:"decision"`
}

// Event is one Server → Client event the agent loop pushes to Outbox. Type is
// one of the 11 event names from api-protocol; Payload is the JSON-marshaled
// type-specific fields without the wrapper (type / idx / session_id).
//
// The HTTP layer wraps the event into the final SSE frame; Idx is filled in
// by emit() against the session's monotonic counter.
type Event struct {
	Type      string
	SessionID string
	Idx       int64
	Payload   map[string]any
	CreatedAt time.Time
}

// MarshalJSON flattens Payload into the top-level JSON object so the wire form
// matches the api-protocol spec: {"type":..., "idx":..., "session_id":...,
// <payload fields>}. Fields in Payload take precedence over the wrapper —
// callers must not collide on type/idx/session_id keys.
func (e Event) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(e.Payload)+3)
	for k, v := range e.Payload {
		out[k] = v
	}
	out["type"] = e.Type
	out["idx"] = e.Idx
	out["session_id"] = e.SessionID
	return json.Marshal(out)
}

// PendingToolUse records a tool_use the assistant emitted but for which the
// loop has not yet appended a matching tool_result. On cancel or panic the
// agent loop synthesises a cancelled tool_result for each entry here so the
// next provider request sees fully-paired blocks.
type PendingToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Session is the in-memory representation of one conversation. Construction
// happens via Manager.CreateSession; direct construction is allowed for tests.
//
// Fields fall into three categories:
//
//   - Immutable after construction: ID, ParentID, Workdir, Env, Ephemeral,
//     Model, ProviderName, AgentType, CreatedAt, Inbox, Outbox.
//   - Guarded by mu: state, history, idx, pending, allowed, updatedAt.
//   - Goroutine-owned: ctx / cancel (created in Start, closed in Stop).
type Session struct {
	ID           string
	ParentID     string
	Workdir      string
	Env          map[string]string
	Ephemeral    bool
	Model        string
	ProviderName string
	AgentType    string
	CreatedAt    time.Time

	// Depth is 0 for top-level sessions and parent.Depth+1 for children created
	// via the Dispatch tool. The multi-agent spec caps it at max_depth (default 5).
	Depth int

	// SystemPromptBase, when non-empty, overrides the loop's default base
	// prompt. Set by Dispatch when the child uses an agent_type with a custom
	// system_prompt.
	SystemPromptBase string

	// Inbox is the only input path. The HTTP layer pushes ClientMessages here;
	// the agent loop reads from it. Buffered so HTTP handlers don't block.
	Inbox chan ClientMessage

	// Outbox carries every Event the loop emits. The SSE writer drains it; in
	// tests, the test driver reads from it directly.
	Outbox chan Event

	// PermissionAnswers is the channel the permission Manager's prompt
	// callback reads from. The loop's inbox watcher routes ClientPermission
	// Decision messages here so they reach the blocked Check() call.
	PermissionAnswers chan PermissionDecisionPayload

	// CompactRequest carries manual compact requests from POST
	// /v1/sessions/{id}/compact. The agent loop only consumes it while Idle
	// (Compacting from inside a turn is a different code path inside the loop).
	// Buffered to 1 so repeated requests coalesce instead of blocking the HTTP
	// handler.
	CompactRequest chan struct{}

	// StreamMu is held by an active GET SSE handler during handover only —
	// long enough to write `: superseded` to the old writer, close it, and
	// register the new one. The handover protocol lives in internal/api; the
	// mutex lives here so it can be reached from the session handle.
	StreamMu sync.Mutex

	// store, when non-nil and !Ephemeral, is the authoritative source for
	// event idx (SQLite AUTOINCREMENT). Set via Options.Store at construction.
	store store.Store

	// MemorySnapshot holds the frozen memory content loaded at session start.
	// Immutable for the session lifetime; mid-session memory_write calls do not
	// affect this value.
	MemorySnapshot *memory.Snapshot

	mu        sync.Mutex
	state     State
	history   []provider.Message
	idx       int64
	pending   map[string]PendingToolUse
	allowed   []string
	updatedAt time.Time
}

// Options bundles construction parameters so Manager.CreateSession stays
// readable.
type Options struct {
	ParentID     string
	Workdir      string
	Env          map[string]string
	Ephemeral    bool
	Model        string
	ProviderName string
	AgentType    string
	AllowedTools []string
	// DenyTools lists tool names to exclude from AllowedTools. Applied as
	// (AllowedTools - DenyTools) in New(). Used when an agent_type declares
	// tools.deny: [Bash] or dispatch overrides it.
	DenyTools []string
	// Depth is the nesting depth in a parent→child Dispatch chain. Zero for
	// top-level sessions; the Dispatch tool sets parent.Depth+1 for children.
	Depth int
	// SystemPromptBase, when non-empty, overrides the loop's default base prompt
	// for this session. Used by Dispatch when an agent_type defines a custom
	// system_prompt.
	SystemPromptBase string
	// Store is the persistence backend for non-ephemeral sessions; Emit/EmitNow
	// route through it so the events table is the authoritative idx source.
	// nil means "in-memory only" (ephemeral or unit-test sessions).
	Store store.Store
	// InboxBuffer / OutboxBuffer override the channel capacities. Zero leaves
	// them at the package defaults below.
	InboxBuffer  int
	OutboxBuffer int
}

const (
	defaultInboxBuffer  = 16
	defaultOutboxBuffer = 256
)

// New constructs a Session in StateIdle with a fresh ULID. Channels are
// created with the configured (or default) buffers. Caller is responsible for
// later calling Stop to release goroutines.
func New(opts Options) *Session {
	now := time.Now().UTC()
	inb := opts.InboxBuffer
	if inb <= 0 {
		inb = defaultInboxBuffer
	}
	out := opts.OutboxBuffer
	if out <= 0 {
		out = defaultOutboxBuffer
	}
	envCopy := map[string]string{}
	for k, v := range opts.Env {
		envCopy[k] = v
	}
	allowed := applyDenyFilter(opts.AllowedTools, opts.DenyTools)
	return &Session{
		ID:                idgen.NewULID(),
		ParentID:          opts.ParentID,
		Workdir:           opts.Workdir,
		Env:               envCopy,
		Ephemeral:         opts.Ephemeral,
		Model:             opts.Model,
		ProviderName:      opts.ProviderName,
		AgentType:         opts.AgentType,
		Depth:             opts.Depth,
		SystemPromptBase:  opts.SystemPromptBase,
		CreatedAt:         now,
		Inbox:             make(chan ClientMessage, inb),
		Outbox:            make(chan Event, out),
		PermissionAnswers: make(chan PermissionDecisionPayload, 4),
		CompactRequest:    make(chan struct{}, 1),
		store:             opts.Store,
		state:             StateIdle,
		history:           nil,
		pending:           map[string]PendingToolUse{},
		allowed:           allowed,
		updatedAt:         now,
	}
}

// applyDenyFilter computes (allowed - denied). When allowed is empty (meaning
// "all tools"), the result is nil (also "all tools"). When allowed is non-empty
// and denied is non-empty, denied tools are removed.
func applyDenyFilter(allowed, denied []string) []string {
	if len(denied) == 0 {
		return append([]string(nil), allowed...)
	}
	if len(allowed) == 0 {
		return nil
	}
	ban := make(map[string]struct{}, len(denied))
	for _, t := range denied {
		ban[t] = struct{}{}
	}
	out := make([]string, 0, len(allowed))
	for _, t := range allowed {
		if _, ok := ban[t]; !ok {
			out = append(out, t)
		}
	}
	return out
}

// State returns the current state under the session mutex.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// UpdatedAt returns the last-modified timestamp.
func (s *Session) UpdatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updatedAt
}

// Transition moves state from `from` to `to` atomically. Returns
// ErrInvalidTransition if the move isn't on the allow-list or the current
// state doesn't match `from`.
func (s *Session) Transition(from, to State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != from {
		return fmt.Errorf("%w: have %q, want %q", ErrInvalidTransition, s.state, from)
	}
	allowed, ok := allowedTransitions[from]
	if !ok {
		return fmt.Errorf("%w: no outgoing edges from %q", ErrInvalidTransition, from)
	}
	if _, ok := allowed[to]; !ok {
		return fmt.Errorf("%w: %q -> %q not allowed", ErrInvalidTransition, from, to)
	}
	s.state = to
	s.updatedAt = time.Now().UTC()
	return nil
}

// ForceTransition skips the `from` check; used by the panic recovery path
// where the prior state is unknown (we might be mid-transition when panic
// fires). The `to` value must still be on the allow-list of the current state.
func (s *Session) ForceTransition(to State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed, ok := allowedTransitions[s.state]
	if !ok {
		return fmt.Errorf("%w: no outgoing edges from %q", ErrInvalidTransition, s.state)
	}
	if _, ok := allowed[to]; !ok {
		return fmt.Errorf("%w: %q -> %q not allowed", ErrInvalidTransition, s.state, to)
	}
	s.state = to
	s.updatedAt = time.Now().UTC()
	return nil
}

// History returns a shallow copy of the message slice. Callers must not mutate
// individual ContentBlocks; the agent loop owns those.
func (s *Session) History() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]provider.Message, len(s.history))
	copy(out, s.history)
	return out
}

// AppendMessage appends one message to the history under the session mutex.
func (s *Session) AppendMessage(m provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, m)
	s.updatedAt = time.Now().UTC()
}

// ReplaceHistory swaps the entire history slice. Used by the compactor after a
// summarising pass.
func (s *Session) ReplaceHistory(msgs []provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append([]provider.Message(nil), msgs...)
	s.updatedAt = time.Now().UTC()
}

// AllowedTools returns a copy of the per-session AllowedTools filter, or nil
// when no filter is set (every registered tool is exposed).
func (s *Session) AllowedTools() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.allowed) == 0 {
		return nil
	}
	out := make([]string, len(s.allowed))
	copy(out, s.allowed)
	return out
}

// SetAllowedTools implements tools.ModifierTarget so LoadSkill (Group 12) can
// shrink the per-session tool set after it loads a skill.
func (s *Session) SetAllowedTools(tools []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowed = append([]string(nil), tools...)
	s.updatedAt = time.Now().UTC()
}

// MarkToolUsePending records a tool_use the assistant just emitted so a later
// cancel/panic can synthesise a matching tool_result.
func (s *Session) MarkToolUsePending(id, name string, input json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[id] = PendingToolUse{ID: id, Name: name, Input: input}
}

// ClearToolUsePending drops the entry once a real tool_result has been
// appended to history. Idempotent — ignores unknown IDs.
func (s *Session) ClearToolUsePending(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

// DrainPendingToolUses returns the current set and clears it. Used by the
// cancel / panic paths to know which synthetic tool_results to append.
func (s *Session) DrainPendingToolUses() []PendingToolUse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	out := make([]PendingToolUse, 0, len(s.pending))
	for _, p := range s.pending {
		out = append(out, p)
	}
	s.pending = map[string]PendingToolUse{}
	return out
}

// NextIdx assigns the next monotonically increasing event sequence number. In
// persistent mode the HTTP layer will call store.AppendEvent which returns the
// canonical idx; the in-memory counter here is used for ephemeral sessions and
// for unit tests.
func (s *Session) NextIdx() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idx++
	return s.idx
}

// Emit pushes one event to Outbox. In persistent mode (store != nil and
// !Ephemeral) the idx comes from store.AppendEvent so the events table is the
// authoritative source; otherwise the in-memory counter is used.
// Blocks if Outbox is full, but respects ctx for cancellation.
func (s *Session) Emit(ctx context.Context, evType string, payload map[string]any) error {
	createdAt := time.Now().UTC()
	idx, err := s.assignIdx(ctx, evType, payload, createdAt)
	if err != nil {
		return err
	}
	e := Event{
		Type:      evType,
		SessionID: s.ID,
		Idx:       idx,
		Payload:   payload,
		CreatedAt: createdAt,
	}
	select {
	case s.Outbox <- e:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EmitNow is the non-blocking variant: drops the event if Outbox is full. Used
// for graceful-shutdown / panic paths where blocking would deadlock. The event
// is still persisted to the store (audit log must remain complete even when
// the SSE channel is back-pressured).
func (s *Session) EmitNow(evType string, payload map[string]any) bool {
	createdAt := time.Now().UTC()
	idx, err := s.assignIdx(context.Background(), evType, payload, createdAt)
	if err != nil {
		return false
	}
	e := Event{
		Type:      evType,
		SessionID: s.ID,
		Idx:       idx,
		Payload:   payload,
		CreatedAt: createdAt,
	}
	select {
	case s.Outbox <- e:
		return true
	default:
		return false
	}
}

// assignIdx resolves the canonical idx for an event. When store is set and the
// session is persistent, it appends the row first and returns SQLite's
// AUTOINCREMENT value; otherwise it bumps the in-memory counter. In persistent
// mode the in-memory counter is kept aligned so a later switch to no-store
// would still produce increasing values.
func (s *Session) assignIdx(ctx context.Context, evType string, payload map[string]any, createdAt time.Time) (int64, error) {
	if s.store == nil || s.Ephemeral {
		return s.NextIdx(), nil
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("session: marshal event payload: %w", err)
	}
	idx, err := s.store.AppendEvent(ctx, &store.Event{
		SessionID:   s.ID,
		Type:        evType,
		PayloadJSON: string(payloadJSON),
		CreatedAt:   createdAt,
	})
	if err != nil {
		return 0, fmt.Errorf("session: persist event: %w", err)
	}
	s.mu.Lock()
	if idx > s.idx {
		s.idx = idx
	}
	s.mu.Unlock()
	return idx, nil
}
