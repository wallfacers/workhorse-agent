package jsonpath_test

import (
	"log/slog"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/extagent/jsonpath"
)

func TestCompile_DollarOnly(t *testing.T) {
	p, err := jsonpath.Compile("$")
	if err != nil {
		t.Fatalf("compile $: %v", err)
	}
	if p.String() != "$" {
		t.Errorf("String(): got %q", p.String())
	}
}

func TestCompile_FieldAccess(t *testing.T) {
	p, err := jsonpath.Compile("$.delta.text")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	root := map[string]any{
		"delta": map[string]any{"text": "hello"},
	}
	got := p.Extract(root, nil)
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestCompile_IndexAccess(t *testing.T) {
	p, err := jsonpath.Compile("$.choices[0].message.content")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	root := map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"content": "hi"}},
		},
	}
	got := p.Extract(root, nil)
	if got != "hi" {
		t.Errorf("got %q, want %q", got, "hi")
	}
}

func TestCompile_Wildcard(t *testing.T) {
	p, err := jsonpath.Compile("$.choices[*].text")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	root := map[string]any{
		"choices": []any{
			map[string]any{"text": "first"},
			map[string]any{"text": "second"},
		},
	}
	got := p.Extract(root, nil)
	if got != "first" {
		t.Errorf("got %q, want first non-empty", got)
	}
}

func TestCompile_NegativeIndex(t *testing.T) {
	p, err := jsonpath.Compile("$[-1]")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	root := []any{"a", "b", "c"}
	got := p.Extract(root, nil)
	if got != "c" {
		t.Errorf("got %q, want %q", got, "c")
	}
}

func TestCompile_BareIndex(t *testing.T) {
	p, err := jsonpath.Compile("$[0].text")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	root := []any{
		map[string]any{"text": "found"},
	}
	got := p.Extract(root, nil)
	if got != "found" {
		t.Errorf("got %q, want %q", got, "found")
	}
}

func TestExtract_NullPath(t *testing.T) {
	p, _ := jsonpath.Compile("$.missing.field")
	root := map[string]any{}
	got := p.Extract(root, slog.Default())
	if got != "" {
		t.Errorf("missing path should return empty, got %q", got)
	}
}

func TestExtract_NonStringCoerced(t *testing.T) {
	p, _ := jsonpath.Compile("$.count")
	root := map[string]any{"count": 42}
	got := p.Extract(root, nil)
	if got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestCompile_Errors(t *testing.T) {
	cases := []struct {
		input string
		desc  string
	}{
		{"", "empty"},
		{"delta.text", "missing dollar"},
		{"$..text", "recursive descent"},
		{"$.foo()", "filter expression"},
		{"$[1:3]", "slice"},
		{"$[\"key\"]", "string key"},
		{"$.foo.bar.", "trailing dot"},
	}
	for _, tc := range cases {
		_, err := jsonpath.Compile(tc.input)
		if err == nil {
			t.Errorf("compile(%q) [%s]: expected error", tc.input, tc.desc)
		}
	}
}

func TestIsValidGrammar(t *testing.T) {
	if !jsonpath.IsValidGrammar("$.delta.text") {
		t.Error("$.delta.text should be valid")
	}
	if jsonpath.IsValidGrammar("$..text") {
		t.Error("$..text should be invalid (recursive descent)")
	}
}

func TestExtract_IndexOutOfBounds(t *testing.T) {
	p, _ := jsonpath.Compile("$[5]")
	root := []any{"a"}
	got := p.Extract(root, slog.Default())
	if got != "" {
		t.Errorf("out of bounds should return empty, got %q", got)
	}
}
