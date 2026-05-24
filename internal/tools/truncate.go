package tools

import (
	"fmt"
	"unicode/utf8"
)

// TruncateOutput enforces the tools.tool_result_max_bytes spec rule. If s is
// at or below the limit, it's returned untouched. Otherwise the function
// walks back from limit to the nearest UTF-8 rune boundary so the truncated
// prefix is still valid UTF-8, then appends a single-line marker:
//
//	[truncated: kept N bytes of M]
//
// Callers (Read/Bash/Grep/etc.) pre-cap their buffers when possible (ring
// buffer for Bash, limit for Read, max-lines for Grep) but should still pass
// the result through this function as a belt-and-braces check.
func TruncateOutput(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	// Walk back until s[:cut] ends on a clean rune boundary. Worst case is
	// 3 steps (the longest continuation tail is 3 bytes). DecodeLastRune
	// returns (RuneError, 1) for *invalid* encodings and (RuneError, 3) for
	// a literal U+FFFD — so we accept either when size != 1.
	for cut > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:cut])
		if r != utf8.RuneError || size != 1 {
			break
		}
		cut--
	}
	marker := fmt.Sprintf("\n[truncated: kept %d bytes of %d]", cut, len(s))
	return s[:cut] + marker, true
}
