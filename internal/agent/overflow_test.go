package agent_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

// withFastCompactor arms the loop with a Compactor backed by a mock fast
// provider that always returns a fixed summary, and pins CompactRecentKeep so
// the relaxed self-heal keep is deterministic.
func withFastCompactor(recentKeep int) func(*loopHarness) {
	return func(h *loopHarness) {
		fast := mockprovider.New("fast")
		fast.SetFallback(func() []provider.ProviderEvent {
			return []provider.ProviderEvent{
				{Type: provider.EventTextDelta, TextDelta: "summarised old turns"},
				{Type: provider.EventStop, StopReason: "end_turn"},
			}
		})
		h.Loop.Compactor = &agent.Compactor{Provider: fast, Model: "fast", RecentKeep: recentKeep, MaxTokens: 100}
		h.Loop.Config.CompactRecentKeep = recentKeep
	}
}

func seedHistory(t *testing.T, sess *session.Session, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		sess.AppendMessage(context.Background(), provider.Message{
			Role:    role,
			Content: []provider.ContentBlock{{Type: provider.BlockText, Text: fmt.Sprintf("history message %d", i)}},
		})
	}
}

func ctxLenErr() *provider.ProviderError {
	return provider.NewProviderError("mock", 400, provider.CodeContextLengthExceeded, "context too long", nil)
}

// Scenario 1: first call overflows, the loop compacts and retries successfully;
// the client sees a compaction event and the final answer, but no error.
func TestLoop_OverflowSelfHeal_RetriesAndSucceeds(t *testing.T) {
	h := newLoopHarness(t, withFastCompactor(4))
	seedHistory(t, h.Session, 12)

	h.Mock.QueueError(ctxLenErr())
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "recovered answer"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "go")

	events := h.collectUntil(t, 3*time.Second, func(es []session.Event) bool {
		return countType(es, "assistant_text_done") >= 1
	})
	if hasType(events, "error") {
		t.Fatalf("self-healed turn must not emit an error: %v", eventTypes(events))
	}
	if countType(events, "compaction") != 1 {
		t.Fatalf("want exactly one compaction event, got %d (%v)", countType(events, "compaction"), eventTypes(events))
	}
	if len(h.Mock.Requests()) != 2 {
		t.Fatalf("want 2 provider calls (overflow then retry), got %d", len(h.Mock.Requests()))
	}
}

// Scenario 2: the retry still overflows; the loop self-heals at most once and
// then surfaces the error.
func TestLoop_OverflowSelfHeal_AtMostOneRetryThenError(t *testing.T) {
	h := newLoopHarness(t, withFastCompactor(4))
	seedHistory(t, h.Session, 12)

	// Every provider stream overflows (mid-stream EventError) so the first
	// attempt self-heals and the retry overflows again.
	h.Mock.SetFallback(func() []provider.ProviderEvent {
		return []provider.ProviderEvent{{Type: provider.EventError, Error: ctxLenErr()}}
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "go")

	events := h.collectUntil(t, 3*time.Second, func(es []session.Event) bool {
		return hasType(es, "error")
	})
	if countType(events, "compaction") != 1 {
		t.Fatalf("want exactly one compaction attempt, got %d", countType(events, "compaction"))
	}
	if !hasType(events, "error") {
		t.Fatalf("want an error event after the second overflow: %v", eventTypes(events))
	}
}

// Scenario 3: partial output was streamed before the overflow; the loop must
// NOT self-heal (it would duplicate the partial output).
func TestLoop_OverflowSelfHeal_NoRetryAfterPartialOutput(t *testing.T) {
	h := newLoopHarness(t, withFastCompactor(4))
	seedHistory(t, h.Session, 12)

	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "partial output"},
		{Type: provider.EventError, Error: ctxLenErr()},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "go")

	events := h.collectUntil(t, 3*time.Second, func(es []session.Event) bool {
		return hasType(es, "error")
	})
	if hasType(events, "compaction") {
		t.Fatalf("must not self-heal after partial output: %v", eventTypes(events))
	}
	if !hasType(events, "error") {
		t.Fatalf("want an error event: %v", eventTypes(events))
	}
}

// Scenario 4 (edge case): history is too short to compact; the loop does not
// retry and surfaces the error directly.
func TestLoop_OverflowSelfHeal_NoCompactionSpace(t *testing.T) {
	h := newLoopHarness(t, withFastCompactor(4))
	seedHistory(t, h.Session, 1) // 1 + user = 2 <= keep+1 (relaxed keep=2 => 3)

	h.Mock.QueueError(ctxLenErr())

	h.start()
	defer h.stop()
	h.sendUser(t, "go")

	events := h.collectUntil(t, 3*time.Second, func(es []session.Event) bool {
		return hasType(es, "error")
	})
	if hasType(events, "compaction") {
		t.Fatalf("must not compact when there is nothing to compact: %v", eventTypes(events))
	}
	if !hasType(events, "error") {
		t.Fatalf("want an error event: %v", eventTypes(events))
	}
}
