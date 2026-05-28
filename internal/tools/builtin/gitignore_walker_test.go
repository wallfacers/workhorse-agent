package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindRepoRoot_finds_nearest_ancestor(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "outer", "repo")
	deep := filepath.Join(repo, "sub", "deeper")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findRepoRoot(deep); got != repo {
		t.Errorf("findRepoRoot: got %q, want %q", got, repo)
	}
}

func TestFindRepoRoot_returns_empty_when_no_ancestor(t *testing.T) {
	tmp := t.TempDir()
	leaf := filepath.Join(tmp, "no", "git", "here")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	got := findRepoRoot(leaf)
	// Either "" (clean) or some unrelated ancestor that does happen to have
	// a .git dir (test running inside this repo). Accept either: "" is the
	// strict expectation; if we found something, it must not be inside tmp.
	if got == "" {
		return
	}
	rel, err := filepath.Rel(tmp, got)
	if err == nil && !filepath.IsAbs(rel) && rel != ".." && !startsWith(rel, "..") {
		t.Errorf("findRepoRoot leaked into tmp: got %q", got)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestGitignoreStack_basic_push_pop(t *testing.T) {
	var s gitignoreStack
	if s.IsIgnored([]string{"foo.txt"}, false) {
		t.Error("empty stack must not ignore anything")
	}
	s.push(nil, []byte("foo.txt\n"))
	if !s.IsIgnored([]string{"foo.txt"}, false) {
		t.Error("foo.txt should be ignored after push")
	}
	s.pop()
	if s.IsIgnored([]string{"foo.txt"}, false) {
		t.Error("pop should clear the pattern")
	}
}

// sabhiram bug #21: '*' must NOT cross '/'. go-git is correct here.
func TestGitignoreStack_star_does_not_cross_slash(t *testing.T) {
	var s gitignoreStack
	s.push(nil, []byte("*.go\n"))
	if !s.IsIgnored([]string{"foo.go"}, false) {
		t.Error("foo.go at root should be ignored by *.go")
	}
	if !s.IsIgnored([]string{"sub", "foo.go"}, false) {
		// Per gitignore semantics, a pattern with no slash matches anywhere.
		// "*.go" should still match sub/foo.go via implicit "**/" prefix.
		t.Error("sub/foo.go should be ignored by *.go (basename match)")
	}

	// But a pattern WITH a slash anchors the location.
	var s2 gitignoreStack
	s2.push(nil, []byte("/foo.go\n"))
	if !s2.IsIgnored([]string{"foo.go"}, false) {
		t.Error("foo.go at root should be ignored by /foo.go")
	}
	if s2.IsIgnored([]string{"sub", "foo.go"}, false) {
		t.Error("sub/foo.go must NOT be ignored by /foo.go (anchored to root)")
	}
}

// sabhiram bug #20: '?' must match exactly one character.
func TestGitignoreStack_question_mark_one_char(t *testing.T) {
	var s gitignoreStack
	s.push(nil, []byte("vul?ano\n"))
	if !s.IsIgnored([]string{"vulkano"}, false) {
		t.Error("vulkano should be ignored by vul?ano")
	}
	if s.IsIgnored([]string{"vulcaano"}, false) {
		t.Error("vulcaano (two chars where ? sits) must NOT match vul?ano")
	}
}

func TestGitignoreStack_negation_reincludes(t *testing.T) {
	var s gitignoreStack
	s.push(nil, []byte("*.log\n!important.log\n"))
	if !s.IsIgnored([]string{"debug.log"}, false) {
		t.Error("debug.log should be ignored")
	}
	if s.IsIgnored([]string{"important.log"}, false) {
		t.Error("important.log should be re-included by !important.log")
	}
}

func TestGitignoreStack_dir_only_pattern(t *testing.T) {
	var s gitignoreStack
	s.push(nil, []byte("build/\n"))
	if !s.IsIgnored([]string{"build"}, true) {
		t.Error("build/ (dir) should be ignored")
	}
	if s.IsIgnored([]string{"build"}, false) {
		t.Error("build (file, not dir) must NOT match build/")
	}
}

func TestGitignoreStack_nested_inheritance(t *testing.T) {
	var s gitignoreStack
	// Repo root .gitignore says ignore *.log
	s.push(nil, []byte("*.log\n"))
	// Subdir .gitignore re-includes its own debug.log
	s.push([]string{"sub"}, []byte("!debug.log\n"))

	if !s.IsIgnored([]string{"foo.log"}, false) {
		t.Error("foo.log at root should be ignored by outer *.log")
	}
	if s.IsIgnored([]string{"sub", "debug.log"}, false) {
		t.Error("sub/debug.log should be re-included by inner !debug.log")
	}
	if !s.IsIgnored([]string{"sub", "other.log"}, false) {
		t.Error("sub/other.log should still be ignored by outer *.log")
	}
}

func TestIsHardVCSDir(t *testing.T) {
	cases := map[string]bool{
		".git":         true,
		".hg":          true,
		".svn":         true,
		"git":          false,
		".github":      false,
		"node_modules": false,
		".gitignore":   false,
	}
	for name, want := range cases {
		if got := isHardVCSDir(name); got != want {
			t.Errorf("isHardVCSDir(%q): got %v, want %v", name, got, want)
		}
	}
}

func TestMatchExclude_builtin_defaults(t *testing.T) {
	hits := []string{
		"node_modules", "vendor", "dist", "build", "target",
		".next", ".cache", ".venv", ".gradle", ".mypy_cache",
		"coverage", "htmlcov", "__snapshots__",
		"Gemfile.lock", "Cargo.lock", "poetry.lock", "yarn.lock",
		"package-lock.json", "pnpm-lock.yaml",
		"vendor.min.js", "site.min.css", ".DS_Store",
	}
	for _, name := range hits {
		if !matchExclude(name, builtinDefaultExcludes) {
			t.Errorf("expected %q to be in builtinDefaultExcludes", name)
		}
	}

	misses := []string{
		"main.go", "README.md", "src", "internal", "package.json",
		"node_module", // close to but not the dir
		"my.log",
		".env",
		".gitignore",
	}
	for _, name := range misses {
		if matchExclude(name, builtinDefaultExcludes) {
			t.Errorf("did NOT expect %q in builtinDefaultExcludes", name)
		}
	}
}
