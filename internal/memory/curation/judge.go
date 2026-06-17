package curation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/prompt"
	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// MergedEntry is the surviving entry a merge decision produces (design D4
// memory_merge `into`). Durability/Category may be empty (the applier falls back
// to sensible defaults).
type MergedEntry struct {
	Name       string `json:"name"`
	Trigger    string `json:"trigger"`
	Content    string `json:"content"`
	Durability string `json:"durability"`
	Category   string `json:"category"`
}

// MergeDecision collapses Names into the single Into entry.
type MergeDecision struct {
	Names []string    `json:"names"`
	Into  MergedEntry `json:"into"`
}

// JudgeDecision is the structured keep/evict/merge verdict (design D5). Entries
// not named in Evict or any merge are kept (conservative default).
type JudgeDecision struct {
	Evict []string        `json:"evict"`
	Merge []MergeDecision `json:"merge"`
}

// ModelCaller performs one text-in/text-out model call. The runtime wires the
// real provider via NewProviderCaller; tests inject a deterministic mock so the
// judge → mutation flow runs CI-safe with no real model call (design test 7.7).
type ModelCaller func(ctx context.Context, system, user string) (string, error)

// NewProviderCaller builds a ModelCaller bound to the configured judge_model
// (design D5: a small cheap model, not the main agent model). judgeModel is the
// "provider:model" string; maxTokens caps the structured output. It resolves the
// provider from the registry once at construction and returns an error if the
// model string is malformed or its provider is not configured.
func NewProviderCaller(providers map[string]provider.Provider, judgeModel string, maxTokens int) (ModelCaller, error) {
	name, model := provider.SplitProviderModel(judgeModel)
	if name == "" {
		return nil, fmt.Errorf("curation: judge_model %q is missing a provider prefix (want \"provider:model\")", judgeModel)
	}
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("curation: provider %q for judge_model is not configured", name)
	}
	return func(ctx context.Context, system, user string) (string, error) {
		req := provider.Request{
			Model:     model,
			System:    system,
			MaxTokens: maxTokens,
			Messages: []provider.Message{{
				Role:    provider.RoleUser,
				Content: []provider.ContentBlock{{Type: provider.BlockText, Text: user}},
			}},
		}
		ch, err := p.Stream(ctx, req)
		if err != nil {
			return "", err // request never left the ground
		}
		var sb strings.Builder
		for ev := range ch {
			switch ev.Type {
			case provider.EventTextDelta:
				sb.WriteString(ev.TextDelta)
			case provider.EventError:
				if ev.Error != nil {
					return "", ev.Error
				}
			}
		}
		return sb.String(), nil
	}, nil
}

// Judge runs one judgment pass: render the prompt, call the model, parse the
// JSON verdict. A call failure or unparseable output is returned as an error;
// the worker treats any error as a fail-safe no-op (design D5).
func Judge(ctx context.Context, call ModelCaller, candidates []prompt.CurationJudgeCandidate, clusters []prompt.CurationJudgeCluster) (*JudgeDecision, error) {
	user := prompt.BuildCurationJudgeUserPrompt(candidates, clusters)
	raw, err := call(ctx, prompt.CurationJudgeSystemPrompt, user)
	if err != nil {
		return nil, fmt.Errorf("curation: judge model call: %w", err)
	}
	return parseJudgeDecision(raw)
}

// parseJudgeDecision extracts and unmarshals the JSON verdict, tolerating a
// model that wraps the object in prose or ```json fences.
func parseJudgeDecision(raw string) (*JudgeDecision, error) {
	js := extractJSON(raw)
	if js == "" {
		return nil, fmt.Errorf("curation: judge produced no JSON object: %q", truncate(raw, 200))
	}
	var d JudgeDecision
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		return nil, fmt.Errorf("curation: parse judge JSON: %w (raw: %q)", err, truncate(js, 200))
	}
	return &d, nil
}

// extractJSON returns the substring from the first '{' to the last '}' inclusive,
// which strips leading/trailing prose and code fences without a full tokenizer.
// Returns "" when no brace pair is present.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
