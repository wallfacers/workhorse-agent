package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/instructions"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/builtin"
)

func TestRead_ProximityInjection(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	fooDir := filepath.Join(srcDir, "foo")
	os.MkdirAll(fooDir, 0o755)

	os.WriteFile(filepath.Join(srcDir, "AGENTS.md"), []byte("src rules"), 0o644)
	os.WriteFile(filepath.Join(fooDir, "bar.go"), []byte("package foo"), 0o644)
	os.WriteFile(filepath.Join(fooDir, "baz.go"), []byte("package foo"), 0o644)

	snap := &instructions.Snapshot{
		Files: []instructions.File{{Path: filepath.Join(root, "AGENTS.md"), Content: "root"}},
	}
	resolver := instructions.NewResolver(snap)

	r := builtin.Read{MaxBytes: 1 << 20}
	env := &tools.Env{
		Workdir:             root,
		InstructionResolver: resolver,
	}

	// First Read — should inject src/AGENTS.md.
	out1, err := r.Run(context.Background(), env, mkReadInput(filepath.Join(fooDir, "bar.go")))
	if err != nil {
		t.Fatal(err)
	}
	if out1.IsError {
		t.Fatalf("unexpected error: %s", out1.Output)
	}
	if !contains(out1.Output, "src rules") {
		t.Error("first read should contain proximity-injected src/AGENTS.md content")
	}
	if !contains(out1.Output, "<system-reminder>") {
		t.Error("first read should contain <system-reminder> block")
	}

	// Second Read of sibling file — should NOT inject again.
	out2, err := r.Run(context.Background(), env, mkReadInput(filepath.Join(fooDir, "baz.go")))
	if err != nil {
		t.Fatal(err)
	}
	if contains(out2.Output, "src rules") {
		t.Error("second read should NOT re-inject src/AGENTS.md (dedup)")
	}
}

func mkReadInput(path string) json.RawMessage {
	return json.RawMessage(`{"path":"` + filepath.ToSlash(path) + `"}`)
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }
