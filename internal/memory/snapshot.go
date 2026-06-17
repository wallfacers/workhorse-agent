package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Snapshot holds the immutable two-layer memory content assembled at session
// start (design D2). It is a pure function of store state and is never mutated
// for the lifetime of the session that holds it.
type Snapshot struct {
	// Pinned is the full content of every pinned entry, sorted by name ascending,
	// joined by "\n\n".
	Pinned string
	// Index is the manifest of non-pinned entries: one "- {name} — {trigger}" line
	// per entry (score desc, name asc), joined by "\n". When the manifest exceeds
	// the budget a final "- … N more memories not shown; use MemorySearch" line is
	// appended (non-silent overflow, design D3).
	Index    string
	LoadedAt time.Time
}

// Loader reads the memory entry store once and assembles an immutable snapshot.
type Loader struct {
	Store   *EntryStore
	Budgets Budgets
	// ScoreFn assigns a score to a non-pinned entry; higher scores survive
	// manifest truncation and sort earlier. When nil a default placeholder
	// ordering is used (see defaultScore).
	ScoreFn func(*Entry) float64
}

// Load reads all entries once and assembles the two-layer snapshot. Pinned
// entries become the PINNED region (full content, name-sorted); non-pinned
// entries become the INDEX manifest (score desc, name asc), bounded by the
// manifest budget with non-silent overflow.
func (l *Loader) Load(ctx context.Context) (*Snapshot, error) {
	entries, err := l.Store.List(ctx)
	if err != nil {
		return nil, err
	}

	var pinned, nonPinned []*Entry
	for _, e := range entries {
		if e.Pinned {
			pinned = append(pinned, e)
		} else {
			nonPinned = append(nonPinned, e)
		}
	}

	// PINNED: full content sorted by name ascending, joined by "\n\n".
	sort.Slice(pinned, func(i, j int) bool { return pinned[i].Name < pinned[j].Name })
	pinnedParts := make([]string, len(pinned))
	for i, e := range pinned {
		pinnedParts[i] = e.Content
	}
	pinnedRegion := strings.Join(pinnedParts, "\n\n")

	// INDEX: manifest of non-pinned, sorted by score desc then name asc.
	score := l.ScoreFn
	if score == nil {
		score = defaultScore
	}
	sort.SliceStable(nonPinned, func(i, j int) bool {
		si, sj := score(nonPinned[i]), score(nonPinned[j])
		if si != sj {
			return si > sj // higher score first
		}
		return nonPinned[i].Name < nonPinned[j].Name
	})

	indexRegion := assembleManifest(nonPinned, l.Budgets.ManifestChars)

	return &Snapshot{
		Pinned:   pinnedRegion,
		Index:    indexRegion,
		LoadedAt: time.Now().UTC(),
	}, nil
}

// manifestLine renders one manifest row for an entry.
func manifestLine(e *Entry) string {
	return fmt.Sprintf("- %s — %s", e.Name, e.Trigger)
}

// assembleManifest builds the INDEX region honouring the manifest budget. Lines
// are added (already score-sorted) while the running code-point total stays
// within budget; when the budget is exceeded the remaining entries are dropped
// and a final visible "- … N more …" line is appended, plus a WARN — entries are
// never silently dropped (design D3 / Manifest layer with non-silent overflow).
func assembleManifest(entries []*Entry, budget int) string {
	if len(entries) == 0 {
		return ""
	}

	var kept []string
	total := 0
	dropped := 0
	for i, e := range entries {
		line := manifestLine(e)
		lineLen := utf8.RuneCountInString(line)
		// Account for the joining newline between consecutive lines.
		extra := lineLen
		if len(kept) > 0 {
			extra++ // "\n"
		}
		if budget > 0 && total+extra > budget {
			dropped = len(entries) - i
			break
		}
		kept = append(kept, line)
		total += extra
	}

	if dropped == 0 {
		return strings.Join(kept, "\n")
	}

	slog.Warn("memory: manifest truncated", "dropped", dropped)
	overflow := fmt.Sprintf("- … %d more memories not shown; use MemorySearch", dropped)
	kept = append(kept, overflow)
	return strings.Join(kept, "\n")
}

// defaultScore is the Phase 2 placeholder ranking used when no ScoreFn is
// injected: hit_count desc, then last_used_at desc (NULL last), then name asc.
// It is encoded as a single float so higher is better; ties on the float fall
// through to the name tiebreaker in the caller's stable sort.
//
// Phase 5 will replace this with the curation scorer.
func defaultScore(e *Entry) float64 {
	// hit_count dominates; recency is a fractional tiebreaker below it.
	s := float64(e.HitCount)
	if e.LastUsedAt != nil && !e.LastUsedAt.IsZero() {
		// Map last-used unix seconds into (0,1) so more-recent ranks higher
		// without overtaking a higher hit_count. NULL (never used) → 0.
		s += recencyFraction(e.LastUsedAt)
	}
	return s
}

// recencyFraction maps a timestamp to (0,1), monotonically increasing with
// recency, so it acts purely as a tiebreaker within equal hit_count buckets.
func recencyFraction(t *time.Time) float64 {
	secs := t.Unix()
	if secs <= 0 {
		return 0
	}
	return float64(secs) / (float64(secs) + 1e12)
}

// CharCount returns the number of Unicode code points in s.
func CharCount(s string) int {
	return utf8.RuneCountInString(s)
}

// readFile reads a file, treating a missing file as empty content. Kept for the
// phase-4 flat-file migration (MEMORY.md/USER.md → entries); unused until then.
//
//nolint:unused
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("memory: read %s: %w", path, err)
	}
	return string(data), nil
}

// memoriesDir resolves the legacy memories directory. Kept for the phase-4
// flat-file migration (locating MEMORY.md/USER.md and the legacy/ copy target);
// unused until then.
//
//nolint:unused
func memoriesDir(profileDir string) string {
	if profileDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		return filepath.Join(home, ".workhorse-agent", "memories")
	}
	return filepath.Join(profileDir, "memories")
}
