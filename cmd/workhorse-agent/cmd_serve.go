package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/wallfacers/workhorse-agent/internal/api/protocol"
	"github.com/wallfacers/workhorse-agent/internal/config"
	"github.com/wallfacers/workhorse-agent/internal/coord"
	"github.com/wallfacers/workhorse-agent/internal/embedding"
	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/approval"
	"github.com/wallfacers/workhorse-agent/internal/extagent/draft"
	extagentdriver "github.com/wallfacers/workhorse-agent/internal/extagent/driver"
	"github.com/wallfacers/workhorse-agent/internal/extagent/regen"
	"github.com/wallfacers/workhorse-agent/internal/extagent/smoke"
	"github.com/wallfacers/workhorse-agent/internal/idgen"
	"github.com/wallfacers/workhorse-agent/internal/instructions"
	"github.com/wallfacers/workhorse-agent/internal/mcp"
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/memory/curation"
	"github.com/wallfacers/workhorse-agent/internal/memory/pipeline"
	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/prompt"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/provider/anthropic"
	"github.com/wallfacers/workhorse-agent/internal/provider/openai"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/skills"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/agentsetup"
	"github.com/wallfacers/workhorse-agent/internal/tools/bash"
	"github.com/wallfacers/workhorse-agent/internal/tools/builtin"
	"github.com/wallfacers/workhorse-agent/internal/tools/dispatch"
	extagenttool "github.com/wallfacers/workhorse-agent/internal/tools/extagent"
	"github.com/wallfacers/workhorse-agent/internal/tools/extagent/drafttool"
	"github.com/wallfacers/workhorse-agent/internal/tools/extagent/genbash"
	"github.com/wallfacers/workhorse-agent/internal/tools/memorytool"
	"github.com/wallfacers/workhorse-agent/internal/tools/sessionsearch"
	"github.com/wallfacers/workhorse-agent/internal/tools/tasklist"
	"github.com/wallfacers/workhorse-agent/internal/tools/toolsearch"
)

// adapterPermGate bridges permission.Manager to extagenttool.PermissionGate.
type adapterPermGate struct {
	mgr *permission.Manager
}

