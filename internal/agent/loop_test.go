package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

// ---- harness ----

type loopHarness struct {
	Loop     *agent.Loop
	Session  *session.Session
	Mock     *mockprovider.Provider
	Registry *tools.Registry
	loopCtx  context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

func newLoopHarness(t *testing.T, opts ...func(*loopHarness)) *loopHarness {
	t.Helper()
	sess := session.New(session.Options{
		Workdir:   t.TempDir(),
		Ephemeral: true,
	})
	reg := tools.NewRegistry()
	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	mp := mockprovider.New("anthropic")

	loop := agent.NewLoop(agent.LoopConfig{
		Model:              "stub",
		MaxTokens:          1024,
		CancelDrainTimeout: 500 * time.Millisecond,
	})
	loop.Session = sess
	loop.Provider = mp
	loop.Orchestrator = orch
	loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}

	h := &loopHarness{Loop: loop, Session: sess, Mock: mp, Registry: reg}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *loopHarness) start() {
	h.loopCtx, h.cancel = context.WithCancel(context.Background())
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		h.Loop.Run(h.loopCtx)
	}()
}

func (h *loopHarness) stop() {
	if h.cancel != nil {
		h.cancel()
	}
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
	}
}

func (h *loopHarness) sendUser(t *testing.T, content string) {
	t.Helper()
	payload, _ := json.Marshal(session.UserMessagePayload{Content: content})
	select {
	case h.Session.Inbox <- session.ClientMessage{Type: session.ClientUserMessage, Payload: payload}:
	case <-time.After(time.Second):
		t.Fatal("inbox push timed out")
	}
}

func (h *loopHarness) sendInterrupt(t *testing.T) {
	t.Helper()
	select {
	case h.Session.Inbox <- session.ClientMessage{Type: session.ClientInterrupt}:
	case <-time.After(time.Second):
		t.Fatal("inbox push timed out")
	}
}

// collectUntildrains the outbox until the predicate matches or the timeout
// expires. Returns the events accumulated so far on timeout.
func (h *loopHarness) collectUntil(t *testing.T, timeout time.Duration, match func([]session.Event) bool) []session.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var events []session.Event
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timeout waiting for events; got types=%v", eventTypes(events))
			return events
		}
		select {
		case e := <-h.Session.Outbox:
			events = append(events, e)
			if match(events) {
				return events
			}
		case <-time.After(remaining):
			t.Fatalf("timeout waiting for events; got types=%v", eventTypes(events))
			return events
		}
	}
}

func eventTypes(es []session.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Type
	}
	return out
}

func hasType(es []session.Event, t string) bool {
	for _, e := range es {
		if e.Type == t {
			return true
		}
	}
	return false
}

func countType(es []session.Event, t string) int {
	n := 0
	for _, e := range es {
		if e.Type == t {
			n++
		}
	}
	return n
}

// ---- stubs ----

type loopStubTool struct {
	name     string
	parallel bool
	readOnly bool
	body     func(ctx context.Context, input json.RawMessage) (*tools.Result, error)
	timeout  time.Duration
}

func (s *loopStubTool) Name() string                  { return s.name }
func (s *loopStubTool) Description() string           { return s.name + " stub" }
func (s *loopStubTool) InputSchema() json.RawMessage  { return []byte(`{}`) }
func (s *loopStubTool) IsReadOnly() bool              { return s.readOnly }
func (s *loopStubTool) CanRunInParallel() bool        { return s.parallel }
func (s *loopStubTool) DefaultTimeout() time.Duration { return s.timeout }
func (s *loopStubTool) Run(ctx context.Context, _ *tools.Env, in json.RawMessage) (*tools.Result, error) {
	return s.body(ctx, in)
}

// ---- 8.7: full user → tool → text cycle ----

