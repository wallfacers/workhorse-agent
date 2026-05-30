// Package config defines the workhorse-agent runtime configuration: a single Config
// struct, the built-in defaults, range/relationship validation, and the load
// pipeline that merges defaults < yaml < env < CLI flags.
package config

// Config is the full runtime configuration tree. Every field maps 1:1 to the
// schema in openspec/changes/init-workhorse-agent-mvp/specs/configuration/spec.md.
type Config struct {
	Server         ServerConfig         `yaml:"server"`
	Auth           AuthConfig           `yaml:"auth"`
	Providers      ProvidersConfig      `yaml:"providers"`
	Models         ModelsConfig         `yaml:"models"`
	Agent          AgentConfig          `yaml:"agent"`
	Tools          ToolsConfig          `yaml:"tools"`
	Store          StoreConfig          `yaml:"store"`
	Sessions       SessionsConfig       `yaml:"sessions"`
	MCP            MCPConfig            `yaml:"mcp"`
	Skills         PathConfig           `yaml:"skills"`
	Agents         PathConfig           `yaml:"agents"`
	Memory         MemoryConfig         `yaml:"memory"`
	ExternalAgents ExternalAgentsConfig `yaml:"external_agents"`
	Logging        LoggingConfig        `yaml:"logging"`
	Debug          DebugConfig          `yaml:"debug"`
}

type ServerConfig struct {
	Host                           string   `yaml:"host"`
	Port                           int      `yaml:"port"`
	ReadHeaderTimeoutSeconds       int      `yaml:"read_header_timeout_seconds"`
	ReadTimeoutSeconds             int      `yaml:"read_timeout_seconds"`
	IdleTimeoutSeconds             int      `yaml:"idle_timeout_seconds"`
	MaxHeaderBytes                 int      `yaml:"max_header_bytes"`
	MaxRequestBodyBytes            int      `yaml:"max_request_body_bytes"`
	GracefulShutdownTimeoutSeconds int      `yaml:"graceful_shutdown_timeout_seconds"`
	SSEKeepaliveSeconds            int      `yaml:"sse_keepalive_seconds"`
	AllowedOrigins                 []string `yaml:"allowed_origins"`
	AllowNullOrigin                bool     `yaml:"allow_null_origin"`
}

type AuthConfig struct {
	Enabled     bool   `yaml:"enabled"`
	BearerToken string `yaml:"bearer_token"`
}

type ProvidersConfig struct {
	Default   string           `yaml:"default"`
	Anthropic ProviderEndpoint `yaml:"anthropic"`
	OpenAI    ProviderEndpoint `yaml:"openai"`
}

type ProviderEndpoint struct {
	APIKey    string `yaml:"api_key"`
	BaseURL   string `yaml:"base_url"`
	FastModel string `yaml:"fast_model"`
}

type ModelsConfig struct {
	Default       string            `yaml:"default"`
	Fast          string            `yaml:"fast"`
	BySessionType map[string]string `yaml:"by_session_type"`
}

type AgentConfig struct {
	MaxTokens                       int            `yaml:"max_tokens"`
	MaxParallelTools                int            `yaml:"max_parallel_tools"`
	MaxDepth                        int            `yaml:"max_depth"`
	AutoCompactRatio                float64        `yaml:"auto_compact_ratio"`
	CompactRecentKeep               int            `yaml:"compact_recent_keep"`
	MaxHistoryTokens                int            `yaml:"max_history_tokens"`
	PermissionRequestTimeoutSeconds int            `yaml:"permission_request_timeout_seconds"`
	CancelDrainTimeoutSeconds       int            `yaml:"cancel_drain_timeout_seconds"`
	ProviderRetryAttempts           int            `yaml:"provider_retry_attempts"`
	ProviderRetryBackoffMs          []int          `yaml:"provider_retry_backoff_ms"`
	Thinking                        ThinkingConfig `yaml:"thinking"`
}

type ThinkingConfig struct {
	Enabled      bool `yaml:"enabled"`
	BudgetTokens int  `yaml:"budget_tokens"`
}

type ToolsConfig struct {
	DefaultTimeoutSeconds int         `yaml:"default_timeout_seconds"`
	ToolResultMaxBytes    int         `yaml:"tool_result_max_bytes"`
	Bash                  ToolTimeout `yaml:"bash"`
	Read                  ToolTimeout `yaml:"read"`
	Grep                  ToolsGrep   `yaml:"grep"`
	DefaultAllowedTools   []string    `yaml:"default_allowed_tools"`
}

