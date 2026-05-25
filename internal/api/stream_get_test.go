package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// sseFrame is one parsed event from an SSE stream — empty event/data fields
// stay empty when the source line was missing (or was a `:` comment line).
type sseFrame struct {
	ID    string
	Event string
	Data  string
}

// readNextFrame consumes one `\n\n`-terminated SSE block from rd. Comment
// lines starting with `:` are returned as a frame with empty ID/Event and
// the raw line in Data (with the leading `:` stripped).
func readNextFrame(t *testing.T, rd *bufio.Reader) (sseFrame, error) {
	t.Helper()
	var f sseFrame
	gotAny := false
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return f, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if gotAny {
				return f, nil
			}
			continue
		}
		gotAny = true
		switch {
		case strings.HasPrefix(line, ":"):
			f.Data = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "id: "):
			f.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			f.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			f.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// openSSE issues a GET against the SSE endpoint with the given Last-Event-ID
// header (empty to omit). The test is responsible for closing the response.
func openSSE(t *testing.T, baseURL, sessID, lastEventID string) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/sessions/"+sessID+"/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("SSE status %d: %s", resp.StatusCode, raw)
	}
	return resp, bufio.NewReader(resp.Body)
}

func TestStreamGet_404_NotFound(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/sessions/01HNOPE/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestStreamGet_406_NotAcceptable(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateIdle)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/sessions/"+sess.ID+"/stream", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("want 406, got %d", resp.StatusCode)
	}
}

func TestStreamGet_HeadersAndLiveEvents(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateIdle)

	resp, rd := openSSE(t, ts.URL, sess.ID, "")
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type: %q", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("Cache-Control: %q", resp.Header.Get("Cache-Control"))
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("X-Accel-Buffering: %q", resp.Header.Get("X-Accel-Buffering"))
	}

	// Push one event after the connection is established.
	time.Sleep(50 * time.Millisecond)
	_ = sess.EmitNow("assistant_text_delta", map[string]any{"delta": "hello\nworld"})

	f, err := readNextFrame(t, rd)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.Event != "assistant_text_delta" {
		t.Fatalf("event: %q", f.Event)
	}
	// Embedded \n in payload must be JSON-escaped (\n literal in data line).
	if strings.Contains(f.Data, "hello\nworld") {
		t.Fatalf("data must escape \\n; got real newline in: %q", f.Data)
	}
	if !strings.Contains(f.Data, `hello\nworld`) {
		t.Fatalf("data missing escaped newline: %q", f.Data)
	}
	if _, err := strconv.Atoi(f.ID); err != nil {
		t.Fatalf("id not numeric: %q", f.ID)
	}
}

func TestStreamGet_Keepalive(t *testing.T) {
	s, ts := newTestServer(t, func(c *Config) { c.SSEKeepalive = 50 * time.Millisecond })
	sess := newSessionWithState(t, s, session.StateIdle)

	resp, rd := openSSE(t, ts.URL, sess.ID, "")
	defer resp.Body.Close()

	// First frame should be a keep-alive comment (no events have been
	// emitted, so the ticker fires before any data).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		f, err := readNextFrame(t, rd)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if f.Event == "" && strings.Contains(f.Data, "keep-alive") {
			return
		}
	}
	t.Fatal("did not see keep-alive within 500ms")
}

func TestStreamGet_LastEventID_HeaderReplay(t *testing.T) {
	st := newSQLiteStore(t)
	mgr := session.NewManager(session.ManagerOptions{Store: st})
	s := NewServer(Config{
		Host:                "127.0.0.1",
		Port:                0,
		MaxRequestBodyBytes: 1 << 20,
		Version:             "test",
	}, mgr, st, newDiscardLogger())
	ts := httptestServer(t, s)

	sess, err := mgr.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: false, Model: "m", ProviderName: "anthropic",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Emit 5 events. Each writes through to the events table because the
	// session is non-ephemeral and store-backed.
	for i := 0; i < 5; i++ {
		if err := sess.Emit(context.Background(), "assistant_text_delta",
			map[string]any{"delta": fmt.Sprintf("e%d", i)}); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	// Drain the outbox so live mode doesn't re-send those 5 events on
	// resume — simulates a previous SSE writer that already consumed them.
	for i := 0; i < 5; i++ {
		<-sess.Outbox
	}

	// First two events have idx 1 and 2 (AUTOINCREMENT, fresh table).
	// Resume from after idx 2 and we should receive 3, 4, 5 only.
	resp, rd := openSSE(t, ts.URL, sess.ID, "2")
	defer resp.Body.Close()

	gotIdx := []string{}
	for i := 0; i < 3; i++ {
		f, err := readNextFrame(t, rd)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Event != "assistant_text_delta" {
			t.Fatalf("frame %d event: %q", i, f.Event)
		}
		gotIdx = append(gotIdx, f.ID)
	}
	if len(gotIdx) != 3 || gotIdx[0] != "3" || gotIdx[2] != "5" {
		t.Fatalf("replay idx mismatch: %v", gotIdx)
	}
}

func TestStreamGet_LastEventID_QueryParamReplay(t *testing.T) {
	st := newSQLiteStore(t)
	mgr := session.NewManager(session.ManagerOptions{Store: st})
	s := NewServer(Config{
		Host:                "127.0.0.1",
		Port:                0,
		MaxRequestBodyBytes: 1 << 20,
		Version:             "test",
	}, mgr, st, newDiscardLogger())
	ts := httptestServer(t, s)

	sess, _ := mgr.CreateSession(context.Background(), session.Options{
		Workdir: "/tmp", Ephemeral: false, Model: "m", ProviderName: "anthropic",
	})
	for i := 0; i < 3; i++ {
		_ = sess.Emit(context.Background(), "assistant_text_delta",
			map[string]any{"delta": fmt.Sprintf("q%d", i)})
		<-sess.Outbox // drain
	}

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/v1/sessions/"+sess.ID+"/stream?last_event_id=1", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	gotIdx := []string{}
	for i := 0; i < 2; i++ {
		f, err := readNextFrame(t, rd)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		gotIdx = append(gotIdx, f.ID)
	}
	if gotIdx[0] != "2" || gotIdx[1] != "3" {
		t.Fatalf("query-param replay idx mismatch: %v", gotIdx)
	}
}

func TestStreamGet_SingleFlowSupersede(t *testing.T) {
	s, ts := newTestServer(t)
	sess := newSessionWithState(t, s, session.StateIdle)

	resp1, rd1 := openSSE(t, ts.URL, sess.ID, "")
	defer resp1.Body.Close()

	// New GET supersedes resp1.
	resp2, rd2 := openSSE(t, ts.URL, sess.ID, "")
	defer resp2.Body.Close()

	// Old stream must receive a `: superseded` comment then close.
	deadline := time.Now().Add(1 * time.Second)
	sawSuperseded := false
	for time.Now().Before(deadline) {
		f, err := readNextFrame(t, rd1)
		if err != nil {
			break
		}
		if strings.Contains(f.Data, "superseded") {
			sawSuperseded = true
			break
		}
	}
	if !sawSuperseded {
		t.Fatal("old SSE did not get supersede signal")
	}

	// New stream stays live: emit one event and read it.
	_ = sess.EmitNow("pong", nil)
	f, err := readNextFrame(t, rd2)
	if err != nil {
		t.Fatalf("new stream read: %v", err)
	}
	if f.Event != "pong" {
		t.Fatalf("new stream first event: %q", f.Event)
	}
}

// --- helpers ---

func newSQLiteStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// httptestServer is a thin wrapper around httptest.NewServer + Cleanup.
func httptestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}
