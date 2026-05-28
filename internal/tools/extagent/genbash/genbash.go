// Package genbash provides a Bash-tool wrapper used exclusively by the
// adapter-generator subagent. It rejects every command that does not match a
// curated allow-list of read-only probes (which / type / readlink / file /
// ls / man / cat / head / <bin> --help / <bin> --version etc.) before the
// command ever reaches the underlying Bash tool, and before the global
// dangerous-command guard ever sees it.
//
// The inspector lives at the tool layer (not in internal/tools/bash) so the
// vanilla Bash tool used by ordinary sessions stays unchanged. Only sessions
// whose agent_type is adapter-generator get this wrapper injected into their
// per-session registry (see runner factory's adapter-generator branch).
package genbash

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// ToolName is the registered name. It deliberately reuses "Bash" so the
// LLM's view is identical to a normal Bash tool surface — the LLM should not
// need to know it's being restricted.
const ToolName = "Bash"

// metacharacters are rejected anywhere in the input. The list mirrors
// add-llm-adapter-generator design G12 / spec §"Bash command inspector".
// Backtick is a single rune; the multi-rune sequences (&& || >> << $( ) are
// detected via substring search below.
var (
	rejectChars = []string{
		";", "|", "&", ">", "<", "`", "\n", "\r",
	}
	rejectSubstrings = []string{
		"&&", "||", ">>", "<<", "$(",
	}
)

