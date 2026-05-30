package e2e

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// TestE2E_ThinkingSession verifies a session with thinking produces
// reasoning_start/delta/end SSE events followed by assistant_text events.
func TestE2E_ThinkingSession(t *testing.T) {
	s := newStack(t)

	s.mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventReasoningStart, BlockIndex: 0, ReasoningType: "thinking"},
		{Type: provider.EventReasoningDelta, ReasoningDelta: "planning", BlockIndex: 0},
		{Type: provider.EventReasoningDelta, ReasoningDelta: " the answer", BlockIndex: 0},
		{Type: provider.EventReasoningEnd, BlockIndex: 0, ReasoningBlock: &provider.ContentBlock{
			Type:      provider.BlockThinking,
			Thinking:  "planning the answer",
			Signature: "sig_e2e",
		}},
		{Type: provider.EventTextDelta, TextDelta: "42"},
		{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 10, OutputTokens: 5}},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	id := s.createSessionViaAPI(false)

	resp, rd := s.openSSE(id, "")
	defer resp.Body.Close()

	postResp := s.postStream(id, `{"type":"user_message","content":"what is the answer?"}`)
	if postResp.StatusCode != 202 {
		t.Fatalf("post status: %d", postResp.StatusCode)
	}
	postResp.Body.Close()

	// Collect SSE event types.
	deadline := time.Now().Add(5 * time.Second)
	var eventTypes []string
	for time.Now().Before(deadline) {
		f, err := readFrame(s.t, rd)
		if err != nil {
			break
		}
		if f.Data == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
			continue
		}
		typ, _ := ev["type"].(string)
		if typ == "" {
			continue
		}
		eventTypes = append(eventTypes, typ)
		if typ == "assistant_text_done" {
			break
		}
	}

	want := []string{"reasoning_start", "reasoning_delta", "reasoning_delta", "reasoning_end", "assistant_text_delta", "assistant_text_done"}
	for _, w := range want {
		found := false
		for _, got := range eventTypes {
			if got == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %q; got %v", w, eventTypes)
		}
	}

	reqs := s.mock.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests received by mock provider")
	}
}
