package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestFSList_NormalDirectory(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0o644)

	_, ts := newTestServer(t, func(c *Config) {
		c.DefaultWorkdir = dir
	})

	resp, err := http.Get(ts.URL + "/v1/fs/list?path=" + dir)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	var body fsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Path != dir {
		t.Fatalf("path: %q", body.Path)
	}

	names := make([]string, len(body.Entries))
	for i, e := range body.Entries {
		names[i] = e.Name
		if e.Path != filepath.Join(dir, e.Name) {
			t.Fatalf("entry %q path: %q", e.Name, e.Path)
		}
	}
	sort.Strings(names)

	want := []string{".hidden", "file.txt", "subdir"}
	if len(names) != len(want) {
		t.Fatalf("entries: %v", names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("entry[%d]: %q want %q", i, names[i], n)
		}
	}

	for _, e := range body.Entries {
		if e.Name == "subdir" && !e.IsDir {
			t.Fatalf("subdir should be IsDir=true")
		}
		if e.Name == "file.txt" && e.IsDir {
			t.Fatalf("file.txt should be IsDir=false")
		}
	}
}

func TestFSList_OmitPathUsesDefaultWorkdir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("x"), 0o644)

	_, ts := newTestServer(t, func(c *Config) {
		c.DefaultWorkdir = dir
	})

	resp, err := http.Get(ts.URL + "/v1/fs/list")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	var body fsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Path != dir {
		t.Fatalf("path: %q want %q", body.Path, dir)
	}
	if len(body.Entries) != 1 || body.Entries[0].Name != "hello.txt" {
		t.Fatalf("entries: %v", body.Entries)
	}
}

func TestFSList_NotFound(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/fs/list?path=/nonexistent_dir_xyz_123")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestFSList_NotDirectory(t *testing.T) {
	f, err := os.CreateTemp("", "fs_test_file_*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/fs/list?path=" + f.Name())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestFSList_VirtualFS(t *testing.T) {
	_, ts := newTestServer(t)
	for _, p := range []string{"/proc", "/sys", "/dev", "/run"} {
		resp, err := http.Get(ts.URL + "/v1/fs/list?path=" + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status %d want 403", p, resp.StatusCode)
		}
	}
}

func TestFSList_OutsideWorkdir(t *testing.T) {
	dir := t.TempDir()
	_, ts := newTestServer(t, func(c *Config) {
		c.DefaultWorkdir = dir
	})

	resp, err := http.Get(ts.URL + "/v1/fs/list?path=/etc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d want 403 (path outside workdir)", resp.StatusCode)
	}
}

func TestFSList_SymlinkEscapesWorkdir(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, ts := newTestServer(t, func(c *Config) {
		c.DefaultWorkdir = dir
	})

	resp, err := http.Get(ts.URL + "/v1/fs/list?path=" + link)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d want 403 (symlink escapes workdir)", resp.StatusCode)
	}
}

func TestFSList_SymlinkResolved(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.Mkdir(target, 0o755)
	os.WriteFile(filepath.Join(target, "inner.txt"), []byte("x"), 0o644)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/fs/list?path=" + link)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	var body fsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Path should be the resolved target, not the symlink.
	if body.Path != target {
		t.Fatalf("path: %q want %q", body.Path, target)
	}
}
