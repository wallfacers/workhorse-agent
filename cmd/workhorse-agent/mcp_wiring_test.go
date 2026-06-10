package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/mcp"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// fakeMCP is a minimal Streamable HTTP MCP server exposing the given tools.
// It records the Authorization header of the last request.
type fakeMCP struct {
	*httptest.Server
	toolNames []string
	lastAuth  atomic.Value
	calls     atomic.Int64
}

func newFakeMCP(t *testing.T, toolNames ...string) *fakeMCP {
	t.Helper()
	f := &fakeMCP{toolNames: toolNames}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeMCP) handle(w http.ResponseWriter, r *http.Request) {
	f.lastAuth.Store(r.Header.Get("Authorization"))
	if r.Method != http.MethodPost {
		// SSE listener GET — hold until client disconnects.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
		return
	}
	var req mcp.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.ID == nil { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result json.RawMessage
	switch req.Method {
	case mcp.MethodInitialize:
		result = mcp.MustJSON(mcp.InitializeResult{
			ProtocolVersion: mcp.ProtocolVersion,
			ServerInfo:      mcp.ServerInfo{Name: "fake", Version: "1"},
		})
	case mcp.MethodToolsList:
		defs := make([]mcp.ToolDef, len(f.toolNames))
		for i, n := range f.toolNames {
			defs[i] = mcp.ToolDef{Name: n, Description: "fake tool " + n,
				InputSchema: json.RawMessage(`{"type":"object"}`)}
		}
		result = mcp.MustJSON(mcp.ListToolsResult{Tools: defs})
	case mcp.MethodToolsCall:
		f.calls.Add(1)
		result = mcp.MustJSON(mcp.CallToolResult{
			Content: []mcp.ContentItem{{Type: "text", Text: "ok"}},
		})
	default:
		http.Error(w, "unknown method "+req.Method, 400)
		return
	}
	resp := mcp.Response{JSONRPC: mcp.JSONRPCVersion, ID: *req.ID, Result: result}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&resp)
}

