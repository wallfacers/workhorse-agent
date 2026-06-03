package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/api/protocol"
	"github.com/wallfacers/workhorse-agent/internal/idgen"
	"github.com/wallfacers/workhorse-agent/internal/instructions"
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/prompt"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/frontend"
)

// LoopConfig groups the dial-able knobs of the agent loop. Every field has a
// safe non-zero default applied by ApplyDefaults so a zero-value LoopConfig
// still produces a working loop (handy in tests).
type LoopConfig struct {
	Model     string
	MaxTokens int

	AutoCompactRatio   float64
	CompactRecentKeep  int
	MaxHistoryTokens   int
	CancelDrainTimeout time.Duration

	Retry RetryConfig

	ThinkingEnabled      bool
	ThinkingBudgetTokens int
}

// ApplyDefaults fills zero-valued fields with the configuration spec defaults.
func (c *LoopConfig) ApplyDefaults() {
	if c.MaxTokens == 0 {
		c.MaxTokens = 4096
	}
	if c.CompactRecentKeep == 0 {
		c.CompactRecentKeep = 8
	}
	if c.MaxHistoryTokens == 0 {
		c.MaxHistoryTokens = 200_000
	}
	if c.CancelDrainTimeout == 0 {
		c.CancelDrainTimeout = 5 * time.Second
	}
	if c.Retry.Attempts == 0 && len(c.Retry.Backoff) == 0 {
		c.Retry = DefaultRetryConfig()
	}
}

// Loop is the per-session agent engine. One Loop is constructed per session in
// the SessionManager's RunnerFactory; its Run method blocks for the lifetime
// of the session goroutine.
//
// Loop satisfies session.Runner. The fields are configured at construction
// time and treated as immutable thereafter; per-call state lives on the
// Session struct or in local turn variables.
type Loop struct {
	Session      *session.Session
	Provider     provider.Provider
	Orchestrator *Orchestrator
	Permissions  *permission.Manager
	Compactor    *Compactor
	Logger       *slog.Logger

	SystemPromptBase string
	Tools            []provider.ToolSchema
	ToolEnv          *tools.Env
	Registry         *tools.Registry

	// ImplicitTriggerInterceptor, when non-nil, is invoked before
	// checkPermissions on every tool batch. It is the hook the
	// adapter-generator Plan A flow uses to intercept ExternalAgent calls
	// against unknown agent_names whose binary resolves on PATH (see
	// add-llm-adapter-generator §10). The interceptor may return synthetic
	// tool results for some calls (which skip the orchestrator entirely)
	// and a filtered list for the rest.
	ImplicitTriggerInterceptor ImplicitTriggerHook

	Config LoopConfig

	// activeTurnCancel holds the cancel func of the in-progress turn so an
	// inbox watcher running in a sibling goroutine can interrupt it. nil when
	// no turn is in flight.
	activeTurnCancel atomic.Pointer[turnHandle]

	// frontendBridge holds the per-session frontend tool bridge. Lazily
	// constructed on the first publish_frontend_tools. The Session's
	// Frontend field is set to this bridge so the HTTP layer can call Resolve.
	frontendBridge *frontend.Bridge

	// registryCloned is true after the per-session registry has been cloned
	// for frontend tool isolation. Subsequent publishes skip the clone.
	registryCloned bool
}

// turnHandle is the small bundle the watcher needs to interrupt and observe a
// turn. Held in an atomic.Pointer so the watcher's reads don't race the main
// goroutine's writes.
type turnHandle struct {
	cancel context.CancelFunc
}

// NewLoop builds a Loop with defaults applied. Caller must populate the
// non-defaultable fields (Session, Provider, Orchestrator).
func NewLoop(cfg LoopConfig) *Loop {
	cfg.ApplyDefaults()
	return &Loop{Config: cfg}
}

// Run is the session goroutine entry point. It selects on ctx.Done and
// Session.Inbox; user messages trigger a turn, other client messages are
// handled inline. The outer recover catches any panic that escapes
// runTurnSafe so the loop survives.
func (l *Loop) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.Session.CompactRequest:
			l.dispatchCompactIdle(ctx)
		case msg, ok := <-l.Session.Inbox:
			if !ok {
				return
			}
			l.dispatchIdle(ctx, msg)
		}
	}
}

