package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/session"
)

func TestHealth_OKShape(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("ok: %v", body["ok"])
	}
	if body["version"] != "test" {
		t.Fatalf("version: %v", body["version"])
	}
	if _, ok := body["uptime_sec"]; !ok {
		t.Fatalf("uptime_sec missing")
	}
	if _, ok := body["sessions_active"]; !ok {
		t.Fatalf("sessions_active missing")
	}
	// The frontend's auto-connect probe verifies protocol_version before
	// attaching and gates on the frontend_tools capability.
	if body["protocol_version"] != ProtocolVersion {
		t.Fatalf("protocol_version: %v", body["protocol_version"])
	}
	caps, ok := body["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities missing or not an array: %v", body["capabilities"])
	}
	hasFrontendTools := false
	for _, c := range caps {
		if c == "frontend_tools" {
			hasFrontendTools = true
		}
	}
	if !hasFrontendTools {
		t.Fatalf("capabilities lacks frontend_tools: %v", caps)
	}
}

func TestHealth_DefaultWorkdirAndPlatform(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	wd, ok := body["default_workdir"].(string)
	if !ok || wd == "" {
		t.Fatalf("default_workdir missing or empty: %v", body["default_workdir"])
	}

	plat, ok := body["platform"].(string)
	if !ok || plat == "" {
		t.Fatalf("platform missing or empty: %v", body["platform"])
	}
	if plat == "" {
		t.Fatalf("platform should not be empty")
	}
}

func TestHealth_DefaultWorkdirConfigOverride(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) {
		c.DefaultWorkdir = "/opt/projects"
	})
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["default_workdir"] != "/opt/projects" {
		t.Fatalf("default_workdir: %v", body["default_workdir"])
	}
}

// TestHealth_DefaultWorkdirHomeFallback: with no config override, default_workdir
// is the user's home directory — never the launch cwd
// (decouple-project-from-launch-cwd D1).
func TestHealth_DefaultWorkdirHomeFallback(t *testing.T) {
	home, herr := os.UserHomeDir()
	if herr != nil || home == "" {
		t.Skip("no resolvable home in this environment")
	}
	_, ts := newTestServer(t) // no DefaultWorkdir override
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["default_workdir"] != home {
		t.Fatalf("default_workdir = %v, want home %q", body["default_workdir"], home)
	}
	if wd, _ := os.Getwd(); wd != home && body["default_workdir"] == wd {
		t.Fatalf("default_workdir must not be the launch cwd %q", wd)
	}
}

// TestHealth_DefaultWorkdirOmittedWhenNoHome: with no override and an
// unresolvable home, the field is omitted so the client routes to its picker —
// never the launch cwd (decouple-project-from-launch-cwd D1).
func TestHealth_DefaultWorkdirOmittedWhenNoHome(t *testing.T) {
	t.Setenv("HOME", "") // make os.UserHomeDir() fail on Linux
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		t.Skip("platform does not derive home from $HOME")
	}
	_, ts := newTestServer(t) // no override
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, present := body["default_workdir"]; present {
		t.Fatalf("default_workdir should be omitted when home is unresolvable, got %v", v)
	}
}

func TestHealth_DistroOnlyOnWSL(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if isWSL() {
		distro, ok := body["distro"].(string)
		if !ok || distro == "" {
			t.Fatalf("WSL environment: distro should be non-empty string")
		}
	} else {
		if _, ok := body["distro"]; ok {
			t.Fatalf("non-WSL environment: distro should not be present")
		}
	}
}

func TestDistroName_PrefersEnvOverOSRelease(t *testing.T) {
	// WSL_DISTRO_NAME is the registration name `wsl.exe -d` needs; it wins over
	// the /etc/os-release PRETTY_NAME fallback (add-wsl-remote D-WSL-8 / A4).
	if got := distroName("Ubuntu"); got != "Ubuntu" {
		t.Fatalf("env should win: got %q", got)
	}
	if got := distroName("  Ubuntu-22.04  "); got != "Ubuntu-22.04" {
		t.Fatalf("env should be trimmed: got %q", got)
	}
	// Empty env → falls back to parseOSRelease() (host-dependent); the only
	// invariant is it never returns the blank env value as a registration name.
	if got := distroName("   "); got == "   " {
		t.Fatalf("blank env must not be used verbatim: got %q", got)
	}
}

func TestHealth_NoAuthRequired(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) {
		c.Auth = BearerConfig{Enabled: true, Token: "secret"}
	})
	// No Authorization header — should still pass.
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestDebugEvents_DisabledReturns404(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateIdle)
	resp, err := http.Get(ts.URL + "/debug/sessions/" + sess.ID + "/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestDebugEvents_StreamsNDJSON(t *testing.T) {
	st := newSQLiteStore(t)
	mgr := session.NewManager(session.ManagerOptions{Store: st})
	s := NewServer(Config{
		Host:                "127.0.0.1",
		Port:                0,
		MaxRequestBodyBytes: 1 << 20,
		DebugEnabled:        true,
		Version:             "test",
	}, mgr, st, newDiscardLogger())
	ts := httptestServer(t, s)

	sess, _ := mgr.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: false, Model: "m", ProviderName: "anthropic",
	})
	for i := 0; i < 3; i++ {
		_ = sess.Emit(context.Background(), "assistant_text_delta",
			map[string]any{"delta": "x"})
		<-sess.Outbox
	}

	resp, err := http.Get(ts.URL + "/debug/sessions/" + sess.ID + "/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type: %q", ct)
	}
	rd := bufio.NewReader(resp.Body)
	got := 0
	for {
		line, err := rd.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			var row map[string]any
			if e := json.Unmarshal([]byte(line), &row); e != nil {
				t.Fatalf("bad line %q: %v", line, e)
			}
			if row["type"] != "assistant_text_delta" {
				t.Fatalf("row type: %v", row["type"])
			}
			got++
		}
		if err != nil {
			break
		}
	}
	if got != 3 {
		t.Fatalf("expected 3 events, got %d", got)
	}
}

func TestDebugEvents_SinceFilter(t *testing.T) {
	st := newSQLiteStore(t)
	mgr := session.NewManager(session.ManagerOptions{Store: st})
	s := NewServer(Config{
		Host:                "127.0.0.1",
		Port:                0,
		MaxRequestBodyBytes: 1 << 20,
		DebugEnabled:        true,
		Version:             "test",
	}, mgr, st, newDiscardLogger())
	ts := httptestServer(t, s)

	sess, _ := mgr.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: false, Model: "m", ProviderName: "anthropic",
	})
	for i := 0; i < 5; i++ {
		_ = sess.Emit(context.Background(), "x", nil)
		<-sess.Outbox
	}
	resp, err := http.Get(ts.URL + "/debug/sessions/" + sess.ID + "/events?since=3")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	gotIdx := []float64{}
	for {
		line, err := rd.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			var row map[string]any
			if e := json.Unmarshal([]byte(line), &row); e != nil {
				t.Fatalf("bad line: %v", e)
			}
			gotIdx = append(gotIdx, row["idx"].(float64))
		}
		if err != nil {
			break
		}
	}
	if len(gotIdx) != 2 || int(gotIdx[0]) != 4 || int(gotIdx[1]) != 5 {
		t.Fatalf("expected idx [4 5], got %v", gotIdx)
	}
}
