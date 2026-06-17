package memory

import "strings"

// Block produces the delimited memory block injected into the system prompt.
// Byte layout is load-bearing for prompt-cache stability — do not change
// whitespace, ordering, or delimiter placement without understanding the
// cache-prefix implications.
//
// Format:
//
//	<memory>
//	PINNED:
//	{snapshot.Pinned}
//	---
//	INDEX:
//	{snapshot.Index}
//	</memory>
//
// When Pinned is empty the PINNED section and the --- separator are omitted.
// When Index is empty the INDEX section and the --- separator are omitted.
// When both are empty the function returns "".
func Block(snapshot *Snapshot) string {
	if snapshot == nil {
		return ""
	}

	var b strings.Builder
	hasPinned := snapshot.Pinned != ""
	hasIndex := snapshot.Index != ""

	if !hasPinned && !hasIndex {
		return ""
	}

	b.WriteString("<memory>\n")

	if hasPinned {
		b.WriteString("PINNED:\n")
		b.WriteString(snapshot.Pinned)
		if hasIndex {
			b.WriteString("\n---\n")
		} else {
			b.WriteString("\n")
		}
	}

	if hasIndex {
		b.WriteString("INDEX:\n")
		b.WriteString(snapshot.Index)
		b.WriteString("\n")
	}

	b.WriteString("</memory>")
	return b.String()
}
