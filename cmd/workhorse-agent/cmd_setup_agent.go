package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/config"
)

// isTTY reports whether fd is connected to a terminal. We avoid pulling in
// golang.org/x/term to keep the dependency footprint (and minimum Go
// version) unchanged: an os.Stat on the fd with ModeCharDevice is enough
// for the platforms we care about.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runSetupAgent implements `workhorse-agent setup-agent <binary>`. It talks
// to a running workhorse-agent server via HTTP to drive the
// adapter-generation flow non-interactively.
//
// MVP shape:
//   - Creates a one-shot ephemeral session.
//   - Posts a single user message instructing the orchestrator to invoke
//     agent_setup with the binary name. The orchestrator's LLM is expected
//     to emit the tool call; the response stream surfaces an
//     adapter_approval_request event carrying the approval_id.
//   - In TTY mode prompts the operator for [a]pprove / [r]eject / [e]dit.
//   - In non-TTY mode prints approval_id=<id> and exits 0.
//
// The TTY interactive flow is delegated to the `approve` subcommand below;
// setup-agent only handles the request → approval_id half of the protocol.
func runSetupAgent(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("setup-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	descriptionHint := fs.String("description-hint", "", "optional hint passed to the generator")
	regenerate := fs.Bool("regenerate", false, "force regeneration even if an adapter already exists")
	model := fs.String("model", "", "override the model used by the generator subagent")
	cfgPath := fs.String("config", "", "path to config.yaml (default: ~/.workhorse-agent/config.yaml)")
	if err := fs.Parse(args); err != nil {
		return errExitUsage
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "setup-agent: <binary> is required")
		return errExitUsage
	}
	binary := fs.Arg(0)

	cfg, err := config.Load(config.LoadOptions{YAMLPath: *cfgPath, LookupEnv: os.LookupEnv})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.ExternalAgents.Generation.Enabled {
		return errors.New("external_agents.generation.enabled is false in config")
	}

	body := map[string]any{
		"binary": binary,
	}
	if *descriptionHint != "" {
		body["description_hint"] = *descriptionHint
	}
	if *regenerate {
		body["regenerate"] = true
	}
	if *model != "" {
		body["model"] = *model
	}

	approvalID, err := dispatchAgentSetup(cfg, body)
	if err != nil {
		return err
	}

	if isTTY(os.Stdin) {
		return interactiveApprovalLoop(stdout, stderr, cfg, "", approvalID)
	}
	fmt.Fprintf(stdout, "approval_id=%s\n", approvalID)
	return nil
}

// dispatchAgentSetup creates an ephemeral session and posts the agent_setup
// trigger message; returns the approval_id surfaced on the SSE stream.
//
// This MVP shortcuts the orchestrator dance: it invokes the agent_setup tool
// directly via the server's /v1/sessions/{id}/approvals proxy. A full
// implementation would create a session and ride the SSE stream until the
// approval event arrives. The shortcut returns a clear message so operators
// know what to do.
func dispatchAgentSetup(cfg config.Config, _ map[string]any) (string, error) {
	// Direct programmatic dispatch through the server requires either a tool
	// invocation API or session-streaming. The MVP CLI uses
	// `workhorse-agent setup-agent` as a thin wrapper that prints
	// instructions when no direct API exists yet. This keeps the surface
	// minimal until a /v1/agent_setup endpoint lands in a follow-up change.
	_ = cfg
	return "", errors.New("setup-agent CLI: invoke agent_setup via the chat session UI for now; this subcommand is a placeholder until /v1/agent_setup ships")
}

// interactiveApprovalLoop is shared by setup-agent (TTY mode) and the
// `approve` subcommand: fetches the approval payload (not yet exposed via
// HTTP — placeholder), prompts the user, posts the decision.
func interactiveApprovalLoop(stdout, stderr io.Writer, cfg config.Config, sessionID, approvalID string) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintf(stdout, "approval_id=%s\n", approvalID)
	fmt.Fprint(stdout, "decision? [a]pprove / [r]eject / [e]dit: ")
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read decision: %w", err)
	}
	line = strings.TrimSpace(line)
	switch line {
	case "a", "approve":
		return postDecision(cfg, sessionID, approvalID, "approve", "")
	case "r", "reject":
		return postDecision(cfg, sessionID, approvalID, "reject", "")
	case "e", "edit":
		fmt.Fprint(stdout, "paste edited YAML, terminate with EOF (Ctrl-D):\n")
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		return postDecision(cfg, sessionID, approvalID, "edit", string(body))
	default:
		fmt.Fprintln(stderr, "unrecognised decision; treating as reject")
		return postDecision(cfg, sessionID, approvalID, "reject", "")
	}
}

func postDecision(cfg config.Config, sessionID, approvalID, decision, editedYAML string) error {
	if sessionID == "" {
		return errors.New("session_id is required for approval POST (set via `workhorse-agent approve --session <id> <approval_id>`)")
	}
	endpoint := buildServerURL(cfg, fmt.Sprintf("/v1/sessions/%s/approvals/%s", url.PathEscape(sessionID), url.PathEscape(approvalID)))
	payload := map[string]any{"decision": decision}
	if editedYAML != "" {
		payload["edited_yaml"] = editedYAML
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Auth.Enabled && cfg.Auth.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Auth.BearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func buildServerURL(cfg config.Config, path string) string {
	host := cfg.Server.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Server.Port
	if port == 0 {
		port = 7821
	}
	return fmt.Sprintf("http://%s:%d%s", host, port, path)
}
