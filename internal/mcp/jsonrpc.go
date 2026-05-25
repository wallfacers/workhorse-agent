// Package mcp implements the MCP (Model Context Protocol) client as specified
// in MCP 2025-11-25 Streamable HTTP. It provides:
//
//   - JSON-RPC 2.0 types and a Transport abstraction
//   - stdio transport (child process)
//   - HTTP Streamable transport (POST + GET SSE)
//   - Host managing one or more MCP servers with lifecycle policies
//   - Adapter to wrap each MCP tool as a tools.Tool
package mcp

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// JSON-RPC 2.0 protocol constants.
const (
	JSONRPCVersion = "2.0"
)

// Request is a JSON-RPC 2.0 request. When ID is nil the message is a
// notification (no response expected).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a successful JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// IsError returns true when the response carries a JSON-RPC error.
func (r *Response) IsError() bool { return r.Error != nil }

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// Standard JSON-RPC 2.0 error codes.
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// MCP protocol method names.
const (
	MethodInitialize  = "initialize"
	MethodInitialized = "notifications/initialized"
	MethodToolsList   = "tools/list"
	MethodToolsCall   = "tools/call"
	MethodCancelled   = "notifications/cancelled"
)

// ---------- MCP protocol types ----------

// MCP protocol version we declare and require.
const ProtocolVersion = "2025-11-25"

// InitializeParams is the params sent with the initialize request.
type InitializeParams struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ClientInfo      ClientInfo   `json:"clientInfo"`
}

// Capabilities declares what the client supports.
type Capabilities struct{}

// ClientInfo identifies this MCP client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the server's response to initialize.
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

// ServerInfo identifies the MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolDef is one tool as returned by tools/list.
type ToolDef struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations carries the optional MCP tool annotations from tools/list.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint,omitempty"`
	DestructiveHint bool `json:"destructiveHint,omitempty"`
	IdempotentHint  bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool `json:"openWorldHint,omitempty"`
}

// ListToolsResult is the result of tools/list.
type ListToolsResult struct {
	Tools []ToolDef `json:"tools"`
}

// CallToolParams is the params for tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult is the result of tools/call.
type CallToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ContentItem is one piece of content within a tool result.
type ContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// CancelledParams is the params for notifications/cancelled.
type CancelledParams struct {
	RequestID int64  `json:"requestId"`
	Reason    string `json:"reason,omitempty"`
}

// idGen is a lock-free counter for JSON-RPC request IDs. IDs start from 1.
type idGen struct{ n atomic.Int64 }

func (g *idGen) Next() int64 { return g.n.Add(1) }

// MustJSON marshals v to json.RawMessage, panicking on error. Only used for
// values that are guaranteed to marshal without errors (static strings, simple
// structs with no cycles).
func MustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mcp: failed to marshal %T: %v", v, err))
	}
	return b
}
