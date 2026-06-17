package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// memoryExportFixture creates a temp store seeded with two entries and a
// config.yaml pointing at it, returning the config path.
func memoryExportFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	es := memory.NewEntryStore(st.DB())
	for _, e := range []*memory.Entry{
		{Name: "user-profile", Trigger: "always", Content: "the user is a Go dev", Pinned: true, Durability: "evergreen", Category: "user", CharCount: 20},
		{Name: "build-quirk", Trigger: "when building", Content: "run make first", Durability: "volatile", Category: "project", CharCount: 14},
	} {
		if err := es.Upsert(ctx, e); err != nil {
			t.Fatalf("seed %q: %v", e.Name, err)
		}
	}
	_ = st.Close()

	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := "store:\n  path: " + dbPath + "\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func TestMemoryExportToStdout(t *testing.T) {
	cfgPath := memoryExportFixture(t)
	var stdout, stderr bytes.Buffer
	if err := runMemory([]string{"export", "--config", cfgPath}, &stdout, &stderr); err != nil {
		t.Fatalf("export: %v (stderr=%s)", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "_2 entries (1 pinned)_") {
		t.Fatalf("summary missing:\n%s", out)
	}
	if !strings.Contains(out, "## user-profile") || !strings.Contains(out, "the user is a Go dev") {
		t.Fatalf("pinned entry missing:\n%s", out)
	}
	// Pinned entry must precede the volatile one.
	if strings.Index(out, "## user-profile") > strings.Index(out, "## build-quirk") {
		t.Fatalf("pinned entry not first:\n%s", out)
	}
}

func TestMemoryExportToFile(t *testing.T) {
	cfgPath := memoryExportFixture(t)

	// --out resolves through pathguard relative to the working directory, so run
	// from a temp dir and write a relative path beneath it.
	wd := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var stdout, stderr bytes.Buffer
	if err := runMemory([]string{"export", "--config", cfgPath, "--out", "memory.md"}, &stdout, &stderr); err != nil {
		t.Fatalf("export to file: %v (stderr=%s)", err, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(wd, "memory.md"))
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	if !strings.Contains(string(data), "## build-quirk") {
		t.Fatalf("file missing entry:\n%s", data)
	}
	if !strings.Contains(stderr.String(), "wrote 2 entries") {
		t.Fatalf("missing confirmation on stderr: %q", stderr.String())
	}
}

func TestMemoryUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := runMemory([]string{"bogus"}, &stdout, &stderr); err == nil {
		t.Fatal("expected usage error for unknown subcommand")
	}
}
