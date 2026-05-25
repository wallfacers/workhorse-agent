package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// Transport abstracts the wire-level communication with an MCP server. Each
// concrete transport (stdio, HTTP) implements Call/Notify for outgoing messages
// and delivers server-initiated notifications via the Notifications channel.
type Transport interface {
	// Call sends a JSON-RPC request with an id and blocks until the response
	// arrives, ctx is done, or the transport fails. The returned Response may
	// carry a JSON-RPC error (IsError()).
	Call(ctx context.Context, req *Request) (*Response, error)

	// Notify sends a JSON-RPC notification (no id, no response expected). The
	// call returns as soon as the message is written to the transport.
	Notify(ctx context.Context, req *Request) error

	// Notifications returns a channel that carries server-initiated notifications.
	// The channel is owned by the transport; consumers must not close it.
	Notifications() <-chan *Request

	// Close shuts down the transport permanently. Subsequent Call will error.
	Close() error
}

// MCPClient wraps a Transport with MCP protocol semantics: it assigns request
// IDs, handles the initialize handshake, and exposes ListTools / CallTool /
// NotifyCancelled as typed methods.
type MCPClient struct {
	Transport Transport
	Logger    *slog.Logger
	id        idGen
}

// Initialize performs the MCP handshake. It returns the server's capabilities
// and info, or an error if the server rejected the handshake.
func (c *MCPClient) Initialize(ctx context.Context) (*InitializeResult, error) {
	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    Capabilities{},
		ClientInfo: ClientInfo{
			Name:    "workhorse-agent",
			Version: "0.1.0",
		},
	}
	resp, err := c.call(ctx, MethodInitialize, MustJSON(params))
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var result InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("initialize: bad result: %w", err)
	}
	if result.ProtocolVersion != ProtocolVersion {
		c.warn("mcp server protocol version mismatch",
			"server", result.ProtocolVersion, "client", ProtocolVersion)
	}
	return &result, nil
}

// SendInitialized sends the initialized notification.
func (c *MCPClient) SendInitialized(ctx context.Context) error {
	return c.notify(ctx, MethodInitialized, nil)
}

// ListTools fetches the tool list from the server.
func (c *MCPClient) ListTools(ctx context.Context) ([]ToolDef, error) {
	resp, err := c.call(ctx, MethodToolsList, nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var result ListToolsResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("tools/list: bad result: %w", err)
	}
	return result.Tools, nil
}

// CallTool invokes a named tool on the server and returns the result content.
func (c *MCPClient) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	params := CallToolParams{Name: name, Arguments: args}
	resp, err := c.call(ctx, MethodToolsCall, MustJSON(params))
	if err != nil {
		return nil, fmt.Errorf("tools/call %s: %w", name, err)
	}
	var result CallToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("tools/call %s: bad result: %w", name, err)
	}
	return &result, nil
}

// NotifyCancelled sends a cancellation notification for a pending request.
func (c *MCPClient) NotifyCancelled(ctx context.Context, requestID int64, reason string) error {
	params := CancelledParams{RequestID: requestID, Reason: reason}
	return c.notify(ctx, MethodCancelled, MustJSON(params))
}

// call sends a request and waits for the matching response.
func (c *MCPClient) call(ctx context.Context, method string, params json.RawMessage) (*Response, error) {
	id := c.id.Next()
	req := &Request{
		JSONRPC: JSONRPCVersion,
		ID:      &id,
		Method:  method,
		Params:  params,
	}
	resp, err := c.Transport.Call(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.ID != id {
		return nil, fmt.Errorf("response id mismatch: got %d, expected %d", resp.ID, id)
	}
	return resp, nil
}

// notify sends a notification (no response expected).
func (c *MCPClient) notify(ctx context.Context, method string, params json.RawMessage) error {
	req := &Request{
		JSONRPC: JSONRPCVersion,
		ID:      nil,
		Method:  method,
		Params:  params,
	}
	return c.Transport.Notify(ctx, req)
}

func (c *MCPClient) warn(msg string, args ...interface{}) {
	if c.Logger != nil {
		c.Logger.Warn(msg, args...)
	}
}
