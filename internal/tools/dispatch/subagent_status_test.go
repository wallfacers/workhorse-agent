package dispatch_test

import (
	"context"
	"encoding/json"
	"strings"
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

// stubDispatchTool is a read-only tool the scripted child calls during a
// subagent_status test. Its Run body is irrelevant — the loop still emits a
// tool_call_start, which is what the status event is derived from.
type stubDispatchTool string

func (s stubDispatchTool) Name() string                  { return string(s) }
func (s stubDispatchTool) Description() string           { return string(s) + " stub" }
func (s stubDispatchTool) InputSchema() json.RawMessage  { return []byte(`{}`) }
func (s stubDispatchTool) IsReadOnly() bool              { return true }
func (s stubDispatchTool) CanRunInParallel() bool        { return true }
func (s stubDispatchTool) DefaultTimeout() time.Duration { return 0 }
func (s stubDispatchTool) Run(context.Context, *tools.Env, json.RawMessage) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}

// subagentStatusHarness wires a session manager whose child sessions run a
// scripted 3-call provider sequence (two tool calls then end_turn), so the
// parent observes two tool_call_start events to translate.
func subagentStatusHarness(t *testing.T) (*dispatch.Host, dispatch.Tool, *session.Manager) {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(stubDispatchTool("Read"))
	reg.Register(stubDispatchTool("Grep"))
	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	factory := func(sess *session.Session) session.Runner {
		mp := mockprovider.New("mock")
		if sess.ParentID != "" {
			calls := 0
			mp.SetFallback(func() []provider.ProviderEvent {
				calls++
				switch calls {
				case 1:
					return []provider.ProviderEvent{
						{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
							Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "Read",
							Input: json.RawMessage(`{"path":"/a/b.go"}`),
						}},
						{Type: provider.EventStop, StopReason: "tool_use"},
					}
				case 2:
					return []provider.ProviderEvent{
						{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
							Type: provider.BlockToolUse, ToolUseID: "tu2", ToolName: "Grep",
							Input: json.RawMessage(`{"pattern":"foo"}`),
						}},
						{Type: provider.EventStop, StopReason: "tool_use"},
					}
				default:
					return []provider.ProviderEvent{
						{Type: provider.EventTextDelta, TextDelta: "done"},
						{Type: provider.EventStop, StopReason: "end_turn"},
					}
				}
			})
		} else {
			mp.SetFallback(func() []provider.ProviderEvent {
				return []provider.ProviderEvent{{Type: provider.EventStop, StopReason: "end_turn"}}
			})
		}
		loop := agent.NewLoop(agent.LoopConfig{Model: "m", MaxTokens: 1024, CancelDrainTimeout: 500 * time.Millisecond})
		loop.Session = sess
		loop.Provider = mp
		loop.Orchestrator = orch
		loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}
		return loop
	}
	mgr := session.NewManager(session.ManagerOptions{RunnerFactory: factory, MaxConcurrent: 20})
	host := &dispatch.Host{Manager: mgr, Loader: coord.NewLoader(t.TempDir()), MaxDepth: 3}
	tool := dispatch.Tool{Host: host}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("register dispatch: %v", err)
	}
	return host, tool, mgr
}

func TestDispatch_SubagentStatusPerToolCall(t *testing.T) {
	_, tool, mgr := subagentStatusHarness(t)
	parent, err := mgr.CreateSession(context.Background(), session.Options{Workdir: t.TempDir(), Ephemeral: true})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	defer mgr.DeleteSession(context.Background(), parent.ID, 2*time.Second)

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir}
	input, _ := json.Marshal(dispatch.DispatchInput{Prompt: "find auth", Mode: "streaming"})

	done := make(chan *tools.Result, 1)
	go func() {
		res, _ := tool.Run(context.Background(), env, input)
		done <- res
	}()

	var events []session.Event
	collectEnd := time.Now().Add(3 * time.Second)
loop:
	for {
		select {
		case ev := <-parent.Outbox:
			events = append(events, ev)
		case r := <-done:
			if r == nil || r.IsError {
				t.Fatalf("dispatch returned error: %+v", r)
			}
			break loop
		case <-time.After(time.Until(collectEnd)):
			t.Fatalf("timeout waiting for dispatch; got %d events", len(events))
		}
	}
	// Drain anything still buffered.
	for {
		select {
		case ev := <-parent.Outbox:
			events = append(events, ev)
		default:
			goto assert
		}
	}

assert:
	// tool_call_start is a child event; it reaches the parent wrapped inside a
	// subagent_event. Count those wrappers.
	toolStartsViaEvent := 0
	for _, ev := range events {
		if ev.Type != "subagent_event" {
			continue
		}
		if inner, ok := ev.Payload["event"].(map[string]any); ok {
			if t, _ := inner["type"].(string); t == "tool_call_start" {
				toolStartsViaEvent++
			}
		}
	}
	if toolStartsViaEvent != 2 {
		t.Fatalf("want 2 forwarded tool_call_start, got %d", toolStartsViaEvent)
	}
	statuses := eventsOfType(events, "subagent_status")
	if len(statuses) < 3 {
		t.Fatalf("want >=3 subagent_status (2 tool + 1 clear), got %d", len(statuses))
	}

	// Each tool_call_start maps to a non-empty single-line activity, in order.
	nonClear := statuses[:len(statuses)-1]
	if len(nonClear) != 2 {
		t.Fatalf("expected exactly 2 non-clear statuses, got %d", len(nonClear))
	}
	for i, ev := range nonClear {
		activity, _ := ev.Payload["activity"].(string)
		if activity == "" {
			t.Fatalf("status %d has empty activity", i)
		}
		if strings.Contains(activity, "\n") {
			t.Fatalf("status %d activity must be single-line: %q", i, activity)
		}
	}
	if firstAct, _ := nonClear[0].Payload["activity"].(string); !strings.Contains(firstAct, "Read") {
		t.Fatalf("first activity should mention Read: %q", firstAct)
	}
	if secondAct, _ := nonClear[1].Payload["activity"].(string); !strings.Contains(secondAct, "Grep") {
		t.Fatalf("second activity should mention Grep: %q", secondAct)
	}

	// The final status is the clear (empty activity).
	clear := statuses[len(statuses)-1]
	if act, _ := clear.Payload["activity"].(string); act != "" {
		t.Fatalf("final status must be a clear (empty activity), got %q", act)
	}
	for _, key := range []string{"agent_id", "agent_type", "description"} {
		if _, ok := clear.Payload[key]; !ok {
			t.Fatalf("clear status missing %q: %+v", key, clear.Payload)
		}
	}

	// The full-fidelity subagent_event passthrough is unaffected.
	if len(eventsOfType(events, "subagent_event")) == 0 {
		t.Fatal("subagent_event passthrough disappeared")
	}
}

func eventsOfType(events []session.Event, typ string) []session.Event {
	var out []session.Event
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}
