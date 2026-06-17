package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// migrateFixture spins up an in-memory store + a temp profile dir, writing the
// given USER.md / MEMORY.md (empty string = file not written).
func migrateFixture(t *testing.T, userMD, memoryMD string) (*memory.EntryStore, string) {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	profileDir := t.TempDir()
	memDir := filepath.Join(profileDir, "memories")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir memories: %v", err)
	}
	if userMD != "" {
		if err := os.WriteFile(filepath.Join(memDir, "USER.md"), []byte(userMD), 0o600); err != nil {
			t.Fatalf("write USER.md: %v", err)
		}
	}
	if memoryMD != "" {
		if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(memoryMD), 0o600); err != nil {
			t.Fatalf("write MEMORY.md: %v", err)
		}
	}
	return memory.NewEntryStore(s.DB()), profileDir
}

func entriesByName(t *testing.T, es *memory.EntryStore) map[string]*memory.Entry {
	t.Helper()
	list, err := es.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := map[string]*memory.Entry{}
	for _, e := range list {
		out[e.Name] = e
	}
	return out
}

func TestMigrateUserMDToPinnedEntry(t *testing.T) {
	ctx := context.Background()
	content := "I am 老王, I prefer concise answers."
	es, dir := migrateFixture(t, content, "")

	if err := memory.MigrateLegacyFiles(ctx, es, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	byName := entriesByName(t, es)
	if len(byName) != 1 {
		t.Fatalf("want 1 entry, got %d", len(byName))
	}
	e := byName["user-profile"]
	if e == nil {
		t.Fatalf("user-profile entry missing")
	}
	if !e.Pinned {
		t.Errorf("user entry should be pinned")
	}
	if e.Durability != "evergreen" {
		t.Errorf("durability = %q, want evergreen", e.Durability)
	}
	if e.Category != "user" {
		t.Errorf("category = %q, want user", e.Category)
	}
	if e.Content != content {
		t.Errorf("content = %q, want %q", e.Content, content)
	}
	if e.CharCount != memory.CharCount(content) {
		t.Errorf("char_count = %d, want %d", e.CharCount, memory.CharCount(content))
	}
}

func TestMigrateMemoryMDSplitsSections(t *testing.T) {
	ctx := context.Background()
	memoryMD := strings.Join([]string{
		"## Deploy Process",
		"Run make deploy. It pushes to prod.",
		"",
		"## Code Style",
		"Use tabs. Keep lines short.",
		"",
		"### Testing Habits",
		"Always run go test. No exceptions.",
	}, "\n")
	es, dir := migrateFixture(t, "", memoryMD)

	if err := memory.MigrateLegacyFiles(ctx, es, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	byName := entriesByName(t, es)
	if len(byName) != 3 {
		t.Fatalf("want 3 entries, got %d: %v", len(byName), keys(byName))
	}
	for _, want := range []string{"deploy-process", "code-style", "testing-habits"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing entry %q; got %v", want, keys(byName))
		}
	}
	deploy := byName["deploy-process"]
	if deploy.Durability != "volatile" {
		t.Errorf("durability = %q, want volatile", deploy.Durability)
	}
	if deploy.Pinned {
		t.Errorf("memory entry should not be pinned")
	}
	if deploy.Trigger != "Run make deploy" {
		t.Errorf("trigger = %q, want first sentence %q", deploy.Trigger, "Run make deploy")
	}
}

func TestMigrateMemoryMDNoHeadingsSingleEntry(t *testing.T) {
	ctx := context.Background()
	memoryMD := "Just a plain blob of memory with no headings at all.\nSecond line."
	es, dir := migrateFixture(t, "", memoryMD)

	if err := memory.MigrateLegacyFiles(ctx, es, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	byName := entriesByName(t, es)
	if len(byName) != 1 {
		t.Fatalf("want 1 entry, got %d: %v", len(byName), keys(byName))
	}
	e := byName["just-a-plain-blob-of-memory-with-no-headings-at-all"]
	if e == nil {
		// slug is derived from the first line; assert there's exactly one entry
		// of any name.
		for _, only := range byName {
			e = only
		}
	}
	if e.Durability != "volatile" {
		t.Errorf("durability = %q, want volatile", e.Durability)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	es, dir := migrateFixture(t, "user data", "## Sec\nbody here.")

	if err := memory.MigrateLegacyFiles(ctx, es, dir); err != nil {
		t.Fatalf("migrate 1: %v", err)
	}
	first := entriesByName(t, es)

	if err := memory.MigrateLegacyFiles(ctx, es, dir); err != nil {
		t.Fatalf("migrate 2: %v", err)
	}
	second := entriesByName(t, es)

	if len(first) != len(second) {
		t.Fatalf("entry count changed across runs: %d → %d", len(first), len(second))
	}
	for name, e := range second {
		fe := first[name]
		if fe == nil {
			t.Errorf("entry %q appeared on second run (duplicate)", name)
			continue
		}
		if fe.ID != e.ID {
			t.Errorf("entry %q ID changed across runs (re-imported)", name)
		}
	}

	// Marker must exist after the first successful run.
	marker := filepath.Join(dir, "memories", ".migrated_to_entries")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker missing after migration: %v", err)
	}
}

func TestMigrateBacksUpLegacyFiles(t *testing.T) {
	ctx := context.Background()
	userMD := "profile content"
	memoryMD := "## A\nalpha."
	es, dir := migrateFixture(t, userMD, memoryMD)

	if err := memory.MigrateLegacyFiles(ctx, es, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	memDir := filepath.Join(dir, "memories")
	// Originals still present.
	for _, f := range []string{"USER.md", "MEMORY.md"} {
		if _, err := os.Stat(filepath.Join(memDir, f)); err != nil {
			t.Errorf("original %q removed: %v", f, err)
		}
	}
	// Backups present with identical content.
	for f, want := range map[string]string{"USER.md": userMD, "MEMORY.md": memoryMD} {
		got, err := os.ReadFile(filepath.Join(memDir, "legacy", f))
		if err != nil {
			t.Errorf("backup %q missing: %v", f, err)
			continue
		}
		if string(got) != want {
			t.Errorf("backup %q content = %q, want %q", f, got, want)
		}
	}
}

func TestMigrateNoFilesIsNoOp(t *testing.T) {
	ctx := context.Background()
	es, dir := migrateFixture(t, "", "")

	if err := memory.MigrateLegacyFiles(ctx, es, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if n := len(entriesByName(t, es)); n != 0 {
		t.Fatalf("want 0 entries, got %d", n)
	}
	// Marker MUST NOT be written when there was nothing to migrate.
	marker := filepath.Join(dir, "memories", ".migrated_to_entries")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker should not exist for empty migration (err=%v)", err)
	}
}

func TestMigrateDeduplicatesSectionNames(t *testing.T) {
	ctx := context.Background()
	memoryMD := strings.Join([]string{
		"## Notes",
		"first note.",
		"",
		"## Notes",
		"second note.",
	}, "\n")
	es, dir := migrateFixture(t, "", memoryMD)

	if err := memory.MigrateLegacyFiles(ctx, es, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	byName := entriesByName(t, es)
	if len(byName) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(byName), keys(byName))
	}
	if _, ok := byName["notes"]; !ok {
		t.Errorf("missing entry %q", "notes")
	}
	if _, ok := byName["notes-2"]; !ok {
		t.Errorf("missing deduped entry %q; got %v", "notes-2", keys(byName))
	}
}

func keys(m map[string]*memory.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