func (g *adapterPermGate) Prompt(ctx context.Context, sessionID, toolName, adapterName string) (bool, error) {
	if g.mgr == nil {
		return false, fmt.Errorf("permission manager not initialized")
	}
	decision, _, err := g.mgr.Check(ctx, permission.CheckInput{
		SessionID: sessionID,
		Tool:      toolName,
		Resource:  adapterName,
	})
	if err != nil {
		return false, err
	}
	switch decision {
	case permission.AllowOnce, permission.AllowSession, permission.AllowPermanent:
		return true, nil
	default:
		return false, nil
	}
}

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

	// 1a. One-time, idempotent flat-file → entry migration (design D7). A WARN
	// on failure is non-fatal: the marker is only written on full success, so a
	// failed migration is retried on the next startup.
	if err := memory.MigrateLegacyFiles(ctx, memory.NewEntryStore(st.DB()), profileDir(cfg)); err != nil {
		logger.Warn("memory: legacy migration failed", "err", err)
	}

	// 1b. Inject preset permission rules from config (idempotent).
	if err := applyPresetRules(ctx, st, cfg.Tools.PresetRules, logger); err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: apply preset rules: %w", err)
	}

	// 2. Providers — build all known providers so per-session ProviderName can
	// pick one (multi-agent spec: child may override parent's provider).
	providers, fastProviders, degraded, err := buildProviderRegistry(cfg)
	if err != nil {
		_ = st.Close()
		return err
	}
	if degraded != "" {
		logger.Warn("serve: starting in degraded mode — provider unavailable until configured",
			"reason", degraded, "default_provider", cfg.Providers.Default)
	}
	// Extended thinking is Anthropic-only; the OpenAI adapter silently ignores
	// the flag. Warn so a thinking.enabled config against an OpenAI default
	// isn't a silent no-op the operator can't diagnose.
	if cfg.Agent.Thinking.Enabled && cfg.Providers.Default == "openai" {
		logger.Warn("agent: thinking.enabled has no effect — the default provider is openai, which does not support extended thinking")
	}

	// 3. Tool registry (built-ins).
	registry := tools.NewRegistry()
	skillCatalog := skills.Scan(cfg.Skills.Dir)
	memRT, err := registerBuiltinTools(registry, cfg, skillCatalog, st, providers, logger)
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: register tools: %w", err)
	}
	// memRT.usage drains the memory usage-bump channel (design D8). Close it on
	// shutdown so pending bumps flush; if a later init step bails before the
	// normal defer path is established, Close is idempotent.
	defer memRT.usage.Close()
	// Write-behind embedder (memory-hybrid-retrieval-locomo). Nil when embedding
	// is unconfigured; Close and Backfill are nil-safe. Backfill runs off the hot
	// path so a slow/absent embedding endpoint never blocks startup.
	defer memRT.embedder.Close()
	// Session-end extraction ingestor (ADD-only pipeline). Nil-safe Close flushes
	// in-flight extractions on shutdown.
	defer memRT.ingestor.Close()
	go func() {
		if err := memRT.embedder.Backfill(ctx); err != nil {
			logger.Warn("memory: embedding backfill failed", "error", err)
		}
	}()
	// Background curation maintenance loop (design D5/D6). Inert when no judge
	// model is configured. Stops when ctx is cancelled at shutdown.
	curator := memRT.curator
	curator.Start(ctx)

	// 3a. MCP host: load mcp.json (sibling of config.yaml) and register every
	// healthy server's tools as namespaced adapters (<server>__<tool>). A
	// missing mcp.json means no MCP; a single failing server is logged inside
	// LoadAndStart and skipped — serve never blocks on MCP startup.
	mcpHost := mcp.NewHost(logger)
	defer mcpHost.Shutdown()
	loadMCPTools(mcpHost, filepath.Join(filepath.Dir(configPath), "mcp.json"), registry, logger)

	// 3b. External agent adapter loading.
	var extReg *extagent.Registry
	extLoader := &extagent.Loader{Logger: logger}
	extSnap, err := extLoader.Load(cfg.ExternalAgents.Dir)
	if err != nil {
		logger.Warn("extagent: load failed, proceeding without external agents", "error", err)
	}
	if extSnap != nil {
		extReg = extagent.NewRegistry(extSnap)
		cacheDir := filepath.Join(profileDir(cfg), "cache", "smoke")
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			logger.Warn("extagent: failed to create smoke cache dir", "dir", cacheDir, "error", err)
		}
		smoke.RunCachedAll(extReg, cacheDir, cfg.ExternalAgents.SmokeTest.CacheTTL, logger)
		// Drift check: surfaces llm_generated adapters whose binary has
		// changed version since generation. Log-only; never auto-regen.
		driftEntries := regen.Check(extReg, logger)
		if len(driftEntries) > 0 {
			logger.Info("extagent: drift detected", "count", len(driftEntries))
		}
	}

	// 4. Forward-declared session manager — needed by the permission prompt
	// callback (which looks up sessions by ID to drive permission_request
	// events). Filled in before any tool actually runs.
	var sessMgr *session.Manager

	permMgr := permission.New(
		st,
		permissionPromptUsingSessions(&sessMgr, logger),
		dangerousCommandPredicate(),
		time.Duration(cfg.Agent.PermissionRequestTimeoutSeconds)*time.Second,
		store.PermissionDecision(cfg.Tools.DefaultPermission),
	)

	// 3c. Register ExternalAgent tool (after permMgr exists for gating).
	if extReg != nil {
		eaTool := extagenttool.New(&extagenttool.Host{
			Registry:        extReg,
			PermissionGate:  &adapterPermGate{mgr: permMgr},
			Driver:          &extagentdriver.Driver{Logger: logger},
			OutputCapBytes:  cfg.Tools.ToolResultMaxBytes,
			KillOnOutputCap: cfg.ExternalAgents.Driver.KillOnOutputCap,
		})
		if eaTool != nil {
			if err := registry.Register(eaTool); err != nil {
				logger.Warn("extagent: register ExternalAgent tool", "error", err)
			}
		}
	}

	// 5. Dispatch tool wiring (multi-agent). Loader rescans yamls on every
	// call so edits to ~/.workhorse-agent/agents/*.yaml take effect on the
	// next Dispatch without restart.
	loader := coord.NewLoader(cfg.Agents.Dir)
	dispatchHost := &dispatch.Host{
		Manager:  nil, // filled after sessMgr exists
		Loader:   loader,
		MaxDepth: cfg.Agent.MaxDepth,
	}
	dispatchTool := dispatch.Tool{Host: dispatchHost}
	if err := registry.Register(dispatchTool); err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: register dispatch: %w", err)
	}

	// 5b. agent_setup tool (LLM-driven adapter generation). The approval
	// manager is wired without an emitter for now — §9 wires the SSE side.
	// The dispatcher is a thin closure delegating to the registered Dispatch
	// tool with agent_type pinned to adapter-generator.
	// approvalMgr is constructed up front; its Emitter / Publisher (which
	// depend on sessMgr and the live external-agents dir) are wired in step 7
	// after sessMgr exists.
	approvalMgr := approval.New(approval.Options{
		Timeout: time.Duration(cfg.ExternalAgents.Generation.ApprovalTimeoutSec) * time.Second,
	})

	if cfg.ExternalAgents.Generation.Enabled {
		examples := loadBuiltinAdapterExamples(logger)
		setupHost := &agentsetup.Host{
			Registry:          extReg,
			ExternalAgentsDir: cfg.ExternalAgents.Dir,
			Dispatcher:        newAdapterGeneratorDispatcher(dispatchTool),
			Approval:          approvalMgr,
			SchemaJSON:        loadAdapterSchemaJSON(logger),
			Examples:          examples,
			ModelDefault:      cfg.Models.Default,
			AllowedModels:     append([]string(nil), cfg.ExternalAgents.Generation.AllowedModels...),
		}
		if err := registry.Register(agentsetup.Tool{Host: setupHost}); err != nil {
			logger.Warn("agent_setup: register failed", "error", err)
		}
	}

	// 6. Session manager with the agent-loop runner factory.
	sessMgr = session.NewManager(session.ManagerOptions{
		Store:         st,
		MaxConcurrent: cfg.Sessions.MaxConcurrent,
		RunnerFactory: newRunnerFactory(cfg, st, providers, fastProviders, registry, permMgr, skillCatalog, extReg, loader, memRT.ingestor, logger),
	})
	dispatchHost.Manager = sessMgr

	// 6b. Late-wire the approval manager hooks that depend on sessMgr / the
	// live external-agents dir.
	approvalMgr.SetEmitter(api.NewSessionEventEmitter(sessMgr))
	approvalMgr.SetPublisher(&draftPublisher{liveDir: cfg.ExternalAgents.Dir})
	approvalMgr.SetDedupClearer(&sessionDedupClearer{manager: sessMgr})
	if extReg != nil {
		approvalMgr.SetRegistryInjector(&registryInjector{registry: extReg, logger: logger})
	}
	approvalMgr.SetMarkApprover(&permissionMarkApprover{mgr: permMgr})
	approvalMgr.SetEditValidator(&editValidator{liveDir: cfg.ExternalAgents.Dir})

	// 7. API server.
	apiCfg := apiConfigFrom(cfg)
	apiCfg.DegradedReason = degraded
	srv := api.NewServer(apiCfg, sessMgr, st, logger)
	srv.SetApprovalManager(approvalMgr)
	if extReg != nil {
		srv.SetDriftSnapshot(regen.Check(extReg, nil))
	}
	exit, err := srv.Start()
	if err != nil {
		_ = st.Close()
		return fmt.Errorf("serve: start listener: %w", err)
	}
	fmt.Fprintf(stdout, "workhorse-agent listening on %s\n", srv.BoundAddr())

	// 8. Hot-reload of the permission subset of config.yaml. The store already
	// has the startup preset rules applied; the reloader diffs future loads
	// against this snapshot and applies only the permission fields.
	reloader := newPermReloader(configPath, cfg, st, permMgr, curator, logger)
	reloader.args = args
	srv.SetPermissionConfig(configPath, func(ctx context.Context) error { return reloader.Reload(ctx) })
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	stopWatch, werr := watchConfigFile(watchCtx, configPath, defaultReloadDebounce, func() {
		_ = reloader.Reload(watchCtx) // Reload warns on failure and keeps prior config
	}, logger)
	if werr != nil {
		logger.Warn("config watch: disabled (file events unavailable); SIGHUP still reloads", "error", werr)
		stopWatch = func() {}
	}
	defer stopWatch()

	// 9. Signal-driven control. SIGHUP triggers a config reload; SIGTERM/SIGINT
	// begin graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				logger.Info("serve: SIGHUP received, reloading config")
				_ = reloader.Reload(watchCtx)
				continue
			}
			logger.Info("serve: signal received, shutting down", "signal", sig.String())
		case err := <-exit:
			if err != nil {
				return fmt.Errorf("serve: listener exited: %w", err)
			}
		}
		break
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.Server.GracefulShutdownTimeoutSeconds)*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Warn("serve: shutdown returned", "err", err)
	}
	// Cancel any in-flight adapter approvals so their AfterFunc goroutines
	// don't outlive the listener and fire against a stale emitter.
	approvalMgr.Cancel()
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
// entry under cfg.Providers.Default is also reachable by that name.
//
// Returns (default+fast) maps keyed by provider name plus a degraded reason. An
// entry is only included when its API key is configured. A missing key for the
// default provider is NOT fatal: serve starts degraded (reason
// "no_provider_key") so a managed launcher can attach, read /health, and guide
// the user to configure a key instead of facing a crash-loop. A missing key for
// the non-default provider just means children can't switch to it.
func buildProviderRegistry(cfg config.Config) (def, fast map[string]provider.Provider, degraded string, err error) {
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
		return def, fast, "no_provider_key", nil
	}
	return def, fast, "", nil
}

