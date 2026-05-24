// Package permission implements the 5-decision permission control model
// (allow_once / allow_session / allow_permanent / deny / deny_permanent) plus
// the glob matcher used in rule patterns. The Manager coordinates rule lookup,
// prompt-and-wait flow, and DangerousCommandGuard escalation.
package permission

import (
	"path/filepath"
	"strings"
)

// MatchGlob reports whether target matches pattern under the following rules:
//
//   - `**`  matches any number of path segments (including zero)
//   - `*`   matches any single segment (does NOT cross `/`)
//   - `?`   matches a single non-`/` character
//   - `[xyz]` is a character class (delegated to filepath.Match)
//   - anything else matches literally
//
// Both pattern and target are split on `/`; comparisons are segment-wise.
// Empty pattern matches an empty target. A bare `*` matches a single segment.
func MatchGlob(pattern, target string) bool {
	patSegs := strings.Split(pattern, "/")
	tgtSegs := strings.Split(target, "/")
	return matchSegs(patSegs, tgtSegs)
}

func matchSegs(pat, tgt []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Greedy match for **: try zero, one, two, ... target segments.
			rest := pat[1:]
			for i := 0; i <= len(tgt); i++ {
				if matchSegs(rest, tgt[i:]) {
					return true
				}
			}
			return false
		}
		if len(tgt) == 0 {
			return false
		}
		ok, err := filepath.Match(pat[0], tgt[0])
		if err != nil || !ok {
			return false
		}
		pat = pat[1:]
		tgt = tgt[1:]
	}
	return len(tgt) == 0
}
