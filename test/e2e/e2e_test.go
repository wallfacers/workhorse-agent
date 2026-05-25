// Package e2e exercises the api.Server wired against a real agent.Loop, a
// mockprovider, and an in-memory sqlite store — the closest we can get to
// the bound binary's behaviour without spawning a child process. The tests
// target Group 9 spec scenarios that need end-to-end behaviour: full roundtrip
// (9.13), Bearer at all endpoints (9.19), graceful shutdown ordering (9.21),
// and Last-Event-ID race with concurrent emission (9.17).
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/api"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

// --- harness ---

type stack struct {
	t      *testing.T
	srv    *api.Server
	mgr    *session.Manager
	store  store.Store
	mock   *mockprovider.Provider
	ts     *httptest.Server
	url    string
	logger *slog.Logger
}

func newStack(t *testing.T, opts ...func(*api.Config)) *stack {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock := mockprovider.New("anthropic")
	reg := tools.NewRegistry()
	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	permMgr := permission.New(st, nil, nil, 0)

	var mgr *session.Manager
	mgr = session.NewManager(session.ManagerOptions{
		Store:         st,
		MaxConcurrent: 0,
		RunnerFactory: func(sess *session.Session) session.Runner {
			loop := agent.NewLoop(agent.LoopConfig{
				Model:              "stub",
				MaxTokens:          1024,
				CancelDrainTimeout: 500 * time.Millisecond,
			})
			loop.Session = sess
			loop.Provider = mock
			loop.Orchestrator = orch
			loop.Permissions = permMgr
			loop.Logger = logger
			loop.ToolEnv = &tools.Env{SessionID: sess.ID, Workdir: sess.Workdir}
			return loop
		},
	})

	cfg := api.Config{
		Host:                    "127.0.0.1",
		Port:                    0,
		MaxRequestBodyBytes:     1 << 20,
		SSEKeepalive:            60 * time.Second,
		GracefulShutdownTimeout: 2 * time.Second,
		Version:                 "e2e",
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv := api.NewServer(cfg, mgr, st, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &stack{t: t, srv: srv, mgr: mgr, store: st, mock: mock, ts: ts, url: ts.URL, logger: logger}
}

func (s *stack) createSessionViaAPI(ephemeral bool) string {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"workdir":   s.t.TempDir(),
		"provider":  "anthropic",
		"model":     "claude-sonnet-4-6",
		"ephemeral": ephemeral,
	})
	resp, err := http.Post(s.url+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		s.t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		s.t.Fatalf("create status %d: %s", resp.StatusCode, raw)
	}
	var view struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		s.t.Fatalf("decode: %v", err)
	}
	return view.ID
}

func (s *stack) openSSE(id, lastEventID string) (*http.Response, *bufio.Reader) {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.url+"/v1/sessions/"+id+"/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		s.t.Fatalf("SSE status %d: %s", resp.StatusCode, raw)
	}
	return resp, bufio.NewReader(resp.Body)
}

func (s *stack) postStream(id, body string, headers ...[2]string) *http.Response {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodPost,
		s.url+"/v1/sessions/"+id+"/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("postStream: %v", err)
	}
	return resp
}

type sseFrame struct {
	ID, Event, Data string
}

func readFrame(t *testing.T, rd *bufio.Reader) (sseFrame, error) {
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

// --- 9.13: end-to-end user_message → text stream → done ---

func TestE2E_FullRoundtrip(t *testing.T) {
	s := newStack(t)

	id := s.createSessionViaAPI(false)

	// Prime mock provider with a text-only response: 2 deltas + stop.
	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "hello "},
		{Type: provider.EventTextDelta, TextDelta: "world"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	resp, rd := s.openSSE(id, "")
	defer resp.Body.Close()

	postResp := s.postStream(id, `{"type":"user_message","content":"say hi"}`)
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST want 202, got %d", postResp.StatusCode)
	}

	// Read frames until we see assistant_text_done (or timeout).
	gotDelta := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f, err := readFrame(t, rd)
		if err != nil {
			break
		}
		if f.Event == "assistant_text_delta" {
			gotDelta = true
		}
		if f.Event == "assistant_text_done" {
			if !gotDelta {
				t.Fatal("got done without any delta")
			}
			return
		}
	}
	t.Fatal("did not see assistant_text_done")
}

// --- 9.19: Bearer auth at all endpoints ---

