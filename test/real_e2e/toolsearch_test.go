//go:build real_e2e

package real_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/test/real_e2e/judge"
)

// weatherTool is a deferrable fake tool. It is withheld from the model's
// initial tool list (only its name appears in <available-deferred-tools>) so
// the model must discover it via ToolSearch before calling it. The MCP-style
// server__tool name verifies the parser handles that convention.
type weatherTool struct{}

func (weatherTool) Name() string        { return "weather__forecast" }
func (weatherTool) Description() string { return "Get the current weather forecast for a given city." }
func (weatherTool) InputSchema() json.RawMessage {
	return []byte(`{"type":"object","properties":{"city":{"type":"string","description":"City name, e.g. Paris"}},"required":["city"]}`)
}
func (weatherTool) IsReadOnly() bool              { return true }
func (weatherTool) CanRunInParallel() bool        { return true }
func (weatherTool) DefaultTimeout() time.Duration { return 10 * time.Second }
func (weatherTool) ShouldDefer() bool             { return true }

func (weatherTool) Run(_ context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var p struct {
		City string `json:"city"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.City == "" {
		p.City = "unknown"
	}
	// Canned, distinctive result so the judge can verify the answer reflects it.
	return &tools.Result{Output: fmt.Sprintf(`{"city":%q,"condition":"sunny","temp_c":22,"wind_kph":11}`, p.City)}, nil
}

func toolUsed(trace *judge.Trace, name string) bool {
	for _, turn := range trace.Turns {
		for _, c := range turn.ToolCalls {
			if c.ToolName == name {
				return true
			}
		}
	}
	return false
}

// TestToolSearch_DiscoverAndCall_Integration drives the full tool-search loop
// against a real model: the deferrable weather tool is hidden behind
// ToolSearch, and the model must discover then call it. Structural assertions
// catch the mechanism; the LLM judge grades the overall behavior.
func TestToolSearch_DiscoverAndCall_Integration(t *testing.T) {
	trace, result := runScenario(t, scenarioConfig{
		UserMessage:    "What's the weather forecast for Paris right now? Use the tools available to you.",
		Rubric:         toolSearchRubric,
		Timeout:        90 * time.Second,
		ToolSearchMode: "tst",
		ExtraTools:     []tools.Tool{weatherTool{}},
	})
	t.Logf("Trace: %d turns", len(trace.Turns))

	if !toolUsed(trace, tools.ToolSearchName) {
		t.Errorf("expected the model to call ToolSearch to discover the deferred tool")
	}
	if !toolUsed(trace, "weather__forecast") {
		t.Errorf("expected the model to call weather__forecast after discovery")
	}
	assertVerdict(t, result)
}
