package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/api"
	"github.com/wallfacers/workhorse-agent/internal/config"
	"github.com/wallfacers/workhorse-agent/internal/coord"
	"github.com/wallfacers/workhorse-agent/internal/idgen"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/provider/anthropic"
	"github.com/wallfacers/workhorse-agent/internal/provider/openai"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/skills"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/bash"
	"github.com/wallfacers/workhorse-agent/internal/tools/builtin"
	"github.com/wallfacers/workhorse-agent/internal/tools/dispatch"
)

// runServe boots the HTTP listener and the agent runtime. The wiring follows
// the api-protocol "Graceful Shutdown" requirement on the exit path: SIGTERM
// → http drain → session cancel → SSE flush → final error event → exit.
func runServe(args []string, stdout, stderr io.Writer) error {
	configPath := extractConfigPath(args)
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("serve: locate home directory: %w", err)
		}
		configPath = filepath.Join(home, ".workhorse-agent", "config.yaml")
	}

	cfg, err := config.Load(config.LoadOptions{
		YAMLPath:         configPath,
		Args:             args,
		ResolveHomePaths: true,
	})
	if err != nil {
		return err
	}

	logger := buildLogger(cfg.Logging, stderr)
	logger.Info("workhorse-agent serve", "config", configPath,
		"host", cfg.Server.Host, "port", cfg.Server.Port,
		"version", versionString())

	ctx := context.Background()

	// 1. Store.
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: cfg.Store.Path, BusyTimeoutMs: cfg.Store.BusyTimeoutMs})
	if err != nil {
		return fmt.Errorf("serve: open store: %w", err)
	}

	// 2. Providers — build all known providers so per-session ProviderName can
	// pick one (multi-agent spec: child may override parent's provider).
	providers, fastProviders, err := buildProviderRegistry(cfg)
	if err != nil {
		_ = st.Close()
		return err
	}

	// 3. Tool registry (built-ins).
	registry := tools.NewRegistry()
	skillCatalog := skills.Scan(cfg.Skills.Dir)
	if err := registerBuiltinTools(registry, cfg, skillCatalog); err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: register tools: %w", err)
	}

	// 4. Forward-declared session manager — needed by the permission prompt
	// callback (which looks up sessions by ID to drive permission_request
	// events). Filled in before any tool actually runs.
	var sessMgr *session.Manager

	permMgr := permission.New(st,
		permissionPromptUsingSessions(&sessMgr, logger),
		dangerousCommandPredicate(),
		time.Duration(cfg.Agent.PermissionRequestTimeoutSeconds)*time.Second,
	)

	// 5. Dispatch tool wiring (multi-agent). Loader rescans yamls on every
	// call so edits to ~/.workhorse-agent/agents/*.yaml take effect on the
	// next Dispatch without restart.
	loader := coord.NewLoader(cfg.Agents.Dir)
	dispatchHost := &dispatch.Host{
		Manager:  nil, // filled after sessMgr exists
		Loader:   loader,
		MaxDepth: cfg.Agent.MaxDepth,
	}
	if err := registry.Register(dispatch.Tool{Host: dispatchHost}); err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: register dispatch: %w", err)
	}

	// 6. Session manager with the agent-loop runner factory.
	sessMgr = session.NewManager(session.ManagerOptions{
		Store:         st,
		MaxConcurrent: cfg.Sessions.MaxConcurrent,
		RunnerFactory: newRunnerFactory(cfg, providers, fastProviders, registry, permMgr, skillCatalog, logger),
	})
	dispatchHost.Manager = sessMgr

	// 7. API server.
	apiCfg := apiConfigFrom(cfg)
	srv := api.NewServer(apiCfg, sessMgr, st, logger)
	exit, err := srv.Start()
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: start listener: %w", err)
	}
	fmt.Fprintf(stdout, "workhorse-agent listening on %s\n", srv.BoundAddr())

	// 8. Signal-driven graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-sigCh:
		logger.Info("serve: signal received, shutting down", "signal", sig.String())
	case err := <-exit:
		if err != nil {
			return fmt.Errorf("serve: listener exited: %w", err)
		}
	}
	signal.Stop(sigCh)

	shutdownCtx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.Server.GracefulShutdownTimeoutSeconds)*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Warn("serve: shutdown returned", "err", err)
	}
	logger.Info("serve: bye")
	return nil
}

func versionString() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