// dispatchCompactIdle runs the manual-compact path requested by
// POST /v1/sessions/{id}/compact. The HTTP layer enforces Idle-only
// (returns 409 otherwise), but we re-check here to keep the transition
// guarded.
func (l *Loop) dispatchCompactIdle(ctx context.Context) {
	if l.Compactor == nil {
		return
	}
	if err := l.Session.Transition(session.StateIdle, session.StateCompacting); err != nil {
		return
	}
	history := l.Session.History()
	newHistory, result, ok, err := l.Compactor.Compact(ctx, history)
	if err != nil {
		l.logger().Warn("agent: manual compact failed", "err", err)
		_ = l.Session.Transition(session.StateCompacting, session.StateIdle)
		return
	}
	if !ok {
		_ = l.Session.Transition(session.StateCompacting, session.StateIdle)
		return
	}
	l.Session.ReplaceHistory(context.Background(), newHistory)
	_ = l.Session.Emit(ctx, "compaction", map[string]any{
		"before_messages": result.BeforeMessages,
		"after_messages":  result.AfterMessages,
		"before_tokens":   result.BeforeTokens,
		"after_tokens":    result.AfterTokens,
	})
	_ = l.Session.Transition(session.StateCompacting, session.StateIdle)
}

// dispatchIdle handles messages received while no turn is in flight. The HTTP
// layer (Group 9) only forwards user_messages when the session is in Idle; the
// other message types we still accept gracefully so tests can drive arbitrary
// orderings.
func (l *Loop) dispatchIdle(ctx context.Context, msg session.ClientMessage) {
	switch msg.Type {
	case session.ClientUserMessage:
		l.runTurnSafe(ctx, msg)
	case session.ClientPing:
		_ = l.Session.Emit(ctx, "pong", nil)
	case session.ClientPublishFrontendTools:
		l.handlePublishFrontendTools(ctx, msg)
	case session.ClientInterrupt, session.ClientPermissionDecision, session.ClientContextUpdate:
		// no-op outside a turn
	default:
		l.logger().Warn("agent: unknown client message", "type", msg.Type)
	}
}

// runTurnSafe wraps a single user_message → ... → end_turn cycle. The actual
// turn loop runs in a child goroutine so cancellation can bail past wedged
// tools (per the cancel-drain-timeout spec): if the inner loop doesn't exit
// within CancelDrainTimeout after cancel, the parent emits `cancel_timeout`
// and force-transitions to Idle, leaving the wedged goroutine to wind down on
// its own (it skips post-cancel side effects via ctx checks inside the loop).
//
// Panic recovery lives inside the turn goroutine so the synthesised
// tool_results and `internal_panic` event come from a stack that still has
// access to the session's pending tool_uses (the outer goroutine doesn't).
func (l *Loop) runTurnSafe(parent context.Context, msg session.ClientMessage) {
	var u session.UserMessagePayload
	if err := json.Unmarshal(msg.Payload, &u); err != nil {
		l.logger().Warn("agent: bad user_message payload", "err", err)
		return
	}
	if err := l.Session.Transition(session.StateIdle, session.StateThinking); err != nil {
		// Session is not idle — should not happen if the HTTP layer enforces
		// the 409 rule. Emit a session_busy error and bail.
		l.emitError(parent, "session_busy", "session is not idle",
			map[string]any{"state": string(l.Session.State())}, true)
		return
	}
	l.Session.AppendMessage(context.Background(), provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.ContentBlock{{Type: provider.BlockText, Text: u.Content}},
	})

	// Derive title from the first user message if empty. The broadcast lets a
	// connected client update its session list live; manual rename goes through
	// the REST PATCH path and needs no SSE frame.
	if l.Session.Title() == "" && u.Content != "" {
		title := deriveTitle(u.Content)
		l.Session.SetTitle(title)
		l.Session.PersistTitle(parent)
		_ = l.Session.Emit(parent, string(protocol.EventSessionTitleUpdated), map[string]any{"title": title})
	}

	turnCtx, cancelTurn := context.WithCancel(parent)
	defer cancelTurn()
	handle := &turnHandle{cancel: cancelTurn}
	l.activeTurnCancel.Store(handle)
	defer l.activeTurnCancel.Store(nil)

	watcherDone := l.startInboxWatcher(turnCtx, cancelTurn)
	defer func() {
		cancelTurn() // ensure watcher exits
		<-watcherDone
	}()

	turnDone := make(chan struct{})
	go func() {
		defer close(turnDone)
		defer func() {
			if r := recover(); r != nil {
				l.handlePanic(parent, r, debug.Stack())
			}
		}()
		l.runTurnLoop(turnCtx)
	}()

	wedged := false
	select {
	case <-turnDone:
		// Either ended naturally or shut down cleanly after cancel.
	case <-turnCtx.Done():
		// Cancel just fired. Give the loop a drain budget to wind down.
		select {
		case <-turnDone:
		case <-time.After(l.Config.CancelDrainTimeout):
			wedged = true
			l.Session.EmitNow("error", map[string]any{
				"code":    "cancel_timeout",
				"message": "tool drain exceeded budget",
				"details": map[string]any{
					"phase":      "tool_drain",
					"elapsed_ms": l.Config.CancelDrainTimeout.Milliseconds(),
				},
				"recoverable": true,
			})
		}
	}

	if turnCtx.Err() != nil {
		l.finishCancelledTurn(parent)
	}

	// Always end the turn in Idle. ForceTransition skips the from-check
	// because we may have raced past several states.
	if l.Session.State() != session.StateIdle {
		_ = l.Session.ForceTransition(session.StateIdle)
	}
	_ = wedged // the wedged goroutine is orphaned by design (see comment above)
}

