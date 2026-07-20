package delegation_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/delegation"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	delegationtool "github.com/wallfacers/workhorse-agent/internal/tools/delegation"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

// integrationHarness wires a full real-loop stack: session.Manager with a
// RunnerFactory (real agent.Loop + mock provider), a tool registry holding the
// delegation tools, and a delegation.Manager whose SessMgr is back-filled after
// the session manager exists (mirroring cmd_serve).
type integrationHarness struct {
	dMgr       *delegation.Manager
	mgr        *session.Manager
	st         store.Store
	reg        *tools.Registry
	parentMock *mockprovider.Provider
}

func newIntegrationHarness(t *testing.T, childFallback func() []provider.ProviderEvent) *integrationHarness {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	reg := tools.NewRegistry()
	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dMgr := delegation.NewManager(s, nil, quiet)
	parentMock := mockprovider.New("mock")

	factory := func(sess *session.Session) session.Runner {
		mp := parentMock
		if _, isChild := sess.Metadata[delegation.ChildMetaKey]; isChild {
			mp = mockprovider.New("mock")
			mp.SetFallback(childFallback)
		}
		loop := agent.NewLoop(agent.LoopConfig{
			Model:              "m",
			MaxTokens:          2048,
			CancelDrainTimeout: 500 * time.Millisecond,
		})
		loop.Session = sess
		loop.Provider = mp
		loop.Orchestrator = orch
		loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir, Delegations: dMgr}
		if sess.ParentID == "" {
			loop.Config.Notifications = dMgr
		}
		return loop
	}
	mgr := session.NewManager(session.ManagerOptions{RunnerFactory: factory, Store: s, MaxConcurrent: 50})
	dMgr.SessMgr = mgr

	for _, tl := range delegationtool.Tools() {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register %s: %v", tl.Name(), err)
		}
	}
	return &integrationHarness{dMgr: dMgr, mgr: mgr, st: s, reg: reg, parentMock: parentMock}
}

func (h *integrationHarness) createParent(t *testing.T) *session.Session {
	t.Helper()
	sess, err := h.mgr.CreateSession(context.Background(), session.Options{Workdir: t.TempDir(), Ephemeral: true})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	t.Cleanup(func() { _ = h.mgr.DeleteSession(context.Background(), sess.ID, 2*time.Second) })
	return sess
}

func integrationSendUser(t *testing.T, sess *session.Session, content string) {
	t.Helper()
	payload, _ := json.Marshal(session.UserMessagePayload{Content: content})
	select {
	case sess.Inbox <- session.ClientMessage{Type: session.ClientUserMessage, Payload: payload}:
	case <-time.After(time.Second):
		t.Fatal("inbox send timed out")
	}
}

func integrationCollect(t *testing.T, sess *session.Session, timeout time.Duration, match func([]session.Event) bool) []session.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var events []session.Event
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timeout waiting for events; got %v", integrationEventTypes(events))
		}
		select {
		case e := <-sess.Outbox:
			events = append(events, e)
			if match(events) {
				return events
			}
		case <-time.After(remaining):
			t.Fatalf("timeout waiting for events; got %v", integrationEventTypes(events))
		}
	}
}

func integrationEventTypes(es []session.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Type
	}
	return out
}

func integrationCountType(es []session.Event, t string) int {
	n := 0
	for _, e := range es {
		if e.Type == t {
			n++
		}
	}
	return n
}

func waitForStateIdle(t *testing.T, sess *session.Session, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sess.State() == session.StateIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session did not return to idle within %v (state=%s)", timeout, sess.State())
}

func waitForDelegationCount(t *testing.T, st store.Store, sessionID string, want int) []*store.Delegation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, err := st.ListDelegations(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) >= want {
			return list
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("delegation count never reached %d", want)
	return nil
}

