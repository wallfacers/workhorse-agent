// Package scheduletool implements the schedule_create / schedule_list /
// schedule_remove / schedule_read_log built-in tools that expose the in-process
// scheduler (001-agent-orchestration US3) to the LLM. The store is obtained
// via a type assertion on tools.Env.Schedules.
package scheduletool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/schedule"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

func storeFrom(env *tools.Env) (store.Store, bool) {
	st, ok := env.Schedules.(store.Store)
	return st, ok && st != nil
}

func errResult(msg string) *tools.Result {
	return &tools.Result{Output: msg, IsError: true}
}

// ---- schedule_create ----

type CreateTool struct{}

func (CreateTool) Name() string { return "schedule_create" }

func (CreateTool) Description() string {
	return `Create a scheduled automation plan that an unattended background session runs on a cron schedule (repeating) or at a fixed time (one-shot). The plan persists across server restarts.

Set exactly one of 'cron' (5 fields: minute hour day-of-month month day-of-week, local timezone; supports *, lists, ranges, and */N steps) or 'run_at' (RFC 3339 timestamp). 'workdir' defaults to this session's working directory and must already exist — the scheduled session runs there.`
}

func (CreateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Human-readable schedule name"},
    "instruction": {"type": "string", "description": "Full instruction the unattended session will execute on each trigger"},
    "cron": {"type": "string", "description": "5-field cron expression (min hour dom month dow), local timezone. Mutually exclusive with run_at"},
    "run_at": {"type": "string", "description": "RFC 3339 timestamp for a one-shot run. Mutually exclusive with cron"},
    "workdir": {"type": "string", "description": "Working directory for the scheduled session. Defaults to the current session workdir"}
  },
  "required": ["name", "instruction"]
}`)
}

func (CreateTool) IsReadOnly() bool              { return false }
func (CreateTool) CanRunInParallel() bool        { return false }
func (CreateTool) DefaultTimeout() time.Duration { return 0 }

func (CreateTool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	st, ok := storeFrom(env)
	if !ok {
		return errResult("schedule store not configured"), nil
	}
	var in struct {
		Name        string `json:"name"`
		Instruction string `json:"instruction"`
		Cron        string `json:"cron"`
		RunAt       string `json:"run_at"`
		Workdir     string `json:"workdir"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid schedule_create input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.Name) == "" {
		return errResult("name is required"), nil
	}
	if strings.TrimSpace(in.Instruction) == "" {
		return errResult("instruction is required"), nil
	}
	hasCron := strings.TrimSpace(in.Cron) != ""
	hasRunAt := strings.TrimSpace(in.RunAt) != ""
	if hasCron == hasRunAt {
		return errResult("exactly one of 'cron' or 'run_at' must be set"), nil
	}

	workdir := in.Workdir
	if workdir == "" {
		workdir = env.Workdir
	}
	if workdir == "" {
		return errResult("workdir is required (set the 'workdir' parameter)"), nil
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return errResult("invalid workdir: " + err.Error()), nil
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return errResult(fmt.Sprintf("workdir does not exist: %s", abs)), nil
	}
	workdir = abs

	var cronExpr string
	var runAt *time.Time
	nextRun := ""
	if hasCron {
		cronExpr = strings.TrimSpace(in.Cron)
		if err := schedule.Validate(cronExpr); err != nil {
			return errResult("invalid cron expression: " + err.Error()), nil
		}
		if nm, err := schedule.NextMatch(cronExpr, time.Now()); err == nil {
			nextRun = nm.Format("2006-01-02 15:04 (MST)")
		}
	} else {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.RunAt))
		if err != nil {
			return errResult("invalid run_at (use RFC 3339, e.g. 2026-07-21T09:00:00+08:00): " + err.Error()), nil
		}
		tt := t
		runAt = &tt
		nextRun = tt.Format("2006-01-02 15:04 (MST)")
	}

	id, err := uniqueScheduleID(ctx, st, slugify(in.Name))
	if err != nil {
		return errResult(err.Error()), nil
	}
	if err := st.CreateSchedule(ctx, &store.Schedule{
		ID:          id,
		Name:        in.Name,
		Instruction: in.Instruction,
		Cron:        cronExpr,
		RunAt:       runAt,
		Workdir:     workdir,
		Enabled:     true,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		return errResult("create schedule failed: " + err.Error()), nil
	}

	out := fmt.Sprintf("Created schedule %q (id: %s).", in.Name, id)
	if nextRun != "" {
		out += " Next run: " + nextRun + "."
	}
	return &tools.Result{Output: out}, nil
}

// ---- schedule_list ----

type ListTool struct{}

func (ListTool) Name() string { return "schedule_list" }

func (ListTool) Description() string {
	return `List every scheduled automation plan with its trigger rule, enabled state, and last run time.`
}

