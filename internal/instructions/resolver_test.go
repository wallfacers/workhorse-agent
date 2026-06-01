package instructions

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func setupResolverTree(t *testing.T) (root, srcDir, srcFooDir string) {
	t.Helper()
	root = t.TempDir()
	srcDir = filepath.Join(root, "src")
	srcFooDir = filepath.Join(srcDir, "foo")
	os.MkdirAll(srcFooDir, 0o755)
	return root, srcDir, srcFooDir
}

func TestResolve_firstInjectionSucceeds(t *testing.T) {
	root, srcDir, srcFooDir := setupResolverTree(t)
	os.WriteFile(filepath.Join(srcDir, "AGENTS.md"), []byte("src rules"), 0o644)

	r := NewResolver(nil)
	results := r.Resolve(filepath.Join(srcFooDir, "bar.go"), root)
	if len(results) != 1 {
		t.Fatalf("expected 1 injection, got %d", len(results))
	}
	if results[0].Content != "src rules" {
		t.Fatalf("expected 'src rules', got %q", results[0].Content)
	}
}

func TestResolve_duplicateSkipped(t *testing.T) {
	root, srcDir, srcFooDir := setupResolverTree(t)
	os.WriteFile(filepath.Join(srcDir, "AGENTS.md"), []byte("src rules"), 0o644)

	r := NewResolver(nil)
	r.Resolve(filepath.Join(srcFooDir, "bar.go"), root)
	results := r.Resolve(filepath.Join(srcFooDir, "baz.go"), root)
	if len(results) != 0 {
		t.Fatalf("expected 0 injections on second call, got %d", len(results))
	}
}

// Regression: the Read tool passes pathguard.Resolve's symlink-resolved path as
// filePath but the raw (unresolved) workdir as root. When the workdir contains a
// symlink component the two spellings diverge and proximity injection must still
// fire — the resolver resolves the root's symlinks to compare on equal footing.
func TestResolve_symlinkedWorkdirRoot(t *testing.T) {
	realRoot, srcDir, srcFooDir := setupResolverTree(t)
	os.WriteFile(filepath.Join(srcDir, "AGENTS.md"), []byte("src rules"), 0o644)

	// symRoot is a symlink pointing at realRoot, mimicking a project root whose
	// path contains a symlink (macOS /tmp→/private/tmp, or a symlinked checkout).
	symRoot := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, symRoot); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	r := NewResolver(nil)
	// filePath is the real (symlink-resolved) path; root is the symlink spelling.
	results := r.Resolve(filepath.Join(srcFooDir, "bar.go"), symRoot)
	if len(results) != 1 {
		t.Fatalf("expected 1 injection through symlinked root, got %d", len(results))
	}
	if results[0].Content != "src rules" {
		t.Fatalf("expected 'src rules', got %q", results[0].Content)
	}
}

// Regression: a transient read error must release the dedup claim so a later
// Read can retry, rather than permanently suppressing the path. An AGENTS.md
// that is a directory passes os.Stat (findInDir) but fails os.ReadFile.
func TestResolve_readErrorReleasesClaim(t *testing.T) {
	root, srcDir, srcFooDir := setupResolverTree(t)
	agentsAsDir := filepath.Join(srcDir, "AGENTS.md")
	os.MkdirAll(agentsAsDir, 0o755)

	r := NewResolver(nil)
	results := r.Resolve(filepath.Join(srcFooDir, "bar.go"), root)
	if len(results) != 0 {
		t.Fatalf("expected 0 injections on read error, got %d", len(results))
	}

	r.mu.Lock()
	claimed := r.injectedPaths[agentsAsDir]
	r.mu.Unlock()
	if claimed {
		t.Fatal("read error must release the claim so a later Read can retry")
	}
}

func TestResolve_systemLevelPathSkipped(t *testing.T) {
	root, srcDir, srcFooDir := setupResolverTree(t)
	srcAgents := filepath.Join(srcDir, "AGENTS.md")
	os.WriteFile(srcAgents, []byte("src rules"), 0o644)

	r := NewResolver(&Snapshot{
		Files: []File{{Path: srcAgents, Content: "src rules"}},
	})
	results := r.Resolve(filepath.Join(srcFooDir, "bar.go"), root)
	if len(results) != 0 {
		t.Fatalf("expected 0 injections (system path), got %d", len(results))
	}
}

func TestResolve_workdirRootNoInjection(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "README.md"), []byte("hello"), 0o644)

	r := NewResolver(nil)
	results := r.Resolve(filepath.Join(root, "README.md"), root)
	if len(results) != 0 {
		t.Fatalf("expected 0 injections at workdir root, got %d", len(results))
	}
}

func TestResolve_concurrentDedup(t *testing.T) {
	root, srcDir, srcFooDir := setupResolverTree(t)
	os.WriteFile(filepath.Join(srcDir, "AGENTS.md"), []byte("src rules"), 0o644)

	r := NewResolver(nil)
	var wg sync.WaitGroup
	var totalInjections int
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results := r.Resolve(filepath.Join(srcFooDir, "bar.go"), root)
			mu.Lock()
			totalInjections += len(results)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if totalInjections != 1 {
		t.Fatalf("expected exactly 1 injection across 10 concurrent calls, got %d", totalInjections)
	}
}

func TestFormatInjection(t *testing.T) {
	got := FormatInjection(Injection{Path: "/foo/AGENTS.md", Content: "hello"})
	want := "<system-reminder>\nInstructions from: /foo/AGENTS.md\nhello\n</system-reminder>"
	if got != want {
		t.Fatalf("expected:\n%s\ngot:\n%s", want, got)
	}
}
