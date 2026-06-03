package config

import (
	"strconv"
	"strings"
)

// Tool search modes (parsed form of tools.tool_search).
const (
	ToolSearchTST      = "tst"      // always defer deferrable tools
	ToolSearchAuto     = "auto"     // defer only above the threshold percentage
	ToolSearchStandard = "standard" // never defer
)

// ParseToolSearch normalizes a tools.tool_search config value. It returns the
// canonical mode ("tst" | "auto" | "standard"), the threshold percentage
// (meaningful only for "auto"; 0-100), and ok=false for malformed input.
//
// Semantics mirror Claude Code's ENABLE_TOOL_SEARCH:
//
//	"" / "tst"      → tst        (always defer; default)
//	"standard"      → standard   (never defer)
//	"auto"          → auto, 10%  (default threshold)
//	"auto:N"        → auto, N%   (0-100; auto:0 == tst, auto:100 == standard)
func ParseToolSearch(s string) (mode string, percent int, ok bool) {
	switch s {
	case "", ToolSearchTST:
		return ToolSearchTST, 0, true
	case ToolSearchStandard:
		return ToolSearchStandard, 0, true
	case ToolSearchAuto:
		return ToolSearchAuto, 10, true
	}
	if rest, found := strings.CutPrefix(s, "auto:"); found {
		n, err := strconv.Atoi(rest)
		if err != nil {
			return "", 0, false
		}
		switch {
		case n <= 0:
			return ToolSearchTST, 0, true
		case n >= 100:
			return ToolSearchStandard, 0, true
		default:
			return ToolSearchAuto, n, true
		}
	}
	return "", 0, false
}
