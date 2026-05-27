package extagent

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
)

func buildEchoBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "echo.go")
	bin := filepath.Join(dir, "echo-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	code := `package main

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
	if err := os.WriteFile(src, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func healthyAdapter(bin string) *extagent.Adapter {
	return &extagent.Adapter{
		Name:           "echo-agent",
		Binary:         bin,
		ResolvedBinary: bin,
		Class:          extagent.ClassSubAgent,
		Invocation: extagent.Invocation{
			PromptVia: "arg",
			PromptArg: "--prompt",
		},
		Output: extagent.Output{
			Format: "text",
			Stderr: "ignore",
		},
		Control: extagent.Control{
			DefaultTimeoutSec: 30,
			MaxTimeoutSec:     60,
		},
		Security:    extagent.Security{Trusted: true},
		SmokePassed: true,
		Description: "echoes the prompt",
	}
}

func TestNew_ReturnsNilWhenNoHealthy(t *testing.T) {
	reg := &extagent.Registry{}
	host := &Host{Registry: reg}
	tool := New(host)
	if tool != nil {
		t.Error("expected nil when no healthy adapters")
	}
}

func TestNew_ReturnsToolWhenHealthy(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)
	if tool == nil {
		t.Fatal("expected non-nil tool")
	}
	if tool.Name() != "ExternalAgent" {
		t.Errorf("Name: got %q", tool.Name())
	}
	if tool.IsReadOnly() {
		t.Error("should not be read-only")
	}
	if !tool.CanRunInParallel() {
		t.Error("should support parallel")
	}
	if !tool.IsInternalGated() {
		t.Error("should be internal gated")
	}
}

func TestTool_InputSchemaHasEnum(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	props := schema["properties"].(map[string]any)
	an := props["agent_name"].(map[string]any)
	enum := an["enum"].([]any)
	if len(enum) != 1 || enum[0] != "echo-agent" {
		t.Errorf("enum: got %v", enum)
	}
}

func TestTool_DescriptionListsAgents(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	desc := tool.Description()
	if !strings.Contains(desc, "echo-agent") {
		t.Errorf("description should mention agent, got: %q", desc)
	}
}

func TestTool_DefaultTimeout(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	a.Control.MaxTimeoutSec = 120
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	expected := 120*time.Second + 30*time.Second
	if tool.DefaultTimeout() != expected {
		t.Errorf("timeout: got %v, want %v", tool.DefaultTimeout(), expected)
	}
}

func TestTool_Run_Success(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name": "echo-agent",
		"prompt":     "hello world",
	})
	env := &tools.Env{SessionID: "s1", Logger: slog.Default()}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected error: %s", result.Output)
	}
	if !strings.Contains(result.Output, "ECHO: hello world") {
		t.Errorf("output: got %q", result.Output)
	}
}

func TestTool_Run_UnknownAgent(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name": "nonexistent",
		"prompt":     "test",
	})
	env := &tools.Env{SessionID: "s1", Logger: slog.Default()}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.IsError {
		t.Error("should be error for unknown agent")
	}
}

func TestTool_Run_UnhealthyAgent(t *testing.T) {
	a := healthyAdapter("/nonexistent/binary")
	a.BinaryMissing = true
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)
	if tool != nil {
		// New returns nil when no healthy adapters.
		// But if we force-register unhealthy, test Run directly.
	}
	// Force-create tool with unhealthy adapter in registry.
	tool2 := &Tool{Host: host}
	tool2.cachedSchema = tool2.buildSchema(reg.All())
	tool2.cachedDesc = tool2.buildDescription(reg.All())
	tool2.cachedTimeout = tool2.computeTimeout(reg.All())

	input, _ := json.Marshal(map[string]any{
		"agent_name": "echo-agent",
		"prompt":     "test",
	})
	env := &tools.Env{SessionID: "s1", Logger: slog.Default()}
	result, err := tool2.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.IsError {
		t.Error("should be error for unhealthy agent")
	}
	if !strings.Contains(result.Output, "not available") {
		t.Errorf("output: got %q", result.Output)
	}
}

func TestTool_Run_InvalidInput(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	env := &tools.Env{SessionID: "s1", Logger: slog.Default()}
	result, err := tool.Run(context.Background(), env, json.RawMessage(`{bad json`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.IsError {
		t.Error("should be error for invalid input")
	}
}

func TestTool_Run_ResumeNotSupported(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	host := &Host{Registry: reg, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name":        "echo-agent",
		"prompt":            "test",
		"resume_session_id": "old-session",
	})
	env := &tools.Env{SessionID: "s1", Logger: slog.Default()}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.IsError {
		t.Error("should error on resume when adapter doesn't support it")
	}
}

type mockPermissionGate struct {
	approved bool
	err      error
	called   bool
}

func (m *mockPermissionGate) Prompt(ctx context.Context, sessionID, toolName, adapterName string) (bool, error) {
	m.called = true
	return m.approved, m.err
}

func TestTool_Run_UntrustedPermissionDenied(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	a.Security.Trusted = false
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	gate := &mockPermissionGate{approved: false}
	host := &Host{Registry: reg, PermissionGate: gate, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name": "echo-agent",
		"prompt":     "test",
	})
	env := &tools.Env{SessionID: "s1", Logger: slog.Default()}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.IsError {
		t.Error("should be error when permission denied")
	}
	if !gate.called {
		t.Error("permission gate should have been called")
	}
}

func TestTool_Run_UntrustedPermissionApproved(t *testing.T) {
	bin := buildEchoBinary(t)
	a := healthyAdapter(bin)
	a.Security.Trusted = false
	reg := extagent.NewRegistry(extagent.NewSnapshot([]*extagent.Adapter{a}))
	gate := &mockPermissionGate{approved: true}
	host := &Host{Registry: reg, PermissionGate: gate, Driver: &driver.Driver{Logger: slog.Default()}}
	tool := New(host)

	input, _ := json.Marshal(map[string]any{
		"agent_name": "echo-agent",
		"prompt":     "hello",
	})
	env := &tools.Env{SessionID: "s1", Logger: slog.Default()}
	result, err := tool.Run(context.Background(), env, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected error: %s", result.Output)
	}
	if !gate.called {
		t.Error("permission gate should have been called")
	}
}
