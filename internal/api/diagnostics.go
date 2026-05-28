package api

import (
	"net/http"
	"sync"

	"github.com/wallfacers/workhorse-agent/internal/extagent/regen"
)

// driftSource is the optional accessor the server calls to populate the
// `external_agents.drift` field of /v1/diagnostics. Set via
// SetDriftSnapshot at startup; nil means the field is omitted.
type driftSnapshot struct {
	mu      sync.RWMutex
	entries []regen.Entry
}

func (d *driftSnapshot) get() []regen.Entry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]regen.Entry, len(d.entries))
	copy(out, d.entries)
	return out
}

func (d *driftSnapshot) set(in []regen.Entry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries[:0], in...)
}

// SetDriftSnapshot replaces the slice surfaced via /v1/diagnostics. Safe to
// call concurrently with handler reads.
func (s *Server) SetDriftSnapshot(entries []regen.Entry) {
	if s.drift == nil {
		s.drift = &driftSnapshot{}
	}
	s.drift.set(entries)
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"version":  s.cfg.Version,
		"uptime_s": int(timeNow().Sub(s.startedAt).Seconds()),
	}
	if s.drift != nil {
		payload["external_agents"] = map[string]any{
			"drift": s.drift.get(),
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// timeNow is a function var so tests can stub time.Now without monkey-
// patching the standard library.
var timeNow = realNow
