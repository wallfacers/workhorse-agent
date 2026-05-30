package prompt

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// EnvironmentInput carries the data for the <environment> block.
type EnvironmentInput struct {
	OS        string
	Shell     string
	CWD       string
	CLITools  []CLIToolEntry
	SubAgents []SubAgentHint
	// DispatchAgents are the agent_type roles invokable via the Dispatch tool's
	// agent_type parameter. Distinct from SubAgents, which are external CLIs
	// reached through the ExternalAgent tool.
	DispatchAgents []SubAgentHint
}

type CLIToolEntry struct {
	Name    string
	Path    string
	Version string
}

type SubAgentHint struct {
	Name        string
	Description string
	Resumable   bool
}

// EnvironmentBlock renders the <environment> block for the system prompt.
// Returns empty string when input produces no content (no tools, no agents).
// Output is byte-stable across calls with identical inputs.
func EnvironmentBlock(input EnvironmentInput) string {
	var sections []string

	// Static fields: os, shell, cwd.
	var staticLines []string
	if input.OS != "" {
		staticLines = append(staticLines, "os: "+input.OS)
	}
	if input.Shell != "" {
		staticLines = append(staticLines, "shell: "+input.Shell)
	}
	if input.CWD != "" {
		staticLines = append(staticLines, "cwd: "+input.CWD)
	}

	// CLI tools section.
	var toolLines []string
	if len(input.CLITools) > 0 {
		// Already sorted by caller, but sort for safety.
		sorted := make([]CLIToolEntry, len(input.CLITools))
		copy(sorted, input.CLITools)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, t := range sorted {
			if t.Version != "" {
				toolLines = append(toolLines, fmt.Sprintf("- %s @ %s (%s)", t.Name, t.Path, t.Version))
			} else {
				toolLines = append(toolLines, fmt.Sprintf("- %s @ %s", t.Name, t.Path))
			}
		}
	}

	// Sub-agents section.
	var agentLines []string
	if len(input.SubAgents) > 0 {
		sorted := make([]SubAgentHint, len(input.SubAgents))
		copy(sorted, input.SubAgents)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, a := range sorted {
			suffix := ""
			if a.Resumable {
				suffix = " (resumable)"
			}
			agentLines = append(agentLines, fmt.Sprintf("- %s: %s%s", a.Name, a.Description, suffix))
		}
	}

	// Dispatch agent_type roles section.
	var dispatchLines []string
	if len(input.DispatchAgents) > 0 {
		sorted := make([]SubAgentHint, len(input.DispatchAgents))
		copy(sorted, input.DispatchAgents)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, a := range sorted {
			dispatchLines = append(dispatchLines, fmt.Sprintf("- %s: %s", a.Name, a.Description))
		}
	}

	// Build sections.
	if len(staticLines) > 0 {
		sections = append(sections, strings.Join(staticLines, "\n"))
	}
	if len(toolLines) > 0 {
		sections = append(sections, "cli_tools (invoke via Bash):\n"+strings.Join(toolLines, "\n"))
	}
	if len(agentLines) > 0 {
		sections = append(sections, "sub_agents (invoke via ExternalAgent tool):\n"+strings.Join(agentLines, "\n"))
	}
	if len(dispatchLines) > 0 {
		sections = append(sections, "dispatch_agents (invoke via Dispatch tool, pass name as agent_type):\n"+strings.Join(dispatchLines, "\n"))
	}

	if len(sections) == 0 {
		return ""
	}

	return "<environment>\n" + strings.Join(sections, "\n\n") + "\n</environment>"
}

// DetectOS returns a human-friendly OS string.
func DetectOS() string {
	return runtime.GOOS
}
