// Package protocol defines the wire types for the workhorse-agent
// application-level Streamable HTTP protocol: the seven Client → Server message
// types, the twenty-one Server → Client event types, and the structured `error`
// event with its full code enum.
//
// The package is intentionally pure data — no HTTP, no SSE framing, no
// goroutines. It is imported by both the api server and the pkg/client SDK.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ClientMessageType is the discriminator for incoming POST bodies.
type ClientMessageType string

const (
	ClientUserMessage          ClientMessageType = "user_message"
	ClientPermissionDecision   ClientMessageType = "permission_decision"
	ClientInterrupt            ClientMessageType = "interrupt"
	ClientPing                 ClientMessageType = "ping"
	ClientContextUpdate        ClientMessageType = "context_update"
	ClientPublishFrontendTools ClientMessageType = "publish_frontend_tools"
	ClientFrontendToolResult   ClientMessageType = "frontend_tool_result"
)

// IsKnown reports whether t matches one of the seven spec-defined types.
func (t ClientMessageType) IsKnown() bool {
	switch t {
	case ClientUserMessage, ClientPermissionDecision, ClientInterrupt,
		ClientPing, ClientContextUpdate,
		ClientPublishFrontendTools, ClientFrontendToolResult:
		return true
	}
	return false
}

// ServerEventType is the discriminator for outgoing SSE frames.
type ServerEventType string

const (
	EventAssistantTextDelta    ServerEventType = "assistant_text_delta"
	EventAssistantTextDone     ServerEventType = "assistant_text_done"
	EventReasoningStart        ServerEventType = "reasoning_start"
	EventReasoningDelta        ServerEventType = "reasoning_delta"
	EventReasoningEnd          ServerEventType = "reasoning_end"
	EventToolCallStart         ServerEventType = "tool_call_start"
	EventToolCallDone          ServerEventType = "tool_call_done"
	EventPermissionRequest     ServerEventType = "permission_request"
	EventPermissionResolved    ServerEventType = "permission_resolved"
	EventSubagentEvent         ServerEventType = "subagent_event"
	EventSubagentStatus        ServerEventType = "subagent_status"
	EventCompaction            ServerEventType = "compaction"
	EventProviderRetry         ServerEventType = "provider_retry"
	EventError                 ServerEventType = "error"
	EventInterrupted           ServerEventType = "interrupted"
	EventPong                  ServerEventType = "pong"
	EventAdapterApprovalReq    ServerEventType = "adapter_approval_request"
	EventAdapterApprovalResolv ServerEventType = "adapter_approval_resolved"
	EventAdapterApprovalExp    ServerEventType = "adapter_approval_expired"
	EventFrontendToolUse       ServerEventType = "frontend_tool_use"
	EventFrontendToolsPub      ServerEventType = "frontend_tools_published"
	EventTaskUpdate            ServerEventType = "task_update"
	EventSessionTitleUpdated   ServerEventType = "session_title_updated"
)

// AllServerEventTypes is the canonical list used by tests and the debug
// snapshot endpoint to validate event sets.
var AllServerEventTypes = []ServerEventType{
	EventAssistantTextDelta, EventAssistantTextDone,
	EventReasoningStart, EventReasoningDelta, EventReasoningEnd,
	EventToolCallStart, EventToolCallDone,
	EventPermissionRequest, EventPermissionResolved, EventSubagentEvent, EventSubagentStatus,
	EventCompaction, EventProviderRetry,
	EventError, EventInterrupted, EventPong,
	EventAdapterApprovalReq, EventAdapterApprovalResolv, EventAdapterApprovalExp,
	EventFrontendToolUse, EventFrontendToolsPub,
	EventTaskUpdate,
	EventSessionTitleUpdated,
}

// IsKnown reports whether t matches one of the twenty-three spec-defined event types.
func (t ServerEventType) IsKnown() bool {
	for _, k := range AllServerEventTypes {
		if k == t {
			return true
		}
	}
	return false
}

// ErrorCode is the machine-readable code carried by `error` events. New codes
// MUST be added to the spec first; see api-protocol/spec.md "error 事件 JSON
// schema" requirement.
type ErrorCode string

const (
	ErrSessionBusy                   ErrorCode = "session_busy"
	ErrUnknownMessageType            ErrorCode = "unknown_message_type"
	ErrHistoryTokenLimit             ErrorCode = "history_token_limit"
	ErrToolNotAllowed                ErrorCode = "tool_not_allowed"
	ErrPermissionDenied              ErrorCode = "permission_denied"
	ErrProviderAuthFailed            ErrorCode = "provider_auth_failed"
	ErrProviderInvalidRequest        ErrorCode = "provider_invalid_request"
	ErrProviderContextLengthExceeded ErrorCode = "provider_context_length_exceeded"
	ErrProviderInsufficientQuota     ErrorCode = "provider_insufficient_quota"
	ErrProviderUnrecoverable         ErrorCode = "provider_unrecoverable"
	ErrCancelTimeout                 ErrorCode = "cancel_timeout"
	ErrInternalPanic                 ErrorCode = "internal_panic"
	ErrServerShutdown                ErrorCode = "server_shutdown"
	ErrRequestTooLarge               ErrorCode = "request_too_large"
)

// AllErrorCodes is the canonical 14-entry list. Order matches the spec table.
var AllErrorCodes = []ErrorCode{
	ErrSessionBusy, ErrUnknownMessageType, ErrHistoryTokenLimit,
	ErrToolNotAllowed, ErrPermissionDenied,
	ErrProviderAuthFailed, ErrProviderInvalidRequest,
	ErrProviderContextLengthExceeded, ErrProviderInsufficientQuota,
	ErrProviderUnrecoverable,
	ErrCancelTimeout, ErrInternalPanic, ErrServerShutdown,
	ErrRequestTooLarge,
}

