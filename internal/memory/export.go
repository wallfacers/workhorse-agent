package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RenderExport renders every entry to a human-readable, deterministic markdown
// document for inspection and git backup (design D7: the mitigation for entries
// no longer being hand-editable files). Entries are sorted by name; pinned
// entries are grouped first (they are the always-loaded identity/rules) so the
// document reads top-down by importance. The output is a pure function of the
// input — no clocks, no IO — so it is trivially testable and diff-friendly.
func RenderExport(entries []*Entry) string {
	sorted := make([]*Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Pinned != sorted[j].Pinned {
			return sorted[i].Pinned // pinned first
		}
		return sorted[i].Name < sorted[j].Name
	})

	pinned := 0
	for _, e := range sorted {
		if e.Pinned {
			pinned++
		}
	}

	var b strings.Builder
	b.WriteString("# workhorse-agent memory export\n\n")
	fmt.Fprintf(&b, "_%d entries (%d pinned)_\n", len(sorted), pinned)

	for _, e := range sorted {
		b.WriteString("\n---\n\n")
		fmt.Fprintf(&b, "## %s\n\n", e.Name)
		fmt.Fprintf(&b, "- pinned: %t | durability: %s | category: %s\n",
			e.Pinned, orNone(e.Durability), orNone(e.Category))
		fmt.Fprintf(&b, "- hits: %d | last used: %s | created: %s\n",
			e.HitCount, fmtTime(e.LastUsedAt), fmtTimeVal(e.CreatedAt))
		if e.Trigger != "" {
			fmt.Fprintf(&b, "- trigger: %s\n", e.Trigger)
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(e.Content, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return fmtTimeVal(*t)
}

func fmtTimeVal(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02")
}
