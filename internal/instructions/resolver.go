package instructions

import (
	"os"
	"path/filepath"
	"sync"
)

// Injection holds a proximity-injected instruction file's metadata and content.
type Injection struct {
	Path    string
	Content string
}

// Resolver handles proximity injection for the Read tool. It tracks which
// instruction files have already been injected (system-level and proximity)
// and deduplicates across calls within the same session.
type Resolver struct {
	// systemPaths are the file paths already loaded in the system prompt.
	systemPaths map[string]bool

	// injectedPaths tracks files already proximity-injected this session.
	injectedPaths map[string]bool
	mu            sync.Mutex
}

// NewResolver creates a Resolver pre-loaded with system-level paths from
// the snapshot so they are never proximity-injected.
func NewResolver(snapshot *Snapshot) *Resolver {
	sys := make(map[string]bool)
	if snapshot != nil {
		for _, f := range snapshot.Files {
			sys[f.Path] = true
		}
	}
	return &Resolver{
		systemPaths:   sys,
		injectedPaths: make(map[string]bool),
	}
}

// Resolve walks upward from the parent directory of filePath to workdirRoot,
// looking for instruction files that are not in the system-level snapshot
// and have not been previously injected. Found files are appended to the
// Read tool output as <system-reminder> blocks. Each path is injected at most
// once per session.
func (r *Resolver) Resolve(filePath string, workdirRoot string) []Injection {
	target := filepath.Clean(filePath)
	dir := filepath.Dir(target)
	// filePath arrives symlink-resolved (Read passes pathguard.Resolve's output,
	// which runs filepath.EvalSymlinks). The workdir root must be resolved the
	// same way or the Rel comparison below compares two spellings of the same
	// tree (e.g. /tmp vs /private/tmp on macOS, or a symlinked project root) and
	// proximity injection silently never fires.
	root := filepath.Clean(workdirRoot)
	if resolvedRoot, err := filepath.EvalSymlinks(workdirRoot); err == nil {
		root = resolvedRoot
	}

	var results []Injection

	for {
		if !isWithinOrEqual(dir, root) {
			break
		}
		if dir == root {
			// At the workdir root itself, stop (no ancestor to check beyond).
			break
		}
		found := r.findInDir(dir)
		if found != "" {
			r.mu.Lock()
			already := r.systemPaths[found] || r.injectedPaths[found]
			if !already {
				r.injectedPaths[found] = true
			}
			r.mu.Unlock()
			if !already {
				content, err := readFile(found)
				if err == nil && content != "" {
					results = append(results, Injection{
						Path:    found,
						Content: content,
					})
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return results
}

// FormatInjection renders an Injection as a <system-reminder> block suitable
// for appending to the Read tool output.
func FormatInjection(inj Injection) string {
	return "<system-reminder>\nInstructions from: " + inj.Path +
		"\n" + inj.Content + "\n</system-reminder>"
}

// findInDir checks for instruction files in dir using the same filename
// priority as the loader. Returns the path of the first match, or "".
func (r *Resolver) findInDir(dir string) string {
	for _, name := range searchFiles {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// isWithinOrEqual reports whether dir is equal to or a subdirectory of root.
func isWithinOrEqual(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !startsWithDotDot(rel))
}

func startsWithDotDot(s string) bool {
	return len(s) >= 2 && s[0] == '.' && s[1] == '.'
}
