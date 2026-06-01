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

// handleFSList answers GET /v1/fs/list?path=<dir>. It enumerates a single
// directory within the sidecar filesystem, respecting a virtual-FS blacklist.
func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = s.defaultWorkdir()
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
			clean = "/" + clean
		}
	}

	if isVirtualFS(clean) {
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
		p = "/" + p
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
