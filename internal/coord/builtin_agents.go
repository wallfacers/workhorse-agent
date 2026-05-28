package coord

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
)

// AdapterGeneratorTypeName is the canonical name of the adapter-generator
// agent type, exported so callers (the agent_setup tool, the runner factory's
// lockdown branch) can spell it consistently.
const AdapterGeneratorTypeName = "adapter-generator"

// adapterGeneratorEnforcedTools is the fixed allow/deny list applied to any
// session whose agent_type is adapter-generator, regardless of what the YAML
// on disk says. See applyAdapterGeneratorLockdown.
var (
	adapterGeneratorAllow = []string{"Bash", "Read", "WriteAdapterDraft"}
	adapterGeneratorDeny  = []string{"Dispatch", "ExternalAgent", "agent_setup", "Write", "Edit"}
)

// loadBuiltinAgents reads every embedded agent-type YAML (sourced from the
// extagent package) and returns them as AgentType values. Each returned
// value has already been passed through applyAdapterGeneratorLockdown so the
// in-memory copy is identical to what the runtime will see — no surprise
// drift between YAML and enforcement.
func loadBuiltinAgents() ([]AgentType, error) {
	names, err := extagent.BuiltinAgentNames()
	if err != nil {
		return nil, err
	}
	out := make([]AgentType, 0, len(names))
	for _, name := range names {
		raw, ok := extagent.BuiltinAgentYAML(name)
		if !ok {
			continue
		}
		var y agentYAML
		if err := yaml.Unmarshal(raw, &y); err != nil {
			return nil, fmt.Errorf("coord: parse embedded agent %q: %w", name, err)
		}
		if y.Name == "" {
			y.Name = name
		}
		if y.MaxIterations <= 0 {
			y.MaxIterations = 50
		}
		at := AgentType{
			Name:          y.Name,
			Description:   y.Description,
			SystemPrompt:  y.SystemPrompt,
			AllowTools:    append([]string(nil), y.Tools.Allow...),
			DenyTools:     append([]string(nil), y.Tools.Deny...),
			Provider:      y.Provider,
			Model:         y.Model,
			MaxIterations: y.MaxIterations,
		}
		applyAdapterGeneratorLockdown(&at)
		out = append(out, at)
	}
	return out, nil
}

// applyAdapterGeneratorLockdown rewrites AllowTools and DenyTools for the
// adapter-generator type to the hardcoded values. Any YAML override is
// ignored — the defense-in-depth posture from add-llm-adapter-generator
// design G1/G4 requires the effective surface to be unspoofable by a
// tampered file.
func applyAdapterGeneratorLockdown(at *AgentType) {
	if at == nil || at.Name != AdapterGeneratorTypeName {
		return
	}
	at.AllowTools = append([]string(nil), adapterGeneratorAllow...)
	at.DenyTools = append([]string(nil), adapterGeneratorDeny...)
}

// AdapterGeneratorAllowTools returns a copy of the canonical allow list.
// Exported so the runner factory can use the same source of truth.
func AdapterGeneratorAllowTools() []string {
	return append([]string(nil), adapterGeneratorAllow...)
}

// AdapterGeneratorDenyTools returns a copy of the canonical deny list.
func AdapterGeneratorDenyTools() []string {
	return append([]string(nil), adapterGeneratorDeny...)
}