// registerBuiltinTools registers the always-loaded local tools. It returns the
// memory usage-logger (whose background goroutine drains usage bumps from
// LoadMemory/MemorySearch, design D8); the caller MUST Close it on shutdown so
// pending bumps flush.
// memoryRuntime bundles the background memory components that outlive
// registerBuiltinTools and must be stopped on shutdown.
type memoryRuntime struct {
	usage    *memory.UsageLogger
	curator  *curation.Worker
	embedder *memory.Embedder          // may be nil (embedding disabled)
	ingestor *pipeline.SessionIngestor // may be nil (pipeline disabled)
}

func registerBuiltinTools(reg *tools.Registry, cfg config.Config, catalog *skills.Catalog, st *sqlite.Store, providers map[string]provider.Provider, logger *slog.Logger) (*memoryRuntime, error) {
	// Per-entry memory store + budgets (from config).
	es := memory.NewEntryStore(st.DB())
	budgets := memoryBudgets(cfg)
	usage := memory.NewUsageLogger(es, memory.DefaultUsageBuffer)

	// Embedding infrastructure (memory-hybrid-retrieval-locomo). A nil client
	// (embedding unconfigured) yields a nil embedder: every vector path degrades
	// to FTS behavior.
	vectors := memory.NewVectorStore(st.DB())
	embClient := buildEmbeddingClient(cfg, logger)
	embedder := memory.NewEmbedder(es, vectors, embClient, memory.DefaultEmbedBuffer)

	// Curation worker (design D5/D6). A missing/misconfigured judge_model leaves
	// the worker inert (call=nil) — memory still works, curation just never runs.
	curator := buildCurationWorker(cfg, es, st, providers, logger)
	writeTool := &memorytool.Write{Store: es, Budgets: budgets}
	if curator != nil {
		writeTool.OnWrite = curator.Notify
	}
	if embedder != nil {
		writeTool.AfterWrite = embedder.Enqueue
	}
	searchTool := &memorytool.MemorySearch{
		DB:        st.DB(),
		Retriever: memory.NewRetriever(es, vectors, embClient).WithReranker(buildReranker(cfg, logger)),
	}

	// ADD-only extraction pipeline (memory-hybrid-retrieval-locomo). Enabled by
	// default; inert when disabled or when the extract model's provider is
	// unavailable.
	ingestor := buildMemoryIngestor(cfg, es, embedder, budgets, curator, providers, logger)

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
		&memorytool.Read{Store: es},
		writeTool,
		memorytool.NewLoadMemory(es, usage),
		searchTool,
		&memorytool.Delete{Store: es},
		&memorytool.Merge{Store: es, Budgets: budgets},
		&sessionsearch.Tool{DB: st.DB()},
		tasklist.TodoWrite{},
		toolsearch.Tool{},
	} {
		if err := reg.Register(t); err != nil {
			usage.Close()
			embedder.Close()
			ingestor.Close()
			return nil, err
		}
	}
	return &memoryRuntime{usage: usage, curator: curator, embedder: embedder, ingestor: ingestor}, nil
}

