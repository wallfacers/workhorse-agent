package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidWorkdir is returned when the workdir fails validation.
var ErrInvalidWorkdir = errors.New("session: invalid workdir")

// ValidateWorkdir normalises and validates a workdir value.
//   - empty string → os.UserHomeDir()
//   - must be an absolute path
//   - symlinks are resolved (as far as possible)
//   - resolved path must not be "/"
//
// Returns the cleaned, resolved path on success.
func ValidateWorkdir(workdir string) (string, error) {
	if workdir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: resolve home: %w", ErrInvalidWorkdir, err)
		}
		workdir = home
	}
	if !filepath.IsAbs(workdir) {
		return "", fmt.Errorf("%w: must be absolute: %q", ErrInvalidWorkdir, workdir)
	}
	resolved, err := resolvePath(workdir)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidWorkdir, err)
	}
	if resolved == "/" {
		return "", fmt.Errorf("%w: cannot be the filesystem root", ErrInvalidWorkdir)
	}
	return resolved, nil
}

// AssertWorkdirWithin checks that child is inside parent's directory tree.
// Both paths should already be validated (absolute, resolved).
func AssertWorkdirWithin(parent, child string) error {
	parentResolved, err := resolvePath(parent)
	if err != nil {
		return fmt.Errorf("resolve parent workdir: %w", err)
	}
	childResolved, err := resolvePath(child)
	if err != nil {
		return fmt.Errorf("resolve child workdir: %w", err)
	}
	rel, err := filepath.Rel(parentResolved, childResolved)
	if err != nil {
		return fmt.Errorf("child workdir escapes parent")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("child workdir must be within parent workdir %q, got %q", parent, child)
	}
	return nil
}

// resolvePath returns the symlink-resolved absolute path. If the leaf does not
// exist, the nearest existing ancestor is resolved and the remaining components
// are appended.
func resolvePath(path string) (string, error) {
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	dir, leaf := filepath.Split(path)
	dir = filepath.Clean(dir)
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// Parent also doesn't exist — fall back to cleaned path. The "/" check
		// in ValidateWorkdir still catches the root case.
		return path, nil
	}
	return filepath.Join(resolvedDir, leaf), nil
}