func (ListTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (ListTool) IsReadOnly() bool              { return true }
func (ListTool) CanRunInParallel() bool        { return true }
func (ListTool) DefaultTimeout() time.Duration { return 0 }

func (ListTool) Run(ctx context.Context, env *tools.Env, _ json.RawMessage) (*tools.Result, error) {
	st, ok := storeFrom(env)
	if !ok {
		return errResult("schedule store not configured"), nil
	}
	list, err := st.ListSchedules(ctx)
	if err != nil {
		return errResult("list schedules failed: " + err.Error()), nil
	}
	if len(list) == 0 {
		return &tools.Result{Output: "No schedules found."}, nil
	}
	var sb strings.Builder
	for _, s := range list {
		state := "enabled"
		if !s.Enabled {
			state = "disabled"
		}
		trigger := s.Cron
		if trigger == "" && s.RunAt != nil {
			trigger = "run_at " + s.RunAt.Format("2006-01-02 15:04")
		}
		last := "never"
		if s.LastRunAt != nil {
			last = s.LastRunAt.Format("2006-01-02 15:04")
		}
		sb.WriteString(fmt.Sprintf("- %s [%s] %q last run: %s — %s\n",
			s.ID, state, trigger, last, firstLine(s.Instruction, 60)))
	}
	return &tools.Result{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

// ---- schedule_remove ----

type RemoveTool struct{}

func (RemoveTool) Name() string { return "schedule_remove" }

func (RemoveTool) Description() string {
	return `Delete a scheduled plan by its id. Deletion takes effect immediately and also removes its run log.`
}

func (RemoveTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {"id": {"type": "string"}},
  "required": ["id"]
}`)
}

func (RemoveTool) IsReadOnly() bool              { return false }
func (RemoveTool) CanRunInParallel() bool        { return false }
func (RemoveTool) DefaultTimeout() time.Duration { return 0 }

func (RemoveTool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	st, ok := storeFrom(env)
	if !ok {
		return errResult("schedule store not configured"), nil
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid schedule_remove input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.ID) == "" {
		return errResult("id is required"), nil
	}
	sch, err := st.GetSchedule(ctx, in.ID)
	if err != nil {
		return errResult(fmt.Sprintf("Schedule %q not found.", in.ID)), nil
	}
	if err := st.DeleteSchedule(ctx, in.ID); err != nil {
		return errResult("delete schedule failed: " + err.Error()), nil
	}
	return &tools.Result{Output: fmt.Sprintf("Removed schedule %q.", sch.Name)}, nil
}

// ---- schedule_read_log ----

type ReadLogTool struct{}

func (ReadLogTool) Name() string { return "schedule_read_log" }

func (ReadLogTool) Description() string {
	return `Read the most recent runs of a scheduled plan: trigger time, status, the unattended session id (replayable via the history API), and the output tail.`
}

func (ReadLogTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string"},
    "limit": {"type": "integer", "description": "Max runs to return, default 5, cap 20"}
  },
  "required": ["id"]
}`)
}

func (ReadLogTool) IsReadOnly() bool              { return true }
func (ReadLogTool) CanRunInParallel() bool        { return true }
func (ReadLogTool) DefaultTimeout() time.Duration { return 0 }

func (ReadLogTool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	st, ok := storeFrom(env)
	if !ok {
		return errResult("schedule store not configured"), nil
	}
	var in struct {
		ID    string `json:"id"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid schedule_read_log input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.ID) == "" {
		return errResult("id is required"), nil
	}
	runs, err := st.ListScheduleRuns(ctx, in.ID, in.Limit)
	if err != nil {
		return errResult("read run log failed: " + err.Error()), nil
	}
	if len(runs) == 0 {
		return &tools.Result{Output: fmt.Sprintf("No runs recorded for schedule %q yet.", in.ID)}, nil
	}
	var sb strings.Builder
	for _, r := range runs {
		sb.WriteString(fmt.Sprintf("- %s [%s] session %s\n",
			r.StartedAt.Format("2006-01-02 15:04"), r.Status, r.SessionID))
		if r.OutputTail != "" {
			sb.WriteString("  " + firstLine(r.OutputTail, 200) + "\n")
		}
		if r.Error != "" {
			sb.WriteString("  error: " + firstLine(r.Error, 200) + "\n")
		}
	}
	return &tools.Result{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

// Tools returns all four schedule tools for registration.
func Tools() []tools.Tool {
	return []tools.Tool{CreateTool{}, ListTool{}, RemoveTool{}, ReadLogTool{}}
}

// ---- helpers ----

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var sb strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			sb.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			sb.WriteRune('-')
		}
	}
	out := strings.Trim(sb.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		out = "schedule"
	}
	return out
}

func uniqueScheduleID(ctx context.Context, st store.Store, base string) (string, error) {
	if _, err := st.GetSchedule(ctx, base); err != nil {
		return base, nil // not found => available
	}
	for i := 2; i <= 50; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if _, err := st.GetSchedule(ctx, cand); err != nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("could not generate a unique schedule id for %q", base)
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
