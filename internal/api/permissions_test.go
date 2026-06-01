package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/api"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func newPermTestServer(t *testing.T) *api.Server {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return api.NewServer(api.Config{Port: 0, Version: "test", MaxRequestBodyBytes: 1 << 20}, nil, st, nil)
}

func TestPermissions_ListEmpty(t *testing.T) {
	srv := newPermTestServer(t)
	w := doGet(t, srv, "/v1/permissions")
	if w.Code != 200 {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"rules":[]`) && !strings.Contains(body, `"rules": []`) {
		t.Errorf("expected empty rules array, got: %s", body)
	}
}

func TestPermissions_ListWithRules(t *testing.T) {
	srv := newPermTestServer(t)
	ctx := context.Background()
	if err := srv.Store().SavePermission(ctx, &store.Permission{
		ID: "perm-manual1", Tool: "Bash", Pattern: "git *",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	w := doGet(t, srv, "/v1/permissions")
	if w.Code != 200 {
		t.Fatalf("status: got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "perm-manual1") {
		t.Errorf("response should contain perm-manual1: %s", w.Body)
	}
}

func TestPermissions_CreateValid(t *testing.T) {
	srv := newPermTestServer(t)
	body := `{"tool":"Bash","pattern":"npm *","decision":"allow_permanent"}`
	w := doPost(t, srv, "/v1/permissions", body)
	if w.Code != 201 {
		t.Fatalf("status: got %d, want 201. body=%s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"tool":"Bash"`) {
		t.Errorf("response should contain tool: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), `"source":"manual"`) {
		t.Errorf("response should have source=manual: %s", w.Body)
	}
	// Verify it appears in list
	w2 := doGet(t, srv, "/v1/permissions")
	if !strings.Contains(w2.Body.String(), `"tool":"Bash"`) {
		t.Errorf("list should contain created rule: %s", w2.Body)
	}
}

func TestPermissions_CreateInvalidDecision(t *testing.T) {
	srv := newPermTestServer(t)
	body := `{"decision":"allow_once"}`
	w := doPost(t, srv, "/v1/permissions", body)
	if w.Code != 400 {
		t.Fatalf("status: got %d, want 400. body=%s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "invalid decision") {
		t.Errorf("error should mention invalid decision: %s", w.Body)
	}
}

func TestPermissions_DeleteExisting(t *testing.T) {
	srv := newPermTestServer(t)
	ctx := context.Background()
	if err := srv.Store().SavePermission(ctx, &store.Permission{
		ID: "perm-todel", Tool: "Read", Pattern: "*",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	w := doDelete(t, srv, "/v1/permissions/perm-todel")
	if w.Code != 204 {
		t.Fatalf("status: got %d, want 204", w.Code)
	}
	// Verify gone
	w2 := doGet(t, srv, "/v1/permissions")
	if strings.Contains(w2.Body.String(), "perm-todel") {
		t.Errorf("deleted rule should not appear: %s", w2.Body)
	}
}

func TestPermissions_DeleteNotFound(t *testing.T) {
	srv := newPermTestServer(t)
	w := doDelete(t, srv, "/v1/permissions/perm-nonexist")
	if w.Code != 404 {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
}

func TestPermissions_SourcePreset(t *testing.T) {
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := api.NewServer(api.Config{
		Port:                0,
		Version:             "test",
		MaxRequestBodyBytes: 1 << 20,
	}, nil, st, nil)

	ctx := context.Background()
	presetID := presetRuleID("Bash", "git *")
	if err := st.SavePermission(ctx, &store.Permission{
		ID: presetID, Tool: "Bash", Pattern: "git *",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := doGet(t, srv, "/v1/permissions")
	if !strings.Contains(w.Body.String(), `"source":"preset"`) {
		t.Errorf("preset rule should have source=preset: %s", w.Body)
	}
}

func presetRuleID(tool, pattern string) string {
	h := sha256.Sum256([]byte(tool + "\x00" + pattern))
	return "preset-" + hex.EncodeToString(h[:])[:16]
}

func doGet(t *testing.T, srv *api.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func doPost(t *testing.T, srv *api.Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func doDelete(t *testing.T, srv *api.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}
