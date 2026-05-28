package jsonpath

import (
	"fmt"
	"log/slog"
	"strconv"
	"unicode"
)

// Path represents a compiled JSONPath expression within the restricted grammar:
//
//	path := "$" segment*
//	segment := "." identifier | "[" (integer | "*") "]"
//	identifier := [A-Za-z_][A-Za-z0-9_]*
//	integer := -?[0-9]+
type Path struct {
	segments []segment
	raw      string
}

type segment struct {
	kind  segKind // field, index, wildcard
	ident string  // for field
	idx   int     // for index
}

type segKind int

const (
	segField segKind = iota
	segIndex
	segWildcard
)

// Compile parses a JSONPath expression. Returns an error for out-of-grammar input.
func Compile(s string) (Path, error) {
	if len(s) == 0 || s[0] != '$' {
		return Path{}, fmt.Errorf("jsonpath: must start with '$', got %q", s)
	}
	p := Path{raw: s}
	i := 1
	for i < len(s) {
		switch s[i] {
		case '.':
			i++
			ident, end, err := parseIdent(s, i)
			if err != nil {
				return Path{}, err
			}
			p.segments = append(p.segments, segment{kind: segField, ident: ident})
			i = end
		case '[':
			i++
			if i < len(s) && s[i] == '*' {
				if i+1 >= len(s) || s[i+1] != ']' {
					return Path{}, fmt.Errorf("jsonpath: expected ']' after [* at position %d", i-1)
				}
				p.segments = append(p.segments, segment{kind: segWildcard})
				i += 2
			} else {
				num, end, err := parseInt(s, i)
				if err != nil {
					return Path{}, err
				}
				if end >= len(s) || s[end] != ']' {
					return Path{}, fmt.Errorf("jsonpath: expected ']' at position %d", end)
				}
				p.segments = append(p.segments, segment{kind: segIndex, idx: num})
				i = end + 1
			}
		default:
			return Path{}, fmt.Errorf("jsonpath: unexpected char %q at position %d", s[i], i)
		}
	}
	return p, nil
}

// Extract evaluates the path against a JSON-decoded value (any).
// Returns empty string for null/undefined paths (with debug log).
// Non-string values are coerced via fmt.Sprintf("%v", v).
func (p Path) Extract(root any, logger *slog.Logger) string {
	return extract(root, p.segments, p.raw, logger)
}

func extract(current any, segs []segment, raw string, logger *slog.Logger) string {
	for i, seg := range segs {
		if current == nil {
			if logger != nil {
				logger.Debug("jsonpath: nil encountered", "path", raw)
			}
			return ""
		}
		switch seg.kind {
		case segField:
			m, ok := current.(map[string]any)
			if !ok {
				if logger != nil {
					logger.Debug("jsonpath: not an object at field access", "path", raw, "field", seg.ident)
				}
				return ""
			}
			current = m[seg.ident]
		case segIndex:
			arr, ok := current.([]any)
			if !ok {
				if logger != nil {
					logger.Debug("jsonpath: not an array at index access", "path", raw, "index", seg.idx)
				}
				return ""
			}
			idx := seg.idx
			if idx < 0 {
				idx = len(arr) + idx
			}
			if idx < 0 || idx >= len(arr) {
				if logger != nil {
					logger.Debug("jsonpath: index out of bounds", "path", raw, "index", seg.idx, "len", len(arr))
				}
				return ""
			}
			current = arr[idx]
		case segWildcard:
			rest := segs[i+1:]
			switch v := current.(type) {
			case []any:
				for _, elem := range v {
					if s := extract(elem, rest, raw, logger); s != "" {
						return s
					}
				}
				return ""
			case map[string]any:
				for _, val := range v {
					if s := extract(val, rest, raw, logger); s != "" {
						return s
					}
				}
				return ""
			default:
				return ""
			}
		}
	}
	return coerceString(current)
}

func coerceString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func parseIdent(s string, start int) (string, int, error) {
	if start >= len(s) {
		return "", start, fmt.Errorf("jsonpath: expected identifier at position %d", start)
	}
	if !isIdentStart(rune(s[start])) {
		return "", start, fmt.Errorf("jsonpath: invalid identifier start %q at position %d", s[start], start)
	}
	i := start + 1
	for i < len(s) && isIdentCont(rune(s[i])) {
		i++
	}
	return s[start:i], i, nil
}

func parseInt(s string, start int) (int, int, error) {
	i := start
	if i < len(s) && s[i] == '-' {
		i++
	}
	if i >= len(s) || !unicode.IsDigit(rune(s[i])) {
		return 0, i, fmt.Errorf("jsonpath: expected integer at position %d", start)
	}
	for i < len(s) && unicode.IsDigit(rune(s[i])) {
		i++
	}
	n, err := strconv.Atoi(s[start:i])
	if err != nil {
		return 0, i, fmt.Errorf("jsonpath: invalid integer at position %d: %v", start, err)
	}
	return n, i, nil
}

func isIdentStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
}

func isIdentCont(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}

// String returns the original path expression.
func (p Path) String() string { return p.raw }

// IsValidGrammar checks if the string matches the restricted JSONPath grammar.
func IsValidGrammar(s string) bool {
	_, err := Compile(s)
	return err == nil
}
