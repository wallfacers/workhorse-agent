package api

import (
	"net/http"
)

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
