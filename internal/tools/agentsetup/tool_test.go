package agentsetup_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/approval"
	"github.com/wallfacers/workhorse-agent/internal/prompt"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/agentsetup"
	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

type recDispatcher struct {
	calls   int
	prompt  string
	model   string
	env     map[string]string
	output  string
	failErr error
}

func (r *recDispatcher) Dispatch(_ context.Context, _, prompt, model string, env map[string]string) (string, error) {
	r.calls++
	r.prompt = prompt
	r.model = model
	r.env = env
	if r.failErr != nil {
		return "", r.failErr
	}
	return r.output, nil
}

// installFakeBin writes a small executable script at <dir>/<name>. Returns
// the absolute path. Caller must ensure <dir> is on PATH or pass the abs
// path directly.
func installFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func validDraftYAML(name string) string {
	return `name: ` + name + `
binary: ` + name + `
class: sub_agent
invocation:
  prompt_via: stdin
  extra_args: []
  env_passthrough: []
output:
  format: text
  stderr: separate
control:
  cancel_signal: SIGINT
  cancel_grace_sec: 5
  default_timeout_sec: 600
  max_timeout_sec: 3600
security:
  network: allowed
  filesystem: full
  trusted: false
smoke_test:
  prompt: "Reply with exactly: WORKHORSE_SMOKE_OK"
  expected_substring: "WORKHORSE_SMOKE_OK"
  timeout_sec: 60
description: "test fixture"
provenance:
  source: llm_generated
`
}

func newHost(t *testing.T) (*agentsetup.Host, string, *recDispatcher) {
	t.Helper()
	extDir := t.TempDir()
	disp := &recDispatcher{output: "draft submitted"}
	h := &agentsetup.Host{
		ExternalAgentsDir: extDir,
		Dispatcher:        disp,
		Approval:          approval.New(approval.Options{}),
		SchemaJSON:        "{}",
		Examples:          []prompt.AdapterGenerationExample{},
		ModelDefault:      "anthropic:claude-opus-4-7",
	}
	return h, extDir, disp
}

func TestAgentSetup_RejectsEmptyBinary(t *testing.T) {
	h, _, _ := newHost(t)
	tool := agentsetup.Tool{Host: h}
	raw, _ := json.Marshal(agentsetup.Input{Binary: " "})
	res, err := tool.Run(context.Background(), &tools.Env{SessionID: "s"}, raw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Errorf("empty binary should be rejected: %s", res.Output)
	}
}

func TestAgentSetup_RejectsMissingBinary(t *testing.T) {
	h, _, _ := newHost(t)
	tool := agentsetup.Tool{Host: h}
	raw, _ := json.Marshal(agentsetup.Input{Binary: "definitely-not-on-path-zzz9999"})
	res, _ := tool.Run(context.Background(), &tools.Env{SessionID: "s"}, raw)
	if !res.IsError || !strings.Contains(res.Output, "not found on PATH") {
		t.Errorf("missing binary should produce PATH error, got: %s", res.Output)
	}
}

func TestAgentSetup_RejectsSystemShell(t *testing.T) {
	h, _, _ := newHost(t)
	// Find the system bash explicitly so we can pass an absolute path even
	// when the test runs in a hardened environment.
	abs, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	tool := agentsetup.Tool{Host: h}
	raw, _ := json.Marshal(agentsetup.Input{Binary: abs})
	res, _ := tool.Run(context.Background(), &tools.Env{SessionID: "s"}, raw)
	if !res.IsError || !strings.Contains(res.Output, "shell") {
		t.Errorf("bash should be rejected, got: %s", res.Output)
	}
}

func TestAgentSetup_AlreadyRegisteredWithoutRegenerate(t *testing.T) {
	// Build a registry holding a pre-existing adapter named "fakebin".
	binDir := t.TempDir()
	abs := installFakeBin(t, binDir, "fakebin", "echo hi")
	existing := &extagent.Adapter{Name: "fakebin", Binary: abs, ResolvedBinary: abs}
	snap := extagent.NewSnapshot([]*extagent.Adapter{existing})
	reg := extagent.NewRegistry(snap)

	h, _, _ := newHost(t)
	h.Registry = reg
	tool := agentsetup.Tool{Host: h}
	raw, _ := json.Marshal(agentsetup.Input{Binary: abs})
	res, _ := tool.Run(context.Background(), &tools.Env{SessionID: "s"}, raw)
	if !res.IsError || !strings.Contains(res.Output, "already registered") {
		t.Errorf("expected already-registered error, got: %s", res.Output)
	}
}

