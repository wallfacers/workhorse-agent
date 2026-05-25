package tools_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

func TestTruncate_NoTruncationUnderLimit(t *testing.T) {
	s := strings.Repeat("a", 100)
	got, truncated := tools.TruncateOutput(s, 1024)
	if truncated || got != s {
		t.Errorf("under-limit input mutated: truncated=%v got=%q", truncated, got)
	}
}

func TestTruncate_ASCIIBoundary(t *testing.T) {
	s := strings.Repeat("a", 5*1024*1024)
	got, truncated := tools.TruncateOutput(s, 1024*1024)
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if !strings.Contains(got, "[truncated:") {
		t.Errorf("missing truncation marker in %q...", got[:80])
	}
	if !strings.Contains(got, "of 5242880]") {
		t.Errorf("marker should record original length, got %q", got[len(got)-80:])
	}
	if len(got) > 1024*1024 {
		t.Errorf("result length %d exceeds maxBytes %d", len(got), 1024*1024)
	}
}

// Spec requirement: UTF-8 边界安全. Cut must not land in the middle of a
// multi-byte rune.
func TestTruncate_UTF8Boundary(t *testing.T) {
	// Each '中' is 3 bytes. Build a 100-rune string == 300 bytes.
	s := strings.Repeat("中", 100)
	// Pick a cut that lands mid-rune (e.g. 50 bytes — between bytes of a rune).
	got, truncated := tools.TruncateOutput(s, 50)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(got) > 50 {
		t.Errorf("result length %d exceeds maxBytes 50", len(got))
	}
	// The bit before the marker must still be valid UTF-8.
	marker := "[truncated:"
	prefix := got[:strings.Index(got, marker)-1] // strip leading newline before marker
	if !utf8.ValidString(prefix) {
		t.Errorf("truncated prefix invalid UTF-8: %x", prefix)
	}
	// Should have rolled back to a rune boundary; len(prefix) must be a
	// multiple of 3 (size of '中').
	if len(prefix)%3 != 0 {
		t.Errorf("prefix length %d is not a rune boundary", len(prefix))
	}
}

func TestTruncate_ZeroLimitDisabled(t *testing.T) {
	s := strings.Repeat("a", 100)
	got, truncated := tools.TruncateOutput(s, 0)
	if truncated || got != s {
		t.Errorf("maxBytes=0 means disabled, got truncated=%v", truncated)
	}
}

func TestTruncate_SmallMaxBytes(t *testing.T) {
	s := strings.Repeat("a", 200)
	got, truncated := tools.TruncateOutput(s, 50)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(got) > 50 {
		t.Errorf("result length %d exceeds maxBytes 50", len(got))
	}
	if !strings.Contains(got, "[truncated:") {
		t.Errorf("missing truncation marker: %q", got)
	}
}

func TestTruncate_VerySmallMaxBytes(t *testing.T) {
	s := strings.Repeat("xyz", 100) // 300 bytes
	got, truncated := tools.TruncateOutput(s, 10)
	if !truncated {
		t.Fatal("expected truncation")
	}
	// When maxBytes < marker length, result is marker-only (no data prefix).
	// The marker itself may exceed maxBytes, but we keep it for diagnosability.
	if !strings.Contains(got, "[truncated:") {
		t.Errorf("missing truncation marker: %q", got)
	}
	// At minimum, the result should be much shorter than the original.
	if len(got) >= len(s) {
		t.Errorf("result should be shorter than original: got %d, orig %d", len(got), len(s))
	}
}
