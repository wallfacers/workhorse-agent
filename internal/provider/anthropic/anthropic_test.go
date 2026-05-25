package anthropic_test

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
	"github.com/wallfacers/workhorse-agent/internal/provider/anthropic"
)

// sse builds the canonical SSE wire format the adapter parses.
func sse(events ...sseFrame) string {
	var sb strings.Builder
	for _, f := range events {
		if f.event != "" {
			sb.WriteString("event: " + f.event + "\n")
		}
		sb.WriteString("data: " + f.data + "\n\n")
	}
	return sb.String()
}

type sseFrame struct {
	event string
	data  string
}

// drain reads every ProviderEvent until the channel closes.
func drain(t *testing.T, ch <-chan provider.ProviderEvent) []provider.ProviderEvent {
	t.Helper()
	var out []provider.ProviderEvent
	for e := range ch {
		out = append(out, e)
	}
	return out
}

// Scenario from spec: 流式接收 Anthropic SSE 简单文本.
func TestAnthropic_SimpleTextStream(t *testing.T) {
	wire := sse(
		sseFrame{"message_start", `{"message":{"id":"m1","usage":{"input_tokens":7}}}`},
		sseFrame{"content_block_start", `{"index":0,"content_block":{"type":"text"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"He"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"ll"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"o"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":", "}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"world"}}`},
		sseFrame{"content_block_stop", `{"index":0}`},
		sseFrame{"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`},
		sseFrame{"message_stop", `{}`},
	)

	srv := newSSEServer(wire)
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), provider.Request{
		Model:    "claude-sonnet-4-6",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	var textDeltas []string
	var usageSeen, stopSeen bool
	for _, e := range events {
		switch e.Type {
		case provider.EventTextDelta:
			textDeltas = append(textDeltas, e.TextDelta)
		case provider.EventUsage:
			usageSeen = true
			if e.Usage.InputTokens != 7 || e.Usage.OutputTokens != 5 {
				t.Errorf("usage tokens: %+v", e.Usage)
			}
		case provider.EventStop:
			stopSeen = true
			if e.StopReason != "end_turn" {
				t.Errorf("stop reason: %q", e.StopReason)
			}
		}
	}
	if got := strings.Join(textDeltas, ""); got != "Hello, world" {
		t.Errorf("text deltas joined: %q", got)
	}
	if len(textDeltas) != 5 {
		t.Errorf("expected 5 text_delta events, got %d", len(textDeltas))
	}
	if !usageSeen || !stopSeen {
		t.Errorf("missing terminal events: usage=%v stop=%v", usageSeen, stopSeen)
	}
}

// Scenario from spec: tool_use 累积完成后 emit.
func TestAnthropic_ToolUseAccumulation(t *testing.T) {
	wire := sse(
		sseFrame{"message_start", `{"message":{"id":"m1","usage":{"input_tokens":12}}}`},
		sseFrame{"content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"abc","name":"Bash","input":{}}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"comma"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"nd\":\"ls\"}"}}`},
		sseFrame{"content_block_stop", `{"index":0}`},
		sseFrame{"message_delta", `{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`},
		sseFrame{"message_stop", `{}`},
	)
	srv := newSSEServer(wire)
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), provider.Request{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	var toolUseSeen *provider.ContentBlock
	for _, e := range events {
		if e.Type == provider.EventToolUse {
			toolUseSeen = e.ToolUse
		}
		if e.Type == provider.EventTextDelta {
			t.Errorf("tool_use stream must not emit text_delta, got %+v", e)
		}
	}
	if toolUseSeen == nil {
		t.Fatal("expected one tool_use event")
	}
	if toolUseSeen.ToolUseID != "abc" || toolUseSeen.ToolName != "Bash" {
		t.Errorf("tool_use metadata wrong: %+v", toolUseSeen)
	}
	var got map[string]string
	if err := json.Unmarshal(toolUseSeen.Input, &got); err != nil {
		t.Fatalf("input JSON not valid: %v", err)
	}
	if got["command"] != "ls" {
		t.Errorf("input contents wrong: %v", got)
	}
}

// Scenario from spec: thinking 块被丢弃（MVP）.
func TestAnthropic_ThinkingDiscarded(t *testing.T) {
	wire := sse(
		sseFrame{"message_start", `{"message":{"usage":{"input_tokens":3}}}`},
		sseFrame{"content_block_start", `{"index":0,"content_block":{"type":"thinking"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"plotting"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":" the answer"}}`},
		sseFrame{"content_block_stop", `{"index":0}`},
		sseFrame{"content_block_start", `{"index":1,"content_block":{"type":"text"}}`},
		sseFrame{"content_block_delta", `{"index":1,"delta":{"type":"text_delta","text":"answer"}}`},
		sseFrame{"content_block_stop", `{"index":1}`},
		sseFrame{"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`},
		sseFrame{"message_stop", `{}`},
	)
	srv := newSSEServer(wire)
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), provider.Request{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drain(t, ch)

	var textDeltas []string
	for _, e := range events {
		if e.Type == provider.EventTextDelta {
			textDeltas = append(textDeltas, e.TextDelta)
		}
	}
	if strings.Join(textDeltas, "") != "answer" {
		t.Errorf("only the text block should be exposed, got %q", textDeltas)
	}
}

// Scenario from spec: 401 不可重试 — but for Anthropic.
func TestAnthropic_HTTP401MapsToAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"bad key"}}`)
	}))
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "bogus", BaseURL: srv.URL})
	_, err := p.Stream(context.Background(), provider.Request{Model: "x"})
	pe, ok := provider.AsProviderError(err)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T %v", err, err)
	}
	if pe.Code != provider.CodeAuthFailed || pe.IsRetryable() {
		t.Errorf("auth_failed should be non-retryable, got code=%q retryable=%v", pe.Code, pe.IsRetryable())
	}
}

// Scenario from spec: 429 标记可重试 + Retry-After.
func TestAnthropic_HTTP429ParsesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: srv.URL})
	_, err := p.Stream(context.Background(), provider.Request{Model: "x"})
	pe, ok := provider.AsProviderError(err)
	if !ok {
		t.Fatalf("expected *ProviderError, got %v", err)
	}
	if pe.Code != provider.CodeRateLimited || !pe.IsRetryable() {
		t.Errorf("rate_limited must be retryable: %+v", pe)
	}
	if pe.RetryAfter() != 5*time.Second {
		t.Errorf("expected RetryAfter=5s, got %v", pe.RetryAfter())
	}
}

// newSSEServer responds to any request with the given canned SSE body. We use
// httptest because the adapter takes BaseURL.
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
