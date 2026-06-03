package api

import (
	"encoding/json"
	"net/http"

	"github.com/wallfacers/workhorse-agent/internal/config"
)

// presetRuleJSON is the wire shape of a single preset rule (snake_case), since
// config.PresetRule only carries yaml tags.
type presetRuleJSON struct {
	Tool     string `json:"tool"`
	Pattern  string `json:"pattern"`
	Decision string `json:"decision"`
}

// permissionConfigJSON is the GET/PUT body for the permission subset of
// config.yaml.
type permissionConfigJSON struct {
	DefaultPermission string           `json:"default_permission"`
	PresetRules       []presetRuleJSON `json:"preset_rules"`
}

// validPermanentDecision is the whitelist shared by preset decisions.
func validPermanentDecision(d string) bool {
	return d == "allow_permanent" || d == "deny_permanent"
}

func toPermissionConfigJSON(pc config.PermissionConfig) permissionConfigJSON {
	rules := make([]presetRuleJSON, 0, len(pc.PresetRules))
	for _, r := range pc.PresetRules {
		rules = append(rules, presetRuleJSON{Tool: r.Tool, Pattern: r.Pattern, Decision: r.Decision})
	}
	return permissionConfigJSON{DefaultPermission: pc.DefaultPermission, PresetRules: rules}
}

// handleGetPermissionConfig handles GET /v1/permission-config — reads the
// permission subset straight from config.yaml (the source of truth).
func (s *Server) handleGetPermissionConfig(w http.ResponseWriter, r *http.Request) {
	if s.permConfigPath == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "permission config not available"})
		return
	}
	pc, err := config.ReadPermissionConfig(s.permConfigPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read permission config: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toPermissionConfigJSON(pc))
}

// handlePutPermissionConfig handles PUT /v1/permission-config — writes the
// permission subset back to config.yaml (preserving comments) and triggers an
// in-process reload so the change takes effect immediately.
func (s *Server) handlePutPermissionConfig(w http.ResponseWriter, r *http.Request) {
	if s.permConfigPath == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "permission config not available"})
		return
	}

	var body permissionConfigJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}

	if body.DefaultPermission != "" && !validPermanentDecision(body.DefaultPermission) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid default_permission: " + body.DefaultPermission,
			"valid": []string{"", "allow_permanent", "deny_permanent"},
		})
		return
	}
	for i, rule := range body.PresetRules {
		if !validPermanentDecision(rule.Decision) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid decision in preset_rules: " + rule.Decision,
				"index": i,
				"valid": []string{"allow_permanent", "deny_permanent"},
			})
			return
		}
	}

	pc := config.PermissionConfig{DefaultPermission: body.DefaultPermission}
	for _, rule := range body.PresetRules {
		pc.PresetRules = append(pc.PresetRules, config.PresetRule{
			Tool: rule.Tool, Pattern: rule.Pattern, Decision: rule.Decision,
		})
	}

	if err := config.WritePermissionConfig(s.permConfigPath, pc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "write permission config: " + err.Error()})
		return
	}

	// Apply immediately so the response reflects effective state. The body was
	// already validated, so reload should succeed; a failure is logged but the
	// file write stands (the watcher / next start will reconcile).
	if s.reloadPermConfig != nil {
		if err := s.reloadPermConfig(r.Context()); err != nil {
			s.logger.Warn("permission config: written but reload failed", "error", err)
		}
	}

	writeJSON(w, http.StatusOK, toPermissionConfigJSON(pc))
}
