package agent_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
)

// fakeNotificationSource mimics a delegation store: ConsumePending returns the
// pending notices once, then clears them (exactly-once).
type fakeNotificationSource struct {
	mu      sync.Mutex
	pending []string
	calls   int
}

func (f *fakeNotificationSource) ConsumePending(_ context.Context, _ string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := f.pending
	f.pending = nil
	return out
}

func (f *fakeNotificationSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestLoop_InjectsNotificationsBeforeUserMessage(t *testing.T) {
	src := &fakeNotificationSource{pending: []string{"[Delegation done] brisk-amber-fox — Auth Flow"}}
	h := newLoopHarness(t, func(h *loopHarness) {
		h.Loop.Config.Notifications = src
	})
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "ok"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "hello")

	h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "assistant_text_done")
	})
	waitForState(t, h.Session, session.StateIdle, time.Second)

	hist := h.Session.History()
	if len(hist) < 3 {
		t.Fatalf("history too short: %d (%+v)", len(hist), hist)
	}
	if hist[0].Role != provider.RoleSystem {
		t.Fatalf("first message must be the system notice, got role %v", hist[0].Role)
	}
	if !strings.Contains(hist[0].Content[0].Text, "brisk-amber-fox") {
		t.Fatalf("notice text: %q", hist[0].Content[0].Text)
	}
	if hist[1].Role != provider.RoleUser {
		t.Fatalf("second message must be the user message, got role %v", hist[1].Role)
	}
	if hist[2].Role != provider.RoleAssistant {
		t.Fatalf("third message must be the assistant turn, got role %v", hist[2].Role)
	}
}

func TestLoop_NotificationsInjectedExactlyOnceAcrossTurns(t *testing.T) {
	src := &fakeNotificationSource{pending: []string{"notice-1"}}
	h := newLoopHarness(t, func(h *loopHarness) {
		h.Loop.Config.Notifications = src
	})
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "turn1"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "turn2"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()

	h.sendUser(t, "first")
	h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "assistant_text_done")
	})
	waitForState(t, h.Session, session.StateIdle, time.Second)

	h.sendUser(t, "second")
	h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "assistant_text_done")
	})
	waitForState(t, h.Session, session.StateIdle, time.Second)

	hist := h.Session.History()
	systemCount, userCount := 0, 0
	for _, m := range hist {
		switch m.Role {
		case provider.RoleSystem:
			systemCount++
		case provider.RoleUser:
			userCount++
		}
	}
	if systemCount != 1 {
		t.Fatalf("system notice count: want 1 (exactly-once), got %d", systemCount)
	}
	if userCount != 2 {
		t.Fatalf("user message count: want 2, got %d", userCount)
	}
	if src.callCount() != 2 {
		t.Fatalf("ConsumePending should run once per turn (2), got %d", src.callCount())
	}
}

func TestLoop_NilNotificationsIsNoOp(t *testing.T) {
	h := newLoopHarness(t) // Notifications left nil
	h.Mock.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "ok"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	h.start()
	defer h.stop()
	h.sendUser(t, "hello")

	h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
		return hasType(es, "assistant_text_done")
	})
	waitForState(t, h.Session, session.StateIdle, time.Second)

	for _, m := range h.Session.History() {
		if m.Role == provider.RoleSystem {
			t.Fatalf("nil Notifications must not inject any system message: %+v", m)
		}
	}
}
