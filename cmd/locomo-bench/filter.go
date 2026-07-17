package main

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

// Listwise LLM filter (EMem-style, the most critical component in its
// ablation): ONE call sees the question plus the whole wide candidate pool and
// selects the relevant set. Unlike a pairwise cross-encoder — which scores each
// memory independently and evicts the complementary facts multi-fact questions
// need (the v4 regression) — a listwise selector judges relevance in context of
// the full pool, so complementary evidence survives.
const listwiseFilterPrompt = `You select which retrieved memories are relevant for answering a question about a long conversation. You will see the question and a numbered list of candidate memories (some may be truncated). Rules:
- Select EVERY memory that could help answer the question — directly, or as one piece of a multi-part answer (one item of a list, one hop of a chain, a date anchor). Err on the side of inclusion: missing a relevant memory is far worse than keeping an extra one.
- Output ONLY the selected numbers as a comma-separated list (e.g. 3, 7, 12). No explanation, no other text.`

// filterLineMaxChars truncates each candidate's rendering in the filter prompt
// (chunks are ~900 chars; the selector only needs enough to judge relevance,
// and the answering model still sees the full content of what survives).
const filterLineMaxChars = 350

// retrieveFiltered retrieves a wide quota'd pool, asks the filter model to
// select the relevant subset, and returns it in fused order capped at topK.
// Any failure (call error, nothing parsed) falls back to the plain quota'd
// top-k so a flaky filter can never do worse than the unfiltered baseline.
func retrieveFiltered(ctx context.Context, r *memory.Retriever, call modelCaller, query string, topK, quota, pool int) ([]memory.Result, error) {
	if pool <= topK {
		return retrieveWithQuota(ctx, r, query, topK, quota)
	}
	scaledQuota := 0
	if quota > 0 {
		scaledQuota = quota * pool / topK
	}
	wide, err := retrieveWithQuota(ctx, r, query, pool, scaledQuota)
	if err != nil {
		return nil, err
	}
	if len(wide) <= topK {
		return wide, nil
	}
	selected, ok := listwiseSelect(ctx, call, query, wide)
	if !ok {
		return applyChunkQuota(wide, topK, quota), nil
	}
	if len(selected) > topK {
		selected = selected[:topK]
	}
	return selected, nil
}

// listwiseSelect runs the single filter call and maps the returned numbers
// back onto the candidates, preserving fused order. ok=false means the caller
// should fall back to the unfiltered pool.
func listwiseSelect(ctx context.Context, call modelCaller, question string, cands []memory.Result) ([]memory.Result, bool) {
	var b strings.Builder
	b.WriteString("QUESTION: ")
	b.WriteString(question)
	b.WriteString("\n\nCANDIDATE MEMORIES:\n")
	for i, m := range toMemories(cands) {
		fmt.Fprintf(&b, "%d. %s\n", i+1, truncateLine(m.Line(), filterLineMaxChars))
	}
	b.WriteString("\nSelected numbers:")
	raw, err := call(ctx, listwiseFilterPrompt, b.String())
	if err != nil {
		return nil, false
	}
	idxs := parseIndexList(raw, len(cands))
	if len(idxs) == 0 {
		return nil, false
	}
	out := make([]memory.Result, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, cands[i-1])
	}
	return out, true
}

// truncateLine flattens a candidate to one line of at most max code points.
func truncateLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// parseIndexList extracts 1-based candidate numbers from the model's reply,
// deduplicated, in reply order, ignoring anything out of range. Tolerates
// commas, whitespace, newlines, and stray prose around the numbers.
func parseIndexList(raw string, n int) []int {
	seen := make(map[int]bool, n)
	var out []int
	num := 0
	inNum := false
	flush := func() {
		if inNum && num >= 1 && num <= n && !seen[num] {
			seen[num] = true
			out = append(out, num)
		}
		num = 0
		inNum = false
	}
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			num = num*10 + int(r-'0')
			if num > 1_000_000 {
				num = 1_000_000
			}
			inNum = true
		} else {
			flush()
		}
	}
	flush()
	return out
}
