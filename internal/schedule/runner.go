package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

// runTimeout bounds one unattended schedule execution. Overruns are recorded as
// cancelled errors.
const runTimeout = 10 * time.Minute

// outputTailMaxBytes caps the run log's output_tail (data-model §3, 64 KiB).
const outputTailMaxBytes = 64 * 1024

// scheduledMetaKey tags an unattended session with the schedule that owns it.
const scheduledMetaKey = "scheduled"

// Runner executes one schedule trigger as an unattended persistent session and
// records the outcome in the schedule_runs table.
type Runner struct {
	SessMgr *session.Manager
	Store   store.Store
	Log     *slog.Logger
}

// NewRunner constructs a Runner. Log defaults to slog.Default() when nil.
func NewRunner(st store.Store, mgr *session.Manager, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{Store: st, SessMgr: mgr, Log: log}
}

// RunOnce creates a persistent unattended session for the schedule, drives it
// through one turn, and records the outcome. A missing workdir records an error
// run and leaves the schedule in place. The created session is persistent (not
// deleted) so its transcript stays replayable via the history API (spec R7).
func (r *Runner) RunOnce(ctx context.Context, sched *store.Schedule) {
	if _, err := os.Stat(sched.Workdir); err != nil {
		r.recordRunError(ctx, sched.ID, fmt.Sprintf("workdir not accessible: %v", err))
		r.Log.Warn("schedule: workdir missing, run failed (schedule kept)",
			"schedule", sched.ID, "workdir", sched.Workdir)
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	sess, err := r.SessMgr.CreateSession(runCtx, session.Options{
		Workdir:  sched.Workdir,
		Metadata: map[string]string{scheduledMetaKey: sched.ID},
	})
	if err != nil {
		r.recordRunError(ctx, sched.ID, fmt.Sprintf("create session: %v", err))
		return
	}

	runID, err := r.Store.CreateScheduleRun(ctx, &store.ScheduleRun{
		ScheduleID: sched.ID,
		SessionID:  sess.ID,
		StartedAt:  time.Now().UTC(),
		Status:     store.ScheduleRunRunning,
	})
	if err != nil {
		r.Log.Warn("schedule: create run row", "schedule", sched.ID, "err", err)
		return
	}

	c := newCollector()
	pumpDone := make(chan struct{})
	go pump(runCtx, sess, c, pumpDone)

	payload, _ := json.Marshal(session.UserMessagePayload{Content: sched.Instruction})
	select {
	case sess.Inbox <- session.ClientMessage{Type: session.ClientUserMessage, Payload: payload}:
	case <-runCtx.Done():
		<-pumpDone
		r.finishRun(ctx, runID, c.FinalText(), "cancelled")
		return
	}

	<-pumpDone

	if runCtx.Err() != nil {
		r.finishRun(ctx, runID, c.FinalText(), "cancelled")
		return
	}
	if msg := c.ErrorMessage(); msg != "" {
		r.finishRun(ctx, runID, c.FinalText(), msg)
		return
	}
	r.finishRun(ctx, runID, tailText(c.FinalText(), outputTailMaxBytes), "")
}

// recordRunError opens a running run row and immediately fails it with errMsg,
// used when the session could not be created at all (so there is no session_id).
func (r *Runner) recordRunError(ctx context.Context, scheduleID, errMsg string) {
	runID, err := r.Store.CreateScheduleRun(ctx, &store.ScheduleRun{
		ScheduleID: scheduleID,
		StartedAt:  time.Now().UTC(),
		Status:     store.ScheduleRunRunning,
	})
	if err != nil {
		r.Log.Warn("schedule: create run row", "schedule", scheduleID, "err", err)
		return
	}
	r.finishRun(ctx, runID, "", errMsg)
}

func (r *Runner) finishRun(ctx context.Context, runID int64, outputTail, errMsg string) {
	status := store.ScheduleRunComplete
	if errMsg != "" {
		status = store.ScheduleRunError
	}
	if err := r.Store.FinishScheduleRun(ctx, runID, status, outputTail, errMsg); err != nil {
		r.Log.Warn("schedule: finish run", "run", runID, "err", err)
	}
}
