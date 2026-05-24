package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadOptions controls a single Load() invocation. Tests pass an isolated
// environment / args slice so they don't depend on the real process state.
type LoadOptions struct {
	// YAMLPath is the path to config.yaml. Empty means "skip yaml step".
	// When non-empty and the file does not exist, the yaml step is skipped
	// silently (init has not been run yet); other I/O errors are reported.
	YAMLPath string
	// Args are CLI args excluding the program name. Pass os.Args[1:] in main.
	Args []string
	// LookupEnv lets callers stub the environment. nil falls back to
	// os.LookupEnv on the live process environment.
	LookupEnv func(key string) (string, bool)
	// ResolveHomePaths, when true, expands "~/" in store.path,
	// mcp.config_path, skills.dir, and agents.dir to absolute paths.
	ResolveHomePaths bool
}

// Load assembles the final Config by walking the four-source merge:
//
//  1. built-in Default()
//  2. yaml file at LoadOptions.YAMLPath, if present (yaml.v3 keeps fields
//     not mentioned in the file at their existing — default — value)
//  3. environment variables prefixed DATAAGENT_
//  4. recognised CLI flags from LoadOptions.Args
//
// After all four steps the result is validated and any home-relative paths
// are expanded. Validation errors are returned verbatim; main prints them on
// stderr and exits non-zero.
func Load(opts LoadOptions) (Config, error) {
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	cfg := Default()

	if opts.YAMLPath != "" {
		if err := mergeYAMLFile(&cfg, opts.YAMLPath); err != nil {
			return Config{}, err
		}
	}

	if err := applyEnv(&cfg, opts.LookupEnv); err != nil {
		return Config{}, err
	}

	if _, err := applyCLI(&cfg, opts.Args); err != nil {
		return Config{}, err
	}

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}

	if opts.ResolveHomePaths {
		if err := ResolvePaths(&cfg); err != nil {
			return Config{}, err
		}
	}

	return cfg, nil
}

// mergeYAMLFile reads the yaml at path and unmarshals into cfg in place. yaml.v3
// preserves any field not mentioned in the document, so default values flow
// through unchanged.
func mergeYAMLFile(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Spec scenario "config.yaml missing → defaults" is intentional:
			// the operator may have only set env vars or CLI flags.
			return nil
		}
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // file is read-only

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // reject unknown keys to catch typos
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty file == use defaults
		}
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// envMap is the explicit allowlist of environment overrides supported by the
// configuration spec. Anything outside this list must be expressed in
// config.yaml — we don't auto-map every nested field by reflection because the
// resulting names get unwieldy ("DATAAGENT_TOOLS_BASH_TIMEOUT_SECONDS") and
// the spec only requires a handful to be settable via env.
type envBinding struct {
	key   string
	apply func(*Config, string) error
}

func envBindings() []envBinding {
	return []envBinding{
		{"DATAAGENT_HOST", func(c *Config, v string) error {
			c.Server.Host = v
			return nil
		}},
		{"DATAAGENT_PORT", func(c *Config, v string) error {
			p, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("DATAAGENT_PORT: %w", err)
			}
			c.Server.Port = p
			return nil
		}},
		{"DATAAGENT_LOG_LEVEL", func(c *Config, v string) error {
			c.Logging.Level = v
			return nil
		}},
		{"DATAAGENT_LOG_FORMAT", func(c *Config, v string) error {
			c.Logging.Format = v
			return nil
		}},
		{"DATAAGENT_AUTH_ENABLED", func(c *Config, v string) error {
			b, err := parseBoolStrict(v)
			if err != nil {
				return fmt.Errorf("DATAAGENT_AUTH_ENABLED: %w", err)
			}
			c.Auth.Enabled = b
			return nil
		}},
		{"DATAAGENT_AUTH_BEARER_TOKEN", func(c *Config, v string) error {
			c.Auth.BearerToken = v
			return nil
		}},
		{"DATAAGENT_PROVIDERS_DEFAULT", func(c *Config, v string) error {
			c.Providers.Default = v
			return nil
		}},
		{"DATAAGENT_PROVIDERS_ANTHROPIC_API_KEY", func(c *Config, v string) error {
			c.Providers.Anthropic.APIKey = v
			return nil
		}},
		{"DATAAGENT_PROVIDERS_ANTHROPIC_BASE_URL", func(c *Config, v string) error {
			c.Providers.Anthropic.BaseURL = v
			return nil
		}},
		{"DATAAGENT_PROVIDERS_OPENAI_API_KEY", func(c *Config, v string) error {
			c.Providers.OpenAI.APIKey = v
			return nil
		}},
		{"DATAAGENT_PROVIDERS_OPENAI_BASE_URL", func(c *Config, v string) error {
			c.Providers.OpenAI.BaseURL = v
			return nil
		}},
		{"DATAAGENT_DEBUG_ENABLED", func(c *Config, v string) error {
			b, err := parseBoolStrict(v)
			if err != nil {
				return fmt.Errorf("DATAAGENT_DEBUG_ENABLED: %w", err)
			}
			c.Debug.Enabled = b
			return nil
		}},
		{"DATAAGENT_STORE_PATH", func(c *Config, v string) error {
			c.Store.Path = v
			return nil
		}},
		{"DATAAGENT_MCP_CONFIG_PATH", func(c *Config, v string) error {
			c.MCP.ConfigPath = v
			return nil
		}},
	}
}

func applyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	for _, b := range envBindings() {
		v, ok := lookup(b.key)
		if !ok {
			continue
		}
		if err := b.apply(cfg, v); err != nil {
			return err
		}
	}
	return nil
}

// applyCLI handles the small set of flags the spec calls out (--port,
// --host, --config, --log-level). The leftover Args (positional) are
// returned so cmd/dataagent can dispatch to subcommands above this layer.
//
// We intentionally do not expose every config knob as a CLI flag — yaml stays
// the configured source of truth. Flags exist for the spec scenarios and for
// quick local overrides.
func applyCLI(cfg *Config, args []string) ([]string, error) {
	fs := flag.NewFlagSet("dataagent", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we own the error messages

	host := fs.String("host", cfg.Server.Host, "")
	port := fs.Int("port", cfg.Server.Port, "")
	logLevel := fs.String("log-level", cfg.Logging.Level, "")
	// --config is consumed by cmd/dataagent before Load() — accept and
	// discard so users can pass it on the same command line.
	_ = fs.String("config", "", "")

	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("cli flags: %w", err)
	}
	cfg.Server.Host = *host
	cfg.Server.Port = *port
	cfg.Logging.Level = *logLevel
	return fs.Args(), nil
}

// parseBoolStrict accepts the exact tokens used by the spec scenarios ("true",
// "false", "1", "0") and refuses anything else so a typo doesn't quietly flip
// a security toggle.
func parseBoolStrict(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "t", "yes", "on":
		return true, nil
	case "0", "false", "f", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("expected boolean (true/false/1/0/yes/no/on/off), got %q", s)
	}
}