func TestLoop_FullCycle_UserToolText(t *testing.T) {
	h := newLoopHarness(t)
	if err := h.Registry.Register(&loopStubTool{
		name:     "Read",
		parallel: true,
		readOnly: true,
		body: func(ctx context.Context, in json.RawMessage) (*tools.Result, error) {
			return &tools.Result{Output: "file contents"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Turn 1 response: brief intro text + tool_use
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "reading file"},
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type:      provider.BlockToolUse,
			ToolUseID: "tu1",
			ToolName:  "Read",
			Input:     json.RawMessage(`{"path":"/x"}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	// Turn 2 response: text summary then end_turn
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "file says: file contents"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "read /x")

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return countType(es, "assistant_text_done") >= 2
	})

	if !hasType(events, "tool_call_start") || !hasType(events, "tool_call_done") {
		t.Fatalf("missing tool_call events: %v", eventTypes(events))
	}
	if !hasType(events, "assistant_text_delta") {
		t.Fatalf("missing assistant_text_delta: %v", eventTypes(events))
	}
	// Two provider calls should have been issued.
	if len(h.Mock.Requests()) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(h.Mock.Requests()))
	}
	// Final session state must be Idle so a follow-up message can land.
	waitForState(t, h.Session, session.StateIdle, time.Second)

	// And history should include user, assistant(text+tool_use), user(tool_result), assistant(summary).
	hist := h.Session.History()
	if len(hist) != 4 {
		t.Fatalf("history length: want 4, got %d (%+v)", len(hist), hist)
	}
	if hist[0].Role != provider.RoleUser || hist[1].Role != provider.RoleAssistant ||
		hist[2].Role != provider.RoleUser || hist[3].Role != provider.RoleAssistant {
		t.Fatalf("history role order off: %+v", roles(hist))
	}
	if !containsToolResult(hist[2].Content, "tu1", "file contents") {
		t.Fatalf("third history entry must be tool_result for tu1: %+v", hist[2])
	}
}

func TestLoop_TextOnlyResponse_NoTools(t *testing.T) {
	h := newLoopHarness(t)
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "hi there"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
	h.start()
	defer h.stop()
	h.sendUser(t, "say hi")

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return countType(es, "assistant_text_done") == 1
	})
	if hasType(events, "tool_call_start") {
		t.Fatalf("text-only response must not emit tool_call_start: %v", eventTypes(events))
	}
	waitForState(t, h.Session, session.StateIdle, time.Second)
}

// ---- helpers used in scenario tests ----

func waitForState(t *testing.T, s *session.Session, target session.State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.State() == target {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session state %q never reached (current=%q)", target, s.State())
}

func roles(h []provider.Message) []string {
	out := make([]string, len(h))
	for i, m := range h {
		out[i] = string(m.Role)
	}
	return out
}

func containsToolResult(blocks []provider.ContentBlock, id, output string) bool {
	for _, b := range blocks {
		if b.Type == provider.BlockToolResult && b.ToolUseID == id && b.Output == output {
			return true
		}
	}
	return false
}

func containsToolResultWithPrefix(blocks []provider.ContentBlock, id, prefix string) bool {
	for _, b := range blocks {
		if b.Type == provider.BlockToolResult && b.ToolUseID == id && strings.HasPrefix(b.Output, prefix) {
			return true
		}
	}
	return false
}

// ---- 8.8: compaction trigger ----

func TestLoop_Compaction_TriggersAndPreservesErrors(t *testing.T) {
	h := newLoopHarness(t)

	// Fast provider produces a fixed summary; configure the compactor with a
	// tiny token limit so any history > a few messages crosses the threshold.
	fastMP := mockprovider.New("anthropic-fast")
	fastMP.SetFallback(func() []provider.ProviderEvent {
		return []provider.ProviderEvent{
			{Type: provider.EventTextDelta, TextDelta: "[summary of prior turns]"},
			{Type: provider.EventStop, StopReason: "end_turn"},
		}
	})
	h.Loop.Compactor = &agent.Compactor{
		Provider:   fastMP,
		Model:      "fast",
		RecentKeep: 2,
	}
	h.Loop.Config.MaxHistoryTokens = 100
	h.Loop.Config.AutoCompactRatio = 0.5 // 50% of 100 = 50 tokens triggers

	// Seed history with several long error tool_results so compaction has
	// "old + error" to preserve. ~100 chars each → ~25 tokens each.
	longErr := strings.Repeat("E", 100)
	longTxt := strings.Repeat("T", 100)
	h.Session.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: longTxt}}})
	h.Session.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.BlockToolUse, ToolUseID: "tu-old", ToolName: "Read", Input: json.RawMessage(`{}`)}}})
	h.Session.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockToolResult, ToolUseID: "tu-old", Output: longErr, IsError: true}}})
	h.Session.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: longTxt}}})

	// Main provider returns end_turn after the user message.
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "ok"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "continue")

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "compaction") && countType(es, "assistant_text_done") >= 1
	})

	var compEvt session.Event
	for _, e := range events {
		if e.Type == "compaction" {
			compEvt = e
			break
		}
	}
	if compEvt.Type != "compaction" {
		t.Fatal("missing compaction event")
	}
	before, _ := compEvt.Payload["before_tokens"].(int)
	after, _ := compEvt.Payload["after_tokens"].(int)
	if before <= after {
		t.Fatalf("compaction did not shrink tokens: before=%d after=%d", before, after)
	}

	// History should now contain the summary system message + preserved error
	// tool_result + recent messages + new user_message + assistant response.
	hist := h.Session.History()
	if hist[0].Role != provider.RoleSystem {
		t.Fatalf("after compaction expected leading system message: %+v", hist[0])
	}
	// The preserved error tool_result must still be reachable.
	if !errorToolResultPresent(hist, "tu-old", longErr) {
		t.Fatalf("error tool_result lost during compaction: %v", roles(hist))
	}
}

func errorToolResultPresent(h []provider.Message, id, output string) bool {
	for _, m := range h {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.ToolUseID == id &&
				b.IsError && b.Output == output {
				return true
			}
		}
	}
	return false
}

// ---- 8.9: cancel mid-tool produces interrupted + cancelled tool_result ----

func TestLoop_Cancel_SynthesisesToolResultAndAllowsReuse(t *testing.T) {
	h := newLoopHarness(t)

	released := make(chan struct{})
	if err := h.Registry.Register(&loopStubTool{
		name: "SlowBash", parallel: false, readOnly: false,
		body: func(ctx context.Context, _ json.RawMessage) (*tools.Result, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-released:
				return &tools.Result{Output: "done"}, nil
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type:      provider.BlockToolUse,
			ToolUseID: "slow1",
			ToolName:  "SlowBash",
			Input:     json.RawMessage(`{}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	// After cancel, second user_message gets a quick text response.
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "ok, picking up"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	defer close(released)
	h.sendUser(t, "run slow thing")

	// Wait until the loop is in Executing (i.e. tool is running).
	waitForState(t, h.Session, session.StateExecuting, time.Second)

	// Drain events emitted so far so collectUntil after interrupt doesn't
	// dredge up stale tool_call_start.
	drainEvents(h)

	// Interrupt.
	h.sendInterrupt(t)

	// We expect an `interrupted` event and session back to Idle.
	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "interrupted")
	})
	if !hasType(events, "interrupted") {
		t.Fatalf("missing interrupted: %v", eventTypes(events))
	}

	waitForState(t, h.Session, session.StateIdle, time.Second)

	// Pending tool_use must have a synthesised cancelled tool_result in history.
	hist := h.Session.History()
	last := hist[len(hist)-1]
	if !containsToolResultWithPrefix(last.Content, "slow1", "[CANCELLED]") {
		t.Fatalf("expected cancelled tool_result for slow1 in last history entry, got %+v", last.Content)
	}

	// Session must accept a new user_message and complete cleanly.
	h.sendUser(t, "continue")
	events2 := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return countType(es, "assistant_text_done") >= 1
	})
	if !hasType(events2, "assistant_text_delta") {
		t.Fatalf("follow-up turn didn't run: %v", eventTypes(events2))
	}
	waitForState(t, h.Session, session.StateIdle, time.Second)
}