// buildMemoryIngestor constructs the session-end extraction ingestor. Returns
// nil when the pipeline is disabled or its extract model's provider is missing.
func buildMemoryIngestor(cfg config.Config, es *memory.EntryStore, embedder *memory.Embedder, budgets memory.Budgets, curator *curation.Worker, providers map[string]provider.Provider, logger *slog.Logger) *pipeline.SessionIngestor {
	if !cfg.Memory.Pipeline.Enabled {
		logger.Info("memory: extraction pipeline disabled (memory.pipeline.enabled=false)")
		return nil
	}
	extractModel := cfg.Memory.Pipeline.ExtractModel
	if extractModel == "" {
		extractModel = cfg.Memory.Curation.JudgeModel
	}
	call, err := curation.NewProviderCaller(providers, extractModel, extractMaxTokens)
	if err != nil {
		logger.Warn("memory: extraction pipeline inert: extract model unavailable", "extract_model", extractModel, "error", err)
		return nil
	}
	var onWrite func()
	if curator != nil {
		onWrite = curator.Notify
	}
	p := pipeline.New(pipeline.Config{
		Entries:  es,
		Embedder: embedder,
		Call:     pipeline.ModelCaller(call),
		Budgets:  budgets,
		OnWrite:  onWrite,
	})
	logger.Info("memory: extraction pipeline enabled", "extract_model", extractModel)
	return pipeline.NewSessionIngestor(p)
}

// extractMaxTokens caps the extraction model's JSON output.
const extractMaxTokens = 2048

// buildEmbeddingClient constructs the embedding client from config. It returns
// nil (embedding disabled → FTS-only degradation) when base_url or model is
// empty, or when construction fails. The API key is never logged.
func buildEmbeddingClient(cfg config.Config, logger *slog.Logger) embedding.Client {
	emb := cfg.Memory.Embedding
	client, err := embedding.New(embedding.Config{
		BaseURL:    emb.BaseURL,
		Model:      emb.Model,
		APIKey:     emb.APIKey,
		Dimensions: emb.Dimensions,
		Timeout:    time.Duration(emb.TimeoutSeconds) * time.Second,
	})
	if err != nil {
		logger.Warn("memory: embedding disabled: client construction failed", "error", err)
		return nil
	}
	if client == nil {
		logger.Info("memory: semantic search disabled (memory.embedding not configured); retrieval uses keyword + entity signals")
		return nil
	}
	logger.Info("memory: semantic search enabled", "model", emb.Model, "base_url", emb.BaseURL)
	return client
}

// buildReranker constructs the cross-encoder rerank client from config. It
// returns nil (rerank disabled → pure RRF order) when memory.embedding.base_url
// or rerank_model is empty, or when construction fails. The API key is never
// logged.
func buildReranker(cfg config.Config, logger *slog.Logger) embedding.Reranker {
	emb := cfg.Memory.Embedding
	rr, err := embedding.NewReranker(embedding.RerankConfig{
		BaseURL: emb.BaseURL,
		Model:   emb.RerankModel,
		APIKey:  emb.APIKey,
		Timeout: time.Duration(emb.TimeoutSeconds) * time.Second,
	})
	if err != nil || rr == nil {
		return nil
	}
	logger.Info("memory: retrieval reranking enabled", "rerank_model", emb.RerankModel)
	return rr
}

// judgeMaxTokens caps the curation judge's structured output. The verdict is a
// short JSON object, so a small ceiling keeps a runaway response bounded.
const judgeMaxTokens = 2048

// buildCurationWorker assembles the background curation worker from config. It
// returns nil only on an internal inconsistency; a missing/misconfigured
// judge_model yields a worker with a nil caller (inert), logged at WARN, so
// memory keeps working and curation is simply disabled until configured.
func buildCurationWorker(cfg config.Config, es *memory.EntryStore, st *sqlite.Store, providers map[string]provider.Provider, logger *slog.Logger) *curation.Worker {
	cur := cfg.Memory.Curation
	call, err := curation.NewProviderCaller(providers, cur.JudgeModel, judgeMaxTokens)
	if err != nil {
		logger.Warn("curation disabled: judge model unavailable", "judge_model", cur.JudgeModel, "error", err)
		call = nil
	}
	return curation.NewWorker(es, st.DB(), call, curation.Config{
		EntryCountHigh:       cur.EntryCountHigh,
		MinInterval:          time.Duration(cur.MinIntervalMinutes) * time.Minute,
		LeaseTTL:             time.Duration(cur.LeaseTTLSeconds) * time.Second,
		ManifestBudgetChars:  cfg.Memory.ManifestBudgetChars,
		MaxCandidatesPerPass: cur.MaxCandidatesPerPass,
		ContentSnippetChars:  cfg.Memory.EntryContentMaxChars,
		Weights:              memoryCurationWeights(cfg),
		Budgets:              memoryBudgets(cfg),
	}, logger)
}

func profileDir(cfg config.Config) string {
	dir := cfg.Memory.Dir
	if dir != "" {
		return dir
	}
	dir = cfg.Store.Path
	if dir == "" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".workhorse-agent")
	}
	return filepath.Dir(dir)
}

// memoryBudgets maps the memory config block onto the memory package's Budgets.
// Kept here (not in the config package) so config does not import memory.
func memoryBudgets(cfg config.Config) memory.Budgets {
	return memory.Budgets{
		PinnedChars:       cfg.Memory.PinnedBudgetChars,
		ManifestChars:     cfg.Memory.ManifestBudgetChars,
		EntryContentChars: cfg.Memory.EntryContentMaxChars,
		TriggerChars:      cfg.Memory.TriggerMaxChars,
	}
}

// memoryCurationWeights maps the configured curation weights onto the curation
// package's local Weights type. Kept here (not in config or curation) so neither
// package depends on the other.
func memoryCurationWeights(cfg config.Config) curation.Weights {
	w := cfg.Memory.Curation.Weights
	return curation.Weights{
		Hit:        w.Hit,
		Recency:    w.Recency,
		Age:        w.Age,
		Volatility: w.Volatility,
	}
}

