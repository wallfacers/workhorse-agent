package genbash_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/extagent/genbash"
)

// stubBash is a minimal tools.Tool that records its inputs and pretends to
// succeed. The inspector should never invoke it for rejected commands.
type stubBash struct {
	called  int
	lastCmd string
}

func (*stubBash) Name() string                  { return "Bash" }
func (*stubBash) Description() string           { return "stub" }
func (*stubBash) InputSchema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (*stubBash) IsReadOnly() bool              { return false }
func (*stubBash) CanRunInParallel() bool        { return false }
func (*stubBash) DefaultTimeout() time.Duration { return 0 }

func (s *stubBash) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(raw, &in)
	s.called++
	s.lastCmd = in.Command
	return &tools.Result{Output: "ok"}, nil
}

func TestInspect_AllowsEachPattern(t *testing.T) {
	cases := []string{
		"which gemini",
		"type gemini",
		"command -v gemini",
		"readlink /usr/local/bin/gemini",
		"readlink -f /usr/local/bin/gemini",
		"file /usr/local/bin/gemini",
		"ls /usr/local/bin",
		"ls -l /usr/local/bin",
		"man gemini",
		"cat /usr/local/share/doc/gemini/README",
		"head /usr/share/doc/gemini/README.md",
		"head -n 100 /usr/share/doc/gemini/README.md",
		"gemini --help",
		"gemini -h",
		"gemini help",
		"gemini -?",
		"gemini chat --help",
		"gemini --version",
		"gemini -V",
		"gemini version",
	}
	for _, cmd := range cases {
		if reason := genbash.Inspect(cmd, "/usr/local/"); reason != "" {
			t.Errorf("cmd=%q: expected allow, got reject: %s", cmd, reason)
		}
	}
}

func TestInspect_RejectsMetacharacters(t *testing.T) {
	cases := map[string]string{
		"semicolon":   "which gemini; rm -rf /",
		"pipe":        "which gemini | tee /tmp/x",
		"and":         "which gemini & sleep 1",
		"andand":      "which gemini && evil",
		"oror":        "which gemini || evil",
		"redirect":    "which gemini > /tmp/x",
		"redirect-in": "cat < /etc/passwd",
		"append":      "cat /etc/hostname >> /tmp/x",
		"heredoc":     "cat <<EOF\nhi\nEOF",
		"backtick":    "echo `which gemini`",
		"cmdsubst":    "readlink -f $(which gemini)",
		"newline":     "which gemini\nrm -rf /",
	}
	for label, cmd := range cases {
		if genbash.Inspect(cmd, "/usr/local/") == "" {
			t.Errorf("%s (cmd=%q) should be rejected", label, cmd)
		}
	}
}

func TestInspect_RejectsUnknownPattern(t *testing.T) {
	cases := []string{
		"rm /tmp/x",
		"echo hi",
		"git status",
		"curl https://example.com",
	}
	for _, cmd := range cases {
		if genbash.Inspect(cmd, "/usr/local/") == "" {
			t.Errorf("cmd=%q should be rejected (no matching pattern)", cmd)
		}
	}
}

func TestInspect_PathCommandsScopedToInstallPrefix(t *testing.T) {
	// cat /etc/passwd is path-taking and falls outside /usr/local/ — reject.
	if reason := genbash.Inspect("cat /etc/passwd", "/usr/local/"); reason == "" {
		t.Error("cat /etc/passwd should be rejected when prefix is /usr/local/")
	}
	// cat under the install prefix is fine.
	if reason := genbash.Inspect("cat /usr/local/share/doc/gemini/README", "/usr/local/"); reason != "" {
		t.Errorf("cat under install prefix should be accepted: %s", reason)
	}
	// cat under standard /usr/share/doc/ is fine regardless of prefix.
	if reason := genbash.Inspect("cat /usr/share/doc/git/README", "/opt/foo/"); reason != "" {
		t.Errorf("cat under /usr/share/doc/ should be accepted: %s", reason)
	}
}

func TestInspect_CommandSubstitutionRejected(t *testing.T) {
	// The spec explicitly calls this out — generator must split into two
	// separate calls (which gemini → /abs/path → readlink -f /abs/path).
	if genbash.Inspect("readlink -f $(which gemini)", "/usr/local/") == "" {
		t.Error("command substitution must be rejected")
	}
}

func TestInspect_CrossBinaryProbeAllowed(t *testing.T) {
	// Probing a different binary's version (e.g. git --version while
	// analyzing gemini) is harmless and explicitly allowed.
	if reason := genbash.Inspect("git --version", "/usr/local/"); reason != "" {
		t.Errorf("cross-binary version probe should be accepted: %s", reason)
	}
}

func TestTool_RejectionDoesNotInvokeBackend(t *testing.T) {
	stub := &stubBash{}
	tool := genbash.Tool{Host: &genbash.Host{InstallPrefix: "/usr/local/"}, Backend: stub}
	raw, _ := json.Marshal(map[string]any{"command": "rm -rf /"})
	res, err := tool.Run(context.Background(), &tools.Env{}, raw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Error("rejected command should produce error result")
	}
	if stub.called != 0 {
		t.Errorf("backend should not be called when inspector rejects, got %d calls", stub.called)
	}
	if !strings.Contains(res.Output, "rejected") {
		t.Errorf("rejection output should explain why: %q", res.Output)
	}
}

func TestTool_AllowedCommandReachesBackend(t *testing.T) {
	stub := &stubBash{}
	tool := genbash.Tool{Host: &genbash.Host{InstallPrefix: "/usr/local/"}, Backend: stub}
	raw, _ := json.Marshal(map[string]any{"command": "which gemini"})
	res, err := tool.Run(context.Background(), &tools.Env{}, raw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("allowed command produced error: %s", res.Output)
	}
	if stub.called != 1 {
		t.Errorf("backend should be called once, got %d", stub.called)
	}
	if stub.lastCmd != "which gemini" {
		t.Errorf("backend should receive original command, got %q", stub.lastCmd)
	}
}
