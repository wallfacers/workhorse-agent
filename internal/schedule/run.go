package schedule

import (
	"context"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
)

// pump drains the unattended session's outbox until its turn ends, feeding
// assistant text and error events to the collector. Mirrors the delegation
// background pump without event forwarding — the run log only needs the final
// text.
func pump(ctx context.Context, child *session.Session, c *collector, done chan<- struct{}) {
	defer close(done)

	const gracePeriod = 50 * time.Millisecond
	turnEnded := false
	var graceDeadline time.Time

	for {
		var wait time.Duration
		if turnEnded {
			wait = time.Until(graceDeadline)
			if wait <= 0 {
				drain(child, c)
				return
			}
		} else {
			wait = time.Second
		}
		timer := time.NewTimer(wait)

		select {
		case ev := <-child.Outbox:
			timer.Stop()
			c.observe(ev)
			if !turnEnded && isTurnEnd(ev) {
				turnEnded = true
				graceDeadline = time.Now().Add(gracePeriod)
			}
		case <-ctx.Done():
			timer.Stop()
			drain(child, c)
			return
		case <-timer.C:
			if turnEnded {
				drain(child, c)
				return
			}
			if child.State() == session.StateIdle {
				graceDeadline = time.Now().Add(gracePeriod)
				turnEnded = true
			}
		}
	}
}

func drain(child *session.Session, c *collector) {
	for {
		select {
		case ev := <-child.Outbox:
			c.observe(ev)
		default:
			return
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
		return true
	default:
		return false
	}
}

type collector struct {
	mu        sync.Mutex
	curAccum  string
	lastFinal string
	errMsg    string
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
	case "error":
		if msg, ok := ev.Payload["message"].(string); ok && msg != "" {
			c.errMsg = msg
		} else if code, ok := ev.Payload["code"].(string); ok {
			c.errMsg = code
		}
	}
}

func (c *collector) FinalText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastFinal != "" {
		return c.lastFinal
	}
	return c.curAccum
}

func (c *collector) ErrorMessage() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errMsg
}

// tailText returns the trailing slice of s bounded by maxBytes, aligned to a
// UTF-8 rune boundary so the result is never a broken multibyte sequence.
func tailText(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && (s[start]&0xC0) == 0x80 {
		start++
	}
	return s[start:]
}
