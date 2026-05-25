package bash_test

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/bash"
)

func newBashEnv(t *testing.T) *tools.Env {
	t.Helper()
	wd := t.TempDir()
	return &tools.Env{
		SessionID: "test",
		Workdir:   wd,
		Env:       map[string]string{"PATH": os.Getenv("PATH")},
	}
}

func runBash(t *testing.T, b bash.Bash, env *tools.Env, in bash.BashInput, ctx context.Context) *tools.Result {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	raw, _ := json.Marshal(in)
	res, err := b.Run(ctx, env, raw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestBash_Happy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash test is Unix-only")
	}
	b := bash.Bash{}
	res := runBash(t, b, newBashEnv(t), bash.BashInput{Command: "echo hello"}, nil)
	if res.IsError {
		t.Errorf("unexpected error: %q", res.Output)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("expected hello in output, got %q", res.Output)
	}
}

func TestBash_NonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	b := bash.Bash{}
	res := runBash(t, b, newBashEnv(t), bash.BashInput{Command: "false"}, nil)
	if !res.IsError {
		t.Error("expected IsError for non-zero exit")
	}
	if !strings.Contains(res.Output, "[exit 1]") {
		t.Errorf("expected [exit 1], got %q", res.Output)
	}
}

func TestBash_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	b := bash.Bash{}
	start := time.Now()
	res := runBash(t, b, newBashEnv(t),
		bash.BashInput{Command: "sleep 5", TimeoutSeconds: 1}, nil)
	elapsed := time.Since(start)
	if !res.IsError || !strings.Contains(res.Output, "timed out") {
		t.Errorf("expected timeout error, got %+v", res)
	}
	// awaitWithKill blocks up to ~1.5s after timeout for SIGKILL escalation;
	// 4s total is comfortable headroom.
	if elapsed > 4*time.Second {
		t.Errorf("took too long: %v", elapsed)
	}
}

// Spec scenario: cancel grandchildren via process group teardown.
func TestBash_CancelKillsGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Setpgid is Unix-only")
	}
	b := bash.Bash{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan *tools.Result, 1)
	go func() {
		done <- runBash(t, b, newBashEnv(t),
			bash.BashInput{Command: "bash -c 'sleep 60 & sleep 60 & wait'"}, ctx)
	}()
	// Let the children start.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		if !strings.Contains(res.Output, "canceled") {
			t.Errorf("expected canceled marker, got %q", res.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bash did not return after cancel within 5s — grandchildren may not have been killed")
	}
}

// Spec requirement: env filter — process started with LD_PRELOAD in the
// parent env must NOT see it.
func TestBash_FiltersLDPreload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	b := bash.Bash{
		BaseEnv: []string{
			"PATH=" + os.Getenv("PATH"),
			"LD_PRELOAD=/tmp/evil.so",
		},
	}
	res := runBash(t, b, newBashEnv(t),
		bash.BashInput{Command: "env | grep -c LD_PRELOAD || true"}, nil)
	if strings.Contains(res.Output, "1\n") {
		t.Errorf("LD_PRELOAD leaked into child env: %q", res.Output)
	}
}

// Spec requirement: output ring buffer keeps recent tail; old output drops.
func TestBash_RingBufferDropsHead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	b := bash.Bash{MaxOutputBytes: 200}
	// Produce 1000 lines; only the last few should remain.
	res := runBash(t, b, newBashEnv(t),
		bash.BashInput{Command: "for i in $(seq 1 1000); do echo line$i; done"}, nil)
	if len(res.Output) > 250 { // slack for trailing newline / no [exit]
		t.Errorf("output not capped, len=%d", len(res.Output))
	}
	if !strings.Contains(res.Output, "line1000") {
		t.Errorf("ring buffer should keep tail, got %q", res.Output)
	}
}
