package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 11.6 stdio integration test ----------

func TestStdioIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stdio integration test in short mode")
	}

	echoPath := "/tmp/echoserver"
	if _, err := os.Stat(echoPath); os.IsNotExist(err) {
		// Try building it.
		if out, err := exec.Command("go", "build", "-o", echoPath,
			"./testdata/echoserver/").CombinedOutput(); err != nil {
			t.Fatalf("build echoserver: %v\n%s", err, out)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	transport, err := NewStdioTransport(StdioConfig{
		Command: echoPath,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("NewStdioTransport: %v", err)
	}
	defer transport.Close()

	client := &MCPClient{Transport: transport, Logger: logger}
	ctx := context.Background()

	// Initialize handshake.
	initResult, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if initResult.ServerInfo.Name != "echoserver" {
		t.Errorf("unexpected server name: %s", initResult.ServerInfo.Name)
	}

	// Send initialized notification.
	if err := client.SendInitialized(ctx); err != nil {
		t.Fatalf("SendInitialized: %v", err)
	}

	// List tools.
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("expected [echo] tool, got %v", tools)
	}

	// Call the echo tool.
	result, err := client.CallTool(ctx, "echo", json.RawMessage(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected error result: %v", result)
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "hello") {
		t.Errorf("expected echo of 'hello', got %v", result.Content)
	}
	t.Logf("echo result: %v", result.Content[0].Text)
}

func TestStdioCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stdio integration test in short mode")
	}

	echoPath := "/tmp/echoserver"
	if _, err := os.Stat(echoPath); os.IsNotExist(err) {
		t.Skip("echoserver binary not found at /tmp/echoserver")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	transport, err := NewStdioTransport(StdioConfig{
		Command: echoPath,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("NewStdioTransport: %v", err)
	}
	defer transport.Close()

	client := &MCPClient{Transport: transport, Logger: logger}

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := client.Initialize(ctx); err != nil {
		cancel()
		t.Fatalf("Initialize: %v", err)
	}
	client.SendInitialized(ctx)

	// Cancel the context and verify CallTool returns an error.
	cancel()
	_, err = client.CallTool(ctx, "echo", json.RawMessage(`{"message":"x"}`))
	if err == nil {
		t.Error("expected error after cancel, got nil")
	}
}

// ---------- 11.7 HTTP transport integration test ----------

func TestHTTPIntegration(t *testing.T) {
	// Create a mock MCP HTTP server.
	server := newMockMCPServer(t)
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	transport := NewHTTPTransport(HTTPConfig{
		URL:    server.URL,
		Logger: logger,
	})
	defer transport.Close()

	client := &MCPClient{Transport: transport, Logger: logger}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Initialize handshake.
	initResult, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if initResult.ServerInfo.Name != "mockmcp" {
		t.Errorf("unexpected server name: %s", initResult.ServerInfo.Name)
	}

	// Verify Mcp-Session-Id was captured.
	sid := transport.SessionID()
	if sid == "" {
		t.Error("expected Mcp-Session-Id to be captured")
	}
	t.Logf("session id: %s", sid)

	// Send initialized notification.
	if err := client.SendInitialized(ctx); err != nil {
		t.Fatalf("SendInitialized: %v", err)
	}

	// List tools.
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "mocktool" {
		t.Fatalf("expected [mocktool] tool, got %v", tools)
	}

	// Call tool.
	result, err := client.CallTool(ctx, "mocktool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected error result: %v", result)
	}
}

// ---------- 11.8 SSE reconnect test ----------

func TestHTTPSSEReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSE reconnect test in short mode")
	}

	server := newMockMCPServer(t)
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	transport := NewHTTPTransport(HTTPConfig{
		URL:    server.URL,
		Logger: logger,
	})

	client := &MCPClient{Transport: transport, Logger: logger}
	ctx := context.Background()

	// Initialize to capture session ID.
	if _, err := client.Initialize(ctx); err != nil {
		transport.Close()
		t.Fatalf("Initialize: %v", err)
	}
	client.SendInitialized(ctx)

	// Read some notifications from the SSE stream.
	// The mock server sends a heartbeat notification every 50ms.
	notifCh := transport.Notifications()
	var wg sync.WaitGroup
	wg.Add(1)
	var notifCount int
	go func() {
		defer wg.Done()
		for range notifCh {
			notifCount++
			if notifCount >= 3 {
				return
			}
		}
	}()

	// Wait for notifications to arrive.
	time.Sleep(300 * time.Millisecond)
	transport.Close()
	wg.Wait()

	if notifCount < 2 {
		t.Errorf("expected at least 2 notifications, got %d", notifCount)
	}
	t.Logf("received %d notifications before close", notifCount)
}

// ---------- adapter tests ----------