// finishCancelledTurn synthesises cancelled tool_results for every pending
// tool_use, transitions through Cancelled → Idle, and emits `interrupted`.
// The wait-for-tools step (checklist item 3 in the spec) lives in runTurnSafe
// via the drain-budget select; this function only handles the synchronous
// bookkeeping that should never block.
func (l *Loop) finishCancelledTurn(parent context.Context) {
	cleanupCtx, cleanupCancel := context.WithTimeout(parent, 10*time.Second)
	defer cleanupCancel()

	pending := l.Session.DrainPendingToolUses()
	if len(pending) > 0 {
		blocks := make([]provider.ContentBlock, 0, len(pending))
		for _, p := range pending {
			blocks = append(blocks, provider.ContentBlock{
				Type:      provider.BlockToolResult,
				ToolUseID: p.ID,
				Output:    prompt.CancelledToolOutput,
				IsError:   true,
			})
		}
		l.Session.AppendMessage(cleanupCtx, provider.Message{
			Role:    provider.RoleUser,
			Content: blocks,
		})
	}
	// Persist the interrupted flag on the last assistant message so the
	// marker survives session rehydration (persist-interrupted-message-flag D8).
	if err := l.Session.MarkMessageInterrupted(cleanupCtx); err != nil {
		l.logger().Warn("agent: mark message interrupted failed", "err", err)
	}
	if l.Session.State() != session.StateCancelled {
		_ = l.Session.ForceTransition(session.StateCancelled)
	}
	// Include the store message ID in the interrupted event payload so SSE
	// consumers can correlate the event with a specific message (D9).
	payload := map[string]any{}
	if msgID := l.Session.LastAssistantMessageID(); msgID != "" {
		payload["message_id"] = msgID
	}
	if !l.Session.EmitNow("interrupted", payload) {
		l.logger().Warn("agent: interrupted event dropped (outbox full)")
	}
}

// drainOrphanedPending drains pending tool uses that were registered before a
// provider fatal error. Unlike finishCancelledTurn it does not emit "interrupted"
// or transition to Cancelled — it just pairs the orphaned tool_use blocks with
// cancelled tool_results so history stays valid for the next turn.
func (l *Loop) drainOrphanedPending() {
	pending := l.Session.DrainPendingToolUses()
	if len(pending) == 0 {
		return
	}
	blocks := make([]provider.ContentBlock, 0, len(pending))
	for _, p := range pending {
		blocks = append(blocks, provider.ContentBlock{
			Type:      provider.BlockToolResult,
			ToolUseID: p.ID,
			Output:    prompt.CancelledToolOutput,
			IsError:   true,
		})
	}
	l.Session.AppendMessage(context.Background(), provider.Message{
		Role:    provider.RoleUser,
		Content: blocks,
	})
}

