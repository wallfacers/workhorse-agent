package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
)

func newSessionWithState(t *testing.T, s *Server, state session.State) *session.Session {
	t.Helper()
	sess, err := s.manager.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if state != session.StateIdle {
		// ForceTransition skips the from-check; the from-edge table doesn't
		// permit Idle → Executing directly, but tests need to start from
		// arbitrary states. Real code uses Transition through legal paths.
		if err := sess.ForceTransition(state); err != nil {
			// Fall back to a two-step legal path for AwaitPerm/Executing.
			_ = sess.ForceTransition(session.StateThinking)
			if e := sess.ForceTransition(state); e != nil {
				t.Fatalf("transition to %s: %v", state, e)
			}
		}
	}
	return sess
}

func postStream(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func TestStreamPost_404_NotFound(t *testing.T) {
	_, ts := newTestServer(t)
	resp := postStream(t, ts.URL+"/v1/sessions/01HFAKE_NOT_PRESENT/stream",
		`{"type":"user_message","content":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestStreamPost_415_NoJSON(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateIdle)
	resp, err := http.Post(ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		"text/plain", strings.NewReader(`{"type":"user_message","content":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415, got %d", resp.StatusCode)
	}
}

func TestStreamPost_400_UnknownType(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateIdle)
	resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		`{"type":"frobnicate"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "unknown_message_type" {
		t.Fatalf("code: %v", body["code"])
	}
	if det, ok := body["details"].(map[string]any); !ok || det["received_type"] != "frobnicate" {
		t.Fatalf("details: %v", body["details"])
	}
}

func TestStreamPost_202_UserMessage_Idle(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateIdle)
	resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		`{"type":"user_message","content":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	// Message must have landed on Inbox.
	select {
	case m := <-sess.Inbox:
		if m.Type != session.ClientUserMessage {
			t.Fatalf("inbox type: %s", m.Type)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("inbox empty")
	}
}

func TestStreamPost_409_UserMessage_Thinking(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateThinking)
	resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		`{"type":"user_message","content":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "session_busy" || body["state"] != "thinking" {
		t.Fatalf("body: %+v", body)
	}
	// SSE mirror must reach the outbox.
	select {
	case ev := <-sess.Outbox:
		if ev.Type != "error" || ev.Payload["code"] != "session_busy" {
			t.Fatalf("mirror event: %+v", ev)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no SSE mirror error event")
	}
}

func TestStreamPost_202_Interrupt_DrainsOutbox(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateThinking)
	// Pile up some pre-interrupt events.
	for i := 0; i < 5; i++ {
		_ = sess.EmitNow("assistant_text_delta", map[string]any{"delta": "x"})
	}
	resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		`{"type":"interrupt"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	// Outbox must be drained (immediate).
	select {
	case ev := <-sess.Outbox:
		t.Fatalf("outbox not drained, got %+v", ev)
	default:
		// good
	}
	// Inbox must carry the interrupt.
	select {
	case m := <-sess.Inbox:
		if m.Type != session.ClientInterrupt {
			t.Fatalf("inbox type: %s", m.Type)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no interrupt in inbox")
	}
}

func TestStreamPost_202_Interrupt_Cancelled_Idempotent(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateThinking)
	if err := sess.ForceTransition(session.StateCancelled); err != nil {
		t.Fatalf("force cancelled: %v", err)
	}
	resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		`{"type":"interrupt"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	// In the idempotent path we must NOT have re-pushed.
	select {
	case m := <-sess.Inbox:
		t.Fatalf("inbox should be untouched, got %v", m)
	default:
	}
}

func TestStreamPost_202_Ping_AnyState(t *testing.T) {
	for _, st := range []session.State{
		session.StateIdle, session.StateThinking, session.StateExecuting,
		session.StateCompacting, session.StateAwaitPerm, session.StateCancelled,
	} {
		s, ts := newTestServer(t)
		sess := newSessionWithState(t, s, st)
		resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
			`{"type":"ping"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("state %s: ping want 202, got %d", st, resp.StatusCode)
		}
	}
}

func TestStreamPost_PermissionDecision_409_WhenNotAwaitPerm(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateThinking)
	resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		`{"type":"permission_decision","request_id":"r1","decision":"allow_once"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
}

func TestStreamPost_PermissionDecision_202_AwaitPerm(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateThinking)
	if err := sess.Transition(session.StateThinking, session.StateAwaitPerm); err != nil {
		t.Fatalf("transition: %v", err)
	}
	resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		`{"type":"permission_decision","request_id":"r1","decision":"allow_once"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	select {
	case dec := <-sess.PermissionAnswers:
		if dec.RequestID != "r1" || string(dec.Decision) != "allow_once" {
			t.Fatalf("decision: %+v", dec)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("permission decision not routed")
	}
}

func TestStreamPost_409_HistoryTokenLimit(t *testing.T) {
	s, ts := newTestServer(t, func(c *Config) { c.MaxHistoryTokens = 1 })
	sess := newSessionWithState(t, s, session.StateIdle)
	// Append enough history to blow past the 1-token cap (EstimateTokens
	// returns at least 1 for non-empty content).
	sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "lots of words go here far beyond one token"}},
	})
	resp := postStream(t, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		`{"type":"user_message","content":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "history_token_limit" {
		t.Fatalf("code: %v", body["code"])
	}
}

func TestStream_405_MethodNotAllowed(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateIdle)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/sessions/"+sess.ID+"/stream",
		bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow header: %q", got)
	}
}
