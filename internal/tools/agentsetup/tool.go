// Package agentsetup exposes agent_setup, the orchestrator-facing tool that
// orchestrates the full adapter-generation flow: binary preflight, metadata
// collection, dispatch to the adapter-generator subagent, validation, smoke
// test, and approval-request submission.
//
// The tool is InternalGated — it gates its own preflight inside Run and the
// orchestrator skips the standard Permissions.Check (see internal/agent/loop
// for the marker interface usage).
package agentsetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/approval"
	"github.com/wallfacers/workhorse-agent/internal/extagent/smoke"
	"github.com/wallfacers/workhorse-agent/internal/prompt"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/extagent/genbash"
	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

// ToolName is the name surfaced to the LLM as a tool_use block.
const ToolName = "agent_setup"

// helpCapBytes / versionCapBytes / manCapBytes / readmeCapBytes cap each
// captured probe output before it lands in the prompt. The caps mirror
// design.md §G3: --help unbounded in practice (most CLIs print well under
// 10k), --version typically tiny, man capped at 10000, README at 5000.
const (
	helpCapBytes    = 16 * 1024
	versionCapBytes = 2 * 1024
	manCapBytes     = 10 * 1024
	readmeCapBytes  = 5 * 1024
)

// dangerousArgPatterns is the post-generation scan applied to every string
// in invocation.extra_args and to every key/value in invocation.env_override.
// It mirrors the bash-tool dangerous-command set from CLAUDE.md so an LLM
// can't smuggle "rm -rf /" via, say, an alias-style extra_arg.
var dangerousArgPatterns = []string{
	"rm -rf /",
	"rm -rf ~",
	"dd of=/dev/",
	"mkfs.",
	"chmod -R 777 /",
	"shutdown",
	"reboot",
	"halt",
	"poweroff",
	"base64 -d | sh",
	"curl | bash",
}

// systemShells is the set rejected at preflight. Shells are not legitimate
// sub_agent surfaces — they would punch a hole through the entire safety
// model. See design.md G13.
var systemShells = map[string]struct{}{
	"bash": {}, "sh": {}, "zsh": {}, "dash": {}, "csh": {},
	"fish": {}, "ash": {}, "ksh": {}, "tcsh": {},
}

// sensitivePathPrefixes are filesystem locations no real binary should
// resolve to. Refuse outright if which() returns one of these.
var sensitivePathPrefixes = []string{"/proc/", "/sys/", "/dev/"}

// Input is the JSON shape the LLM emits.
type Input struct {
	Binary          string `json:"binary"`
	DescriptionHint string `json:"description_hint,omitempty"`
	Regenerate      bool   `json:"regenerate,omitempty"`
	Model           string `json:"model,omitempty"`
}

// Dispatcher is the callable the runner factory wires up. It runs the
// generator subagent and returns the subagent's final assistant text.
type Dispatcher interface {
	Dispatch(ctx context.Context, parentSessionID, prompt, model string, env map[string]string) (string, error)
}

// Host bundles the runtime dependencies. None of the fields are optional —
// New panics in the runner factory if any are nil.
type Host struct {
	Registry          *extagent.Registry
	ExternalAgentsDir string
	Dispatcher        Dispatcher
	Approval          *approval.Manager
	// SchemaJSON is the adapter JSON schema embedded as a string. The runner
	// factory injects this once at startup.
	SchemaJSON string
	// Examples are the few-shot adapter snippets bundled into the generation
	// prompt. The runner factory loads them from the extagent builtins.
	Examples []prompt.AdapterGenerationExample
	// ModelDefault is the model used when Input.Model is empty.
	ModelDefault string
	// AllowedModels gates which models may be used; empty means "any".
	AllowedModels []string
}

// Tool implements tools.Tool.
type Tool struct {
	Host *Host
}

// IsInternalGated marks the tool as gating its own preflight; the
// orchestrator skips the standard permission check.
func (Tool) IsInternalGated() bool { return true }

var _ tools.Tool = (*Tool)(nil)

func (Tool) Name() string { return ToolName }

