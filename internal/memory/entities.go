package memory

import (
	"strings"
	"unicode"
)

// EntityNorm normalizes an entity string into its index key: trimmed, folded to
// lower case, and with internal whitespace runs collapsed to a single space.
// Returns "" for entities that carry no indexable content. Both the extraction
// indexer (PutEntities) and the query tokenizer (EntityQueryTokens) route
// through this so the entity-match retrieval signal compares like with like.
func EntityNorm(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// EntityQueryTokens derives candidate entity keys from a free-text query for the
// entity-match retrieval signal. It emits both single word-like runs and the
// whole normalized query, so multi-word entities ("new york") and single-word
// entities ("sweden") both have a chance to match an indexed entity_norm.
// CJK runs are emitted whole (word segmentation is out of scope); ASCII/digit
// runs are split on non-alphanumeric boundaries.
func EntityQueryTokens(query string) []string {
	norm := EntityNorm(query)
	if norm == "" {
		return nil
	}
	out := []string{norm}
	seen := map[string]struct{}{norm: {}}

	add := func(tok string) {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return
		}
		if _, dup := seen[tok]; dup {
			return
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}

	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			add(cur.String())
			cur.Reset()
		}
	}
	for _, r := range norm {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			// Each CJK character is a candidate; also accumulate runs below.
			flush()
			add(string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}
