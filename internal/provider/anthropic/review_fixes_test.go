package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/provider/anthropic"
)

// #1: max_tokens must exceed thinking.budget_tokens. encodeRequest rejects the
// request up front instead of letting Anthropic 400 every turn.
func TestEncode_ThinkingBudgetExceedsMaxTokens(t *testing.T) {
	_, err := anthropic.EncodeRequestForTest(provider.Request{
		Model:                "claude-sonnet-4-6",
		Messages:             []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}}},
		MaxTokens:            4096,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err == nil {
		t.Fatal("expected error when max_tokens <= thinking budget_tokens")
	}
	if !strings.Contains(err.Error(), "greater than thinking") {
		t.Errorf("error should explain the constraint, got %q", err.Error())
	}
}

// #6: a dated snapshot id of a listed base model is accepted for thinking
// without the allowlist enumerating every dated build.
func TestEncode_DatedThinkingModelSupported(t *testing.T) {
	_, err := anthropic.EncodeRequestForTest(provider.Request{
		Model:                "claude-sonnet-4-6-20250514",
		Messages:             []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}}},
		MaxTokens:            24000,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err != nil {
		t.Fatalf("dated variant of a listed model should support thinking, got %v", err)
	}
}

// #6: a genuinely unknown model still hard-fails thinking, fast.
func TestEncode_UnknownThinkingModelRejected(t *testing.T) {
	_, err := anthropic.EncodeRequestForTest(provider.Request{
		Model:                "totally-made-up-model",
		Messages:             []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}}},
		MaxTokens:            24000,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if !errors.Is(err, anthropic.ErrThinkingNotSupported) {
		t.Fatalf("expected ErrThinkingNotSupported, got %v", err)
	}
}

// #4: stripping thinking from an all-thinking closed turn empties its content;
// the encoder must skip it rather than ship content:null (Anthropic 400).
func TestEncode_AllThinkingMessageSkipped(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}},
		{Role: provider.RoleAssistant, StopReason: "end_turn", Content: []provider.ContentBlock{
			{Type: provider.BlockThinking, Thinking: "only thought, no answer", Signature: "sig1"},
		}},
	}
	body, err := anthropic.EncodeRequestForTest(provider.Request{
		Model:                "claude-sonnet-4-6",
		Messages:             msgs,
		MaxTokens:            24000,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Messages) != 1 {
		t.Fatalf("emptied assistant message should be skipped; got %d messages", len(parsed.Messages))
	}
	for _, m := range parsed.Messages {
		c := strings.TrimSpace(string(m.Content))
		if c == "null" || c == "[]" || c == "" {
			t.Errorf("message %q has empty content %q", m.Role, c)
		}
	}
}

// #5: the strip boundary prefers the real stop_reason over block shape. A turn
// marked stop_reason=tool_use is treated as an active (open) chain, so its
// thinking is preserved even though (synthetically) it carries no tool_use
// block — which the old "no tool_use ⇒ closed" heuristic would have stripped.
func TestEncode_StopReasonOverridesHeuristic(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "q1"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockThinking, Thinking: "a", Signature: "sigA"},
			{Type: provider.BlockText, Text: "ans1"},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "q2"}}},
		{Role: provider.RoleAssistant, StopReason: "tool_use", Content: []provider.ContentBlock{
			{Type: provider.BlockThinking, Thinking: "b", Signature: "sigB"},
			{Type: provider.BlockText, Text: "ans2"},
		}},
	}
	body, err := anthropic.EncodeRequestForTest(provider.Request{
		Model:                "claude-sonnet-4-6",
		Messages:             msgs,
		MaxTokens:            24000,
		ThinkingEnabled:      true,
		ThinkingBudgetTokens: 16000,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	hasThinking := decodeThinkingByMessage(t, body)
	if len(hasThinking) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(hasThinking))
	}
	if hasThinking[1] {
		t.Error("closed turn (msg 1) thinking should be stripped")
	}
	if !hasThinking[3] {
		t.Error("active turn (msg 3, stop_reason=tool_use) thinking must be kept")
	}
}

// #14: an unknown content block type is a programming error; the encoder must
// fail loudly rather than coercing a signature-bearing block into text.
func TestEncode_UnknownBlockTypeErrors(t *testing.T) {
	_, err := anthropic.EncodeRequestForTest(provider.Request{
		Model: "claude-sonnet-4-6",
		Messages: []provider.Message{{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockType("mystery_block")},
		}}},
		MaxTokens: 4096,
	})
	if err == nil {
		t.Fatal("expected error for unknown content block type")
	}
	if !strings.Contains(err.Error(), "unknown content block") {
		t.Errorf("error should name the unknown block, got %q", err.Error())
	}
}

func decodeThinkingByMessage(t *testing.T, body []byte) []bool {
	t.Helper()
	var parsed struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := make([]bool, len(parsed.Messages))
	for i, m := range parsed.Messages {
		for _, b := range m.Content {
			if b.Type == "thinking" {
				out[i] = true
			}
		}
	}
	return out
}

// #8: a redacted_thinking block that ends with no opaque data is a broken
// stream (mirrors the missing-signature guard for plain thinking), not a
// silently-corrupt block that 400s on the next round-trip.
func TestStream_RedactedThinkingMissingData(t *testing.T) {
	wire := sse(
		sseFrame{"message_start", `{"message":{"usage":{"input_tokens":3}}}`},
		sseFrame{"content_block_start", `{"index":0,"content_block":{"type":"redacted_thinking"}}`},
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
			t.Error("reasoning_end should not be emitted for a data-less redacted block")
		}
	}
	if !gotError {
		t.Error("expected stream_broken error for redacted_thinking without data")
	}
}
