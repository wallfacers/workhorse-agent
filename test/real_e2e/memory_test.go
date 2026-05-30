//go:build real_e2e

package real_e2e

import (
	"testing"
	"time"
)

func TestMemoryWrite_Read_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Write 'E2E memory test content' to memory (kind: memory), then read it back and confirm it matches.",
		Rubric:      memoryRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}

func TestMemoryCrossSession_Integration(t *testing.T) {
	traceA, resultA := runScenario(t, scenarioConfig{
		UserMessage: "Use the memory_write tool to save 'cross-session test data' to memory (kind: memory). Do not read first, just write directly.",
		Rubric:      memoryRubric,
		Timeout:     90 * time.Second,
	})
	t.Logf("Session A: %d turns", len(traceA.Turns))
	assertVerdict(t, resultA)

	traceB, resultB := runScenario(t, scenarioConfig{
		UserMessage: "Use the memory_read tool to read memory (kind: memory) and tell me what it contains.",
		Rubric:      memoryRubric,
		Timeout:     60 * time.Second,
	})
	t.Logf("Session B: %d turns", len(traceB.Turns))
	assertVerdict(t, resultB)
}

func TestSessionSearch_Basic_Smoke(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage: "Search all sessions for the term 'workhorse-agent'.",
		Rubric:      sessionSearchRubric,
		Timeout:     60 * time.Second,
	})
	t.Logf("Trace: %d turns", len(trace.Turns))
	assertVerdict(t, result)
}
