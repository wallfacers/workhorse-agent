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
	// CJK Unified Ideographs Extension C
	case r >= 0x2A700 && r <= 0x2B73F:
		return true
	// CJK Unified Ideographs Extension D
	case r >= 0x2B740 && r <= 0x2B81F:
		return true
	// CJK Unified Ideographs Extension E
	case r >= 0x2B820 && r <= 0x2CEAF:
		return true
	// CJK Unified Ideographs Extension F
	case r >= 0x2CEB0 && r <= 0x2EBEF:
		return true
	// CJK Compatibility Ideographs
	case r >= 0xF900 && r <= 0xFAFF:
		return true
	// CJK Radicals Supplement
	case r >= 0x2E80 && r <= 0x2EFF:
		return true
	// CJK Symbols and Punctuation
	case r >= 0x3000 && r <= 0x303F:
		return true
	// Hiragana
	case r >= 0x3040 && r <= 0x309F:
		return true
	// Katakana
	case r >= 0x30A0 && r <= 0x30FF:
		return true
	// Katakana Phonetic Extensions
	case r >= 0x31F0 && r <= 0x31FF:
		return true
	// Hangul Syllables
	case r >= 0xAC00 && r <= 0xD7AF:
		return true
	// Hangul Jamo
	case r >= 0x1100 && r <= 0x11FF:
		return true
	// Hangul Compatibility Jamo
	case r >= 0x3130 && r <= 0x318F:
		return true
	// Halfwidth Katakana
	case r >= 0xFF65 && r <= 0xFF9F:
		return true
	default:
		return false
	}
}