// loadMCPTools starts the MCP host from mcpPath (if the file exists) and
// registers every healthy server's tools into reg under the namespaced
// <server>__<tool> name. It never fails serve startup: a missing file is a
// silent skip, a parse error or failing server is a WARN.
func loadMCPTools(host *mcp.Host, mcpPath string, reg *tools.Registry, logger *slog.Logger) {
	if _, err := os.Stat(mcpPath); err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("mcp: stat mcp.json", "path", mcpPath, "error", err)
		}
		return
	}
	if err := host.LoadAndStart(mcpPath); err != nil {
		logger.Warn("mcp: load failed, proceeding without MCP tools", "path", mcpPath, "error", err)
	}
	mcpTools := host.AllTools()
	for _, srvTool := range mcpTools {
		if err := reg.Register(mcp.NewAdapter(srvTool)); err != nil {
			logger.Warn("mcp: register tool", "tool", srvTool.Server+"__"+srvTool.Def.Name, "error", err)
		}
	}
	if len(mcpTools) > 0 {
		logger.Info("mcp: tools registered", "count", len(mcpTools))
	}
}

// allowlistDroppedTools returns the registered tool names a session's
// allowed_tools filter removes from the model-facing schema. Empty allowed
// means "no filter" → nothing dropped.
func allowlistDroppedTools(reg *tools.Registry, allowed []string) []string {
	if len(allowed) == 0 {
		return nil
	}
	kept := map[string]struct{}{}
	for _, t := range reg.Filtered(allowed) {
		kept[t.Name()] = struct{}{}
	}
	var dropped []string
	for _, t := range reg.Filtered(nil) {
		if _, ok := kept[t.Name()]; !ok {
			dropped = append(dropped, t.Name())
		}
	}
	return dropped
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
		// Correlate with the originating tool_use; non-loop callers (e.g. the
		// extagent adapter gate) pass no RequestID, so mint one.
		requestID := req.RequestID
		if requestID == "" {
			requestID = idgen.NewULID()
		}
		payload := map[string]any{
			"request_id": requestID,
			"tool":       req.Tool,
			"resource":   req.Resource,
			"dangerous":  req.Dangerous,
			"reason":     req.Reason,
		}
		if deadline, ok := ctx.Deadline(); ok {
			payload["expires_at"] = deadline.UTC().Format(time.RFC3339)
		}
		if err := sess.Emit(ctx, "permission_request", payload); err != nil {
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
	st *sqlite.Store,
	defProv, fastProv map[string]provider.Provider,
	reg *tools.Registry,
	permMgr *permission.Manager,
	skillCatalog *skills.Catalog,
	extReg *extagent.Registry,
	agentLoader *coord.Loader,
	ingestor *pipeline.SessionIngestor,
	logger *slog.Logger,
) session.RunnerFactory {
	orch := &agent.Orchestrator{
		Registry:       reg,
		MaxParallel:    cfg.Agent.MaxParallelTools,
		DefaultTimeout: time.Duration(cfg.Tools.DefaultTimeoutSeconds) * time.Second,
		MaxResultBytes: cfg.Tools.ToolResultMaxBytes,
	}
	loopCfg := agent.LoopConfig{
		MaxTokens:          cfg.Agent.MaxTokens,
		AutoCompactRatio:   cfg.Agent.AutoCompactRatio,
		CompactRecentKeep:  cfg.Agent.CompactRecentKeep,
		MaxHistoryTokens:   cfg.Agent.MaxHistoryTokens,
		CancelDrainTimeout: time.Duration(cfg.Agent.CancelDrainTimeoutSeconds) * time.Second,
		Retry: agent.RetryConfig{
			Attempts: cfg.Agent.ProviderRetryAttempts,
			Backoff:  msToDurations(cfg.Agent.ProviderRetryBackoffMs),
		},
		ThinkingEnabled:      cfg.Agent.Thinking.Enabled,
		ThinkingBudgetTokens: cfg.Agent.Thinking.BudgetTokens,
	}
	{
		mode, pct, _ := config.ParseToolSearch(cfg.Tools.ToolSearch)
		loopCfg.ToolSearchMode = mode
		loopCfg.ToolSearchPercent = pct
	}
	loopCfg.ApplyDefaults()
	defaultProvName := cfg.Providers.Default
	defaultModel := cfg.Models.Default
	defaultFastModel := cfg.Models.Fast
	// Memory is now a per-entry SQLite store assembled into a two-layer snapshot
	// (design D2). The manifest survival ordering uses the same curation scorer
	// that drives eviction ranking (design D5) — one ranking, no divergence.
	memWeights := memoryCurationWeights(cfg)
	memLoader := &memory.Loader{
		Store:   memory.NewEntryStore(st.DB()),
		Budgets: memoryBudgets(cfg),
		ScoreFn: func(e *memory.Entry) float64 {
			return curation.Score(e, memWeights, time.Now().UTC())
		},
	}
	instrLoader := &instructions.Loader{ProfileDir: profileDir(cfg)}
	return func(sess *session.Session) session.Runner {
		snap, err := memLoader.Load(context.Background())
		if err != nil {
			slog.Warn("memory snapshot load failed, proceeding without memory", "error", err)
		}
		sess.MemorySnapshot = snap

		// Locked-down internal agent types (adapter-generator) do not inherit
		// project instructions: their prompt is a fixed sandboxed surface and
		// arbitrary AGENTS.md content is a prompt-injection vector. Leaving both
		// fields nil yields an empty instructions block and a nil resolver, both
		// handled by the downstream nil checks.
		if coord.AgentTypeInheritsInstructions(sess.AgentType) {
			instrSnap, err := instrLoader.Load(sess.Workdir)
			if err != nil {
				slog.Warn("instruction snapshot load failed, proceeding without instructions", "error", err)
			}
			sess.InstructionSnapshot = instrSnap
			sess.InstructionResolver = instructions.NewResolver(instrSnap)
		}

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
		// Copy loopCfg so per-session thinking override does not
		// mutate the shared template for other sessions.
		sessLoopCfg := loopCfg
		// If only_models is set, disable thinking for models not in the list.
		if sessLoopCfg.ThinkingEnabled && len(cfg.Agent.Thinking.OnlyModels) > 0 {
			matched := false
			for _, m := range cfg.Agent.Thinking.OnlyModels {
				if m == modelID {
					matched = true
					break
				}
			}
			if !matched {
				sessLoopCfg.ThinkingEnabled = false
			}
		}
		loop := agent.NewLoop(sessLoopCfg)
		loop.Session = sess
		loop.Provider = prov
		loop.Orchestrator = orch
		loop.Permissions = permMgr
		loop.Logger = logger
		loop.SystemPromptBase = sess.SystemPromptBase
		// Top-level sessions that didn't supply their own system_prompt fall
		// back to the built-in orchestrator base prompt. Child agents keep the
		// system_prompt from their agent_type definition untouched.
		if sess.ParentID == "" && strings.TrimSpace(loop.SystemPromptBase) == "" {
			loop.SystemPromptBase = prompt.DefaultBasePrompt
		}
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
		// Session-end memory extraction runs for top-level sessions only; child
		// agents (Dispatch) don't accumulate durable user memory. Nil when the
		// pipeline is disabled.
		if sess.ParentID == "" && ingestor != nil {
			loop.MemoryIngestor = ingestor
		}
		// Per-session task list: in-memory, broadcast over SSE on every change so
		// the user sees progress. Independent per session — child agents get their
		// own store (add-todo-tool D2a). sess is captured per factory call.
		taskSess := sess
		taskStore := tasklist.NewStore(func(ctx context.Context, tasks []tasklist.Task) {
			_ = taskSess.Emit(ctx, string(protocol.EventTaskUpdate), map[string]any{"tasks": tasks})
		})
		loop.ToolEnv = &tools.Env{
			SessionID:           sess.ID,
			Workdir:             sess.Workdir,
			Env:                 sess.Env,
			ExtAgentRegistry:    extReg,
			TaskList:            taskStore,
			InstructionResolver: sess.InstructionResolver,
		}

		// Build environment block for the system prompt. Always include the
		// static fields; external sub-agents and dispatch agent_type roles are
		// added when present.
		envInput := prompt.EnvironmentInput{
			OS:    prompt.DetectOS(),
			Shell: "bash",
			CWD:   sess.Workdir,
		}
		if extReg != nil {
			if healthy := extReg.HealthySubAgents(); len(healthy) > 0 {
				envInput.SubAgents = buildSubAgentHints(healthy)
			}
		}
		// dispatch_agents are only meaningful for sessions that can Dispatch.
		// Child sessions inherit a restricted tool surface, so list roles only
		// for top-level sessions to avoid implying capabilities they lack.
		if sess.ParentID == "" && agentLoader != nil {
			envInput.DispatchAgents = buildDispatchAgentHints(agentLoader)
		}
		sess.EnvSnapshot = prompt.EnvironmentBlock(envInput)
		// adapter-generator sessions get a per-session registry overlay: the
		// real Bash is shadowed by genbash (read-only probes only) and
		// WriteAdapterDraft is added. The global registry is left untouched
		// so non-generator sessions cannot see either tool.
		sessReg := reg
		if sess.AgentType == coord.AdapterGeneratorTypeName {
			sessReg = reg.Clone()
			extDir := cfg.ExternalAgents.Dir
			_ = sessReg.Replace(genbash.Tool{
				Host: &genbash.Host{},
				Backend: bash.Bash{
					DefaultTimeoutSeconds: cfg.Tools.Bash.TimeoutSeconds,
					MaxOutputBytes:        cfg.Tools.ToolResultMaxBytes,
					BaseEnv:               os.Environ(),
				},
			})
			_ = sessReg.Register(drafttool.Tool{
				Host: &drafttool.Host{ExternalAgentsDir: extDir},
			})
			sessOrch := &agent.Orchestrator{
				Registry:        sessReg,
				MaxParallel:     orch.MaxParallel,
				DefaultTimeout:  orch.DefaultTimeout,
				PerToolTimeouts: orch.PerToolTimeouts,
				MaxResultBytes:  orch.MaxResultBytes,
			}
			loop.Orchestrator = sessOrch
		}
		loop.Registry = sessReg
		// Surface what the allowlist drops, so a tool missing from the model's
		// schema is diagnosable from the log instead of silently absent.
		if dropped := allowlistDroppedTools(sessReg, sess.AllowedTools()); len(dropped) > 0 {
			logger.Debug("session: tools filtered by allowed_tools",
				"session", sess.ID, "dropped", dropped)
		}
		loop.Compactor = &agent.Compactor{
			Provider:   fast,
			Model:      fastModelID,
			RecentKeep: cfg.Agent.CompactRecentKeep,
		}

		// Implicit-trigger interceptor: only attached to top-level sessions.
		// Child sessions (Dispatch-spawned subagents) MUST NOT recursively
		// trigger adapter generation — that would risk infinite loops.
		if sess.ParentID == "" && cfg.ExternalAgents.Generation.ImplicitTriggerEnabled {
			if t, ok := sessReg.Get("agent_setup"); ok {
				loop.ImplicitTriggerInterceptor = agent.MakeImplicitTriggerHook(agent.ImplicitTriggerConfig{
					Enabled:   true,
					SetupTool: t,
					Env:       loop.ToolEnv,
				})
			}
		}

		// Per-session AllowedTools filters the schema list the LLM sees.
		loop.Tools = buildProviderToolSchemas(sessReg, sess.AllowedTools())
		return loop
	}
}

// buildDispatchAgentHints lists the agent_type roles the orchestrator can pass
// to the Dispatch tool. adapter-generator is excluded: it is an internal,
// locked-down role driven by the agent_setup flow, not a general delegation
// target. Multi-line descriptions are flattened to a single line so the
// <environment> block stays one entry per row.
func buildDispatchAgentHints(loader *coord.Loader) []prompt.SubAgentHint {
	types, err := loader.List()
	if err != nil {
		slog.Warn("dispatch agent hints: list agent types failed", "error", err)
		return nil
	}
	out := make([]prompt.SubAgentHint, 0, len(types))
	for _, at := range types {
		if at.Name == coord.AdapterGeneratorTypeName {
			continue
		}
		out = append(out, prompt.SubAgentHint{
			Name:        at.Name,
			Description: strings.Join(strings.Fields(at.Description), " "),
		})
	}
	return out
}

func buildSubAgentHints(adapters []*extagent.Adapter) []prompt.SubAgentHint {
	out := make([]prompt.SubAgentHint, len(adapters))
	for i, a := range adapters {
		out[i] = prompt.SubAgentHint{
			Name:        a.Name,
			Description: a.Description,
			Resumable:   a.Session.SupportsResume,
		}
	}
	return out
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

// adapterGeneratorDispatcher implements agentsetup.Dispatcher by invoking the
// registered Dispatch tool with agent_type pinned to adapter-generator.
type adapterGeneratorDispatcher struct {
	tool dispatch.Tool
}

func newAdapterGeneratorDispatcher(t dispatch.Tool) *adapterGeneratorDispatcher {
	return &adapterGeneratorDispatcher{tool: t}
}

// Dispatch invokes the underlying Dispatch tool. The env map is forwarded
// onto the child session's Env so the genbash install-prefix hint (set per
// call by agent_setup) reaches the generator subagent's restricted Bash.
func (d *adapterGeneratorDispatcher) Dispatch(ctx context.Context, parentSessionID, p, model string, env map[string]string) (string, error) {
	in := dispatch.DispatchInput{
		Prompt:    p,
		AgentType: coord.AdapterGeneratorTypeName,
		Mode:      "blocking",
		Model:     model,
		Env:       env,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	res, err := d.tool.Run(ctx, &tools.Env{SessionID: parentSessionID}, raw)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("dispatch failed: %s", res.Output)
	}
	return res.Output, nil
}

// loadBuiltinAdapterExamples returns the embedded adapter YAMLs as few-shot
// examples for the AdapterGeneration template. Reads directly from the
// embedded FS via extagent.BuiltinAdapterNames + BuiltinAdapterYAML — the
// previous implementation called loader.Load(os.TempDir()) which exposed
// the generator's prompt to any attacker-placed YAML under /tmp.
func loadBuiltinAdapterExamples(logger *slog.Logger) []prompt.AdapterGenerationExample {
	names := extagent.BuiltinAdapterNames()
	out := make([]prompt.AdapterGenerationExample, 0, len(names))
	for _, name := range names {
		body, ok := extagent.BuiltinAdapterYAML(name)
		if !ok {
			logger.Warn("agent_setup: missing embedded adapter YAML", "name", name)
			continue
		}
		out = append(out, prompt.AdapterGenerationExample{Name: name, Body: string(body)})
	}
	return out
}

// sessionDedupClearer implements approval.DedupClearer by walking the
// session manager to find the originating session and clearing its
// adapter-setup dedup entry. On approval the entry is removed so the next
// retry sees the adapter as freshly available.
type sessionDedupClearer struct {
	manager *session.Manager
}

func (c *sessionDedupClearer) ClearImplicitTriggerDedup(sessionID, agentName string) {
	if c == nil || c.manager == nil || sessionID == "" {
		return
	}
	sess, err := c.manager.GetSession(sessionID)
	if err != nil {
		return
	}
	sess.SetAdapterSetupState(agentName, "")
}

func (c *sessionDedupClearer) MarkAdapterSetupUnavailable(sessionID, agentName, _ string) {
	if c == nil || c.manager == nil || sessionID == "" {
		return
	}
	sess, err := c.manager.GetSession(sessionID)
	if err != nil {
		return
	}
	sess.SetAdapterSetupState(agentName, "unavailable")
}

// draftPublisher adapts the draft.Publisher to the approval.Publisher
// interface. The approval manager calls Publish on approve; this wrapper
// builds the GenmetaPayload from the provenance fields the manager carries.
type draftPublisher struct {
	liveDir string
}

func (p *draftPublisher) Publish(draftPath string, prov approval.Provenance) (string, error) {
	pub := &draft.Publisher{LiveDir: p.liveDir}
	return pub.Publish(draftPath, draft.GenmetaPayload{
		GeneratedBy:   prov.GeneratedBy,
		GeneratedAt:   prov.GeneratedAt,
		ToolVersion:   prov.ToolVersion,
		Binary:        prov.Binary,
		Prompt:        prov.Prompt,
		HelpOutput:    prov.HelpOutput,
		VersionOutput: prov.VersionOutput,
		ManOutput:     prov.ManOutput,
	})
}

// registryInjector implements approval.RegistryInjector by re-parsing the
// just-published adapter and injecting it into the live extagent.Registry
// snapshot. The session ID is currently ignored: snapshots are shared
// process-wide for the registry, so injecting once makes the adapter visible
// to every live session that asks. SmokePassed flows from the approval flow's
// pre-publish smoke result so the adapter immediately satisfies IsHealthy()
// and ExternalAgent.Run accepts it.
type registryInjector struct {
	registry *extagent.Registry
	logger   *slog.Logger
}

func (r *registryInjector) Inject(_ /* sessionID */, _ /* adapterName */, publishedPath string, smokePassed bool) {
	if r == nil || r.registry == nil || publishedPath == "" {
		return
	}
	raw, err := os.ReadFile(publishedPath)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("registryInjector: read published adapter", "path", publishedPath, "err", err)
		}
		return
	}
	adapter, err := extagent.Parse(raw)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("registryInjector: parse published adapter", "path", publishedPath, "err", err)
		}
		return
	}
	adapter.SmokePassed = smokePassed
	if !smokePassed {
		adapter.SmokeError = "smoke failed pre-approval (still injected per design G5)"
	}
	r.registry.Snapshot().Inject(adapter)
}

