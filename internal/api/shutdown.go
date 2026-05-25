package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/api/protocol"
)

// Shutdown implements the api-protocol "Graceful Shutdown" requirement: a
// seven-step strict-order tear-down. The key invariant the order encodes is
// that cancelled tool_results and `interrupted` events SHALL reach SSE
// clients BEFORE the final `error{server_shutdown}` — and that final error
// SHALL reach them before the connection closes.
//
// Steps:
//
//  1. Mark the server as shutting down so new POST creates refuse with 503.
//  2. Cancel every active session (manager.Shutdown waits for goroutines).
//     Each agent loop synthesises cancelled tool_results, emits
//     `interrupted`, and exits.
//  3. Brief grace window so the SSE writer goroutines pick up the
//     cancelled / interrupted events from Outbox and write them to the wire.
//  4. Emit an `error{server_shutdown}` event on every still-open session's
//     outbox. The SSE writer drains it before exiting (see drainAndFlush).
//  5. Cancel each active stream slot's context so the SSE handlers exit.
//  6. http.Server.Shutdown closes the listener and waits for handlers to
//     return. Since step 5 caused the long-lived SSE handlers to exit, this
//     completes promptly.
//  7. Close the underlying store.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownInFlight.Store(true)

	// Per-step budget: keep the manager drain to a fraction of the overall
	// budget so the SSE flush windows still fit.
	total := s.cfg.GracefulShutdownTimeout
	if total <= 0 {
		total = 30 * time.Second
	}
	sessDrain := total / 3
	if sessDrain < 500*time.Millisecond {
		sessDrain = 500 * time.Millisecond
	}

	// Step 2: cancel all sessions and wait for their goroutines. After this
	// returns, the agent loops have exited and any cancelled/interrupted
	// events have been pushed to each session's Outbox.
	s.manager.Shutdown(sessDrain)

	sessions := s.manager.ListSessions()

	// Step 3: emit server_shutdown on each session's Outbox. Because the
	// agent loops have already enqueued their interrupted events at lower
	// idx, the SSE writer (which respects idx order) will deliver
	// interrupted *before* server_shutdown — the key spec invariant.
	for _, sess := range sessions {
		_ = sess.EmitNow(string(protocol.EventError),
			protocol.NewErrorPayload(protocol.ErrServerShutdown, "", nil))
	}

	// Step 4: poll until every session's Outbox has drained or we hit the
	// SSE-flush budget. This bounds the wait so a slow SSE writer can't
	// stall shutdown, but waits long enough that interrupted +
	// server_shutdown both reach the wire under normal conditions.
	flushBudget := 500 * time.Millisecond
	flushDeadline := time.Now().Add(flushBudget)
	for {
		allEmpty := true
		for _, sess := range sessions {
			if len(sess.Outbox) > 0 {
				allEmpty = false
				break
			}
		}
		if allEmpty || time.Now().After(flushDeadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Step 5: cancel every active stream slot. The SSE handler's
	// drainAndFlush picks up any straggler events (race fallback) before
	// returning.
	s.streamSlotsMu.Lock()
	slots := make([]*streamSlot, 0, len(s.streamSlots))
	for _, slot := range s.streamSlots {
		slots = append(slots, slot)
	}
	s.streamSlotsMu.Unlock()
	for _, slot := range slots {
		slot.cancel()
	}

	// Step 6: shut down the HTTP listener and wait for handlers.
	if s.httpSrv != nil {
		if err := s.httpSrv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Warn("api: http shutdown returned", "err", err)
		}
	}

	// Step 7: close the persistence handle.
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			s.logger.Warn("api: store close", "err", err)
		}
	}

	return nil
}