func drainEvents(h *loopHarness) {
	for {
		select {
		case <-h.Session.Outbox:
		default:
			return
		}
	}
}

// ---- 8.10: tool panic surfaces internal_panic, session survives ----

func TestLoop_ToolPanic_RecoversAndContinues(t *testing.T) {
	h := newLoopHarness(t)

	// Note: the orchestrator already wraps tool.Run with its own recover() —
	// so a tool panic surfaces as an is_error tool_result, NOT as a panic at
	// the loop's top level. The loop emits the tool_call_done and proceeds,
	// the model sees the error result on the next turn. Verify that path.
	if err := h.Registry.Register(&loopStubTool{
		name: "BoomTool", parallel: false, readOnly: false,
		body: func(ctx context.Context, _ json.RawMessage) (*tools.Result, error) {
			panic("kaboom")
		},
	}); err != nil {
		t.Fatal(err)
	}

	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type: provider.BlockToolUse, ToolUseID: "boom1", ToolName: "BoomTool",
			Input: json.RawMessage(`{}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "got it; will not retry"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "boom please")

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return countType(es, "assistant_text_done") >= 1
	})

	// tool_call_done must mark ok:false (orchestrator-level recover turns it
	// into an error result).
	found := false
	for _, e := range events {
		if e.Type == "tool_call_done" {
			if ok, _ := e.Payload["ok"].(bool); !ok {
				found = true
				out, _ := e.Payload["output"].(string)
				if !strings.Contains(out, "panic") {
					t.Errorf("expected output to mention panic, got %q", out)
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected tool_call_done with ok=false: %v", eventTypes(events))
	}

	// Session must be Idle and follow-up message must run normally.
	waitForState(t, h.Session, session.StateIdle, time.Second)
	h.sendUser(t, "what now")
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "all good"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
	events2 := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return countType(es, "assistant_text_done") >= 1
	})
	if !hasType(events2, "assistant_text_done") {
		t.Fatalf("follow-up turn did not complete: %v", eventTypes(events2))
	}
}

// ---- 8.10 (bis): provider panic surfaces internal_panic event ----

func TestLoop_ProviderPanic_RecoversAtTopLevel(t *testing.T) {
	h := newLoopHarness(t)

	// The mockprovider doesn't panic by default; substitute a panicking
	// provider for this test only.
	h.Loop.Provider = panickingProvider{name: "anthropic"}

	// Pre-load one pending tool_use so cancel synthesis has something to do.
	// Easiest: appendmessage assistant tool_use + MarkToolUsePending.
	h.Session.MarkToolUsePending("pre1", "Read", json.RawMessage(`{}`))

	h.start()
	defer h.stop()
	h.sendUser(t, "trigger panic")

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		for _, e := range es {
			if e.Type == "error" {
				if code, _ := e.Payload["code"].(string); code == "internal_panic" {
					return true
				}
			}
		}
		return false
	})

	var panicEvt session.Event
	for _, e := range events {
		if e.Type == "error" {
			if c, _ := e.Payload["code"].(string); c == "internal_panic" {
				panicEvt = e
				break
			}
		}
	}
	if panicEvt.Type == "" {
		t.Fatalf("missing internal_panic: %v", eventTypes(events))
	}
	// stack must NOT be exposed to clients
	if _, leaked := panicEvt.Payload["stack"]; leaked {
		t.Fatal("internal_panic must not include stack in payload")
	}

	// Session back to Idle, history has cancelled tool_result for pre1.
	waitForState(t, h.Session, session.StateIdle, time.Second)
	hist := h.Session.History()
	last := hist[len(hist)-1]
	if !containsToolResultWithPrefix(last.Content, "pre1", "[CANCELLED]") {
		t.Fatalf("expected cancelled tool_result for pre1, got %+v", last.Content)
	}
}

type panickingProvider struct{ name string }

func (p panickingProvider) Name() string { return p.name }
func (p panickingProvider) Stream(_ context.Context, _ provider.Request) (<-chan provider.ProviderEvent, error) {
	panic("provider exploded")
}

// ---- 8.11/8.12: cancel timeout when a tool ignores ctx ----

func TestLoop_CancelTimeout_ForcesIdleWhenToolIgnoresCtx(t *testing.T) {
	h := newLoopHarness(t)
	// Shorter cancel budget to keep the test snappy.
	h.Loop.Config.CancelDrainTimeout = 200 * time.Millisecond

	exit := make(chan struct{})
	defer close(exit)

	if err := h.Registry.Register(&loopStubTool{
		name: "WedgedMCP", parallel: false, readOnly: false,
		body: func(ctx context.Context, _ json.RawMessage) (*tools.Result, error) {
			// Ignore ctx — wait for the test to release.
			<-exit
			return &tools.Result{Output: "late"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type: provider.BlockToolUse, ToolUseID: "stuck1", ToolName: "WedgedMCP",
			Input: json.RawMessage(`{}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	// Session may accept a follow-up after cancel_timeout; queue a quick reply.
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "carrying on"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "do wedged thing")
	waitForState(t, h.Session, session.StateExecuting, time.Second)
	drainEvents(h)

	h.sendInterrupt(t)

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		for _, e := range es {
			if e.Type == "error" {
				if c, _ := e.Payload["code"].(string); c == "cancel_timeout" {
					return true
				}
			}
		}
		return false
	})

	var ct session.Event
	for _, e := range events {
		if e.Type == "error" {
			if c, _ := e.Payload["code"].(string); c == "cancel_timeout" {
				ct = e
				break
			}
		}
	}
	if ct.Type == "" {
		t.Fatalf("missing cancel_timeout: %v", eventTypes(events))
	}
	details, _ := ct.Payload["details"].(map[string]any)
	if phase, _ := details["phase"].(string); phase != "tool_drain" {
		t.Errorf("expected phase=tool_drain in cancel_timeout details, got %v", details)
	}

	// Session must accept a new user_message even though the wedged goroutine
	// is still running.
	waitForState(t, h.Session, session.StateIdle, time.Second)
	h.sendUser(t, "continue")
	events2 := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return countType(es, "assistant_text_done") >= 1
	})
	if !hasType(events2, "assistant_text_done") {
		t.Fatalf("follow-up didn't run after cancel_timeout: %v", eventTypes(events2))
	}
}

