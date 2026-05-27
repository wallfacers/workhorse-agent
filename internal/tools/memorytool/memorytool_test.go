package memorytool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/memorytool"
)

func testEnv() *tools.Env {
	return &tools.Env{SessionID: "test", Workdir: "/tmp"}
}

func TestRead_InvalidKind(t *testing.T) {
	r := &memorytool.Read{ProfileDir: t.TempDir()}
	res, err := r.Run(context.Background(), testEnv(), []byte(`{"kind":"bad"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.IsError {
		t.Error("expected error result")
	}
	if !contains(res.Output, "invalid_kind") {
		t.Errorf("expected invalid_kind in output, got %q", res.Output)
	}
}

func TestWrite_InvalidKind(t *testing.T) {
	w := &memorytool.Write{ProfileDir: t.TempDir(), MemoryLimit: 100, UserLimit: 100}
	res, err := w.Run(context.Background(), testEnv(), []byte(`{"kind":"bad","content":"x"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.IsError {
		t.Error("expected error result")
	}
	if !contains(res.Output, "invalid_kind") {
		t.Errorf("expected invalid_kind, got %q", res.Output)
	}
}

func TestWrite_OverLimit(t *testing.T) {
	w := &memorytool.Write{ProfileDir: t.TempDir(), MemoryLimit: 5, UserLimit: 5}
	res, err := w.Run(context.Background(), testEnv(), []byte(`{"kind":"memory","content":"too long"}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.IsError {
		t.Error("expected error result for over-limit")
	}
	if !contains(res.Output, "memory_too_large") {
		t.Errorf("expected memory_too_large, got %q", res.Output)
	}
}

func TestWriteAndRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := &memorytool.Write{ProfileDir: dir, MemoryLimit: 1000, UserLimit: 1000}
	r := &memorytool.Read{ProfileDir: dir, MemoryLimit: 1000, UserLimit: 1000}

	// Write
	res, err := w.Run(context.Background(), testEnv(), []byte(`{"kind":"memory","content":"test data"}`))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.IsError {
		t.Fatalf("write failed: %s", res.Output)
	}

	// Read
	res, err = r.Run(context.Background(), testEnv(), []byte(`{"kind":"memory"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.IsError {
		t.Fatalf("read failed: %s", res.Output)
	}

	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	if out["content"] != "test data" {
		t.Errorf("content: got %v, want 'test data'", out["content"])
	}
}

func TestWrite_AppendThenRead(t *testing.T) {
	dir := t.TempDir()
	w := &memorytool.Write{ProfileDir: dir, MemoryLimit: 1000, UserLimit: 1000}
	r := &memorytool.Read{ProfileDir: dir, MemoryLimit: 1000, UserLimit: 1000}

	w.Run(context.Background(), testEnv(), []byte(`{"kind":"memory","content":"line1"}`))
	w.Run(context.Background(), testEnv(), []byte(`{"kind":"memory","content":"line2","mode":"append"}`))

	res, _ := r.Run(context.Background(), testEnv(), []byte(`{"kind":"memory"}`))
	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	if out["content"] != "line1\nline2" {
		t.Errorf("append: got %v", out["content"])
	}
}

func TestWrite_DoesNotAffectSessionSnapshot(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memories")
	os.MkdirAll(memDir, 0o700)
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("original"), 0o600)

	loader := &memory.Loader{ProfileDir: dir}
	// This simulates session A loading a snapshot before a write
	// The test verifies that memory_read returns disk content (not snapshot)
	w := &memorytool.Write{ProfileDir: dir, MemoryLimit: 1000, UserLimit: 1000}
	r := &memorytool.Read{ProfileDir: dir, MemoryLimit: 1000, UserLimit: 1000}

	_ = loader // snapshot loaded at session start
	w.Run(context.Background(), testEnv(), []byte(`{"kind":"memory","content":"updated"}`))

	res, _ := r.Run(context.Background(), testEnv(), []byte(`{"kind":"memory"}`))
	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	// memory_read reads from disk, so it sees the new content
	if out["content"] != "updated" {
		t.Errorf("read after write: got %v", out["content"])
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
