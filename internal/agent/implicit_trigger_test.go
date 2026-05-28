package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// fakeSetupTool returns a fixed text on Run; suitable for asserting the
// interceptor invokes agent_setup exactly when it should.
type fakeSetupTool struct {
	called  int
	binArg  string
	output  string
	isError bool
}

func (*fakeSetupTool) Name() string                  { return "agent_setup" }
func (*fakeSetupTool) Description() string           { return "fake" }
func (*fakeSetupTool) InputSchema() json.RawMessage  { return json.RawMessage(`{}`) }
func (*fakeSetupTool) IsReadOnly() bool              { return false }
func (*fakeSetupTool) CanRunInParallel() bool        { return false }
func (*fakeSetupTool) DefaultTimeout() time.Duration { return 0 }

func (f *fakeSetupTool) Run(_ context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	f.called++
	var in struct {
		Binary string `json:"binary"`
	}
	_ = json.Unmarshal(raw, &in)
	f.binArg = in.Binary
	return &tools.Result{Output: f.output, IsError: f.isError}, nil
}

func newSession(t *testing.T) *session.Session {
	t.Helper()
	return session.New(session.Options{
		Workdir: t.TempDir(),
		Env:     map[string]string{},
	})
}

func mkToolCall(id, agentName, prompt string) agent.ToolCall {
	in, _ := json.Marshal(map[string]any{"agent_name": agentName, "prompt": prompt})
	return agent.ToolCall{ID: id, Name: "ExternalAgent", Input: in}
}

