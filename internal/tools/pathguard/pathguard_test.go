package pathguard_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

func setupWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// EvalSymlinks needs the directory to exist; on macOS the tmp prefix is a
	// symlink itself, which is fine because we resolve it.
	return dir
}

func TestResolve_HappyPath(t *testing.T) {
	wd := setupWorkdir(t)
	target := filepath.Join(wd, "a.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := pathguard.Resolve(wd, "a.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Resolve must return absolute path, got %q", got)
	}
}

func TestResolve_RejectsTraversal(t *testing.T) {
	wd := setupWorkdir(t)
	_, err := pathguard.Resolve(wd, "../etc/passwd")
	if !errors.Is(err, pathguard.ErrPathEscape) {
		// May also be ENOENT depending on platform; the important thing is
		// that we don't return success.
		if err == nil {
			t.Fatal("expected error for traversal, got nil")
		}
		t.Logf("got non-Escape error (acceptable): %v", err)
	}
}

func TestResolve_RejectsSymlinkEscape(t *testing.T) {
	wd := setupWorkdir(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wd, "link")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, err := pathguard.Resolve(wd, "link")
	if !errors.Is(err, pathguard.ErrPathEscape) {
		t.Errorf("symlink to outside file should be rejected, got %v", err)
	}
}

func TestResolveForWrite_AllowsMissingLeaf(t *testing.T) {
	wd := setupWorkdir(t)
	got, err := pathguard.ResolveForWrite(wd, "new.txt")
	if err != nil {
		t.Fatalf("ResolveForWrite: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveForWrite must return abs path, got %q", got)
	}
}

func TestResolveForWrite_RejectsParentEscape(t *testing.T) {
	wd := setupWorkdir(t)
	_, err := pathguard.ResolveForWrite(wd, "../escape.txt")
	if !errors.Is(err, pathguard.ErrPathEscape) {
		// Some platforms may not resolve the parent; accept any non-nil.
		if err == nil {
			t.Fatal("expected error for write traversal, got nil")
		}
	}
}

func TestResolveForWrite_RejectsSymlinkedParent(t *testing.T) {
	wd := setupWorkdir(t)
	outside := t.TempDir()
	subDir := filepath.Join(outside, "inside")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wd, "links")
	if err := os.Symlink(subDir, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	_, err := pathguard.ResolveForWrite(wd, "links/new.txt")
	if !errors.Is(err, pathguard.ErrPathEscape) {
		t.Errorf("write under symlinked-out directory must be rejected, got %v", err)
	}
}

func TestOpenRead_NoFollow(t *testing.T) {
	wd := setupWorkdir(t)
	file := filepath.Join(wd, "real.txt")
	if err := os.WriteFile(file, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wd, "ln")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	// OpenRead via the symlink path should fail on platforms with
	// O_NOFOLLOW. We use the raw path here (Resolve would have resolved it).
	if _, err := pathguard.OpenRead(link); err == nil {
		// On platforms without O_NOFOLLOW the package falls back to Lstat,
		// which we verify reports the symlink.
		t.Errorf("OpenRead on a symlink leaf should fail")
	}
}
