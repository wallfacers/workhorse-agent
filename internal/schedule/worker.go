package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
)

// RunnerExec executes one schedule trigger. *Runner implements it; tests inject
// a fake to assert fire decisions without driving a real session.
type RunnerExec interface {
	RunOnce(ctx context.Context, sched *store.Schedule)
}

// Worker is the minute-aligned scheduler loop. Every tick it scans enabled
// schedules, evaluates which are due, and fires each in its own goroutine.
// Unlike the curation worker it has no leader lease: a single serve process is
// the only scheduler instance (port-bound singleton).
type Worker struct {
	Store  store.Store
	Runner RunnerExec
	Log    *slog.Logger

	nowFn        func() time.Time
	tickInterval time.Duration
}

// NewWorker constructs a Worker. Log defaults to slog.Default() when nil.
func NewWorker(st store.Store, runner RunnerExec, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		Store:        st,
		Runner:       runner,
		Log:          log,
		nowFn:        func() time.Time { return time.Now() },
		tickInterval: time.Minute,
	}
}

// SetClockForTest overrides the clock and tick interval (test-only).
func (w *Worker) SetClockForTest(now func() time.Time, tick time.Duration) {
	if now != nil {
		w.nowFn = now
	}
	if tick > 0 {
		w.tickInterval = tick
	}
}

// Start runs the scheduler loop until ctx is cancelled. It is a no-op when no
// Runner is wired. The caller owns ctx cancellation (serve cancels it on
// shutdown so the loop exits — see T019).
func (w *Worker) Start(ctx context.Context) {
	if w.Runner == nil {
		w.Log.Debug("schedule worker inert (no runner)")
		return
	}
	ticker := time.NewTicker(w.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs one scheduler scan. Exposed so tests drive deterministically
// without the real ticker.
func (w *Worker) Tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.Log.Error("schedule: worker tick panic", "recover", fmt.Sprintf("%v", r))
		}
	}()
	now := w.nowFn()
	schedules, err := w.Store.ListSchedules(ctx)
	if err != nil {
		w.Log.Warn("schedule: list failed", "err", err)
		return
	}
	for _, sched := range schedules {
		if !w.shouldFire(sched, now) {
			continue
		}
		// Touch BEFORE firing: stamps last_run_at (same-minute de-dupe) and
		// disables a one-shot, so a concurrent tick cannot double-fire and a
		// one-shot never fires twice.
		if err := w.Store.TouchScheduleRun(ctx, sched.ID, now); err != nil {
			w.Log.Warn("schedule: touch failed", "schedule", sched.ID, "err", err)
			continue
		}
		go w.Runner.RunOnce(ctx, sched)
	}
}

// shouldFire applies the data-model trigger rules: enabled; not already run in
// this minute; cron matches the current minute, or a one-shot's run_at minute
// has arrived and it has never run. Missed minutes are not backfilled.
func (w *Worker) shouldFire(sched *store.Schedule, now time.Time) bool {
	if !sched.Enabled {
		return false
	}
	if sched.LastRunAt != nil && sameMinute(*sched.LastRunAt, now) {
		return false
	}
	if sched.Cron != "" {
		return Matches(sched.Cron, now)
	}
	if sched.RunAt != nil {
		runAtMin := sched.RunAt.Truncate(time.Minute)
		nowMin := now.Truncate(time.Minute)
		return !runAtMin.After(nowMin) && sched.LastRunAt == nil
	}
	return false
}

func sameMinute(a, b time.Time) bool {
	return a.Truncate(time.Minute).Equal(b.Truncate(time.Minute))
}
