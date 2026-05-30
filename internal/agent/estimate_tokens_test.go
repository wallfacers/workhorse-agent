package agent_test

import (
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/agent"
	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// #3: thinking content counts toward the context window, so EstimateTokens must
// include it — otherwise a thinking-heavy active chain never trips
// auto-compaction and eventually overflows the model window.
func TestEstimateTokens_IncludesThinking(t *testing.T) {
	thinkingMsg := []provider.Message{{
		Role: provider.RoleAssistant,
		Content: []provider.ContentBlock{{
			Type:      provider.BlockThinking,
			Thinking:  strings.Repeat("x", 4000),
			Signature: strings.Repeat("s", 400),
		}},
	}}
	got := agent.EstimateTokens(thinkingMsg)
	if got == 0 {
		t.Fatal("EstimateTokens counted a 4400-char thinking block as 0 tokens")
	}
	// ~4400 chars / 4 ≈ 1100 tokens; assert it is in the right ballpark.
	if got < 1000 {
		t.Errorf("thinking bytes under-counted: got %d tokens for ~4400 chars", got)
	}

	redactedMsg := []provider.Message{{
		Role:    provider.RoleAssistant,
		Content: []provider.ContentBlock{{Type: provider.BlockRedactedThinking, RedactedData: strings.Repeat("d", 800)}},
	}}
	if agent.EstimateTokens(redactedMsg) == 0 {
		t.Error("EstimateTokens counted redacted_thinking data as 0 tokens")
	}
}
