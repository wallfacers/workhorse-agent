//go:build nginx

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
)

var noProxyClient = &http.Client{
	Transport: &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
	},
}

func hasDocker(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "info").Run() == nil
}

// sharedNginxHarness creates one API server and one nginx container that live
// for the duration of the parent test. All subtests share the same instance.
type sharedNginxHarness struct {
	t       *testing.T
	mgr     *session.Manager
	apiPort string
	ngxPort string
	ngxURL  string
	ngxName string
}

func newSharedNginxHarness(t *testing.T) *sharedNginxHarness {
	t.Helper()
	h := &sharedNginxHarness{
		t:       t,
		ngxPort: "17900",
		ngxURL:  "http://127.0.0.1:17900",
		ngxName: "workhorse-nginx-shared",
	}

	// Start API server on 0.0.0.0:0.
	h.mgr = session.NewManager(session.ManagerOptions{MaxConcurrent: 0})
	cfg := Config{
		Host:                    "0.0.0.0",
		Port:                    0,
		MaxRequestBodyBytes:     1 << 20,
		GracefulShutdownTimeout: 5 * time.Second,
		SSEKeepalive:            5 * time.Second,
		Version:                 "test",
	}
	srv := NewServer(cfg, h.mgr, nil, newDiscardLogger())
	exitCh, err := srv.Start()
	if err != nil {
		t.Fatalf("server Start: %v", err)
	}
	var once sync.Once
	go func() {
		if e := <-exitCh; e != nil {
			once.Do(func() { t.Errorf("server exited: %v", e) })
		}
	}()
	_, h.apiPort, _ = net.SplitHostPort(srv.BoundAddr())

	// Write nginx config with the real API port.
	tmpl, err := os.ReadFile(filepath.Join("testdata", "nginx", "nginx.conf"))
	if err != nil {
		t.Fatalf("read nginx template: %v", err)
	}
	conf := strings.ReplaceAll(string(tmpl), "{{API_PORT}}", h.apiPort)
	conf = strings.ReplaceAll(conf, "listen 17821;", "listen "+h.ngxPort+";")

	ngxDir, err := os.MkdirTemp("", "nginx-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ngxDir, "nginx.conf"), []byte(conf), 0o644); err != nil {
		t.Fatalf("write nginx.conf: %v", err)
	}

	// Start nginx container.
	exec.Command("docker", "rm", "-f", h.ngxName).Run()
	out, err := exec.Command("docker", "run", "-d",
		"--name", h.ngxName,
		"--add-host=host.docker.internal:192.168.65.254",
		"-p", h.ngxPort+":"+h.ngxPort,
		"--rm",
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/etc/nginx/nginx.conf,readonly", filepath.Join(ngxDir, "nginx.conf")),
		"nginx:alpine",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, string(out))
	}

	// Wait for nginx to become healthy.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := noProxyClient.Get(h.ngxURL + "/health")
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Logf("nginx+api ready on %s (api port %s)", h.ngxURL, h.apiPort)
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	t.Cleanup(func() {
		exec.Command("docker", "stop", h.ngxName).Run()
		srv.Shutdown(context.Background())
		<-exitCh
		os.RemoveAll(ngxDir)
	})

	return h
}

func (h *sharedNginxHarness) createSession() string {
	body, _ := json.Marshal(map[string]any{
		"workdir":   "/tmp/nginx-test",
		"provider":  "anthropic",
		"model":     "claude-sonnet-4-6",
		"ephemeral": true,
	})
	req, _ := http.NewRequest(http.MethodPost, h.ngxURL+"/v1/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := noProxyClient.Do(req)
	if err != nil {
		h.t.Fatalf("create session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var raw bytes.Buffer
		raw.ReadFrom(resp.Body)
		h.t.Fatalf("create session: status %d: %s", resp.StatusCode, raw.String())
	}
	var v sessionView
	json.NewDecoder(resp.Body).Decode(&v)
	return v.ID
}

func (h *sharedNginxHarness) openSSE(sessID string) (*http.Response, *bufio.Reader) {
	req, _ := http.NewRequest(http.MethodGet, h.ngxURL+"/v1/sessions/"+sessID+"/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := noProxyClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		var raw bytes.Buffer
		raw.ReadFrom(resp.Body)
		resp.Body.Close()
		h.t.Fatalf("SSE status %d: %s", resp.StatusCode, raw.String())
	}
	return resp, bufio.NewReader(resp.Body)
}

// TestNginxProxyIntegration runs all nginx reverse-proxy tests against a
// shared API server + nginx container.
func TestNginxProxyIntegration(t *testing.T) {
	if !hasDocker(t) {
		t.Skip("docker not available")
	}

	h := newSharedNginxHarness(t)

	t.Run("buffering_off_streams_immediately", func(t *testing.T) {
		id := h.createSession()
		sess, _ := h.mgr.GetSession(id)

		resp, rd := h.openSSE(id)
		defer resp.Body.Close()

		_ = sess.EmitNow("test_event", map[string]any{"msg": "hello from nginx"})
		ch := make(chan sseFrame, 1)
		go func() {
			f, _ := readNextFrame(t, rd)
			ch <- f
		}()
		select {
		case f := <-ch:
			if f.Event != "test_event" || !strings.Contains(f.Data, "hello from nginx") {
				t.Errorf("unexpected: %v / %q", f.Event, f.Data)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("SSE event did not arrive within 2s")
		}
	})

	t.Run("session_crud_through_proxy", func(t *testing.T) {
		id := h.createSession()

		resp, _ := noProxyClient.Get(h.ngxURL + "/v1/sessions/" + id)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET session: %d", resp.StatusCode)
		}
		var v sessionView
		json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if v.ID != id {
			t.Errorf("id mismatch: %q != %q", v.ID, id)
		}

		resp2, _ := noProxyClient.Get(h.ngxURL + "/v1/sessions")
		var list []sessionView
		json.NewDecoder(resp2.Body).Decode(&list)
		resp2.Body.Close()

		req, _ := http.NewRequest(http.MethodDelete, h.ngxURL+"/v1/sessions/"+id, nil)
		resp3, _ := noProxyClient.Do(req)
		resp3.Body.Close()
		if resp3.StatusCode != http.StatusNoContent {
			t.Errorf("DELETE: %d", resp3.StatusCode)
		}
	})

	t.Run("health_endpoint", func(t *testing.T) {
		resp, _ := noProxyClient.Get(h.ngxURL + "/health")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("health: %d", resp.StatusCode)
		}
		var v map[string]any
		json.NewDecoder(resp.Body).Decode(&v)
		if ok, _ := v["ok"].(bool); !ok {
			t.Error("health ok != true")
		}
	})
}
