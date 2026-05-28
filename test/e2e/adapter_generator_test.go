package e2e_test

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
	"github.com/wallfacers/workhorse-agent/internal/extagent/draft"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// noopRunner satisfies session.Runner without driving the agent loop —
// these e2e tests target the approval HTTP surface, not the LLM cycle.
type noopRunner struct{}

func (noopRunner) Run(ctx context.Context) { <-ctx.Done() }

// validAdapterYAML returns a draft body that survives the publisher's
// re-validation step.
func validAdapterYAML(name string) string {
	return `name: ` + name + `
binary: ` + name + `
class: sub_agent
invocation:
  prompt_via: stdin
  extra_args: []
  env_passthrough: []
output:
  format: text
  stderr: separate
control:
  cancel_signal: SIGINT
  cancel_grace_sec: 5
  default_timeout_sec: 600
  max_timeout_sec: 3600
security:
  network: allowed
  filesystem: full
  trusted: false
smoke_test:
  prompt: "Reply with exactly: WORKHORSE_SMOKE_OK"
  expected_substring: "WORKHORSE_SMOKE_OK"
  timeout_sec: 60
description: "e2e test fixture"
provenance:
  source: llm_generated
`
}

// TestE2E_ApproveLifecycle_Publishes drives a full approve via the HTTP
// surface: session created → pending approval registered with a draft on
// disk → POST /v1/sessions/{id}/approvals/{aid} {approve} → server-side
// Publisher relocates the file. Confirms the integration of approval
// manager + HTTP handler + draft.Publisher works without the LLM in the
// loop.
func TestE2E_ApproveLifecycle_Publishes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: dbPath, BusyTimeoutMs: 5000})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mgr := session.NewManager(session.ManagerOptions{
		Store:         st,
		MaxConcurrent: 10,
		RunnerFactory: func(*session.Session) session.Runner { return noopRunner{} },
	})
	sess, err := mgr.CreateSession(context.Background(), session.Options{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Build extDir + drafts/ layout the publisher expects.
	extDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(extDir, ".drafts"), 0o700); err != nil {
		t.Fatal(err)
	}
	draftPath := filepath.Join(extDir, ".drafts", "e2eapp.yaml")
	if err := os.WriteFile(draftPath, []byte(validAdapterYAML("e2eapp")), 0o600); err != nil {
		t.Fatal(err)
	}

	pub := &draft.Publisher{LiveDir: extDir}
	approvalMgr := approval.New(approval.Options{
		Timeout:   time.Minute,
		Publisher: approverFromDraft(pub),
	})
	approvalID := approvalMgr.Register(&approval.PendingApproval{
		SessionID: sess.ID,
		AgentName: "e2eapp",
		DraftPath: draftPath,
		DraftYAML: validAdapterYAML("e2eapp"),
		Provenance: approval.Provenance{
			GeneratedBy: "anthropic:claude-opus-4-7",
			GeneratedAt: time.Now().UTC(),
			ToolVersion: "1.0.0",
		},
	})

	srv := api.NewServer(api.Config{
		Host: "127.0.0.1", Port: 0,
		MaxRequestBodyBytes:     1 << 20,
		GracefulShutdownTimeout: time.Second,
	}, mgr, st, nil)
	srv.SetApprovalManager(approvalMgr)

	body := bytes.NewBufferString(`{"decision":"approve"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sess.ID+"/approvals/"+approvalID, body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "approve" {
		t.Errorf("unexpected status: %q", resp["status"])
	}

	livePath := filepath.Join(extDir, "e2eapp.yaml")
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("live adapter not published: %v", err)
	}
	if _, err := os.Stat(draftPath); !os.IsNotExist(err) {
		t.Errorf("draft should be moved, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(extDir, "e2eapp"+draft.GenmetaExt)); err != nil {
		t.Errorf("genmeta sibling not written: %v", err)
	}
}

// approverFromDraft adapts draft.Publisher to approval.Publisher. The real
// cmd-level wiring uses the same pattern.
type draftAdapter struct {
	pub *draft.Publisher
}

func (d *draftAdapter) Publish(draftPath string, prov approval.Provenance) (string, error) {
	return d.pub.Publish(draftPath, draft.GenmetaPayload{
		GeneratedBy: prov.GeneratedBy,
		GeneratedAt: prov.GeneratedAt,
		ToolVersion: prov.ToolVersion,
	})
}

func approverFromDraft(p *draft.Publisher) approval.Publisher {
	return &draftAdapter{pub: p}
}
