package config_test

import (
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/config"
)

// #1: when thinking is enabled, agent.max_tokens must exceed
// agent.thinking.budget_tokens (Anthropic's constraint). Validate must catch
// the documented budget_tokens:16000 against the default max_tokens:4096 at
// startup instead of letting every request 400.
func TestValidate_ThinkingBudgetMustBeUnderMaxTokens(t *testing.T) {
	c := config.Default()
	c.Agent.Thinking.Enabled = true
	c.Agent.Thinking.BudgetTokens = 16000 // > default max_tokens (4096)

	err := config.Validate(c)
	if err == nil {
		t.Fatal("expected validation error when budget_tokens >= max_tokens")
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error should mention max_tokens, got %q", err.Error())
	}

	// Raising max_tokens above the budget makes the config valid.
	c.Agent.MaxTokens = 24000
	if err := config.Validate(c); err != nil {
		t.Fatalf("config with max_tokens > budget should validate, got %v", err)
	}
}

// #1: max_tokens has a sane lower bound so a misconfigured 0 can't slip through.
func TestValidate_MaxTokensLowerBound(t *testing.T) {
	c := config.Default()
	c.Agent.MaxTokens = 0
	if err := config.Validate(c); err == nil {
		t.Fatal("expected validation error for agent.max_tokens = 0")
	}
}
