package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

// handleStreamGet implements GET /v1/sessions/{id}/stream per the
// api-protocol "Streamable HTTP 传输" and "断线重连按 Last-Event-ID 增量同步"
// requirements. The handler:
//  1. validates the session and Accept header,
//  2. claims the single active GET slot for this session (signalling any
//     prior handler to write `: superseded` and exit),
//  3. optionally replays events strictly newer than Last-Event-ID up to a
//     snapshot of the current MAX(idx),
//  4. switches to live mode draining Session.Outbox until the client closes
//     or a newer GET supersedes us.
//
// Single-writer discipline: this goroutine is the only writer to the
// http.ResponseWriter for the lifetime of the connection.
func (s *Server) handleStreamGet(w http.ResponseWriter, r *http.Request) {
	if !acceptsEventStream(r) {
		writeJSON(w, http.StatusNotAcceptable, map[string]any{
			"code":    "not_acceptable",
			"message": "client must Accept text/event-stream",
		})
		return
	}
	id := r.PathValue("id")
	sess, err := s.manager.GetOrHydrate(r.Context(), id)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Shouldn't happen with stock net/http, but guard anyway.
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	lastEventID, hasLast := parseLastEventID(r)

	// Claim the slot. claim() returns a context that gets cancelled either
	// when the client disconnects (via r.Context()) or when a newer GET
	// supersedes us. claim() also writes any handover bookkeeping in
	// streamSlots atomically.
	streamCtx, slot := s.claimStreamSlot(r.Context(), sess)
	defer s.releaseStreamSlot(sess.ID, slot)

	// Write SSE headers and flush so the client sees the connection upgrade
	// immediately even before any event.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var lastSentIdx int64

	// Replay phase. The session's store may be nil for ephemeral sessions;
	// in that case Last-Event-ID is silently ignored (the events table was
	// never populated).
	if hasLast && s.store != nil && !sess.Ephemeral {
		var snapshot int64
		snapshot, err = s.store.MaxEventIdx(streamCtx, sess.ID)
		if err != nil {
			s.logger.Warn("api: snapshot idx failed", "err", err, "session", sess.ID)
		} else if snapshot > lastEventID {
			events, e := s.store.EventsAfter(streamCtx, sess.ID, lastEventID, snapshot)
			if e != nil {
				s.logger.Warn("api: replay query failed", "err", e, "session", sess.ID)
			} else {
				for _, ev := range events {
					if streamCtx.Err() != nil {
						return
					}
					if writeErr := writeStoreEvent(w, flusher, ev); writeErr != nil {
						return
					}
					lastSentIdx = ev.Idx
				}
			}
		}
	}

	// Live phase: drain Outbox until ctx.Done. The keepalive ticker writes
	// SSE comments so proxies don't sever the connection. lastSentIdx is the
	// dedup guard against a tiny race where an event landed in both events
	// table (during replay) and Outbox (concurrent live emit).
	keepalive := s.cfg.SSEKeepalive
	if keepalive <= 0 {
		keepalive = 25 * time.Second
	}
	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()

	for {
		select {
		case <-streamCtx.Done():
			// Either client disconnected or shutdown/supersede. Drain any
			// pending Outbox entries non-blocking so the final shutdown
			// error (or any racing event) still reaches the wire before TCP
			// close. The drain is bounded by the channel capacity.
			s.drainAndFlush(w, flusher, sess, lastSentIdx)
			if slot.superseded.Load() {
				_, _ = io.WriteString(w, ": superseded\n\n")
				flusher.Flush()
			}
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-sess.Outbox:
			if !ok {
				return
			}
			if ev.Idx <= lastSentIdx {
				continue
			}
			if err := writeSessionEvent(w, flusher, ev); err != nil {
				return
			}
			lastSentIdx = ev.Idx
		}
	}
}

// drainAndFlush consumes any events queued in Outbox right now and writes
// them to the SSE wire. Called on stream exit so a server-side shutdown can
// still deliver the final `error{server_shutdown}` even when ctx.Done won
// the select race against the Outbox read.
func (s *Server) drainAndFlush(w http.ResponseWriter, flusher http.Flusher, sess *session.Session, lastSentIdx int64) {
	for {
		select {
		case ev, ok := <-sess.Outbox:
			if !ok {
				return
			}
			if ev.Idx <= lastSentIdx {
				continue
			}
			if err := writeSessionEvent(w, flusher, ev); err != nil {
				return
			}
			lastSentIdx = ev.Idx
		default:
			return
		}
	}
}

