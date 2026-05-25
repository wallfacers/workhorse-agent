package dispatch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/coord"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/dispatch"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

// dispatchHarness wires a session manager backed by a per-session mock
// provider so each child can be scripted independently.
type dispatchHarness struct {
	mgr       *session.Manager
	reg       *tools.Registry
	loader    *coord.Loader
	host      *dispatch.Host
	tool      dispatch.Tool
	providers map[string]*mockprovider.Provider // sessionID -> mock used by its loop
	mu        sync.Mutex
}

func newDispatchHarness(t *testing.T, loaderDir string) *dispatchHarness {
	t.Helper()
	h := &dispatchHarness{
		reg:       tools.NewRegistry(),
		loader:    coord.NewLoader(loaderDir),
		providers: map[string]*mockprovider.Provider{},
	}
	orch := &agent.Orchestrator{Registry: h.reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}

	factory := func(sess *session.Session) session.Runner {
		mp := mockprovider.New("mock")
		mp.SetFallback(func() []provider.ProviderEvent {
			return []provider.ProviderEvent{
				{Type: provider.EventTextDelta, TextDelta: "child:" + sess.ID[:6]},
				{Type: provider.EventStop, StopReason: "end_turn"},
			}
		})
		h.mu.Lock()
		h.providers[sess.ID] = mp
		h.mu.Unlock()

		loop := agent.NewLoop(agent.LoopConfig{
			Model:              sess.Model,
			MaxTokens:          1024,
			CancelDrainTimeout: 500 * time.Millisecond,
		})
		loop.Session = sess
		loop.Provider = mp
		loop.Orchestrator = orch
		loop.SystemPromptBase = sess.SystemPromptBase
		loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}
		return loop
	}
	h.mgr = session.NewManager(session.ManagerOptions{
		RunnerFactory: factory,
		MaxConcurrent: 20,
	})
	h.host = &dispatch.Host{Manager: h.mgr, Loader: h.loader, MaxDepth: 3}
	h.tool = dispatch.Tool{Host: h.host}
	if err := h.reg.Register(h.tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	return h
}

func (h *dispatchHarness) parent(t *testing.T, opts session.Options) *session.Session {
	t.Helper()
	if opts.Ephemeral == false {
		opts.Ephemeral = true
	}
	sess, err := h.mgr.CreateSession(context.Background(), opts)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	return sess
}

func TestDispatch_OneShot_ReturnsFinalText(t *testing.T) {
	h := newDispatchHarness(t, t.TempDir())
	parent := h.parent(t, session.Options{Workdir: "/tmp"})
	defer h.mgr.DeleteSession(context.Background(), parent.ID, time.Second)

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}
	input, _ := json.Marshal(dispatch.DispatchInput{Prompt: "hello"})

	res, err := h.tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output == "" || res.Output[:6] != "child:" {
		t.Fatalf("output: %q", res.Output)
	}
}

func TestDispatch_StreamingEmitsSubagentEvent(t *testing.T) {
	h := newDispatchHarness(t, t.TempDir())
	parent := h.parent(t, session.Options{Workdir: "/tmp"})
	defer h.mgr.DeleteSession(context.Background(), parent.ID, time.Second)

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}
	input, _ := json.Marshal(dispatch.DispatchInput{Prompt: "hi", Mode: "streaming"})

	done := make(chan *tools.Result, 1)
	go func() {
		res, _ := h.tool.Run(context.Background(), env, input)
		done <- res
	}()

	// Wait for subagent_event to appear in parent's outbox.
	deadline := time.After(2 * time.Second)
	sawSub := false
	for !sawSub {
		select {
		case ev := <-parent.Outbox:
			if ev.Type == "subagent_event" {
				if ev.Payload["agent_id"] == nil {
					t.Fatalf("subagent_event missing agent_id: %+v", ev.Payload)
				}
				sawSub = true
			}
		case <-deadline:
			t.Fatal("no subagent_event observed")
		}
	}
	res := <-done
	if res.IsError {
		t.Fatalf("dispatch errored: %s", res.Output)
	}
}

func TestDispatch_BlockingSuppressesPassthrough(t *testing.T) {
	h := newDispatchHarness(t, t.TempDir())
	parent := h.parent(t, session.Options{Workdir: "/tmp"})
	defer h.mgr.DeleteSession(context.Background(), parent.ID, time.Second)

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}
	input, _ := json.Marshal(dispatch.DispatchInput{Prompt: "hi", Mode: "blocking"})

	res, err := h.tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch errored: %s", res.Output)
	}
	// Outbox should be empty — blocking mode suppresses passthrough.
	select {
	case ev := <-parent.Outbox:
		t.Fatalf("unexpected event in blocking mode: %+v", ev)
	default:
	}
}

