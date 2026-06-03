package toolsearch

import (
	"encoding/json"
	"regexp"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// functionTag matches one <function>{...}</function> entry in a rendered
// ToolSearch result so the embedded tool name can be recovered on rehydration.
var functionTag = regexp.MustCompile(`(?s)<function>(.*?)</function>`)

// ReconstructDiscovered rebuilds the set of tool names a session previously
// surfaced via ToolSearch by scanning persisted history. It correlates
// assistant tool_use blocks naming ToolSearch with their tool_result blocks
// (by ToolUseID) and parses the function names out of the <functions> block.
//
// This is called on session rehydration so that, after a restart, the model's
// pending calls to already-discovered tools still resolve to a loaded schema.
// The returned slice is de-duplicated; order is unspecified.
func ReconstructDiscovered(history []provider.Message) []string {
	searchUseIDs := map[string]struct{}{}
	for _, msg := range history {
		for _, b := range msg.Content {
			if b.Type == provider.BlockToolUse && b.ToolName == Name && b.ToolUseID != "" {
				searchUseIDs[b.ToolUseID] = struct{}{}
			}
		}
	}
	if len(searchUseIDs) == 0 {
		return nil
	}

	discovered := map[string]struct{}{}
	for _, msg := range history {
		for _, b := range msg.Content {
			if b.Type != provider.BlockToolResult {
				continue
			}
			if _, ok := searchUseIDs[b.ToolUseID]; !ok {
				continue
			}
			for _, name := range namesFromFunctions(b.Output) {
				discovered[name] = struct{}{}
			}
		}
	}
	if len(discovered) == 0 {
		return nil
	}
	out := make([]string, 0, len(discovered))
	for n := range discovered {
		out = append(out, n)
	}
	return out
}

// namesFromFunctions extracts the "name" field from each <function> entry in a
// rendered ToolSearch result body.
func namesFromFunctions(body string) []string {
	var out []string
	for _, m := range functionTag.FindAllStringSubmatch(body, -1) {
		var e struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(m[1]), &e); err == nil && e.Name != "" {
			out = append(out, e.Name)
		}
	}
	return out
}
