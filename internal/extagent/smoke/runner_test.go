package smoke_test

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/smoke"
)

func buildSmokeFake(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.go")
	bin := filepath.Join(dir, "fake-smoke")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	code := `package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--prompt" && i+1 < len(args) {
			fmt.Println(args[i+1])
			return
		}
	}
	fmt.Println("WORKHORSE_SMOKE_OK")
}
`
	if err := os.WriteFile(src, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func buildFailFake(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fail.go")
	bin := filepath.Join(dir, "fake-fail")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	code := `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("wrong output")
	os.Exit(1)
}
`
	if err := os.WriteFile(src, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func testAdapter(bin string) *extagent.Adapter {
	return &extagent.Adapter{
		Name:           "test-agent",
		Binary:         bin,
		ResolvedBinary: bin,
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
			CancelSignal:      "SIGINT",
			CancelGraceSec:    1,
			DefaultTimeoutSec: 60,
			MaxTimeoutSec:     300,
		},
		Security:    extagent.Security{Network: "none", Filesystem: "full", Trusted: true},
		SmokeTest: extagent.SmokeTest{
			Prompt:            "WORKHORSE_SMOKE_OK",
			ExpectedSubstring: "WORKHORSE_SMOKE_OK",
			TimeoutSec:        30,
		},
		Description: "test",
		Provenance:  extagent.Provenance{Source: "builtin"},
	}
}

func TestRun_Pass(t *testing.T) {
	bin := buildSmokeFake(t)
	a := testAdapter(bin)
	result := smoke.Run(a, slog.Default())
	if !result.Passed {
		t.Errorf("expected pass, got error: %s", result.Error)
	}
}

func TestRun_Fail(t *testing.T) {
	bin := buildFailFake(t)
	a := testAdapter(bin)
	a.SmokeTest.ExpectedSubstring = "WORKHORSE_SMOKE_OK"
	result := smoke.Run(a, slog.Default())
	if result.Passed {
		t.Error("expected fail")
	}
}

func TestRunCached_CacheHit(t *testing.T) {
	bin := buildSmokeFake(t)
	a := testAdapter(bin)
	cacheDir := t.TempDir()

	// First run: should pass and create cache.
	passed := smoke.RunCached(a, cacheDir, 168, slog.Default())
	if !passed {
		t.Fatal("first run should pass")
	}

	// Verify cache file exists.
	cacheFile := filepath.Join(cacheDir, a.Name+".smoke")
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file should exist: %v", err)
	}

	// Second run: should use cache (even if binary is now gone).
	a.ResolvedBinary = "/nonexistent/binary"
	passed = smoke.RunCached(a, cacheDir, 168, slog.Default())
	if !passed {
		t.Error("second run should use cache and pass")
	}
}

func TestRunCached_MalformedCache_Reruns(t *testing.T) {
	bin := buildSmokeFake(t)
	a := testAdapter(bin)
	cacheDir := t.TempDir()

	// Write malformed cache.
	cacheFile := filepath.Join(cacheDir, a.Name+".smoke")
	if err := os.WriteFile(cacheFile, []byte("bad json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Should re-run and pass.
	passed := smoke.RunCached(a, cacheDir, 168, slog.Default())
	if !passed {
		t.Error("should re-run on malformed cache and pass")
	}
}