// startInboxWatcher spawns a goroutine that consumes inbox messages while a
// turn is in flight. The watcher converts ClientInterrupt into a turn cancel,
// forwards ClientPermissionDecision into Session.PermissionAnswers for the
// blocked Check() call to receive, and answers ClientPing with `pong`.
//
// The returned channel closes when the watcher exits.
func (l *Loop) startInboxWatcher(turnCtx context.Context, cancelTurn context.CancelFunc) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-turnCtx.Done():
				return
			case m, ok := <-l.Session.Inbox:
				if !ok {
					return
				}
				switch m.Type {
				case session.ClientInterrupt:
					cancelTurn()
					return
				case session.ClientPing:
					_ = l.Session.Emit(turnCtx, "pong", nil)
				case session.ClientPermissionDecision:
					var pd session.PermissionDecisionPayload
					if err := json.Unmarshal(m.Payload, &pd); err != nil {
						continue
					}
					select {
					case l.Session.PermissionAnswers <- pd:
					case <-turnCtx.Done():
						return
					}
				case session.ClientContextUpdate:
					// Group 9 will wire context updates; ignore for MVP loop tests.
				case session.ClientUserMessage:
					// HTTP layer should have 409'd this; if it slipped through
					// during a turn, drop it.
					l.logger().Warn("agent: user_message during active turn — dropped")
				}
			}
		}
	}()
	return done
}

// runTurnLoop is the LLM ↔ tools ↔ LLM iteration. It exits either when the
// model signals end_turn (no tool_use), when ctx is cancelled, or when a
// provider error terminates the turn.
func (l *Loop) runTurnLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		if l.shouldCompact() {
			if err := l.runCompaction(ctx); err != nil {
				l.logger().Warn("agent: compaction failed; continuing", "err", err)
			}
			if ctx.Err() != nil {
				return
			}
		}

		// Composition order: base → environment → instructions → memory,
		// joined by "\n\n" only between non-empty pieces. The static base段
		// leads so it forms the Anthropic prompt-cache prefix; the dynamic
		// environment, instructions, and memory blocks follow
		// (optimize-prompt-cache-order). The prompt package owns the ordering
		// and delimiters — this is the single assembly path.
		req := provider.Request{
			Model: l.Config.Model,
			System: prompt.BuildSystemPrompt(prompt.SystemPromptInput{
				Base:         l.SystemPromptBase,
				Environment:  l.Session.EnvSnapshot,
				Instructions: instructions.Block(l.Session.InstructionSnapshot),
				Memory:       memory.Block(l.Session.MemorySnapshot),
			}),
			Messages:             l.Session.History(),
			Tools:                l.buildToolSchemas(),
			MaxTokens:            l.Config.MaxTokens,
			ThinkingEnabled:      l.Config.ThinkingEnabled,
			ThinkingBudgetTokens: l.Config.ThinkingBudgetTokens,
		}

		events, err := streamWithRetry(ctx, l.Provider, req, l.Config.Retry, func(attempt int, wait time.Duration, code string) {
			_ = l.Session.Emit(ctx, "provider_retry", map[string]any{
				"attempt":  attempt,
				"after_ms": wait.Milliseconds(),
				"code":     code,
			})
		})
		if err != nil {
			l.emitProviderError(ctx, err)
			return
		}

		toolCalls, terminate := l.consumeProviderStream(ctx, events)
		if terminate || ctx.Err() != nil {
			if ctx.Err() == nil {
				l.drainOrphanedPending()
			}
			return
		}

		if len(toolCalls) == 0 {
			// Turn naturally ended.
			return
		}

		l.runToolBatch(ctx, toolCalls)
		if ctx.Err() != nil {
			return
		}

		// After tools, we expect to be in Executing — transition back to
		// Thinking so the next loop iteration can call the LLM again.
		cur := l.Session.State()
		if cur == session.StateExecuting {
			_ = l.Session.Transition(session.StateExecuting, session.StateThinking)
		} else if cur == session.StateAwaitPerm {
			_ = l.Session.Transition(session.StateAwaitPerm, session.StateThinking)
		}
	}
}

