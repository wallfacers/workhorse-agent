package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// modelCaller is one text-in/text-out call against the benchmark model.
type modelCaller func(ctx context.Context, system, user string) (string, error)

// newModelCaller wraps a provider.Provider into a modelCaller.
func newModelCaller(p provider.Provider, model string, maxTokens int) modelCaller {
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
		return sb.String(), nil
	}
}

const answerSystemPrompt = `You answer a question about a long conversation using ONLY the retrieved memories provided. Rules:
- Answer with the shortest phrase that fully answers the question — a name, a date, a place, a list. No explanation, no restating the question.
- For "when" questions, read the time from the memory's [event: YYYY-MM-DD] marker (that is when it happened). NEVER answer relative to today's date. Answer at the granularity the memory supports (a month like "May 2023" is fine if that is all that is known).
- Make your best supported inference from the evidence — combine multiple memories if needed. Only reply "I don't know" when NO retrieved memory is relevant to the question at all; do not bail out just because the phrasing differs.`

// openDomainAnswerPrompt relaxes the grounding rule for open-domain questions
// (LoCoMo category 3), which probe opinion, motivation, and likely behavior
// rather than exact fact lookup. Mirrors AtomMem's split prompt design: ground
// in memories, but reason with common sense and world knowledge on top.
const openDomainAnswerPrompt = `You answer a question about a person based on retrieved memories from their long conversation. This question asks about opinions, motivations, preferences, or likely behavior — not an exact fact lookup. Rules:
- Ground your answer in the retrieved memories: use them to understand the person's traits, habits, values, and past events.
- COMBINE the memories with common sense, cause-and-effect reasoning, and world knowledge to infer the most plausible answer. An answer supported by reasonable inference is far better than "I don't know".
- Answer with a short, direct phrase or sentence. No explanation, no restating the question.
- Only reply "I don't know" when the memories offer no basis whatsoever for even an informed inference.`

// answerPromptFor picks the system prompt by LoCoMo category
// (3 = open-domain; everything else is extraction-style).
func answerPromptFor(category int) string {
	if category == 3 {
		return openDomainAnswerPrompt
	}
	return answerSystemPrompt
}

// queryRewriteSystemPrompt turns a failed question into an alternative retrieval
// query (EverMemOS-style second-round rewriting, triggered only on IDK).
const queryRewriteSystemPrompt = `A memory search for the following question returned nothing relevant. Write ONE alternative search query for the same information need: use different words — synonyms, the underlying event or object, likely entity names — not a rephrasing of the question. Output ONLY the query text, a short keyword-style phrase, no quotes, no explanation.`

// isIDK reports whether a predicted answer is an "I don't know" bail-out.
func isIDK(predicted string) bool {
	p := strings.ToLower(strings.TrimSpace(predicted))
	if p == "" {
		return true
	}
	return strings.Contains(p, "don't know") || strings.Contains(p, "do not know") ||
		strings.Contains(p, "not mentioned") || strings.Contains(p, "no information")
}

func buildAnswerPrompt(question string, memories []retrievedMemory) string {
	var b strings.Builder
	b.WriteString("RETRIEVED MEMORIES:\n")
	if len(memories) == 0 {
		b.WriteString("(none)\n")
	}
	for i, m := range memories {
		fmt.Fprintf(&b, "%d. %s\n", i+1, m.Line())
	}
	fmt.Fprintf(&b, "\nQUESTION: %s\n\nAnswer:", question)
	return b.String()
}

// retrievedMemory is one hit passed to the answering model.
type retrievedMemory struct {
	Content   string
	EventDate string // rendered date or ""
	Recorded  string
}

// Line renders a memory with its time markers, mirroring MemorySearch output so
// the answering model sees the same time-aware context the agent would.
func (m retrievedMemory) Line() string {
	var b strings.Builder
	if m.EventDate != "" {
		fmt.Fprintf(&b, "[event: %s] ", m.EventDate)
	}
	if m.Recorded != "" {
		fmt.Fprintf(&b, "[recorded: %s] ", m.Recorded)
	}
	b.WriteString(m.Content)
	return b.String()
}

// judgeSystemPrompt aligns with the open mem0ai/memory-benchmarks LLM-as-a-Judge:
// a lenient semantic-equivalence check, not exact string match.
const judgeSystemPrompt = `You grade a predicted answer against a gold answer for a question about a conversation, aligned with the LoCoMo / mem0 LLM-as-a-judge convention. Output STRICT JSON only: {"correct": true|false}.

Mark "correct": true when the prediction conveys the SAME key fact as the gold answer. Be lenient on form, strict on fact:
- Ignore wording, verbosity, and extra correct detail. A more detailed answer that still contains the gold fact is correct (e.g. gold "reminding herself of her successes" vs prediction "she reminds herself of her successes and progress" → true).
- Accept synonyms and paraphrases of the same fact (e.g. "a trophy" vs "first place" for a contest prize → true).
- Accept a coarser-but-consistent date (gold "May 2023" vs prediction "May 2023" or "8 May 2023" → true); mark false only if the date actually differs.
- Mark false when the prediction contradicts the gold fact, omits it, gives a wrong name/date/number, or says it does not know.`

func buildJudgePrompt(question, gold, predicted string) string {
	return fmt.Sprintf("QUESTION: %s\n\nGOLD ANSWER: %s\n\nPREDICTED ANSWER: %s\n\nReturn the JSON verdict now.", question, gold, predicted)
}

// parseJudgeVerdict extracts {"correct": bool} tolerantly.
func parseJudgeVerdict(raw string) bool {
	lower := strings.ToLower(raw)
	// Fast path: find "correct" then the next true/false token.
	idx := strings.Index(lower, "correct")
	if idx < 0 {
		return false
	}
	rest := lower[idx:]
	tIdx := strings.Index(rest, "true")
	fIdx := strings.Index(rest, "false")
	switch {
	case tIdx < 0:
		return false
	case fIdx < 0:
		return true
	default:
		return tIdx < fIdx
	}
}
