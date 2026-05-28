package regen_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/regen"
)

func makeAdapter(t *testing.T, name, binDir, body, toolVersion string) *extagent.Adapter {
	t.Helper()
	binPath := filepath.Join(binDir, name)
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &extagent.Adapter{
		Name:           name,
		Binary:         binPath,
		ResolvedBinary: binPath,
	}
	a.Provenance.Source = "llm_generated"
	a.Provenance.ToolVersion = toolVersion
	return a
}

func newRegistry(adapters []*extagent.Adapter) *extagent.Registry {
	return extagent.NewRegistry(extagent.NewSnapshot(adapters))
}

func TestCheck_MatchingVersionNoDrift(t *testing.T) {
	bin := t.TempDir()
	a := makeAdapter(t, "fake", bin, `echo "fake 1.2.3"`, "1.2.3")
	got := regen.Check(newRegistry([]*extagent.Adapter{a}), nil)
	if len(got) != 0 {
		t.Errorf("matching version should produce no drift, got %+v", got)
	}
}

func TestCheck_MismatchedVersionFlagged(t *testing.T) {
	bin := t.TempDir()
	a := makeAdapter(t, "fake", bin, `echo "fake 2.0.0"`, "1.2.3")
	got := regen.Check(newRegistry([]*extagent.Adapter{a}), nil)
	if len(got) != 1 {
		t.Fatalf("mismatched version should flag drift, got %d entries", len(got))
	}
	if got[0].Was != "1.2.3" {
		t.Errorf("entry.was: %q", got[0].Was)
	}
	if got[0].Now != "fake 2.0.0" {
		t.Errorf("entry.now: %q", got[0].Now)
	}
}

func TestCheck_EmptyStoredVersionSkipped(t *testing.T) {
	bin := t.TempDir()
	a := makeAdapter(t, "fake", bin, `echo "fake 2.0.0"`, "")
	got := regen.Check(newRegistry([]*extagent.Adapter{a}), nil)
	if len(got) != 0 {
		t.Errorf("empty stored version should be skipped, got %+v", got)
	}
}

func TestCheck_BinaryMissingSkipped(t *testing.T) {
	bin := t.TempDir()
	a := makeAdapter(t, "fake", bin, `echo "fake 2.0.0"`, "1.0.0")
	a.BinaryMissing = true
	got := regen.Check(newRegistry([]*extagent.Adapter{a}), nil)
	if len(got) != 0 {
		t.Errorf("binary-missing should be skipped, got %+v", got)
	}
}

func TestCheck_OnlyLLMGeneratedConsidered(t *testing.T) {
	bin := t.TempDir()
	a := makeAdapter(t, "fake", bin, `echo "fake 2.0.0"`, "1.0.0")
	a.Provenance.Source = "builtin" // not llm_generated
	got := regen.Check(newRegistry([]*extagent.Adapter{a}), nil)
	if len(got) != 0 {
		t.Errorf("builtin adapters should not be drift-checked, got %+v", got)
	}
}

func TestCheck_VersionEmbeddedInBannerTreatedAsMatch(t *testing.T) {
	// CLIs that print "tool v1.2.3 (build abc, 2026-05-28)" must be accepted
	// when stored is "v1.2.3" or "1.2.3" — the substring rule prevents false
	// positives.
	bin := t.TempDir()
	a := makeAdapter(t, "fake", bin, `echo "fake v1.2.3 (build abc, 2026-05-28)"`, "v1.2.3")
	got := regen.Check(newRegistry([]*extagent.Adapter{a}), nil)
	if len(got) != 0 {
		t.Errorf("banner containing stored version should match, got %+v", got)
	}
}
