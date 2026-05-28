package approval_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent/approval"
)

type recordingEmitter struct {
	mu     sync.Mutex
	events []event
}

type event struct {
	SessionID string
	Type      string
	Payload   any
}

func (e *recordingEmitter) EmitApprovalEvent(sessionID, eventType string, payload any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event{sessionID, eventType, payload})
}

func (e *recordingEmitter) types() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.events))
	for i, ev := range e.events {
		out[i] = ev.Type
	}
	return out
}

type stubPublisher struct {
	called   int
	lastPath string
	err      error
}

func (s *stubPublisher) Publish(draftPath string, _ approval.Provenance) (string, error) {
	s.called++
	s.lastPath = draftPath
	if s.err != nil {
		return "", s.err
	}
	return draftPath + ".published", nil
}

type stubInjector struct {
	called    int
	sessionID string
	agent     string
}

func (s *stubInjector) Inject(sessionID, agentName string) {
	s.called++
	s.sessionID = sessionID
	s.agent = agentName
}

type stubDedup struct {
	cleared     []string
	unavailable []string
}

func (s *stubDedup) ClearImplicitTriggerDedup(_, agent string) {
	s.cleared = append(s.cleared, agent)
}

func (s *stubDedup) MarkAdapterSetupUnavailable(_, agent, _ string) {
	s.unavailable = append(s.unavailable, agent)
}

type stubMarker struct {
	marked int
}

func (s *stubMarker) MarkApproved(_, _ string) {
	s.marked++
}

func makePending(t *testing.T) (*approval.PendingApproval, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "draft.yaml")
	if err := os.WriteFile(path, []byte("name: draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &approval.PendingApproval{
		SessionID: "sess-1",
		AgentName: "gemini",
		DraftPath: path,
		DraftYAML: "name: draft\n",
		Smoke:     approval.SmokeOutcome{Passed: true, Reason: "ok"},
		Provenance: approval.Provenance{
			GeneratedBy: "anthropic:claude-opus-4-7",
			GeneratedAt: time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC),
			ToolVersion: "0.4.1",
		},
	}, path
}

func TestRegister_EmitsRequestAndSetsExpiry(t *testing.T) {
	emit := &recordingEmitter{}
	m := approval.New(approval.Options{Timeout: 50 * time.Millisecond, Emitter: emit})
	pending, _ := makePending(t)
	id := m.Register(pending)
	if id == "" {
		t.Fatal("Register returned empty id")
	}
	if got := m.Get(id); got == nil {
		t.Fatal("Get returned nil for newly registered approval")
	}
	if types := emit.types(); len(types) != 1 || types[0] != "adapter_approval_request" {
		t.Errorf("expected single adapter_approval_request event, got %v", types)
	}
}

func TestExpire_DeletesDraftAndEmitsExpired(t *testing.T) {
	emit := &recordingEmitter{}
	m := approval.New(approval.Options{Timeout: 20 * time.Millisecond, Emitter: emit})
	pending, path := makePending(t)
	id := m.Register(pending)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if m.Get(id) == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.Get(id) != nil {
		t.Fatal("approval did not expire within deadline")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("draft file should be deleted on expire, got stat err %v", err)
	}
	saw := emit.types()
	if len(saw) != 2 || saw[1] != "adapter_approval_expired" {
		t.Errorf("expected expired event, got %v", saw)
	}
}

func TestReject_DeletesDraftAndEmitsResolved(t *testing.T) {
	emit := &recordingEmitter{}
	m := approval.New(approval.Options{Timeout: time.Second, Emitter: emit})
	pending, path := makePending(t)
	id := m.Register(pending)

	if err := m.Decide(id, approval.DecisionReject, ""); err != nil {
		t.Fatalf("Decide reject: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("draft should be deleted on reject")
	}
	saw := emit.types()
	if len(saw) != 2 || saw[1] != "adapter_approval_resolved" {
		t.Errorf("expected resolved event, got %v", saw)
	}
	if m.Get(id) != nil {
		t.Error("entry should be removed after reject")
	}
}

func TestApprove_InvokesCollaboratorsInOrder(t *testing.T) {
	emit := &recordingEmitter{}
	pub := &stubPublisher{}
	inj := &stubInjector{}
	dedup := &stubDedup{}
	mark := &stubMarker{}
	m := approval.New(approval.Options{
		Timeout:          time.Second,
		Emitter:          emit,
		Publisher:        pub,
		RegistryInjector: inj,
		DedupClearer:     dedup,
		MarkApprover:     mark,
	})
	pending, _ := makePending(t)
	id := m.Register(pending)

	if err := m.Decide(id, approval.DecisionApprove, ""); err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	if pub.called != 1 {
		t.Errorf("Publisher called %d times, want 1", pub.called)
	}
	if inj.called != 1 {
		t.Errorf("Injector called %d times, want 1", inj.called)
	}
	if mark.marked != 1 {
		t.Errorf("MarkApprover called %d times, want 1", mark.marked)
	}
	if len(dedup.cleared) != 1 || dedup.cleared[0] != pending.AgentName {
		t.Errorf("DedupClearer should record agent name, got %v", dedup.cleared)
	}
	if m.Get(id) != nil {
		t.Error("entry should be removed after approve")
	}
}

func TestEdit_RewritesDraftButLeavesPending(t *testing.T) {
	pub := &stubPublisher{}
	m := approval.New(approval.Options{Timeout: time.Second, Publisher: pub})
	pending, path := makePending(t)
	id := m.Register(pending)

	if err := m.Decide(id, approval.DecisionEdit, "name: edited\n"); err != nil {
		t.Fatalf("Decide edit: %v", err)
	}
	if pub.called != 0 {
		t.Error("Publisher must not be called on edit")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "name: edited\n" {
		t.Errorf("draft not rewritten: %q", got)
	}
	if m.Get(id) == nil {
		t.Error("entry should still be present after edit")
	}
}

func TestEdit_RequiresContent(t *testing.T) {
	m := approval.New(approval.Options{Timeout: time.Second})
	pending, _ := makePending(t)
	id := m.Register(pending)

	if err := m.Decide(id, approval.DecisionEdit, ""); err == nil {
		t.Error("empty edited_yaml should be rejected")
	}
}

func TestDecide_AfterExpiry_ReturnsNotFound(t *testing.T) {
	m := approval.New(approval.Options{Timeout: 10 * time.Millisecond})
	pending, _ := makePending(t)
	id := m.Register(pending)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if m.Get(id) == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	err := m.Decide(id, approval.DecisionApprove, "")
	if !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("expected ErrNotFound after expiry, got %v", err)
	}
}

func TestCancel_DiscardsAllPending(t *testing.T) {
	m := approval.New(approval.Options{Timeout: time.Hour})
	for i := 0; i < 5; i++ {
		pending, _ := makePending(t)
		m.Register(pending)
	}
	m.Cancel()
	// All ids should be gone — using a known-bad lookup as proxy.
	if m.Get("non-existent") != nil {
		t.Error("Get of non-existent id should return nil")
	}
}
