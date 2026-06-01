package instructions

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// searchFiles is the priority-ordered list of instruction filenames.
// The first filename with any match wins; subsequent filenames are skipped.
var searchFiles = []string{"AGENTS.md", "CLAUDE.md"}

// Loader discovers and reads instruction files from the project tree and
// global config directory.
type Loader struct {
	ProfileDir string
}

// Load discovers instruction files from two scopes and returns an immutable
// snapshot:
//
//   - Project-level: walk from workdir upward to the nearest .git ancestor
//     (git root), collecting all instances of the first matching filename.
//   - Global-level: check <profileDir>/AGENTS.md (single file).
//
// Missing files are treated as empty. If no files are found, the snapshot
// contains an empty Files slice.
func (l *Loader) Load(workdir string) (*Snapshot, error) {
	var files []File

	// Project-level discovery.
	projectFiles, err := findProjectFiles(workdir)
	if err != nil {
		return nil, fmt.Errorf("instructions: project scan: %w", err)
	}
	files = append(files, projectFiles...)

	// Global-level discovery.
	globalPath := globalFilePath(l.ProfileDir)
	if content, err := readFile(globalPath); err != nil {
		return nil, fmt.Errorf("instructions: global: %w", err)
	} else if content != "" {
		files = append(files, File{Path: globalPath, Content: content})
	}

	return &Snapshot{
		Files:    files,
		LoadedAt: time.Now().UTC(),
	}, nil
}

// findProjectFiles walks from workdir upward to the git root, collecting
// instruction files using the first-match filename strategy.
func findProjectFiles(workdir string) ([]File, error) {
	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	// Resolve symlinks so snapshot paths match the symlink-resolved paths the
	// Read tool's proximity resolver produces (via pathguard). Otherwise the
	// per-session dedup keyed on file path misses on a symlinked workdir and a
	// file already in the system prompt gets re-injected. Fall back to absWorkdir
	// if resolution fails.
	if resolved, rerr := filepath.EvalSymlinks(absWorkdir); rerr == nil {
		absWorkdir = resolved
	}

	gitRoot := findGitRoot(absWorkdir)
	topBound := gitRoot
	if topBound == "" {
		topBound = absWorkdir
	}

	// Collect all directories from workdir to topBound (inclusive).
	dirs := collectAncestorDirs(absWorkdir, topBound)

	// Determine which filename to use (first one with any match).
	var winner string
	for _, name := range searchFiles {
		for _, dir := range dirs {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				winner = name
				break
			}
		}
		if winner != "" {
			break
		}
	}
	if winner == "" {
		return nil, nil
	}

	// Collect all instances of the winning filename (bottom-up order).
	var files []File
	for _, dir := range dirs {
		p := filepath.Join(dir, winner)
		content, err := readFile(p)
		if err != nil {
			return nil, err
		}
		if content != "" {
			files = append(files, File{Path: p, Content: content})
		}
	}
	return files, nil
}

// collectAncestorDirs returns directories from startDir up to and including
// topBound, in bottom-up order (startDir first).
func collectAncestorDirs(startDir, topBound string) []string {
	var dirs []string
	cur := startDir
	for {
		dirs = append(dirs, cur)
		if cur == topBound {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return dirs
}

// findGitRoot walks upward from dir looking for a .git directory or file.
// Returns empty string if none found.
func findGitRoot(dir string) string {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func globalFilePath(profileDir string) string {
	if profileDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		return filepath.Join(home, ".workhorse-agent", "AGENTS.md")
	}
	return filepath.Join(profileDir, "AGENTS.md")
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("instructions: read %s: %w", path, err)
	}
	return string(data), nil
}
