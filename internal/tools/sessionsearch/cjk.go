package sessionsearch

// isCJK reports whether r is a CJK character within the Unicode ranges
// specified in design.md D6.
func isCJK(r rune) bool {
	switch {
	// CJK Unified Ideographs
	case r >= 0x4E00 && r <= 0x9FFF:
		return true
	// CJK Unified Ideographs Extension A
	case r >= 0x3400 && r <= 0x4DBF:
		return true
	// CJK Unified Ideographs Extension B
	case r >= 0x20000 && r <= 0x2A6DF:
		return true
	// Hiragana
	case r >= 0x3040 && r <= 0x309F:
		return true
	// Katakana
	case r >= 0x30A0 && r <= 0x30FF:
		return true
	// Hangul Syllables
	case r >= 0xAC00 && r <= 0xD7AF:
		return true
	// Hangul Jamo
	case r >= 0x1100 && r <= 0x11FF:
		return true
	default:
		return false
	}
}
