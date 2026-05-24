// Package pathguard centralises the path-validation rules every tool that
// touches the filesystem must follow. Source: AI #2 review H-6. The five-step
// algorithm is:
//
//  1. filepath.Clean the input
//  2. filepath.EvalSymlinks; if the leaf doesn't exist (Write/Edit case),
//     fall back to EvalSymlinks on the parent directory and rejoin the leaf
//  3. filepath.Rel against the session workdir; reject if the result starts
//     with ".." or is an absolute path (workdir escape)
//  4. open with O_NOFOLLOW on Linux/macOS so a TOCTOU symlink swap during the
//     race window can't redirect us
//  5. on platforms without O_NOFOLLOW, os.Lstat the path after open and
//     reject if it's a symlink
//
// Callers must use Resolve* before constructing any file handle, and Open*
// from this package whenever they actually open the file.
package pathguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathEscape is returned whenever a user-supplied path would resolve to a
// location outside the session workdir, or would traverse a symlink the agent
// has no business following.
var ErrPathEscape = errors.New("pathguard: path escapes workdir")

// Resolve validates path against workdir under the "file must already exist"
// rules. It returns the cleaned, symlink-resolved absolute path. Use this for
// Read / Grep.
func Resolve(workdir, path string) (string, error) {
	abs, err := canonicalise(workdir, path, false)
	if err != nil {
		return "", err
	}
	if err := assertInside(workdir, abs); err != nil {
		return "", err
	}
	return abs, nil
}

// ResolveForWrite is the same as Resolve but the leaf may not yet exist; only
// the parent directory must already resolve cleanly. Use this for Write and
// for Edit when the file is being created.
func ResolveForWrite(workdir, path string) (string, error) {
	abs, err := canonicalise(workdir, path, true)
	if err != nil {
		return "", err
	}
	if err := assertInside(workdir, abs); err != nil {
		return "", err
	}
	return abs, nil
}

// canonicalise produces an absolute, symlink-resolved path. When allowMissing
// is true and the leaf does not exist, we resolve the parent directory and
// rejoin the leaf — that matches Write/Edit semantics where the file is being
// created.
func canonicalise(workdir, path string, allowMissing bool) (string, error) {
	if path == "" {
		return "", errors.New("pathguard: empty path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workdir, path)
	}
	path = filepath.Clean(path)

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || !allowMissing {
			return "", fmt.Errorf("pathguard: resolve %s: %w", path, err)
		}
		// Leaf missing — resolve parent so we still catch a symlinked
		// directory above the leaf.
		dir, leaf := filepath.Split(path)
		dir = filepath.Clean(dir)
		resolvedDir, dirErr := filepath.EvalSymlinks(dir)
		if dirErr != nil {
			return "", fmt.Errorf("pathguard: resolve parent of %s: %w", path, dirErr)
		}
		resolved = filepath.Join(resolvedDir, leaf)
	}
	return filepath.Clean(resolved), nil
}

// assertInside verifies abs lies under workdir. workdir may itself be a
// symlink — we resolve it too so the comparison is apples-to-apples.
func assertInside(workdir, abs string) error {
	wdResolved, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		// If the workdir itself can't be resolved we can't validate; treat
		// that as an unconditional reject. The caller should have ensured
		// workdir exists.
		return fmt.Errorf("pathguard: resolve workdir %s: %w", workdir, err)
	}
	rel, err := filepath.Rel(wdResolved, abs)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrPathEscape, abs)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %s", ErrPathEscape, abs)
	}
	return nil
}
