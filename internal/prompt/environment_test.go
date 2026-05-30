package prompt_test

import (
	"fmt"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/prompt"
)

func TestEnvironmentBlock_Empty(t *testing.T) {
	got := prompt.EnvironmentBlock(prompt.EnvironmentInput{})
	if got != "" {
		t.Errorf("empty input should produce empty output, got %q", got)
	}
}

func TestEnvironmentBlock_Full(t *testing.T) {
	input := prompt.EnvironmentInput{
		OS:    "linux",
		Shell: "bash",
		CWD:   "/home/user/project",
		CLITools: []prompt.CLIToolEntry{
			{Name: "git", Path: "/usr/bin/git", Version: "2.43.0"},
			{Name: "pandoc", Path: "/usr/bin/pandoc"},
		},
		SubAgents: []prompt.SubAgentHint{
			{Name: "claude-code", Description: "Anthropic's coding agent", Resumable: true},
		},
	}
	got := prompt.EnvironmentBlock(input)
	// Must contain sections in order.
	if err := assertContains(t, got, "os: linux"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "shell: bash"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "cwd: /home/user/project"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "cli_tools"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "sub_agents"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "git @ /usr/bin/git (2.43.0)"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "pandoc @ /usr/bin/pandoc"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "claude-code: Anthropic's coding agent (resumable)"); err != nil {
		t.Error(err)
	}
}

func TestEnvironmentBlock_ByteStable(t *testing.T) {
	input := prompt.EnvironmentInput{
		OS:    "linux",
		Shell: "bash",
		CLITools: []prompt.CLIToolEntry{
			{Name: "b-tool", Path: "/b"},
			{Name: "a-tool", Path: "/a"},
		},
	}
	first := prompt.EnvironmentBlock(input)
	second := prompt.EnvironmentBlock(input)
	if first != second {
		t.Errorf("output not byte-stable:\nfirst:  %q\nsecond: %q", first, second)
	}
}

func TestEnvironmentBlock_SortingStable(t *testing.T) {
	input := prompt.EnvironmentInput{
		CLITools: []prompt.CLIToolEntry{
			{Name: "zebra", Path: "/z"},
			{Name: "alpha", Path: "/a"},
			{Name: "middle", Path: "/m"},
		},
	}
	got := prompt.EnvironmentBlock(input)
	// alpha should appear before middle, middle before zebra.
	alphaIdx := indexOf(got, "alpha")
	middleIdx := indexOf(got, "middle")
	zebraIdx := indexOf(got, "zebra")
	if alphaIdx >= middleIdx || middleIdx >= zebraIdx {
		t.Errorf("entries not sorted alphabetically: alpha=%d, middle=%d, zebra=%d", alphaIdx, middleIdx, zebraIdx)
	}
}

func TestEnvironmentBlock_NoTools(t *testing.T) {
	input := prompt.EnvironmentInput{
		OS:    "linux",
		Shell: "bash",
		CWD:   "/home",
	}
	got := prompt.EnvironmentBlock(input)
	if err := assertContains(t, got, "<environment>"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "cli_tools"); err == nil {
		t.Error("should not contain cli_tools section when empty")
	}
	if err := assertContains(t, got, "sub_agents"); err == nil {
		t.Error("should not contain sub_agents section when empty")
	}
}

func TestEnvironmentBlock_SubAgentsNonResumable(t *testing.T) {
	input := prompt.EnvironmentInput{
		SubAgents: []prompt.SubAgentHint{
			{Name: "codex", Description: "Review tool", Resumable: false},
		},
	}
	got := prompt.EnvironmentBlock(input)
	if err := assertContains(t, got, "codex: Review tool"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "resumable"); err == nil {
		t.Error("non-resumable agent should not have (resumable) suffix")
	}
}

func TestEnvironmentBlock_DispatchAgents(t *testing.T) {
	input := prompt.EnvironmentInput{
		OS: "linux",
		DispatchAgents: []prompt.SubAgentHint{
			{Name: "zeta", Description: "Last role"},
			{Name: "general-purpose", Description: "General sub-agent"},
		},
	}
	got := prompt.EnvironmentBlock(input)
	if err := assertContains(t, got, "dispatch_agents (invoke via Dispatch tool, pass name as agent_type):"); err != nil {
		t.Error(err)
	}
	if err := assertContains(t, got, "- general-purpose: General sub-agent"); err != nil {
		t.Error(err)
	}
	// Sorted alphabetically: general-purpose before zeta.
	if indexOf(got, "general-purpose") >= indexOf(got, "zeta") {
		t.Error("dispatch_agents not sorted alphabetically")
	}
}

func TestEnvironmentBlock_NoDispatchAgents(t *testing.T) {
	got := prompt.EnvironmentBlock(prompt.EnvironmentInput{OS: "linux"})
	if err := assertContains(t, got, "dispatch_agents"); err == nil {
		t.Error("should not contain dispatch_agents section when empty")
	}
}

func assertContains(t *testing.T, haystack, needle string) error {
	t.Helper()
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if haystack[i:i+len(needle)] == needle {
			return nil
		}
	}
	return fmt.Errorf("substring %q not found in output", needle)
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