// ---- 8.6 verify: provider retry on retryable errors ----

func TestLoop_ProviderRetry_EmitsRetryEventThenSucceeds(t *testing.T) {
	h := newLoopHarness(t)
	h.Loop.Config.Retry = agent.RetryConfig{
		Attempts: 2,
		Backoff:  []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
	}

	// First Stream call: rate-limited (retryable).
	h.Mock.QueueError(provider.NewProviderError("anthropic", 429, provider.CodeRateLimited, "slow down", nil))
	// Second Stream call: succeeds with quick text.
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "ok"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "go")

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "assistant_text_done")
	})
	if !hasType(events, "provider_retry") {
		t.Fatalf("expected provider_retry event: %v", eventTypes(events))
	}
}

func TestLoop_ProviderAuthFailed_TerminatesWithoutRetry(t *testing.T) {
	h := newLoopHarness(t)
	h.Mock.QueueError(provider.NewProviderError("anthropic", 401, provider.CodeAuthFailed, "bad key", nil))

	h.start()
	defer h.stop()
	h.sendUser(t, "go")

	events := h.collectUntil(t, time.Second, func(es []session.Event) bool {
		for _, e := range es {
			if e.Type == "error" {
				if c, _ := e.Payload["code"].(string); c == "provider_auth_failed" {
					return true
				}
			}
		}
		return false
	})
	if hasType(events, "provider_retry") {
		t.Fatal("auth_failed must not trigger retry")
	}
	if !hasType(events, "error") {
		t.Fatalf("expected error event: %v", eventTypes(events))
	}
	waitForState(t, h.Session, session.StateIdle, time.Second)
}

