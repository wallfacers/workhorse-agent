package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/config"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/builtin"
)

// mkdirAll-aware write. Helps tests build nested fixtures inline.
func mustWritePath(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeRepo wires the workdir as a git repo root and returns a Grep
// configured with respect_gitignore=true. The result is sorted (path, line).
func fakeRepo(t *testing.T) (*tools.Env, builtin.Grep) {
	t.Helper()
	e := env(t)
	mustMkdir(t, filepath.Join(e.Workdir, ".git"))
	g := builtin.Grep{
		Cfg: config.ToolsGrep{
			Workers:          2,
			RespectGitignore: true,
		},
	}
	return e, g
}

// Spec scenario: gitignore + default excludes + binary skip + nested negation.
func TestGrep_GitignoreMonorepo(t *testing.T) {
	e, g := fakeRepo(t)
	w := e.Workdir

	// Root .gitignore: ignore all .log and ignore node_modules redundantly.
	mustWritePath(t, w, ".gitignore", "*.log\nnode_modules/\n")

	// Source files we want to match.
	mustWritePath(t, w, "main.go", "package main\n// TODO real impl\nfunc Foo() {}\n")
	mustWritePath(t, w, "src/util.go", "// TODO refactor util\nfunc Util() {}\n")

	// Nested .gitignore re-includes one specific log under src/.
	mustWritePath(t, w, "src/.gitignore", "!important.log\n")
	mustWritePath(t, w, "src/important.log", "TODO must check this log\n")
	mustWritePath(t, w, "src/noise.log", "TODO must NOT appear\n")

	// Default-excluded paths.
	mustWritePath(t, w, "node_modules/dep/index.js", "// TODO from node_modules — must NOT appear\n")
	mustWritePath(t, w, "dist/built.js", "// TODO from dist — must NOT appear\n")
	mustWritePath(t, w, "yarn.lock", "TODO from lockfile — must NOT appear\n")

	// Hard VCS sanity: a fake .git/info/foo with the keyword.
	mustWritePath(t, w, ".git/objects/junk", "TODO from .git — must NOT appear\n")

	// Binary file: NUL in first byte should silently skip.
	mustWritePath(t, w, "data.bin", "TODO header\n\x00\x00\x00")

	res, _ := g.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
	if res.IsError {
		t.Fatalf("grep errored: %s", res.Output)
	}

	want := []string{
		"main.go:",
		"src/important.log:",
		"src/util.go:",
	}
	for _, fragment := range want {
		if !strings.Contains(res.Output, fragment) {
			t.Errorf("expected %q in output, got:\n%s", fragment, res.Output)
		}
	}
	mustNotContain := []string{
		"noise.log",    // excluded by *.log
		"node_modules", // default-excluded
		"dist/",        // default-excluded
		"yarn.lock",    // matches *.lock default-exclude
		".git/",        // hard VCS skip
		"data.bin",     // binary sniff
	}
	for _, frag := range mustNotContain {
		if strings.Contains(res.Output, frag) {
			t.Errorf("output leaks %q:\n%s", frag, res.Output)
		}
	}
}

// Spec scenario: 并行执行输出可复现 (race detector friendly).
func TestGrep_DeterministicOutput(t *testing.T) {
	e, g := fakeRepo(t)
	// Bump workers so parallel completion order matters.
	g.Cfg.Workers = 8

	for i := 0; i < 50; i++ {
		mustWritePath(t, e.Workdir, fmt.Sprintf("src/f%02d.go", i),
			"// TODO line one\n// TODO line two\n")
	}

	first, _ := g.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
	if first.IsError {
		t.Fatalf("first run errored: %s", first.Output)
	}
	for i := 0; i < 20; i++ {
		got, _ := g.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
		if got.Output != first.Output {
			t.Fatalf("run %d diverged from run 0\nfirst:\n%s\ngot:\n%s", i, first.Output, got.Output)
		}
	}
}

// Spec scenario: max_hits 早停.
func TestGrep_MaxHitsEarlyStop(t *testing.T) {
	e, g := fakeRepo(t)
	// 300 files × 5 matches each = 1500 candidate hits.
	for i := 0; i < 300; i++ {
		mustWritePath(t, e.Workdir, fmt.Sprintf("f%03d.go", i),
			"TODO 1\nTODO 2\nTODO 3\nTODO 4\nTODO 5\n")
	}
	res, _ := g.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO","max_hits":10}`))
	if res.IsError {
		t.Fatalf("grep errored: %s", res.Output)
	}
	lines := strings.Split(strings.TrimRight(res.Output, "\n"), "\n")
	if len(lines) > 10 {
		t.Errorf("max_hits=10 not honored, got %d lines:\n%s", len(lines), res.Output)
	}
}

// Spec scenario: ignore_vcs=false + default_excludes=["never_matches"] disables
// all default behaviors, exposing what used to be hidden.
func TestGrep_DegradationToggles(t *testing.T) {
	e := env(t)
	mustMkdir(t, filepath.Join(e.Workdir, ".git"))
	mustWritePath(t, e.Workdir, ".gitignore", "*.log\n")
	mustWritePath(t, e.Workdir, "ignored.log", "TODO ignored\n")
	mustWritePath(t, e.Workdir, "node_modules/dep/main.go", "TODO from node_modules\n")

	// Default behavior: both should be excluded.
	gDefault := builtin.Grep{Cfg: config.ToolsGrep{Workers: 2, RespectGitignore: true}}
	res, _ := gDefault.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
	if strings.Contains(res.Output, "ignored.log") || strings.Contains(res.Output, "node_modules") {
		t.Errorf("default behavior should hide both, got:\n%s", res.Output)
	}

	// ignore_vcs=false alone: still has default_excludes; node_modules still hidden,
	// but ignored.log NOW appears (no longer .gitignore-excluded).
	res, _ = gDefault.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO","ignore_vcs":false}`))
	if !strings.Contains(res.Output, "ignored.log") {
		t.Errorf("ignore_vcs=false should expose ignored.log, got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "node_modules") {
		t.Errorf("default_excludes still active should hide node_modules, got:\n%s", res.Output)
	}

	// Both off: default_excludes replaced with an inert sentinel + ignore_vcs=false.
	gOff := builtin.Grep{Cfg: config.ToolsGrep{
		Workers:          2,
		RespectGitignore: false,
		DefaultExcludes:  []string{"__never_matches_anything__"},
	}}
	res, _ = gOff.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
	if !strings.Contains(res.Output, "ignored.log") {
		t.Errorf("with everything off, ignored.log must appear, got:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "node_modules") {
		t.Errorf("with everything off, node_modules must appear, got:\n%s", res.Output)
	}
}

// Spec scenario: workers=1 walks the serial codepath but produces identical
// output to parallel.
func TestGrep_SerialMatchesParallel(t *testing.T) {
	e, gPar := fakeRepo(t)
	gPar.Cfg.Workers = 8

	mustWritePath(t, e.Workdir, ".gitignore", "*.log\n")
	for i := 0; i < 20; i++ {
		mustWritePath(t, e.Workdir, fmt.Sprintf("src/f%02d.go", i),
			"// TODO one\n// TODO two\n")
		mustWritePath(t, e.Workdir, fmt.Sprintf("src/f%02d.log", i), "TODO ignored\n")
	}
	mustWritePath(t, e.Workdir, "node_modules/dep/x.js", "TODO from nm\n")

	gSer := gPar
	gSer.Cfg.Workers = 1

	parRes, _ := gPar.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
	serRes, _ := gSer.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
	if parRes.Output != serRes.Output {
		t.Fatalf("workers=8 vs workers=1 diverged\nparallel:\n%s\nserial:\n%s", parRes.Output, serRes.Output)
	}
}

// Benchmark: realistic-ish small monorepo (200 files, 10 lines each).
func BenchmarkGrep_SmallMonorepo(b *testing.B) {
	tmp := b.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".gitignore"), []byte("*.log\nbuild/\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		dir := filepath.Join(tmp, fmt.Sprintf("pkg%02d", i/20))
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.go", i)),
			[]byte("package x\n// TODO line\nfunc F() {}\n// TODO 2\n"), 0o600)
	}
	e := &tools.Env{Workdir: tmp}
	g := builtin.Grep{Cfg: config.ToolsGrep{Workers: 4, RespectGitignore: true}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, _ := g.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
		if res.IsError {
			b.Fatal(res.Output)
		}
	}
}

// buildMonorepoFixture spreads `n` small Go files across ~20-file packages so
// that directory fanout stays sane at 20k files. Half the files contain a
// matchable TODO; the other half are filler so the scan does real work.
func buildMonorepoFixture(b *testing.B, root string, n int) {
	b.Helper()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\nbuild/\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("pkg%04d", i/20))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		body := "package x\n// TODO line\nfunc F() {}\n// TODO 2\n"
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.go", i)), []byte(body), 0o600); err != nil {
			b.Fatal(err)
		}
	}
}

// Scaling matrix: (files × workers). Each (files=N) row reuses one tempdir so
// the 20k-file setup cost isn't paid per worker variant.
func BenchmarkGrep_Scaling(b *testing.B) {
	workerSet := map[int]struct{}{1: {}, 2: {}, 4: {}, 8: {}, runtime.NumCPU(): {}}
	workers := make([]int, 0, len(workerSet))
	for w := range workerSet {
		workers = append(workers, w)
	}
	sort.Ints(workers)

	for _, files := range []int{200, 2000, 20000} {
		tmp := b.TempDir()
		buildMonorepoFixture(b, tmp, files)
		e := &tools.Env{Workdir: tmp}

		for _, w := range workers {
			name := fmt.Sprintf("files=%d/workers=%d", files, w)
			b.Run(name, func(b *testing.B) {
				g := builtin.Grep{Cfg: config.ToolsGrep{Workers: w, RespectGitignore: true}}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					res, _ := g.Run(context.Background(), e, json.RawMessage(`{"pattern":"TODO"}`))
					if res.IsError {
						b.Fatal(res.Output)
					}
				}
			})
		}
	}
}
