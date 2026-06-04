package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// virtualFSPrefixes are filesystem roots that must never be enumerated.
var virtualFSPrefixes = []string{
	"/proc",
	"/sys",
	"/dev",
	"/run",
}

// fsEntry is a single directory entry in the /v1/fs/list response.
type fsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

// fsListResponse is the JSON shape returned by GET /v1/fs/list.
type fsListResponse struct {
	Path    string    `json:"path"`
	Entries []fsEntry `json:"entries"`
}

// handleFSList answers GET /v1/fs/list?path=<dir>&root=<projectRoot>. It
// enumerates a single directory within the sidecar filesystem, respecting a
// virtual-FS blacklist. Confinement follows the request's project `root` (the
// project being browsed) rather than a single global config dir, so any project
// the user opens elsewhere is browsable; `root` omitted falls back to
// default_workdir.
func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	if root == "" {
		root = s.defaultWorkdir()
	}
	if root == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = root
	}

	clean, err := resolveDirPath(dir)
	if err != nil {
		// EvalSymlinks fails for non-existent paths; fall through to Stat
		// which will give the correct 404 response.
		if !os.IsNotExist(err) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
			return
		}
		clean = filepath.Clean(dir)
		if !filepath.IsAbs(clean) {
			if abs, aerr := filepath.Abs(clean); aerr == nil {
				clean = abs
			}
		}
	}

	if isVirtualFS(clean) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}

	if !isWithinWorkdir(clean, root) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}

	info, err := os.Stat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "not a directory"})
		return
	}

	entries, err := os.ReadDir(clean)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}

	out := make([]fsEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, fsEntry{
			Name:  e.Name(),
			Path:  filepath.Join(clean, e.Name()),
			IsDir: e.IsDir(),
		})
	}

	writeJSON(w, http.StatusOK, fsListResponse{Path: clean, Entries: out})
}

// resolveDirPath canonicalises a path (Clean + EvalSymlinks). Returns an
// error if symlink resolution fails (broken symlink, permission denied).
func resolveDirPath(p string) (string, error) {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			p = abs
		}
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// isVirtualFS returns true if the canonical path falls under a virtual
// filesystem prefix (/proc, /sys, /dev, /run).
func isVirtualFS(p string) bool {
	for _, prefix := range virtualFSPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// isWithinWorkdir returns true if path is equal to or a descendant of workdir.
//
// Uses filepath.Rel rather than a hardcoded "/" prefix match so the check is
// correct on every OS: on Windows paths use "\" separators (and different
// drives), where `HasPrefix(path, workdir+"/")` never matched and the file
// tree failed to load with a 403.
func isWithinWorkdir(path, workdir string) bool {
	if workdir == "" {
		return false
	}
	resolvedWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedWorkdir, path)
	if err != nil {
		// Different volumes on Windows, or otherwise unrelatable paths.
		return false
	}
	// Inside iff the relative path does not climb out of workdir.
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
