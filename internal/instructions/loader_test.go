package instructions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_noFiles(t *testing.T) {
	dir := t.TempDir()
	l := Loader{ProfileDir: t.TempDir()}
	snap, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(snap.Files))
	}
}

func TestLoad_agentsMdTakesPriorityOverClaudeMd(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents"), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude"), 0o644)

	l := Loader{ProfileDir: t.TempDir()}
	snap, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(snap.Files))
	}
	if snap.Files[0].Content != "agents" {
		t.Fatalf("expected agents content, got %q", snap.Files[0].Content)
	}
}

func TestLoad_claudeMdFallback(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude"), 0o644)

	l := Loader{ProfileDir: t.TempDir()}
	snap, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(snap.Files))
	}
	if snap.Files[0].Content != "claude" {
		t.Fatalf("expected claude content, got %q", snap.Files[0].Content)
	}
}

func TestLoad_monorepoNested(t *testing.T) {
	// repo/packages/sdk/ with git root at repo/
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	pkg := filepath.Join(repo, "packages", "sdk")
	os.MkdirAll(pkg, 0o755)

	os.WriteFile(filepath.Join(pkg, "AGENTS.md"), []byte("sdk"), 0o644)
	os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("root"), 0o644)

	l := Loader{ProfileDir: t.TempDir()}
	snap, err := l.Load(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(snap.Files))
	}
	// Bottom-up order: sdk first, then root.
	if snap.Files[0].Content != "sdk" {
		t.Fatalf("first file: expected sdk, got %q", snap.Files[0].Content)
	}
	if snap.Files[1].Content != "root" {
		t.Fatalf("second file: expected root, got %q", snap.Files[1].Content)
	}
}

func TestLoad_nonGitProject(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("only here"), 0o644)

	l := Loader{ProfileDir: t.TempDir()}
	snap, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(snap.Files))
	}
	if snap.Files[0].Content != "only here" {
		t.Fatalf("unexpected content: %q", snap.Files[0].Content)
	}
}

func TestLoad_globalOnly(t *testing.T) {
	profileDir := t.TempDir()
	os.WriteFile(filepath.Join(profileDir, "AGENTS.md"), []byte("global"), 0o644)

	projectDir := t.TempDir() // no AGENTS.md in project
	l := Loader{ProfileDir: profileDir}
	snap, err := l.Load(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(snap.Files))
	}
	if snap.Files[0].Content != "global" {
		t.Fatalf("expected global content, got %q", snap.Files[0].Content)
	}
}

func TestLoad_projectAndGlobal(t *testing.T) {
	profileDir := t.TempDir()
	os.WriteFile(filepath.Join(profileDir, "AGENTS.md"), []byte("global"), 0o644)

	projectDir := t.TempDir()
	os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), []byte("project"), 0o644)

	l := Loader{ProfileDir: profileDir}
	snap, err := l.Load(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(snap.Files))
	}
	// Project file first, global file second.
	if snap.Files[0].Content != "project" {
		t.Fatalf("first: expected project, got %q", snap.Files[0].Content)
	}
	if snap.Files[1].Content != "global" {
		t.Fatalf("second: expected global, got %q", snap.Files[1].Content)
	}
}

func TestLoad_emptyAgentsMdDoesNotBlackholeClaudeMd(t *testing.T) {
	dir := t.TempDir()
	// Empty AGENTS.md (e.g. a stray `touch`) must not suppress a content-bearing
	// CLAUDE.md fallback.
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude rules"), 0o644)

	l := Loader{ProfileDir: t.TempDir()}
	snap, err := l.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(snap.Files))
	}
	if snap.Files[0].Content != "claude rules" {
		t.Fatalf("expected claude rules content, got %q", snap.Files[0].Content)
	}
}

func TestLoad_missingGlobalFile(t *testing.T) {
	projectDir := t.TempDir()
	os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), []byte("project"), 0o644)

	l := Loader{ProfileDir: t.TempDir()} // profileDir has no AGENTS.md
	snap, err := l.Load(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(snap.Files))
	}
}
