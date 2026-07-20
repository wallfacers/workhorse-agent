package dispatch

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
)

// pump drains the child session's outbox until the child's turn ends. It
// recognises end-of-turn from the events themselves — assistant_text_done
// with stop_reason="end_turn", interrupted, or a fatal error — rather than
// from state polling, because the child's Idle→Thinking→Idle cycle can
// complete inside a single ticker interval and be missed entirely.
//
// In streaming mode each child event is wrapped as a subagent_event on the
// parent's outbox, and a tool_call_start also emits a human-readable
// subagent_status summary (US4). In blocking mode events feed only the
// collector.
//
// agentType/description are threaded through so every subagent_status event
// carries the sub-agent's role and task description.
//
// After the end-of-turn signal we still drain a brief grace window so
// trailing events (a late `pong` etc.) make it through before the pump exits,
// and on exit we emit a final subagent_status with an empty activity so
// clients can clear the status line.
func pump(
	ctx context.Context,
	child, parent *session.Session,
	mode, agentType, description string,
	c *collector,
	done chan<- struct{},
) {
	defer close(done)
	defer func() {
		if mode == modeStreaming {
			emitStatusClear(parent, child.ID, agentType, description)
		}
	}()

	const gracePeriod = 50 * time.Millisecond

	turnEnded := false
	var graceDeadline time.Time

	for {
		// Pick the wait timeout based on whether we're in the grace window.
		var wait time.Duration
		if turnEnded {
			wait = time.Until(graceDeadline)
			if wait <= 0 {
				drainRemaining(parent, child, mode, agentType, description, c)
				return
			}
		} else {
			wait = time.Second // long; we wake on events or ctx.Done
		}
		timer := time.NewTimer(wait)

		select {
		case ev := <-child.Outbox:
			timer.Stop()
			c.observe(ev)
			if mode == modeStreaming {
				forward(ctx, parent, child.ID, agentType, description, ev)
			}
			if !turnEnded && isTurnEnd(ev) {
				turnEnded = true
				graceDeadline = time.Now().Add(gracePeriod)
			}
		case <-ctx.Done():
			timer.Stop()
			drainRemaining(parent, child, mode, agentType, description, c)
			return
		case <-timer.C:
			if turnEnded {
				drainRemaining(parent, child, mode, agentType, description, c)
				return
			}
			// No end-of-turn signal yet and no events arrived in a second —
			// keep waiting, but fall back to state polling so an empty turn
			// (no text, no tools) still terminates.
			if child.State() == session.StateIdle {
				// Try one more quick window for late events, then give up.
				graceDeadline = time.Now().Add(gracePeriod)
				turnEnded = true
			}
		}
	}
}

func isTurnEnd(ev session.Event) bool {
	switch ev.Type {
	case "assistant_text_done":
		if sr, ok := ev.Payload["stop_reason"].(string); ok && sr == "end_turn" {
			return true
		}
		return false
	case "interrupted":
		return true
	case "error":
		// Any error event terminates the turn from dispatch's POV; the
		// collector records the message for the result.
		return true
	default:
		return false
	}
}

// forward wraps a child event into a `subagent_event` payload on the parent's
// outbox. When the child event is a tool_call_start it also emits a
// `subagent_status` summary translating the tool call to one human-readable
// line (US4). EmitNow is non-blocking; a full parent outbox silently drops the
// status (best-effort, FR-022 — the sub-agent task itself is unaffected).
func forward(ctx context.Context, parent *session.Session, childID, agentType, description string, ev session.Event) {
	if ctx.Err() != nil {
		return
	}
	inner := map[string]any{
		"type": ev.Type,
		"idx":  ev.Idx,
	}
	for k, v := range ev.Payload {
		inner[k] = v
	}
	parent.EmitNow("subagent_event", map[string]any{
		"agent_id": childID,
		"event":    inner,
	})
	if ev.Type == "tool_call_start" {
		toolName, _ := ev.Payload["tool"].(string)
		input, _ := ev.Payload["input"].(json.RawMessage)
		parent.EmitNow("subagent_status", map[string]any{
			"agent_id":    childID,
			"agent_type":  agentType,
			"description": description,
			"activity":    FormatActivity(toolName, input),
		})
	}
}

// emitStatusClear publishes a subagent_status with an empty activity so clients
// can remove the sub-agent's status line when its turn ends (US4 scenario 2).
func emitStatusClear(parent *session.Session, childID, agentType, description string) {
	parent.EmitNow("subagent_status", map[string]any{
		"agent_id":    childID,
		"agent_type":  agentType,
		"description": description,
		"activity":    "",
	})
}

// drainRemaining flushes whatever is still sitting in the child outbox before
// the pump returns. Best-effort; if the parent's outbox is full the
// subagent_event/status is dropped rather than blocked-on.
func drainRemaining(parent, child *session.Session, mode, agentType, description string, c *collector) {
	for {
		select {
		case ev := <-child.Outbox:
			c.observe(ev)
			if mode == modeStreaming {
				forward(context.Background(), parent, child.ID, agentType, description, ev)
			}
		default:
			return
		}
	}
}

// collector accumulates assistant text deltas across all of the child's
// in-turn LLM rounds. Each assistant_text_done snapshot replaces the
// "current final" — when the turn ends, the last snapshot is what we return
// as the dispatch tool's output.
type collector struct {
	mu           sync.Mutex
	curAccum     string
	lastFinal    string
	errMsg       string
	wasCancelled bool
}

func newCollector() *collector { return &collector{} }

func (c *collector) observe(ev session.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev.Type {
	case "assistant_text_delta":
		if d, ok := ev.Payload["delta"].(string); ok {
			c.curAccum += d
		}
	case "assistant_text_done":
		if c.curAccum != "" {
			c.lastFinal = c.curAccum
			c.curAccum = ""
		}
	case "interrupted":
		c.wasCancelled = true
	case "error":
		if msg, ok := ev.Payload["message"].(string); ok && msg != "" {
			c.errMsg = msg
		} else if code, ok := ev.Payload["code"].(string); ok {
			c.errMsg = code
		}
	}
}

// FinalText is the child's final assistant text snapshot.
func (c *collector) FinalText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastFinal != "" {
		return c.lastFinal
	}
	return c.curAccum
}

// ErrorMessage is non-empty when the child emitted an `error` event during the
// turn (excluding recoverable provider_retry, which is its own event).
func (c *collector) ErrorMessage() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errMsg
}

// Cancelled is true when the child emitted an `interrupted` event.
func (c *collector) Cancelled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wasCancelled
}
