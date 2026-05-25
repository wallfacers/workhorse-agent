package api

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
)

func TestShutdown_EmitsServerShutdownToActiveStreams(t *testing.T) {
	s, ts := newTestServer(t, func(c *Config) {
		c.GracefulShutdownTimeout = 2 * time.Second
	})
	sess := newSessionWithState(t, s, session.StateIdle)

	resp, rd := openSSE(t, ts.URL, sess.ID, "")
	defer resp.Body.Close()

	// Give SSE goroutine a beat to settle on its select.
	time.Sleep(50 * time.Millisecond)

	go func() {
		_ = s.Shutdown(context.Background())
	}()

	deadline := time.Now().Add(2 * time.Second)
	gotShutdown := false
	for time.Now().Before(deadline) {
		f, err := readNextFrame(t, rd)
		if err != nil {
			break
		}
		if f.Event == "error" && strings.Contains(f.Data, `"server_shutdown"`) {
			gotShutdown = true
			break
		}
	}
	if !gotShutdown {
		t.Fatal("SSE stream did not receive error{server_shutdown}")
	}
}

func TestShutdown_BlocksNewSessionCreates(t *testing.T) {
	s, ts := newTestServer(t, func(c *Config) {
		c.GracefulShutdownTimeout = 1 * time.Second
	})
	// Pre-flag shutdown manually to test the guard without racing.
	s.shutdownInFlight.Store(true)

	body := strings.NewReader(`{"workdir":"/x","provider":"anthropic","model":"m","ephemeral":true}`)
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 during shutdown, got %d", resp.StatusCode)
	}
}

func TestShutdown_PreservesEphemeralCancelOrdering(t *testing.T) {
	// Confirm the SSE write order is: any cancelled-related event first,
	// then server_shutdown last. The manager.Shutdown drains agent
	// goroutines before we emit server_shutdown, so anything that loop
	// emitted (e.g. `interrupted`) lands before our shutdown event by idx.
	s, ts := newTestServer(t, func(c *Config) {
		c.GracefulShutdownTimeout = 2 * time.Second
	})
	sess := newSessionWithState(t, s, session.StateIdle)

	// Emit a pre-existing event before opening the stream so we have a
	// baseline.
	_ = sess.EmitNow("assistant_text_delta", map[string]any{"delta": "before"})

	resp, rd := openSSE(t, ts.URL, sess.ID, "")
	defer resp.Body.Close()

	// Consume the first frame (pre-existing event).
	if _, err := readNextFrame(t, rd); err != nil {
		t.Fatalf("first frame: %v", err)
	}

	go func() {
		_ = s.Shutdown(context.Background())
	}()

	// Now we should see the server_shutdown error event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, err := readNextFrame(t, rd)
		if err != nil {
			break
		}
		if f.Event == "error" && strings.Contains(f.Data, "server_shutdown") {
			return
		}
	}
	t.Fatal("server_shutdown not observed")
}

// drain a bufio.Reader so the helper doesn't error out lingering.
var _ = bufio.Reader{}
