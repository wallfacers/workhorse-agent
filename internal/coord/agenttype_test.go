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

func TestLoader_List_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := NewLoader(dir).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
	}
}

func TestLoader_List_MissingDirIsEmpty(t *testing.T) {
	got, err := NewLoader("/nonexistent/path/should/not/exist").List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
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
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
}
