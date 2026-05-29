//go:build real_e2e

package real_e2e

import (
	"testing"
	"time"
)

func TestExtAgent_Invoke_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Use the echo agent to run 'echo hello from E2E test'.",
		Rubric:      extAgentRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestExtAgent_Error_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Use the external agent called 'nonexistent-agent-xyz' to do something.",
		Rubric:      extAgentRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