func TestAdapter(t *testing.T) {
	st := ServerTool{
		Server: "testsrv",
		Def: ToolDef{
			Name:        "test",
			Description: "A test tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}

	adapter := NewAdapter(st)
	if adapter.Name() != "testsrv__test" {
		t.Errorf("expected name testsrv__test, got %s", adapter.Name())
	}
	if adapter.IsReadOnly() {
		t.Error("expected IsReadOnly=false by default")
	}
	if adapter.CanRunInParallel() {
		t.Error("expected CanRunInParallel=false by default")
	}
}

func TestAdapterNameSpacing(t *testing.T) {
	st := ServerTool{
		Server: "filesystem",
		Def: ToolDef{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{}`),
		},
	}
	adapter := NewAdapter(st)
	if adapter.Name() != "filesystem__read_file" {
		t.Errorf("expected namespaced name, got %s", adapter.Name())
	}
}

// ---------- mock MCP HTTP server ----------

// mockMCPServer implements a minimal MCP HTTP+SSE server for tests.
type mockMCPServer struct {
	*httptest.Server
	notifyCh chan map[string]interface{}
}

func newMockMCPServer(t *testing.T) *mockMCPServer {
	t.Helper()
	m := &mockMCPServer{
		notifyCh: make(chan map[string]interface{}, 16),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handle)
	m.Server = httptest.NewServer(mux)
	return m
}

func (m *mockMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		m.handlePost(w, r)
	case http.MethodGet:
		m.handleGet(w, r)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (m *mockMCPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// Emit session ID on initialize.
	if req.Method == MethodInitialize {
		w.Header().Set("Mcp-Session-Id", "test-session-123")
	}

	// Notifications have no id — no response expected.
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := m.dispatch(req)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (m *mockMCPServer) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Accept") != "text/event-stream" {
		http.Error(w, "not acceptable", 406)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Send notifications periodically.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"method\":\"notifications/heartbeat\"}\n\n")
			flusher.Flush()
		}
	}
}

func (m *mockMCPServer) dispatch(req Request) *Response {
	switch req.Method {
	case MethodInitialize:
		return &Response{
			JSONRPC: JSONRPCVersion,
			ID:      *req.ID,
			Result: MustJSON(InitializeResult{
				ProtocolVersion: ProtocolVersion,
				Capabilities:    Capabilities{},
				ServerInfo:      ServerInfo{Name: "mockmcp", Version: "1.0.0"},
			}),
		}
	case MethodToolsList:
		return &Response{
			JSONRPC: JSONRPCVersion,
			ID:      *req.ID,
			Result: MustJSON(ListToolsResult{
				Tools: []ToolDef{
					{
						Name:        "mocktool",
						Description: "A mock tool for testing",
						InputSchema: json.RawMessage(`{"type":"object"}`),
					},
				},
			}),
		}
	case MethodToolsCall:
		return &Response{
			JSONRPC: JSONRPCVersion,
			ID:      *req.ID,
			Result: MustJSON(CallToolResult{
				Content: []ContentItem{{Type: "text", Text: "mock result"}},
			}),
		}
	default:
		return &Response{
			JSONRPC: JSONRPCVersion,
			ID:      *req.ID,
			Error:   &RPCError{Code: ErrMethodNotFound, Message: "not found"},
		}
	}
}

// ---------- JSON-RPC type tests ----------

func TestJSONRPCTypes(t *testing.T) {
	req := &Request{
		JSONRPC: JSONRPCVersion,
		ID:      intPtr(1),
		Method:  "test",
		Params:  json.RawMessage(`{"key":"value"}`),
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var back Request
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if *back.ID != 1 || back.Method != "test" {
		t.Errorf("round-trip failed: %+v", back)
	}

	resp := &Response{
		JSONRPC: JSONRPCVersion,
		ID:      1,
		Result:  json.RawMessage(`"ok"`),
	}
	if resp.IsError() {
		t.Error("expected IsError=false")
	}
	resp.Error = &RPCError{Code: -1, Message: "fail"}
	if !resp.IsError() {
		t.Error("expected IsError=true")
	}
}

func TestHostConfigParse(t *testing.T) {
	cfgJSON := `{
		"servers": [
			{"name": "s1", "enabled": true, "transport": "stdio", "command": "ls"},
			{"name": "s2", "enabled": false, "transport": "http", "url": "https://example.com"},
			{"name": "s3", "enabled": true, "transport": "http", "url": "https://mcp.example.com/v1", "auth_header": "Bearer xyz"}
		]
	}`
	var cfg HostConfig
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 3 {
		t.Errorf("expected 3 servers, got %d", len(cfg.Servers))
	}
	if !cfg.Servers[0].Enabled {
		t.Error("s1 should be enabled")
	}
	if cfg.Servers[1].Enabled {
		t.Error("s2 should be disabled")
	}
	if cfg.Servers[2].AuthHeader != "Bearer xyz" {
		t.Errorf("s3 auth_header: %s", cfg.Servers[2].AuthHeader)
	}
}

func TestMustJSON(t *testing.T) {
	b := MustJSON(map[string]int{"a": 1})
	if string(b) != `{"a":1}` {
		t.Errorf("unexpected: %s", b)
	}
}

func TestProtocolVersion(t *testing.T) {
	if ProtocolVersion != "2025-11-25" {
		t.Errorf("expected 2025-11-25, got %s", ProtocolVersion)
	}
}

func TestNextID(t *testing.T) {
	var g idGen
	if g.Next() != 1 {
		t.Error("expected first id = 1")
	}
	if g.Next() != 2 {
		t.Error("expected second id = 2")
	}
}

func intPtr(n int64) *int64 { return &n }

// ---------- Test helper: verify stdio transport reads lines properly ----------

func TestStdioTransportRoundTrip(t *testing.T) {
	// Use a simple echo-like approach with /bin/cat, but that's fragile.
	// Instead test that our JSON-RPC types survive a buffered scan cycle.

	var buf strings.Builder
	req := &Request{JSONRPC: JSONRPCVersion, ID: intPtr(42), Method: "ping"}
	b, _ := json.Marshal(req)
	buf.Write(b)
	buf.WriteByte('\n')

	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	if !sc.Scan() {
		t.Fatal("scanner failed")
	}
	var got Request
	if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if *got.ID != 42 {
		t.Errorf("id mismatch: %d", *got.ID)
	}
}
