package dispatch

import (
	"encoding/json"
	"strings"
)

// maxActivityRunes caps the subagent_status activity line (FR/events.md: 80).
const maxActivityRunes = 80

// FormatActivity translates one sub-agent tool call into a single-line,
// human-readable activity description for the subagent_status event. Input is
// JSON; the relevant field is pulled per tool name per the events.md table.
// Multi-line values are folded to one line and the result is capped at
// maxActivityRunes code points (truncation appends an ellipsis, on a rune
// boundary so CJK text is never split mid-character).
func FormatActivity(toolName string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	str := func(key string) string {
		if v, ok := m[key].(string); ok {
			return v
		}
		return ""
	}

	var activity string
	switch toolName {
	case "Read":
		activity = "Read " + firstNonEmpty(str("path"), str("file_path"))
	case "Grep":
		activity = "Grep " + quoted(str("pattern"))
	case "Bash":
		activity = str("command")
	case "session_search", "MemorySearch":
		activity = "Search " + quoted(str("query"))
	default:
		activity = toolName
	}
	return foldAndCap(activity)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// quoted folds the value to one line and wraps it in double quotes; an empty
// value yields an empty string so e.g. "Grep " trims down to "Grep".
func quoted(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	return `"` + s + `"`
}

func foldAndCap(s string) string {
	folded := strings.Join(strings.Fields(s), " ")
	r := []rune(folded)
	if len(r) <= maxActivityRunes {
		return folded
	}
	return string(r[:maxActivityRunes-1]) + "…"
}
