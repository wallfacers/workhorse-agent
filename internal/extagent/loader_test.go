package extagent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
)

func TestLoad_EmptyDir_BuiltinsOnly(t *testing.T) {
	dir := t.TempDir()
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg := extagent.NewRegistry(snap)
	all := reg.All()
	if len(all) == 0 {
		t.Error("expected builtin adapters to be loaded")
	}
	// Builtins will be BinaryMissing since the binaries aren't installed in test.
	for _, a := range all {
		if !a.BinaryMissing {
			// Could happen if the binary is on PATH in the test environment.
			t.Logf("adapter %q binary resolved to %q", a.Name, a.ResolvedBinary)
		}
	}
}

func TestLoad_DirNotExists_Created(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "external-agents")
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("directory should be created: stat err=%v, isDir=%v", err, fi != nil && fi.IsDir())
	}
	_ = snap
}

func TestLoad_OnDiskOverrideBuiltin(t *testing.T) {
	dir := t.TempDir()
	override := []byte(`
name: claude-code
binary: /my/custom/claude
class: cli_tool
description: "Custom override"
security:
  network: none
  filesystem: full
  trusted: true
provenance:
  source: user_yaml
`)
	if err := os.WriteFile(filepath.Join(dir, "claude-code.yaml"), override, 0o600); err != nil {
		t.Fatal(err)
	}
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg := extagent.NewRegistry(snap)
	a := reg.Get("claude-code")
	if a == nil {
		t.Fatal("claude-code adapter not found")
	}
	if a.Class != extagent.ClassCLITool {
		t.Errorf("expected cli_tool override, got %q", a.Class)
	}
	if a.Provenance.Source != "user_yaml" {
		t.Errorf("provenance: got %q, want user_yaml", a.Provenance.Source)
	}
}

func TestLoad_MixedValidInvalid(t *testing.T) {
	dir := t.TempDir()
	// Valid adapter.
	valid := []byte(`
name: my-tool
binary: my-tool-bin
class: cli_tool
description: "A tool"
security:
  network: none
  filesystem: full
  trusted: false
provenance:
  source: user_yaml
`)
	if err := os.WriteFile(filepath.Join(dir, "my-tool.yaml"), valid, 0o600); err != nil {
		t.Fatal(err)
	}
	// Invalid YAML (missing required fields).
	invalid := []byte(`name: bad`)
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load should not fail for one bad file: %v", err)
	}
	reg := extagent.NewRegistry(snap)
	if reg.Get("my-tool") == nil {
		t.Error("valid adapter should be loaded")
	}
	if reg.Get("bad") != nil {
		t.Error("invalid adapter should not be loaded")
	}
}

func TestLoad_UppercaseFilenameRejected(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`
name: my-uc-test
binary: my-uc-test
class: cli_tool
description: "test"
security:
  network: none
  filesystem: full
  trusted: true
provenance:
  source: user_yaml
`)
	if err := os.WriteFile(filepath.Join(dir, "My-UC-Test.yaml"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg := extagent.NewRegistry(snap)
	if reg.Get("my-uc-test") != nil {
		t.Error("uppercase filename should be rejected")
	}
}

func TestLoad_SpaceFilenameRejected(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`
name: my-space-test
binary: my-space-test
class: cli_tool
description: "test"
security:
  network: none
  filesystem: full
  trusted: true
provenance:
  source: user_yaml
`)
	if err := os.WriteFile(filepath.Join(dir, "my space test.yaml"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg := extagent.NewRegistry(snap)
	if reg.Get("my-space-test") != nil {
		t.Error("space in filename should be rejected")
	}
}

func TestLoad_NameMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`
name: wrong-name
binary: claude
class: cli_tool
description: "test"
security:
  network: none
  filesystem: full
  trusted: true
provenance:
  source: user_yaml
`)
	if err := os.WriteFile(filepath.Join(dir, "claude-code.yaml"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg := extagent.NewRegistry(snap)
	if reg.Get("wrong-name") != nil {
		t.Error("name mismatch should be rejected")
	}
}

func TestLoad_NonYAMLFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	// Write a .smoke file and a .bak file — both should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "test.smoke"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backup.bak"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Should not crash; non-YAML files silently skipped.
	_ = snap
}

func TestLoad_DotPrefixedSubdirIgnored(t *testing.T) {
	// .drafts/ holds in-flight adapter generation drafts (add-llm-adapter-generator
	// G2): valid YAML files there MUST NOT be loaded into a session's registry.
	dir := t.TempDir()
	drafts := filepath.Join(dir, ".drafts")
	if err := os.MkdirAll(drafts, 0o700); err != nil {
		t.Fatal(err)
	}
	validInDrafts := []byte(`
name: leaky-draft
binary: leaky-draft-bin
class: cli_tool
description: "Should never appear in a registry"
security:
  network: none
  filesystem: full
  trusted: false
provenance:
  source: llm_generated
`)
	if err := os.WriteFile(filepath.Join(drafts, "leaky-draft.yaml"), validInDrafts, 0o600); err != nil {
		t.Fatal(err)
	}
	// Also drop a dot-prefixed file at the top level — already excluded by the
	// .yaml suffix check, but the new guard should catch it independently.
	if err := os.WriteFile(filepath.Join(dir, ".hidden.yaml"), validInDrafts, 0o600); err != nil {
		t.Fatal(err)
	}
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg := extagent.NewRegistry(snap)
	if reg.Get("leaky-draft") != nil {
		t.Error("YAML under .drafts/ must not be loaded into the registry")
	}
	if reg.Get(".hidden") != nil || reg.Get("hidden") != nil {
		t.Error("dot-prefixed top-level YAML must not be loaded")
	}
}

func TestRegistry_Healthy(t *testing.T) {
	dir := t.TempDir()
	loader := &extagent.Loader{}
	snap, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	reg := extagent.NewRegistry(snap)
	// Without smoke test, nothing is healthy.
	healthy := reg.Healthy()
	if len(healthy) != 0 {
		t.Errorf("no adapters should be healthy without smoke test, got %d", len(healthy))
	}
}
