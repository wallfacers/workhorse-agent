package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wallfacers/data-agent/internal/config"
)

// runServe boots the HTTP listener and the agent runtime. The full assembly —
// session manager, agent loop, provider clients, MCP host, API router — is
// wired up in later task groups (8, 9, 11). At this stage the command loads
// configuration and reports what it would bind, so operators can confirm the
// merge ordering before the runtime is feature-complete.
func runServe(args []string, stdout, stderr io.Writer) error {
	configPath := extractConfigPath(args)
	if configPath == "" {
		// Fall back to ~/.dataagent/config.yaml.
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("serve: locate home directory: %w", err)
		}
		configPath = filepath.Join(home, ".dataagent", "config.yaml")
	}

	cfg, err := config.Load(config.LoadOptions{
		YAMLPath:         configPath,
		Args:             args,
		ResolveHomePaths: true,
	})
	if err != nil {
		return err
	}

	// TODO(group-8, group-9): assemble session manager, agent loop, provider
	// clients, MCP host, API router; bind cfg.Server.Host:cfg.Server.Port;
	// register SIGTERM/SIGINT; execute the 7-step graceful shutdown defined
	// in api-protocol spec § Graceful Shutdown.
	fmt.Fprintf(stdout, "dataagent serve — runtime not yet implemented\n")
	fmt.Fprintf(stdout, "  resolved config:  %s\n", configPath)
	fmt.Fprintf(stdout, "  would bind:       %s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Fprintf(stdout, "  default provider: %s\n", cfg.Providers.Default)
	fmt.Fprintf(stdout, "  store:            %s\n", cfg.Store.Path)
	fmt.Fprintf(stdout, "  log:              %s/%s\n", cfg.Logging.Level, cfg.Logging.Format)
	_ = stderr
	return errors.New("serve subcommand is not yet implemented (group 8/9 pending)")
}
