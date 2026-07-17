package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

// Verbatim-chunk union store: alongside the extracted facts, each session's raw
// dialogue is stored as speaker-attributed chunk entries in the SAME store, so
// every retrieval signal (vector, BM25, RRF) sees both representations. This is
// the "chunks ∪ artifacts" configuration from An 2026 (arXiv:2601.00821), which
// matches verbatim-chunk accuracy while extraction alone forfeits 15-30 pp —
// extraction commits to relevance before the question exists; chunks defer that
// decision to query time.
const (
	chunkTargetChars = 900  // soft target per chunk (entry budget is 1200)
	chunkMaxChars    = 1100 // hard cap for a single oversized turn
)

// buildSessionChunks splits one session's turns into speaker-attributed chunks
// of at most ~chunkTargetChars code points, never splitting a turn except when
// a single turn alone exceeds chunkMaxChars (then it is truncated).
func buildSessionChunks(s session) []string {
	var chunks []string
	var b strings.Builder
	size := 0
	for _, t := range s.Turns {
		line := t.Speaker + ": " + t.Text
		if n := utf8.RuneCountInString(line); n > chunkMaxChars {
			line = string([]rune(line)[:chunkMaxChars])
		}
		n := utf8.RuneCountInString(line)
		if size > 0 && size+1+n > chunkTargetChars {
			chunks = append(chunks, b.String())
			b.Reset()
			size = 0
		}
		if size > 0 {
			b.WriteByte('\n')
			size++
		}
		b.WriteString(line)
		size += n
	}
	if size > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}

// chunkTrigger derives the single-line manifest trigger from chunk content.
func chunkTrigger(content string) string {
	line := content
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if utf8.RuneCountInString(line) > 100 {
		line = string([]rune(line)[:100])
	}
	return line
}

// retrieveWithQuota reserves `quota` of the topK slots for verbatim chunks.
// The RRF signals are biased toward extracted facts (chunks carry no entities
// and embed diffusely), so without a quota chunks fill only ~0-6% of the top-k
// even when they hold the answer verbatim. A wide fused search is partitioned
// by kind, each side keeps its fused order, and shortfall on either side is
// backfilled from the other. quota <= 0 degrades to a plain Search.
func retrieveWithQuota(ctx context.Context, r *memory.Retriever, query string, topK, quota int) ([]memory.Result, error) {
	if quota <= 0 {
		return r.Search(ctx, query, topK)
	}
	widePool := topK * 6
	if widePool < 300 {
		widePool = 300
	}
	wide, err := r.Search(ctx, query, widePool)
	if err != nil {
		return nil, err
	}
	return applyChunkQuota(wide, topK, quota), nil
}

// applyChunkQuota partitions a fused result list into facts and chunks, keeps
// topK-quota facts + quota chunks (backfilling shortfall from the other side),
// and restores fused (score-descending) order.
func applyChunkQuota(wide []memory.Result, topK, quota int) []memory.Result {
	var facts, chunks []memory.Result
	for _, h := range wide {
		if strings.HasPrefix(h.Name, "chunk-") {
			chunks = append(chunks, h)
		} else {
			facts = append(facts, h)
		}
	}
	factSlots := topK - quota
	if len(chunks) < quota {
		factSlots = topK - len(chunks)
	}
	if factSlots > len(facts) {
		factSlots = len(facts)
	}
	chunkSlots := topK - factSlots
	if chunkSlots > len(chunks) {
		chunkSlots = len(chunks)
	}
	out := append(facts[:factSlots:factSlots], chunks[:chunkSlots]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// ingestChunks writes one conversation's verbatim chunks as entries. Upsert is
// keyed by deterministic names, so re-running over a persisted store is
// idempotent. Returns the number of chunks written.
func ingestChunks(ctx context.Context, es *memory.EntryStore, conv conversation) (int, error) {
	n := 0
	for _, s := range conv.Sessions {
		var eventDate *time.Time
		if !s.Date.IsZero() {
			d := s.Date
			eventDate = &d
		}
		for i, content := range buildSessionChunks(s) {
			e := &memory.Entry{
				Name:            fmt.Sprintf("chunk-c%d-s%d-%03d", conv.ID, s.Index, i),
				Trigger:         chunkTrigger(content),
				Content:         content,
				Durability:      "volatile",
				Category:        "chunk",
				EventDate:       eventDate,
				FactSource:      "verbatim_chunk",
				SourceSessionID: fmt.Sprintf("conv%d-sess%d", conv.ID, s.Index),
			}
			if err := es.Upsert(ctx, e); err != nil {
				return n, fmt.Errorf("chunk %s: %w", e.Name, err)
			}
			n++
		}
	}
	return n, nil
}
