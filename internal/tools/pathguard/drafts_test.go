package pathguard_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

func TestDraftsDir_JoinsCorrectly(t *testing.T) {
	got := pathguard.DraftsDir("/tmp/ext")
	want := filepath.Join("/tmp/ext", ".drafts")
	if got != want {
		t.Errorf("DraftsDir: got %q, want %q", got, want)
	}
}

func TestResolveDraftForWrite_AcceptsFlatYAML(t *testing.T) {
	ext := t.TempDir()
	if err := os.MkdirAll(pathguard.DraftsDir(ext), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := pathguard.ResolveDraftForWrite(ext, "gemini.yaml")
	if err != nil {
		t.Fatalf("ResolveDraftForWrite: %v", err)
	}
	if filepath.Base(got) != "gemini.yaml" {
		t.Errorf("base: got %q, want gemini.yaml", filepath.Base(got))
	}
}

func TestResolveDraftForWrite_RejectsNestedSubdir(t *testing.T) {
	ext := t.TempDir()
	drafts := pathguard.DraftsDir(ext)
	if err := os.MkdirAll(filepath.Join(drafts, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := pathguard.ResolveDraftForWrite(ext, "sub/gemini.yaml")
	if !errors.Is(err, pathguard.ErrPathEscape) {
		t.Errorf("nested path should be rejected with ErrPathEscape, got %v", err)
	}
}

func TestResolveDraftForWrite_RejectsTraversal(t *testing.T) {
	ext := t.TempDir()
	if err := os.MkdirAll(pathguard.DraftsDir(ext), 0o700); err != nil {
		t.Fatal(err)
	}
	// Path traversal where the resolved-parent exists: must be rejected with
	// ErrPathEscape because the resulting absolute path falls outside drafts/.
	for _, p := range []string{
		"../escape.yaml",
		filepath.Join(ext, "live.yaml"), // absolute path into the live dir
	} {
		_, err := pathguard.ResolveDraftForWrite(ext, p)
		if !errors.Is(err, pathguard.ErrPathEscape) {
			t.Errorf("path %q should be rejected with ErrPathEscape, got %v", p, err)
		}
	}
	// Path traversal where the parent itself does not exist: the rejection is
	// a resolve error, not an escape error. Either way the caller cannot write.
	if _, err := pathguard.ResolveDraftForWrite(ext, "../../doesnotexist/x.yaml"); err == nil {
		t.Error("deeply non-existent parent should be rejected")
	}
}

func TestResolveDraftForWrite_RejectsSymlinkEscape(t *testing.T) {
	ext := t.TempDir()
	drafts := pathguard.DraftsDir(ext)
	if err := os.MkdirAll(drafts, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(drafts, "evil.yaml")
	if err := os.Symlink(filepath.Join(outside, "target.yaml"), link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	_, err := pathguard.ResolveDraftForWrite(ext, "evil.yaml")
	if !errors.Is(err, pathguard.ErrPathEscape) {
		t.Errorf("symlink target outside drafts should be rejected, got %v", err)
	}
}

func TestResolveDraft_RequiresExistence(t *testing.T) {
	ext := t.TempDir()
	if err := os.MkdirAll(pathguard.DraftsDir(ext), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := pathguard.ResolveDraft(ext, "missing.yaml")
	if err == nil {
		t.Error("expected error for missing draft")
	}
	// The error should NOT be ErrPathEscape — it's a not-found error.
	if errors.Is(err, pathguard.ErrPathEscape) {
		t.Errorf("missing file should produce not-found, got escape: %v", err)
	}
}
