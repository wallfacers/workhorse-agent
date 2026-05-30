package protocol

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestClientMessageType_IsKnown(t *testing.T) {
	for _, ok := range []ClientMessageType{
		ClientUserMessage, ClientPermissionDecision,
		ClientInterrupt, ClientPing, ClientContextUpdate,
		ClientPublishFrontendTools, ClientFrontendToolResult,
	} {
		if !ok.IsKnown() {
			t.Fatalf("%q must be known", ok)
		}
	}
	for _, bad := range []ClientMessageType{"", "frobnicate", "user-message"} {
		if bad.IsKnown() {
			t.Fatalf("%q must not be known", bad)
		}
	}
}

func TestServerEventTypes_FullSet(t *testing.T) {
	// 11 spec-defined event types from the api-protocol MVP plus 3 added
	// by add-llm-adapter-generator (adapter_approval_request,
	// adapter_approval_resolved, adapter_approval_expired) plus 2 added
	// by add-frontend-tool-bridge (frontend_tool_use,
	// frontend_tools_published) plus 1 added by add-todo-tool
	// (task_update) plus 3 added by add-thinking-mode-and-prompt-cache
	// (reasoning_start, reasoning_delta, reasoning_end) → 20.
	const want = 20
	if got := len(AllServerEventTypes); got != want {
		t.Fatalf("event-type catalog drifted: got %d, want %d", got, want)
	}
}

func TestAllErrorCodes_Fourteen(t *testing.T) {
	if got := len(AllErrorCodes); got != 14 {
		t.Fatalf("api-protocol spec requires 14 error codes, got %d", got)
	}
}

func TestErrorCode_RecoverableMatchesSpec(t *testing.T) {
	// Per spec table 256-274.
	cases := map[ErrorCode]bool{
		ErrSessionBusy:                   true,
		ErrUnknownMessageType:            false,
		ErrHistoryTokenLimit:             false,
		ErrToolNotAllowed:                true,
		ErrPermissionDenied:              true,
		ErrProviderAuthFailed:            false,
		ErrProviderInvalidRequest:        false,
		ErrProviderContextLengthExceeded: false,
		ErrProviderInsufficientQuota:     false,
		ErrProviderUnrecoverable:         false,
		ErrCancelTimeout:                 true,
		ErrInternalPanic:                 true,
		ErrServerShutdown:                false,
		ErrRequestTooLarge:               true,
	}
	for code, want := range cases {
		if got := code.Recoverable(); got != want {
			t.Errorf("%s.Recoverable()=%v, want %v", code, got, want)
		}
	}
}

func TestNewErrorPayload_ShapeAndDefaults(t *testing.T) {
	p := NewErrorPayload(ErrSessionBusy, "", map[string]any{"state": "Compacting"})
	if p["code"] != string(ErrSessionBusy) {
		t.Fatalf("code: %v", p["code"])
	}
	if p["message"] != ErrSessionBusy.Message() {
		t.Fatalf("default message lost: %v", p["message"])
	}
	if p["recoverable"] != true {
		t.Fatalf("session_busy must be recoverable")
	}
	det, ok := p["details"].(map[string]any)
	if !ok || det["state"] != "Compacting" {
		t.Fatalf("details lost: %v", p["details"])
	}
}

func TestNewErrorPayload_NilDetailsBecomesObject(t *testing.T) {
	p := NewErrorPayload(ErrServerShutdown, "", nil)
	det, ok := p["details"].(map[string]any)
	if !ok {
		t.Fatalf("details must always be an object, got %T", p["details"])
	}
	if len(det) != 0 {
		t.Fatalf("details default must be empty, got %+v", det)
	}
	// Roundtrip through JSON to confirm shape stays stable.
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["details"].(map[string]any); !ok {
		t.Fatalf("details lost roundtrip: %v", decoded["details"])
	}
}

func TestDecodeClientMessage_OK(t *testing.T) {
	body := []byte(`{"type":"user_message","content":"hi"}`)
	msg, err := DecodeClientMessage(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Type != ClientUserMessage {
		t.Fatalf("type: %s", msg.Type)
	}
	var p UserMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Content != "hi" {
		t.Fatalf("content: %q", p.Content)
	}
}

func TestDecodeClientMessage_UnknownType(t *testing.T) {
	body := []byte(`{"type":"frobnicate"}`)
	msg, err := DecodeClientMessage(body)
	if !errors.Is(err, ErrUnknownClientType) {
		t.Fatalf("expected ErrUnknownClientType, got %v", err)
	}
	if msg.Type != "frobnicate" {
		t.Fatalf("type should be preserved for diagnostics, got %q", msg.Type)
	}
}

func TestDecodeClientMessage_MissingType(t *testing.T) {
	body := []byte(`{}`)
	_, err := DecodeClientMessage(body)
	if !errors.Is(err, ErrUnknownClientType) {
		t.Fatalf("expected ErrUnknownClientType for missing type, got %v", err)
	}
}

func TestDecodeClientMessage_BadJSON(t *testing.T) {
	if _, err := DecodeClientMessage([]byte(`{not-json`)); err == nil {
		t.Fatalf("expected decode error for bad JSON")
	}
}

func TestDecodeClientMessage_PublishFrontendTools(t *testing.T) {
	body := []byte(`{"type":"publish_frontend_tools","catalog":[{"name":"click","description":"Click","inputSchema":{},"parallelSafety":"safe"}]}`)
	msg, err := DecodeClientMessage(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Type != ClientPublishFrontendTools {
		t.Fatalf("type: %s", msg.Type)
	}
	var p PublishFrontendToolsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(p.Catalog) != 1 || p.Catalog[0].Name != "click" {
		t.Fatalf("catalog: %+v", p.Catalog)
	}
	if p.Catalog[0].ParallelSafety != "safe" {
		t.Fatalf("parallelSafety: %s", p.Catalog[0].ParallelSafety)
	}
}

func TestDecodeClientMessage_FrontendToolResult(t *testing.T) {
	body := []byte(`{"type":"frontend_tool_result","tool_use_id":"abc123","result":{"ok":true,"value":42}}`)
	msg, err := DecodeClientMessage(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Type != ClientFrontendToolResult {
		t.Fatalf("type: %s", msg.Type)
	}
	var p FrontendToolResultPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.ToolUseID != "abc123" {
		t.Fatalf("tool_use_id: %s", p.ToolUseID)
	}
}