func buildLogger(cfg config.LoggingConfig, w io.Writer) *slog.Logger {
	lvl := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// buildProviderRegistry constructs every available provider (anthropic +
// openai) so that per-session ProviderName can pick one — required by the
// multi-agent spec's "child overrides parent provider" scenario. The default
// entry under cfg.Providers.Default is also reachable via that name.
//
// Returns (default+fast) maps keyed by provider name. An entry is only
// included when its API key is configured; missing keys for the default
// provider is fatal, missing keys for the other provider just means children
// can't switch to it.
func buildProviderRegistry(cfg config.Config) (def, fast map[string]provider.Provider, err error) {
	def = map[string]provider.Provider{}
	fast = map[string]provider.Provider{}

	if cfg.Providers.Anthropic.APIKey != "" {
		def["anthropic"] = anthropic.New(anthropic.Options{
			APIKey:  cfg.Providers.Anthropic.APIKey,
			BaseURL: cfg.Providers.Anthropic.BaseURL,
		})
		fast["anthropic"] = anthropic.New(anthropic.Options{
			APIKey:  cfg.Providers.Anthropic.APIKey,
			BaseURL: cfg.Providers.Anthropic.BaseURL,
		})
	}
	if cfg.Providers.OpenAI.APIKey != "" {
		def["openai"] = openai.New(openai.Options{
			APIKey:  cfg.Providers.OpenAI.APIKey,
			BaseURL: cfg.Providers.OpenAI.BaseURL,
		})
		fast["openai"] = openai.New(openai.Options{
			APIKey:  cfg.Providers.OpenAI.APIKey,
			BaseURL: cfg.Providers.OpenAI.BaseURL,
		})
	}
	if _, ok := def[cfg.Providers.Default]; !ok {
		return nil, nil, fmt.Errorf("serve: providers.default %q has no API key configured", cfg.Providers.Default)
	}
	return def, fast, nil
}

func registerBuiltinTools(reg *tools.Registry, cfg config.Config, catalog *skills.Catalog) error {
	for _, t := range []tools.Tool{
		builtin.Read{
			MaxBytes: cfg.Tools.ToolResultMaxBytes,
			Timeout:  time.Duration(cfg.Tools.Read.TimeoutSeconds) * time.Second,
		},
		builtin.Write{},
		builtin.Edit{},
		builtin.Grep{
			Timeout: time.Duration(cfg.Tools.Grep.TimeoutSeconds) * time.Second,
			Cfg:     cfg.Tools.Grep,
		},
		bash.Bash{
			DefaultTimeoutSeconds: cfg.Tools.Bash.TimeoutSeconds,
			MaxOutputBytes:        cfg.Tools.ToolResultMaxBytes,
			BaseEnv:               os.Environ(),
		},
		skills.NewLoadSkill(catalog),
	} {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func dangerousCommandPredicate() func(tool, resource string) (bool, string) {
	return func(tool, resource string) (bool, string) {
		if tool != "Bash" {
			return false, ""
		}
		lvl, label := bash.Inspect(resource)
		return lvl == bash.Dangerous, label
	}
}

// permissionPromptUsingSessions returns the PromptFunc used by permission.Manager.
// It emits a `permission_request` event and blocks on the session's
// PermissionAnswers channel until a matching decision arrives (or ctx
// cancels). The `mgr` indirection is necessary because the permission manager
// is constructed before the session manager exists.
func permissionPromptUsingSessions(mgr **session.Manager, logger *slog.Logger) permission.PromptFunc {
	return func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
		if mgr == nil || *mgr == nil {
			return permission.Deny, false
		}
		sess, err := (*mgr).GetSession(req.SessionID)
		if err != nil {
			logger.Warn("permission prompt: session lookup", "err", err, "session", req.SessionID)
			return permission.Deny, false
		}
		requestID := idgen.NewULID()
		if err := sess.Emit(ctx, "permission_request", map[string]any{
			"request_id": requestID,
			"tool":       req.Tool,
			"resource":   req.Resource,
			"dangerous":  req.Dangerous,
			"reason":     req.Reason,
		}); err != nil {
			return permission.Deny, false
		}
		// Loop on the answers channel until we see one with our request_id;
		// drop stale answers (e.g. from a prior prompt).
		for {
			select {
			case ans, ok := <-sess.PermissionAnswers:
				if !ok {
					return permission.Deny, false
				}
				if ans.RequestID != requestID {
					continue
				}
				return permission.Decision(ans.Decision), true
			case <-ctx.Done():
				return permission.Deny, false
			}
		}
	}
}

// newRunnerFactory returns the closure session.Manager calls per new session
// to instantiate that session's agent.Loop. The loop holds per-session state
// (ToolEnv) while sharing the long-lived provider, permission manager, and
// tool registry. Per-session ProviderName / Model / SystemPromptBase override
// the server-wide defaults so the multi-agent Dispatch can spin children with
// distinct provider+role configuration.
func newRunnerFactory(
	cfg config.Config,
	defProv, fastProv map[string]provider.Provider,
	reg *tools.Registry,
	permMgr *permission.Manager,
	skillCatalog *skills.Catalog,
	logger *slog.Logger,
) session.RunnerFactory {
	orch := &agent.Orchestrator{
		Registry:       reg,
		MaxParallel:    cfg.Agent.MaxParallelTools,
		DefaultTimeout: time.Duration(cfg.Tools.DefaultTimeoutSeconds) * time.Second,
		MaxResultBytes: cfg.Tools.ToolResultMaxBytes,
	}
	loopCfg := agent.LoopConfig{
		MaxTokens:          4096,
		AutoCompactRatio:   cfg.Agent.AutoCompactRatio,
		CompactRecentKeep:  cfg.Agent.CompactRecentKeep,
		MaxHistoryTokens:   cfg.Agent.MaxHistoryTokens,
		CancelDrainTimeout: time.Duration(cfg.Agent.CancelDrainTimeoutSeconds) * time.Second,
		Retry: agent.RetryConfig{
			Attempts: cfg.Agent.ProviderRetryAttempts,
			Backoff:  msToDurations(cfg.Agent.ProviderRetryBackoffMs),
		},
	}
	loopCfg.ApplyDefaults()
	defaultProvName := cfg.Providers.Default
	defaultModel := cfg.Models.Default
	defaultFastModel := cfg.Models.Fast
	return func(sess *session.Session) session.Runner {
		provName := sess.ProviderName
		if provName == "" {
			provName = defaultProvName
		}
		prov, ok := defProv[provName]
		if !ok {
			prov = defProv[defaultProvName]
		}
		fast, ok := fastProv[provName]
		if !ok {
			fast = fastProv[defaultProvName]
		}
		model := sess.Model
		if model == "" {
			model = defaultModel
		}
		_, modelID := provider.SplitProviderModel(model)
		_, fastModelID := provider.SplitProviderModel(defaultFastModel)
		loop := agent.NewLoop(loopCfg)
		loop.Session = sess
		loop.Provider = prov
		loop.Orchestrator = orch
		loop.Permissions = permMgr
		loop.Logger = logger
		loop.SystemPromptBase = sess.SystemPromptBase
		// Skill manifest is injected only for top-level sessions; child agents
		// (Dispatch) get their tool surface from their agent_type definition.
		if sess.ParentID == "" {
			if manifest := skills.FormatManifest(skillCatalog); manifest != "" {
				base := strings.TrimRight(loop.SystemPromptBase, " \t\n")
				if base != "" {
					base += "\n\n"
				}
				loop.SystemPromptBase = base + manifest
			}
		}
		loop.Config.Model = modelID
		loop.ToolEnv = &tools.Env{
			SessionID: sess.ID,
			Workdir:   sess.Workdir,
			Env:       sess.Env,
		}
		loop.Registry = reg
		loop.Compactor = &agent.Compactor{
			Provider:   fast,
			Model:      fastModelID,
			RecentKeep: cfg.Agent.CompactRecentKeep,
		}
		// Per-session AllowedTools filters the schema list the LLM sees.
		loop.Tools = buildProviderToolSchemas(reg, sess.AllowedTools())
		return loop
	}
}

// buildProviderToolSchemas converts the registry's tools into the
// provider-facing schema list (name + JSON schema) the LLM sees. If allowed
// is non-empty, only those tools are exposed (per-session AllowedTools filter).
func buildProviderToolSchemas(reg *tools.Registry, allowed []string) []provider.ToolSchema {
	tools := reg.Filtered(allowed)
	out := make([]provider.ToolSchema, 0, len(tools))
	for _, t := range tools {
		out = append(out, provider.ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	return out
}

func apiConfigFrom(cfg config.Config) api.Config {
	bytesLimit := int64(cfg.Server.MaxRequestBodyBytes)
	if bytesLimit <= 0 {
		bytesLimit = 1 << 20
	}
	return api.Config{
		Host:                    cfg.Server.Host,
		Port:                    cfg.Server.Port,
		ReadHeaderTimeout:       time.Duration(cfg.Server.ReadHeaderTimeoutSeconds) * time.Second,
		ReadTimeout:             time.Duration(cfg.Server.ReadTimeoutSeconds) * time.Second,
		IdleTimeout:             time.Duration(cfg.Server.IdleTimeoutSeconds) * time.Second,
		MaxHeaderBytes:          cfg.Server.MaxHeaderBytes,
		MaxRequestBodyBytes:     bytesLimit,
		GracefulShutdownTimeout: time.Duration(cfg.Server.GracefulShutdownTimeoutSeconds) * time.Second,
		SSEKeepalive:            time.Duration(cfg.Server.SSEKeepaliveSeconds) * time.Second,
		AllowedOrigins:          cfg.Server.AllowedOrigins,
		AllowNullOrigin:         cfg.Server.AllowNullOrigin,
		DebugEnabled:            cfg.Debug.Enabled,
		Auth: api.BearerConfig{
			Enabled: cfg.Auth.Enabled,
			Token:   cfg.Auth.BearerToken,
		},
		MaxConcurrentSessions: cfg.Sessions.MaxConcurrent,
		MaxHistoryTokens:      cfg.Agent.MaxHistoryTokens,
		Version:               versionString(),
	}
}

func msToDurations(ms []int) []time.Duration {
	if len(ms) == 0 {
		return nil
	}
	out := make([]time.Duration, len(ms))
	for i, v := range ms {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

// ensure store interface satisfied
var _ store.Store = (*sqlite.Store)(nil)