// consumeProviderStream drains the provider's event channel. It emits
// assistant_text_delta and reasoning events as they arrive, accumulates the
// assistant turn's content blocks IN THEIR EMISSION ORDER (thinking, text, and
// tool_use interleaved exactly as the model produced them — required for
// interleaved-thinking round-trips), and appends the turn to history. Returns
// the list of tool calls and a `terminate` flag that's true when the stream
// ended the turn (no tools and end_turn) or hit an error already surfaced.
func (l *Loop) consumeProviderStream(ctx context.Context, events <-chan provider.ProviderEvent) ([]ToolCall, bool) {
	var (
		blocks     []provider.ContentBlock // assistant content in emission order
		toolCalls  []ToolCall
		textLen    int // total accumulated assistant text (drives the done event)
		stopReason string
		fatal      bool
	)
	// appendText coalesces consecutive text deltas into the trailing text block
	// so interleaving (thinking → text → tool_use) preserves block boundaries.
	appendText := func(delta string) {
		if n := len(blocks); n > 0 && blocks[n-1].Type == provider.BlockText {
			blocks[n-1].Text += delta
		} else {
			blocks = append(blocks, provider.ContentBlock{Type: provider.BlockText, Text: delta})
		}
		textLen += len(delta)
	}
	for ev := range events {
		switch ev.Type {
		case provider.EventTextDelta:
			appendText(ev.TextDelta)
			_ = l.Session.Emit(ctx, string(protocol.EventAssistantTextDelta), map[string]any{"delta": ev.TextDelta})
		case provider.EventReasoningStart:
			_ = l.Session.Emit(ctx, string(protocol.EventReasoningStart), map[string]any{
				"block_index": ev.BlockIndex,
				"type":        ev.ReasoningType,
			})
		case provider.EventReasoningDelta:
			_ = l.Session.Emit(ctx, string(protocol.EventReasoningDelta), map[string]any{
				"block_index": ev.BlockIndex,
				"delta":       ev.ReasoningDelta,
			})
		case provider.EventReasoningEnd:
			if ev.ReasoningBlock != nil {
				blocks = append(blocks, *ev.ReasoningBlock)
			}
			_ = l.Session.Emit(ctx, string(protocol.EventReasoningEnd), map[string]any{
				"block_index": ev.BlockIndex,
			})
		case provider.EventToolUse:
			if ev.ToolUse == nil {
				continue
			}
			tc := ToolCall{
				ID:    ev.ToolUse.ToolUseID,
				Name:  ev.ToolUse.ToolName,
				Input: ev.ToolUse.Input,
			}
			toolCalls = append(toolCalls, tc)
			blocks = append(blocks, provider.ContentBlock{
				Type:      provider.BlockToolUse,
				ToolUseID: tc.ID,
				ToolName:  tc.Name,
				Input:     tc.Input,
			})
			l.Session.MarkToolUsePending(tc.ID, tc.Name, tc.Input)
		case provider.EventStop:
			stopReason = ev.StopReason
		case provider.EventError:
			if ev.Error != nil {
				l.emitProviderError(ctx, ev.Error)
				fatal = true
			}
		case provider.EventUsage:
			// Group 9 will surface usage events to the client; for now drop.
		}
	}
	if ctx.Err() != nil {
		return toolCalls, true
	}
	if fatal {
		return toolCalls, true
	}

	// Append the assistant turn. Blocks are already in emission order; the
	// signature on any thinking block is persisted but never sent to the
	// client (SSE events above carry only reasoning text, not signature).
	// StopReason is recorded so the thinking strip rule can read the real
	// turn boundary instead of re-deriving it from block shape.
	if len(blocks) > 0 {
		l.Session.AppendMessage(context.Background(), provider.Message{
			Role:       provider.RoleAssistant,
			Content:    blocks,
			StopReason: stopReason,
		})
	}
	// Emit assistant_text_done when the turn produced text, OR when it ended
	// without further tool calls (a terminal turn) so the client always gets a
	// turn terminator + stop_reason — even for a thinking-only end_turn.
	if textLen > 0 || len(toolCalls) == 0 {
		_ = l.Session.Emit(ctx, string(protocol.EventAssistantTextDone), map[string]any{
			"message_id":  idgen.NewULID(),
			"stop_reason": stopReason,
		})
	}

	return toolCalls, false
}

// ImplicitTriggerHook is the runner-factory-supplied hook for synthesising
// tool results for ExternalAgent calls against unknown adapters. Returning
// `intercepted` for a call means the original call is skipped and the
// caller emits the synthetic result back to the model verbatim.
type ImplicitTriggerHook func(ctx context.Context, sess *session.Session, calls []ToolCall) (passThrough []ToolCall, intercepted []ToolCallResult)

