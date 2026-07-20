package schedule_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/schedule"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

func openScheduleStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newRunnerHarness(t *testing.T, childFallback func() []provider.ProviderEvent) (*schedule.Runner, store.Store, *session.Manager) {
	t.Helper()
	st := openScheduleStore(t)
	orch := &agent.Orchestrator{Registry: tools.NewRegistry(), MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	factory := func(sess *session.Session) session.Runner {
		mp := mockprovider.New("mock")
		if childFallback != nil {
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
		loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}
		return loop
	}
	mgr := session.NewManager(session.ManagerOptions{RunnerFactory: factory, Store: st, MaxConcurrent: 50})
	return schedule.NewRunner(st, mgr, slog.New(slog.NewTextHandler(io.Discard, nil))), st, mgr
}

func drainSessions(t *testing.T, mgr *session.Manager) {
	t.Helper()
	t.Cleanup(func() {
		for _, s := range mgr.ListSessions() {
			_ = mgr.DeleteSession(context.Background(), s.ID, time.Second)
		}
	})
}

func TestRunner_RunOnce_Succeeds(t *testing.T) {
	runner, st, mgr := newRunnerHarness(t, func() []provider.ProviderEvent {
		return []provider.ProviderEvent{
			{Type: provider.EventTextDelta, TextDelta: "audit done, no vulnerabilities found"},
			{Type: provider.EventStop, StopReason: "end_turn"},
		}
	})
	drainSessions(t, mgr)
	ctx := context.Background()
	sched := &store.Schedule{
		ID: "dep-audit", Name: "dep-audit", Instruction: "Run dependency audit",
		Workdir: t.TempDir(), Enabled: true, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateSchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}

	runner.RunOnce(ctx, sched)

	runs, _ := st.ListScheduleRuns(ctx, sched.ID, 5)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].Status != store.ScheduleRunComplete {
		t.Fatalf("status: got %s want complete (err=%q)", runs[0].Status, runs[0].Error)
	}
	if !strings.Contains(runs[0].OutputTail, "audit done") {
		t.Fatalf("output_tail: %q", runs[0].OutputTail)
	}
	if runs[0].SessionID == "" {
		t.Fatal("session_id should be recorded for replay")
	}
	if runs[0].CompletedAt == nil {
		t.Fatal("completed_at should be set")
	}
}

func TestRunner_RunOnce_ChildError(t *testing.T) {
	runner, st, mgr := newRunnerHarness(t, func() []provider.ProviderEvent {
		return []provider.ProviderEvent{{
			Type:  provider.EventError,
			Error: provider.NewProviderError("mock", 0, provider.CodeStreamBroken, "model down", nil),
		}}
	})
	drainSessions(t, mgr)
	ctx := context.Background()
	sched := &store.Schedule{ID: "fail", Name: "fail", Instruction: "i", Workdir: t.TempDir(), Enabled: true, CreatedAt: time.Now().UTC()}
	st.CreateSchedule(ctx, sched)

	runner.RunOnce(ctx, sched)

	runs, _ := st.ListScheduleRuns(ctx, sched.ID, 5)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].Status != store.ScheduleRunError {
		t.Fatalf("status: got %s want error", runs[0].Status)
	}
	if !strings.Contains(runs[0].Error, "model down") {
		t.Fatalf("error: %q", runs[0].Error)
	}
}

func TestRunner_RunOnce_MissingWorkdir(t *testing.T) {
	runner, st, _ := newRunnerHarness(t, nil)
	ctx := context.Background()
	sched := &store.Schedule{
		ID: "bad", Name: "bad", Instruction: "i",
		Workdir: "/nonexistent-schedule-path-xyz-12345", Enabled: true, CreatedAt: time.Now().UTC(),
	}
	st.CreateSchedule(ctx, sched)

	runner.RunOnce(ctx, sched)

	runs, _ := st.ListScheduleRuns(ctx, sched.ID, 5)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].Status != store.ScheduleRunError {
		t.Fatalf("status: got %s want error", runs[0].Status)
	}
	if !strings.Contains(runs[0].Error, "workdir") {
		t.Fatalf("error should mention workdir: %q", runs[0].Error)
	}
	// Schedule is kept (not deleted).
	if got, err := st.GetSchedule(ctx, sched.ID); err != nil || got == nil {
		t.Fatalf("schedule should be retained: %v", err)
	}
}
