package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// ImplicitTriggerConfig bundles the configuration the interceptor needs.
type ImplicitTriggerConfig struct {
	// Enabled mirrors external_agents.generation.implicit_trigger_enabled.
	// When false, the interceptor is a no-op (calls pass through).
	Enabled bool

	// SetupTool is the agent_setup tool the interceptor invokes to drive
	// the synchronous adapter-generation flow. May be nil — in which case
	// the interceptor falls back to the standard "unknown agent" error
	// (per the modified external-agents spec).
	SetupTool tools.Tool

	// Env is the tool-execution environment passed to SetupTool.Run. The
	// runner factory wires the per-session env.
	Env *tools.Env
}

// MakeImplicitTriggerHook returns an ImplicitTriggerHook closure suitable for
// Loop.ImplicitTriggerInterceptor. The closure captures the session's live
// extagent registry via env.ExtAgentRegistry so a per-session snapshot is
// always consulted (avoids leaking adapters across sessions).
func MakeImplicitTriggerHook(cfg ImplicitTriggerConfig) ImplicitTriggerHook {
	return func(ctx context.Context, sess *session.Session, calls []ToolCall) (passThrough []ToolCall, intercepted []ToolCallResult) {
		if !cfg.Enabled {
			return calls, nil
		}
		passThrough = make([]ToolCall, 0, len(calls))
		for _, c := range calls {
			if c.Name != "ExternalAgent" {
				passThrough = append(passThrough, c)
				continue
			}
			agentName := extractAgentName(c.Input)
			if agentName == "" {
				passThrough = append(passThrough, c)
				continue
			}
			if known(cfg.Env, agentName) {
				passThrough = append(passThrough, c)
				continue
			}
			// Reject path-bearing agent names — exec.LookPath treats them as
			// direct file references and would skip PATH search entirely,
			// letting an LLM-emitted "/tmp/foo" or "../bar" through.
			if strings.ContainsAny(agentName, "/\\") {
				passThrough = append(passThrough, c)
				continue
			}
			// Unknown agent. Inspect dedup state first.
			switch sess.AdapterSetupState(agentName) {
			case "pending":
				intercepted = append(intercepted, syntheticResult(c.ID, c.Name,
					fmt.Sprintf("Adapter setup for %q is pending user approval. Wait for the user or use a different approach.", agentName)))
				continue
			case "unavailable":
				intercepted = append(intercepted, syntheticResult(c.ID, c.Name,
					fmt.Sprintf("Adapter setup for %q was rejected/expired this session. Use a different approach.", agentName)))
				continue
			}
			// Confirm a binary by that name resolves on PATH.
			if _, err := exec.LookPath(agentName); err != nil {
				// Not on PATH: fall back to the standard "unknown agent"
				// rejection — ExternalAgent will produce it.
				passThrough = append(passThrough, c)
				continue
			}
			// Run agent_setup synchronously. Only mark dedup as "pending"
			// when the setup actually registered an approval — a transient
			// failure (provider down, schema error, etc.) should NOT lock
			// the session out of retrying this name for the rest of the
			// turn.
			text, registered := runImplicitSetup(ctx, cfg, agentName, c.Input)
			if registered {
				sess.SetAdapterSetupState(agentName, "pending")
			}
			intercepted = append(intercepted, syntheticResult(c.ID, c.Name, text))
		}
		return passThrough, intercepted
	}
}

// extractAgentName pulls the agent_name field out of an ExternalAgent
// tool_use's input JSON. Returns "" on any parse failure — the caller then
// passes the call through to its normal rejection path.
func extractAgentName(raw json.RawMessage) string {
	var in struct {
		AgentName string `json:"agent_name"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ""
	}
	return in.AgentName
}

// known reports whether the session's per-session extagent registry currently
// contains the named agent.
func known(env *tools.Env, name string) bool {
	if env == nil {
		return false
	}
	reg, ok := env.ExtAgentRegistry.(*extagent.Registry)
	if !ok || reg == nil {
		return false
	}
	return reg.Get(name) != nil
}

// runImplicitSetup invokes the agent_setup tool with binary=agentName and
// returns a tool-result text that includes the approval_id (or an error
// hint when setup itself failed). The original ExternalAgent call's
// parameters are echoed back so the model can re-emit them on retry —
// see add-llm-adapter-generator design G8 / spec §"Plan A".
//
// The second return is true when agent_setup actually registered an approval
// (i.e. emitted a non-error result). Callers use this to decide whether to
// burn the per-session dedup slot.
func runImplicitSetup(ctx context.Context, cfg ImplicitTriggerConfig, agentName string, origInput json.RawMessage) (string, bool) {
	if cfg.SetupTool == nil {
		return fmt.Sprintf("Adapter for %q was not registered and adapter-generator is unavailable. Use a different approach.", agentName), false
	}
	in := map[string]any{"binary": agentName}
	raw, _ := json.Marshal(in)
	res, err := cfg.SetupTool.Run(ctx, cfg.Env, raw)
	if err != nil {
		return fmt.Sprintf("Adapter for %q was not registered and setup failed: %v", agentName, err), false
	}
	registered := res != nil && !res.IsError
	body := fmt.Sprintf("Adapter for '%s' was not registered. agent_setup result: %s\n\nYour original ExternalAgent call's parameters were:\n%s\n\nOnce the user approves, retry the same ExternalAgent call verbatim.",
		agentName, res.Output, string(origInput))
	return body, registered
}

func syntheticResult(id, name, text string) ToolCallResult {
	return ToolCallResult{
		ID:   id,
		Name: name,
		Result: &tools.Result{
			Output:  text,
			IsError: false,
		},
	}
}
