package pipeline

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// SessionIngestor adapts a Pipeline to the agent.MemoryIngestor interface: it
// converts a finished session's provider.Message history into extraction
// messages and runs the pipeline on a detached goroutine so loop teardown never
// blocks on the extraction model call.
//
// A nil *SessionIngestor is inert (IngestSession is a no-op), the state when the
// pipeline is disabled.
type SessionIngestor struct {
	p       *Pipeline
	timeout time.Duration

	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// NewSessionIngestor wraps a Pipeline. Returns nil when the pipeline is nil.
func NewSessionIngestor(p *Pipeline) *SessionIngestor {
	if p == nil {
		return nil
	}
	return &SessionIngestor{p: p, timeout: 2 * time.Minute}
}

// IngestSession launches extraction over the session history in the background.
// Non-blocking; safe to call on a nil ingestor.
func (s *SessionIngestor) IngestSession(sessionID string, history []provider.Message) {
	if s == nil {
		return
	}
	msgs := toExtractionMessages(history)
	if len(msgs) == 0 {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		n, err := s.p.Ingest(ctx, time.Now().UTC(), sessionID, msgs)
		if err != nil {
			slog.Warn("memory: session ingest failed", "session", sessionID, "err", err)
			return
		}
		if n > 0 {
			slog.Info("memory: extracted facts from session", "session", sessionID, "count", n)
		}
	}()
}

// Close waits for in-flight ingests to finish (best-effort flush on shutdown).
func (s *SessionIngestor) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait()
}

// toExtractionMessages flattens provider messages into role/text pairs, keeping
// only user and assistant text blocks (tool traffic is not memory material).
func toExtractionMessages(history []provider.Message) []Message {
	out := make([]Message, 0, len(history))
	for _, m := range history {
		role := string(m.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		var text strings.Builder
		for _, block := range m.Content {
			if block.Type == provider.BlockText && strings.TrimSpace(block.Text) != "" {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(block.Text)
			}
		}
		if text.Len() == 0 {
			continue
		}
		out = append(out, Message{Role: role, Text: text.String()})
	}
	return out
}
