package instructions

import "strings"

// Block renders the instruction snapshot as a <instructions> XML block for
// injection into the system prompt. Each file is prefixed with a header line
// and files are separated by "---". Returns "" when the snapshot is empty or
// nil.
func Block(snapshot *Snapshot) string {
	if snapshot == nil || len(snapshot.Files) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<instructions>\n")
	for i, f := range snapshot.Files {
		if i > 0 {
			b.WriteString("---\n")
		}
		b.WriteString("Instructions from: ")
		b.WriteString(f.Path)
		b.WriteByte('\n')
		b.WriteString(f.Content)
		if len(f.Content) > 0 && f.Content[len(f.Content)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	b.WriteString("</instructions>")
	return b.String()
}
