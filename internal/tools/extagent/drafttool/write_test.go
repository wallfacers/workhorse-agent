package drafttool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/extagent/drafttool"
	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

// validYAML returns a schema-valid adapter YAML with the given name. The
// fixture mirrors the aider builtin so it stays the smallest viable surface.
func validYAML(name string) string {
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
description: "test fixture adapter"
provenance:
  source: llm_generated
`
}

func runTool(t *testing.T, ext, path, content string) (*tools.Result, error) {
	t.Helper()
	tool := drafttool.Tool{Host: &drafttool.Host{ExternalAgentsDir: ext}}
	in := map[string]any{"path": path, "content": content}
	raw, _ := json.Marshal(in)
	return tool.Run(context.Background(), &tools.Env{SessionID: "test"}, raw)
}

func TestWriteAdapterDraft_HappyPath(t *testing.T) {
	ext := t.TempDir()
	draftPath := filepath.Join(pathguard.DraftsDir(ext), "gemini.yaml")
	res, err := runTool(t, ext, draftPath, validYAML("gemini"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	got, readErr := os.ReadFile(draftPath)
	if readErr != nil {
		t.Fatalf("read written draft: %v", readErr)
	}
	if !strings.Contains(string(got), "name: gemini") {
		t.Errorf("written content missing name field: %s", got)
	}
	info, statErr := os.Stat(draftPath)
	if statErr != nil {
		t.Fatalf("stat draft: %v", statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("draft mode: got %o, want 0600", info.Mode().Perm())
	}
}

func TestWriteAdapterDraft_CreatesDraftsDirWith0700(t *testing.T) {
	ext := t.TempDir()
	// .drafts/ does not exist yet
	if _, err := os.Stat(pathguard.DraftsDir(ext)); !os.IsNotExist(err) {
		t.Fatalf("precondition: drafts dir should not exist, got %v", err)
	}
	draftPath := filepath.Join(pathguard.DraftsDir(ext), "gemini.yaml")
	res, err := runTool(t, ext, draftPath, validYAML("gemini"))
	if err != nil || res.IsError {
		t.Fatalf("Run failed: err=%v, output=%s", err, res.Output)
	}
	info, statErr := os.Stat(pathguard.DraftsDir(ext))
	if statErr != nil {
		t.Fatalf("drafts dir not created: %v", statErr)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("drafts dir mode: got %o, want 0700", info.Mode().Perm())
	}
}

func TestWriteAdapterDraft_RejectsLiveDirPath(t *testing.T) {
	ext := t.TempDir()
	// Path into the live dir (not .drafts/)
	livePath := filepath.Join(ext, "gemini.yaml")
	res, err := runTool(t, ext, livePath, validYAML("gemini"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Errorf("live-dir path should be rejected, got success: %s", res.Output)
	}
	if _, statErr := os.Stat(livePath); statErr == nil {
		t.Error("file should not have been created in live dir")
	}
}

func TestWriteAdapterDraft_RejectsTraversal(t *testing.T) {
	ext := t.TempDir()
	cases := []string{
		"/etc/passwd.yaml",
		filepath.Join(ext, "..", "escape.yaml"),
		filepath.Join(pathguard.DraftsDir(ext), "sub", "gemini.yaml"),
		"relative/path.yaml",
		"",
	}
	for _, p := range cases {
		res, err := runTool(t, ext, p, validYAML("gemini"))
		if err != nil {
			t.Fatalf("path=%q Run: %v", p, err)
		}
		if !res.IsError {
			t.Errorf("path=%q should be rejected, got success", p)
		}
	}
}

func TestWriteAdapterDraft_RejectsInvalidNameRegex(t *testing.T) {
	ext := t.TempDir()
	// Each filename below is illegal: starts with dash, contains uppercase,
	// contains slash, contains dot in stem.
	bad := []string{"-leading-dash.yaml", "UpperCase.yaml", "with.dot.yaml", ".yaml"}
	for _, name := range bad {
		p := filepath.Join(pathguard.DraftsDir(ext), name)
		res, err := runTool(t, ext, p, validYAML(strings.TrimSuffix(name, ".yaml")))
		if err != nil {
			t.Fatalf("name=%q Run: %v", name, err)
		}
		if !res.IsError {
			t.Errorf("name=%q should be rejected as invalid", name)
		}
	}
}

func TestWriteAdapterDraft_RejectsSchemaInvalidContent(t *testing.T) {
	ext := t.TempDir()
	draftPath := filepath.Join(pathguard.DraftsDir(ext), "broken.yaml")
	res, err := runTool(t, ext, draftPath, "name: broken\nbinary: ''\n") // missing required fields
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Error("schema-invalid content should be rejected")
	}
	if _, statErr := os.Stat(draftPath); statErr == nil {
		t.Error("file should not be written when schema validation fails")
	}
}

func TestWriteAdapterDraft_RejectsNameMismatch(t *testing.T) {
	ext := t.TempDir()
	// Filename stem says "alpha" but YAML name says "beta" — must be caught.
	draftPath := filepath.Join(pathguard.DraftsDir(ext), "alpha.yaml")
	res, err := runTool(t, ext, draftPath, validYAML("beta"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Error("name mismatch should be rejected")
	}
}

func TestWriteAdapterDraft_AtomicReplace(t *testing.T) {
	ext := t.TempDir()
	draftPath := filepath.Join(pathguard.DraftsDir(ext), "alpha.yaml")
	// First write — establishes the file.
	if res, _ := runTool(t, ext, draftPath, validYAML("alpha")); res.IsError {
		t.Fatalf("first write: %s", res.Output)
	}
	// Second write — overwrites with new content.
	updated := validYAML("alpha") + "# trailing comment\n"
	if res, _ := runTool(t, ext, draftPath, updated); res.IsError {
		t.Fatalf("second write: %s", res.Output)
	}
	got, _ := os.ReadFile(draftPath)
	if !strings.Contains(string(got), "trailing comment") {
		t.Errorf("second write did not replace content: %s", got)
	}
	// Confirm the .draft-*.tmp pattern left no orphans.
	entries, _ := os.ReadDir(pathguard.DraftsDir(ext))
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".draft-") {
			t.Errorf("temp file %q not cleaned up", ent.Name())
		}
	}
}

func TestWriteAdapterDraft_InputSchemaShape(t *testing.T) {
	// Defensive check on the published schema — the LLM's tool_use planning
	// depends on these field names being stable.
	tool := drafttool.Tool{Host: &drafttool.Host{ExternalAgentsDir: "/tmp"}}
	raw := tool.InputSchema()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 2 {
		t.Fatalf("required: got %v", schema["required"])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["path"]; !ok {
		t.Error("missing 'path' property")
	}
	if _, ok := props["content"]; !ok {
		t.Error("missing 'content' property")
	}
}
