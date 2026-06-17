package memory_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

func TestRenderExportOrdersPinnedFirstThenName(t *testing.T) {
	used := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	entries := []*memory.Entry{
		{Name: "zebra", Content: "z", Durability: "volatile", Category: "project", HitCount: 2, LastUsedAt: &used, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "alpha", Content: "a", Durability: "volatile", Category: "reference", CreatedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "user-profile", Content: "the user", Pinned: true, Durability: "evergreen", Category: "user", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	out := memory.RenderExport(entries)

	if !strings.Contains(out, "_3 entries (1 pinned)_") {
		t.Fatalf("missing/incorrect summary line:\n%s", out)
	}
	// Pinned entry must appear before the non-pinned ones; among non-pinned,
	// alpha before zebra.
	iUser := strings.Index(out, "## user-profile")
	iAlpha := strings.Index(out, "## alpha")
	iZebra := strings.Index(out, "## zebra")
	if !(iUser >= 0 && iUser < iAlpha && iAlpha < iZebra) {
		t.Fatalf("ordering wrong: user=%d alpha=%d zebra=%d\n%s", iUser, iAlpha, iZebra, out)
	}
	if !strings.Contains(out, "last used: 2026-06-01") {
		t.Fatalf("expected formatted last-used date:\n%s", out)
	}
	if !strings.Contains(out, "last used: never") {
		t.Fatalf("expected 'never' for unused entry:\n%s", out)
	}
}

func TestRenderExportDeterministic(t *testing.T) {
	entries := []*memory.Entry{
		{Name: "b", Content: "two", Durability: "volatile"},
		{Name: "a", Content: "one", Durability: "volatile"},
	}
	first := memory.RenderExport(entries)
	for i := 0; i < 10; i++ {
		if memory.RenderExport(entries) != first {
			t.Fatal("RenderExport not deterministic")
		}
	}
}

func TestRenderExportEmpty(t *testing.T) {
	out := memory.RenderExport(nil)
	if !strings.Contains(out, "_0 entries (0 pinned)_") {
		t.Fatalf("empty export missing summary:\n%s", out)
	}
}