func TestInterceptor_PassesThroughNonExternalAgent(t *testing.T) {
	setup := &fakeSetupTool{}
	hook := agent.MakeImplicitTriggerHook(agent.ImplicitTriggerConfig{
		Enabled: true, SetupTool: setup, Env: &tools.Env{},
	})
	calls := []agent.ToolCall{{ID: "1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}}
	pass, intercepted := hook(context.Background(), newSession(t), calls)
	if len(pass) != 1 || len(intercepted) != 0 {
		t.Errorf("non-ExternalAgent should pass through, got pass=%d intercepted=%d", len(pass), len(intercepted))
	}
	if setup.called != 0 {
		t.Errorf("agent_setup must not be invoked for Bash, got %d", setup.called)
	}
}

func TestInterceptor_KnownAdapterPassesThrough(t *testing.T) {
	// Build a registry holding "gemini".
	adapter := &extagent.Adapter{Name: "gemini"}
	snap := extagent.NewSnapshot([]*extagent.Adapter{adapter})
	env := &tools.Env{ExtAgentRegistry: extagent.NewRegistry(snap)}

	setup := &fakeSetupTool{}
	hook := agent.MakeImplicitTriggerHook(agent.ImplicitTriggerConfig{
		Enabled: true, SetupTool: setup, Env: env,
	})
	pass, intercepted := hook(context.Background(), newSession(t),
		[]agent.ToolCall{mkToolCall("a", "gemini", "hi")})
	if len(pass) != 1 || len(intercepted) != 0 {
		t.Errorf("known adapter should pass through, got pass=%d intercepted=%d", len(pass), len(intercepted))
	}
	if setup.called != 0 {
		t.Errorf("agent_setup must not be invoked for known adapter")
	}
}

func TestInterceptor_UnknownWithoutBinaryPassesThrough(t *testing.T) {
	env := &tools.Env{ExtAgentRegistry: extagent.NewRegistry(extagent.NewSnapshot(nil))}
	setup := &fakeSetupTool{}
	hook := agent.MakeImplicitTriggerHook(agent.ImplicitTriggerConfig{
		Enabled: true, SetupTool: setup, Env: env,
	})
	// "definitely-not-on-path-xyz999" is not installed → ExternalAgent's own
	// rejection path handles it.
	pass, intercepted := hook(context.Background(), newSession(t),
		[]agent.ToolCall{mkToolCall("a", "definitely-not-on-path-xyz999", "hi")})
	if len(pass) != 1 {
		t.Errorf("missing binary should pass through, got %d", len(pass))
	}
	if len(intercepted) != 0 {
		t.Errorf("missing binary must not be intercepted, got %d", len(intercepted))
	}
	if setup.called != 0 {
		t.Errorf("agent_setup must not be invoked when binary is missing")
	}
}

func TestInterceptor_UnknownWithBinaryTriggersSetup(t *testing.T) {
	// Install a fake bin on a temp PATH.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "fakeimplicit")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho hi"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Confirm precondition.
	if _, err := exec.LookPath("fakeimplicit"); err != nil {
		t.Fatalf("setup precondition: %v", err)
	}

	env := &tools.Env{ExtAgentRegistry: extagent.NewRegistry(extagent.NewSnapshot(nil))}
	setup := &fakeSetupTool{output: "approval_id=01HEXTEST"}
	hook := agent.MakeImplicitTriggerHook(agent.ImplicitTriggerConfig{
		Enabled: true, SetupTool: setup, Env: env,
	})
	sess := newSession(t)
	pass, intercepted := hook(context.Background(), sess,
		[]agent.ToolCall{mkToolCall("a", "fakeimplicit", "hi")})
	if len(pass) != 0 || len(intercepted) != 1 {
		t.Fatalf("expected single interception, got pass=%d intercepted=%d", len(pass), len(intercepted))
	}
	if setup.called != 1 {
		t.Errorf("agent_setup should be invoked exactly once, got %d", setup.called)
	}
	if setup.binArg != "fakeimplicit" {
		t.Errorf("setup should receive binary=fakeimplicit, got %q", setup.binArg)
	}
	if !strings.HasPrefix(intercepted[0].Result.Output, "Adapter for 'fakeimplicit' was not registered") {
		t.Errorf("intercepted text should start with the marker substring, got: %s", intercepted[0].Result.Output)
	}
	if sess.AdapterSetupState("fakeimplicit") != "pending" {
		t.Errorf("dedup state should be pending after setup, got %q", sess.AdapterSetupState("fakeimplicit"))
	}
}

func TestInterceptor_PendingDedupShortCircuits(t *testing.T) {
	env := &tools.Env{ExtAgentRegistry: extagent.NewRegistry(extagent.NewSnapshot(nil))}
	setup := &fakeSetupTool{}
	hook := agent.MakeImplicitTriggerHook(agent.ImplicitTriggerConfig{
		Enabled: true, SetupTool: setup, Env: env,
	})
	sess := newSession(t)
	sess.SetAdapterSetupState("fakeimplicit", "pending")
	pass, intercepted := hook(context.Background(), sess,
		[]agent.ToolCall{mkToolCall("a", "fakeimplicit", "hi")})
	if len(pass) != 0 || len(intercepted) != 1 {
		t.Fatalf("expected single interception")
	}
	if setup.called != 0 {
		t.Errorf("pending dedup should short-circuit setup, got %d calls", setup.called)
	}
	if !strings.Contains(intercepted[0].Result.Output, "pending") {
		t.Errorf("pending dedup should explain status, got: %s", intercepted[0].Result.Output)
	}
}

func TestInterceptor_UnavailableDedupShortCircuits(t *testing.T) {
	env := &tools.Env{ExtAgentRegistry: extagent.NewRegistry(extagent.NewSnapshot(nil))}
	setup := &fakeSetupTool{}
	hook := agent.MakeImplicitTriggerHook(agent.ImplicitTriggerConfig{
		Enabled: true, SetupTool: setup, Env: env,
	})
	sess := newSession(t)
	sess.SetAdapterSetupState("fakeimplicit", "unavailable")
	pass, intercepted := hook(context.Background(), sess,
		[]agent.ToolCall{mkToolCall("a", "fakeimplicit", "hi")})
	if len(pass) != 0 || len(intercepted) != 1 {
		t.Fatalf("expected single interception")
	}
	if setup.called != 0 {
		t.Errorf("unavailable dedup should short-circuit setup")
	}
	if !strings.Contains(intercepted[0].Result.Output, "rejected") &&
		!strings.Contains(intercepted[0].Result.Output, "expired") {
		t.Errorf("unavailable dedup should explain status, got: %s", intercepted[0].Result.Output)
	}
}

func TestInterceptor_DisabledIsNoOp(t *testing.T) {
	setup := &fakeSetupTool{}
	hook := agent.MakeImplicitTriggerHook(agent.ImplicitTriggerConfig{
		Enabled: false, SetupTool: setup, Env: &tools.Env{},
	})
	calls := []agent.ToolCall{mkToolCall("a", "anything", "hi")}
	pass, intercepted := hook(context.Background(), newSession(t), calls)
	if len(pass) != 1 || len(intercepted) != 0 {
		t.Errorf("disabled hook should be a no-op")
	}
	if setup.called != 0 {
		t.Errorf("disabled hook should never invoke setup")
	}
}
