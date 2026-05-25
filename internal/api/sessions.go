package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/wallfacers/workhorse-agent/internal/api/protocol"
	"github.com/wallfacers/workhorse-agent/internal/session"
)

// createSessionRequest is the POST /v1/sessions body shape.
type createSessionRequest struct {
	Workdir      string            `json:"workdir"`
	Env          map[string]string `json:"env,omitempty"`
	Provider     string            `json:"provider"`
	Model        string            `json:"model"`
	AgentType    string            `json:"agent_type,omitempty"`
	Ephemeral    bool              `json:"ephemeral,omitempty"`
	AllowedTools []string          `json:"allowed_tools,omitempty"`
	ParentID     string            `json:"parent_id,omitempty"`
}

// sessionView is the JSON projection returned for both POST and GET.
type sessionView struct {
	ID        string            `json:"id"`
	ParentID  string            `json:"parent_id,omitempty"`
	Status    string            `json:"status"`
	Workdir   string            `json:"workdir"`
	Env       map[string]string `json:"env,omitempty"`
	Provider  string            `json:"provider"`
	Model     string            `json:"model"`
	AgentType string            `json:"agent_type,omitempty"`
	Ephemeral bool              `json:"ephemeral"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

func viewOf(s *session.Session) sessionView {
	return sessionView{
		ID:        s.ID,
		ParentID:  s.ParentID,
		Status:    string(s.State()),
		Workdir:   s.Workdir,
		Env:       s.Env,
		Provider:  s.ProviderName,
		Model:     s.Model,
		AgentType: s.AgentType,
		Ephemeral: s.Ephemeral,
		CreatedAt: s.CreatedAt.Format("2006-01-02T15:04:05.000Z"),
		UpdatedAt: s.UpdatedAt().Format("2006-01-02T15:04:05.000Z"),
	}
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if s.shutdownInFlight.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"code":    string(protocol.ErrServerShutdown),
			"message": "server is shutting down",
		})
		return
	}
	if !requireJSONBody(w, r) {
		return
	}
	defer r.Body.Close()
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
	var req createSessionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "bad_request",
			"message": "invalid JSON body: " + err.Error(),
		})
		return
	}

	// Enforce max_concurrent at the API layer too — the manager checks but
	// returning 429 with a clear shape is the spec's contract for this end.
	if s.cfg.MaxConcurrentSessions > 0 && s.manager.CountActive() >= s.cfg.MaxConcurrentSessions {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"code":    "too_many_sessions",
			"message": "active session count is at sessions.max_concurrent",
			"limit":   s.cfg.MaxConcurrentSessions,
		})
		return
	}

	sess, err := s.manager.CreateSession(r.Context(), session.Options{
		Workdir:      req.Workdir,
		Env:          req.Env,
		Ephemeral:    req.Ephemeral,
		Model:        req.Model,
		ProviderName: req.Provider,
		AgentType:    req.AgentType,
		ParentID:     req.ParentID,
		AllowedTools: req.AllowedTools,
	})
	if err != nil {
		if errors.Is(err, session.ErrTooManyConcurrent) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"code":    "too_many_sessions",
				"message": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code":    "internal",
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusCreated, viewOf(sess))
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	all := s.manager.ListSessions()
	out := make([]sessionView, 0, len(all))
	for _, sess := range all {
		out = append(out, viewOf(sess))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.manager.GetSession(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	writeJSON(w, http.StatusOK, viewOf(sess))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.manager.DeleteSession(r.Context(), id, s.cfg.GracefulShutdownTimeout)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"code":    "session_not_found",
				"message": "no such session",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code":    "internal",
			"message": err.Error(),
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCancelSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.manager.Cancel(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCompactSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.manager.GetSession(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	if state := sess.State(); state != session.StateIdle {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":    string(protocol.ErrSessionBusy),
			"message": "compact requires session in Idle",
			"state":   string(state),
		})
		// Mirror to SSE so SSE-only clients see the same outcome (spec
		// requires double-emit for POST/state conflicts).
		_ = sess.EmitNow(string(protocol.EventError),
			protocol.NewErrorPayload(protocol.ErrSessionBusy,
				"compact requires session in Idle",
				map[string]any{"state": string(state)}))
		return
	}
	if err := s.manager.RequestCompact(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// requireJSONBody enforces Content-Type: application/json on POST. Returns
// false (and writes 415) if absent. POSTs without bodies (cancel/compact)
// skip this; they call directly.
func requireJSONBody(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
			"code":    "unsupported_media_type",
			"message": "missing Content-Type",
		})
		return false
	}
	// Allow `application/json; charset=utf-8` and the like.
	if !isJSONContentType(ct) {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
			"code":    "unsupported_media_type",
			"message": "Content-Type must be application/json",
		})
		return false
	}
	return true
}

func isJSONContentType(ct string) bool {
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	for i := 0; i < len(ct); i++ {
		if ct[i] == ' ' || ct[i] == '\t' {
			continue
		}
		ct = ct[i:]
		break
	}
	// Trim trailing whitespace.
	for len(ct) > 0 && (ct[len(ct)-1] == ' ' || ct[len(ct)-1] == '\t') {
		ct = ct[:len(ct)-1]
	}
	return ct == "application/json"
}
