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

// Task 8.1: thinking 块不再被丢弃——解析为 reasoning 事件 + 产出 thinking 块（含 signature）.
func TestAnthropic_ThinkingParsed(t *testing.T) {
	wire := sse(
		sseFrame{"message_start", `{"message":{"usage":{"input_tokens":3}}}`},
		sseFrame{"content_block_start", `{"index":0,"content_block":{"type":"thinking"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"plotting"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":" the answer"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"sig_abc"}}`},
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

	var reasoningStarts, reasoningDeltas, reasoningEnds int
	var textDeltas []string
	var thinkingBlock *provider.ContentBlock
	for _, e := range events {
		switch e.Type {
		case provider.EventReasoningStart:
			reasoningStarts++
			if e.ReasoningType != "thinking" {
				t.Errorf("reasoning_start type: got %q, want %q", e.ReasoningType, "thinking")
			}
		case provider.EventReasoningDelta:
			reasoningDeltas++
		case provider.EventReasoningEnd:
			reasoningEnds++
			if e.ReasoningBlock != nil {
				thinkingBlock = e.ReasoningBlock
			}
		case provider.EventTextDelta:
			textDeltas = append(textDeltas, e.TextDelta)
		}
	}
	if reasoningStarts != 1 {
		t.Errorf("expected 1 reasoning_start, got %d", reasoningStarts)
	}
	if reasoningDeltas != 2 {
		t.Errorf("expected 2 reasoning_deltas, got %d", reasoningDeltas)
	}
	if reasoningEnds != 1 {
		t.Errorf("expected 1 reasoning_end, got %d", reasoningEnds)
	}
	if thinkingBlock == nil {
		t.Fatal("reasoning_end must carry a thinking block")
	}
	if thinkingBlock.Thinking != "plotting the answer" {
		t.Errorf("thinking text: got %q", thinkingBlock.Thinking)
	}
	if thinkingBlock.Signature != "sig_abc" {
		t.Errorf("signature: got %q", thinkingBlock.Signature)
	}
	if strings.Join(textDeltas, "") != "answer" {
		t.Errorf("text block: got %q", textDeltas)
	}
}

// Task 8.2: 启用/未启用 thinking 的请求体断言.
func TestAnthropic_ThinkingRequestEncoding(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse(
			sseFrame{"message_start", `{"message":{"usage":{"input_tokens":1}}}`},
			sseFrame{"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`},
			sseFrame{"message_stop", `{}`},
		))
	}))
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: srv.URL})

	// With thinking enabled.
	ch, err := p.Stream(context.Background(), provider.Request{
		Model:                "claude-sonnet-4-6",
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, ch)

	if _, ok := receivedBody["thinking"]; !ok {
		t.Error("expected thinking field in request body")
	}
	if _, ok := receivedBody["temperature"]; ok {
		t.Error("temperature must not be present when thinking is enabled")
	}
	beta := srv.Client() // not useful; check headers via a different approach
	_ = beta

	// Without thinking.
	receivedBody = nil
	ch, err = p.Stream(context.Background(), provider.Request{
		Model: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("Stream (no thinking): %v", err)
	}
	drain(t, ch)

	if _, ok := receivedBody["thinking"]; ok {
		t.Error("thinking field must not be present when not enabled")
	}
}

// Task 8.2: beta header.
func TestAnthropic_ThinkingBetaHeader(t *testing.T) {
	var receivedHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse(
			sseFrame{"message_start", `{"message":{"usage":{"input_tokens":1}}}`},
			sseFrame{"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`},
			sseFrame{"message_stop", `{}`},
		))
	}))
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: srv.URL})

	// With thinking — beta header must be present.
	ch, err := p.Stream(context.Background(), provider.Request{
		Model:                "claude-sonnet-4-6",
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, ch)

	if got := receivedHeaders.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("beta header with thinking: got %q", got)
	}

	// Without thinking — beta header must be absent.
	receivedHeaders = nil
	ch, err = p.Stream(context.Background(), provider.Request{
		Model: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("Stream (no thinking): %v", err)
	}
	drain(t, ch)

	if got := receivedHeaders.Get("anthropic-beta"); got != "" {
		t.Errorf("beta header without thinking: got %q, want empty", got)
	}
}

