package agent_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

func permStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func queueToolTurn(h *loopHarness, id, tool string) {
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type:      provider.BlockToolUse,
			ToolUseID: id,
			ToolName:  tool,
			Input:     json.RawMessage(`{"path":"/x"}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "done"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
}

func findEvent(es []session.Event, typ string) (map[string]any, bool) {
	for _, e := range es {
		if e.Type == typ {
			return e.Payload, true
		}
	}
	return nil, false
}

// Spec scenario: 规则放行不产生审批请求 — a rule-allowed call emits
// permission_resolved{source:rule} and no permission_request.
func TestLoop_PermissionResolved_RuleAllow(t *testing.T) {
	st := permStore(t)
	if err := st.SavePermission(context.Background(), &store.Permission{
		ID: "preset-x", Tool: "dataweave__query_*", Pattern: "",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	h := newLoopHarness(t)
	h.Loop.Permissions = permission.New(st,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			t.Errorf("prompt must not fire for rule-allowed %s", req.Tool)
			return permission.Deny, true
		}, nil, time.Second, "")
	if err := h.Registry.Register(&loopStubTool{
		name: "dataweave__query_tasks", parallel: true, readOnly: true,
		body: func(ctx context.Context, in json.RawMessage) (*tools.Result, error) {
			return &tools.Result{Output: "rows"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	h.Loop.Registry = h.Registry
	queueToolTurn(h, "tu1", "dataweave__query_tasks")

	h.start()
	defer h.stop()
	h.sendUser(t, "query tasks")

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "assistant_text_done") && hasType(es, "tool_call_done")
	})

	if hasType(events, "permission_request") {
		t.Fatalf("rule-allowed call must not emit permission_request: %v", eventTypes(events))
	}
	p, ok := findEvent(events, "permission_resolved")
	if !ok {
		t.Fatalf("missing permission_resolved: %v", eventTypes(events))
	}
	if p["request_id"] != "tu1" || p["decision"] != "allow_permanent" || p["source"] != "rule" {
		t.Fatalf("permission_resolved payload off: %+v", p)
	}
	if !hasType(events, "tool_call_start") {
		t.Fatalf("allowed tool should run: %v", eventTypes(events))
	}
}

// Spec scenario: 超时决议可观察 — an unanswered prompt resolves as
// deny/timeout and the tool is denied without tool_call_start.
func TestLoop_PermissionResolved_Timeout(t *testing.T) {
	st := permStore(t)
	h := newLoopHarness(t)
	h.Loop.Permissions = permission.New(st,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			<-ctx.Done()
			return permission.Deny, false
		}, nil, 30*time.Millisecond, "")
	if err := h.Registry.Register(&loopStubTool{
		name: "dataweave__node_exec", parallel: true,
		body: func(ctx context.Context, in json.RawMessage) (*tools.Result, error) {
			t.Error("denied tool must not run")
			return &tools.Result{Output: "nope"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	h.Loop.Registry = h.Registry
	queueToolTurn(h, "tu2", "dataweave__node_exec")

	h.start()
	defer h.stop()
	h.sendUser(t, "exec")

	events := h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "permission_resolved") && hasType(es, "tool_call_done")
	})

	p, _ := findEvent(events, "permission_resolved")
	if p["request_id"] != "tu2" || p["decision"] != "deny" || p["source"] != "timeout" {
		t.Fatalf("timeout resolution payload off: %+v", p)
	}
	if hasType(events, "tool_call_start") {
		t.Fatalf("denied call must not start: %v", eventTypes(events))
	}
	d, _ := findEvent(events, "tool_call_done")
	if d["ok"] != false {
		t.Fatalf("denied call must surface as failed tool_call_done: %+v", d)
	}
}
