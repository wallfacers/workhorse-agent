package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/api"
	"github.com/wallfacers/workhorse-agent/internal/extagent/approval"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// noopRunner satisfies session.Runner for tests that don't actually drive the
// agent loop. Run blocks until ctx is cancelled, mirroring real runner
// shutdown behavior.
type noopRunner struct{}

func (noopRunner) Run(ctx context.Context) { <-ctx.Done() }

// newTestServer builds a Server backed by an in-memory store and a session
// manager whose runner factory is a no-op (we don't drive the agent loop —
// only the approvals handler is under test here).
func newTestServer(t *testing.T) (*api.Server, *session.Manager) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: dbPath, BusyTimeoutMs: 5000})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mgr := session.NewManager(session.ManagerOptions{
		Store:         st,
		MaxConcurrent: 10,
		RunnerFactory: func(*session.Session) session.Runner {
			return noopRunner{}
		},
	})
	cfg := api.Config{
		Host:                    "127.0.0.1",
		Port:                    0,
		MaxRequestBodyBytes:     1 << 20,
		GracefulShutdownTimeout: time.Second,
	}
	return api.NewServer(cfg, mgr, st, nil), mgr
}

func newSession(t *testing.T, mgr *session.Manager) string {
	t.Helper()
	sess, err := mgr.CreateSession(context.Background(), session.Options{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess.ID
}

func makePending(t *testing.T, sessionID string) (*approval.PendingApproval, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "draft.yaml")
	if err := os.WriteFile(path, []byte("name: gemini\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &approval.PendingApproval{
		SessionID: sessionID,
		AgentName: "gemini",
		DraftPath: path,
		DraftYAML: "name: gemini\n",
	}, path
}

func TestApprovalEndpoint_404OnUnknownApproval(t *testing.T) {
	srv, mgr := newTestServer(t)
	mgrApp := approval.New(approval.Options{Timeout: time.Minute})
	srv.SetApprovalManager(mgrApp)
	sessionID := newSession(t, mgr)

	body := bytes.NewBufferString(`{"decision":"approve"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/approvals/nope", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestApprovalEndpoint_ApproveSuccess(t *testing.T) {
	srv, mgr := newTestServer(t)
	mgrApp := approval.New(approval.Options{Timeout: time.Minute})
	srv.SetApprovalManager(mgrApp)
	sessionID := newSession(t, mgr)
	pending, _ := makePending(t, sessionID)
	approvalID := mgrApp.Register(pending)

	body := bytes.NewBufferString(`{"decision":"approve"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/approvals/"+approvalID, body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "approve" {
		t.Errorf("status field: %q", resp["status"])
	}
}

func TestApprovalEndpoint_InvalidDecision(t *testing.T) {
	srv, mgr := newTestServer(t)
	mgrApp := approval.New(approval.Options{Timeout: time.Minute})
	srv.SetApprovalManager(mgrApp)
	sessionID := newSession(t, mgr)
	pending, _ := makePending(t, sessionID)
	approvalID := mgrApp.Register(pending)

	body := bytes.NewBufferString(`{"decision":"maybe"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/approvals/"+approvalID, body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestApprovalEndpoint_SessionNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	mgrApp := approval.New(approval.Options{Timeout: time.Minute})
	srv.SetApprovalManager(mgrApp)

	body := bytes.NewBufferString(`{"decision":"approve"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/01ZZZZZZZZZZZZZZZZZZZZZZZZ/approvals/aprv1", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown session, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestApprovalEndpoint_NoManagerConfigured(t *testing.T) {
	srv, mgr := newTestServer(t)
	sessionID := newSession(t, mgr)
	body := bytes.NewBufferString(`{"decision":"approve"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/approvals/aprv1", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestSessionEventEmitter_EmitPushesToOutbox(t *testing.T) {
	_, mgr := newTestServer(t)
	sessionID := newSession(t, mgr)
	sess, _ := mgr.GetSession(sessionID)

	em := api.NewSessionEventEmitter(mgr)
	em.EmitApprovalEvent(sessionID, "adapter_approval_request", map[string]any{
		"approval_id": "test-id",
		"agent_name":  "gemini",
	})
	select {
	case ev := <-sess.Outbox:
		if ev.Type != "adapter_approval_request" {
			t.Errorf("event type: %q", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not pushed to Outbox")
	}
}
