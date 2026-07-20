package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func newScheduleStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSchedule_CreateGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newScheduleStore(t)
	runAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	if err := s.CreateSchedule(ctx, &store.Schedule{
		ID: "smoke", Name: "smoke", Instruction: "do thing",
		RunAt: &runAt, Workdir: "/repo", Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create one-shot: %v", err)
	}
	got, err := s.GetSchedule(ctx, "smoke")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Cron != "" || got.RunAt == nil || !got.RunAt.Equal(runAt) || !got.Enabled {
		t.Fatalf("one-shot round-trip: cron=%q runAt=%v enabled=%v", got.Cron, got.RunAt, got.Enabled)
	}

	if err := s.CreateSchedule(ctx, &store.Schedule{
		ID: "audit", Name: "audit", Instruction: "audit", Cron: "0 9 * * 1-5",
		Workdir: "/repo", Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create cron: %v", err)
	}
	cron, _ := s.GetSchedule(ctx, "audit")
	if cron.Cron != "0 9 * * 1-5" || cron.RunAt != nil {
		t.Fatalf("cron round-trip: %+v", cron)
	}

	list, _ := s.ListSchedules(ctx)
	if len(list) != 2 {
		t.Fatalf("list: want 2 schedules, got %d", len(list))
	}
	if _, err := s.GetSchedule(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get ghost: %v", err)
	}
}

func TestSchedule_DeleteCascadesRuns(t *testing.T) {
	ctx := context.Background()
	s := newScheduleStore(t)
	if err := s.CreateSchedule(ctx, &store.Schedule{
		ID: "s1", Name: "s1", Instruction: "i", Cron: "* * * * *",
		Workdir: "/w", Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateScheduleRun(ctx, &store.ScheduleRun{
		ScheduleID: "s1", StartedAt: time.Now().UTC(), Status: store.ScheduleRunRunning,
	}); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.ListScheduleRuns(ctx, "s1", 10)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}

	if err := s.DeleteSchedule(ctx, "s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	runs, _ = s.ListScheduleRuns(ctx, "s1", 10)
	if len(runs) != 0 {
		t.Fatalf("runs must cascade-delete, got %d", len(runs))
	}
	if _, err := s.GetSchedule(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("schedule must be gone: %v", err)
	}
	if err := s.DeleteSchedule(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete ghost: %v", err)
	}
}

func TestSchedule_TouchDisablesOneShotOnly(t *testing.T) {
	ctx := context.Background()
	s := newScheduleStore(t)
	runAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	s.CreateSchedule(ctx, &store.Schedule{ID: "once", Name: "once", Instruction: "i", RunAt: &runAt, Workdir: "/w", Enabled: true, CreatedAt: time.Now().UTC()})
	s.CreateSchedule(ctx, &store.Schedule{ID: "rep", Name: "rep", Instruction: "i", Cron: "0 9 * * *", Workdir: "/w", Enabled: true, CreatedAt: time.Now().UTC()})

	now := time.Now().UTC()
	if err := s.TouchScheduleRun(ctx, "once", now); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchScheduleRun(ctx, "rep", now); err != nil {
		t.Fatal(err)
	}

	once, _ := s.GetSchedule(ctx, "once")
	if once.Enabled {
		t.Fatal("one-shot must be disabled after touch")
	}
	if once.LastRunAt == nil {
		t.Fatal("last_run_at must be set")
	}
	rep, _ := s.GetSchedule(ctx, "rep")
	if !rep.Enabled {
		t.Fatal("repeating schedule must stay enabled")
	}
}

func TestScheduleRun_CreatePrunesToTwenty(t *testing.T) {
	ctx := context.Background()
	s := newScheduleStore(t)
	if err := s.CreateSchedule(ctx, &store.Schedule{
		ID: "p", Name: "p", Instruction: "i", Cron: "* * * * *",
		Workdir: "/w", Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		id, err := s.CreateScheduleRun(ctx, &store.ScheduleRun{
			ScheduleID: "p",
			StartedAt:  base.Add(time.Duration(i) * time.Minute),
			Status:     store.ScheduleRunComplete,
			OutputTail: fmt.Sprintf("run %d", i),
		})
		if err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
		if id == 0 {
			t.Fatalf("run %d: id=0", i)
		}
	}

	var count int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schedule_runs WHERE schedule_id = 'p'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 20 {
		t.Fatalf("prune must keep 20 runs, DB has %d", count)
	}

	runs, _ := s.ListScheduleRuns(ctx, "p", 20)
	if len(runs) != 20 {
		t.Fatalf("list: want 20, got %d", len(runs))
	}
	// Newest first; the two oldest (run 0, run 1) were pruned, so the tail is run 2.
	if !strings.Contains(runs[0].OutputTail, "run 24") {
		t.Fatalf("newest first: got %q", runs[0].OutputTail)
	}
	if !strings.Contains(runs[19].OutputTail, "run 5") {
		t.Fatalf("oldest kept: got %q want run 5", runs[19].OutputTail)
	}

	// Finish updates the run.
	if err := s.FinishScheduleRun(ctx, runs[0].ID, store.ScheduleRunError, "tail-x", "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListScheduleRuns(ctx, "p", 1)
	if got[0].Status != store.ScheduleRunError || got[0].Error != "boom" ||
		got[0].OutputTail != "tail-x" || got[0].CompletedAt == nil {
		t.Fatalf("finish fields: %+v", got[0])
	}
}

func TestScheduleRun_ListLimitClamped(t *testing.T) {
	ctx := context.Background()
	s := newScheduleStore(t)
	s.CreateSchedule(ctx, &store.Schedule{ID: "c", Name: "c", Instruction: "i", Cron: "* * * * *", Workdir: "/w", Enabled: true, CreatedAt: time.Now().UTC()})
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		s.CreateScheduleRun(ctx, &store.ScheduleRun{ScheduleID: "c", StartedAt: base.Add(time.Duration(i) * time.Minute), Status: store.ScheduleRunComplete})
	}
	// default (<=0) means 5; clamp to available count.
	def, _ := s.ListScheduleRuns(ctx, "c", 0)
	if len(def) != 3 {
		t.Fatalf("default limit: got %d want 3", len(def))
	}
	// limit > 20 clamps to 20.
	big, _ := s.ListScheduleRuns(ctx, "c", 100)
	if len(big) != 3 {
		t.Fatalf("over-limit: got %d want 3", len(big))
	}
}