// runToolBatch handles the permission gate, executes the orchestrator batch,
// emits tool_call_start / tool_call_done events, and appends the tool_result
// blocks to history.
func (l *Loop) runToolBatch(ctx context.Context, calls []ToolCall) {
	// 0. Implicit-trigger interceptor: short-circuits ExternalAgent calls
	// against unknown adapter names so the adapter-generation flow can take
	// over without ever reaching the orchestrator. The intercepted results
	// flow back through the standard tool_call_done path (see resultsByID
	// merge below) so the model sees uniform shape.
	origCalls := calls
	var intercepted []ToolCallResult
	if l.ImplicitTriggerInterceptor != nil {
		calls, intercepted = l.ImplicitTriggerInterceptor(ctx, l.Session, calls)
	}

	// 1. Permission check — sequential, transitions through AwaitPerm.
	cleared, denied := l.checkPermissions(ctx, calls)
	if ctx.Err() != nil {
		return
	}

	// 2. Transition into Executing for the run.
	cur := l.Session.State()
	switch cur {
	case session.StateAwaitPerm:
		_ = l.Session.Transition(session.StateAwaitPerm, session.StateExecuting)
	case session.StateThinking:
		_ = l.Session.Transition(session.StateThinking, session.StateExecuting)
	}

	// 3. Emit tool_call_start for every cleared call. Intercepted calls
	// (handled by the implicit-trigger hook) also need a start event so
	// observers see a complete start/done pair.
	for _, c := range cleared {
		_ = l.Session.Emit(ctx, "tool_call_start", map[string]any{
			"id":    c.ID,
			"tool":  c.Name,
			"input": json.RawMessage(c.Input),
		})
	}
	for _, ic := range intercepted {
		_ = l.Session.Emit(ctx, "tool_call_start", map[string]any{
			"id":   ic.ID,
			"tool": ic.Name,
		})
	}

	// 4. Run the orchestrator. The orchestrator handles batching by
	// CanRunInParallel and applies ContextModifiers after each batch.
	started := time.Now()
	results := l.Orchestrator.RunAll(ctx, l.ToolEnv, l.Session, cleared)

	// If the parent cancelled while the orchestrator was running, bail before
	// writing any post-cancel side effects (history/events). The parent's
	// finishCancelledTurn handles the cleanup; this guard prevents a wedge
	// that wakes up late from polluting a subsequent turn.
	if ctx.Err() != nil {
		return
	}

	// 5. Map results back by ID, splice in denied + intercepted entries in
	// original order, emit tool_call_done events, append to history.
	resultsByID := make(map[string]ToolCallResult, len(results))
	for _, r := range results {
		resultsByID[r.ID] = r
	}
	for _, d := range denied {
		resultsByID[d.ID] = d
	}
	for _, ic := range intercepted {
		resultsByID[ic.ID] = ic
	}

	contentBlocks := make([]provider.ContentBlock, 0, len(origCalls))
	for _, c := range origCalls {
		r, ok := resultsByID[c.ID]
		if !ok {
			// Lost between checkPermissions and run — synthesise an error.
			r = ToolCallResult{
				Result: &tools.Result{
					Output:  "tool execution lost",
					IsError: true,
				},
			}
		}
		output := ""
		isErr := false
		if r.Result != nil {
			output = r.Result.Output
			isErr = r.Result.IsError
		}
		took := time.Since(started)
		_ = l.Session.Emit(ctx, "tool_call_done", map[string]any{
			"id":      c.ID,
			"tool":    c.Name,
			"output":  output,
			"ok":      !isErr,
			"took_ms": took.Milliseconds(),
		})
		l.Session.ClearToolUsePending(c.ID)
		contentBlocks = append(contentBlocks, provider.ContentBlock{
			Type:      provider.BlockToolResult,
			ToolUseID: c.ID,
			Output:    output,
			IsError:   isErr,
		})
	}
	l.Session.AppendMessage(context.Background(), provider.Message{
		Role:    provider.RoleUser,
		Content: contentBlocks,
	})
}

// checkPermissions runs Permissions.Check for every tool in `calls`. Denied
// tools (or absent permission manager → default deny) get a synthesised
// `tool_result { is_error: true, output: "permission denied" }`. Cleared tools
// flow through to the orchestrator.
func (l *Loop) checkPermissions(ctx context.Context, calls []ToolCall) (cleared []ToolCall, denied []ToolCallResult) {
	if l.Permissions == nil {
		// No permission gate — everything cleared. Used by unit tests.
		return calls, nil
	}
	awaitEntered := false
	for _, c := range calls {
		// InternalGated tools handle their own permission flow.
		if l.Registry != nil {
			if t, ok := l.Registry.Get(c.Name); ok {
				if ig, ok := t.(interface{ IsInternalGated() bool }); ok && ig.IsInternalGated() {
					cleared = append(cleared, c)
					continue
				}
			}
		}
		resource := extractResource(c.Name, c.Input)
		// Emit permission_request event (the prompt callback may do the same;
		// having one here lets tests without a real prompt still observe it).
		// We emit a single request per call before Check blocks.
		// Transition to AwaitPerm on first prompted call.
		if !awaitEntered && l.Session.State() == session.StateThinking {
			if err := l.Session.Transition(session.StateThinking, session.StateAwaitPerm); err == nil {
				awaitEntered = true
			}
		}
		_ = l.Session.Emit(ctx, "permission_request", map[string]any{
			"request_id": c.ID,
			"tool":       c.Name,
			"resource":   resource,
		})

		decision, err := l.Permissions.Check(ctx, l.Session.ID, c.Name, resource)
		if err != nil || decision == permission.Deny || decision == permission.DenyPermanent {
			denied = append(denied, ToolCallResult{
				ID:   c.ID,
				Name: c.Name,
				Result: &tools.Result{
					Output:  fmt.Sprintf("permission denied for %s", c.Name),
					IsError: true,
				},
			})
			continue
		}
		cleared = append(cleared, c)
	}
	return cleared, denied
}

