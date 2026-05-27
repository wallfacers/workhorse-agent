package driver_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/driver"
)

// fakeAdapter creates a test adapter that points at a compiled fake binary.
func fakeAdapter(t *testing.T, binPath string) *extagent.Adapter {
	return &extagent.Adapter{
		Name:           "fake-agent",
		Binary:         binPath,
		ResolvedBinary: binPath,
		Class:          extagent.ClassSubAgent,
		Invocation: extagent.Invocation{
			PromptVia: "arg",
			PromptArg: "--prompt",
		},
		Output: extagent.Output{
			Format: "text",
			Stderr: "separate",
		},
		Control: extagent.Control{
			CancelSignal:     "SIGINT",
			CancelGraceSec:   1,
			DefaultTimeoutSec: 60,
			MaxTimeoutSec:    300,
		},
		Security:    extagent.Security{Network: "none", Filesystem: "full", Trusted: true},
		SmokeTest:   extagent.SmokeTest{Prompt: "test", ExpectedSubstring: "OK"},
		Description: "test adapter",
		Provenance:  extagent.Provenance{Source: "builtin"},
		SmokePassed: true,
	}
}

// buildFakeBinary compiles a small Go program that echoes input.
func buildFakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.go")
	bin := filepath.Join(dir, "fake-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	code := `package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--prompt":
			if i+1 < len(args) {
				fmt.Println("ECHO:", args[i+1])
				return
			}
		case "--delay":
			time.Sleep(10 * time.Second)
			fmt.Println("delayed")
			return
		case "--env-dump":
			for _, e := range os.Environ() {
				fmt.Println(e)
			}
			return
		case "--stderr-output":
			fmt.Fprintf(os.Stderr, "error message\n")
			fmt.Println("stdout message")
			return
		case "--fail":
			fmt.Fprintln(os.Stderr, "failure")
			os.Exit(1)
			return
		}
	}
	fmt.Println("no matching args")
}
`
	if err := os.WriteFile(src, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake binary: %v\n%s", err, out)
	}
	return bin
}

func TestDriver_BasicInvocation(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)
	d := &driver.Driver{Logger: slog.Default()}

	result, err := d.Run(context.Background(), a, "hello world", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code: got %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "ECHO: hello world") {
		t.Errorf("stdout: got %q", result.Stdout)
	}
}

func TestDriver_EnvFilterBlocksLDPreload(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)
	a.Invocation.EnvOverride = map[string]string{
		"LD_PRELOAD": "/tmp/evil.so",
		"MY_VAR":     "safe_value",
	}
	a.Invocation.PromptArg = "--env-dump"
	a.Invocation.EnvPassthrough = []string{"PATH", "HOME"}

	d := &driver.Driver{Logger: slog.Default()}
	result, err := d.Run(context.Background(), a, "dump", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(result.Stdout, "LD_PRELOAD") {
		t.Error("LD_PRELOAD should be filtered out")
	}
	if !strings.Contains(result.Stdout, "MY_VAR=safe_value") {
		t.Error("MY_VAR should pass through")
	}
}

func TestDriver_EnvFilterBlocksNODEOptions(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)
	a.Invocation.EnvOverride = map[string]string{
		"NODE_OPTIONS": "--require /tmp/x.js",
	}
	a.Invocation.PromptArg = "--env-dump"
	a.Invocation.EnvPassthrough = []string{"PATH", "HOME"}

	d := &driver.Driver{Logger: slog.Default()}
	result, err := d.Run(context.Background(), a, "dump", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(result.Stdout, "NODE_OPTIONS") {
		t.Error("dangerous NODE_OPTIONS should be filtered")
	}
}

func TestDriver_Timeout(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)
	a.Invocation.PromptArg = "--delay"
	a.Control.DefaultTimeoutSec = 1
	a.Control.CancelGraceSec = 1

	d := &driver.Driver{Logger: slog.Default()}
	result, err := d.Run(context.Background(), a, "wait", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.TimedOut {
		t.Error("should be timed out")
	}
	if !strings.Contains(result.Stdout, "[TIMEOUT]") {
		t.Errorf("stdout should have [TIMEOUT] prefix, got: %q", result.Stdout)
	}
}

func TestDriver_NonZeroExit(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)
	a.Invocation.PromptArg = "--fail"

	d := &driver.Driver{Logger: slog.Default()}
	result, err := d.Run(context.Background(), a, "fail", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode == 0 {
		t.Error("should have non-zero exit")
	}
	if result.RawDumpPath == "" {
		t.Error("should have raw dump on non-zero exit")
	}
}

func TestDriver_StderrSeparate(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)
	a.Invocation.PromptArg = "--stderr-output"
	a.Output.Stderr = "separate"

	d := &driver.Driver{Logger: slog.Default()}
	result, err := d.Run(context.Background(), a, "test", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(result.Stdout, "<stderr>") {
		t.Errorf("should have <stderr> block, got: %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "error message") {
		t.Errorf("should have stderr content, got: %q", result.Stdout)
	}
}

func TestDriver_StderrIgnore(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)
	a.Invocation.PromptArg = "--stderr-output"
	a.Output.Stderr = "ignore"

	d := &driver.Driver{Logger: slog.Default()}
	result, err := d.Run(context.Background(), a, "test", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(result.Stdout, "<stderr>") {
		t.Error("should not have <stderr> block when stderr=ignore")
	}
}

func TestDriver_Cancel(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)
	a.Invocation.PromptArg = "--delay"
	a.Control.CancelGraceSec = 1

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	d := &driver.Driver{Logger: slog.Default()}
	result, err := d.Run(ctx, a, "wait", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.Cancelled {
		t.Error("should be cancelled")
	}
	if !strings.Contains(result.Stdout, "[CANCELLED]") {
		t.Errorf("should have [CANCELLED] prefix, got: %q", result.Stdout)
	}
}

func TestDriver_HappyPathNoRawDump(t *testing.T) {
	bin := buildFakeBinary(t)
	a := fakeAdapter(t, bin)

	d := &driver.Driver{Logger: slog.Default()}
	result, err := d.Run(context.Background(), a, "test", driver.Opts{
		SessionID: "s1",
		CallID:    "c1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.RawDumpPath != "" {
		t.Errorf("happy path should not produce raw dump, got: %q", result.RawDumpPath)
	}
	if result.Truncated {
		t.Error("happy path should not be truncated")
	}
}
