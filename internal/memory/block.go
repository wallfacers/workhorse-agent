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
//	USER:
//	{userMD}
//	---
//	MEMORY:
//	{memoryMD}
//	</memory>
//
// When userMD is empty the USER section and the --- separator are omitted.
// When memoryMD is empty the MEMORY section and the --- separator are omitted.
// When both are empty the function returns "".
func Block(snapshot *Snapshot) string {
	if snapshot == nil {
		return ""
	}

	var b strings.Builder
	hasUser := snapshot.UserMD != ""
	hasMemory := snapshot.MemoryMD != ""

	if !hasUser && !hasMemory {
		return ""
	}

	b.WriteString("<memory>\n")

	if hasUser {
		b.WriteString("USER:\n")
		b.WriteString(snapshot.UserMD)
		if hasMemory {
			b.WriteString("\n---\n")
		} else {
			b.WriteString("\n")
		}
	}

	if hasMemory {
		b.WriteString("MEMORY:\n")
		b.WriteString(snapshot.MemoryMD)
		b.WriteString("\n")
	}

	b.WriteString("</memory>")
	return b.String()
}