// permissionMarkApprover implements approval.MarkApprover by pre-populating a
// session-scoped allow rule on the permission manager so the next
// ExternalAgent invocation against this adapter in the originating session
// skips the first-invocation prompt (per the modified external-agents spec).
type permissionMarkApprover struct {
	mgr *permission.Manager
}

func (p *permissionMarkApprover) MarkApproved(sessionID, agentName string) {
	if p == nil || p.mgr == nil {
		return
	}
	p.mgr.GrantSession(sessionID, "ExternalAgent", agentName)
}

// editValidator implements approval.EditValidator by re-validating an edited
// adapter YAML against the schema + the dangerous-argument scan. Smoke is
// NOT re-run inline (it can take up to a minute and the approval flow holds
// the manager mutex); the eventual approve path re-publishes through
// draft.Publish which itself re-parses the YAML.
type editValidator struct {
	liveDir string
}

func (v *editValidator) ValidateEdit(_ string, editedYAML []byte) error {
	adapter, err := extagent.Parse(editedYAML)
	if err != nil {
		return fmt.Errorf("schema validation: %w", err)
	}
	for _, arg := range adapter.Invocation.ExtraArgs {
		for _, pat := range dangerousAdapterArgPatterns {
			if strings.Contains(arg, pat) {
				return fmt.Errorf("dangerous pattern %q in invocation.extra_args[%q]", pat, arg)
			}
		}
	}
	for k, val := range adapter.Invocation.EnvOverride {
		for _, pat := range dangerousAdapterArgPatterns {
			if strings.Contains(k, pat) || strings.Contains(val, pat) {
				return fmt.Errorf("dangerous pattern %q in env_override", pat)
			}
		}
	}
	return nil
}