func writeMCPJSON(t *testing.T, servers ...mcp.ServerConfig) string {
	t.Helper()
	dir := t.TempDir()
	raw, err := json.Marshal(mcp.HostConfig{Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Spec scenario: HTTP MCP server 工具进入会话 (mcp-integration).
func TestLoadMCPTools_RegistersNamespacedAdapters(t *testing.T) {
	f := newFakeMCP(t, "query_tasks", "node_exec")
	path := writeMCPJSON(t, mcp.ServerConfig{
		Name: "dataweave", Enabled: true, Transport: "http",
		URL: f.URL, AuthHeader: "Bearer tok-123",
	})
	host := mcp.NewHost(discardLogger())
	defer host.Shutdown()
	reg := tools.NewRegistry()
	loadMCPTools(host, path, reg, discardLogger())

	for _, name := range []string{"dataweave__query_tasks", "dataweave__node_exec"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("registry missing %s; have %v", name, registryNames(reg))
		}
		if tool.Description() == "" {
			t.Errorf("%s: empty description", name)
		}
	}
	if got := f.lastAuth.Load(); got != "Bearer tok-123" {
		t.Errorf("auth header not forwarded: got %v", got)
	}

	// The adapter must round-trip a call through the HTTP transport.
	tool, _ := reg.Get("dataweave__query_tasks")
	res, err := tool.Run(context.Background(), &tools.Env{}, json.RawMessage(`{}`))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("adapter call failed: res=%+v err=%v", res, err)
	}
	if f.calls.Load() == 0 {
		t.Error("tools/call never reached the MCP server")
	}
}

// Spec scenario: 单 server 失败不阻塞启动 (mcp-integration).
func TestLoadMCPTools_FailingServerSkipped(t *testing.T) {
	good := newFakeMCP(t, "query_tasks")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	path := writeMCPJSON(t,
		mcp.ServerConfig{Name: "dead", Enabled: true, Transport: "http",
			URL: dead.URL},
		mcp.ServerConfig{Name: "dataweave", Enabled: true, Transport: "http",
			URL: good.URL},
	)
	host := mcp.NewHost(discardLogger())
	defer host.Shutdown()
	reg := tools.NewRegistry()
	loadMCPTools(host, path, reg, discardLogger())

	if _, ok := reg.Get("dataweave__query_tasks"); !ok {
		t.Fatalf("healthy server's tool missing; have %v", registryNames(reg))
	}
	for _, n := range registryNames(reg) {
		if n == "dead__anything" {
			t.Error("dead server must contribute no tools")
		}
	}
}

// Spec scenario: 无 mcp.json 静默继续 (mcp-integration).
func TestLoadMCPTools_MissingFileIsSilentNoop(t *testing.T) {
	host := mcp.NewHost(discardLogger())
	defer host.Shutdown()
	reg := tools.NewRegistry()
	loadMCPTools(host, filepath.Join(t.TempDir(), "mcp.json"), reg, discardLogger())
	if n := len(registryNames(reg)); n != 0 {
		t.Fatalf("expected empty registry, got %d tools", n)
	}
}

// Spec scenario: tool glob 免打扰放行 MCP 只读工具 — the permission chain works
// against registered MCP tool names with a preset glob rule (task 3.4).
func TestMCPTools_PresetGlobSkipsPrompt(t *testing.T) {
	f := newFakeMCP(t, "query_tasks", "query_instances", "node_exec")
	path := writeMCPJSON(t, mcp.ServerConfig{
		Name: "dataweave", Enabled: true, Transport: "http", URL: f.URL,
	})
	host := mcp.NewHost(discardLogger())
	defer host.Shutdown()
	reg := tools.NewRegistry()
	loadMCPTools(host, path, reg, discardLogger())

	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SavePermission(context.Background(), &store.Permission{
		ID: "preset-glob", Tool: "dataweave__query_*", Pattern: "",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	perm := permission.New(st,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			prompts++
			return permission.Deny, true
		}, dangerousCommandPredicate(), time.Second, "")

	for _, name := range registryNames(reg) {
		if name == "dataweave__node_exec" {
			continue
		}
		d, src, err := perm.Check(context.Background(), permission.CheckInput{
			SessionID: "sess", Tool: name,
		})
		if err != nil || d != permission.AllowPermanent || src != permission.SourceRule {
			t.Errorf("%s: got (%v,%v,%v), want rule allow", name, d, src, err)
		}
	}
	if prompts != 0 {
		t.Errorf("read-only MCP tools must not prompt, got %d prompts", prompts)
	}
	// The uncovered tool still routes to the prompt (external approval path).
	if d, _, _ := perm.Check(context.Background(), permission.CheckInput{
		SessionID: "sess", Tool: "dataweave__node_exec",
	}); d != permission.Deny || prompts != 1 {
		t.Errorf("node_exec should prompt: d=%v prompts=%d", d, prompts)
	}
}

func registryNames(reg *tools.Registry) []string {
	var names []string
	for _, t := range reg.Filtered(nil) {
		names = append(names, t.Name())
	}
	return names
}

// tool-system spec scenario: 过滤可观察 — the dropped-tools computation that
// feeds the session-creation log.
func TestAllowlistDroppedTools(t *testing.T) {
	f := newFakeMCP(t, "query_tasks")
	path := writeMCPJSON(t, mcp.ServerConfig{
		Name: "dataweave", Enabled: true, Transport: "http", URL: f.URL,
	})
	host := mcp.NewHost(discardLogger())
	defer host.Shutdown()
	reg := tools.NewRegistry()
	loadMCPTools(host, path, reg, discardLogger())

	if got := allowlistDroppedTools(reg, nil); got != nil {
		t.Fatalf("no filter must drop nothing, got %v", got)
	}
	dropped := allowlistDroppedTools(reg, []string{"nope_*"})
	if len(dropped) != 1 || dropped[0] != "dataweave__query_tasks" {
		t.Fatalf("dropped = %v, want the filtered MCP tool", dropped)
	}
	if got := allowlistDroppedTools(reg, []string{"dataweave__*"}); len(got) != 0 {
		t.Fatalf("glob admits the tool; dropped = %v", got)
	}
}
