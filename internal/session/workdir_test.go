package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateWorkdir_EmptyFallsBackToHome(t *testing.T) {
	got, err := ValidateWorkdir("")
	if err != nil {
		t.Fatalf("empty workdir: %v", err)
	}
	want, _ := os.UserHomeDir()
	if want != "" && got != want {
		t.Fatalf("expected home %q, got %q", want, got)
	}
}

func TestValidateWorkdir_RelativePathRejected(t *testing.T) {
	_, err := ValidateWorkdir("relative/path")
	if err == nil {
		t.Fatal("relative path should be rejected")
	}
}

func TestValidateWorkdir_RootRejected(t *testing.T) {
	_, err := ValidateWorkdir("/")
	if err == nil {
		t.Fatal("/ should be rejected")
	}
}

func TestValidateWorkdir_TraversalsToRootRejected(t *testing.T) {
	_, err := ValidateWorkdir("/tmp/..")
	if err == nil {
		t.Fatal("/tmp/.. (cleaned to /) should be rejected")
	}
}

func TestValidateWorkdir_NonExistentAbsolutePath(t *testing.T) {
	got, err := ValidateWorkdir("/nonexistent_dir_for_test")
	if err != nil {
		t.Fatalf("non-existent absolute path should be accepted: %v", err)
	}
	want := "/nonexistent_dir_for_test"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestValidateWorkdir_ExistingDir(t *testing.T) {
	dir := t.TempDir()
	got, err := ValidateWorkdir(dir)
	if err != nil {
		t.Fatalf("existing dir should pass: %v", err)
	}
	// Should be resolved (may differ on macOS /tmp → /private/tmp).
	want, _ := filepath.EvalSymlinks(dir)
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestValidateWorkdir_SymlinkToRootRejected(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "rootlink")
	if err := os.Symlink("/", link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	_, err := ValidateWorkdir(link)
	if err == nil {
		t.Fatal("symlink to / should be rejected")
	}
}

func TestAssertWorkdirWithin_SameDir(t *testing.T) {
	if err := AssertWorkdirWithin("/home/user/project", "/home/user/project"); err != nil {
		t.Fatalf("same dir should pass: %v", err)
	}
}

func TestAssertWorkdirWithin_Subdirectory(t *testing.T) {
	if err := AssertWorkdirWithin("/home/user/project", "/home/user/project/src"); err != nil {
		t.Fatalf("subdirectory should pass: %v", err)
	}
}

func TestAssertWorkdirWithin_ParentRejected(t *testing.T) {
	err := AssertWorkdirWithin("/home/user/project", "/home/user")
	if err == nil {
		t.Fatal("parent directory should be rejected")
	}
}

func TestAssertWorkdirWithin_UnrelatedRejected(t *testing.T) {
	err := AssertWorkdirWithin("/home/user/project", "/tmp")
	if err == nil {
		t.Fatal("unrelated directory should be rejected")
	}
}

func TestAssertWorkdirWithin_RootEscalationRejected(t *testing.T) {
	err := AssertWorkdirWithin("/home/user/project", "/")
	if err == nil {
		t.Fatal("/ should be rejected")
	}
}
