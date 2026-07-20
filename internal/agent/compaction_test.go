package agent_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/test/mockprovider"
)

func summaryProvider(t *testing.T) *mockprovider.Provider {
	t.Helper()
	mp := mockprovider.New("mock")
	mp.SetFallback(func() []provider.ProviderEvent {
		return []provider.ProviderEvent{
			{Type: provider.EventTextDelta, TextDelta: "summarised earlier turns"},
			{Type: provider.EventStop, StopReason: "end_turn"},
		}
	})
	return mp
}

func makeHistory(n int) []provider.Message {
	out := make([]provider.Message, n)
	for i := range out {
		role := provider.RoleUser
		if i%2 == 1 {
			role = provider.RoleAssistant
		}
		out[i] = provider.Message{
			Role:    role,
			Content: []provider.ContentBlock{{Type: provider.BlockText, Text: fmt.Sprintf("message %d", i)}},
		}
	}
	return out
}

func TestCompactWithKeep_NoSpaceReturnsFalse(t *testing.T) {
	c := &agent.Compactor{Provider: summaryProvider(t), RecentKeep: 8, MaxTokens: 100}
	history := makeHistory(2) // <= keep+1 => nothing to compact

	out, _, ok, err := c.CompactWithKeep(context.Background(), history, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when there is nothing to compact")
	}
	if len(out) != len(history) {
		t.Fatalf("history must be returned unchanged, got len %d want %d", len(out), len(history))
	}
}

func TestCompactWithKeep_OverridesRetainedCount(t *testing.T) {
	history := makeHistory(12)

	// keep=2 -> summary(1) + recent(2) = 3 retained messages.
	c2 := &agent.Compactor{Provider: summaryProvider(t), RecentKeep: 8, MaxTokens: 100}
	_, res2, ok2, err := c2.CompactWithKeep(context.Background(), history, 2)
	if err != nil || !ok2 {
		t.Fatalf("keep=2: ok=%v err=%v", ok2, err)
	}

	// keep=8 -> summary(1) + recent(8) = 9 retained messages.
	c8 := &agent.Compactor{Provider: summaryProvider(t), RecentKeep: 8, MaxTokens: 100}
	_, res8, ok8, err := c8.CompactWithKeep(context.Background(), history, 8)
	if err != nil || !ok8 {
		t.Fatalf("keep=8: ok=%v err=%v", ok8, err)
	}

	if res2.AfterMessages >= res8.AfterMessages {
		t.Fatalf("smaller keep must retain fewer messages: keep=2 after=%d, keep=8 after=%d",
			res2.AfterMessages, res8.AfterMessages)
	}
	if res2.AfterMessages != 3 {
		t.Fatalf("keep=2 after_messages: got %d want 3 (summary + 2)", res2.AfterMessages)
	}
}

func TestCompact_DelegatesToRecentKeep(t *testing.T) {
	history := makeHistory(12)
	c := &agent.Compactor{Provider: summaryProvider(t), RecentKeep: 3, MaxTokens: 100}
	out, res, ok, err := c.Compact(context.Background(), history)
	if err != nil || !ok {
		t.Fatalf("Compact: ok=%v err=%v", ok, err)
	}
	// summary(1) + recent(3) = 4
	if res.AfterMessages != 4 {
		t.Fatalf("Compact with RecentKeep=3: after=%d want 4", res.AfterMessages)
	}
	if out[0].Role != provider.RoleSystem {
		t.Fatalf("first retained message must be the summary (system), got %v", out[0].Role)
	}
}
