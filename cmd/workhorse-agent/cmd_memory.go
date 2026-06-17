package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/config"
	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

const memoryUsageText = `workhorse-agent memory — inspect the per-entry memory store

USAGE
  workhorse-agent memory export [--config <path>] [--out <file>]

  export   render every memory entry to a human-readable markdown document for
           inspection / git backup. Writes to stdout, or to --out when given
           (resolved through pathguard relative to the current directory).
`

// runMemory dispatches the `memory` subcommands. Only `export` exists today.
func runMemory(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, memoryUsageText)
		return errExitUsage
	}
	switch args[0] {
	case "export":
		return runMemoryExport(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, memoryUsageText)
		return nil
	default:
		fmt.Fprintf(stderr, "unknown memory subcommand %q\n\n", args[0])
		fmt.Fprint(stderr, memoryUsageText)
		return errExitUsage
	}
}

func runMemoryExport(args []string, stdout, stderr io.Writer) error {
	out := extractFlag(args, "--out")

	configPath := extractConfigPath(args)
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("memory export: locate home directory: %w", err)
		}
		configPath = filepath.Join(home, ".workhorse-agent", "config.yaml")
	}
	// --out is this command's own flag; strip it before config.Load, whose flag
	// parser only knows the global/serve flags and would reject it.
	cfg, err := config.Load(config.LoadOptions{YAMLPath: configPath, Args: stripFlag(args, "--out"), ResolveHomePaths: true})
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: cfg.Store.Path, BusyTimeoutMs: cfg.Store.BusyTimeoutMs})
	if err != nil {
		return fmt.Errorf("memory export: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	entries, err := memory.NewEntryStore(st.DB()).List(ctx)
	if err != nil {
		return fmt.Errorf("memory export: list entries: %w", err)
	}
	doc := memory.RenderExport(entries)

	if out == "" {
		_, err := io.WriteString(stdout, doc)
		return err
	}

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("memory export: resolve working directory: %w", err)
	}
	resolved, err := pathguard.ResolveForWrite(wd, out)
	if err != nil {
		return fmt.Errorf("memory export: invalid --out path: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(doc), 0o600); err != nil {
		return fmt.Errorf("memory export: write %s: %w", resolved, err)
	}
	fmt.Fprintf(stderr, "wrote %d entries to %s\n", len(entries), resolved)
	return nil
}

// extractFlag returns the value of `--name <value>` or `--name=<value>` from
// args, or "" when absent.
func extractFlag(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"=")
		}
	}
	return ""
}

// stripFlag returns args with `--name <value>` or `--name=<value>` removed, so a
// command-specific flag is not passed through to config.Load's flag parser.
func stripFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == name {
			i++ // also skip the value
			continue
		}
		if strings.HasPrefix(a, name+"=") {
			continue
		}
		out = append(out, a)
	}
	return out
}
