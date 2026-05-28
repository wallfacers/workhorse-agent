package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wallfacers/workhorse-agent/internal/config"
)

// runApprove implements `workhorse-agent approve <approval_id>`. The
// subcommand is symmetric to setup-agent: it can be driven interactively
// when stdin is a TTY (prompts for a/r/e) or non-interactively via
// --decision flag.
func runApprove(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sessionID := fs.String("session", "", "originating session id (required)")
	decision := fs.String("decision", "", "approve|reject|edit (non-interactive mode)")
	file := fs.String("file", "", "path to edited YAML file (only with --decision edit)")
	cfgPath := fs.String("config", "", "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return errExitUsage
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "approve: <approval_id> is required")
		return errExitUsage
	}
	approvalID := fs.Arg(0)

	cfg, err := config.Load(config.LoadOptions{YAMLPath: *cfgPath, LookupEnv: os.LookupEnv})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if *sessionID == "" {
		return errors.New("--session <id> is required (the session that triggered agent_setup)")
	}

	if *decision == "" {
		// No explicit decision — TTY mode prompts interactively; non-TTY
		// errors out with an instructive message.
		if !isTTY(os.Stdin) {
			return errors.New("--decision is required in non-TTY mode (approve|reject|edit)")
		}
		return interactiveApprovalLoop(stdout, stderr, cfg, *sessionID, approvalID)
	}

	switch *decision {
	case "approve", "reject":
		return postDecision(cfg, *sessionID, approvalID, *decision, "")
	case "edit":
		if *file == "" {
			return errors.New("--decision edit requires --file <edited.yaml>")
		}
		body, err := os.ReadFile(*file)
		if err != nil {
			return fmt.Errorf("read edited yaml: %w", err)
		}
		return postDecision(cfg, *sessionID, approvalID, "edit", string(body))
	default:
		fmt.Fprintf(stderr, "approve: unknown decision %q\n", *decision)
		return errExitUsage
	}
}
