package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

func TestBuildSessionChunks(t *testing.T) {
	long := strings.Repeat("x", 1500)
	s := session{
		Index: 3,
		Date:  time.Date(2023, 5, 8, 0, 0, 0, 0, time.UTC),
		Turns: []turn{
			{Speaker: "Caroline", Text: "I adopted a golden retriever named Max in March."},
			{Speaker: "Melanie", Text: "That's wonderful! I've been busy with my pottery class."},
			{Speaker: "Caroline", Text: long},
			{Speaker: "Melanie", Text: "See you Friday."},
		},
	}
	chunks := buildSessionChunks(s)
	if len(chunks) < 2 {
		t.Fatalf("expected the oversized turn to force a split, got %d chunk(s)", len(chunks))
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > chunkMaxChars {
			t.Errorf("chunk %d exceeds hard cap: %d > %d", i, n, chunkMaxChars)
		}
	}
	joined := strings.Join(chunks, "\n")
	for _, want := range []string{"Caroline: I adopted", "Melanie: That's wonderful", "Melanie: See you Friday."} {
		if !strings.Contains(joined, want) {
			t.Errorf("chunks lost turn content %q", want)
		}
	}
	if buildSessionChunks(session{}) != nil {
		t.Errorf("empty session should yield no chunks")
	}
}

func TestChunkTrigger(t *testing.T) {
	got := chunkTrigger("Caroline: hello\nMelanie: hi")
	if got != "Caroline: hello" {
		t.Errorf("trigger = %q, want first line", got)
	}
	long := chunkTrigger(strings.Repeat("y", 300))
	if n := utf8.RuneCountInString(long); n != 100 {
		t.Errorf("long trigger not truncated to 100, got %d", n)
	}
	if strings.ContainsAny(long, "\r\n") {
		t.Errorf("trigger must be single-line")
	}
}

func TestApplyChunkQuota(t *testing.T) {
	mk := func(name string, score float64) memory.Result { return memory.Result{Name: name, Score: score} }
	wide := []memory.Result{
		mk("fact-a", 0.9), mk("fact-b", 0.8), mk("chunk-c0-s1-000", 0.7),
		mk("fact-c", 0.6), mk("chunk-c0-s2-000", 0.5), mk("fact-d", 0.4),
	}
	got := applyChunkQuota(wide, 4, 2)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	chunks := 0
	for i, h := range got {
		if strings.HasPrefix(h.Name, "chunk-") {
			chunks++
		}
		if i > 0 && got[i-1].Score < h.Score {
			t.Errorf("fused order broken at %d", i)
		}
	}
	if chunks != 2 {
		t.Errorf("chunk slots = %d, want 2", chunks)
	}
	// Shortfall backfill: only one chunk available.
	got = applyChunkQuota(wide[:4], 4, 3)
	if len(got) != 4 {
		t.Errorf("backfill len = %d, want 4", len(got))
	}
	// quota 0 handled by caller; still behaves as plain truncation here.
	if got := applyChunkQuota(wide, 10, 2); len(got) != 6 {
		t.Errorf("small pool len = %d, want all 6", len(got))
	}
}
