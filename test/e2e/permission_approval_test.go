package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/api"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

type stubMCPTool struct{ name string }

func (s stubMCPTool) Name() string                 { return s.name }
func (s stubMCPTool) Description() string          { return "stub " + s.name }
func (s stubMCPTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s stubMCPTool) IsReadOnly() bool             { return false }
func (s stubMCPTool) CanRunInParallel() bool       { return false }
func (s stubMCPTool) DefaultTimeout() time.Duration {
	return 0
}
func (s stubMCPTool) Run(context.Context, *tools.Env, json.RawMessage) (*tools.Result, error) {
	return &tools.Result{Output: "executed"}, nil
}

// newApprovalStack mirrors newStack but wires the permission manager with the
// serve-style session-driven prompt (emit permission_request on the session's
// stream, block on PermissionAnswers) — the headless external-approval path.
func newApprovalStack(t *testing.T, promptTimeout time.Duration) *stack {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock := mockprovider.New("anthropic")
	reg := tools.NewRegistry()
	for _, n := range []string{"dataweave__query_tasks", "dataweave__node_exec"} {
		if err := reg.Register(stubMCPTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}

	var mgr *session.Manager
	prompt := func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
		sess, err := mgr.GetSession(req.SessionID)
		if err != nil {
			return permission.Deny, false
		}
		payload := map[string]any{
			"request_id": req.RequestID,
			"tool":       req.Tool,
			"resource":   req.Resource,
			"dangerous":  req.Dangerous,
			"reason":     req.Reason,
		}
		if deadline, ok := ctx.Deadline(); ok {
			payload["expires_at"] = deadline.UTC().Format(time.RFC3339)
		}
		if err := sess.Emit(ctx, "permission_request", payload); err != nil {
			return permission.Deny, false
		}
		for {
			select {
			case ans, ok := <-sess.PermissionAnswers:
				if !ok {
					return permission.Deny, false
				}
				if ans.RequestID != req.RequestID {
					continue
				}
				return permission.Decision(ans.Decision), true
			case <-ctx.Done():
				return permission.Deny, false
			}
		}
	}
	permMgr := permission.New(st, prompt, nil, promptTimeout, "")

	mgr = session.NewManager(session.ManagerOptions{
		Store: st,
		RunnerFactory: func(sess *session.Session) session.Runner {
			loop := agent.NewLoop(agent.LoopConfig{
				Model:              "stub",
				MaxTokens:          1024,
				CancelDrainTimeout: 500 * time.Millisecond,
			})
			loop.Session = sess
			loop.Provider = mock
			loop.Orchestrator = orch
			loop.Permissions = permMgr
			loop.Logger = logger
			loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}
			return loop
		},
	})

	cfg := api.Config{
		Host: "127.0.0.1", Port: 0, MaxRequestBodyBytes: 1 << 20,
		SSEKeepalive: 60 * time.Second, GracefulShutdownTimeout: 2 * time.Second,
		Version: "e2e",
	}
	srv := api.NewServer(cfg, mgr, st, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &stack{t: t, srv: srv, mgr: mgr, store: st, mock: mock, ts: ts, url: ts.URL, logger: logger}
}

func collectFrames(t *testing.T, rd *bufio.Reader, stopEvent string, max int) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for i := 0; i < max; i++ {
		f, err := readFrame(t, rd)
		if err != nil {
			t.Fatalf("readFrame: %v (got %d frames)", err, len(frames))
		}
		frames = append(frames, f)
		if f.Event == stopEvent {
			return frames
		}
	}
	t.Fatalf("never saw %s in %d frames: %+v", stopEvent, max, frames)
	return nil
}

func frameByEvent(frames []sseFrame, event string) (sseFrame, bool) {
	for _, f := range frames {
		if f.Event == event {
			return f, true
		}
	}
	return sseFrame{}, false
}

