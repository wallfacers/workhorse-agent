// Package api wires the HTTP surface — REST sessions CRUD, Streamable HTTP
// (POST/GET single endpoint), Origin enforcement, Bearer auth, body-size cap,
// graceful shutdown — over the session.Manager and store.Store. The package
// owns no domain logic; it only translates HTTP into ClientMessages and SSE.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent/approval"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

// Config is the dial-able view of the HTTP server. It mirrors a subset of
// config.ServerConfig + config.AuthConfig + config.DebugConfig so the server
// can be configured without depending on the full Config type at the package
// boundary.
type Config struct {
	Host                    string
	Port                    int
	DefaultWorkdir          string
	ReadHeaderTimeout       time.Duration
	ReadTimeout             time.Duration
	IdleTimeout             time.Duration
	MaxHeaderBytes          int
	MaxRequestBodyBytes     int64
	GracefulShutdownTimeout time.Duration
	SSEKeepalive            time.Duration
	AllowedOrigins          []string
	AllowNullOrigin         bool
	DebugEnabled            bool
	Auth                    BearerConfig
	MaxConcurrentSessions   int
	MaxHistoryTokens        int
	Version                 string

	// DegradedReason is non-empty when the server is reachable but not fully
	// usable (e.g. "no_provider_key" — started without a usable provider key).
	// It surfaces via GET /health as {ok:false, reason:...} and blocks session
	// creation, so a managed launcher can attach and guide the user instead of
	// facing a crash-loop. Empty means fully healthy.
	DegradedReason string
}

// Server is the long-lived HTTP server bound by serve. Construct via
// NewServer; start with Start; cancel with Shutdown.
type Server struct {
	cfg     Config
	manager *session.Manager
	store   store.Store
	logger  *slog.Logger

	startedAt time.Time
	httpSrv   *http.Server

	// streamSlots tracks the active GET SSE stream per session for the single-
	// flow switching protocol (api-protocol "并发 GET 关闭旧流且无事件丢失").
	streamSlotsMu sync.Mutex
	streamSlots   map[string]*streamSlot

	// shutdownInFlight switches to true once Shutdown begins so handlers can
	// refuse new POSTs while still letting SSE writers drain.
	shutdownInFlight atomic.Bool

	// approvals is the adapter-generation approval manager. Wired by
	// cmd/serve via SetApprovalManager so the api package doesn't depend on
	// the cmd-level wiring path. May be nil — the approvals endpoint then
	// responds with 503.
	approvals *approval.Manager

	// drift is the adapter-drift snapshot surfaced via /v1/diagnostics.
	// Populated by SetDriftSnapshot during server start.
	drift *driftSnapshot
}

// NewServer builds a Server but does NOT start it. Caller wires routes via
// Handler() (for tests) or starts the bound HTTP server via Start().
func NewServer(cfg Config, manager *session.Manager, st store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	return &Server{
		cfg:         cfg,
		manager:     manager,
		store:       st,
		logger:      logger,
		startedAt:   time.Now().UTC(),
		streamSlots: map[string]*streamSlot{},
	}
}

// Handler returns the fully-wired http.Handler with all middlewares chained.
// Tests call this directly; Start() wraps it with an http.Server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)

	origin := OriginConfig{
		Allowed:          s.cfg.AllowedOrigins,
		AllowNullOrigin:  s.cfg.AllowNullOrigin,
		ServerBoundLocal: isLoopback(s.cfg.Host),
	}
	return chain(
		recoveryMW(s.logger),
		loggingMW(s.logger),
		originMW(origin),
		bearerMW(s.cfg.Auth),
		maxBytesMW(s.cfg.MaxRequestBodyBytes),
	)(mux)
}

// routes registers all endpoints on the mux. The /v1/sessions/{id}/stream
// endpoint is the only one with method-specific dispatch — the spec's
// "405 + Allow: GET, POST" requirement is enforced inside the handler so we
// can write the exact response shape.
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /v1/sessions/{id}", s.handleRenameSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /v1/sessions/{id}/history", s.handleHistory)
	mux.HandleFunc("POST /v1/sessions/{id}/cancel", s.handleCancelSession)
	mux.HandleFunc("POST /v1/sessions/{id}/compact", s.handleCompactSession)
	mux.HandleFunc("POST /v1/sessions/{id}/approvals/{aid}", s.handleAdapterApproval)
	mux.HandleFunc("/v1/sessions/{id}/stream", s.handleStream)
	mux.HandleFunc("GET /v1/projects", s.handleListProjects)
	mux.HandleFunc("DELETE /v1/projects", s.handleDeleteProject)

	mux.HandleFunc("GET /v1/permissions", s.handleListPermissions)
	mux.HandleFunc("POST /v1/permissions", s.handleCreatePermission)
	mux.HandleFunc("DELETE /v1/permissions/{id}", s.handleDeletePermission)

	mux.HandleFunc("GET /v1/fs/list", s.handleFSList)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/diagnostics", s.handleDiagnostics)
	mux.HandleFunc("GET /debug/sessions/{id}/events", s.handleDebugEvents)

	ui := s.handleUI()
	mux.Handle("GET /ui/", ui)
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
}

// Start binds the server and begins serving in a goroutine. The returned
// channel closes when the server exits (either via Shutdown or an unexpected
// listener error).
func (s *Server) Start() (<-chan error, error) {
	addr := net.JoinHostPort(s.cfg.Host, itoa(s.cfg.Port))
	s.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		MaxHeaderBytes:    s.cfg.MaxHeaderBytes,
		// WriteTimeout is intentionally 0: the SSE handler keeps the connection
		// open indefinitely. Non-SSE handlers manage their own deadlines via
		// http.ResponseController.SetWriteDeadline when needed.
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.httpSrv.Addr = ln.Addr().String()
	exit := make(chan error, 1)
	go func() {
		serveErr := s.httpSrv.Serve(ln)
		if errors.Is(serveErr, http.ErrServerClosed) {
			exit <- nil
		} else {
			exit <- serveErr
		}
		close(exit)
	}()
	s.logger.Info("api: listening", "addr", s.httpSrv.Addr)
	return exit, nil
}

// BoundAddr returns the actual address the listener was bound to. Useful for
// tests that pass port 0.
func (s *Server) BoundAddr() string {
	if s.httpSrv == nil {
		return ""
	}
	return s.httpSrv.Addr
}

// itoa is a tiny strconv.Itoa wrapper that keeps the import surface tight.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func isLoopback(host string) bool {
	if host == "" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// streamSlot tracks a single active GET SSE writer. cancel terminates the
// existing handler so a new GET can take over (single-flow switching). The
// superseded flag tells the exiting handler to emit `: superseded` before
// closing — set by the incoming handler under streamSlotsMu.
type streamSlot struct {
	cancel     context.CancelFunc
	done       chan struct{}
	superseded atomic.Bool
}

// Manager exposes the underlying session manager for tests / wire-up code.
func (s *Server) Manager() *session.Manager { return s.manager }

// Store exposes the persistence handle for tests / debug endpoint internals.
func (s *Server) Store() store.Store { return s.store }
