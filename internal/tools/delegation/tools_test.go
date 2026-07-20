package delegationtool_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/delegation"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	delegationtool "github.com/wallfacers/workhorse-agent/internal/tools/delegation"
)

func newToolsHarness(t *testing.T) (*delegation.Manager, store.Store, *session.Manager) {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mgr := session.NewManager(session.ManagerOptions{Store: s, MaxConcurrent: 50})
	dmgr := delegation.NewManager(s, mgr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return dmgr, s, mgr
}

func mustParent(t *testing.T, mgr *session.Manager) *session.Session {
	t.Helper()
	sess, err := mgr.CreateSession(context.Background(), session.Options{Workdir: t.TempDir(), Ephemeral: true})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	t.Cleanup(func() { _ = mgr.DeleteSession(context.Background(), sess.ID, time.Second) })
	return sess
}

func waitTerminal(t *testing.T, st store.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d, err := st.GetDelegation(context.Background(), id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if d.Status != store.DelegationRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("delegation %s did not terminate", id)
}

func TestDelegateTool_SuccessOutput(t *testing.T) {
	dmgr, st, mgr := newToolsHarness(t)
	parent := mustParent(t, mgr)
	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir, Delegations: dmgr}
	input, _ := json.Marshal(map[string]string{"description": "Research auth", "prompt": "find auth flow"})

	res, err := delegationtool.DelegateTool{}.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	for _, want := range []string{"Delegation started:", "read-only", "delegation_read("} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output missing %q:\n%s", want, res.Output)
		}
	}
	// Extract the id (second token of the first line) and wait for the
	// background goroutine to wind down so the test leaves no leak.
	id := strings.TrimSpace(strings.TrimPrefix(strings.Split(res.Output, "\n")[0], "Delegation started:"))
	if id == "" {
		t.Fatalf("could not extract id from %q", res.Output)
	}
	waitTerminal(t, st, id)
}

func TestDelegateTool_ConcurrencyCapError(t *testing.T) {
	dmgr, st, mgr := newToolsHarness(t)
	parent := mustParent(t, mgr)
	ctx := context.Background()
	for i := 0; i < delegation.MaxConcurrent; i++ {
		if err := st.CreateDelegation(ctx, &store.Delegation{
			ID: "cap-" + string(rune('a'+i)), SessionID: parent.ID,
			Description: "x", Prompt: "y", Workdir: parent.Workdir,
			Status: store.DelegationRunning, StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir, Delegations: dmgr}
	input, _ := json.Marshal(map[string]string{"description": "extra", "prompt": "do it"})

	res, err := delegationtool.DelegateTool{}.Run(ctx, env, input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Output, "Too many running delegations") {
		t.Fatalf("want concurrency cap error, got %+v", res)
	}
}

func TestDelegationReadTool_States(t *testing.T) {
	dmgr, st, mgr := newToolsHarness(t)
	parent := mustParent(t, mgr)
	ctx := context.Background()
	now := time.Now().UTC()

	st.CreateDelegation(ctx, &store.Delegation{ID: "done-id", SessionID: parent.ID, Description: "d", Prompt: "p", Workdir: "/w", Status: store.DelegationRunning, StartedAt: now})
	st.CompleteDelegation(ctx, "done-id", "Title", "summary", "FULL RESULT TEXT")

	st.CreateDelegation(ctx, &store.Delegation{ID: "run-id", SessionID: parent.ID, Description: "d", Prompt: "p", Workdir: "/w", Status: store.DelegationRunning, StartedAt: now})

	st.CreateDelegation(ctx, &store.Delegation{ID: "err-id", SessionID: parent.ID, Description: "d", Prompt: "p", Workdir: "/w", Status: store.DelegationRunning, StartedAt: now})
	st.FailDelegation(ctx, "err-id", "model timeout", "partial detail")

	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir, Delegations: dmgr}

	cases := []struct {
		name    string
		id      string
		want    string
		isError bool
	}{
		{"complete returns full result", "done-id", "FULL RESULT TEXT", false},
		{"running is non-blocking", "run-id", "still running", false},
		{"error returns failure detail", "err-id", "model timeout", false},
		{"unknown id is error", "ghost", "not found", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"id": tc.id})
			res, err := delegationtool.DelegationReadTool{}.Run(ctx, env, input)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.IsError != tc.isError {
				t.Fatalf("isError: got %v want %v (%s)", res.IsError, tc.isError, res.Output)
			}
			if !strings.Contains(res.Output, tc.want) {
				t.Fatalf("output missing %q: %s", tc.want, res.Output)
			}
		})
	}
}

func TestDelegationListTool_EmptyAndItems(t *testing.T) {
	dmgr, st, mgr := newToolsHarness(t)
	parent := mustParent(t, mgr)
	ctx := context.Background()
	env := &tools.Env{SessionID: parent.ID, Workdir: parent.Workdir, Delegations: dmgr}

	emptyRes, err := delegationtool.DelegationListTool{}.Run(ctx, env, nil)
	if err != nil {
		t.Fatalf("Run empty: %v", err)
	}
	if emptyRes.Output != "No delegations found for this session." {
		t.Fatalf("empty output: %q", emptyRes.Output)
	}

	now := time.Now().UTC()
	st.CreateDelegation(ctx, &store.Delegation{ID: "calm-teal-owl", SessionID: parent.ID, Description: "Map SSE protocol", Prompt: "p", Workdir: "/w", Status: store.DelegationRunning, StartedAt: now})
	st.CreateDelegation(ctx, &store.Delegation{ID: "brisk-amber-fox", SessionID: parent.ID, Description: "Research auth flow", Prompt: "p", Workdir: "/w", Status: store.DelegationRunning, StartedAt: now.Add(time.Second)})
	st.CompleteDelegation(ctx, "brisk-amber-fox", "Auth Flow", "tokens via Manager.Check", "result body")

	res, err := delegationtool.DelegationListTool{}.Run(ctx, env, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "brisk-amber-fox [complete] Research auth flow") {
		t.Fatalf("missing complete line: %s", res.Output)
	}
	if !strings.Contains(res.Output, "calm-teal-owl [running] Map SSE protocol") {
		t.Fatalf("missing running line: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Running in the background.") {
		t.Fatalf("missing running hint: %s", res.Output)
	}
}

func TestDelegationTools_NotConfigured(t *testing.T) {
	env := &tools.Env{Delegations: nil}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func() (*tools.Result, error)
	}{
		{"delegate", func() (*tools.Result, error) {
			return delegationtool.DelegateTool{}.Run(ctx, env, json.RawMessage(`{"description":"x","prompt":"y"}`))
		}},
		{"read", func() (*tools.Result, error) {
			return delegationtool.DelegationReadTool{}.Run(ctx, env, json.RawMessage(`{"id":"x"}`))
		}},
		{"list", func() (*tools.Result, error) {
			return delegationtool.DelegationListTool{}.Run(ctx, env, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.run()
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !res.IsError || !strings.Contains(res.Output, "not configured") {
				t.Fatalf("want not-configured error, got %+v", res)
			}
		})
	}
}
