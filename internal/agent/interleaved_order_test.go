package agent_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// #2: with interleaved thinking, a single assistant turn can emit
// thinking → tool_use → thinking → tool_use. The loop must persist those blocks
// in emission order (not regrouped as thinking,thinking,tool,tool), or the
// next round-trip desyncs each signed thinking block from its tool_use and
// Anthropic rejects it. Also asserts the real stop_reason is recorded (#5).
func TestLoop_InterleavedThinkingPreservesBlockOrder(t *testing.T) {
	h := newLoopHarness(t)
	for _, name := range []string{"ToolA", "ToolB"} {
		if err := h.Registry.Register(&loopStubTool{
			name:     name,
			parallel: true,
			readOnly: true,
			body: func(ctx context.Context, _ json.RawMessage) (*tools.Result, error) {
				return &tools.Result{Output: "ok"}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Turn 1: thinking_A, tool_use A, thinking_B, tool_use B (interleaved).
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventReasoningStart, BlockIndex: 0, ReasoningType: "thinking"},
		{Type: provider.EventReasoningDelta, BlockIndex: 0, ReasoningDelta: "plan A"},
		{Type: provider.EventReasoningEnd, BlockIndex: 0, ReasoningBlock: &provider.ContentBlock{
			Type: provider.BlockThinking, Thinking: "tA", Signature: "sA",
		}},
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type: provider.BlockToolUse, ToolUseID: "a1", ToolName: "ToolA", Input: json.RawMessage(`{}`),
		}},
		{Type: provider.EventReasoningStart, BlockIndex: 1, ReasoningType: "thinking"},
		{Type: provider.EventReasoningEnd, BlockIndex: 1, ReasoningBlock: &provider.ContentBlock{
			Type: provider.BlockThinking, Thinking: "tB", Signature: "sB",
		}},
		{Type: provider.EventToolUse, ToolUse: &provider.ContentBlock{
			Type: provider.BlockToolUse, ToolUseID: "b1", ToolName: "ToolB", Input: json.RawMessage(`{}`),
		}},
		{Type: provider.EventStop, StopReason: "tool_use"},
	})
	// Turn 2: terminal text.
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "done"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "go")

	h.collectUntil(t, 3*time.Second, func(es []session.Event) bool {
		return countType(es, "assistant_text_done") >= 1
	})
	waitForState(t, h.Session, session.StateIdle, time.Second)

	hist := h.Session.History()
	if len(hist) < 2 {
		t.Fatalf("history too short: %d", len(hist))
	}
	turn1 := hist[1] // user, [assistant turn1], ...
	if turn1.Role != provider.RoleAssistant {
		t.Fatalf("history[1] should be the assistant turn, got %q", turn1.Role)
	}
	if turn1.StopReason != "tool_use" {
		t.Errorf("assistant turn stop_reason: got %q, want tool_use", turn1.StopReason)
	}

	gotTypes := make([]string, len(turn1.Content))
	for i, b := range turn1.Content {
		gotTypes[i] = string(b.Type)
	}
	wantTypes := []string{"thinking", "tool_use", "thinking", "tool_use"}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("block count: got %v, want %v", gotTypes, wantTypes)
	}
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Fatalf("block order: got %v, want %v", gotTypes, wantTypes)
		}
	}
	// Spot-check the interleaving carried the right payloads in place.
	if turn1.Content[0].Thinking != "tA" || turn1.Content[1].ToolUseID != "a1" ||
		turn1.Content[2].Thinking != "tB" || turn1.Content[3].ToolUseID != "b1" {
		t.Errorf("interleaved payloads misplaced: %+v", turn1.Content)
	}
}
