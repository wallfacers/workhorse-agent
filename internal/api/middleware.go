package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/api/protocol"
)

// statusRecorder lets logging middleware see the status the handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped writer's Flush, when implemented. Required so
// the SSE handler can call http.Flusher even when wrapped by middleware.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recoveryMW catches panics from downstream handlers, logs the stack, and
// responds 500. It does NOT re-panic — keeps the server alive.
func recoveryMW(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					logger.Error("api panic",
						"method", r.Method,
						"path", r.URL.Path,
						"panic", fmt.Sprintf("%v", rv),
						"stack", string(debug.Stack()))
					writeJSON(w, http.StatusInternalServerError, map[string]any{
						"code":    string(protocol.ErrInternalPanic),
						"message": "server panic recovered",
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// loggingMW logs one structured line per request. The token field is NEVER
// logged — bearer values must not leak.
func loggingMW(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"remote", r.RemoteAddr,
				"dur_ms", time.Since(start).Milliseconds())
		})
	}
}

// OriginConfig is the per-server origin whitelist policy. ServerBoundLocal is
// true iff config.server.host resolves to a loopback (127.0.0.1 / ::1 /
// localhost) — the only case where missing Origin is permitted. The exact
// host check is documented in api-protocol/spec.md "Origin 校验".
type OriginConfig struct {
	Allowed          []string
	AllowNullOrigin  bool
	ServerBoundLocal bool
}

// originMW enforces exact-host Origin matching. Applies only to /v1/* and
// /debug/* routes. /health and /ui are exempt (monitoring + static).
func originMW(cfg OriginConfig) func(http.Handler) http.Handler {
	allowSet := map[string]struct{}{}
	for _, o := range cfg.Allowed {
		allowSet[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !originPathScope(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if origin == "" {
				if cfg.ServerBoundLocal {
					next.ServeHTTP(w, r)
					return
				}
				writeJSON(w, http.StatusForbidden, map[string]any{
					"code":    "origin_forbidden",
					"message": "missing Origin header on non-loopback bind",
				})
				return
			}
			if origin == "null" {
				if cfg.AllowNullOrigin {
					next.ServeHTTP(w, r)
					return
				}
				writeJSON(w, http.StatusForbidden, map[string]any{
					"code":    "origin_forbidden",
					"message": "null Origin is not allowed",
				})
				return
			}
			if originAllowed(origin, allowSet) {
				next.ServeHTTP(w, r)
				return
			}
			writeJSON(w, http.StatusForbidden, map[string]any{
				"code":    "origin_forbidden",
				"message": "origin not in allow-list",
			})
		})
	}
}

func originPathScope(path string) bool {
	return strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/debug/")
}

// originAllowed parses origin via net/url and exact-matches scheme+hostname+port
// against the spec's built-in localhost rules plus any custom entries.
func originAllowed(origin string, custom map[string]struct{}) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	port := u.Port()
	// Built-in localhost allow: 127.0.0.1 / localhost / ::1 with any port.
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	_ = port
	// Custom whitelist must match the *full normalized origin* — the spec's
	// "exact host" rule is implemented by requiring an exact string match on
	// scheme://host[:port].
	norm := u.Scheme + "://" + host
	if port != "" {
		norm = norm + ":" + port
	}
	if _, ok := custom[norm]; ok {
		return true
	}
	return false
}

// BearerConfig describes the optional bearer-token authentication policy.
type BearerConfig struct {
	Enabled bool
	Token   string
}

// bearerMW enforces Authorization: Bearer <token> on protected routes.
// /health and /ui are exempt; everything else under /v1/* and /debug/* is
// gated when Enabled.
func bearerMW(cfg BearerConfig) func(http.Handler) http.Handler {
	tokenBytes := []byte(cfg.Token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Enabled {
				next.ServeHTTP(w, r)
				return
			}
			if !authPathScope(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			auth := r.Header.Get("Authorization")
			if auth == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"code": "auth_required"})
				return
			}
			if !strings.HasPrefix(auth, "Bearer ") {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"code": "invalid_token"})
				return
			}
			got := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), tokenBytes) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"code": "invalid_token"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func authPathScope(path string) bool {
	switch {
	case path == "/health", strings.HasPrefix(path, "/ui"):
		return false
	}
	return true
}

// maxBytesMW wraps r.Body in http.MaxBytesReader so handlers reading the body
// see a clean io.EOF (or io.ErrUnexpectedEOF) at the limit. Body size errors
// surface as 413 only when the handler reads them — we don't pre-read here.
// Only applies to bodied methods.
func maxBytesMW(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch:
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeRequestTooLarge writes the 413 response prescribed by spec when a
// handler's body read errors out as MaxBytesReader overflow.
func writeRequestTooLarge(w http.ResponseWriter, limit int64) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
		"code":    string(protocol.ErrRequestTooLarge),
		"limit":   limit,
		"message": protocol.ErrRequestTooLarge.Message(),
	})
}

// isMaxBytesError reports whether err came from http.MaxBytesReader's limit.
func isMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	// Older Go fall-through: MaxBytesReader returned an *errors.errorString
	// with a fixed message before Go 1.19.
	return strings.Contains(err.Error(), "request body too large")
}

// chain composes middlewares in the order given: chain(m1, m2)(h) is
// equivalent to m1(m2(h)). This matches the conventional reading order:
// outermost first.
func chain(mws ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

// writeJSON is the canonical JSON response helper. It sets Content-Type and
// writes the marshalled body in one shot so middleware can't sneak headers in
// after WriteHeader.
func writeJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "json marshal failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
