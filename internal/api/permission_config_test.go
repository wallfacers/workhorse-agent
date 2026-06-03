package api_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func doPut(t *testing.T, srv interface{ Handler() http.Handler }, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

const commentedConfig = `# top comment
server:
  port: 7821   # the port
tools:
  default_permission: ""
  preset_rules:
    - tool: Bash
      pattern: "git *"
      decision: allow_permanent
`

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPermissionConfig_GetReadsFile(t *testing.T) {
	srv := newPermTestServer(t)
	srv.SetPermissionConfig(writeConfigFile(t, commentedConfig), nil)

	w := doGet(t, srv, "/v1/permission-config")
	if w.Code != 200 {
		t.Fatalf("status %d, body=%s", w.Code, w.Body)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"tool":"Bash"`) || !strings.Contains(body, `"decision":"allow_permanent"`) {
		t.Errorf("GET should return the preset rule: %s", body)
	}
}

func TestPermissionConfig_GetMissingFile(t *testing.T) {
	srv := newPermTestServer(t)
	srv.SetPermissionConfig(filepath.Join(t.TempDir(), "absent.yaml"), nil)

	w := doGet(t, srv, "/v1/permission-config")
	if w.Code != 200 {
		t.Fatalf("status %d, body=%s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"preset_rules":[]`) {
		t.Errorf("missing file should yield empty rules: %s", w.Body)
	}
}

func TestPermissionConfig_Unavailable(t *testing.T) {
	srv := newPermTestServer(t) // SetPermissionConfig not called
	w := doGet(t, srv, "/v1/permission-config")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503. body=%s", w.Code, w.Body)
	}
}

func TestPermissionConfig_PutWritesAndPreservesComments(t *testing.T) {
	srv := newPermTestServer(t)
	path := writeConfigFile(t, commentedConfig)
	srv.SetPermissionConfig(path, nil)

	body := `{"default_permission":"deny_permanent","preset_rules":[{"tool":"Read","pattern":"/etc/**","decision":"deny_permanent"}]}`
	w := doPut(t, srv, "/v1/permission-config", body)
	if w.Code != 200 {
		t.Fatalf("status %d, body=%s", w.Code, w.Body)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"# top comment", "# the port", "port: 7821", "/etc/**", "deny_permanent"} {
		if !strings.Contains(s, want) {
			t.Errorf("written file missing %q:\n%s", want, s)
		}
	}
}

func TestPermissionConfig_PutInvalidDecision(t *testing.T) {
	srv := newPermTestServer(t)
	path := writeConfigFile(t, commentedConfig)
	srv.SetPermissionConfig(path, nil)

	body := `{"default_permission":"","preset_rules":[{"tool":"Read","pattern":"/x","decision":"allow_session"}]}`
	w := doPut(t, srv, "/v1/permission-config", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400. body=%s", w.Code, w.Body)
	}
	// File must be untouched (still has the original git * rule).
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "git *") {
		t.Errorf("invalid PUT must not modify the file:\n%s", out)
	}
}