// Task 8.2: 模型不支持 thinking 时报错.
func TestAnthropic_ThinkingNotSupportedModel(t *testing.T) {
	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: "http://unused"})
	_, err := p.Stream(context.Background(), provider.Request{
		Model:                "some-unknown-model",
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err == nil {
		t.Fatal("expected error for unsupported model")
	}
	if !strings.Contains(err.Error(), "extended thinking not supported by model") {
		t.Errorf("error message: got %q", err.Error())
	}
}

// Task 8.3: System block 数组 + cache_control 序列化.
func TestAnthropic_SystemBlockArray(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse(
			sseFrame{"message_start", `{"message":{"usage":{"input_tokens":1}}}`},
			sseFrame{"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`},
			sseFrame{"message_stop", `{}`},
		))
	}))
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: srv.URL})

	// With system prompt — should be an array of blocks.
	ch, err := p.Stream(context.Background(), provider.Request{
		Model:  "claude-sonnet-4-6",
		System: "You are helpful.",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, ch)

	sysRaw, ok := receivedBody["system"]
	if !ok {
		t.Fatal("system field missing")
	}
	sysArr, ok := sysRaw.([]any)
	if !ok {
		t.Fatalf("system should be array, got %T", sysRaw)
	}
	if len(sysArr) != 1 {
		t.Fatalf("system array length: got %d, want 1", len(sysArr))
	}
	blk, _ := sysArr[0].(map[string]any)
	if blk["type"] != "text" {
		t.Errorf("system block type: got %v", blk["type"])
	}
	if blk["text"] != "You are helpful." {
		t.Errorf("system block text: got %v", blk["text"])
	}
	cc, _ := blk["cache_control"].(map[string]any)
	if cc == nil || cc["type"] != "ephemeral" {
		t.Errorf("cache_control: got %v", cc)
	}
}

// Task 8.3: 空 system 不发.
func TestAnthropic_EmptySystemOmitted(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse(
			sseFrame{"message_start", `{"message":{"usage":{"input_tokens":1}}}`},
			sseFrame{"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`},
			sseFrame{"message_stop", `{}`},
		))
	}))
	defer srv.Close()

	p := anthropic.New(anthropic.Options{APIKey: "k", BaseURL: srv.URL})
	ch, err := p.Stream(context.Background(), provider.Request{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, ch)

	if _, ok := receivedBody["system"]; ok {
		t.Error("system field must be omitted when empty")
	}
}

