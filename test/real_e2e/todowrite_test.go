//go:build real_e2e

package real_e2e

import (
	"testing"
	"time"
)

func TestTodoWrite_CreateList_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Create a task list with these three items: 1) Read the config file, 2) Validate the schema, 3) Write the output. Mark them all as pending.",
		Rubric:      todoWriteRubric,
		Timeout:     60 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestTodoWrite_UpdateStatus_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Create a task list with: 'Set up environment' (pending), 'Run tests' (pending), 'Deploy' (pending). Then update 'Set up environment' to completed.",
		Rubric:      todoWriteRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestTodoWrite_MultiStepTracking_Full(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "I need to refactor the codebase. Create a task list: 'Identify deprecated APIs', 'Write migration guide', 'Update tests', 'Bump version'. Then mark 'Identify deprecated APIs' as in_progress. Then mark it completed and 'Write migration guide' as in_progress.",
		Rubric:      todoWriteRubric,
		Timeout:     120 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
