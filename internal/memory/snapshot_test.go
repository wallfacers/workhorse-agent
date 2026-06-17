package memory_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

func loadSnapshot(t *testing.T, es *memory.EntryStore, b memory.Budgets) *memory.Snapshot {
	t.Helper()
	l := &memory.Loader{Store: es, Budgets: b}
	snap, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return snap
}

func TestSnapshot_PinnedFullContentNameSorted(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	// Insert out of order; pinned must come back name-sorted, full content.
	for _, e := range []*memory.Entry{
		{Name: "charlie", Content: "C body", Pinned: true},
		{Name: "alpha", Content: "A body", Pinned: true},
		{Name: "bravo", Content: "B body", Pinned: true},
	} {
		if err := es.Upsert(ctx, e); err != nil {
			t.Fatalf("upsert %q: %v", e.Name, err)
		}
	}
	snap := loadSnapshot(t, es, memory.DefaultBudgets())
	want := "A body\n\nB body\n\nC body"
	if snap.Pinned != want {
		t.Fatalf("Pinned = %q, want %q", snap.Pinned, want)
	}
	if snap.Index != "" {
		t.Fatalf("Index should be empty with no non-pinned entries, got %q", snap.Index)
	}
}

func TestSnapshot_NonPinnedManifestDefaultOrdering(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	// hit_count desc is the primary default key; equal hits fall to name asc.
	must(t, es.Upsert(ctx, &memory.Entry{Name: "low", Trigger: "rarely", Content: "x"}))
	must(t, es.Upsert(ctx, &memory.Entry{Name: "hot", Trigger: "often", Content: "y"}))
	must(t, es.Upsert(ctx, &memory.Entry{Name: "mid", Trigger: "sometimes", Content: "z"}))
	// Bump hot the most, mid once.
	now := mustNow()
	for i := 0; i < 3; i++ {
		must(t, es.BumpUsage(ctx, "hot", now))
	}
	must(t, es.BumpUsage(ctx, "mid", now))

	snap := loadSnapshot(t, es, memory.DefaultBudgets())
	wantIndex := "- hot — often\n- mid — sometimes\n- low — rarely"
	if snap.Index != wantIndex {
		t.Fatalf("Index =\n%q\nwant\n%q", snap.Index, wantIndex)
	}
	if snap.Pinned != "" {
		t.Fatalf("Pinned should be empty with no pinned entries, got %q", snap.Pinned)
	}
}

func TestSnapshot_ByteStableAcrossLoads(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	must(t, es.Upsert(ctx, &memory.Entry{Name: "p", Content: "pinned body", Pinned: true}))
	must(t, es.Upsert(ctx, &memory.Entry{Name: "n1", Trigger: "t1", Content: "a"}))
	must(t, es.Upsert(ctx, &memory.Entry{Name: "n2", Trigger: "t2", Content: "b"}))

	a := memory.Block(loadSnapshot(t, es, memory.DefaultBudgets()))
	b := memory.Block(loadSnapshot(t, es, memory.DefaultBudgets()))
	if a != b {
		t.Fatalf("Block not byte-stable across loads:\nA=%q\nB=%q", a, b)
	}
	if !strings.Contains(a, "<memory>") || !strings.Contains(a, "PINNED:") || !strings.Contains(a, "INDEX:") {
		t.Fatalf("expected full <memory> block with PINNED and INDEX, got:\n%s", a)
	}
}

func TestSnapshot_EmptyStoreYieldsEmptyBlock(t *testing.T) {
	es, _ := newEntryStore(t)
	snap := loadSnapshot(t, es, memory.DefaultBudgets())
	if snap.Pinned != "" || snap.Index != "" {
		t.Fatalf("empty store should yield empty regions, got Pinned=%q Index=%q", snap.Pinned, snap.Index)
	}
	if got := memory.Block(snap); got != "" {
		t.Fatalf("empty store Block should be \"\", got %q", got)
	}
}

func TestSnapshot_OnlyPinnedHasNoIndexSection(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	must(t, es.Upsert(ctx, &memory.Entry{Name: "u", Content: "user prefs", Pinned: true}))

	block := memory.Block(loadSnapshot(t, es, memory.DefaultBudgets()))
	if !strings.Contains(block, "PINNED:") {
		t.Fatalf("expected PINNED section, got:\n%s", block)
	}
	if strings.Contains(block, "INDEX:") {
		t.Fatalf("should not contain INDEX section, got:\n%s", block)
	}
	if strings.Contains(block, "---") {
		t.Fatalf("should not contain separator with only pinned, got:\n%s", block)
	}
}

func TestSnapshot_OnlyNonPinnedHasNoPinnedSection(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	must(t, es.Upsert(ctx, &memory.Entry{Name: "n", Trigger: "trig", Content: "fact"}))

	block := memory.Block(loadSnapshot(t, es, memory.DefaultBudgets()))
	if !strings.Contains(block, "INDEX:") {
		t.Fatalf("expected INDEX section, got:\n%s", block)
	}
	if strings.Contains(block, "PINNED:") {
		t.Fatalf("should not contain PINNED section, got:\n%s", block)
	}
	if strings.Contains(block, "---") {
		t.Fatalf("should not contain separator with only index, got:\n%s", block)
	}
}

func TestSnapshot_ManifestBudgetOverflowNonSilent(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	// 10 non-pinned entries, each manifest line ~ "- name-00 — trigger padding..."
	const total = 10
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("name-%02d", i)
		// equal hit_count → deterministic name asc ordering
		must(t, es.Upsert(ctx, &memory.Entry{Name: name, Trigger: "a short trigger line here", Content: "x"}))
	}
	// Tiny budget so only a few lines fit.
	b := memory.DefaultBudgets()
	b.ManifestChars = 80
	snap := loadSnapshot(t, es, b)

	lines := strings.Split(snap.Index, "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "- … ") || !strings.Contains(last, "more memories not shown; use MemorySearch") {
		t.Fatalf("expected overflow line, got last line %q\nfull:\n%s", last, snap.Index)
	}
	// Parse N and verify it equals (total - kept lines).
	keptLines := len(lines) - 1
	var n int
	if _, err := fmt.Sscanf(last, "- … %d more", &n); err != nil {
		t.Fatalf("could not parse N from %q: %v", last, err)
	}
	if n != total-keptLines {
		t.Fatalf("overflow N = %d, want %d (total=%d kept=%d)", n, total-keptLines, total, keptLines)
	}
	if n <= 0 {
		t.Fatalf("expected some dropped entries, got N=%d", n)
	}
	// The kept lines (excluding overflow) must stay within budget.
	keptRegion := strings.Join(lines[:keptLines], "\n")
	if memory.CharCount(keptRegion) > b.ManifestChars {
		t.Fatalf("kept manifest region %d cp exceeds budget %d", memory.CharCount(keptRegion), b.ManifestChars)
	}
}

func TestSnapshot_ManifestWithinBudgetListsAll(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		must(t, es.Upsert(ctx, &memory.Entry{Name: fmt.Sprintf("e%d", i), Trigger: "t", Content: "x"}))
	}
	snap := loadSnapshot(t, es, memory.DefaultBudgets())
	lines := strings.Split(snap.Index, "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 manifest lines, got %d:\n%s", len(lines), snap.Index)
	}
	for _, l := range lines {
		if strings.Contains(l, "more memories not shown") {
			t.Fatalf("did not expect overflow line within budget:\n%s", snap.Index)
		}
	}
}