type ToolTimeout struct {
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// ToolsGrep configures the Grep tool. Beyond the shared timeout it carries
// the gitignore + parallel-walker knobs documented in
// openspec/changes/speed-up-grep/specs/configuration/spec.md.
type ToolsGrep struct {
	TimeoutSeconds   int      `yaml:"timeout_seconds"`
	Workers          int      `yaml:"workers"`           // 0 = min(runtime.NumCPU(), 8); 1 = serial codepath
	RespectGitignore bool     `yaml:"respect_gitignore"` // overridden by Grep input.ignore_vcs
	DefaultExcludes  []string `yaml:"default_excludes"`  // nil/empty = builtin list; non-empty = full replacement
}

type StoreConfig struct {
	Path          string `yaml:"path"`
	BusyTimeoutMs int    `yaml:"busy_timeout_ms"`
}

type SessionsConfig struct {
	MaxConcurrent int `yaml:"max_concurrent"`
}

type MCPConfig struct {
	ConfigPath string `yaml:"config_path"`
}

type PathConfig struct {
	Dir string `yaml:"dir"`
}

type LoggingConfig struct {
	Level         string `yaml:"level"`
	Format        string `yaml:"format"`
	LogLLMPayload bool   `yaml:"log_llm_payload"`
}

type DebugConfig struct {
	Enabled bool `yaml:"enabled"`
}

type MemoryConfig struct {
	Dir             string `yaml:"dir"`
	MemoryCharLimit int    `yaml:"memory_char_limit"`
	UserCharLimit   int    `yaml:"user_char_limit"`
}

type ExternalAgentsConfig struct {
	Dir        string                         `yaml:"dir"`
	SmokeTest  ExternalAgentsSmokeTestConfig  `yaml:"smoke_test"`
	PathScan   ExternalAgentsPathScanConfig   `yaml:"pathscan"`
	Driver     ExternalAgentsDriverConfig     `yaml:"driver"`
	Generation ExternalAgentsGenerationConfig `yaml:"generation"`
}

type ExternalAgentsSmokeTestConfig struct {
	CacheTTL int `yaml:"cache_ttl"` // hours; default 168 (7 days)
}

type ExternalAgentsPathScanConfig struct {
	CacheTTL int      `yaml:"cache_ttl"` // hours; default 24
	Extra    []string `yaml:"extra"`
	Disabled []string `yaml:"disabled"`
}

type ExternalAgentsDriverConfig struct {
	KillOnOutputCap bool `yaml:"kill_on_output_cap"`
}

type ExternalAgentsGenerationConfig struct {
	Enabled                bool     `yaml:"enabled"`
	ApprovalTimeoutSec     int      `yaml:"approval_timeout_sec"`
	ImplicitTriggerEnabled bool     `yaml:"implicit_trigger_enabled"`
	AllowedModels          []string `yaml:"allowed_models"`
}

// Default returns a fully-populated Config using the built-in defaults
// described in the configuration spec.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Host:                           "127.0.0.1",
			Port:                           7821,
			ReadHeaderTimeoutSeconds:       10,
			ReadTimeoutSeconds:             60,
			IdleTimeoutSeconds:             120,
			MaxHeaderBytes:                 1 << 20,
			MaxRequestBodyBytes:            1 << 20,
			GracefulShutdownTimeoutSeconds: 30,
			SSEKeepaliveSeconds:            25,
			AllowedOrigins:                 nil,
			AllowNullOrigin:                false,
		},
		Auth: AuthConfig{Enabled: false, BearerToken: ""},
		Providers: ProvidersConfig{
			Default: "anthropic",
			Anthropic: ProviderEndpoint{
				BaseURL:   "https://api.anthropic.com",
				FastModel: "claude-haiku-4-5-20251001",
			},
			OpenAI: ProviderEndpoint{
				BaseURL:   "https://api.openai.com/v1",
				FastModel: "gpt-4o-mini",
			},
		},
		Models: ModelsConfig{
			Default: "anthropic:claude-sonnet-4-6",
			Fast:    "anthropic:claude-haiku-4-5-20251001",
		},
		Agent: AgentConfig{
			MaxTokens:                       4096,
			MaxParallelTools:                10,
			MaxDepth:                        5,
			AutoCompactRatio:                0.85,
			CompactRecentKeep:               8,
			MaxHistoryTokens:                200_000,
			PermissionRequestTimeoutSeconds: 300,
			CancelDrainTimeoutSeconds:       5,
			ProviderRetryAttempts:           3,
			ProviderRetryBackoffMs:          []int{500, 2000, 8000},
		},
		Tools: ToolsConfig{
			DefaultTimeoutSeconds: 60,
			ToolResultMaxBytes:    1 << 20,
			Bash:                  ToolTimeout{TimeoutSeconds: 120},
			Read:                  ToolTimeout{TimeoutSeconds: 30},
			Grep: ToolsGrep{
				TimeoutSeconds:   60,
				Workers:          0,
				RespectGitignore: true,
				DefaultExcludes:  nil,
			},
		},
		Store:    StoreConfig{Path: "~/.workhorse-agent/state.db", BusyTimeoutMs: 5000},
		Sessions: SessionsConfig{MaxConcurrent: 50},
		MCP:      MCPConfig{ConfigPath: "~/.workhorse-agent/mcp.json"},
		Skills:   PathConfig{Dir: "~/.workhorse-agent/skills"},
		Agents:   PathConfig{Dir: "~/.workhorse-agent/agents"},
		Logging:  LoggingConfig{Level: "info", Format: "json", LogLLMPayload: false},
		Memory:   MemoryConfig{MemoryCharLimit: 2200, UserCharLimit: 1375},
		ExternalAgents: ExternalAgentsConfig{
			Dir:       "~/.workhorse-agent/external-agents",
			SmokeTest: ExternalAgentsSmokeTestConfig{CacheTTL: 168},
			PathScan:  ExternalAgentsPathScanConfig{CacheTTL: 24},
			Driver:    ExternalAgentsDriverConfig{KillOnOutputCap: true},
			Generation: ExternalAgentsGenerationConfig{
				Enabled:                true,
				ApprovalTimeoutSec:     300,
				ImplicitTriggerEnabled: true,
				AllowedModels:          nil,
			},
		},
		Debug: DebugConfig{Enabled: false},
	}
}
