package e2e

import (
	"context"
	"encoding/json"
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
	"github.com/wallfacers/workhorse-agent/internal/tools"
	extagenttool "github.com/wallfacers/workhorse-agent/internal/tools/extagent"
)

func buildFakeAgent(t *testing.T, behavior string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.go")
	bin := filepath.Join(dir, "fake-ext-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	var code string
	switch behavior {
	case "echo":
		code = `package main

import (
	"fmt"
	"os"
)

func main() {
	for i, a := range os.Args[1:] {
		if a == "--prompt" && i+1 < len(os.Args) {
			fmt.Println("ECHO:", os.Args[i+2])
			return
		}
	}
	fmt.Println("no prompt arg")
}
`
	case "huge":
		code = `package main

import (
	"fmt"
	"os"
)

func main() {
	for i, a := range os.Args[1:] {
		if a == "--prompt" && i+1 < len(os.Args) {
			// Generate >4MiB of output.
			chunk := strings.Repeat("x", 4096)
			for i := 0; i < 1100; i++ {
				os.Stdout.WriteString(chunk)
				os.Stdout.WriteString("\n")
			}
			return
		}
	}
}
`
		code = `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	for i, a := range os.Args[1:] {
		if a == "--prompt" && i+1 < len(os.Args) {
			chunk := strings.Repeat("x", 4096)
			for i := 0; i < 1100; i++ {
				os.Stdout.WriteString(chunk)
				os.Stdout.WriteString("\n")
			}
			return
		}
	}
	fmt.Println("no prompt arg")
}
`
	case "slow":
		code = `package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	time.Sleep(10 * time.Second)
	for i, a := range os.Args[1:] {
		if a == "--prompt" && i+1 < len(os.Args) {
			fmt.Println("SLOW:", os.Args[i+2])
			return
		}
	}
}
`
	case "fail":
		code = `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "intentional failure")
	os.Exit(1)
}
`
	case "stderr":
		code = `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintf(os.Stderr, "stderr output\n")
	fmt.Println("stdout output")
}
`
	default:
		t.Fatalf("unknown behavior: %s", behavior)
	}

	if err := os.WriteFile(src, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", behavior, err, out)
	}
	return bin
}

func fakeExtAgent(bin string) *extagent.Adapter {
	return &extagent.Adapter{
		Name:           "test-fake",
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
			DefaultTimeoutSec: 30,
			MaxTimeoutSec:     60,
		},
		Security:    extagent.Security{Trusted: true},
		SmokePassed: true,
		Description: "test fake agent",
	}
}

// 13.1: Basic invocation through the ExternalAgent tool.
func TestExtAgent_BasicInvocation(t *testing.T) {
	bin := buildFakeAgent(t, "echo")
	a := fakeExtAgent(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &extagenttool.Host{
		Registry:        reg,
		Driver:          &driver.Driver{Logger: slog.Default()},
		OutputCapBytes:  1 << 20,
		KillOnOutputCap: true,
	}
	tool := extagenttool.New(host)
	if tool == nil {
		t.Fatal("tool should not be nil")
	}

	input, _ := json.Marshal(map[string]any{
		"agent_name": "test-fake",
		"prompt":     "hello e2e",
	})
	env := &tools.Env{
		SessionID:        "e2e-session",
		Workdir:          t.TempDir(),
		ExtAgentRegistry: reg,
	}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected error: %s", result.Output)
	}
	if !strings.Contains(result.Output, "ECHO: hello e2e") {
		t.Errorf("output: got %q", result.Output)
	}
}

// 13.3: Output truncation with large output.
func TestExtAgent_OutputTruncation(t *testing.T) {
	bin := buildFakeAgent(t, "huge")
	a := fakeExtAgent(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &extagenttool.Host{
		Registry:        reg,
		Driver:          &driver.Driver{Logger: slog.Default()},
		OutputCapBytes:  1 << 20, // 1 MiB cap
		KillOnOutputCap: true,
	}
	tool := extagenttool.New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name": "test-fake",
		"prompt":     "generate huge",
	})
	env := &tools.Env{
		SessionID:        "e2e-trunc",
		Workdir:          t.TempDir(),
		ExtAgentRegistry: reg,
	}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(result.Output, "truncated") {
		t.Errorf("should have truncation marker, output length: %d", len(result.Output))
	}
}