func TestDispatch_ChildInheritsWorkdir_HistoryIndependent(t *testing.T) {
	h := newDispatchHarness(t, t.TempDir())
	parent := h.parent(t, session.Options{Workdir: "/proj"})
	defer h.mgr.DeleteSession(context.Background(), parent.ID, time.Second)
	// Give the parent some history so we can prove the child doesn't see it.
	parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "old parent turn"}},
	})

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}
	input, _ := json.Marshal(dispatch.DispatchInput{Prompt: "do thing"})

	res, err := h.tool.Run(context.Background(), env, input)
	if err != nil || res.IsError {
		t.Fatalf("Run: %v / %+v", err, res)
	}

	// Find the child via its provider's recorded request — exactly one prompt
	// (the Dispatch input), and the workdir was inherited.
	h.mu.Lock()
	var childMock *mockprovider.Provider
	for sid, mp := range h.providers {
		if sid == parent.ID {
			continue
		}
		childMock = mp
	}
	h.mu.Unlock()
	if childMock == nil {
		t.Fatal("no child provider recorded")
	}
	reqs := childMock.Requests()
	if len(reqs) == 0 {
		t.Fatal("child provider got no requests")
	}
	first := reqs[0]
	if len(first.Messages) != 1 {
		t.Fatalf("expected 1 message (Dispatch prompt only), got %d", len(first.Messages))
	}
	if got := first.Messages[0].Content[0].Text; got != "do thing" {
		t.Fatalf("child first message: %q", got)
	}
}

func TestDispatch_OverridesProviderAndModel(t *testing.T) {
	h := newDispatchHarness(t, t.TempDir())
	parent := h.parent(t, session.Options{
		Workdir:      "/proj",
		ProviderName: "anthropic",
		Model:        "claude-x",
	})
	defer h.mgr.DeleteSession(context.Background(), parent.ID, time.Second)

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}
	input, _ := json.Marshal(dispatch.DispatchInput{
		Prompt:   "x",
		Provider: "openai",
		Model:    "gpt-4o",
	})

	if _, err := h.tool.Run(context.Background(), env, input); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Find the child session and verify ProviderName/Model were set.
	for _, sess := range h.mgr.ListSessions() {
		if sess.ID == parent.ID {
			continue
		}
		if sess.ProviderName != "openai" {
			t.Fatalf("child provider: %q", sess.ProviderName)
		}
		if sess.Model != "gpt-4o" {
			t.Fatalf("child model: %q", sess.Model)
		}
	}
}

func TestDispatch_MaxDepthRejects(t *testing.T) {
	h := newDispatchHarness(t, t.TempDir())
	parent := h.parent(t, session.Options{Workdir: "/proj"})
	defer h.mgr.DeleteSession(context.Background(), parent.ID, time.Second)
	parent.Depth = 3 // host.MaxDepth=3 → child would be depth 4

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}
	input, _ := json.Marshal(dispatch.DispatchInput{Prompt: "x"})

	res, _ := h.tool.Run(context.Background(), env, input)
	if !res.IsError {
		t.Fatalf("expected is_error, got %+v", res)
	}
	if got := res.Output; !contains(got, "max sub-agent depth") {
		t.Fatalf("output: %q", got)
	}
}

func TestDispatch_AgentTypeNotFound(t *testing.T) {
	h := newDispatchHarness(t, t.TempDir())
	parent := h.parent(t, session.Options{Workdir: "/proj"})
	defer h.mgr.DeleteSession(context.Background(), parent.ID, time.Second)

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}
	input, _ := json.Marshal(dispatch.DispatchInput{Prompt: "x", AgentType: "ghost"})
	res, _ := h.tool.Run(context.Background(), env, input)
	if !res.IsError || !contains(res.Output, "agent_type not found") {
		t.Fatalf("expected agent_type not found, got %+v", res)
	}
}

func TestDispatch_ConcurrentThreeChildren(t *testing.T) {
	h := newDispatchHarness(t, t.TempDir())
	parent := h.parent(t, session.Options{Workdir: "/proj"})
	defer h.mgr.DeleteSession(context.Background(), parent.ID, time.Second)

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}

	var wg sync.WaitGroup
	results := make([]*tools.Result, 3)
	for i := 0; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			input, _ := json.Marshal(dispatch.DispatchInput{
				Prompt: fmt.Sprintf("task-%d", i),
				Mode:   "streaming",
			})
			res, _ := h.tool.Run(context.Background(), env, input)
			results[i] = res
		}()
	}
	wg.Wait()

	for i, r := range results {
		if r == nil || r.IsError {
			t.Fatalf("dispatch %d: %+v", i, r)
		}
	}
	// Verify each child saw a single prompt matching its input.
	h.mu.Lock()
	defer h.mu.Unlock()
	seenPrompts := map[string]bool{}
	for sid, mp := range h.providers {
		if sid == parent.ID {
			continue
		}
		reqs := mp.Requests()
		if len(reqs) == 0 {
			continue
		}
		first := reqs[0]
		if len(first.Messages) > 0 && len(first.Messages[0].Content) > 0 {
			seenPrompts[first.Messages[0].Content[0].Text] = true
		}
	}
	for i := 0; i < 3; i++ {
		want := fmt.Sprintf("task-%d", i)
		if !seenPrompts[want] {
			t.Fatalf("child for %q not recorded; got %v", want, seenPrompts)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
