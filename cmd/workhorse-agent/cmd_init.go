package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// runInit creates ~/.workhorse-agent and its expected children on first run. It
// prompts for a handful of fields (provider, API key, port, auth toggle) and
// writes a minimally annotated config.yaml plus empty mcp.json, skills/,
// agents/, and state.db. Existing files are never overwritten; the command
// reports them and exits non-zero so the operator can decide.
func runInit(args []string, stdout, stderr io.Writer) error {
	// args is currently unused — init has no flags of its own. We accept it
	// so the dispatcher in main.go can pass through uniformly.
	_ = args

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("init: locate home directory: %w", err)
	}
	root := filepath.Join(home, ".workhorse-agent")
	cfgPath := filepath.Join(root, "config.yaml")
	mcpPath := filepath.Join(root, "mcp.json")
	skillsDir := filepath.Join(root, "skills")
	agentsDir := filepath.Join(root, "agents")
	dbPath := filepath.Join(root, "state.db")

	if exists(cfgPath) {
		fmt.Fprintf(stderr, "workhorse-agent: %s already exists; refusing to overwrite\n", cfgPath)
		fmt.Fprintln(stderr, "delete or back up the existing file, then re-run `workhorse-agent init`")
		return errExitUsage
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("init: create %s: %w", root, err)
	}
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return fmt.Errorf("init: create %s: %w", skillsDir, err)
	}
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return fmt.Errorf("init: create %s: %w", agentsDir, err)
	}

	answers, err := promptInit(stdout, os.Stdin)
	if err != nil {
		return err
	}

	if err := writeFileIfAbsent(cfgPath, renderConfigYAML(answers), 0o600); err != nil {
		return err
	}
	if err := writeFileIfAbsent(mcpPath, []byte("{\n  \"servers\": {}\n}\n"), 0o600); err != nil {
		return err
	}
	// state.db is a placeholder until internal/store wires up migrations
	// in Group 3. modernc.org/sqlite opens an empty file fine.
	if err := writeFileIfAbsent(dbPath, nil, 0o600); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\nworkhorse-agent: configured under %s\n", root)
	fmt.Fprintf(stdout, "  config: %s\n", cfgPath)
	fmt.Fprintf(stdout, "  mcp:    %s\n", mcpPath)
	fmt.Fprintf(stdout, "  state:  %s\n", dbPath)
	fmt.Fprintf(stdout, "next step: `workhorse-agent serve`\n")
	return nil
}

type initAnswers struct {
	Provider     string
	AnthropicKey string
	OpenAIKey    string
	Port         int
	AuthEnabled  bool
	AuthToken    string
}

func promptInit(out io.Writer, in io.Reader) (initAnswers, error) {
	r := bufio.NewReader(in)
	fmt.Fprintln(out, "workhorse-agent init — answer a few questions, defaults in [brackets].")
	fmt.Fprintln(out)

	provider := promptOne(r, out, "Default provider [anthropic / openai]", "anthropic",
		func(s string) bool { return s == "anthropic" || s == "openai" })

	a := initAnswers{Provider: provider, Port: 7821}

	switch provider {
	case "anthropic":
		a.AnthropicKey = promptOne(r, out, "Anthropic API key (leave blank to fill in later)", "", nil)
	case "openai":
		a.OpenAIKey = promptOne(r, out, "OpenAI API key (leave blank to fill in later)", "", nil)
	}

	portStr := promptOne(r, out, "Bind port", "7821", func(s string) bool {
		n, err := strconv.Atoi(s)
		return err == nil && n >= 1 && n <= 65535
	})
	a.Port, _ = strconv.Atoi(portStr)

	authStr := promptOne(r, out, "Enable bearer-token auth? [y/N]", "N",
		func(s string) bool { return matchYesNo(s) >= 0 })
	a.AuthEnabled = matchYesNo(authStr) == 1
	if a.AuthEnabled {
		a.AuthToken = promptOne(r, out, "Bearer token (32+ random chars recommended)", "", func(s string) bool {
			return len(s) >= 8
		})
	}
	return a, nil
}

// promptOne prints a prompt with the default and rejects answers until validate
// returns true. validate=nil accepts everything (including the empty string).
func promptOne(r *bufio.Reader, out io.Writer, label, def string, validate func(string) bool) string {
	for {
		if def != "" {
			fmt.Fprintf(out, "%s [%s]: ", label, def)
		} else {
			fmt.Fprintf(out, "%s: ", label)
		}
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return def
		}
		line = strings.TrimSpace(line)
		if line == "" {
			line = def
		}
		if validate == nil || validate(line) {
			return line
		}
		fmt.Fprintf(out, "  invalid input, please try again\n")
	}
}

// matchYesNo returns 1 for yes-ish, 0 for no-ish, -1 for unrecognised.
func matchYesNo(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "true", "1":
		return 1
	case "n", "no", "false", "0", "":
		return 0
	default:
		return -1
	}
}

// renderConfigYAML produces a minimal, commented config.yaml. Operators can
// extend it later — every default lives in code, and yaml.v3 leaves unmentioned
// fields at their built-in defaults.
func renderConfigYAML(a initAnswers) []byte {
	var sb strings.Builder
	sb.WriteString("# workhorse-agent configuration. See specs/configuration/spec.md for the full schema.\n")
	sb.WriteString("# Fields omitted here keep their built-in defaults.\n\n")

	sb.WriteString("server:\n")
	sb.WriteString("  host: 127.0.0.1\n")
	fmt.Fprintf(&sb, "  port: %d\n\n", a.Port)

	sb.WriteString("auth:\n")
	fmt.Fprintf(&sb, "  enabled: %t\n", a.AuthEnabled)
	if a.AuthEnabled {
		fmt.Fprintf(&sb, "  bearer_token: %q\n", a.AuthToken)
	}
	sb.WriteString("\n")

	sb.WriteString("providers:\n")
	fmt.Fprintf(&sb, "  default: %s\n", a.Provider)
	if a.Provider == "anthropic" && a.AnthropicKey != "" {
		sb.WriteString("  anthropic:\n")
		fmt.Fprintf(&sb, "    api_key: %q\n", a.AnthropicKey)
	}
	if a.Provider == "openai" && a.OpenAIKey != "" {
		sb.WriteString("  openai:\n")
		fmt.Fprintf(&sb, "    api_key: %q\n", a.OpenAIKey)
	}
	sb.WriteString("\n")

	sb.WriteString("logging:\n")
	sb.WriteString("  level: info\n")
	sb.WriteString("  format: json\n")

	return []byte(sb.String())
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func writeFileIfAbsent(path string, data []byte, mode os.FileMode) error {
	if exists(path) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("init: create %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck
	if data != nil {
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("init: write %s: %w", path, err)
		}
	}
	return nil
}
