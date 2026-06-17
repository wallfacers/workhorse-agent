// Command workhorse-agent is the workhorse-agent server binary. It speaks MCP 2025-11-25
// Streamable HTTP over a local 127.0.0.1 listener and brokers tool calls,
// permission prompts, MCP servers, and sub-agent dispatch on behalf of a
// single user.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// version is overridden at link time via -ldflags "-X main.version=..."
var version = "dev"

const usageText = `workhorse-agent — local AI agent server (research build)

USAGE
  workhorse-agent <command> [flags]

COMMANDS
  init          generate ~/.workhorse-agent/{config.yaml,mcp.json,skills,agents} on first run
  serve         start the HTTP + MCP Streamable HTTP server
  setup-agent   trigger adapter generation for a newly installed CLI (talks to a running server)
  approve       resolve a pending adapter-generation approval by id
  permissions   manage permanent permission rules (talks to a running server)
  memory        inspect the memory store (e.g. "memory export" to markdown)
  version       print build version

GLOBAL FLAGS
  --config <path>      path to config.yaml (default: ~/.workhorse-agent/config.yaml)
  --host <addr>        bind address (default: 127.0.0.1)
  --port <int>         bind port (default: 7821)
  --log-level <level>  debug|info|warn|error (default: info)

Run "workhorse-agent <command> --help" for command-specific help.
`

// errExitUsage signals "user mis-typed the command line"; main turns it into
// exit code 2 (the Unix convention for usage errors).
var errExitUsage = errors.New("usage error")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errExitUsage) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return errExitUsage
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "setup-agent":
		return runSetupAgent(args[1:], stdout, stderr)
	case "approve":
		return runApprove(args[1:], stdout, stderr)
	case "permissions", "perm":
		return runPermissions(args[1:], stdout, stderr)
	case "memory":
		return runMemory(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		return runVersion(stdout)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usageText)
		return nil
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usageText)
		return errExitUsage
	}
}

func runVersion(out io.Writer) error {
	fmt.Fprintf(out, "workhorse-agent %s\n", version)
	fmt.Fprintf(out, "  built with %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

// extractConfigPath scans for `--config <path>` or `--config=<path>` and
// returns the captured value. The path controls *which* yaml file the config
// loader reads, so it has to be resolved before config.Load runs.
func extractConfigPath(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "--config=") {
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return ""
}
