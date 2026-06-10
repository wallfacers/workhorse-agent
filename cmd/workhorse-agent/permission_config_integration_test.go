package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/api"
	"github.com/wallfacers/workhorse-agent/internal/permission"
)

// 6.1 — a PUT to /v1/permission-config writes config.yaml, triggers a reload,
// and the new deny rule is honored by the very next Check on an already-running
// manager (no restart).
func TestIntegration_PutReloadAffectsRunningManager(t *testing.T) {
	st := openMemStore(t)
	perm := newManager(st, "")
	reloader, path := newReloader(t, "tools:\n  preset_rules: []\n", st, perm, discardLogger())

	srv := api.NewServer(api.Config{Port: 0, Version: "test", MaxRequestBodyBytes: 1 << 20}, nil, st, nil)
	srv.SetPermissionConfig(path, reloader.Reload)

	// Before: no rule, default empty → prompt path returns deny (our stub).
	if d, _, _ := perm.Check(context.Background(), permission.CheckInput{SessionID: "sess", Tool: "Bash", Resource: "rm tmp"}); d != permission.Deny {
		t.Fatalf("precondition: got %v, want deny via prompt", d)
	}

	body := `{"default_permission":"","preset_rules":[{"tool":"Bash","pattern":"rm *","decision":"deny_permanent"}]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/permission-config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT status %d, body=%s", w.Code, w.Body)
	}

	// After: the running manager immediately sees the persisted deny_permanent.
	d, _, err := perm.Check(context.Background(), permission.CheckInput{SessionID: "sess", Tool: "Bash", Resource: "rm tmp"})
	if err != nil || d != permission.DenyPermanent {
		t.Fatalf("after reload Check = (%v,%v), want deny_permanent", d, err)
	}
}

// 6.2 — switching tools.default_permission to deny_permanent via reload makes
// unmatched calls deny without prompting.
func TestIntegration_DefaultPermissionReload(t *testing.T) {
	st := openMemStore(t)
	promptCalls := 0
	perm := permission.New(st,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			promptCalls++
			return permission.Deny, true
		}, nil, 0, "")
	reloader, path := newReloader(t, "tools:\n  default_permission: \"\"\n", st, perm, discardLogger())

	srv := api.NewServer(api.Config{Port: 0, Version: "test", MaxRequestBodyBytes: 1 << 20}, nil, st, nil)
	srv.SetPermissionConfig(path, reloader.Reload)

	body := `{"default_permission":"deny_permanent","preset_rules":[]}`
	req := httptest.NewRequest(http.MethodPut, "/v1/permission-config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT status %d, body=%s", w.Code, w.Body)
	}

	before := promptCalls
	d, _, err := perm.Check(context.Background(), permission.CheckInput{SessionID: "sess", Tool: "Write", Resource: "/some/file"})
	if err != nil || d != permission.DenyPermanent {
		t.Fatalf("Check = (%v,%v), want deny_permanent fallback", d, err)
	}
	if promptCalls != before {
		t.Errorf("default deny must not prompt, prompt calls grew %d → %d", before, promptCalls)
	}
}