// dangerousAdapterArgPatterns mirrors agentsetup.dangerousArgPatterns. Kept
// here so the editValidator doesn't import the agentsetup package (which
// would create a cycle: agentsetup → approval → editValidator → agentsetup).
var dangerousAdapterArgPatterns = []string{
	"rm -rf /", "rm -rf ~", "dd of=/dev/", "mkfs.", "chmod -R 777 /",
	"shutdown", "reboot", "halt", "poweroff",
	"base64 -d | sh", "curl | bash",
}

// loadAdapterSchemaJSON returns the embedded adapter JSON schema as a string,
// for interpolation into the generator's system prompt.
func loadAdapterSchemaJSON(logger *slog.Logger) string {
	body, ok := extagent.AdapterSchemaJSON()
	if !ok {
		logger.Warn("agent_setup: adapter schema embed missing")
		return "{}"
	}
	return string(body)
}

func apiConfigFrom(cfg config.Config) api.Config {
	bytesLimit := int64(cfg.Server.MaxRequestBodyBytes)
	if bytesLimit <= 0 {
		bytesLimit = 1 << 20
	}
	return api.Config{
		Host:                    cfg.Server.Host,
		Port:                    cfg.Server.Port,
		DefaultWorkdir:          cfg.Server.DefaultWorkdir,
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
		DefaultAllowedTools:     cfg.Tools.DefaultAllowedTools,
		Auth: api.BearerConfig{
			Enabled: cfg.Auth.Enabled,
			Token:   cfg.Auth.BearerToken,
		},
		MaxConcurrentSessions: cfg.Sessions.MaxConcurrent,
		MaxHistoryTokens:      cfg.Agent.MaxHistoryTokens,
		Version:               versionString(),
	}
}