// allowedPatterns is the canonical list of command shapes the generator
// subagent may execute. Each entry is anchored with ^...$ and uses \s+ for
// inter-token separation, so leading/trailing whitespace is rejected.
var allowedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^which\s+\S+$`),
	regexp.MustCompile(`^type\s+\S+$`),
	regexp.MustCompile(`^command\s+-v\s+\S+$`),
	regexp.MustCompile(`^readlink\s+(-f\s+)?\S+$`),
	regexp.MustCompile(`^file\s+\S+$`),
	regexp.MustCompile(`^ls\s+(-l\s+)?\S+$`),
	regexp.MustCompile(`^man\s+\S+$`),
	regexp.MustCompile(`^cat\s+\S+$`),
	regexp.MustCompile(`^head(\s+-n\s+\d+)?\s+\S+$`),
	regexp.MustCompile(`^\S+(\s+\S+)*?\s+(--help|-h|help|-\?)(\s+.*)?$`),
	regexp.MustCompile(`^\S+(\s+\S+)*?\s+(--version|-V|version)$`),
}

// pathTakingCommands identifies which of the allowed patterns terminate in a
// path argument that needs additional containment (must be under an install
// prefix or a standard documentation root). The map keys are the leading
// command tokens.
var pathTakingCommands = map[string]struct{}{
	"readlink": {},
	"file":     {},
	"ls":       {},
	"cat":      {},
	"head":     {},
}

// standardDocRoots lists filesystem prefixes that count as benign
// documentation locations regardless of which binary is being analyzed.
// Operators on Linux distributions frequently keep README and man content
// under these roots.
var standardDocRoots = []string{
	"/usr/share/man/",
	"/usr/share/doc/",
}

// Host injects per-call context: the install prefix of the binary the
// generator is currently analyzing. The runner factory wires this to the
// agent_setup tool's "binary path" so each call's path checks are scoped
// to the analysis target. May be empty when the generator is just running
// universal probes (which / <bin> --version on unrelated binaries).
type Host struct {
	// InstallPrefix is a directory the generator's path-taking commands may
	// touch (in addition to the standard doc roots). Resolved at agent_setup
	// time as dirname(dirname(resolved binary path)) — e.g. for
	// /usr/local/bin/gemini → /usr/local/.
	InstallPrefix string
}

// Tool implements tools.Tool. It does NOT embed the real Bash tool: instead
// it delegates to an injected runner. The runner is the same Bash struct
// the rest of the system uses; tests substitute a recording stub.
type Tool struct {
	Host    *Host
	Backend tools.Tool
}

var _ tools.Tool = (*Tool)(nil)

func (Tool) Name() string { return ToolName }

func (t Tool) Description() string {
	return "Restricted Bash for the adapter-generator subagent. Allows only read-only " +
		"probes of a binary's documentation and identity: which/type/command -v, " +
		"readlink, file, ls, man, cat, head, <bin> --help, <bin> --version. " +
		"Shell metacharacters and command substitution are rejected outright. " +
		"Path-taking commands are restricted to the analyzed binary's install " +
		"prefix or standard system documentation roots."
}

func (t Tool) InputSchema() json.RawMessage {
	if t.Backend != nil {
		return t.Backend.InputSchema()
	}
	return json.RawMessage(`{"type":"object","required":["command"],"properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer"}}}`)
}

func (Tool) IsReadOnly() bool { return false }

func (Tool) CanRunInParallel() bool { return false }

func (t Tool) DefaultTimeout() time.Duration {
	if t.Backend != nil {
		return t.Backend.DefaultTimeout()
	}
	return 30 * time.Second
}

type bashInput struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

func (t Tool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in bashInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid input: %v", err), nil
	}
	cmd := in.Command
	if reason := Inspect(cmd, t.resolveInstallPrefix(env)); reason != "" {
		return errResult("generator Bash rejected: %s", reason), nil
	}
	if t.Backend == nil {
		return errResult("genbash: no backend wired"), nil
	}
	return t.Backend.Run(ctx, env, raw)
}

// EnvInstallPrefix is the per-session environment variable agent_setup writes
// when dispatching the adapter-generator subagent. The inspector reads it on
// every call so the install-prefix scope follows the binary being analyzed
// without rebuilding the tool struct.
const EnvInstallPrefix = "WORKHORSE_AGENT_GENBASH_PREFIX"

func (t Tool) resolveInstallPrefix(env *tools.Env) string {
	if env != nil {
		if p := env.Env[EnvInstallPrefix]; p != "" {
			return p
		}
	}
	if t.Host == nil {
		return ""
	}
	return t.Host.InstallPrefix
}

// Inspect returns "" when the command is acceptable and a human-readable
// reason string otherwise. Exported so tests in this package (and §4.5's
// regression tests across the codebase) can exercise the inspector
// independently of running a real backend.
func Inspect(cmd, installPrefix string) string {
	if cmd == "" {
		return "empty command"
	}
	if strings.TrimSpace(cmd) != cmd {
		return "leading or trailing whitespace"
	}
	for _, ch := range rejectChars {
		if strings.Contains(cmd, ch) {
			return fmt.Sprintf("metacharacter %q not allowed", ch)
		}
	}
	for _, sub := range rejectSubstrings {
		if strings.Contains(cmd, sub) {
			return fmt.Sprintf("metacharacter sequence %q not allowed", sub)
		}
	}
	matched := false
	for _, p := range allowedPatterns {
		if p.MatchString(cmd) {
			matched = true
			break
		}
	}
	if !matched {
		return "command does not match any allowed pattern"
	}
	// Path-taking commands need an additional containment check.
	first, rest, _ := splitFirstToken(cmd)
	if _, isPath := pathTakingCommands[first]; isPath {
		path := lastToken(rest)
		if path == "" {
			return "path argument missing"
		}
		if !filepath.IsAbs(path) {
			return "path must be absolute"
		}
		if !pathAllowed(path, installPrefix) {
			return fmt.Sprintf("path %q outside install prefix %q and standard doc roots", path, installPrefix)
		}
	}
	return ""
}

// splitFirstToken returns the first whitespace-delimited token and the
// remainder. Both are trimmed of leading whitespace.
func splitFirstToken(s string) (first, rest string, ok bool) {
	idx := strings.IndexAny(s, " \t")
	if idx == -1 {
		return s, "", false
	}
	return s[:idx], strings.TrimLeft(s[idx:], " \t"), true
}

// lastToken returns the last whitespace-delimited token in s, or "" when s
// holds no token. Used to extract the path argument from "readlink -f /x" et
// al. — patterns that admit zero or one flag tokens before the path.
func lastToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func pathAllowed(path, installPrefix string) bool {
	clean := filepath.Clean(path)
	for _, root := range standardDocRoots {
		if hasPrefix(clean, root) {
			return true
		}
	}
	if installPrefix != "" {
		prefixClean := filepath.Clean(installPrefix)
		if !strings.HasSuffix(prefixClean, string(filepath.Separator)) {
			prefixClean += string(filepath.Separator)
		}
		if hasPrefix(clean, prefixClean) || clean == strings.TrimRight(prefixClean, string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func hasPrefix(path, prefix string) bool {
	return strings.HasPrefix(path, prefix)
}

func errResult(format string, args ...any) *tools.Result {
	return &tools.Result{Output: fmt.Sprintf(format, args...), IsError: true}
}