// acceptsEventStream returns true if Accept absent, contains */*, or
// text/event-stream. Per spec "Accept header 必须包含 text/event-stream（或
// `*/*`、缺失视为允许）".
func acceptsEventStream(r *http.Request) bool {
	a := r.Header.Get("Accept")
	if a == "" {
		return true
	}
	for _, part := range strings.Split(a, ",") {
		part = strings.TrimSpace(part)
		if i := strings.Index(part, ";"); i >= 0 {
			part = part[:i]
		}
		part = strings.TrimSpace(part)
		if part == "*/*" || part == "text/event-stream" || part == "text/*" {
			return true
		}
	}
	return false
}

// parseLastEventID extracts the resumption point from the spec-defined two
// sources: the standard SSE `Last-Event-ID` header (preferred) or the
// `?last_event_id=N` query parameter (curl fallback). Returns 0 + false if
// neither is present or the value is unparsable.
func parseLastEventID(r *http.Request) (int64, bool) {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
			return n, true
		}
	}
	if v := r.URL.Query().Get("last_event_id"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
			return n, true
		}
	}
	return 0, false
}

// claimStreamSlot performs the spec's atomic single-flow switching protocol.
// If an older slot exists for this session, it is signalled and waited on
// (bounded) before the new slot replaces it. The returned context is the
// derived stream-lifetime context; it is cancelled when either the parent
// (client disconnect) or the slot.cancel (server-side supersede) fires.
func (s *Server) claimStreamSlot(parent context.Context, sess *session.Session) (context.Context, *streamSlot) {
	streamCtx, cancel := context.WithCancel(parent)
	slot := &streamSlot{
		cancel: cancel,
		done:   make(chan struct{}),
	}

	s.streamSlotsMu.Lock()
	prev := s.streamSlots[sess.ID]
	s.streamSlots[sess.ID] = slot
	s.streamSlotsMu.Unlock()

	if prev != nil {
		prev.superseded.Store(true)
		prev.cancel()
		// Wait for old handler to finish writing its `: superseded` frame
		// and return. Bound by 1 second so a wedged old handler can't block
		// us indefinitely; in practice the old handler exits in microseconds
		// once cancel fires.
		select {
		case <-prev.done:
		case <-time.After(1 * time.Second):
			s.logger.Warn("api: superseded GET stream took >1s to exit", "session", sess.ID)
		}
	}

	return streamCtx, slot
}

// releaseStreamSlot is the cleanup hook. It only removes the slot from the
// map if our slot is still the registered one — if a newer GET already took
// over, that newer slot's entry must remain.
func (s *Server) releaseStreamSlot(sessionID string, slot *streamSlot) {
	close(slot.done)
	s.streamSlotsMu.Lock()
	if s.streamSlots[sessionID] == slot {
		delete(s.streamSlots, sessionID)
	}
	s.streamSlotsMu.Unlock()
}

// writeSessionEvent serialises a live session.Event into an SSE frame. Errors
// (typically client close) propagate up so the handler exits.
func writeSessionEvent(w http.ResponseWriter, flusher http.Flusher, ev session.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Idx, ev.Type, body); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeStoreEvent serialises a replayed store.Event into an SSE frame. The
// payload_json column already holds the exact bytes we want as data (the
// per-event fields except type/idx/session_id), so we splice them with the
// envelope keys at write time.
func writeStoreEvent(w http.ResponseWriter, flusher http.Flusher, ev *store.Event) error {
	// Reconstruct the same wrapper that Event.MarshalJSON produces for live
	// events so the wire shape is identical whether the data came from
	// replay or live.
	var payload map[string]any
	if len(ev.PayloadJSON) > 0 {
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("replay payload: %w", err)
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = ev.Type
	payload["idx"] = ev.Idx
	payload["session_id"] = ev.SessionID
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal replay: %w", err)
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Idx, ev.Type, body); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// atomic.Bool wrapper for the streamSlot.superseded flag — declared as a
// method-pointer recipient to avoid pulling sync/atomic into every consumer.
var _ = atomic.Bool{}
