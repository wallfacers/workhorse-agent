package judge

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

func TestCollectTrace_TextOnly(t *testing.T) {
	sse := strings.Join([]string{
		"event: assistant_text_delta",
		"data: {\"delta\":\"hello \"}",
		"",
		"event: assistant_text_delta",
		"data: {\"delta\":\"world\"}",
		"",
		"event: assistant_text_done",
		"data: {\"stop_reason\":\"end_turn\"}",
		"",
	}, "\n")

	trace, err := CollectTrace("test", "hi", bufio.NewReader(strings.NewReader(sse)), 5*time.Second)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if trace.UserMessage != "hi" {
		t.Errorf("user message = %q, want hi", trace.UserMessage)
	}
	if len(trace.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(trace.Turns))
	}
	if trace.Turns[0].ModelOutput != "hello world" {
		t.Errorf("output = %q, want hello world", trace.Turns[0].ModelOutput)
	}
}

func TestCollectTrace_WithToolCall(t *testing.T) {
	sse := strings.Join([]string{
		"event: tool_call_start",
		"data: {\"tool_name\":\"Read\"}",
		"",
		"event: tool_call_done",
		"data: {\"tool_name\":\"Read\",\"output\":\"file contents here\",\"is_error\":false}",
		"",
		"event: assistant_text_delta",
		"data: {\"delta\":\"The file says hello\"}",
		"",
		"event: assistant_text_done",
		"data: {\"stop_reason\":\"end_turn\"}",
		"",
	}, "\n")

	trace, err := CollectTrace("test", "read file", bufio.NewReader(strings.NewReader(sse)), 5*time.Second)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(trace.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(trace.Turns))
	}
	if len(trace.Turns[0].ToolCalls) != 1 || trace.Turns[0].ToolCalls[0].ToolName != "Read" {
		t.Errorf("tool calls = %v, want [Read]", trace.Turns[0].ToolCalls)
	}
	if len(trace.Turns[0].ToolResults) != 1 || trace.Turns[0].ToolResults[0].Output != "file contents here" {
		t.Errorf("tool results = %v", trace.Turns[0].ToolResults)
	}
	if trace.Turns[0].ModelOutput != "The file says hello" {
		t.Errorf("output = %q, want 'The file says hello'", trace.Turns[0].ModelOutput)
	}
}

func TestCollectTrace_Timeout(t *testing.T) {
	trace, err := CollectTrace("test", "hi", bufio.NewReader(strings.NewReader("")), 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if trace == nil {
		t.Fatal("expected non-nil trace even on timeout")
	}
}
