package memory_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

func TestSnapshot_ImmutableAfterDiskChange(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memories")
	os.MkdirAll(memDir, 0o700)
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("original content"), 0o600)
	os.WriteFile(filepath.Join(memDir, "USER.md"), []byte("user original"), 0o600)

	loader := &memory.Loader{ProfileDir: dir}

	// Session A loads snapshot
	snapA, err := loader.Load()
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	if snapA.MemoryMD != "original content" {
		t.Errorf("session A memory: got %q", snapA.MemoryMD)
	}

	// Disk changes after session A loaded
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("updated content"), 0o600)

	// Session A's snapshot should be unchanged
	if snapA.MemoryMD != "original content" {
		t.Errorf("session A snapshot mutated: got %q, want %q", snapA.MemoryMD, "original content")
	}

	// Session B started after the change sees new content
	snapB, err := loader.Load()
	if err != nil {
		t.Fatalf("load B: %v", err)
	}
	if snapB.MemoryMD != "updated content" {
		t.Errorf("session B memory: got %q, want %q", snapB.MemoryMD, "updated content")
	}

	// Session A still unchanged
	if snapA.MemoryMD != "original content" {
		t.Errorf("session A snapshot should still be original: got %q", snapA.MemoryMD)
	}
}
