package pathscan_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent/pathscan"
)

func TestAllowlist_Default(t *testing.T) {
	al := pathscan.Allowlist(nil, nil)
	if len(al) == 0 {
		t.Error("default allowlist should not be empty")
	}
}

func TestAllowlist_Extra(t *testing.T) {
	al := pathscan.Allowlist([]string{"poetry"}, nil)
	found := false
	for _, name := range al {
		if name == "poetry" {
			found = true
		}
	}
	if !found {
		t.Error("extra entry should be in allowlist")
	}
}

func TestAllowlist_Disabled(t *testing.T) {
	al := pathscan.Allowlist(nil, []string{"docker"})
	for _, name := range al {
		if name == "docker" {
			t.Error("docker should be suppressed")
		}
	}
}

func TestAllowlist_DisabledWinsOverExtra(t *testing.T) {
	al := pathscan.Allowlist([]string{"docker"}, []string{"docker"})
	for _, name := range al {
		if name == "docker" {
			t.Error("disabled should win over extra")
		}
	}
}

func TestScan_FindsGit(t *testing.T) {
	al := pathscan.Allowlist([]string{"git"}, nil)
	entries := pathscan.Scan(slog.Default(), al)
	if len(entries) == 0 {
		t.Skip("git not installed")
	}
	found := false
	for _, e := range entries {
		if e.Name == "git" {
			found = true
			if e.Version == "" {
				t.Log("git version not detected (tolerated)")
			}
		}
	}
	if !found {
		t.Error("git should be found")
	}
}

func TestCache_WriteAndLoad(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "pathscan.json")

	cf := &pathscan.CacheFile{
		ScannedAt:           time.Now(),
		ExtraFingerprint:    "a",
		DisabledFingerprint: "b",
		Entries: []pathscan.Entry{
			{Name: "git", Path: "/usr/bin/git", Version: "2.43.0"},
		},
	}
	if err := pathscan.WriteCache(cachePath, cf); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := pathscan.LoadCache(cachePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded == nil {
		t.Fatal("cache should load")
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Name != "git" {
		t.Errorf("entries: %v", loaded.Entries)
	}
}

func TestCache_Missing(t *testing.T) {
	loaded, err := pathscan.LoadCache(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("missing cache should not error: %v", err)
	}
	if loaded != nil {
		t.Error("missing cache should return nil")
	}
}

func TestCache_Malformed(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "pathscan.json")
	if err := os.WriteFile(cachePath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := pathscan.LoadCache(cachePath)
	if err != nil {
		t.Fatalf("malformed cache should not error: %v", err)
	}
	if loaded != nil {
		t.Error("malformed cache should return nil")
	}
}

func TestCachedScan_CacheHit(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "pathscan.json")

	// Write a valid cache.
	cf := &pathscan.CacheFile{
		ScannedAt:           time.Now(),
		ExtraFingerprint:    "",
		DisabledFingerprint: "",
		Entries: []pathscan.Entry{
			{Name: "fake-tool", Path: "/usr/bin/fake-tool", Version: "1.0"},
		},
	}
	if err := pathscan.WriteCache(cachePath, cf); err != nil {
		t.Fatal(err)
	}

	entries := pathscan.CachedScan(slog.Default(), cachePath, 24*time.Hour, nil, nil)
	if len(entries) != 1 || entries[0].Name != "fake-tool" {
		t.Errorf("cache hit should return cached entry, got %v", entries)
	}
}

func TestCachedScan_TTLExpired(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "pathscan.json")

	cf := &pathscan.CacheFile{
		ScannedAt:           time.Now().Add(-48 * time.Hour), // expired
		ExtraFingerprint:    "",
		DisabledFingerprint: "",
		Entries: []pathscan.Entry{
			{Name: "fake-tool", Path: "/usr/bin/fake-tool"},
		},
	}
	if err := pathscan.WriteCache(cachePath, cf); err != nil {
		t.Fatal(err)
	}

	entries := pathscan.CachedScan(slog.Default(), cachePath, 24*time.Hour, nil, nil)
	// Should re-scan. fake-tool won't be on PATH, so we won't find it.
	for _, e := range entries {
		if e.Name == "fake-tool" {
			t.Error("expired cache should trigger re-scan")
		}
	}
}

func TestCachedScan_FingerprintMismatch(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "pathscan.json")

	cf := &pathscan.CacheFile{
		ScannedAt:           time.Now(),
		ExtraFingerprint:    "old",
		DisabledFingerprint: "",
		Entries: []pathscan.Entry{
			{Name: "fake-tool", Path: "/usr/bin/fake-tool"},
		},
	}
	if err := pathscan.WriteCache(cachePath, cf); err != nil {
		t.Fatal(err)
	}

	// Pass different extra — fingerprint won't match.
	entries := pathscan.CachedScan(slog.Default(), cachePath, 24*time.Hour, []string{"new-extra"}, nil)
	for _, e := range entries {
		if e.Name == "fake-tool" {
			t.Error("fingerprint mismatch should trigger re-scan")
		}
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "pathscan.json")

	cf := &pathscan.CacheFile{
		ScannedAt: time.Now(),
		Entries:   []pathscan.Entry{},
	}
	if err := pathscan.WriteCache(cachePath, cf); err != nil {
		t.Fatal(err)
	}
	// Verify no .tmp file left behind.
	if _, err := os.Stat(cachePath + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should be cleaned up after rename")
	}
	// Verify the cache is valid JSON.
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Error("cache file should be valid JSON")
	}
}

func TestFormatVersion(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"git version 2.43.0", "2.43.0"},
		{"1.2.3", "1.2.3"},
		{"v14.21.0", "v14.21.0"},
		{"some random text", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got := pathscan.FormatVersion(tc.in)
		if got != tc.want {
			t.Errorf("FormatVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
