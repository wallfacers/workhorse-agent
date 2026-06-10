package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
)

// session-management spec scenario: instructions 注入 system prompt 动态段 —
// the session-level instructions appear after the static base, never inside
// the cache prefix.
func TestLoop_SessionInstructionsJoinDynamicSegment(t *testing.T) {
	const base = "BASEPROMPT-STATIC-PREFIX"
	const extra = "当前页面上下文：taskId=T-1024"

	run := func(instructions string) string {
		h := newLoopHarness(t)
		h.Loop.SystemPromptBase = base
		h.Session.Instructions = instructions
		h.Mock.QueueResponse([]provider.ProviderEvent{
			{Type: provider.EventTextDelta, TextDelta: "hi"},
			{Type: provider.EventStop, StopReason: "end_turn"},
		})
		h.start()
		defer h.stop()
		h.sendUser(t, "hello")
		h.collectUntil(t, 2*time.Second, func(es []session.Event) bool {
			return hasType(es, "assistant_text_done")
		})
		reqs := h.Mock.Requests()
		if len(reqs) == 0 {
			t.Fatal("no provider request issued")
		}
		return reqs[0].System
	}

	with := run(extra)
	without := run("")

	if !strings.HasPrefix(with, base) || !strings.HasPrefix(without, base) {
		t.Fatal("static base must stay the prompt prefix")
	}
	if !strings.Contains(with, "<session-instructions>\n"+extra+"\n</session-instructions>") {
		t.Fatalf("session instructions missing from system prompt:\n%s", with)
	}
	if strings.Contains(without, "<session-instructions>") {
		t.Fatal("no-instructions session must not carry the block")
	}
	// The injected block must sit strictly after the base segment.
	if idx := strings.Index(with, "<session-instructions>"); idx < len(base) {
		t.Fatalf("session instructions leaked into the cache prefix (idx=%d)", idx)
	}
}
