package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/provider/openai"
)

func chunks(lines ...string) string {
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString("data: " + l + "\n\n")
	}
	return sb.String()
}

func drain(t *testing.T, ch <-chan provider.ProviderEvent) []provider.ProviderEvent {
	t.Helper()
	var out []provider.ProviderEvent
	for e := range ch {
		out = append(out, e)
	}
	return out
}

func TestOpenAI_SimpleTextStream(t *testing.T) {
	wire := chunks(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":", world"}}]}`,
		`{"choices":[{"index":0,"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	srv := newSSEServer(wire)
	defer srv.Close()

	p := openai.New(openai.Options{APIKey: "k", BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), provider.Request{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	var text string
	var stop bool
	for _, e := range events {
		if e.Type == provider.EventTextDelta {
			text += e.TextDelta
		}
		if e.Type == provider.EventStop {
			stop = true
			if e.StopReason != "end_turn" {
				t.Errorf("stop reason: %q", e.StopReason)
			}
		}
	}
	if text != "Hello, world" {
		t.Errorf("text: %q", text)
	}
	if !stop {
		t.Error("missing stop event")
	}
}

// Scenario from spec: 并发 tool_calls 累积（index 字段）.
func TestOpenAI_ConcurrentToolCalls(t *testing.T) {
	wire := chunks(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"Read","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"comm"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"path"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\":\"a.txt\"}"}}]}}]}`,
		`{"choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	srv := newSSEServer(wire)
	defer srv.Close()

	p := openai.New(openai.Options{APIKey: "k", BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), provider.Request{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	var tools []*provider.ContentBlock
	var stopReason string
	for _, e := range events {
		if e.Type == provider.EventToolUse {
			tools = append(tools, e.ToolUse)
		}
		if e.Type == provider.EventStop {
			stopReason = e.StopReason
		}
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tool_use events, want 2", len(tools))
	}
	if tools[0].ToolUseID != "a" || tools[0].ToolName != "Bash" {
		t.Errorf("tool 0: %+v", tools[0])
	}
	if tools[1].ToolUseID != "b" || tools[1].ToolName != "Read" {
		t.Errorf("tool 1: %+v", tools[1])
	}
	var args0 map[string]string
	if err := json.Unmarshal(tools[0].Input, &args0); err != nil || args0["command"] != "ls" {
		t.Errorf("tool 0 args: %v err=%v", args0, err)
	}
	var args1 map[string]string
	if err := json.Unmarshal(tools[1].Input, &args1); err != nil || args1["path"] != "a.txt" {
		t.Errorf("tool 1 args: %v err=%v", args1, err)
	}
	if stopReason != "tool_use" {
		t.Errorf("stop reason should map tool_calls→tool_use, got %q", stopReason)
	}
}

// Scenario from spec: finish_reason=stop 但含 tool_calls（非流式 message 字段）.
func TestOpenAI_NonStreamingMessageWithToolCalls(t *testing.T) {
	wire := chunks(
		`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"echo\"}"}}]},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	srv := newSSEServer(wire)
	defer srv.Close()

	p := openai.New(openai.Options{APIKey: "k", BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), provider.Request{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	var toolUseCount int
	for _, e := range events {
		if e.Type == provider.EventToolUse {
			toolUseCount++
		}
	}
	if toolUseCount != 1 {
		t.Errorf("expected 1 tool_use from message field, got %d", toolUseCount)
	}
}

// Scenario from spec: tool_result 翻译为独立 tool 消息.
func TestOpenAI_EncodesToolResultAsToolMessage(t *testing.T) {
	// We capture the outgoing request body to assert the translation.
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	req := provider.Request{
		Model: "gpt-4o",
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
				{Type: provider.BlockText, Text: "let me check"},
				{Type: provider.BlockToolUse, ToolUseID: "x", ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
			}},
			{Role: provider.RoleUser, Content: []provider.ContentBlock{
				{Type: provider.BlockToolResult, ToolUseID: "x", Output: "file.txt\n"},
			}},
		},
	}
	p := openai.New(openai.Options{APIKey: "k", BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, ch)

	var parsed struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured, &parsed); err != nil {
		t.Fatalf("captured body not JSON: %v", err)
	}
	// Expect: assistant (text + tool_calls) + tool (tool_result).
	if len(parsed.Messages) != 2 {
		t.Fatalf("expected 2 messages in wire, got %d (%s)", len(parsed.Messages), captured)
	}
	if parsed.Messages[0].Role != "assistant" || parsed.Messages[0].Content != "let me check" {
		t.Errorf("msg[0]: %+v", parsed.Messages[0])
	}
	if len(parsed.Messages[0].ToolCalls) != 1 || parsed.Messages[0].ToolCalls[0].Function.Name != "Bash" {
		t.Errorf("tool_calls: %+v", parsed.Messages[0].ToolCalls)
	}
	if parsed.Messages[1].Role != "tool" || parsed.Messages[1].ToolCallID != "x" {
		t.Errorf("msg[1] should be role:tool with id=x, got %+v", parsed.Messages[1])
	}
}

func TestOpenAI_HTTP401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"bad key"}}`)
	}))
	defer srv.Close()
	p := openai.New(openai.Options{APIKey: "bogus", BaseURL: srv.URL})
	_, err := p.Stream(context.Background(), provider.Request{Model: "x"})
	pe, ok := provider.AsProviderError(err)
	if !ok || pe.Code != provider.CodeAuthFailed || pe.IsRetryable() {
		t.Errorf("expected non-retryable auth_failed, got %+v ok=%v", pe, ok)
	}
}

func TestOpenAI_HTTP429RetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow"}}`)
	}))
	defer srv.Close()
	p := openai.New(openai.Options{APIKey: "k", BaseURL: srv.URL})
	_, err := p.Stream(context.Background(), provider.Request{Model: "x"})
	pe, _ := provider.AsProviderError(err)
	if pe == nil || pe.Code != provider.CodeRateLimited || !pe.IsRetryable() || pe.RetryAfter() != 3*time.Second {
		t.Errorf("expected rate_limited retryable with RetryAfter=3s, got %+v", pe)
	}
}

func newSSEServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
}