// ---- 8.10b: panic in one session doesn't affect another ----

func TestLoop_PanicIsolationBetweenSessions(t *testing.T) {
	hA := newLoopHarness(t)
	hA.Loop.Provider = panickingProvider{name: "anthropic"}

	hB := newLoopHarness(t)
	hB.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "B is fine"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	hA.start()
	defer hA.stop()
	hB.start()
	defer hB.stop()

	// Trigger both turns concurrently.
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		hB.sendUser(t, "hello B")
		hB.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
			return countType(es, "assistant_text_done") >= 1
		})
	}()
	hA.sendUser(t, "trigger panic")
	hA.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		for _, e := range es {
			if e.Type == "error" {
				if c, _ := e.Payload["code"].(string); c == "internal_panic" {
					return true
				}
			}
		}
		return false
	})

	select {
	case <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session B never completed despite A's panic")
	}
	if hB.Session.State() != session.StateIdle {
		t.Fatalf("session B state: want Idle, got %q", hB.Session.State())
	}
}

// ---- guards ----

func TestLoop_RunCancellation_ExitsCleanly(t *testing.T) {
	h := newLoopHarness(t)
	h.start()
	// We didn't send anything; cancel should let Run exit promptly.
	doneAt := make(chan time.Time, 1)
	go func() { <-h.done; doneAt <- time.Now() }()
	start := time.Now()
	h.cancel()
	select {
	case end := <-doneAt:
		if end.Sub(start) > 500*time.Millisecond {
			t.Fatalf("Run did not exit promptly: %s", end.Sub(start))
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after cancel")
	}
	_ = atomic.LoadInt64(new(int64)) // silence unused import if refactored away
}
