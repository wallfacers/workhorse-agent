package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/extagent/approval"
	"github.com/wallfacers/workhorse-agent/internal/session"
)

// SetApprovalManager wires the singleton adapter-generation approval manager
// into the server so the approvals HTTP handler can read state and dispatch
// decisions. Called by cmd/serve once at startup.
func (s *Server) SetApprovalManager(m *approval.Manager) {
	s.approvals = m
}

type approvalDecisionBody struct {
	Decision   string `json:"decision"`
	EditedYAML string `json:"edited_yaml,omitempty"`
}

// handleAdapterApproval processes POST /v1/sessions/{id}/approvals/{aid}.
// Returns 200 with {"status": "<resolved-status>"} on success, 404 if the
// approval has expired/disappeared, 400 on invalid body, 422 on a rejected
// edit (validation/smoke failure during re-validation).
func (s *Server) handleAdapterApproval(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"code":    "approval_manager_unavailable",
			"message": "adapter approval manager not configured",
		})
		return
	}
	sessionID := r.PathValue("id")
	approvalID := r.PathValue("aid")
	if sessionID == "" || approvalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "invalid_path",
			"message": "session_id and approval_id are required",
		})
		return
	}
	// Confirm the session exists — a request against a deleted session must
	// not silently succeed if a stale approval still sits in the manager.
	if _, err := s.manager.GetSession(sessionID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": err.Error(),
		})
		return
	}
	// Verify the approval id actually belongs to the session id in the URL —
	// otherwise an authenticated caller could approve/reject another
	// session's pending approval by guessing the id.
	if pending := s.approvals.Get(approvalID); pending == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "approval_not_found",
			"message": "approval expired or already resolved",
		})
		return
	} else if pending.SessionID != sessionID {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "approval_not_found",
			"message": "approval does not belong to this session",
		})
		return
	}
	var body approvalDecisionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "invalid_body",
			"message": err.Error(),
		})
		return
	}
	decision := approval.Decision(strings.ToLower(strings.TrimSpace(body.Decision)))
	switch decision {
	case approval.DecisionApprove, approval.DecisionReject, approval.DecisionEdit:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "invalid_decision",
			"message": "decision must be one of approve | reject | edit",
		})
		return
	}
	if err := s.approvals.Decide(approvalID, decision, body.EditedYAML); err != nil {
		if errors.Is(err, approval.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"code":    "approval_not_found",
				"message": "approval expired or already resolved",
			})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"code":    "decision_failed",
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": string(decision)})
}

// sessionEventEmitter implements approval.EventEmitter by pushing events
// into the named session's Outbox so the live SSE stream surfaces them.
// Constructed once by cmd/serve and stored on the Server.
type sessionEventEmitter struct {
	manager *session.Manager
}

// NewSessionEventEmitter returns an emitter ready to be passed to
// approval.New as the Emitter field.
func NewSessionEventEmitter(m *session.Manager) approval.EventEmitter {
	return &sessionEventEmitter{manager: m}
}

func (e *sessionEventEmitter) EmitApprovalEvent(sessionID, eventType string, payload any) {
	if e == nil || e.manager == nil || sessionID == "" {
		return
	}
	sess, err := e.manager.GetSession(sessionID)
	if err != nil {
		return
	}
	flat, ok := normalizeApprovalPayload(payload)
	if !ok {
		return
	}
	sess.EmitNow(eventType, flat)
}

// normalizeApprovalPayload accepts the manager's any-typed payload and
// converts it into the map[string]any shape session.Event expects. We round-
// trip through JSON so we don't depend on the manager's internal struct
// layouts — anything the manager can json.Marshal, we can carry.
func normalizeApprovalPayload(payload any) (map[string]any, bool) {
	switch v := payload.(type) {
	case map[string]any:
		return v, true
	default:
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, false
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false
		}
		return out, true
	}
}
