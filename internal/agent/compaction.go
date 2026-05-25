package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// Compactor summarises the first N−K messages of a session's history using a
// "fast" provider (Anthropic Haiku / OpenAI gpt-4o-mini) so the remaining
// context fits comfortably under the model's window. The retain rules come
// straight from the agent-loop spec:
//
//   - Keep the most recent RecentKeep messages verbatim.
//   - Always keep every error tool_result, no matter how old (so the model
//     can still reason about what went wrong).
//   - Prepend one synthesised system-role message holding the summary.
type Compactor struct {
	Provider     provider.Provider
	Model        string
	RecentKeep   int
	MaxTokens    int
	SystemPrompt string
}

// CompactionResult captures the before/after token counts so callers can
// surface them on the `compaction` SSE event.
type CompactionResult struct {
	BeforeMessages int
	AfterMessages  int
	BeforeTokens   int
	AfterTokens    int
	Summary        string
}

// Compact returns a new history slice the loop should swap in. It does NOT
// mutate the input. When history is short enough that summarising would lose
// more than it gains (≤ RecentKeep+1 entries), Compact returns the original
// slice unchanged and ok=false.
func (c *Compactor) Compact(ctx context.Context, history []provider.Message) ([]provider.Message, CompactionResult, bool, error) {
	keep := c.RecentKeep
	if keep <= 0 {
		keep = 8
	}
	if len(history) <= keep+1 {
		return history, CompactionResult{}, false, nil
	}

	// Split into "old" (to summarise) and "recent" (verbatim).
	cut := len(history) - keep
	old := history[:cut]
	recent := history[cut:]

	// Pull out every error tool_result from old; they bypass the summary.
	var preserved []provider.ContentBlock
	for _, m := range old {
		for _, b := range m.Content {
			if b.Type == provider.BlockToolResult && b.IsError {
				preserved = append(preserved, b)
			}
		}
	}

	summary, err := c.summarise(ctx, old)
	if err != nil {
		return history, CompactionResult{}, false, fmt.Errorf("compaction: summarise: %w", err)
	}

	// Build new history: [summary system message] + [preserved error tool_results
	// in a single user-role message] + [recent K messages].
	newHistory := make([]provider.Message, 0, 2+len(recent))
	newHistory = append(newHistory, provider.Message{
		Role: provider.RoleSystem,
		Content: []provider.ContentBlock{{
			Type: provider.BlockText,
			Text: summary,
		}},
	})
	if len(preserved) > 0 {
		newHistory = append(newHistory, provider.Message{
			Role:    provider.RoleUser,
			Content: preserved,
		})
	}
	newHistory = append(newHistory, recent...)

	return newHistory, CompactionResult{
		BeforeMessages: len(history),
		AfterMessages:  len(newHistory),
		BeforeTokens:   EstimateTokens(history),
		AfterTokens:    EstimateTokens(newHistory),
		Summary:        summary,
	}, true, nil
}

// summarise asks the fast provider to condense the given messages into a
// single paragraph the loop can prepend as a system message. The prompt is
// kept short and instruction-only — we never fold the user's content into the
// system prompt directly.
func (c *Compactor) summarise(ctx context.Context, msgs []provider.Message) (string, error) {
	req := provider.Request{
		Model: c.Model,
		System: "You are a conversation summariser. Read the messages provided " +
			"and produce a single dense paragraph (≤ 400 tokens) capturing every " +
			"factual claim, decision, and open question. Do not editorialise. " +
			"Do not include greetings or meta-commentary about the summary itself.",
		Messages:  msgs,
		MaxTokens: c.MaxTokens,
	}
	ch, err := c.Provider.Stream(ctx, req)
	if err != nil {
		return "", err
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
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "(compaction summary unavailable)", nil
	}
	return out, nil
}

// EstimateTokens returns a rough token count for a history slice. The
// estimate uses a "≈ 4 chars per token" heuristic that's good enough for the
// 0.85 compaction threshold check — exact counting needs a tokenizer per
// model family which we don't carry yet.
func EstimateTokens(msgs []provider.Message) int {
	chars := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			chars += len(b.Text) + len(b.Output) + len(b.ToolName) + len(b.Input)
		}
	}
	if chars == 0 {
		return 0
	}
	tokens := chars / 4
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}