func (Tool) Description() string {
	return "Set up an external sub-agent adapter for a newly installed CLI. " +
		"Runs the adapter-generator subagent, validates the draft, smoke-tests " +
		"it, and submits an approval request to the user. Returns the approval_id " +
		"on success."
}

func (Tool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "required": ["binary"],
  "additionalProperties": false,
  "properties": {
    "binary":           {"type": "string", "description": "Binary name on PATH or absolute path"},
    "description_hint": {"type": "string"},
    "regenerate":       {"type": "boolean"},
    "model":            {"type": "string"}
  }
}`)
}

func (Tool) IsReadOnly() bool { return false }

func (Tool) CanRunInParallel() bool { return false }

// DefaultTimeout reflects the worst-case end-to-end runtime: metadata
// collection (≤ ~30s) + generator subagent (≤ ~10 turns × 5s) + smoke
// (≤ 60s) + comfortable margin. 600s sits within the orchestrator's
// backstop. (design.md G7 / Change 1 D20.)
func (Tool) DefaultTimeout() time.Duration { return 600 * time.Second }

// Run is the entry point invoked by the orchestrator.
func (t Tool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	if t.Host == nil {
		return errResult("agent_setup misconfigured: host is nil"), nil
	}
	var in Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid input: %v", err), nil
	}
	if strings.TrimSpace(in.Binary) == "" {
		return errResult("input.binary is required"), nil
	}

	// 1. Preflight.
	pre, perr := t.preflight(in)
	if perr != "" {
		return errResult("%s", perr), nil
	}

	// 2. Retry hygiene: stale drafts from a prior failed run would otherwise
	// blend into a fresh approval.
	draftPath := filepath.Join(pathguard.DraftsDir(t.Host.ExternalAgentsDir), pre.Name+".yaml")
	_ = os.Remove(draftPath)

	// 3. Metadata collection.
	meta := collectMetadata(ctx, pre.Path)

	// 4. Render the generation prompt.
	templated, err := t.Host.renderPrompt(pre, in, meta)
	if err != nil {
		return errResult("render prompt: %v", err), nil
	}

	// 5. Validate model selection against the allowlist.
	model := in.Model
	if model == "" {
		model = t.Host.ModelDefault
	}
	if !t.Host.modelAllowed(model) {
		return errResult("model %q not in external_agents.generation.allowed_models", model), nil
	}

	// 6. Dispatch the adapter-generator subagent. The subagent's env carries
	// the install-prefix scope for genbash's path checks.
	dispatchEnv := map[string]string{
		genbash.EnvInstallPrefix: deriveInstallPrefix(pre.Path),
	}
	subagentOutput, derr := t.Host.Dispatcher.Dispatch(ctx, env.SessionID, templated, model, dispatchEnv)
	if derr != nil {
		return errResult("generator subagent failed: %v", derr), nil
	}
	if strings.Contains(subagentOutput, "CLI_TOOL_REFUSAL") {
		return errResult("cli_tool refusal: %s", trim(subagentOutput, 400)), nil
	}

	// 7. The subagent must have written the draft via WriteAdapterDraft.
	draftRaw, rerr := os.ReadFile(draftPath)
	if rerr != nil {
		return errResult("subagent did not write a draft at %s: %v", draftPath, rerr), nil
	}
	adapter, perr2 := extagent.Parse(draftRaw)
	if perr2 != nil {
		return errResult("draft failed schema validation: %v", perr2), nil
	}
	if adapter.Name != pre.Name {
		return errResult("draft name=%q does not match expected %q", adapter.Name, pre.Name), nil
	}
	if reason := scanDangerousArgs(adapter); reason != "" {
		return errResult("dangerous pattern in generated adapter: %s", reason), nil
	}

	// 8. Smoke test the draft.
	smokeOutcome := runSmoke(adapter)
	if !smokeOutcome.Passed {
		// Per design.md G5: smoke failure does NOT block approval. The user
		// sees the failure in the approval payload and may approve a
		// fail-but-close adapter to fix it manually.
	}

	// 9. Prior YAML + diff for the regenerate path.
	priorYAML, diff := ""+"", ""
	if in.Regenerate && t.Host.Registry != nil {
		if prior := t.Host.Registry.Get(pre.Name); prior != nil {
			priorYAML, diff = priorAndDiff(t.Host.ExternalAgentsDir, pre.Name, draftRaw)
		}
	}

	// 10. Register pending approval.
	pending := &approval.PendingApproval{
		SessionID: env.SessionID,
		AgentName: pre.Name,
		DraftPath: draftPath,
		DraftYAML: string(draftRaw),
		PriorYAML: priorYAML,
		Diff:      diff,
		Smoke:     smokeOutcome,
		Provenance: approval.Provenance{
			GeneratedBy: model,
			GeneratedAt: time.Now().UTC(),
			ToolVersion: trim(meta.Version, 256),
		},
	}
	id := t.Host.Approval.Register(pending)
	return &tools.Result{
		Output: fmt.Sprintf("Submitted adapter draft for %q. approval_id=%s. Awaiting user decision via SSE.", pre.Name, id),
	}, nil
}

// preflightResult is the small typed bundle produced by preflight.
type preflightResult struct {
	Name string // sanitized name used as adapter name and draft filename stem
	Path string // absolute, resolved binary path
}

func (t Tool) preflight(in Input) (preflightResult, string) {
	bin := strings.TrimSpace(in.Binary)
	if bin == "" {
		return preflightResult{}, "binary is required"
	}
	// Resolve to absolute path.
	abs, err := resolveBinary(bin)
	if err != nil {
		return preflightResult{}, fmt.Sprintf("Cannot set up %s: binary not found on PATH. Install it first.", bin)
	}
	// System shell rejection.
	base := filepath.Base(abs)
	if _, banned := systemShells[base]; banned {
		return preflightResult{}, fmt.Sprintf("Refusing to generate an adapter for shell %q. Shells are not appropriate as sub_agents.", base)
	}
	// Sensitive path rejection.
	for _, prefix := range sensitivePathPrefixes {
		if strings.HasPrefix(abs, prefix) {
			return preflightResult{}, fmt.Sprintf("Refusing to generate adapter for sensitive path %s.", abs)
		}
	}
	// Already-registered without regenerate.
	name := strings.TrimSuffix(filepath.Base(in.Binary), filepath.Ext(filepath.Base(in.Binary)))
	if name == "" {
		name = base
	}
	if !in.Regenerate && t.Host.Registry != nil {
		if existing := t.Host.Registry.Get(name); existing != nil {
			return preflightResult{}, fmt.Sprintf("adapter %q is already registered. Pass regenerate=true to refresh.", name)
		}
	}
	return preflightResult{Name: name, Path: abs}, ""
}

func resolveBinary(bin string) (string, error) {
	if filepath.IsAbs(bin) {
		fi, err := os.Stat(bin)
		if err != nil {
			return "", err
		}
		if fi.Mode()&0o111 == 0 {
			return "", fmt.Errorf("not executable: %s", bin)
		}
		return bin, nil
	}
	return exec.LookPath(bin)
}

func deriveInstallPrefix(absBin string) string {
	// dirname(dirname(absBin)) — e.g. /usr/local/bin/gemini → /usr/local.
	parent := filepath.Dir(filepath.Dir(absBin))
	if parent == "" || parent == "/" {
		return parent
	}
	return parent + string(filepath.Separator)
}

// metadata bundles the probes collected before invoking the subagent.
type metadata struct {
	Help    string
	Version string
	Man     string
	Readme  string
}

func collectMetadata(ctx context.Context, absBin string) metadata {
	m := metadata{
		Help:    runAndCap(ctx, helpCapBytes, absBin, "--help"),
		Version: runAndCap(ctx, versionCapBytes, absBin, "--version"),
	}
	if manPath, _ := exec.LookPath("man"); manPath != "" {
		m.Man = runAndCap(ctx, manCapBytes, manPath, filepath.Base(absBin))
	}
	// README hint: look one directory above the install prefix.
	prefix := deriveInstallPrefix(absBin)
	for _, leaf := range []string{"README", "README.md", "README.txt"} {
		candidate := filepath.Join(prefix, "share", "doc", filepath.Base(absBin), leaf)
		if b, err := os.ReadFile(candidate); err == nil {
			m.Readme = capBytes(b, readmeCapBytes)
			break
		}
	}
	return m
}

func runAndCap(ctx context.Context, cap int, cmd string, args ...string) string {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, cmd, args...).Output()
	if err != nil {
		return ""
	}
	return capBytes(out, cap)
}

func capBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "\n[... truncated]"
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func scanDangerousArgs(a *extagent.Adapter) string {
	for _, arg := range a.Invocation.ExtraArgs {
		for _, p := range dangerousArgPatterns {
			if strings.Contains(arg, p) {
				return fmt.Sprintf("extra_args[%q] matches %q", arg, p)
			}
		}
	}
	for k, v := range a.Invocation.EnvOverride {
		for _, p := range dangerousArgPatterns {
			if strings.Contains(k, p) || strings.Contains(v, p) {
				return fmt.Sprintf("env_override key/value contains %q", p)
			}
		}
	}
	return ""
}

func runSmoke(a *extagent.Adapter) approval.SmokeOutcome {
	if a == nil || a.SmokeTest.Prompt == "" {
		return approval.SmokeOutcome{Passed: false, Reason: "no_smoke_test_declared"}
	}
	res := smoke.Run(a, nil)
	out := approval.SmokeOutcome{
		Passed: res.Passed,
		Reason: "ok",
	}
	if !res.Passed {
		out.Reason = res.Error
	}
	return out
}

// priorAndDiff returns the existing YAML on disk and a unified diff between
// it and the new draft. Both may be empty.
func priorAndDiff(liveDir, name string, newRaw []byte) (string, string) {
	priorPath := filepath.Join(liveDir, name+".yaml")
	priorBytes, err := os.ReadFile(priorPath)
	if err != nil {
		return "", ""
	}
	return string(priorBytes), unifiedDiff(string(priorBytes), string(newRaw), priorPath)
}

// unifiedDiff produces a small unified diff. We deliberately avoid pulling a
// large third-party diff library: the LLM's task is to read the diff, not to
// patch it. A line-by-line diff with @@ headers is good enough for human
// review.
func unifiedDiff(prior, draft, label string) string {
	if prior == draft {
		return ""
	}
	priorLines := strings.Split(prior, "\n")
	draftLines := strings.Split(draft, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ draft\n", label)
	// Naive LCS-free diff: emit -prior and +draft blocks. This is sufficient
	// for the review use case; a fancier diff is a later optimisation.
	fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", len(priorLines), len(draftLines))
	for _, l := range priorLines {
		fmt.Fprintf(&b, "-%s\n", l)
	}
	for _, l := range draftLines {
		fmt.Fprintf(&b, "+%s\n", l)
	}
	return b.String()
}

// renderPrompt wraps the prompt template with the host's bundled inputs.
func (h *Host) renderPrompt(pre preflightResult, in Input, meta metadata) (string, error) {
	return prompt.AdapterGeneration.Execute(map[string]any{
		"SchemaJSON":      h.SchemaJSON,
		"BinaryName":      pre.Name,
		"BinaryPath":      pre.Path,
		"HelpOutput":      meta.Help,
		"VersionOutput":   meta.Version,
		"ManOutput":       meta.Man,
		"ReadmeOutput":    meta.Readme,
		"DescriptionHint": in.DescriptionHint,
		"Examples":        h.Examples,
	})
}

func (h *Host) modelAllowed(model string) bool {
	if h == nil || len(h.AllowedModels) == 0 {
		return true
	}
	for _, m := range h.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

func errResult(format string, args ...any) *tools.Result {
	return &tools.Result{Output: fmt.Sprintf(format, args...), IsError: true}
}

// ensure the dispatcher interface is referenced from a doc-friendly spot
// even if linkers ever try to be too clever.
var _ = errors.New