// permission-control spec scenario: 外部审批闭环 — the pure-HTTP path an
// external approver (e.g. dataweave-api) drives: dangerous tool call →
// permission_request on SSE (with expires_at) → POST permission_decision →
// permission_resolved{source:prompt} → tool executes.
func TestE2E_ExternalApprovalFlow(t *testing.T) {
	s := newApprovalStack(t, 10*time.Second)

	// Preset glob rule: the read-only family is friction-free.
	if err := s.store.SavePermission(context.Background(), &store.Permission{
		ID: "preset-ro", Tool: "dataweave__query_*", Pattern: "",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Turn: one rule-allowed call and one approval-needing call.
	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type: provider.BlockToolUse, ToolUseID: "tu_ro",
			ToolName: "dataweave__query_tasks", Input: json.RawMessage(`{}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type: provider.BlockToolUse, ToolUseID: "tu_exec",
			ToolName: "dataweave__node_exec", Input: json.RawMessage(`{"command":"dw run"}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "all done"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	id := s.createSessionViaAPI(false)
	sse, rd := s.openSSE(id, "")
	defer sse.Body.Close()

	resp := s.postStream(id, `{"type":"user_message","content":"run it"}`)
	resp.Body.Close()

	// Phase 1: the rule-allowed call must surface resolved{rule} and NO request.
	frames := collectFrames(t, rd, "permission_request", 64)
	for _, f := range frames[:len(frames)-1] {
		if f.Event == "permission_request" {
			t.Fatal("rule-allowed call emitted a permission_request")
		}
	}
	if f, ok := frameByEvent(frames, "permission_resolved"); !ok {
		t.Fatalf("missing permission_resolved for rule-allowed call: %+v", frames)
	} else {
		var p map[string]any
		_ = json.Unmarshal([]byte(f.Data), &p)
		if p["request_id"] != "tu_ro" || p["source"] != "rule" || p["decision"] != "allow_permanent" {
			t.Fatalf("rule resolution payload off: %v", p)
		}
	}

	// Phase 2: the approval request for node_exec, with expires_at + tool_use id.
	reqFrame := frames[len(frames)-1]
	var reqPayload map[string]any
	_ = json.Unmarshal([]byte(reqFrame.Data), &reqPayload)
	if reqPayload["request_id"] != "tu_exec" || reqPayload["tool"] != "dataweave__node_exec" {
		t.Fatalf("permission_request payload off: %v", reqPayload)
	}
	if _, ok := reqPayload["expires_at"].(string); !ok {
		t.Fatalf("permission_request missing expires_at: %v", reqPayload)
	}

	// Phase 3: the external approver answers over plain HTTP.
	decision := fmt.Sprintf(
		`{"type":"permission_decision","request_id":%q,"decision":"allow_once"}`,
		reqPayload["request_id"])
	dResp := s.postStream(id, decision)
	raw, _ := io.ReadAll(dResp.Body)
	dResp.Body.Close()
	if dResp.StatusCode != http.StatusAccepted && dResp.StatusCode != http.StatusOK {
		t.Fatalf("decision POST status %d: %s", dResp.StatusCode, raw)
	}

	// Phase 4: resolution, execution, completion.
	frames = collectFrames(t, rd, "assistant_text_done", 64)
	f, ok := frameByEvent(frames, "permission_resolved")
	if !ok {
		t.Fatalf("missing permission_resolved after decision: %+v", frames)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(f.Data), &p)
	if p["request_id"] != "tu_exec" || p["decision"] != "allow_once" || p["source"] != "prompt" {
		t.Fatalf("prompt resolution payload off: %v", p)
	}
	if start, ok := frameByEvent(frames, "tool_call_start"); !ok {
		t.Fatalf("approved tool never started: %+v", frames)
	} else {
		var sp map[string]any
		_ = json.Unmarshal([]byte(start.Data), &sp)
		if sp["id"] != "tu_exec" {
			t.Fatalf("tool_call_start for wrong call: %v", sp)
		}
	}
}

// permission-control spec scenario: 超时决议可观察 (wire level).
func TestE2E_ApprovalTimeoutResolvesDeny(t *testing.T) {
	s := newApprovalStack(t, 300*time.Millisecond)

	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type: provider.BlockToolUse, ToolUseID: "tu_to",
			ToolName: "dataweave__node_exec", Input: json.RawMessage(`{}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "denied then"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	id := s.createSessionViaAPI(false)
	sse, rd := s.openSSE(id, "")
	defer sse.Body.Close()
	resp := s.postStream(id, `{"type":"user_message","content":"go"}`)
	resp.Body.Close()

	frames := collectFrames(t, rd, "assistant_text_done", 64)
	if _, ok := frameByEvent(frames, "permission_request"); !ok {
		t.Fatalf("missing permission_request: %+v", frames)
	}
	f, ok := frameByEvent(frames, "permission_resolved")
	if !ok {
		t.Fatalf("missing permission_resolved: %+v", frames)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(f.Data), &p)
	if p["decision"] != "deny" || p["source"] != "timeout" {
		t.Fatalf("timeout resolution payload off: %v", p)
	}
	if _, ok := frameByEvent(frames, "tool_call_start"); ok {
		t.Fatal("denied tool must not start")
	}
}
