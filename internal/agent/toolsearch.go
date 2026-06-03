package agent

import (
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// isDeferrable reports whether a tool opts into tool-search deferral. The
// ToolSearch tool itself is never deferrable (the model needs it to bootstrap
// discovery of everything else).
func isDeferrable(t tools.Tool) bool {
	if t.Name() == tools.ToolSearchName {
		return false
	}
	d, ok := t.(tools.Deferrable)
	return ok && d.ShouldDefer()
}

// deferCatalog is a snapshot of the deferred tools for one turn, handed to the
// ToolSearch tool via Env.ToolCatalog. It satisfies tools.ToolCatalog.
type deferCatalog struct{ infos []tools.ToolInfo }

func (c deferCatalog) DeferredTools() []tools.ToolInfo { return c.infos }

// deferActive decides whether deferral is in effect this turn given the mode
// and (for "auto") whether the deferrable tools exceed the context threshold.
func (l *Loop) deferActive(deferrable []tools.Tool) bool {
	switch l.Config.ToolSearchMode {
	case "standard":
		return false
	case "auto":
		if len(deferrable) == 0 {
			return false
		}
		pct := l.Config.ToolSearchPercent
		if pct <= 0 {
			pct = 10
		}
		threshold := l.Config.MaxHistoryTokens * pct / 100
		return estimateToolTokens(deferrable) >= threshold
	default: // "tst" (and empty)
		return len(deferrable) > 0
	}
}

// estimateToolTokens approximates the token cost of the deferrable tool
// definitions using the same chars/4 heuristic as history estimation.
func estimateToolTokens(ts []tools.Tool) int {
	chars := 0
	for _, t := range ts {
		chars += len(t.Name()) + len(t.Description()) + len(t.InputSchema())
	}
	return chars / 4
}

// toolInfo converts a Tool into the catalog's ToolInfo.
func toolInfo(t tools.Tool) tools.ToolInfo {
	return tools.ToolInfo{Name: t.Name(), Description: t.Description(), InputSchema: t.InputSchema()}
}

// deferredAnnouncement builds the <available-deferred-tools> meta message that
// tells the model which deferred tools exist but are not yet loaded. Returns
// nil when there is nothing to announce.
func deferredAnnouncement(names []string) *provider.Message {
	if len(names) == 0 {
		return nil
	}
	var sb []byte
	sb = append(sb, "<available-deferred-tools>\n"...)
	for _, n := range names {
		sb = append(sb, n...)
		sb = append(sb, '\n')
	}
	sb = append(sb, "</available-deferred-tools>"...)
	return &provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.ContentBlock{{Type: provider.BlockText, Text: string(sb)}},
	}
}
