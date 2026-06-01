package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/wallfacers/workhorse-agent/internal/config"
)

func runPermissions(args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "permissions: <subcommand> required: list | add | remove")
		return errExitUsage
	}

	cfgPath := extractConfigPath(args)
	cfg, err := config.Load(config.LoadOptions{YAMLPath: cfgPath, LookupEnv: os.LookupEnv})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	switch args[0] {
	case "list":
		return permList(cfg, stdout, stderr)
	case "add":
		return permAdd(cfg, args[1:], stdout, stderr)
	case "remove":
		return permRemove(cfg, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "permissions: unknown subcommand %q (expected list | add | remove)\n", args[0])
		return errExitUsage
	}
}

func permList(cfg config.Config, stdout, _ io.Writer) error {
	url := buildServerURL(cfg, "/v1/permissions")
	resp, err := doAPI(http.MethodGet, url, cfg, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	type rule struct {
		ID       string `json:"id"`
		Tool     string `json:"tool"`
		Pattern  string `json:"pattern"`
		Decision string `json:"decision"`
		Source   string `json:"source"`
	}
	type listResp struct {
		Rules []rule `json:"rules"`
	}
	var lr listResp
	if err := json.Unmarshal(body, &lr); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	fmt.Fprintf(stdout, "%-24s %-10s %-20s %-20s %-10s\n", "ID", "TOOL", "PATTERN", "DECISION", "SOURCE")
	for _, r := range lr.Rules {
		tool := r.Tool
		if tool == "" {
			tool = "*"
		}
		pat := r.Pattern
		if pat == "" {
			pat = "**"
		}
		fmt.Fprintf(stdout, "%-24s %-10s %-20s %-20s %-10s\n", r.ID, tool, pat, r.Decision, r.Source)
	}
	return nil
}

func permAdd(cfg config.Config, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("permissions add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tool := fs.String("tool", "", "tool name (empty = all tools)")
	pattern := fs.String("pattern", "", "glob pattern (empty = all resources)")
	if err := fs.Parse(args); err != nil {
		return errExitUsage
	}
	args = fs.Args()
	if len(args) < 1 {
		fmt.Fprintln(stderr, "permissions add: <decision> required (allow_permanent | deny_permanent)")
		fmt.Fprintln(stderr, "usage: permissions add [--tool <name>] [--pattern <glob>] <decision>")
		return errExitUsage
	}
	decision := args[0]
	if decision != "allow_permanent" && decision != "deny_permanent" {
		fmt.Fprintf(stderr, "permissions add: decision must be allow_permanent or deny_permanent, got %q\n", decision)
		return errExitUsage
	}

	payload := map[string]string{
		"tool":     *tool,
		"pattern":  *pattern,
		"decision": decision,
	}
	body, _ := json.Marshal(payload)
	url := buildServerURL(cfg, "/v1/permissions")
	resp, err := doAPI(http.MethodPost, url, cfg, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(respBody))
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Fprintf(stdout, "Created: %s\n", r.ID)
	return nil
}

func permRemove(cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "permissions remove: <id> required")
		return errExitUsage
	}
	id := args[0]
	url := buildServerURL(cfg, "/v1/permissions/"+id)
	resp, err := doAPI(http.MethodDelete, url, cfg, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(respBody))
	}
	fmt.Fprintf(stdout, "Deleted: %s\n", id)
	return nil
}

func doAPI(method, url string, cfg config.Config, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Auth.Enabled && cfg.Auth.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Auth.BearerToken)
	}
	return http.DefaultClient.Do(req)
}