func TestAgentSetup_HappyPath_Approval(t *testing.T) {
	binDir := t.TempDir()
	abs := installFakeBin(t, binDir, "fakebin", "echo hi")

	h, extDir, disp := newHost(t)
	// Simulate the subagent writing a valid draft.
	draftsDir := pathguard.DraftsDir(extDir)
	if err := os.MkdirAll(draftsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	disp.output = "ok"
	go func() {
		// Sleep slightly so this happens during the subagent dispatch (the
		// stub returns immediately, but ordering doesn't matter — we write
		// before the dispatcher returns when the stub synchronously calls
		// back. Use a synchronous placement instead.)
	}()
	// Pre-write the draft so the simulated subagent appears to have done so.
	drafted := filepath.Join(draftsDir, "fakebin.yaml")
	if err := os.WriteFile(drafted, []byte(validDraftYAML("fakebin")), 0o600); err != nil {
		t.Fatal(err)
	}

	// Override the dispatcher so the draft already exists when we get here:
	// the real subagent would write inside Dispatch.
	h.Dispatcher = &draftWritingDispatcher{
		extDir:   extDir,
		yamlBody: validDraftYAML("fakebin"),
		name:     "fakebin",
	}

	tool := agentsetup.Tool{Host: h}
	raw, _ := json.Marshal(agentsetup.Input{Binary: abs})
	res, _ := tool.Run(context.Background(), &tools.Env{SessionID: "s"}, raw)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "approval_id=") {
		t.Errorf("output should include approval_id: %s", res.Output)
	}
}

// draftWritingDispatcher simulates an adapter-generator subagent by writing
// a draft into <ext>/.drafts/<name>.yaml inside Dispatch, then returning a
// success string.
type draftWritingDispatcher struct {
	extDir   string
	yamlBody string
	name     string
}

func (d *draftWritingDispatcher) Dispatch(_ context.Context, _, _, _ string, _ map[string]string) (string, error) {
	dir := filepath.Join(d.extDir, ".drafts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return "ok", os.WriteFile(filepath.Join(dir, d.name+".yaml"), []byte(d.yamlBody), 0o600)
}

func TestAgentSetup_RejectsCLIToolRefusal(t *testing.T) {
	binDir := t.TempDir()
	abs := installFakeBin(t, binDir, "fakebin", "echo hi")
	h, _, _ := newHost(t)
	// Dispatcher emits CLI_TOOL_REFUSAL — agent_setup must surface it.
	h.Dispatcher = &recDispatcher{output: "CLI_TOOL_REFUSAL: fakebin has no prompt-passing convention. Add to pathscan.extra."}
	tool := agentsetup.Tool{Host: h}
	raw, _ := json.Marshal(agentsetup.Input{Binary: abs})
	res, _ := tool.Run(context.Background(), &tools.Env{SessionID: "s"}, raw)
	if !res.IsError || !strings.Contains(res.Output, "cli_tool refusal") {
		t.Errorf("expected cli_tool refusal surface, got: %s", res.Output)
	}
}

func TestAgentSetup_ModelAllowlist(t *testing.T) {
	binDir := t.TempDir()
	abs := installFakeBin(t, binDir, "fakebin", "echo hi")
	h, _, _ := newHost(t)
	h.AllowedModels = []string{"anthropic:claude-haiku-4-5-20251001"}
	h.Dispatcher = &draftWritingDispatcher{extDir: h.ExternalAgentsDir, yamlBody: validDraftYAML("fakebin"), name: "fakebin"}
	tool := agentsetup.Tool{Host: h}
	raw, _ := json.Marshal(agentsetup.Input{Binary: abs, Model: "anthropic:claude-opus-4-7"})
	res, _ := tool.Run(context.Background(), &tools.Env{SessionID: "s"}, raw)
	if !res.IsError || !strings.Contains(res.Output, "allowed_models") {
		t.Errorf("model outside allowlist should be rejected, got: %s", res.Output)
	}
}