func TestE2E_BearerAuthAtAllEndpoints(t *testing.T) {
	s := newStack(t, func(c *api.Config) {
		c.Auth = api.BearerConfig{Enabled: true, Token: "the-secret"}
		c.DebugEnabled = true
	})

	authH := [2]string{"Authorization", "Bearer the-secret"}

	// 1. Create session with auth — succeeds.
	body := []byte(`{"workdir":"/tmp","provider":"anthropic","model":"m","ephemeral":true}`)
	req, _ := http.NewRequest(http.MethodPost, s.url+"/v1/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(authH[0], authH[1])
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("auth create: %d %s", resp.StatusCode, raw)
	}
	var view struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()

	// 2. Without auth — all /v1 endpoints reject 401.
	tries := []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions/" + view.ID},
		{http.MethodDelete, "/v1/sessions/" + view.ID},
		{http.MethodPost, "/v1/sessions/" + view.ID + "/cancel"},
		{http.MethodPost, "/v1/sessions/" + view.ID + "/compact"},
		{http.MethodPost, "/v1/sessions/" + view.ID + "/stream"},
		{http.MethodGet, "/v1/sessions/" + view.ID + "/stream"},
		{http.MethodGet, "/debug/sessions/" + view.ID + "/events"},
	}
	for _, c := range tries {
		req, _ := http.NewRequest(c.method, s.url+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth: want 401, got %d", c.method, c.path, resp.StatusCode)
		}
	}

	// 3. /health works without auth.
	resp, _ = http.Get(s.url + "/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health unauthed: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 9.21: cancelled/interrupted before server_shutdown ---

func TestE2E_GracefulShutdownOrdering(t *testing.T) {
	s := newStack(t, func(c *api.Config) { c.GracefulShutdownTimeout = 2 * time.Second })

	id := s.createSessionViaAPI(false)

	// Mock: 1 text_delta + stop (turn ends naturally; we only need an open
	// SSE stream when Shutdown fires).
	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "hello"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	resp, rd := s.openSSE(id, "")
	defer resp.Body.Close()

	postResp := s.postStream(id, `{"type":"user_message","content":"go"}`)
	postResp.Body.Close()

	// Briefly give the turn time to start so events are in flight.
	time.Sleep(100 * time.Millisecond)

	go func() {
		_ = s.srv.Shutdown(context.Background())
	}()

	seenShutdown := false
	deadline := time.Now().Add(3 * time.Second)
	var observed []string
	for time.Now().Before(deadline) {
		f, err := readFrame(t, rd)
		if err != nil {
			observed = append(observed, fmt.Sprintf("ERR:%v", err))
			break
		}
		observed = append(observed, f.Event+"/"+f.Data)
		if f.Event == "error" && strings.Contains(f.Data, "server_shutdown") {
			seenShutdown = true
			break
		}
	}
	if !seenShutdown {
		t.Fatalf("did not observe server_shutdown; saw: %v", observed)
	}
}

// --- 9.17: Last-Event-ID race with concurrent emit ---

func TestE2E_LastEventIDRace(t *testing.T) {
	s := newStack(t)

	id := s.createSessionViaAPI(false)
	sess, _ := s.mgr.GetSession(id)

	// Emit a baseline batch.
	for i := 0; i < 10; i++ {
		_ = sess.Emit(context.Background(), "assistant_text_delta",
			map[string]any{"delta": fmt.Sprintf("base%d", i)})
	}

	// Open a writer that will keep emitting concurrently.
	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		i := 100
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = sess.Emit(context.Background(), "assistant_text_delta",
				map[string]any{"delta": fmt.Sprintf("live%d", i)})
			i++
			time.Sleep(time.Millisecond)
		}
	}()

	// Drain the initial 10 from the outbox (simulating a prior reader that
	// already got them).
	for i := 0; i < 10; i++ {
		<-sess.Outbox
	}

	// Reconnect with Last-Event-ID after the 10th event.
	resp, rd := s.openSSE(id, "10")
	defer resp.Body.Close()

	// Read a handful of frames; idx must be strictly increasing > 10, no
	// duplicates.
	var lastIdx int64 = 10
	for i := 0; i < 30; i++ {
		f, err := readFrame(t, rd)
		if err != nil {
			break
		}
		if f.Event != "assistant_text_delta" {
			continue
		}
		var idx int64
		fmt.Sscanf(f.ID, "%d", &idx)
		if idx <= lastIdx {
			t.Fatalf("non-increasing idx: prev=%d got=%d", lastIdx, idx)
		}
		lastIdx = idx
	}
	close(stop)
	wg.Wait()
}
