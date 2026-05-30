package prompt_test

import (
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/prompt"
)

// Scenario: 默认提示词含引导 — DefaultBasePrompt guides TodoWrite usage.
func TestDefaultBasePrompt_TaskListGuidance(t *testing.T) {
	p := prompt.DefaultBasePrompt
	for _, want := range []string{"TodoWrite", "in_progress", "completed", "three or more steps"} {
		if !strings.Contains(p, want) {
			t.Errorf("DefaultBasePrompt missing task-list guidance phrase %q", want)
		}
	}
}
