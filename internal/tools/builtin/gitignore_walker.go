package builtin

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/tools/builtin/gitignore"
)

// findRepoRoot walks up from start looking for an ancestor directory that
// contains a .git directory. Returns "" if none is found. The .git contents
// are not inspected; only its existence as a directory matters.
func findRepoRoot(start string) string {
	dir := start
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// gitignoreStack accumulates parsed .gitignore patterns as the walker
// descends into directories. push at directory enter, pop at directory exit.
// The cumulative pattern slice is rebuilt on every push/pop so IsIgnored is
// allocation-cheap per call.
type gitignoreStack struct {
	frames     []gitignoreFrame
	cumulative []gitignore.Pattern
}

type gitignoreFrame struct {
	patterns []gitignore.Pattern
}

// push parses one .gitignore file's content with domain = directory of the
// .gitignore file relative to the repo root (split on filepath.Separator).
// Empty lines and # comments are skipped. domain may be nil for the repo root.
func (s *gitignoreStack) push(domain []string, content []byte) {
	pats := parseGitignore(domain, content)
	s.frames = append(s.frames, gitignoreFrame{patterns: pats})
	s.rebuild()
}

// pop drops the most recently pushed frame.
func (s *gitignoreStack) pop() {
	if n := len(s.frames); n > 0 {
		s.frames = s.frames[:n-1]
		s.rebuild()
	}
}

func (s *gitignoreStack) rebuild() {
	s.cumulative = s.cumulative[:0]
	for _, f := range s.frames {
		s.cumulative = append(s.cumulative, f.patterns...)
	}
}

// IsIgnored reports whether the path (split components, relative to repo root)
// should be excluded according to the current stack. An empty stack returns
// false.
func (s *gitignoreStack) IsIgnored(pathParts []string, isDir bool) bool {
	if len(s.cumulative) == 0 {
		return false
	}
	return gitignore.NewMatcher(s.cumulative).Match(pathParts, isDir)
}

func parseGitignore(domain []string, content []byte) []gitignore.Pattern {
	var pats []gitignore.Pattern
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimRight(raw, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if p := gitignore.ParsePattern(line, domain); p != nil {
			pats = append(pats, p)
		}
	}
	return pats
}

// hardVCSDirs are directory basenames that are always skipped regardless of
// any user-facing toggle. .git/objects in particular is huge and never useful
// to grep through in an agent context.
var hardVCSDirs = map[string]struct{}{
	".git": {},
	".hg":  {},
	".svn": {},
}

// isHardVCSDir reports whether the basename is one of the always-skip VCS
// metadata directories.
func isHardVCSDir(name string) bool {
	_, ok := hardVCSDirs[name]
	return ok
}

// builtinDefaultExcludes is the hardcoded default list used when
// tools.grep.default_excludes is nil/empty. Each entry is a glob matched
// against the entry's basename via path.Match. Directories matched here are
// skipped (filepath.SkipDir); files matched here are skipped before opening.
// Order does not matter — the first match wins.
var builtinDefaultExcludes = []string{
	// directories — package managers, build artefacts, framework caches
	"node_modules", "vendor", "__pycache__",
	"dist", "build", "target", "out",
	".next", ".nuxt", ".turbo", ".cache", ".venv",
	".gradle",
	".mypy_cache", ".pytest_cache", ".ruff_cache", ".parcel-cache", ".tox",
	"coverage", "htmlcov",
	"__snapshots__",
	// files — lockfiles, minified artefacts, OS junk
	"*.lock",
	"package-lock.json", "pnpm-lock.yaml",
	"*.min.js", "*.min.css",
	".DS_Store",
}

// matchExclude returns true if name (a basename) matches any glob in patterns.
// Used for both directory pruning (filepath.SkipDir) and file skipping.
func matchExclude(name string, patterns []string) bool {
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}