// extractResource pulls the natural "resource string" the permission spec
// uses to disambiguate prompts: file path for Read/Write/Edit, command for
// Bash, glob for Grep, and so on. Falls back to "" for tools that don't
// declare a path/command field.
func extractResource(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	for _, k := range []string{"path", "file_path", "command", "pattern", "glob"} {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// buildToolSchemas rebuilds the LLM-facing tool list from the registry filtered
// by the session's current AllowedTools set. Called before each provider request
// so that LoadSkill's AllowedToolsModifier takes effect on the next turn.
func (l *Loop) buildToolSchemas() []provider.ToolSchema {
	if l.Registry == nil {
		return l.Tools
	}
	allowed := l.Session.AllowedTools()
	tools := l.Registry.Filtered(allowed)
	out := make([]provider.ToolSchema, 0, len(tools))
	for _, t := range tools {
		out = append(out, provider.ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	return out
}

// shouldCompact returns true when the current history is above the configured
// ratio of MaxHistoryTokens. The estimate uses EstimateTokens — a coarse
// chars/4 heuristic that's good enough for triggering the safety net but not
// for exact accounting.
func (l *Loop) shouldCompact() bool {
	if l.Compactor == nil || l.Config.AutoCompactRatio <= 0 || l.Config.MaxHistoryTokens <= 0 {
		return false
	}
	used := EstimateTokens(l.Session.History())
	limit := float64(l.Config.MaxHistoryTokens) * l.Config.AutoCompactRatio
	return float64(used) >= limit
}

// runCompaction transitions Thinking → Compacting → Thinking and asks the
// Compactor to produce a fresh history. Errors are non-fatal; the loop falls
// back to running with the un-compacted history.
func (l *Loop) runCompaction(ctx context.Context) error {
	if err := l.Session.Transition(session.StateThinking, session.StateCompacting); err != nil {
		return err
	}
	defer func() {
		// Restore Thinking so the loop can call the LLM again.
		if l.Session.State() == session.StateCompacting {
			_ = l.Session.Transition(session.StateCompacting, session.StateThinking)
		}
	}()

	history := l.Session.History()
	newHistory, result, ok, err := l.Compactor.Compact(ctx, history)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	l.Session.ReplaceHistory(context.Background(), newHistory)
	_ = l.Session.Emit(ctx, "compaction", map[string]any{
		"before_tokens":   result.BeforeTokens,
		"after_tokens":    result.AfterTokens,
		"before_messages": result.BeforeMessages,
		"after_messages":  result.AfterMessages,
	})
	return nil
}

// handlePanic is the recovery path described by the session-management spec:
// log the panic, emit `error{code:internal_panic}` (no stack to the client),
// synthesise cancelled tool_results, transition to Idle.
func (l *Loop) handlePanic(parent context.Context, r any, stack []byte) {
	l.logger().Error("agent: panic in turn loop",
		"recover", fmt.Sprintf("%v", r),
		"session_id", l.Session.ID,
		"stack", string(stack))

	pending := l.Session.DrainPendingToolUses()
	if len(pending) > 0 {
		blocks := make([]provider.ContentBlock, 0, len(pending))
		for _, p := range pending {
			blocks = append(blocks, provider.ContentBlock{
				Type:      provider.BlockToolResult,
				ToolUseID: p.ID,
				Output:    prompt.CancelledToolOutput,
				IsError:   true,
			})
		}
		l.Session.AppendMessage(context.Background(), provider.Message{
			Role:    provider.RoleUser,
			Content: blocks,
		})
	}

	// Emit panic event (no stack in client payload).
	l.Session.EmitNow("error", map[string]any{
		"code":        "internal_panic",
		"message":     "tool execution failed",
		"details":     map[string]any{},
		"recoverable": true,
	})

	// State may be in any of several values; force back to Idle.
	if l.Session.State() != session.StateIdle {
		_ = l.Session.ForceTransition(session.StateIdle)
	}
	_ = parent // parent ctx is fine; we just don't need to thread it here
}

// emitProviderError surfaces a provider error as an `error` SSE event.
func (l *Loop) emitProviderError(ctx context.Context, err error) {
	pe, ok := provider.AsProviderError(err)
	if !ok {
		l.emitError(ctx, "provider_unrecoverable", err.Error(),
			map[string]any{"provider": l.Provider.Name()}, false)
		return
	}
	code, recoverable := providerErrorCodeFor(pe)
	details := map[string]any{"provider": pe.Provider}
	if pe.Message != "" && pe.Message != pe.Code {
		details["upstream_message"] = pe.Message
	}
	l.emitError(ctx, code, pe.Message, details, recoverable)
}

// emitError is a tiny helper for the multi-arg `error` event payload.
func (l *Loop) emitError(ctx context.Context, code, message string, details map[string]any, recoverable bool) {
	_ = l.Session.Emit(ctx, "error", map[string]any{
		"code":        code,
		"message":     message,
		"details":     details,
		"recoverable": recoverable,
	})
}

func (l *Loop) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// handlePublishFrontendTools processes a publish_frontend_tools message
// received while Idle. It lazily constructs the per-session Bridge, clones
// the registry (so frontend tools don't leak to other sessions), unregisters
// the prior frontend set, registers the new one, and emits
// frontend_tools_published.
func (l *Loop) handlePublishFrontendTools(ctx context.Context, msg session.ClientMessage) {
	var payload session.PublishFrontendToolsPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		l.logger().Warn("agent: bad publish_frontend_tools payload", "err", err)
		return
	}

	if l.frontendBridge == nil {
		l.frontendBridge = frontend.NewBridge(l.Session.Emit)
		l.Session.SetFrontend(l.frontendBridge)
	}

	if err := l.ensureClonedRegistry(); err != nil {
		l.logger().Warn("agent: failed to clone registry for frontend tools", "err", err)
		return
	}

	// Unregister the prior frontend set before registering new ones so that
	// a new entry with the same name as an old frontend tool does not collide.
	old := l.Session.SwapFrontendToolNames(nil)
	for _, name := range old {
		l.Registry.Unregister(name)
	}

	registered := make([]string, 0)
	rejected := make([]map[string]string, 0)
	for _, entry := range payload.Catalog {
		proxy := frontend.NewTool(frontend.ToolSpec{
			Name:           entry.Name,
			Description:    entry.Description,
			InputSchema:    entry.InputSchema,
			OutputSchema:   entry.OutputSchema,
			ParallelSafety: entry.ParallelSafety,
		}, l.frontendBridge)
		if err := l.Registry.Register(proxy); err != nil {
			rejected = append(rejected, map[string]string{
				"name":   entry.Name,
				"reason": err.Error(),
			})
			continue
		}
		registered = append(registered, entry.Name)
	}

	l.Session.SwapFrontendToolNames(registered)

	_ = l.Session.Emit(ctx, "frontend_tools_published", map[string]any{
		"registered": registered,
		"rejected":   rejected,
	})
}

// ensureClonedRegistry clones the registry into a per-session copy so
// frontend tool registrations don't leak across sessions (the runner factory
// only clones for adapter-generator sessions; regular sessions share the
// global registry). The Orchestrator is also replaced with a new instance
// pointing at the clone. No-op after the first call (tracked by registryCloned).
func (l *Loop) ensureClonedRegistry() error {
	if l.registryCloned {
		return nil
	}
	l.registryCloned = true
	cloned := l.Registry.Clone()
	l.Registry = cloned
	l.Orchestrator = &Orchestrator{
		Registry:        cloned,
		MaxParallel:     l.Orchestrator.MaxParallel,
		DefaultTimeout:  l.Orchestrator.DefaultTimeout,
		PerToolTimeouts: l.Orchestrator.PerToolTimeouts,
		MaxResultBytes:  l.Orchestrator.MaxResultBytes,
	}
	return nil
}

// deriveTitle produces a single-line, length-capped title from the first user
// message (add-project-sessions §5). Leading/trailing whitespace is trimmed;
// newlines collapse to spaces; the result is capped at 80 runes.
func deriveTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '\r'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:77]) + "..."
	}
	return s
}
