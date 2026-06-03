package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
)

// newTestServer builds a Server backed by a noop session.Manager (no agent
// loop). Tests can drive session lifecycle via the manager directly. The
// returned httptest.Server speaks to Server.Handler() with auth disabled and
// localhost origin allowed by default.
func newTestServer(t *testing.T, cfg ...func(*Config)) (*Server, *httptest.Server) {
	t.Helper()
	mgr := session.NewManager(session.ManagerOptions{MaxConcurrent: 0})
	c := Config{
		Host:                    "127.0.0.1",
		Port:                    0,
		MaxRequestBodyBytes:     1 << 20,
		GracefulShutdownTimeout: 5 * time.Second,
		Version:                 "test",
	}
	for _, fn := range cfg {
		fn(&c)
	}
	s := NewServer(c, mgr, nil, newDiscardLogger())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestCreateSession_201(t *testing.T) {
	_, ts := newTestServer(t)
	body, _ := json.Marshal(map[string]any{
		"workdir":   "/tmp/proj",
		"provider":  "anthropic",
		"model":     "claude-sonnet-4-6",
		"ephemeral": true,
	})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var view sessionMeta
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.ID) != 26 {
		t.Fatalf("session ID not 26-char ULID: %q", view.ID)
	}
	if view.Status != "idle" {
		t.Fatalf("status: %q", view.Status)
	}
	if view.Workdir != "/tmp/proj" || view.Provider != "anthropic" {
		t.Fatalf("view fields lost: %+v", view)
	}
}

func TestCreateSession_415_NoContentType(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sessions",
		strings.NewReader(`{"workdir":"/x"}`))
	// Strip the implicit text/plain Go would add — emulating curl --data with no -H.
	req.Header.Del("Content-Type")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415, got %d", resp.StatusCode)
	}
}

func TestCreateSession_415_WrongContentType(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/v1/sessions", "text/plain", strings.NewReader(`hi`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415, got %d", resp.StatusCode)
	}
}

func TestCreateSession_429_MaxConcurrent(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) { c.MaxConcurrentSessions = 1 })
	body := bytes.NewReader([]byte(`{"workdir":"/x","provider":"anthropic","model":"m","ephemeral":true}`))
	first, err := http.Post(ts.URL+"/v1/sessions", "application/json", body)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first: %d", first.StatusCode)
	}
	body = bytes.NewReader([]byte(`{"workdir":"/x","provider":"anthropic","model":"m","ephemeral":true}`))
	second, err := http.Post(ts.URL+"/v1/sessions", "application/json", body)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		raw, _ := io.ReadAll(second.Body)
		t.Fatalf("want 429, got %d: %s", second.StatusCode, raw)
	}
}

func TestListSessions(t *testing.T) {
	s, ts := newTestServer(t)
	for i := 0; i < 3; i++ {
		_, err := s.manager.CreateSession(context.Background(), session.Options{
			Workdir: "/tmp", Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Sessions []sessionMeta `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(body.Sessions))
	}
}

func TestGetSession_404(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/sessions/01HFAKE_NOT_PRESENT")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestDeleteSession(t *testing.T) {
	s, ts := newTestServer(t)
	sess, err := s.manager.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/sessions/"+sess.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 204, got %d: %s", resp.StatusCode, raw)
	}
	// Second delete should 404.
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/v1/sessions/"+sess.ID, nil)
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("repeat delete: want 404, got %d", resp2.StatusCode)
	}
}

func TestCancelSession(t *testing.T) {
	s, ts := newTestServer(t)
	sess, err := s.manager.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp, err := http.Post(ts.URL+"/v1/sessions/"+sess.ID+"/cancel", "", nil)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	resp2, _ := http.Post(ts.URL+"/v1/sessions/01HFAKE_NOT_PRESENT/cancel", "", nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("nonexistent cancel: want 404, got %d", resp2.StatusCode)
	}
}

func TestCompactSession_202_WhenIdle(t *testing.T) {
	s, ts := newTestServer(t)
	sess, err := s.manager.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.State() != session.StateIdle {
		t.Fatalf("setup: expected Idle, got %q", sess.State())
	}
	resp, err := http.Post(ts.URL+"/v1/sessions/"+sess.ID+"/compact", "", nil)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 202, got %d: %s", resp.StatusCode, raw)
	}
}

func TestCompactSession_409_WhenBusy(t *testing.T) {
	s, ts := newTestServer(t)
	sess, err := s.manager.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sess.Transition(session.StateIdle, session.StateThinking); err != nil {
		t.Fatalf("force thinking: %v", err)
	}
	resp, err := http.Post(ts.URL+"/v1/sessions/"+sess.ID+"/compact", "", nil)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 409, got %d: %s", resp.StatusCode, raw)
	}
	// SSE mirror error should land in outbox.
	select {
	case ev := <-sess.Outbox:
		if ev.Type != "error" {
			t.Fatalf("expected error event in outbox, got %q", ev.Type)
		}
		if ev.Payload["code"] != "session_busy" {
			t.Fatalf("expected session_busy, got %v", ev.Payload["code"])
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no SSE mirror error event received")
	}
}

func TestCreateSession_413_BodyTooLarge(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) { c.MaxRequestBodyBytes = 128 })
	huge := strings.Repeat("a", 256)
	body := `{"workdir":"/x","provider":"anthropic","model":"m","ephemeral":true,"agent_type":"` + huge + `"}`
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 413, got %d: %s", resp.StatusCode, raw)
	}
	var body2 map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body2["code"] != "request_too_large" || body2["limit"].(float64) != 128 {
		t.Fatalf("body: %+v", body2)
	}
}

func TestIsJSONContentType(t *testing.T) {
	cases := map[string]bool{
		"application/json":                true,
		"application/json; charset=utf-8": true,
		" application/json ":              true,
		"text/plain":                      false,
		"":                                false,
	}
	for ct, want := range cases {
		if got := isJSONContentType(ct); got != want {
			t.Errorf("isJSONContentType(%q)=%v, want %v", ct, got, want)
		}
	}
}

func TestListSessions_AllPersistedAcrossWorkdirs(t *testing.T) {
	st := newSQLiteStore(t)
	mgr := session.NewManager(session.ManagerOptions{Store: st})
	s := NewServer(Config{
		Host:                    "127.0.0.1",
		Port:                    0,
		MaxRequestBodyBytes:     1 << 20,
		GracefulShutdownTimeout: 5 * time.Second,
		Version:                 "test",
	}, mgr, st, newDiscardLogger())
	ts := httptestServer(t, s)

	// Create sessions across two different workdirs.
	for _, wd := range []string{"/tmp/projA", "/tmp/projB"} {
		_, err := mgr.CreateSession(context.Background(), session.Options{
			Workdir: wd, Ephemeral: false, Model: "m", ProviderName: "anthropic",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	resp, err := http.Get(ts.URL + "/v1/sessions")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d: %s", resp.StatusCode, raw)
	}

	var body struct {
		Sessions []sessionMeta `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(body.Sessions))
	}

	// Each session should carry its own workdir.
	workdirs := map[string]bool{}
	for _, sm := range body.Sessions {
		workdirs[sm.Workdir] = true
		if sm.Status != "idle" {
			t.Fatalf("session %s status: %q, want idle", sm.ID, sm.Status)
		}
	}
	if !workdirs["/tmp/projA"] || !workdirs["/tmp/projB"] {
		t.Fatalf("expected both workdirs, got: %v", workdirs)
	}
}
