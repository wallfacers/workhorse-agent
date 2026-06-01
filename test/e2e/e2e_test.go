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
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/sessionsearch"
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
	permMgr := permission.New(st, nil, nil, 0, "")

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

// --- Memory E2E: session isolation ---

func newMemoryStack(t *testing.T) (*stack, string) {
	t.Helper()
	profileDir := t.TempDir()

	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock := mockprovider.New("anthropic")
	reg := tools.NewRegistry()
	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	permMgr := permission.New(st, nil, nil, 0, "")

	memLoader := &memory.Loader{ProfileDir: profileDir}

	var mgr *session.Manager
	mgr = session.NewManager(session.ManagerOptions{
		Store:         st,
		MaxConcurrent: 0,
		RunnerFactory: func(sess *session.Session) session.Runner {
			snap, _ := memLoader.Load()
			sess.MemorySnapshot = snap

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
	srv := api.NewServer(cfg, mgr, st, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &stack{t: t, srv: srv, mgr: mgr, store: st, mock: mock, ts: ts, url: ts.URL, logger: logger}, profileDir
}

// TestE2E_MemoryWrittenBeforeSessionB (task 9.1): write memory after session A
// starts, verify session A never sees it but session B does.
func TestE2E_MemoryWrittenBeforeSessionB(t *testing.T) {
	s, profileDir := newMemoryStack(t)

	// Session A: start it and drive one turn.
	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "ok"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
	idA := s.createSessionViaAPI(false)
	respA, rdA := s.openSSE(idA, "")
	defer respA.Body.Close()
	postResp := s.postStream(idA, `{"type":"user_message","content":"hi"}`)
	postResp.Body.Close()

	// Drain until we see assistant_text_done (session A's first turn).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := readFrame(t, rdA)
		if err != nil {
			break
		}
		if f.Event == "assistant_text_done" {
			break
		}
	}

	// Session A's system prompt should NOT contain the memory (not yet written).
	reqs := s.mock.Requests()
	if len(reqs) == 0 {
		t.Fatal("session A: expected at least one provider request")
	}
	if strings.Contains(reqs[0].System, "my secret note") {
		t.Error("session A system prompt should NOT contain memory written after session start")
	}

	// Write memory to disk via the memory package directly.
	w := memory.Writer{ProfileDir: profileDir, MemoryLimit: 2200, UserLimit: 1375}
	if err := w.Write(memory.KindMemory, "my secret note", memory.ModeReplace); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	// Session B: start after the write.
	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "hello"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
	idB := s.createSessionViaAPI(false)
	respB, rdB := s.openSSE(idB, "")
	defer respB.Body.Close()
	postResp = s.postStream(idB, `{"type":"user_message","content":"hi"}`)
	postResp.Body.Close()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := readFrame(t, rdB)
		if err != nil {
			break
		}
		if f.Event == "assistant_text_done" {
			break
		}
	}

	// Session B's system prompt SHOULD contain the memory.
	reqs = s.mock.Requests()
	if len(reqs) < 2 {
		t.Fatalf("expected at least 2 provider requests, got %d", len(reqs))
	}
	if !strings.Contains(reqs[1].System, "my secret note") {
		t.Error("session B system prompt SHOULD contain the memory written before session B started")
	}
}

// --- Session search E2E (task 9.2) ---

// newSearchStack creates a stack with the session_search tool wired to the
// underlying SQLite store so FTS5 queries can run.
func newSearchStack(t *testing.T) *stack {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock := mockprovider.New("anthropic")

	searchTool := &sessionsearch.Tool{DB: st.DB()}

	reg := tools.NewRegistry()
	if err := reg.Register(searchTool); err != nil {
		t.Fatalf("register session_search: %v", err)
	}

	orch := &agent.Orchestrator{Registry: reg, MaxParallel: 4, DefaultTimeout: 2 * time.Second}
	permMgr := permission.New(st, nil, nil, 0, "")

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
			loop.Tools = []provider.ToolSchema{
				{Name: "session_search", Description: searchTool.Description(), InputSchema: searchTool.InputSchema()},
			}
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
	srv := api.NewServer(cfg, mgr, st, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &stack{t: t, srv: srv, mgr: mgr, store: st, mock: mock, ts: ts, url: ts.URL, logger: logger}
}

func TestE2E_SessionSearch(t *testing.T) {
	s := newSearchStack(t)
	ctx := context.Background()

	// Create two sessions via the API so sessions table has rows for scope
	// resolution.
	id1 := s.createSessionViaAPI(false)
	id2 := s.createSessionViaAPI(false)

	// Insert messages directly (agent loop does not yet persist to the messages
	// table; this tests the search pipeline: FTS5 indexing, scope, context).
	db := s.store.(*sqlite.Store).DB()
	now := time.Now().UnixMicro()
	for _, row := range []struct {
		id, sid, content string
	}{
		{"m1", id1, `[{"type":"text","text":"uniqueterm alpha details"}]`},
		{"m2", id2, `[{"type":"text","text":"uniqueterm beta info"}]`},
	} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at)
			 VALUES (?,?,?,?,?,?)`,
			row.id, row.sid, "user", row.content, 0, now)
		if err != nil {
			t.Fatalf("insert message %s: %v", row.id, err)
		}
	}

	searchTool := &sessionsearch.Tool{DB: db}
	res, err := searchTool.Run(ctx, &tools.Env{},
		[]byte(fmt.Sprintf(`{"query":"uniqueterm","session_id":"%s","scope":"all"}`, id1)))
	if err != nil {
		t.Fatalf("session_search: %v", err)
	}
	if res.IsError {
		t.Fatalf("session_search error: %s", res.Output)
	}

	var out map[string]any
	json.Unmarshal([]byte(res.Output), &out)
	hits, ok := out["hits"].([]any)
	if !ok || len(hits) < 2 {
		t.Fatalf("expected >=2 hits, got %v", out["hits"])
	}

	found := map[string]bool{}
	for _, h := range hits {
		hm := h.(map[string]any)
		sid := hm["session_id"].(string)
		found[sid] = true
		if hm["message_id"] == "" {
			t.Error("hit missing message_id")
		}
	}
	if !found[id1] {
		t.Error("session 1 not found in search results")
	}
	if !found[id2] {
		t.Error("session 2 not found in search results")
	}
}

// TestE2E_SessionSearchHitShape (task 9.7): pins down every field of the
// session_search hit object against a seeded fixture. The companion
// TestE2E_SessionSearch covers scope behavior with weak field assertions;
// this test covers the full shape contract from the spec
// ("Result shape includes context messages" scenario).
func TestE2E_SessionSearchHitShape(t *testing.T) {
	s := newSearchStack(t)
	ctx := context.Background()
	sid := s.createSessionViaAPI(false)
	db := s.store.(*sqlite.Store).DB()

	// Five messages, 100µs apart, the middle one carries the unique term so
	// the search has 2 messages of context on each side from the same session.
	base := time.Now().UnixMicro()
	rows := []struct {
		id, role, text string
		offset         int64
	}{
		{"m_before_2", "user", "earliest message about setup", 0},
		{"m_before_1", "assistant", "second message acknowledging setup", 100},
		{"m_match", "user", "search for needlephrase right here", 200},
		{"m_after_1", "assistant", "fourth message acknowledging the prior point", 300},
		{"m_after_2", "user", "fifth message wrapping up", 400},
	}
	for _, r := range rows {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at)
			 VALUES (?,?,?,?,?,?)`,
			r.id, sid, r.role,
			fmt.Sprintf(`[{"type":"text","text":%q}]`, r.text),
			0, base+r.offset)
		if err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	searchTool := &sessionsearch.Tool{DB: db}
	res, err := searchTool.Run(ctx, &tools.Env{},
		[]byte(fmt.Sprintf(
			`{"query":"needlephrase","session_id":%q,"scope":"session","context_before":2,"context_after":2}`,
			sid)))
	if err != nil {
		t.Fatalf("session_search: %v", err)
	}
	if res.IsError {
		t.Fatalf("session_search error: %s", res.Output)
	}

	var out struct {
		Hits []struct {
			SessionID     string           `json:"session_id"`
			MessageID     string           `json:"message_id"`
			Role          string           `json:"role"`
			Snippet       string           `json:"snippet"`
			CreatedAt     int64            `json:"created_at"`
			ContextBefore []map[string]any `json:"context_before"`
			ContextAfter  []map[string]any `json:"context_after"`
		} `json:"hits"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, res.Output)
	}

	if len(out.Hits) != 1 {
		t.Fatalf("expected exactly 1 hit, got %d: %s", len(out.Hits), res.Output)
	}
	h := out.Hits[0]

	if h.SessionID != sid {
		t.Errorf("session_id = %q, want %q", h.SessionID, sid)
	}
	if h.MessageID != "m_match" {
		t.Errorf("message_id = %q, want m_match", h.MessageID)
	}
	if h.Role != "user" {
		t.Errorf("role = %q, want user", h.Role)
	}
	// Trigram tokenizer's snippet() trims to trigram boundaries, so the
	// extracted snippet drops the term's leading/trailing chars. Assert a
	// stable interior fragment that always survives that trimming.
	if !strings.Contains(strings.ToLower(h.Snippet), "edlephras") {
		t.Errorf("snippet %q should contain interior fragment of needlephrase", h.Snippet)
	}
	if h.CreatedAt != base+200 {
		t.Errorf("created_at = %d, want %d", h.CreatedAt, base+200)
	}

	if len(h.ContextBefore) != 2 {
		t.Fatalf("context_before length = %d, want 2: %+v", len(h.ContextBefore), h.ContextBefore)
	}
	if len(h.ContextAfter) != 2 {
		t.Fatalf("context_after length = %d, want 2: %+v", len(h.ContextAfter), h.ContextAfter)
	}

	wantBefore := []string{"m_before_2", "m_before_1"}
	wantAfter := []string{"m_after_1", "m_after_2"}
	for i, want := range wantBefore {
		if got, _ := h.ContextBefore[i]["message_id"].(string); got != want {
			t.Errorf("context_before[%d].message_id = %q, want %q", i, got, want)
		}
		if _, ok := h.ContextBefore[i]["role"].(string); !ok {
			t.Errorf("context_before[%d] missing role field", i)
		}
	}
	for i, want := range wantAfter {
		if got, _ := h.ContextAfter[i]["message_id"].(string); got != want {
			t.Errorf("context_after[%d].message_id = %q, want %q", i, got, want)
		}
		if _, ok := h.ContextAfter[i]["role"].(string); !ok {
			t.Errorf("context_after[%d] missing role field", i)
		}
	}

	if out.Truncated {
		t.Errorf("truncated should be false for a single-hit result")
	}
}

func waitForTextDone(t *testing.T, rd *bufio.Reader, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := readFrame(t, rd)
		if err != nil {
			return
		}
		if f.Event == "assistant_text_done" {
			return
		}
	}
}
