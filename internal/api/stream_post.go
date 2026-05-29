package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/api/protocol"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

// handleStreamPost implements POST /v1/sessions/{id}/stream per the
// api-protocol Requirement: "POST 与会话状态冲突" and "中断到达时清空 SSE
// 积压". On accepting a message, the handler returns 202 with no body; on
// conflict it returns 409 AND emits a mirror `error` event to the SSE stream
// so SSE-only clients see the same outcome.
func (s *Server) handleStreamPost(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	id := r.PathValue("id")
	sess, err := s.manager.GetSession(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			writeRequestTooLarge(w, s.cfg.MaxRequestBodyBytes)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}

	msg, err := protocol.DecodeClientMessage(body)
	if err != nil {
		if errors.Is(err, protocol.ErrUnknownClientType) {
			details := map[string]any{}
			if msg.Type != "" {
				details["received_type"] = string(msg.Type)
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"code":    string(protocol.ErrUnknownMessageType),
				"message": protocol.ErrUnknownMessageType.Message(),
				"details": details,
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}

	// Enforce the history-token hard cap (agent.max_history_tokens) on
	// user_message before any state check — even an Idle session has to
	// refuse new user input once we'd blow past the limit, otherwise the
	// next LLM call would crash with context_length_exceeded anyway.
	if session.ClientMessageType(msg.Type) == session.ClientUserMessage && s.cfg.MaxHistoryTokens > 0 {
		cur := agent.EstimateTokens(sess.History())
		if cur >= s.cfg.MaxHistoryTokens {
			writeJSON(w, http.StatusConflict, map[string]any{
				"code":    string(protocol.ErrHistoryTokenLimit),
				"message": protocol.ErrHistoryTokenLimit.Message(),
				"details": map[string]any{
					"limit":   s.cfg.MaxHistoryTokens,
					"current": cur,
				},
			})
			_ = sess.EmitNow(string(protocol.EventError),
				protocol.NewErrorPayload(protocol.ErrHistoryTokenLimit, "",
					map[string]any{"limit": s.cfg.MaxHistoryTokens, "current": cur}))
			return
		}
	}

	state := sess.State()
	if !stateAccepts(state, msg.Type) {
		// 409 + SSE mirror per spec.
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":    string(protocol.ErrSessionBusy),
			"message": "session is in state " + string(state),
			"state":   string(state),
		})
		_ = sess.EmitNow(string(protocol.EventError),
			protocol.NewErrorPayload(protocol.ErrSessionBusy,
				"session is in state "+string(state),
				map[string]any{"state": string(state)}))
		return
	}

	// Cancelled is a transient drain state. An interrupt during Cancelled is
	// idempotent — return 202 without re-pushing.
	if state == session.StateCancelled && session.ClientMessageType(msg.Type) == session.ClientInterrupt {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Route the message to the agent loop. Interrupt has extra steps; the
	// others just enqueue.
	switch session.ClientMessageType(msg.Type) {
	case session.ClientInterrupt:
		s.handleInboxInterrupt(sess)
	case session.ClientPermissionDecision:
		// Decode and forward to PermissionAnswers; the agent loop's blocked
		// Check() call reads it.
		var p protocol.PermissionDecisionPayload
		if err := decodePayload(msg, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"code":    "bad_request",
				"message": "invalid permission_decision payload: " + err.Error(),
			})
			return
		}
		// The PermissionAnswers channel is buffered (4) so this typically does
		// not block; if it does (multiple stacked decisions), we fall through
		// to the default and reply 409.
		select {
		case sess.PermissionAnswers <- session.PermissionDecisionPayload{
			RequestID: p.RequestID,
			Decision:  store.PermissionDecision(p.Decision),
		}:
		default:
			writeJSON(w, http.StatusConflict, map[string]any{
				"code":    string(protocol.ErrSessionBusy),
				"message": "permission_decision queue full",
				"state":   string(state),
			})
			return
		}
	case session.ClientFrontendToolResult:
		var p protocol.FrontendToolResultPayload
		if err := decodePayload(msg, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"code":    "bad_request",
				"message": "invalid frontend_tool_result payload: " + err.Error(),
			})
			return
		}
		if fb := sess.Frontend(); fb != nil {
			fb.Resolve(p.ToolUseID, p.Result)
		}
	default:
		// user_message, ping, context_update, publish_frontend_tools — forward to Inbox.
		select {
		case sess.Inbox <- session.ClientMessage{
			Type:    session.ClientMessageType(msg.Type),
			Payload: msg.Payload,
		}:
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"code":    "inbox_full",
				"message": "session inbox is saturated; retry",
			})
			return
		}
	}

	w.WriteHeader(http.StatusAccepted)
}

// stateAccepts implements the spec's POST/state conflict table.
func stateAccepts(state session.State, t protocol.ClientMessageType) bool {
	mt := session.ClientMessageType(t)
	switch state {
	case session.StateIdle:
		if mt == session.ClientFrontendToolResult {
			return false
		}
		return true
	case session.StateThinking, session.StateExecuting, session.StateCompacting:
		allowed := mt == session.ClientInterrupt || mt == session.ClientPing
		if state == session.StateExecuting {
			allowed = allowed || mt == session.ClientFrontendToolResult
		}
		return allowed
	case session.StateAwaitPerm:
		return mt == session.ClientInterrupt || mt == session.ClientPing ||
			mt == session.ClientPermissionDecision
	case session.StateCancelled:
		return mt == session.ClientInterrupt || mt == session.ClientPing
	}
	return false
}

// handleInboxInterrupt routes a POST interrupt: drain any backlog from the
// outbox (so SSE clients stop seeing the soon-to-be-cancelled turn's leftover
// events) then push the interrupt to the inbox. The events table retains
// every drained event — clients can pull them back via Last-Event-ID per
// spec "中断后 Last-Event-ID 重连能拉回被丢的事件".
func (s *Server) handleInboxInterrupt(sess *session.Session) {
	// Drain outbox in a tight non-blocking loop.
	for {
		select {
		case <-sess.Outbox:
		default:
			goto enqueue
		}
	}
enqueue:
	// Push the interrupt into the inbox. Best-effort: if the inbox is full
	// the turn has bigger problems than a missed interrupt.
	select {
	case sess.Inbox <- session.ClientMessage{Type: session.ClientInterrupt}:
	default:
	}
}

// decodePayload extracts type-specific fields from the raw payload. The
// protocol-package payloads are JSON-tagged so json.Unmarshal handles them.
func decodePayload(msg protocol.ClientMessage, into any) error {
	if len(msg.Payload) == 0 {
		return errors.New("empty payload")
	}
	return json.Unmarshal(msg.Payload, into)
}