// errorMeta encodes the recoverable flag and a human-readable message
// templated per code. Details are filled in by the caller.
type errorMeta struct {
	recoverable bool
	message     string
}

var errorMetaByCode = map[ErrorCode]errorMeta{
	ErrSessionBusy:                   {true, "session is currently busy"},
	ErrUnknownMessageType:            {false, "unknown client message type"},
	ErrHistoryTokenLimit:             {false, "history token limit exceeded"},
	ErrToolNotAllowed:                {true, "tool is not in the allowed set for this session"},
	ErrPermissionDenied:              {true, "permission denied for this tool invocation"},
	ErrProviderAuthFailed:            {false, "provider authentication failed"},
	ErrProviderInvalidRequest:        {false, "provider rejected the request"},
	ErrProviderContextLengthExceeded: {false, "provider context length exceeded"},
	ErrProviderInsufficientQuota:     {false, "provider quota exhausted"},
	ErrProviderUnrecoverable:         {false, "provider returned an unrecoverable error"},
	ErrCancelTimeout:                 {true, "cancel drain exceeded its timeout budget"},
	ErrInternalPanic:                 {true, "session goroutine panicked and recovered"},
	ErrServerShutdown:                {false, "server is shutting down"},
	ErrRequestTooLarge:               {true, "request body exceeds the configured limit"},
}

// Recoverable reports whether the client can keep using this session after
// receiving an error of code c. Unknown codes default to non-recoverable.
func (c ErrorCode) Recoverable() bool {
	if m, ok := errorMetaByCode[c]; ok {
		return m.recoverable
	}
	return false
}

// Message returns the default human-readable message for code c. Callers may
// pass a more specific message via NewErrorPayload; the default is only used
// when the caller passes an empty string.
func (c ErrorCode) Message() string {
	if m, ok := errorMetaByCode[c]; ok {
		return m.message
	}
	return string(c)
}

// NewErrorPayload builds the payload map for the `error` event. The caller
// supplies the code, an optional message override (empty means use default),
// and a details object whose fields are code-specific (see spec table). The
// resulting map is shaped for session.Emit which flattens it into the wire
// frame alongside type/idx/session_id.
func NewErrorPayload(code ErrorCode, message string, details map[string]any) map[string]any {
	if message == "" {
		message = code.Message()
	}
	payload := map[string]any{
		"code":        string(code),
		"message":     message,
		"recoverable": code.Recoverable(),
	}
	// Per spec we always include details (empty object when unset) so the
	// schema is stable for clients.
	if details == nil {
		details = map[string]any{}
	}
	payload["details"] = details
	return payload
}

// UserMessagePayload is the JSON shape of {type:"user_message", ...}.
type UserMessagePayload struct {
	Content     string            `json:"content"`
	Attachments []json.RawMessage `json:"attachments,omitempty"`
}

// PermissionDecisionPayload is the JSON shape of {type:"permission_decision", ...}.
type PermissionDecisionPayload struct {
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
}

// ContextUpdatePayload is the JSON shape of {type:"context_update", ...}.
type ContextUpdatePayload struct {
	Workdir string            `json:"workdir,omitempty"`
	Files   []json.RawMessage `json:"files,omitempty"`
}

// PublishFrontendToolsPayload is the JSON shape of {type:"publish_frontend_tools", ...}.
type PublishFrontendToolsPayload struct {
	Catalog []FrontendToolEntry `json:"catalog"`
}

// FrontendToolEntry is one entry in the catalog.
type FrontendToolEntry struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	InputSchema    json.RawMessage `json:"inputSchema"`
	OutputSchema   json.RawMessage `json:"outputSchema,omitempty"`
	ParallelSafety string          `json:"parallelSafety"`
}

// FrontendToolResultPayload is the JSON shape of {type:"frontend_tool_result", ...}.
type FrontendToolResultPayload struct {
	ToolUseID string          `json:"tool_use_id"`
	Result    json.RawMessage `json:"result"`
}

// ClientMessage is the discriminated union over the seven message types. The
// raw payload bytes are kept on Payload so handlers can do typed unmarshal
// only when needed.
type ClientMessage struct {
	Type    ClientMessageType
	Payload json.RawMessage
}

// ErrUnknownClientType is returned by Decode when the JSON body's "type"
// field is not one of the seven spec types. Handlers translate this to the
// `unknown_message_type` error code.
var ErrUnknownClientType = errors.New("protocol: unknown client message type")

// DecodeClientMessage parses one ClientMessage from a JSON body. It uses
// json.NewDecoder with DisallowUnknownFields=false (forward compat) and
// rejects empty / non-object inputs.
func DecodeClientMessage(body []byte) (ClientMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var probe struct {
		Type string `json:"type"`
	}
	if err := dec.Decode(&probe); err != nil {
		return ClientMessage{}, fmt.Errorf("protocol: decode envelope: %w", err)
	}
	t := ClientMessageType(probe.Type)
	if !t.IsKnown() {
		return ClientMessage{Type: t}, fmt.Errorf("%w: %q", ErrUnknownClientType, probe.Type)
	}
	// Re-decode the full body to capture payload fields.
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ClientMessage{}, fmt.Errorf("protocol: re-decode payload: %w", err)
	}
	return ClientMessage{Type: t, Payload: raw}, nil
}
