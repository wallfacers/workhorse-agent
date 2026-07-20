package schedule_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/schedule"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	scheduletool "github.com/wallfacers/workhorse-agent/internal/tools/scheduletool"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

func waitForScheduleRun(t *testing.T, st store.Store, scheduleID string, timeout time.Duration) *store.ScheduleRun {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		runs, err := st.ListScheduleRuns(context.Background(), scheduleID, 5)
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		if len(runs) > 0 && runs[0].Status != store.ScheduleRunRunning {
			return runs[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run for schedule %q did not terminate within %v", scheduleID, timeout)
	return nil
}

// TestUS3_OneShotFullChain drives the whole US3 path: the schedule_create tool
// persists a one-shot plan, the Worker tick fires it, the Runner drives an
// unattended session, the run lands in schedule_runs, schedule_read_log reads
// it, and the plan is disabled (spec US3 scenarios 1-4).
func TestUS3_OneShotFullChain(t *testing.T) {
	childResult := "audit result: clean, no vulnerabilities"
	st := openScheduleStore(t)
	orch := &agent.Orchestrator{Registry: tools.NewRegistry(), MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	factory := func(sess *session.Session) session.Runner {
		mp := mockprovider.New("mock")
		mp.SetFallback(func() []provider.ProviderEvent {
			return []provider.ProviderEvent{
				{Type: provider.EventTextDelta, TextDelta: childResult},
				{Type: provider.EventStop, StopReason: "end_turn"},
			}
		})
		loop := agent.NewLoop(agent.LoopConfig{Model: "m", MaxTokens: 2048, CancelDrainTimeout: 500 * time.Millisecond})
		loop.Session = sess
		loop.Provider = mp
		loop.Orchestrator = orch
		loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}
		return loop
	}
	mgr := session.NewManager(session.ManagerOptions{RunnerFactory: factory, Store: st, MaxConcurrent: 50})
	drainSessions(t, mgr)
	runner := schedule.NewRunner(st, mgr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	worker := schedule.NewWorker(st, runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	dir := t.TempDir()
	runAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)

	env := &tools.Env{Workdir: dir, Schedules: st}
	createBody, _ := json.Marshal(map[string]string{
		"name": "smoke", "instruction": "Run smoke check",
		"run_at": runAt.Format(time.RFC3339), "workdir": dir,
	})
	res, err := scheduletool.CreateTool{}.Run(ctx, env, createBody)
	if err != nil || res.IsError {
		t.Fatalf("schedule_create: err=%v res=%+v", err, res)
	}

	worker.SetClockForTest(func() time.Time { return runAt }, time.Minute)
	worker.Tick(ctx)

	run := waitForScheduleRun(t, st, "smoke", 5*time.Second)
	if run.Status != store.ScheduleRunComplete {
		t.Fatalf("run status: got %s want complete (err=%q)", run.Status, run.Error)
	}
	if !strings.Contains(run.OutputTail, childResult) {
		t.Fatalf("output_tail: %q", run.OutputTail)
	}
	if run.SessionID == "" {
		t.Fatal("session_id should be recorded for replay")
	}

	sched, _ := st.GetSchedule(ctx, "smoke")
	if sched.Enabled {
		t.Fatal("one-shot must be disabled (enabled=0) after firing (FR-019)")
	}

	logBody, _ := json.Marshal(map[string]string{"id": "smoke"})
	logRes, _ := scheduletool.ReadLogTool{}.Run(ctx, env, logBody)
	if logRes.IsError {
		t.Fatalf("read_log: %+v", logRes)
	}
	if !strings.Contains(logRes.Output, "complete") || !strings.Contains(logRes.Output, run.SessionID) {
		t.Fatalf("run log output: %s", logRes.Output)
	}
}

type stubSensitiveTool struct{}

func (stubSensitiveTool) Name() string                  { return "SensitiveOp" }
func (stubSensitiveTool) Description() string           { return "A tool that needs permission" }
func (stubSensitiveTool) InputSchema() json.RawMessage  { return []byte(`{}`) }
func (stubSensitiveTool) IsReadOnly() bool              { return false }
func (stubSensitiveTool) CanRunInParallel() bool        { return false }
func (stubSensitiveTool) DefaultTimeout() time.Duration { return 0 }
func (stubSensitiveTool) Run(context.Context, *tools.Env, json.RawMessage) (*tools.Result, error) {
	return &tools.Result{Output: "ran (should not happen unattended)"}, nil
}

// TestUS3_UnattendedPermissionTimesOutDoesNotHang verifies spec US3 scenario 7:
// a permission request in an unattended run has no one to answer it, so it
// times out to Deny; the run keeps going and finishes instead of hanging.
func TestUS3_UnattendedPermissionTimesOutDoesNotHang(t *testing.T) {
	st := openScheduleStore(t)
	reg := tools.NewRegistry()
	if err := reg.Register(stubSensitiveTool{}); err != nil {
		t.Fatal(err)
	}
	// prompt blocks until the per-request ctx expires (no answer arrives), then
	// reports unanswered → the manager returns Deny/SourceTimeout.
	permMgr := permission.New(st,
		func(ctx context.Context, _ permission.Request) (permission.Decision, bool) {
			<-ctx.Done()
			return permission.Deny, false
		},
		func(string, string) (bool, string) { return false, "" },
		200*time.Millisecond,
		permission.AllowOnce,
	)
	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	mp := mockprovider.New("mock")
	factory := func(sess *session.Session) session.Runner {
		loop := agent.NewLoop(agent.LoopConfig{Model: "m", MaxTokens: 2048, CancelDrainTimeout: 500 * time.Millisecond})
		loop.Session = sess
		loop.Provider = mp
		loop.Orchestrator = orch
		loop.Permissions = permMgr
		loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}
		return loop
	}
	mgr := session.NewManager(session.ManagerOptions{RunnerFactory: factory, Store: st, MaxConcurrent: 50})
	drainSessions(t, mgr)
	runner := schedule.NewRunner(st, mgr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	worker := schedule.NewWorker(st, runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	dir := t.TempDir()
	runAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	if err := st.CreateSchedule(ctx, &store.Schedule{
		ID: "perm", Name: "perm", Instruction: "do a sensitive thing",
		RunAt: &runAt, Workdir: dir, Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// First provider call asks for the gated tool (denied on timeout); the
	// second call finishes the turn.
	mp.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: "SensitiveOp",
			Input: json.RawMessage(`{}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	mp.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "done after denial"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	worker.SetClockForTest(func() time.Time { return runAt }, time.Minute)
	start := time.Now()
	worker.Tick(ctx)

	run := waitForScheduleRun(t, st, "perm", 5*time.Second)
	elapsed := time.Since(start)
	if run.Status != store.ScheduleRunComplete {
		t.Fatalf("run should complete after the permission denial, got %s (err=%q)", run.Status, run.Error)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("run appears to have hung on the permission prompt: took %v", elapsed)
	}
	if !strings.Contains(run.OutputTail, "done after denial") {
		t.Fatalf("output_tail: %q", run.OutputTail)
	}
}
