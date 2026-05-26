package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/config"
)

func TestDefault_PassesValidation(t *testing.T) {
	if err := config.Validate(config.Default()); err != nil {
		t.Fatalf("built-in defaults must validate, got: %v", err)
	}
}

func TestLoad_YAMLOverridesDefaults(t *testing.T) {
	path := writeYAML(t, `
server:
  port: 12345
`)
	cfg, err := config.Load(config.LoadOptions{
		YAMLPath:  path,
		LookupEnv: emptyEnv,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Port != 12345 {
		t.Errorf("port: got %d, want 12345", cfg.Server.Port)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("default host should survive yaml partial: got %q", cfg.Server.Host)
	}
}

// Scenario from spec: 环境变量覆盖配置文件
func TestLoad_EnvOverridesYAML(t *testing.T) {
	path := writeYAML(t, `
server:
  port: 7821
`)
	cfg, err := config.Load(config.LoadOptions{
		YAMLPath: path,
		LookupEnv: stubEnv(map[string]string{
			"WORKHORSE_AGENT_PORT": "9000",
		}),
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Port != 9000 {
		t.Errorf("port: got %d, want 9000 (env override)", cfg.Server.Port)
	}
}

// Scenario from spec: 命令行覆盖环境变量
func TestLoad_CLIOverridesEnv(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{
		Args: []string{"--port", "8000"},
		LookupEnv: stubEnv(map[string]string{
			"WORKHORSE_AGENT_PORT": "9000",
		}),
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Port != 8000 {
		t.Errorf("port: got %d, want 8000 (cli override)", cfg.Server.Port)
	}
}

// Scenario from spec: 非法端口拒绝启动
func TestLoad_RejectsOutOfRangePort(t *testing.T) {
	path := writeYAML(t, `
server:
  port: 70000
`)
	_, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err == nil {
		t.Fatal("expected validation error for port=70000")
	}
	if !strings.Contains(err.Error(), "server.port must be 1-65535, got 70000") {
		t.Errorf("error message does not match spec scenario: %v", err)
	}
}

// Scenario from spec: enabled=true 但 token 为空
func TestLoad_RejectsAuthEnabledWithoutToken(t *testing.T) {
	path := writeYAML(t, `
auth:
  enabled: true
  bearer_token: ""
`)
	_, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err == nil {
		t.Fatal("expected validation error for auth.enabled without token")
	}
	if !strings.Contains(err.Error(), "auth.bearer_token must be set when auth.enabled is true") {
		t.Errorf("error message does not match spec scenario: %v", err)
	}
}

// Scenario from spec: sse_keepalive_seconds 超出范围
func TestLoad_RejectsSSEKeepaliveZero(t *testing.T) {
	path := writeYAML(t, `
server:
  sse_keepalive_seconds: 0
`)
	_, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err == nil {
		t.Fatal("expected validation error for sse_keepalive_seconds=0")
	}
	if !strings.Contains(err.Error(), "server.sse_keepalive_seconds must be 5-300, got 0") {
		t.Errorf("error message does not match spec scenario: %v", err)
	}
}

func TestLoad_RejectsUnknownYAMLKey(t *testing.T) {
	path := writeYAML(t, `
server:
  hostz: 0.0.0.0
`)
	_, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err == nil {
		t.Fatal("expected error for unknown yaml key 'hostz'")
	}
	if !strings.Contains(err.Error(), "hostz") {
		t.Errorf("unknown-key error should mention the offending field, got: %v", err)
	}
}

func TestLoad_MissingYAMLFallsBackToDefaults(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{
		YAMLPath:  filepath.Join(t.TempDir(), "does-not-exist.yaml"),
		LookupEnv: emptyEnv,
	})
	if err != nil {
		t.Fatalf("missing yaml should be tolerated, got: %v", err)
	}
	if cfg.Server.Port != 7821 {
		t.Errorf("default port: got %d, want 7821", cfg.Server.Port)
	}
}

func TestLoad_RejectsBackoffShorterThanAttempts(t *testing.T) {
	path := writeYAML(t, `
agent:
  provider_retry_attempts: 5
  provider_retry_backoff_ms: [500, 2000]
`)
	_, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err == nil {
		t.Fatal("expected error: backoff slice shorter than attempts")
	}
}

func TestLoad_RejectsUnknownProvider(t *testing.T) {
	path := writeYAML(t, `
providers:
  default: gemini
`)
	_, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestLoad_AuthBearerTokenViaEnv(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{
		LookupEnv: stubEnv(map[string]string{
			"WORKHORSE_AGENT_AUTH_ENABLED":      "true",
			"WORKHORSE_AGENT_AUTH_BEARER_TOKEN": "s3cret-token",
		}),
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Auth.Enabled || cfg.Auth.BearerToken != "s3cret-token" {
		t.Errorf("auth state not applied: %+v", cfg.Auth)
	}
}

func TestLoad_RejectsBadEnvBool(t *testing.T) {
	_, err := config.Load(config.LoadOptions{
		LookupEnv: stubEnv(map[string]string{
			"WORKHORSE_AGENT_AUTH_ENABLED": "maybe",
		}),
	})
	if err == nil {
		t.Fatal("expected error for invalid boolean env value")
	}
}

func TestExpandPath_HomeRelative(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}
	got, err := config.ExpandPath("~/data.db")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := filepath.Join(home, "data.db")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandPath_EmptyStaysEmpty(t *testing.T) {
	got, err := config.ExpandPath("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("empty input should stay empty, got %q", got)
	}
}

func TestLoad_ResolveHomePaths(t *testing.T) {
	home, _ := os.UserHomeDir()
	cfg, err := config.Load(config.LoadOptions{
		LookupEnv:        emptyEnv,
		ResolveHomePaths: true,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.HasPrefix(cfg.Store.Path, home) {
		t.Errorf("store.path should be expanded under %q, got %q", home, cfg.Store.Path)
	}
	if !strings.HasPrefix(cfg.Skills.Dir, home) {
		t.Errorf("skills.dir should be expanded under %q, got %q", home, cfg.Skills.Dir)
	}
}

// Scenario from speed-up-grep: 默认值正确装配
func TestDefault_GrepWorkers(t *testing.T) {
	c := config.Default()
	if c.Tools.Grep.Workers != 0 {
		t.Errorf("default workers must be 0 (= min(runtime.NumCPU(), 8)), got %d", c.Tools.Grep.Workers)
	}
	if !c.Tools.Grep.RespectGitignore {
		t.Errorf("default respect_gitignore must be true")
	}
	if c.Tools.Grep.DefaultExcludes != nil {
		t.Errorf("default default_excludes must be nil (use builtin list), got %v", c.Tools.Grep.DefaultExcludes)
	}
}

// Scenario from speed-up-grep configuration spec: yaml 解析新键
func TestLoad_GrepKeysFromYAML(t *testing.T) {
	path := writeYAML(t, `
tools:
  grep:
    workers: 4
    respect_gitignore: false
    default_excludes: ["only_this", "*.bin"]
`)
	cfg, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Tools.Grep.Workers != 4 {
		t.Errorf("workers: got %d, want 4", cfg.Tools.Grep.Workers)
	}
	if cfg.Tools.Grep.RespectGitignore {
		t.Errorf("respect_gitignore: got true, want false")
	}
	if len(cfg.Tools.Grep.DefaultExcludes) != 2 ||
		cfg.Tools.Grep.DefaultExcludes[0] != "only_this" ||
		cfg.Tools.Grep.DefaultExcludes[1] != "*.bin" {
		t.Errorf("default_excludes: got %v", cfg.Tools.Grep.DefaultExcludes)
	}
}

// Scenario: tools.grep.workers 越界启动失败
func TestLoad_RejectsGrepWorkersOutOfRange(t *testing.T) {
	path := writeYAML(t, `
tools:
  grep:
    workers: 1000
`)
	_, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err == nil {
		t.Fatal("expected workers=1000 to fail validation")
	}
	if !strings.Contains(err.Error(), "tools.grep.workers") {
		t.Errorf("error message should mention tools.grep.workers, got: %v", err)
	}
}

// Scenario: 非法 default_excludes glob 启动失败
func TestLoad_RejectsInvalidGrepDefaultExcludes(t *testing.T) {
	path := writeYAML(t, `
tools:
  grep:
    default_excludes: ["[bad"]
`)
	_, err := config.Load(config.LoadOptions{YAMLPath: path, LookupEnv: emptyEnv})
	if err == nil {
		t.Fatal("expected invalid glob to fail validation")
	}
	if !strings.Contains(err.Error(), "default_excludes[0]") {
		t.Errorf("error should pinpoint the offending entry, got: %v", err)
	}
}

// helpers

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

func emptyEnv(string) (string, bool) { return "", false }

func stubEnv(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}
