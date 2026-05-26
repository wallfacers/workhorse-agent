package config

import (
	"errors"
	"fmt"
	"path"
)

// Validate enforces the numeric ranges and string enums listed in the
// configuration spec ("完整 Config Schema" requirement). The first failing
// rule is reported; callers print the message verbatim and exit non-zero.
func Validate(c Config) error {
	if err := validateServer(c.Server); err != nil {
		return err
	}
	if err := validateAuth(c.Auth); err != nil {
		return err
	}
	if err := validateProviders(c.Providers); err != nil {
		return err
	}
	if err := validateAgent(c.Agent); err != nil {
		return err
	}
	if err := validateTools(c.Tools); err != nil {
		return err
	}
	if err := validateSessions(c.Sessions); err != nil {
		return err
	}
	if err := validateLogging(c.Logging); err != nil {
		return err
	}
	if err := validateStore(c.Store); err != nil {
		return err
	}
	return nil
}

type rangeIntRule struct {
	field    string
	value    int
	min, max int
}

func (r rangeIntRule) check() error {
	if r.value < r.min || r.value > r.max {
		return fmt.Errorf("invalid config: %s must be %d-%d, got %d",
			r.field, r.min, r.max, r.value)
	}
	return nil
}

func validateServer(s ServerConfig) error {
	if s.Host == "" {
		return errors.New("invalid config: server.host must not be empty")
	}
	rules := []rangeIntRule{
		{"server.port", s.Port, 1, 65535},
		{"server.read_header_timeout_seconds", s.ReadHeaderTimeoutSeconds, 1, 60},
		{"server.read_timeout_seconds", s.ReadTimeoutSeconds, 5, 3600},
		{"server.idle_timeout_seconds", s.IdleTimeoutSeconds, 10, 3600},
		{"server.max_header_bytes", s.MaxHeaderBytes, 4096, 16 * 1024 * 1024},
		{"server.max_request_body_bytes", s.MaxRequestBodyBytes, 1024, 100 * 1024 * 1024},
		{"server.graceful_shutdown_timeout_seconds", s.GracefulShutdownTimeoutSeconds, 1, 600},
		{"server.sse_keepalive_seconds", s.SSEKeepaliveSeconds, 5, 300},
	}
	for _, r := range rules {
		if err := r.check(); err != nil {
			return err
		}
	}
	return nil
}

func validateAuth(a AuthConfig) error {
	if a.Enabled && a.BearerToken == "" {
		return errors.New("invalid config: auth.bearer_token must be set when auth.enabled is true")
	}
	return nil
}

func validateProviders(p ProvidersConfig) error {
	switch p.Default {
	case "anthropic", "openai":
	default:
		return fmt.Errorf("invalid config: providers.default must be one of [anthropic openai], got %q", p.Default)
	}
	if p.Anthropic.BaseURL == "" {
		return errors.New("invalid config: providers.anthropic.base_url must not be empty")
	}
	if p.OpenAI.BaseURL == "" {
		return errors.New("invalid config: providers.openai.base_url must not be empty")
	}
	return nil
}

func validateAgent(a AgentConfig) error {
	intRules := []rangeIntRule{
		{"agent.max_parallel_tools", a.MaxParallelTools, 1, 100},
		{"agent.max_depth", a.MaxDepth, 1, 20},
		{"agent.compact_recent_keep", a.CompactRecentKeep, 1, 100},
		{"agent.max_history_tokens", a.MaxHistoryTokens, 1000, 10_000_000},
		{"agent.permission_request_timeout_seconds", a.PermissionRequestTimeoutSeconds, 5, 3600},
		{"agent.cancel_drain_timeout_seconds", a.CancelDrainTimeoutSeconds, 1, 60},
	}
	for _, r := range intRules {
		if err := r.check(); err != nil {
			return err
		}
	}
	if a.AutoCompactRatio < 0.5 || a.AutoCompactRatio > 0.99 {
		return fmt.Errorf("invalid config: agent.auto_compact_ratio must be 0.5-0.99, got %g", a.AutoCompactRatio)
	}
	if a.ProviderRetryAttempts < 0 || a.ProviderRetryAttempts > 10 {
		return fmt.Errorf("invalid config: agent.provider_retry_attempts must be 0-10, got %d", a.ProviderRetryAttempts)
	}
	// Backoff slice must cover every attempt so the loop never indexes past the end.
	if len(a.ProviderRetryBackoffMs) < a.ProviderRetryAttempts {
		return fmt.Errorf("invalid config: agent.provider_retry_backoff_ms must have at least %d entries to cover provider_retry_attempts, has %d",
			a.ProviderRetryAttempts, len(a.ProviderRetryBackoffMs))
	}
	for i, v := range a.ProviderRetryBackoffMs {
		if v < 0 || v > 5*60*1000 {
			return fmt.Errorf("invalid config: agent.provider_retry_backoff_ms[%d] must be 0-300000, got %d", i, v)
		}
	}
	return nil
}

func validateTools(t ToolsConfig) error {
	rules := []rangeIntRule{
		{"tools.default_timeout_seconds", t.DefaultTimeoutSeconds, 1, 3600},
		{"tools.tool_result_max_bytes", t.ToolResultMaxBytes, 1024, 100 * 1024 * 1024},
		{"tools.bash.timeout_seconds", t.Bash.TimeoutSeconds, 1, 3600},
	}
	for _, r := range rules {
		if err := r.check(); err != nil {
			return err
		}
	}
	if t.Read.TimeoutSeconds < 0 || t.Read.TimeoutSeconds > 3600 {
		return fmt.Errorf("invalid config: tools.read.timeout_seconds must be 0-3600 (0 means inherit default), got %d", t.Read.TimeoutSeconds)
	}
	if t.Grep.TimeoutSeconds < 0 || t.Grep.TimeoutSeconds > 3600 {
		return fmt.Errorf("invalid config: tools.grep.timeout_seconds must be 0-3600, got %d", t.Grep.TimeoutSeconds)
	}
	if t.Grep.Workers < 0 || t.Grep.Workers > 256 {
		return fmt.Errorf("invalid config: tools.grep.workers must be 0-256 (0 means runtime.NumCPU()), got %d", t.Grep.Workers)
	}
	for i, pat := range t.Grep.DefaultExcludes {
		if _, err := path.Match(pat, "x"); err != nil {
			return fmt.Errorf("invalid config: tools.grep.default_excludes[%d] is not a valid glob: %v (pattern: %q)", i, err, pat)
		}
	}
	return nil
}

func validateSessions(s SessionsConfig) error {
	return rangeIntRule{"sessions.max_concurrent", s.MaxConcurrent, 1, 10_000}.check()
}

func validateLogging(l LoggingConfig) error {
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid config: logging.level must be one of [debug info warn error], got %q", l.Level)
	}
	switch l.Format {
	case "json", "text":
	default:
		return fmt.Errorf("invalid config: logging.format must be one of [json text], got %q", l.Format)
	}
	return nil
}

func validateStore(s StoreConfig) error {
	if s.Path == "" {
		return errors.New("invalid config: store.path must not be empty")
	}
	if s.BusyTimeoutMs < 0 || s.BusyTimeoutMs > 60_000 {
		return fmt.Errorf("invalid config: store.busy_timeout_ms must be 0-60000, got %d", s.BusyTimeoutMs)
	}
	return nil
}
