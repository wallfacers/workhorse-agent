package prompt

import "strings"

// CancelledToolOutput is the synthesised tool_result text when a tool_use
// loses its result to a cancel or panic. The leading [CANCELLED] marker is
// what CancelledNote describes to the model.
const CancelledToolOutput = "[CANCELLED] Tool execution was interrupted by user"

// CompactionFallback is returned when the summariser produces an empty response.
const CompactionFallback = "(compaction summary unavailable)"

// CancelledNote explains the [CANCELLED] prefix to the model. It does NOT
// include a leading newline — the SystemPrompt template controls spacing via
// {{if .BasePrompt}}\n\n{{end}}.
const CancelledNote = "Note: if a tool_result begins with `[CANCELLED]`, " +
	"the tool call was interrupted by the user. Do not retry it automatically; " +
	"acknowledge the interruption and ask the user how to proceed."

// SystemPrompt renders the agent's system prompt. Empty base yields just the
// CancelledNote; non-empty base yields "base\n\nCancelledNote".
var SystemPrompt = MustParse("system_prompt",
	"{{.BasePrompt}}{{if .BasePrompt}}\n\n{{end}}"+CancelledNote)

// Compaction is the summariser's system prompt. No placeholders.
var Compaction = MustParse("compaction",
	"You are a conversation summariser. Read the messages provided "+
		"and produce a single dense paragraph (≤ 400 tokens) capturing every "+
		"factual claim, decision, and open question. Do not editorialise. "+
		"Do not include greetings or meta-commentary about the summary itself.")

// SkillManifest renders the <available_skills> block injected into the system
// prompt when skills are loaded. Footer is hardcoded in the template.
var SkillManifest = MustParse("skill_manifest",
	"<available_skills>\n"+
		"{{range $s := .Skills}}"+
		"- name: {{$s.Name}}\n"+
		"  trigger: {{$s.Trigger}}\n"+
		"{{end}}"+
		"</available_skills>\n\n"+
		"You can use the LoadSkill tool to load full instructions.\n")

// BuildSystemPrompt is the drop-in replacement for agent.BuildSystemPrompt.
// It trims trailing whitespace from base and renders the SystemPrompt template.
// On the (impossible-by-construction) Execute error, falls back to a manual
// concatenation so CancelledNote still ships and the model isn't left without
// the [CANCELLED] marker explanation.
func BuildSystemPrompt(base string) string {
	base = strings.TrimRight(base, " \t\n")
	out, err := SystemPrompt.Execute(map[string]any{"BasePrompt": base})
	if err != nil {
		if base == "" {
			return CancelledNote
		}
		return base + "\n\n" + CancelledNote
	}
	return out
}
