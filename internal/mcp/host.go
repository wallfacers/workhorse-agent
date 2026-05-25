package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// ServerConfig is the per-server entry in mcp.json.
type ServerConfig struct {
	Name       string            `json:"name"`
	Enabled    bool              `json:"enabled"`
	Transport  string            `json:"transport"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	AuthHeader string            `json:"auth_header,omitempty"`
}

// HostConfig is the top-level structure of mcp.json.
type HostConfig struct {
	Servers []ServerConfig `json:"servers"`
}

// Host manages the lifecycle of all configured MCP servers. It is
// process-global: all sessions share the same set of servers.
type Host struct {
	logger  *slog.Logger
	servers map[string]*serverInstance
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// serverInstance tracks one running MCP server.
type serverInstance struct {
	config    ServerConfig
	transport Transport
	client    *MCPClient
	tools     []ToolDef
	healthy   bool
	restarts  int
}

// NewHost creates an empty Host. Call LoadAndStart to populate it.
func NewHost(logger *slog.Logger) *Host {
	ctx, cancel := context.WithCancel(context.Background())
	return &Host{
		logger:  logger,
		servers: make(map[string]*serverInstance),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// LoadAndStart reads the mcp.json config file, creates transports for every
// enabled server, runs the initialize handshake, and fetches tools. It returns
// any startup errors but does not fail the whole host if a single server fails
// — that server is marked unhealthy.
func (h *Host) LoadAndStart(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("mcp host: read config: %w", err)
	}
	var cfg HostConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("mcp host: parse config: %w", err)
	}

	for _, srv := range cfg.Servers {
		if !srv.Enabled {
			h.log("mcp server disabled, skipping", "name", srv.Name)
			continue
		}
		if _, exists := h.servers[srv.Name]; exists {
			h.log("mcp server name conflict, skipping duplicate", "name", srv.Name)
			continue
		}
		inst := &serverInstance{config: srv}
		if err := h.startServer(inst); err != nil {
			h.log("mcp server start failed", "name", srv.Name, "err", err)
			inst.healthy = false
		} else {
			inst.healthy = true
		}
		h.servers[srv.Name] = inst

		// Begin health monitoring for stdio servers.
		if srv.Transport == "stdio" {
			h.wg.Add(1)
			go h.monitorStdio(inst)
		}
	}

	return nil
}

// Shutdown gracefully stops all servers by closing their transports.
func (h *Host) Shutdown() {
	h.cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, inst := range h.servers {
		if inst.transport != nil {
			if err := inst.transport.Close(); err != nil {
				h.log("mcp server shutdown error", "name", name, "err", err)
			}
		}
	}
	h.wg.Wait()
}

// AllTools returns the aggregated tool list from all healthy servers.
func (h *Host) AllTools() []ServerTool {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []ServerTool
	for name, inst := range h.servers {
		if !inst.healthy {
			continue
		}
		for _, t := range inst.tools {
			out = append(out, ServerTool{Server: name, Def: t, inst: inst})
		}
	}
	return out
}

// ServerTool pairs a ToolDef with its originating server and a method to call it.
type ServerTool struct {
	Server string
	Def    ToolDef
	inst   *serverInstance
}

// CallTool invokes the tool on the server that owns it.
func (st ServerTool) CallTool(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
	return st.inst.client.CallTool(ctx, st.Def.Name, args)
}

// ---------- internal ----------

func (h *Host) startServer(inst *serverInstance) error {
	var transport Transport
	var err error

	switch inst.config.Transport {
	case "stdio":
		transport, err = NewStdioTransport(StdioConfig{
			Command: inst.config.Command,
			Args:    inst.config.Args,
			Env:     inst.config.Env,
			Logger:  h.logger,
		})
		if err != nil {
			return fmt.Errorf("stdio transport: %w", err)
		}
	case "http":
		transport = NewHTTPTransport(HTTPConfig{
			URL:        inst.config.URL,
			AuthHeader: inst.config.AuthHeader,
			Logger:     h.logger,
		})
	default:
		return fmt.Errorf("unknown transport %q", inst.config.Transport)
	}

	client := &MCPClient{Transport: transport, Logger: h.logger}

	ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancel()

	_, err = client.Initialize(ctx)
	if err != nil {
		transport.Close()
		return fmt.Errorf("initialize: %w", err)
	}

	if err := client.SendInitialized(ctx); err != nil {
		transport.Close()
		return fmt.Errorf("send initialized: %w", err)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		transport.Close()
		return fmt.Errorf("tools/list: %w", err)
	}

	inst.transport = transport
	inst.client = client
	inst.tools = tools
	return nil
}

func (h *Host) monitorStdio(inst *serverInstance) {
	defer h.wg.Done()

	t, ok := inst.transport.(*StdioTransport)
	if !ok {
		return
	}

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.Exited():
		}

		h.mu.Lock()
		if inst.restarts >= 3 {
			inst.healthy = false
			h.mu.Unlock()
			h.log("mcp server restart limit reached, marking unhealthy",
				"name", inst.config.Name, "restarts", inst.restarts)
			return
		}
		inst.restarts++
		h.mu.Unlock()

		h.log("mcp server restarting", "name", inst.config.Name, "attempt", inst.restarts)
		time.Sleep(1 * time.Second)

		// Close old transport and create a new one.
		if inst.transport != nil {
			inst.transport.Close()
		}

		if err := h.startServer(inst); err != nil {
			h.log("mcp server restart failed", "name", inst.config.Name, "err", err)
			continue
		}

		inst.healthy = true
		h.log("mcp server restarted successfully", "name", inst.config.Name)

		// Restart monitoring on the new transport.
		t, ok = inst.transport.(*StdioTransport)
		if !ok {
			return
		}
	}
}

func (h *Host) log(msg string, args ...interface{}) {
	if h.logger != nil {
		h.logger.Info(msg, args...)
	}
}