// presetIDPrefix tags permission rows that originate from config preset_rules
// so they are self-identifying: the API reports their source and startup
// reconciliation can drop ones no longer declared in config. Manual rules use
// the "perm-" prefix instead, so the two never collide.
const presetIDPrefix = "preset-"

// applyPresetRules reconciles the configured preset_rules into the store at
// startup. The deterministic ID derives from (tool, pattern) only — excluding
// decision — so retightening a preset's decision REPLACEs the same row instead
// of leaving the old grant behind. Presets dropped from config are deleted so
// they don't linger as stale grants.
func applyPresetRules(ctx context.Context, st *sqlite.Store, rules []config.PresetRule, logger *slog.Logger) error {
	desired := make(map[string]struct{}, len(rules))
	for _, r := range rules {
		id := presetRuleID(r.Tool, r.Pattern)
		desired[id] = struct{}{}
		if err := st.SavePermission(ctx, &store.Permission{
			ID:        id,
			SessionID: "",
			Tool:      r.Tool,
			Pattern:   r.Pattern,
			Decision:  store.PermissionDecision(r.Decision),
			Scope:     store.ScopePermanent,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			return fmt.Errorf("preset rule %q: %w", id, err)
		}
	}

	// Drop preset rows no longer declared in config (removed or retightened to
	// a different (tool, pattern)). Global permanent rules carry session_id="".
	existing, err := st.ListPermissions(ctx, "")
	if err != nil {
		return fmt.Errorf("list permissions: %w", err)
	}
	removed := 0
	for _, p := range existing {
		if !strings.HasPrefix(p.ID, presetIDPrefix) {
			continue
		}
		if _, ok := desired[p.ID]; ok {
			continue
		}
		if err := st.DeletePermission(ctx, p.ID); err != nil {
			return fmt.Errorf("remove stale preset %q: %w", p.ID, err)
		}
		removed++
	}

	if len(rules) > 0 || removed > 0 {
		logger.Info("applied preset permission rules", "count", len(rules), "removed_stale", removed)
	}
	return nil
}

// presetRuleID returns a deterministic permission ID for a preset rule, keyed
// on (tool, pattern) so a decision change updates the same row in place.
func presetRuleID(tool, pattern string) string {
	h := sha256.Sum256([]byte(tool + "\x00" + pattern))
	return presetIDPrefix + hex.EncodeToString(h[:])[:16]
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
