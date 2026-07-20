package schedule_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/schedule"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

type fakeRunner struct {
	mu   sync.Mutex
	runs []string
}

func (f *fakeRunner) RunOnce(_ context.Context, sched *store.Schedule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, sched.ID)
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func newWorkerHarness(t *testing.T) (*schedule.Worker, store.Store, *fakeRunner) {
	t.Helper()
	st := openScheduleStore(t)
	fr := &fakeRunner{}
	w := schedule.NewWorker(st, fr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return w, st, fr
}

// settle lets the fire goroutine land so the fake runner records the call.
func settle() { time.Sleep(50 * time.Millisecond) }

func TestWorker_SameMinuteDedupAndNextMinute(t *testing.T) {
	w, st, fr := newWorkerHarness(t)
	clock := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local) // Tuesday 9:00
	w.SetClockForTest(func() time.Time { return clock }, time.Minute)
	ctx := context.Background()
	if err := st.CreateSchedule(ctx, &store.Schedule{
		ID: "per-min", Name: "p", Instruction: "i", Cron: "*/1 * * * *",
		Workdir: t.TempDir(), Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	w.Tick(ctx)
	settle()
	if c := fr.count(); c != 1 {
		t.Fatalf("tick 1: want 1 fire, got %d", c)
	}

	// Same minute: last_run_at was stamped, so the dedupe rule suppresses a repeat.
	w.Tick(ctx)
	settle()
	if c := fr.count(); c != 1 {
		t.Fatalf("tick 2 (same minute): want 1, got %d", c)
	}

	// Advance to the next minute: cron matches again, fresh fire.
	clock = clock.Add(time.Minute)
	w.Tick(ctx)
	settle()
	if c := fr.count(); c != 2 {
		t.Fatalf("tick 3 (next minute): want 2, got %d", c)
	}
}

func TestWorker_OneShotFiresOnceThenDisabled(t *testing.T) {
	w, st, fr := newWorkerHarness(t)
	runAt := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	clock := runAt
	w.SetClockForTest(func() time.Time { return clock }, time.Minute)
	ctx := context.Background()
	if err := st.CreateSchedule(ctx, &store.Schedule{
		ID: "once", Name: "o", Instruction: "i", RunAt: &runAt,
		Workdir: t.TempDir(), Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	w.Tick(ctx)
	settle()
	if c := fr.count(); c != 1 {
		t.Fatalf("one-shot should fire once, got %d", c)
	}
	got, _ := st.GetSchedule(ctx, "once")
	if got.Enabled {
		t.Fatal("one-shot must be disabled (enabled=0) after firing")
	}

	// Next minute: enabled=false, so it never fires again.
	clock = clock.Add(time.Minute)
	w.Tick(ctx)
	settle()
	if c := fr.count(); c != 1 {
		t.Fatalf("disabled one-shot must not fire again, got %d", c)
	}
}

func TestWorker_CronMissedMinuteNotBackfilled(t *testing.T) {
	w, st, fr := newWorkerHarness(t)
	clock := time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local) // 10:00, not 9:00
	w.SetClockForTest(func() time.Time { return clock }, time.Minute)
	ctx := context.Background()
	if err := st.CreateSchedule(ctx, &store.Schedule{
		ID: "nine", Name: "n", Instruction: "i", Cron: "0 9 * * *",
		Workdir: t.TempDir(), Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	w.Tick(ctx)
	settle()
	if c := fr.count(); c != 0 {
		t.Fatalf("9am cron must not fire at 10am (no backfill), got %d", c)
	}
}

func TestWorker_DeletedScheduleDoesNotFire(t *testing.T) {
	w, st, fr := newWorkerHarness(t)
	clock := time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local)
	w.SetClockForTest(func() time.Time { return clock }, time.Minute)
	ctx := context.Background()
	if err := st.CreateSchedule(ctx, &store.Schedule{
		ID: "gone", Name: "g", Instruction: "i", Cron: "*/1 * * * *",
		Workdir: t.TempDir(), Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSchedule(ctx, "gone"); err != nil {
		t.Fatal(err)
	}

	w.Tick(ctx)
	settle()
	if c := fr.count(); c != 0 {
		t.Fatalf("deleted schedule must not fire, got %d", c)
	}
}

func TestWorker_StartStopsOnContextCancel(t *testing.T) {
	w, _, _ := newWorkerHarness(t)
	w.SetClockForTest(func() time.Time { return time.Now() }, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Start(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not exit after ctx cancel")
	}
}
