package api

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ProtocolVersion identifies the wire protocol spoken between the frontend
// and the sidecar (session creation, SSE events, client message types,
// tool_use/tool_result correlation). A bump indicates breaking changes.
const ProtocolVersion = "1"

// DefaultCapabilities lists the named features the sidecar advertises via
// GET /health. The frontend checks for specific entries before enabling
// features (e.g. "frontend_tools" before publishing the UI tool catalog).
var DefaultCapabilities = []string{
	"frontend_tools",
	"external_agents",
}

// defaultWorkdir resolves the sidecar's default project directory:
// config override (server.default_workdir) > os.Getwd().
func (s *Server) defaultWorkdir() string {
	if s.cfg.DefaultWorkdir != "" {
		return s.cfg.DefaultWorkdir
	}
	wd, _ := os.Getwd()
	if wd == "" {
		wd = "/"
	}
	return wd
}

// handleHealth answers GET /health. The endpoint is intentionally exempt from
// bearer auth (monitoring probes) and Origin checks (server-side probes).
// Returns protocol_version and capabilities so the frontend can verify
// identity and feature compatibility before attaching.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// A degraded server is reachable (200) but not fully usable; availability is
	// carried by `ok`, not the HTTP status, so monitoring probes still see 200.
	// `reason` is a machine-readable enum, present only when ok is false.
	degraded := s.cfg.DegradedReason
	resp := map[string]any{
		"ok":               degraded == "",
		"version":          s.cfg.Version,
		"protocol_version": ProtocolVersion,
		"capabilities":     DefaultCapabilities,
		"uptime_sec":       int(time.Since(s.startedAt).Seconds()),
		"sessions_active":  s.manager.CountActive(),
		"default_workdir":  s.defaultWorkdir(),
		"platform":         runtime.GOOS,
	}
	if degraded != "" {
		resp["reason"] = degraded
	}

	if distro := getDistro(); distro != "" {
		resp["distro"] = distro
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleDebugEvents serves GET /debug/sessions/{id}/events?since=N as a
// streaming NDJSON dump (one event JSON per line). Gated by debug.enabled
// AND bearer auth (the middleware enforces auth; the handler enforces
// debug.enabled).
func (s *Server) handleDebugEvents(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DebugEnabled {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "not_found",
			"message": "debug endpoints are disabled",
		})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "not_found",
			"message": "persistent store not configured",
		})
		return
	}
	id := r.PathValue("id")
	if _, err := s.manager.GetSession(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	since := int64(0)
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			since = n
		}
	}
	events, err := s.store.EventsAfter(r.Context(), id, since, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code":    "internal",
			"message": err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, ev := range events {
		// Build the same envelope shape live events have so debug parity is
		// preserved.
		var payload map[string]any
		if len(ev.PayloadJSON) > 0 {
			_ = json.Unmarshal([]byte(ev.PayloadJSON), &payload)
		}
		if payload == nil {
			payload = map[string]any{}
		}
		payload["type"] = ev.Type
		payload["idx"] = ev.Idx
		payload["session_id"] = ev.SessionID
		line, _ := json.Marshal(payload)
		if _, err := w.Write(append(line, '\n')); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// getDistro returns the WSL distribution identifier when running under WSL, or
// "" otherwise. See distroName for why WSL_DISTRO_NAME is preferred.
// (add-wsl-remote D-WSL-8 / A4)
func getDistro() string {
	if !isWSL() {
		return ""
	}
	return distroName(os.Getenv("WSL_DISTRO_NAME"))
}

// distroName picks the WSL *registration* name — what `wsl.exe -d <distro>`
// requires. WSL sets WSL_DISTRO_NAME to the registration name (as listed by
// `wsl -l`, e.g. "Ubuntu") inside every distro, so it is preferred. The
// /etc/os-release PRETTY_NAME fallback (e.g. "Ubuntu 24.04.3 LTS") is NOT a
// valid `-d` argument and would yield WSL_E_DISTRO_NOT_FOUND on the host; it is
// used only when the env var is unset.
func distroName(envName string) string {
	if name := strings.TrimSpace(envName); name != "" {
		return name
	}
	return parseOSRelease()
}

// parseOSRelease reads /etc/os-release and returns PRETTY_NAME (or NAME).
func parseOSRelease() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	var name string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(line[len("PRETTY_NAME="):], `"`)
		}
		if strings.HasPrefix(line, "NAME=") && name == "" {
			name = strings.Trim(line[len("NAME="):], `"`)
		}
	}
	return name
}
