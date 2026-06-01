package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
)

// presetIDPrefix tags permission rows injected from config preset_rules. It
// must match the prefix used by the serve command when it derives preset IDs.
const presetIDPrefix = "preset-"

// permissionSource reports whether a rule originated from a config preset
// (self-identifying via its ID prefix) or was created manually.
func permissionSource(id string) string {
	if strings.HasPrefix(id, presetIDPrefix) {
		return "preset"
	}
	return "manual"
}

// permissionRuleJSON is the JSON shape for a single rule in the list/create response.
type permissionRuleJSON struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Tool      string    `json:"tool"`
	Pattern   string    `json:"pattern"`
	Decision  string    `json:"decision"`
	Scope     string    `json:"scope"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// createPermissionRequest is the POST /v1/permissions body.
type createPermissionRequest struct {
	Tool     string `json:"tool"`
	Pattern  string `json:"pattern"`
	Decision string `json:"decision"`
}

// handleListPermissions handles GET /v1/permissions.
func (s *Server) handleListPermissions(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListPermissions(r.Context(), "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list permissions: " + err.Error()})
		return
	}

	out := make([]permissionRuleJSON, 0, len(rules))
	for _, p := range rules {
		if p.Scope != store.ScopePermanent {
			continue
		}
		out = append(out, permissionRuleJSON{
			ID:        p.ID,
			SessionID: p.SessionID,
			Tool:      p.Tool,
			Pattern:   p.Pattern,
			Decision:  string(p.Decision),
			Scope:     string(p.Scope),
			Source:    permissionSource(p.ID),
			CreatedAt: p.CreatedAt,
		})
	}
	if out == nil {
		out = []permissionRuleJSON{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

// handleCreatePermission handles POST /v1/permissions.
func (s *Server) handleCreatePermission(w http.ResponseWriter, r *http.Request) {
	var req createPermissionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}

	if req.Decision != "allow_permanent" && req.Decision != "deny_permanent" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid decision: " + req.Decision})
		return
	}

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "generate id: " + err.Error()})
		return
	}
	id := "perm-" + hex.EncodeToString(b)
	now := time.Now().UTC()
	p := &store.Permission{
		ID:        id,
		SessionID: "",
		Tool:      req.Tool,
		Pattern:   req.Pattern,
		Decision:  store.PermissionDecision(req.Decision),
		Scope:     store.ScopePermanent,
		CreatedAt: now,
	}

	if err := s.store.SavePermission(r.Context(), p); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save permission: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, permissionRuleJSON{
		ID:        p.ID,
		SessionID: p.SessionID,
		Tool:      p.Tool,
		Pattern:   p.Pattern,
		Decision:  string(p.Decision),
		Scope:     string(p.Scope),
		Source:    permissionSource(id),
		CreatedAt: p.CreatedAt,
	})
}

// handleDeletePermission handles DELETE /v1/permissions/{id}.
func (s *Server) handleDeletePermission(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeletePermission(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "delete permission: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
