package pathguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathEscape is returned whenever a user-supplied path would resolve to a
// location outside the containment root, or would traverse a symlink the agent
// has no business following.
var ErrPathEscape = errors.New("pathguard: path escapes workdir")

// ErrInvalidMemoryKind is returned when the kind parameter to a memory
// resolver is not "memory" or "user".
var ErrInvalidMemoryKind = errors.New("pathguard: invalid memory kind")

type resolver struct {
	root string
}

func (r *resolver) resolve(path string, allowMissing bool) (string, error) {
	abs, err := r.canonicalise(path, allowMissing)
	if err != nil {
		return "", err
	}
	if err := r.assertInside(abs); err != nil {
		return "", err
	}
	return abs, nil
}

func (r *resolver) canonicalise(path string, allowMissing bool) (string, error) {
	if path == "" {
		return "", errors.New("pathguard: empty path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.root, path)
	}
	path = filepath.Clean(path)

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || !allowMissing {
			return "", fmt.Errorf("pathguard: resolve %s: %w", path, err)
		}
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

func (r *resolver) assertInside(abs string) error {
	wdResolved, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return fmt.Errorf("pathguard: resolve root %s: %w", r.root, err)
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

// Resolve validates path against workdir under the "file must already exist"
// rules. It returns the cleaned, symlink-resolved absolute path. Use this for
// Read / Grep.
func Resolve(workdir, path string) (string, error) {
	return (&resolver{root: workdir}).resolve(path, false)
}

// ResolveForWrite is the same as Resolve but the leaf may not yet exist; only
// the parent directory must already resolve cleanly. Use this for Write and
// for Edit when the file is being created.
func ResolveForWrite(workdir, path string) (string, error) {
	return (&resolver{root: workdir}).resolve(path, true)
}

// ResolveMemory resolves the memory file for the given kind under the
// profile's memories directory. The file must already exist.
func ResolveMemory(profileDir, kind string) (string, error) {
	filename, memDir, err := memoryTarget(profileDir, kind)
	if err != nil {
		return "", err
	}
	return (&resolver{root: memDir}).resolve(filename, false)
}

// ResolveMemoryForWrite resolves the memory file for writing (may not yet exist).
func ResolveMemoryForWrite(profileDir, kind string) (string, error) {
	filename, memDir, err := memoryTarget(profileDir, kind)
	if err != nil {
		return "", err
	}
	return (&resolver{root: memDir}).resolve(filename, true)
}

func memoryTarget(profileDir, kind string) (filename, memDir string, err error) {
	switch kind {
	case "memory":
		filename = "MEMORY.md"
	case "user":
		filename = "USER.md"
	default:
		return "", "", fmt.Errorf("%w: %q", ErrInvalidMemoryKind, kind)
	}
	memDir = filepath.Join(profileDir, "memories")
	return filename, memDir, nil
}

// DraftsDir returns the resolved drafts directory under the external-agents
// profile location: <externalAgentsDir>/.drafts/. Used by the adapter-generator
// subagent's WriteAdapterDraft tool as the only writable location.
func DraftsDir(externalAgentsDir string) string {
	return filepath.Join(externalAgentsDir, ".drafts")
}

// ResolveDraft validates that path resolves to a file directly under the
// drafts directory of externalAgentsDir. The file must already exist.
// Symlinks are followed (and the resolved target must still be inside the
// drafts dir). Use this when reading or smoke-testing a previously written
// draft.
func ResolveDraft(externalAgentsDir, path string) (string, error) {
	return (&resolver{root: DraftsDir(externalAgentsDir)}).resolve(path, false)
}

// ResolveDraftForWrite is the create/overwrite variant: the leaf may not yet
// exist, but its parent must be the drafts directory itself (no nested
// subdirectories — drafts are flat). The WriteAdapterDraft tool uses this.
//
// Symlinks at the leaf are rejected outright (whether the target exists or
// not) because allow-missing path resolution would otherwise quietly accept a
// dangling symlink. Runtime O_NOFOLLOW on OpenWrite is the second line of
// defense; this check is the first.
func ResolveDraftForWrite(externalAgentsDir, path string) (string, error) {
	abs, err := (&resolver{root: DraftsDir(externalAgentsDir)}).resolve(path, true)
	if err != nil {
		return "", err
	}
	if fi, lerr := os.Lstat(abs); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: draft path is a symlink: %s", ErrPathEscape, abs)
	}
	wdResolved, derr := filepath.EvalSymlinks(DraftsDir(externalAgentsDir))
	if derr != nil {
		return "", fmt.Errorf("pathguard: resolve drafts root: %w", derr)
	}
	if filepath.Dir(abs) != wdResolved {
		return "", fmt.Errorf("%w: drafts must be flat (no subdirs): %s", ErrPathEscape, abs)
	}
	return abs, nil
}
