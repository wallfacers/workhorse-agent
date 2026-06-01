package api

import (
	"errors"
	"net/http"

	"github.com/wallfacers/workhorse-agent/internal/session"
)

// writeSessionLookupError maps a Manager.GetOrHydrate error to a response:
// ErrNotFound (unknown or soft-deleted session) → 404; anything else (a store
// read failure during hydration) → 500.
func writeSessionLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, session.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"code":    "internal",
		"message": "session lookup failed",
	})
}

// handleStream is the entry point for both GET and POST on
// /v1/sessions/{id}/stream. Method-specific dispatch enforces the spec's
// "405 + Allow: GET, POST" rule for non-stream verbs.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleStreamGet(w, r)
	case http.MethodPost:
		s.handleStreamPost(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"code":    "method_not_allowed",
			"message": "use GET to subscribe or POST to send a client message",
		})
	}
}