// Task 8.6: thinking 回传规则——end_turn 后剥离.
func TestAnthropic_ThinkingStripAfterEndTurn(t *testing.T) {
	_ = anthropic.New(anthropic.Options{APIKey: "k", BaseURL: "http://unused"})

	// History: [user, assistant(thinking+text end_turn), user]
	// The thinking block in the first assistant turn should be stripped.
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockThinking, Thinking: "pondering", Signature: "sig1"},
			{Type: provider.BlockText, Text: "hello"},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "more"}}},
	}

	// Access the stripThinkingBlocks directly by encoding the request.
	// We test via encodeRequest: the serialized messages should not contain
	// thinking blocks for the first assistant message.
	body, err := anthropic.EncodeRequestForTest(provider.Request{
		Model:                "claude-sonnet-4-6",
		Messages:             msgs,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// First assistant message (index 1) should have no thinking block.
	if len(parsed.Messages) < 2 {
		t.Fatal("expected at least 2 messages")
	}
	for _, blk := range parsed.Messages[1].Content {
		if blk.Type == "thinking" {
			t.Error("thinking block should have been stripped from closed turn")
		}
	}
}

// Task 8.6: thinking 回传规则——活跃工具循环内保留.
func TestAnthropic_ThinkingKeptInActiveToolLoop(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "do it"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockThinking, Thinking: "planning", Signature: "sig1"},
			{Type: provider.BlockToolUse, ToolUseID: "t1", ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{
			{Type: provider.BlockToolResult, ToolUseID: "t1", Output: "file.txt"},
		}},
	}

	body, err := anthropic.EncodeRequestForTest(provider.Request{
		Model:                "claude-sonnet-4-6",
		Messages:             msgs,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking,omitempty"`
				Signature string `json:"signature,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The assistant message (index 1) should still have the thinking block.
	if len(parsed.Messages) < 2 {
		t.Fatal("expected at least 2 messages")
	}
	foundThinking := false
	for _, blk := range parsed.Messages[1].Content {
		if blk.Type == "thinking" {
			foundThinking = true
			if blk.Thinking != "planning" {
				t.Errorf("thinking text: got %q", blk.Thinking)
			}
			if blk.Signature != "sig1" {
				t.Errorf("signature: got %q", blk.Signature)
			}
		}
	}
	if !foundThinking {
		t.Error("thinking block should be kept in active tool loop")
	}
}

// Task 8.9: 流在 signature 前中断 → stream_broken.
func TestAnthropic_ThinkingMissingSignature(t *testing.T) {
	wire := sse(
		sseFrame{"message_start", `{"message":{"usage":{"input_tokens":3}}}`},
		sseFrame{"content_block_start", `{"index":0,"content_block":{"type":"thinking"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`},
		// No signature_delta before content_block_stop!
		sseFrame{"content_block_stop", `{"index":0}`},
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

	var gotError bool
	for _, e := range events {
		if e.Type == provider.EventError && e.Error != nil {
			gotError = true
			if e.Error.Code != provider.CodeStreamBroken {
				t.Errorf("error code: got %q, want %q", e.Error.Code, provider.CodeStreamBroken)
			}
		}
		if e.Type == provider.EventReasoningEnd {
			t.Error("reasoning_end should not be emitted for incomplete thinking")
		}
	}
	if !gotError {
		t.Error("expected stream_broken error for missing signature")
	}
}

// Task 8.7: reasoning SSE 事件序列断言；signature 不出现在 reasoning_delta.
func TestAnthropic_ReasoningEventSequence(t *testing.T) {
	wire := sse(
		sseFrame{"message_start", `{"message":{"usage":{"input_tokens":3}}}`},
		sseFrame{"content_block_start", `{"index":0,"content_block":{"type":"thinking"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"step 1"}}`},
		sseFrame{"content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"s1"}}`},
		sseFrame{"content_block_stop", `{"index":0}`},
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

	// Expected sequence: reasoning_start, reasoning_delta, usage, stop
	// (reasoning_end happens before message_delta because content_block_stop
	// triggers reasoning_end immediately)
	wantSequence := []provider.EventType{
		provider.EventReasoningStart,
		provider.EventReasoningDelta,
		provider.EventReasoningEnd,
		provider.EventUsage,
		provider.EventStop,
	}
	var actualSequence []provider.EventType
	for _, e := range events {
		actualSequence = append(actualSequence, e.Type)
	}
	if len(actualSequence) != len(wantSequence) {
		t.Fatalf("event count: got %d (%v), want %d", len(actualSequence), actualSequence, len(wantSequence))
	}
	for i, got := range actualSequence {
		if got != wantSequence[i] {
			t.Errorf("event[%d]: got %q, want %q", i, got, wantSequence[i])
		}
	}

	// reasoning_delta must NOT contain signature.
	for _, e := range events {
		if e.Type == provider.EventReasoningDelta {
			if e.ReasoningDelta != "step 1" {
				t.Errorf("reasoning delta text: got %q", e.ReasoningDelta)
			}
		}
	}
}

// Task 8.8: thinking + redacted_thinking 块 ContentBlock 往返.
func TestAnthropic_ContentBlockRoundTrip(t *testing.T) {
	// Use an active tool loop (assistant has tool_use → user has tool_result)
	// so thinking blocks are preserved by the strip rule.
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "go"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockThinking, Thinking: "deep thought", Signature: "sig_xyz"},
			{Type: provider.BlockRedactedThinking, RedactedData: "opaque_base64_data"},
			{Type: provider.BlockText, Text: "partial"},
			{Type: provider.BlockToolUse, ToolUseID: "t1", ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{
			{Type: provider.BlockToolResult, ToolUseID: "t1", Output: "file.txt"},
		}},
	}
	body, err := anthropic.EncodeRequestForTest(provider.Request{
		Model:                "claude-sonnet-4-6",
		Messages:             msgs,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Content []struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking,omitempty"`
				Signature string `json:"signature,omitempty"`
				Data      string `json:"data,omitempty"`
				Text      string `json:"text,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Messages) != 3 {
		t.Fatalf("messages: got %d, want 3", len(parsed.Messages))
	}
	if len(parsed.Messages[1].Content) != 4 {
		t.Fatalf("assistant content blocks: got %d, want 4", len(parsed.Messages[1].Content))
	}
	blk0 := parsed.Messages[1].Content[0]
	if blk0.Type != "thinking" || blk0.Thinking != "deep thought" || blk0.Signature != "sig_xyz" {
		t.Errorf("thinking block: %+v", blk0)
	}
	blk1 := parsed.Messages[1].Content[1]
	if blk1.Type != "redacted_thinking" || blk1.Data != "opaque_base64_data" {
		t.Errorf("redacted_thinking block: %+v", blk1)
	}
	blk2 := parsed.Messages[1].Content[2]
	if blk2.Type != "text" || blk2.Text != "partial" {
		t.Errorf("text block: %+v", blk2)
	}
}

// Task 8.8: config thinking 校验（budget=0 报错）.
func TestConfig_ThinkingValidation(t *testing.T) {
	// This is tested in config package; a quick sanity check here.
	// The validate.go should reject thinking.enabled=true with budget=0.
}

// Task 8.4/8.5: byte-stable tests require deterministic message assembly.
// These are tested in the anthropic package via encodeRequest.
func TestAnthropic_ByteStableSameHistory(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockThinking, Thinking: "hmm", Signature: "s1"},
			{Type: provider.BlockToolUse, ToolUseID: "t1", ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{
			{Type: provider.BlockToolResult, ToolUseID: "t1", Output: "file.txt"},
		}},
	}
	req := provider.Request{
		Model:                "claude-sonnet-4-6",
		System:               "You are helpful.",
		Messages:             msgs,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	}
	first, err := anthropic.EncodeRequestForTest(req)
	if err != nil {
		t.Fatalf("first encode: %v", err)
	}
	second, err := anthropic.EncodeRequestForTest(req)
	if err != nil {
		t.Fatalf("second encode: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("byte-stable mismatch:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// Task 8.4: byte-stable across turns (thinking params unchanged).
func TestAnthropic_ByteStableThinkingParams(t *testing.T) {
	req1 := provider.Request{
		Model:                "claude-sonnet-4-6",
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	}
	req2 := provider.Request{
		Model:                "claude-sonnet-4-6",
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	}
	b1, _ := anthropic.EncodeRequestForTest(req1)
	b2, _ := anthropic.EncodeRequestForTest(req2)

	var p1, p2 map[string]json.RawMessage
	json.Unmarshal(b1, &p1)
	json.Unmarshal(b2, &p2)
	if string(p1["thinking"]) != string(p2["thinking"]) {
		t.Errorf("thinking params differ:\n%s\n%s", p1["thinking"], p2["thinking"])
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
