package agent

import "strings"

// CancelledToolOutput is the literal output the loop synthesises for any
// tool_use that lost its tool_result to a cancel or panic. The leading
// `[CANCELLED]` marker is what cancelledNote describes to the model so it
// doesn't misread the string as user input.
const CancelledToolOutput = "[CANCELLED] Tool execution was interrupted by user"

// cancelledNote is appended to the user-supplied system prompt so the model
// understands the `[CANCELLED]` prefix it may see in a tool_result. Kept short
// so it doesn't bloat every request.
const cancelledNote = "\n\nNote: if a tool_result begins with `[CANCELLED]`, " +
	"the tool call was interrupted by the user. Do not retry it automatically; " +
	"acknowledge the interruption and ask the user how to proceed."

// BuildSystemPrompt joins the operator-supplied base prompt with the
// cancellation-semantics paragraph the agent loop relies on. If base is empty
// the cancelled-note still ships so the model gets the marker explanation.
func BuildSystemPrompt(base string) string {
	base = strings.TrimRight(base, " \t\n")
	if base == "" {
		return strings.TrimLeft(cancelledNote, "\n")
	}
	return base + cancelledNote
}
