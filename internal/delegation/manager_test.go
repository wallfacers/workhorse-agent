package delegation_test

import (
	"context"
	"fmt"
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
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

type managerHarness struct {
	dMgr *delegation.Manager
	mgr  *session.Manager
	st   store.Store
}

// newManagerHarness wires a session.Manager whose RunnerFactory drives each
// delegation child session with a real agent.Loop backed by a mock provider.
// childFallback controls the child's scripted behavior; the parent session gets
// a trivial end_turn response.
func newManagerHarness(t *testing.T, childFallback func() []provider.ProviderEvent) *managerHarness {
	t.Helper()
	st := openDelegationStore(t)
	orch := &agent.Orchestrator{Registry: tools.NewRegistry(), MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	factory := func(sess *session.Session) session.Runner {
		mp := mockprovider.New("mock")
		if _, isChild := sess.Metadata[delegation.ChildMetaKey]; isChild && childFallback != nil {
			mp.SetFallback(childFallback)
		} else {
			mp.SetFallback(func() []provider.ProviderEvent {
				return []provider.ProviderEvent{{Type: provider.EventStop, StopReason: "end_turn"}}
			})
		}
		loop := agent.NewLoop(agent.LoopConfig{
			Model:              "m",
			MaxTokens:          1024,
			CancelDrainTimeout: 500 * time.Millisecond,
		})
		loop.Session = sess
		loop.Provider = mp
		loop.Orchestrator = orch
		loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}
		return loop
	}
	mgr := session.NewManager(session.ManagerOptions{RunnerFactory: factory, Store: st, MaxConcurrent: 50})
	return &managerHarness{
		dMgr: delegation.NewManager(st, mgr, slog.New(slog.NewTextHandler(io.Discard, nil))),
		mgr:  mgr,
		st:   st,
	}
}

func openDelegationStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (h *managerHarness) newParent(t *testing.T) *session.Session {
	t.Helper()
	sess, err := h.mgr.CreateSession(context.Background(), session.Options{Workdir: t.TempDir(), Ephemeral: true})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	t.Cleanup(func() { _ = h.mgr.DeleteSession(context.Background(), sess.ID, time.Second) })
	return sess
}

func waitForDelegationTerminal(t *testing.T, st store.Store, id string, timeout time.Duration) *store.Delegation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d, err := st.GetDelegation(context.Background(), id)
		if err != nil {
			t.Fatalf("get delegation %s: %v", id, err)
		}
		if d.Status != store.DelegationRunning {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("delegation %s did not terminate within %v", id, timeout)
	return nil
}

func TestManager_StartCompletesAndPersistsResult(t *testing.T) {
	body := "Auth uses bearer tokens in headers."
	h := newManagerHarness(t, func() []provider.ProviderEvent {
		return []provider.ProviderEvent{
			{Type: provider.EventTextDelta, TextDelta: body},
			{Type: provider.EventStop, StopReason: "end_turn"},
		}
	})
	parent := h.newParent(t)

	id, err := h.dMgr.Start(context.Background(), parent.ID, parent.Workdir, "Research auth flow", "Explain permission checks.")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := len(strings.Split(id, "-")); got != 3 {
		t.Fatalf("id %q: want 3 dash-separated parts", id)
	}

	d := waitForDelegationTerminal(t, h.st, id, 5*time.Second)
	if d.Status != store.DelegationComplete {
		t.Fatalf("status: got %s want complete (err=%q)", d.Status, d.Error)
	}
	if d.Result != body {
		t.Fatalf("result: got %q want %q", d.Result, body)
	}
	if d.Title != body { // single short line → title is the whole line
		t.Fatalf("title: got %q want %q", d.Title, body)
	}
	if !strings.Contains(d.Summary, "Auth uses") {
		t.Fatalf("summary: got %q", d.Summary)
	}
	if rc := len([]rune(d.Title)); rc > 48 {
		t.Fatalf("title %d runes exceeds 48", rc)
	}
	if rc := len([]rune(d.Summary)); rc > 180 {
		t.Fatalf("summary %d runes exceeds 180", rc)
	}
	if d.CompletedAt == nil {
		t.Fatal("completed_at not set")
	}
}

func TestManager_StartFailsOnChildError(t *testing.T) {
	h := newManagerHarness(t, func() []provider.ProviderEvent {
		return []provider.ProviderEvent{{
			Type:  provider.EventError,
			Error: provider.NewProviderError("mock", 0, provider.CodeStreamBroken, "model overloaded", nil),
		}}
	})
	parent := h.newParent(t)

	id, err := h.dMgr.Start(context.Background(), parent.ID, parent.Workdir, "Research", "do it")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	d := waitForDelegationTerminal(t, h.st, id, 5*time.Second)
	if d.Status != store.DelegationError {
		t.Fatalf("status: got %s want error", d.Status)
	}
	if !strings.Contains(d.Error, "model overloaded") {
		t.Fatalf("error message: got %q", d.Error)
	}
}

func TestManager_StartRejectsConcurrencyCap(t *testing.T) {
	h := newManagerHarness(t, nil)
	parent := h.newParent(t)
	ctx := context.Background()

	for i := 0; i < delegation.MaxConcurrent; i++ {
		if err := h.st.CreateDelegation(ctx, &store.Delegation{
			ID:          fmt.Sprintf("cap-seed-%d", i),
			SessionID:   parent.ID,
			Description: "blocked",
			Prompt:      "p",
			Workdir:     parent.Workdir,
			Status:      store.DelegationRunning,
			StartedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	_, err := h.dMgr.Start(ctx, parent.ID, parent.Workdir, "extra", "do it")
	if err == nil {
		t.Fatal("expected concurrency cap error, got nil")
	}
	if !strings.Contains(err.Error(), "Too many running delegations") {
		t.Fatalf("error: %v", err)
	}
}

func TestManager_StartRejectsNested(t *testing.T) {
	h := newManagerHarness(t, nil)
	parent, err := h.mgr.CreateSession(context.Background(), session.Options{
		Workdir:   t.TempDir(),
		Ephemeral: true,
		Metadata:  map[string]string{delegation.ChildMetaKey: "parent-delegation"},
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	t.Cleanup(func() { _ = h.mgr.DeleteSession(context.Background(), parent.ID, time.Second) })

	_, err = h.dMgr.Start(context.Background(), parent.ID, parent.Workdir, "nested", "do it")
	if err == nil {
		t.Fatal("expected nested rejection, got nil")
	}
	if !strings.Contains(err.Error(), "Nested delegations") {
		t.Fatalf("error: %v", err)
	}
}

func TestManager_StartValidationErrors(t *testing.T) {
	h := newManagerHarness(t, nil)
	parent := h.newParent(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		description string
		prompt      string
		want        string
	}{
		{"empty description", "", "do it", "description is required"},
		{"empty prompt", "task", "  ", "prompt is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.dMgr.Start(ctx, parent.ID, parent.Workdir, tc.description, tc.prompt)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q in error, got %v", tc.want, err)
			}
		})
	}
}

func TestManager_ConsumeNotificationsExactlyOnce(t *testing.T) {
	h := newManagerHarness(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := h.st.CreateDelegation(ctx, &store.Delegation{
		ID: "brisk-amber-fox", SessionID: "sess1", Description: "d", Prompt: "p",
		Workdir: "/tmp", Status: store.DelegationRunning, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CompleteDelegation(ctx, "brisk-amber-fox", "Auth Flow", "summary text", "full result"); err != nil {
		t.Fatal(err)
	}

	first := h.dMgr.ConsumeNotifications(ctx, "sess1")
	if len(first) != 1 {
		t.Fatalf("first consume: want 1 notice, got %d", len(first))
	}
	notice := first[0]
	for _, want := range []string{"brisk-amber-fox", "Auth Flow", "delegation_read"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q:\n%s", want, notice)
		}
	}

	if second := h.dMgr.ConsumeNotifications(ctx, "sess1"); len(second) != 0 {
		t.Fatalf("second consume: want 0 (exactly-once), got %d", len(second))
	}
}

func TestReadOnlyToolSurfaceExcludesMutating(t *testing.T) {
	banned := []string{"Write", "Edit", "Bash", "Dispatch", "delegate", "schedule_create", "schedule_remove"}
	set := map[string]bool{}
	for _, a := range delegation.ReadOnlyAllowedTools {
		set[a] = true
	}
	for _, b := range banned {
		if set[b] {
			t.Errorf("mutating tool %q must not appear in the read-only surface", b)
		}
	}
}
