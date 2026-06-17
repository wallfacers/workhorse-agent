package prompt

import (
	"fmt"
	"strings"
)

// CurationJudgeCandidate is one low-value entry presented to the curation judge
// for a keep-or-evict decision. Fields are plain data so the prompt package
// stays IO-free and free of memory-package dependencies (see boundary_test).
type CurationJudgeCandidate struct {
	Name       string
	Trigger    string
	Content    string // may be a truncated snippet
	Durability string
	Category   string
	HitCount   int
	AgeDays    int
	Score      float64
}

// CurationJudgeCluster is a group of near-duplicate entry names the judge may
// collapse into one via a merge decision.
type CurationJudgeCluster struct {
	Names []string
}

// CurationJudgeSystemPrompt instructs a small, cheap model to act as a bounded
// memory curator. It is deliberately strict about output shape: pure JSON, no
// prose, conservative defaults (keep when unsure) so a confused judge degrades
// to a no-op rather than deleting good memories.
const CurationJudgeSystemPrompt = `You are a memory curator for an AI agent. You are given a bounded set of LOW-VALUE memory entries (eviction candidates) and groups of NEAR-DUPLICATE entries. Decide, conservatively, which to evict and which to merge.

Rules:
- Default to KEEP. Only evict an entry that is clearly obsolete, redundant, trivial, or superseded. When unsure, keep it.
- Evict by listing the entry's exact "name". An entry you do not mention is kept.
- For a near-duplicate group, you MAY merge it into ONE entry: provide the merged "into" entry (name, trigger, content, durability, category) and the list of source "names" it replaces. Choose the most useful surviving name; write a single trigger line and concise merged content that preserves every distinct fact. Do not merge entries that are merely on the same topic but carry different facts.
- Never invent names. Every name in "evict" and in any "names" list MUST be one of the names shown below.
- Output STRICT JSON and nothing else — no markdown, no code fences, no commentary. Shape:
{"evict":["name", ...],"merge":[{"names":["a","b"],"into":{"name":"a","trigger":"...","content":"...","durability":"volatile","category":"..."}}]}
- If there is nothing worth changing, output {"evict":[],"merge":[]}.`

// BuildCurationJudgeUserPrompt renders the candidate list and near-duplicate
// clusters into the user message for one judgment pass. The format is compact
// and stable so it is cheap and deterministic.
func BuildCurationJudgeUserPrompt(candidates []CurationJudgeCandidate, clusters []CurationJudgeCluster) string {
	var b strings.Builder

	b.WriteString("EVICTION CANDIDATES (lowest value first):\n")
	if len(candidates) == 0 {
		b.WriteString("(none)\n")
	}
	for _, c := range candidates {
		fmt.Fprintf(&b, "- name: %s\n  durability: %s | category: %s | hits: %d | age_days: %d | score: %.3f\n  trigger: %s\n  content: %s\n",
			c.Name, c.Durability, c.Category, c.HitCount, c.AgeDays, c.Score,
			oneLine(c.Trigger), oneLine(c.Content))
	}

	b.WriteString("\nNEAR-DUPLICATE CLUSTERS (consider merging each group):\n")
	if len(clusters) == 0 {
		b.WriteString("(none)\n")
	}
	for i, cl := range clusters {
		fmt.Fprintf(&b, "- cluster %d: %s\n", i+1, strings.Join(cl.Names, ", "))
	}

	b.WriteString("\nReturn the JSON decision now.")
	return b.String()
}

// oneLine collapses newlines so a single entry cannot break the line-oriented
// prompt layout. Whitespace runs become single spaces.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