// TestUS1_FullChain_DelegateCompleteNotifyRead exercises the whole US1 path:
// the parent loop calls delegate, the background child finishes, the next turn
// injects a completion notice, and delegation_read returns the full result.
func TestUS1_FullChain_DelegateCompleteNotifyRead(t *testing.T) {
	childResult := "Auth uses bearer tokens in the Authorization header."
	h := newIntegrationHarness(t, func() []provider.ProviderEvent {
		return []provider.ProviderEvent{
			{Type: provider.EventTextDelta, TextDelta: childResult},
			{Type: provider.EventStop, StopReason: "end_turn"},
		}
	})
	parent := h.createParent(t)

	// Turn 1, provider call 1: emit a delegate tool_use.
	h.parentMock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type:      provider.BlockToolUse,
			ToolUseID: "tu-delegate",
			ToolName:  "delegate",
			Input:     json.RawMessage(`{"description":"Research auth","prompt":"Explain how auth works."}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	// Turn 1, provider call 2: acknowledge, then end the turn.
	h.parentMock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "delegated, standing by"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	integrationSendUser(t, parent, "research auth in the background for me")
	integrationCollect(t, parent, 3*time.Second, func(es []session.Event) bool {
		return integrationCountType(es, "assistant_text_done") >= 1
	})
	waitForStateIdle(t, parent, 2*time.Second)

	// The background child should complete; wait for exactly-once notification
	// eligibility (status != running).
	list := waitForDelegationCount(t, h.st, parent.ID, 1)
	delegationID := list[0].ID
	d := waitForDelegationTerminal(t, h.st, delegationID, 5*time.Second)
	if d.Status != store.DelegationComplete {
		t.Fatalf("child status: got %s want complete (err=%q)", d.Status, d.Error)
	}

	// Turn 2: a fresh user message triggers notification injection before the
	// user message is appended.
	h.parentMock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "ok"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
	integrationSendUser(t, parent, "what did you find?")
	integrationCollect(t, parent, 3*time.Second, func(es []session.Event) bool {
		return integrationCountType(es, "assistant_text_done") >= 1
	})
	waitForStateIdle(t, parent, 2*time.Second)

	// History must contain exactly one system notice carrying the delegation id,
	// positioned before the second user message.
	hist := parent.History()
	noticeIdx := -1
	secondUserIdx := -1
	notices := 0
	for i, m := range hist {
		if m.Role == provider.RoleSystem {
			notices++
			if noticeIdx < 0 {
				noticeIdx = i
			}
		}
		if m.Role == provider.RoleUser && secondUserIdx < 0 && i > noticeIdx && noticeIdx >= 0 {
			secondUserIdx = i
		}
	}
	if notices != 1 {
		t.Fatalf("expected exactly one injected system notice, got %d", notices)
	}
	if !strings.Contains(hist[noticeIdx].Content[0].Text, delegationID) {
		t.Fatalf("notice missing delegation id %q: %q", delegationID, hist[noticeIdx].Content[0].Text)
	}
	if secondUserIdx < 0 || secondUserIdx < noticeIdx {
		t.Fatalf("notice must precede the second user message; notice=%d user=%d", noticeIdx, secondUserIdx)
	}

	// delegation_read returns the full child result.
	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir, Delegations: h.dMgr}
	readInput, _ := json.Marshal(map[string]string{"id": delegationID})
	res, err := delegationtool.DelegationReadTool{}.Run(context.Background(), env, readInput)
	if err != nil || res.IsError {
		t.Fatalf("delegation_read: err=%v res=%+v", err, res)
	}
	if res.Output != childResult {
		t.Fatalf("delegation_read output: got %q want %q", res.Output, childResult)
	}

	// A third turn must NOT re-inject the notice (exactly-once).
	h.parentMock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
	integrationSendUser(t, parent, "thanks")
	integrationCollect(t, parent, 3*time.Second, func(es []session.Event) bool {
		return integrationCountType(es, "assistant_text_done") >= 1
	})
	notices = 0
	for _, m := range parent.History() {
		if m.Role == provider.RoleSystem {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("notice re-injected on third turn: want 1, got %d", notices)
	}
}

// TestUS1_ChildToolSurfaceIsReadOnly captures the live child session while it is
// blocked in its provider stream and asserts its AllowedTools excludes every
// mutating tool (spec US1 scenarios 5/6).
func TestUS1_ChildToolSurfaceIsReadOnly(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := newIntegrationHarness(t, func() []provider.ProviderEvent {
		<-release // block the child's provider stream until the test releases it
		return []provider.ProviderEvent{{Type: provider.EventStop, StopReason: "end_turn"}}
	})
	parent := h.createParent(t)

	h.parentMock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type:      provider.BlockToolUse,
			ToolUseID: "tu-delegate",
			ToolName:  "delegate",
			Input:     json.RawMessage(`{"description":"Research","prompt":"do it"}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	h.parentMock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
	integrationSendUser(t, parent, "delegate research")
	integrationCollect(t, parent, 3*time.Second, func(es []session.Event) bool {
		return integrationCountType(es, "assistant_text_done") >= 1
	})

	// The child is blocked in its Stream call; find it among live sessions.
	deadline := time.Now().Add(3 * time.Second)
	var child *session.Session
	for time.Now().Before(deadline) {
		for _, s := range h.mgr.ListSessions() {
			if s.ID == parent.ID || !s.Ephemeral {
				continue
			}
			if _, ok := s.Metadata[delegation.ChildMetaKey]; ok {
				child = s
				break
			}
		}
		if child != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child == nil {
		t.Fatal("child session never appeared")
	}

	allowed := child.AllowedTools()
	banned := []string{"Write", "Edit", "Bash", "Dispatch", "delegate", "delegation_read", "schedule_create"}
	for _, b := range banned {
		if contains(allowed, b) {
			t.Errorf("child AllowedTools must not include %q (was %v)", b, allowed)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
