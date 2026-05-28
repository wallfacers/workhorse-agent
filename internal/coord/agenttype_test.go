package coord

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeYAML(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// builtinCount is the number of embedded agent types the test environment
// expects (currently: adapter-generator). Tests that inspect List() output
// reference this so we don't have to update them every time a new builtin
// agent ships.
const builtinCount = 1

func TestLoader_List_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := NewLoader(dir).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != builtinCount {
		t.Fatalf("expected %d builtins, got %d", builtinCount, len(got))
	}
}

func TestLoader_List_MissingDirIsEmpty(t *testing.T) {
	got, err := NewLoader("/nonexistent/path/should/not/exist").List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != builtinCount {
		t.Fatalf("expected %d builtins even with missing dir, got %d", builtinCount, len(got))
	}
}

func TestLoader_Get_ByExplicitName(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "researcher.yaml", `
name: researcher
description: web research
system_prompt: You are a researcher.
tools:
  allow: [Read, Grep]
provider: anthropic
model: claude-sonnet-4-6
max_iterations: 30
`)
	at, err := NewLoader(dir).Get("researcher")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if at.Name != "researcher" || at.SystemPrompt == "" || at.MaxIterations != 30 {
		t.Fatalf("parsed: %+v", at)
	}
	if len(at.AllowTools) != 2 || at.AllowTools[0] != "Read" {
		t.Fatalf("allow: %v", at.AllowTools)
	}
}

func TestLoader_Get_NameDefaultsFromFilename(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "coder.yaml", `
description: writes code
system_prompt: You are a coder.
`)
	at, err := NewLoader(dir).Get("coder")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if at.Name != "coder" {
		t.Fatalf("default name: %s", at.Name)
	}
	if at.MaxIterations != 50 {
		t.Fatalf("default max_iterations: %d", at.MaxIterations)
	}
}

func TestLoader_Get_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := NewLoader(dir).Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestLoader_List_DuplicateNameError(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "a.yaml", `name: dup`)
	writeYAML(t, dir, "b.yaml", `name: dup`)
	_, err := NewLoader(dir).List()
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestLoader_HotReload(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "agent.yaml", `
name: agent
system_prompt: v1
`)
	l := NewLoader(dir)
	at, _ := l.Get("agent")
	if at.SystemPrompt != "v1" {
		t.Fatalf("v1 read: %q", at.SystemPrompt)
	}
	// Edit and re-Get.
	writeYAML(t, dir, "agent.yaml", `
name: agent
system_prompt: v2
`)
	at2, _ := l.Get("agent")
	if at2.SystemPrompt != "v2" {
		t.Fatalf("hot reload failed: %q", at2.SystemPrompt)
	}
}

func TestLoader_List_IgnoresNonYAML(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "x.yaml", `name: x`)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# stuff"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := NewLoader(dir).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1+builtinCount {
		t.Fatalf("expected %d (1 on-disk + builtins), got %d", 1+builtinCount, len(got))
	}
}

func TestLoader_BuiltinAdapterGenerator(t *testing.T) {
	// With no on-disk overrides the builtin adapter-generator must be
	// discoverable, and its tool surface must be the locked-down list
	// regardless of what the YAML says.
	dir := t.TempDir()
	at, err := NewLoader(dir).Get(AdapterGeneratorTypeName)
	if err != nil {
		t.Fatalf("Get %s: %v", AdapterGeneratorTypeName, err)
	}
	wantAllow := []string{"Bash", "Read", "WriteAdapterDraft"}
	if len(at.AllowTools) != len(wantAllow) {
		t.Fatalf("AllowTools: got %v, want %v", at.AllowTools, wantAllow)
	}
	for i, name := range wantAllow {
		if at.AllowTools[i] != name {
			t.Errorf("AllowTools[%d]: got %q, want %q", i, at.AllowTools[i], name)
		}
	}
	for _, denied := range []string{"Dispatch", "ExternalAgent", "agent_setup", "Write", "Edit"} {
		found := false
		for _, d := range at.DenyTools {
			if d == denied {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DenyTools missing %q: got %v", denied, at.DenyTools)
		}
	}
}

func TestLoader_AdapterGenerator_OnDiskTamperingIgnored(t *testing.T) {
	// A user dropping adapter-generator.yaml on disk with bogus allow/deny
	// must NOT change the locked-down surface — the lockdown is enforced in
	// code regardless of the loaded YAML.
	dir := t.TempDir()
	writeYAML(t, dir, "adapter-generator.yaml", `
name: adapter-generator
description: tampered
system_prompt: pwned
tools:
  allow: [Bash, Write, Edit, Dispatch, ExternalAgent]
  deny: []
max_iterations: 99
`)
	at, err := NewLoader(dir).Get(AdapterGeneratorTypeName)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Override took effect for non-locked fields (system_prompt) but tools
	// list MUST stay locked.
	if at.SystemPrompt != "pwned" {
		t.Errorf("system_prompt override should still apply: %q", at.SystemPrompt)
	}
	for _, banned := range []string{"Write", "Edit", "Dispatch", "ExternalAgent"} {
		for _, a := range at.AllowTools {
			if a == banned {
				t.Errorf("allow list leaked banned tool %q", banned)
			}
		}
	}
}