// 13.4: Cancellation mid-invocation.
func TestExtAgent_Cancellation(t *testing.T) {
	bin := buildFakeAgent(t, "slow")
	a := fakeExtAgent(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &extagenttool.Host{
		Registry:        reg,
		Driver:          &driver.Driver{Logger: slog.Default()},
		OutputCapBytes:  1 << 20,
		KillOnOutputCap: true,
	}
	tool := extagenttool.New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name": "test-fake",
		"prompt":     "wait",
	})
	env := &tools.Env{
		SessionID:        "e2e-cancel",
		Workdir:          t.TempDir(),
		ExtAgentRegistry: reg,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	result, err := tool.Run(ctx, env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(result.Output, "[CANCELLED]") {
		t.Errorf("should have [CANCELLED] prefix, got: %q", result.Output)
	}
}

// 13.5: Timeout via per-call timeout_sec.
func TestExtAgent_Timeout(t *testing.T) {
	bin := buildFakeAgent(t, "slow")
	a := fakeExtAgent(bin)
	a.Control.DefaultTimeoutSec = 1
	a.Control.CancelGraceSec = 1
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &extagenttool.Host{
		Registry:        reg,
		Driver:          &driver.Driver{Logger: slog.Default()},
		OutputCapBytes:  1 << 20,
		KillOnOutputCap: true,
	}
	tool := extagenttool.New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name":  "test-fake",
		"prompt":      "wait",
		"timeout_sec": 1,
	})
	env := &tools.Env{
		SessionID:        "e2e-timeout",
		Workdir:          t.TempDir(),
		ExtAgentRegistry: reg,
	}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(result.Output, "[TIMEOUT]") {
		t.Errorf("should have [TIMEOUT] prefix, got: %q", result.Output)
	}
}

// 13.2 variant: Per-session registry isolation.
func TestExtAgent_PerSessionRegistry(t *testing.T) {
	bin := buildFakeAgent(t, "echo")
	a := fakeExtAgent(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &extagenttool.Host{
		Registry:        reg,
		Driver:          &driver.Driver{Logger: slog.Default()},
		OutputCapBytes:  1 << 20,
		KillOnOutputCap: true,
	}
	tool := extagenttool.New(host)

	// Session A: valid agent.
	inputA, _ := json.Marshal(map[string]any{
		"agent_name": "test-fake",
		"prompt":     "session A",
	})
	envA := &tools.Env{
		SessionID:        "session-A",
		Workdir:          t.TempDir(),
		ExtAgentRegistry: reg,
	}
	resultA, err := tool.Run(context.Background(), envA, inputA)
	if err != nil {
		t.Fatalf("run A: %v", err)
	}
	if resultA.IsError {
		t.Errorf("session A error: %s", resultA.Output)
	}

	// Session B: different registry (no adapters) — should fail.
	emptyReg := extagent.NewRegistry(extagent.NewSnapshot(nil))
	inputB, _ := json.Marshal(map[string]any{
		"agent_name": "test-fake",
		"prompt":     "session B",
	})
	envB := &tools.Env{
		SessionID:        "session-B",
		Workdir:          t.TempDir(),
		ExtAgentRegistry: emptyReg,
	}
	resultB, err := tool.Run(context.Background(), envB, inputB)
	if err != nil {
		t.Fatalf("run B: %v", err)
	}
	if !resultB.IsError {
		t.Error("session B should error — empty registry")
	}
}

// Non-zero exit code: tool returns output with stderr and raw dump.
func TestExtAgent_NonZeroExit(t *testing.T) {
	bin := buildFakeAgent(t, "fail")
	a := fakeExtAgent(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &extagenttool.Host{
		Registry:        reg,
		Driver:          &driver.Driver{Logger: slog.Default()},
		OutputCapBytes:  1 << 20,
		KillOnOutputCap: true,
	}
	tool := extagenttool.New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name": "test-fake",
		"prompt":     "fail",
	})
	env := &tools.Env{
		SessionID:        "e2e-fail",
		Workdir:          t.TempDir(),
		ExtAgentRegistry: reg,
	}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Non-zero exit is not IsError at the tool level — the driver returns
	// the output with stderr included and a raw dump path.
	if !strings.Contains(result.Output, "stderr") {
		t.Errorf("should contain stderr block, got: %q", result.Output)
	}
}

// InternalGated bypass in checkPermissions.
func TestExtAgent_InternalGatedBypass(t *testing.T) {
	bin := buildFakeAgent(t, "echo")
	a := fakeExtAgent(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &extagenttool.Host{
		Registry:        reg,
		Driver:          &driver.Driver{Logger: slog.Default()},
		OutputCapBytes:  1 << 20,
		KillOnOutputCap: true,
	}
	tool := extagenttool.New(host)

	// Verify the interface is satisfied.
	_ = interface{ IsInternalGated() bool }(tool)
	if !tool.IsInternalGated() {
		t.Error("should be internal gated")
	}
}
