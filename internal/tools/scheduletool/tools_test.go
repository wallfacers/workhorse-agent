package scheduletool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	scheduletool "github.com/wallfacers/workhorse-agent/internal/tools/scheduletool"
)

func newScheduleToolsStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func runCreate(t *testing.T, st store.Store, workdir, body string) (*tools.Result, error) {
	t.Helper()
	env := &tools.Env{Workdir: workdir, Schedules: st}
	return scheduletool.CreateTool{}.Run(context.Background(), env, json.RawMessage(body))
}

func TestCreate_CronValidatesAndPersists(t *testing.T) {
	st := newScheduleToolsStore(t)
	dir := t.TempDir()
	body := `{"name":"Dep Audit","instruction":"audit dependencies","cron":"0 9 * * 1-5","workdir":"` + dir + `"}`
	res, err := runCreate(t, st, dir, body)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, `Created schedule "Dep Audit"`) || !strings.Contains(res.Output, "id: dep-audit") {
		t.Fatalf("output: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Next run") {
		t.Fatalf("missing next-run hint: %s", res.Output)
	}
	got, err := st.GetSchedule(context.Background(), "dep-audit")
	if err != nil {
		t.Fatalf("persisted schedule not found: %v", err)
	}
	if got.Cron != "0 9 * * 1-5" || got.RunAt != nil || !got.Enabled {
		t.Fatalf("persisted fields: %+v", got)
	}
}

func TestCreate_RunAtOneShot(t *testing.T) {
	st := newScheduleToolsStore(t)
	dir := t.TempDir()
	body := `{"name":"Once","instruction":"i","run_at":"2026-07-21T09:00:00+00:00","workdir":"` + dir + `"}`
	res, _ := runCreate(t, st, dir, body)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	got, _ := st.GetSchedule(context.Background(), "once")
	if got == nil || got.RunAt == nil || got.Cron != "" {
		t.Fatalf("one-shot persisted: %+v", got)
	}
}

func TestCreate_ValidationErrors(t *testing.T) {
	st := newScheduleToolsStore(t)
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing name", `{"instruction":"i","cron":"0 9 * * *","workdir":"` + dir + `"}`, "name is required"},
		{"missing instruction", `{"name":"x","cron":"0 9 * * *","workdir":"` + dir + `"}`, "instruction is required"},
		{"both cron and run_at", `{"name":"x","instruction":"i","cron":"0 9 * * *","run_at":"2026-07-21T09:00:00+00:00","workdir":"` + dir + `"}`, "exactly one"},
		{"neither cron nor run_at", `{"name":"x","instruction":"i","workdir":"` + dir + `"}`, "exactly one"},
		{"invalid cron", `{"name":"x","instruction":"i","cron":"not a cron","workdir":"` + dir + `"}`, "invalid cron"},
		{"invalid run_at", `{"name":"x","instruction":"i","run_at":"tomorrow","workdir":"` + dir + `"}`, "invalid run_at"},
		{"missing workdir", `{"name":"x","instruction":"i","cron":"0 9 * * *","workdir":"/nonexistent-schedule-xyz-123"}`, "workdir does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := runCreate(t, st, dir, tc.body)
			if !res.IsError || !strings.Contains(res.Output, tc.want) {
				t.Fatalf("want %q in error, got %+v", tc.want, res)
			}
		})
	}
}

func TestListAndRemove(t *testing.T) {
	ctx := context.Background()
	st := newScheduleToolsStore(t)
	dir := t.TempDir()
	env := &tools.Env{Workdir: dir, Schedules: st}

	emptyRes, _ := scheduletool.ListTool{}.Run(ctx, env, nil)
	if emptyRes.Output != "No schedules found." {
		t.Fatalf("empty list: %q", emptyRes.Output)
	}

	runCreate(t, st, dir, `{"name":"Daily Audit","instruction":"audit","cron":"0 9 * * *","workdir":"`+dir+`"}`)
	listRes, _ := scheduletool.ListTool{}.Run(ctx, env, nil)
	if !strings.Contains(listRes.Output, "daily-audit [enabled]") || !strings.Contains(listRes.Output, `"0 9 * * *"`) {
		t.Fatalf("list output: %s", listRes.Output)
	}

	// slug conflict => daily-audit-2
	runCreate(t, st, dir, `{"name":"Daily Audit","instruction":"audit","cron":"0 10 * * *","workdir":"`+dir+`"}`)
	conflict, _ := st.GetSchedule(ctx, "daily-audit-2")
	if conflict == nil {
		t.Fatal("slug conflict should produce daily-audit-2")
	}

	rmBody, _ := json.Marshal(map[string]string{"id": "daily-audit"})
	rmRes, _ := scheduletool.RemoveTool{}.Run(ctx, env, rmBody)
	if rmRes.IsError || !strings.Contains(rmRes.Output, "Removed schedule") {
		t.Fatalf("remove: %+v", rmRes)
	}
	if _, err := st.GetSchedule(ctx, "daily-audit"); err == nil {
		t.Fatal("removed schedule should be gone")
	}

	// remove unknown
	rmRes2, _ := scheduletool.RemoveTool{}.Run(ctx, env, json.RawMessage(`{"id":"ghost"}`))
	if !rmRes2.IsError || !strings.Contains(rmRes2.Output, "not found") {
		t.Fatalf("remove ghost: %+v", rmRes2)
	}
}

func TestReadLog(t *testing.T) {
	ctx := context.Background()
	st := newScheduleToolsStore(t)
	dir := t.TempDir()
	env := &tools.Env{Workdir: dir, Schedules: st}
	st.CreateSchedule(ctx, &store.Schedule{ID: "smoke", Name: "smoke", Instruction: "i", Cron: "* * * * *", Workdir: dir, Enabled: true, CreatedAt: time.Now().UTC()})

	// no runs yet
	noneBody, _ := json.Marshal(map[string]string{"id": "smoke"})
	none, _ := scheduletool.ReadLogTool{}.Run(ctx, env, noneBody)
	if !strings.Contains(none.Output, "No runs recorded") {
		t.Fatalf("empty log: %q", none.Output)
	}

	// seed two runs
	id1, _ := st.CreateScheduleRun(ctx, &store.ScheduleRun{ScheduleID: "smoke", SessionID: "sess-a", StartedAt: time.Now().UTC(), Status: store.ScheduleRunComplete, OutputTail: "all good"})
	st.FinishScheduleRun(ctx, id1, store.ScheduleRunComplete, "all good", "")
	st.CreateScheduleRun(ctx, &store.ScheduleRun{ScheduleID: "smoke", SessionID: "sess-b", StartedAt: time.Now().UTC(), Status: store.ScheduleRunError, OutputTail: "halfway", Error: "boom"})

	log, _ := scheduletool.ReadLogTool{}.Run(ctx, env, noneBody)
	if !strings.Contains(log.Output, "sess-") || !strings.Contains(log.Output, "boom") {
		t.Fatalf("log output: %s", log.Output)
	}
}
