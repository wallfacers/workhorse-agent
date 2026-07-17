package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

func TestParseIndexList(t *testing.T) {
	cases := []struct {
		raw  string
		n    int
		want []int
	}{
		{"3, 7, 12", 20, []int{3, 7, 12}},
		{"3,7,12", 20, []int{3, 7, 12}},
		{"Selected: 1, 2 and 5.", 5, []int{1, 2, 5}},
		{"1\n2\n3", 3, []int{1, 2, 3}},
		{"5, 5, 5", 5, []int{5}},       // dedupe
		{"0, 4, 99", 10, []int{4}},     // out of range dropped
		{"no numbers here", 10, nil},   // nothing → fallback
		{"", 10, nil},                  // empty → fallback
		{"12", 5, nil},                 // single out-of-range
		{"2, 1", 5, []int{2, 1}},       // reply order preserved
		{"999999999999999999", 5, nil}, // overflow guard
	}
	for _, c := range cases {
		if got := parseIndexList(c.raw, c.n); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseIndexList(%q, %d) = %v, want %v", c.raw, c.n, got, c.want)
		}
	}
}

func TestTruncateLine(t *testing.T) {
	if got := truncateLine("a\nb\tc  d", 100); got != "a b c d" {
		t.Errorf("flatten = %q", got)
	}
	long := truncateLine(strings.Repeat("界", 500), 350)
	if n := utf8.RuneCountInString(long); n != 351 { // 350 + ellipsis
		t.Errorf("truncated length = %d runes", n)
	}
}

func TestListwiseSelect(t *testing.T) {
	cands := []memory.Result{
		{Name: "fact-a", Content: "Caroline adopted a dog named Max."},
		{Name: "chunk-c0-s1-000", Content: "Melanie: I went hiking.\nCaroline: Nice!"},
		{Name: "fact-b", Content: "Melanie takes pottery classes."},
	}
	var gotUser string
	call := func(ctx context.Context, system, user string) (string, error) {
		gotUser = user
		return "1, 3", nil
	}
	out, ok := listwiseSelect(context.Background(), call, "What pets?", cands)
	if !ok || len(out) != 2 || out[0].Name != "fact-a" || out[1].Name != "fact-b" {
		t.Fatalf("select = %v ok=%v", out, ok)
	}
	if !strings.Contains(gotUser, "QUESTION: What pets?") || !strings.Contains(gotUser, "2. Melanie: I went hiking. Caroline: Nice!") {
		t.Errorf("filter prompt malformed:\n%s", gotUser)
	}

	failing := func(ctx context.Context, system, user string) (string, error) {
		return "", fmt.Errorf("boom")
	}
	if _, ok := listwiseSelect(context.Background(), failing, "q", cands); ok {
		t.Error("call error must report ok=false for fallback")
	}
	empty := func(ctx context.Context, system, user string) (string, error) {
		return "none of these are relevant", nil
	}
	if _, ok := listwiseSelect(context.Background(), empty, "q", cands); ok {
		t.Error("unparseable reply must report ok=false for fallback")
	}
}

func TestParseCatOverrides(t *testing.T) {
	m, err := parseCatOverrides("1=150, 4=30")
	if err != nil || m[1] != 150 || m[4] != 30 || len(m) != 2 {
		t.Fatalf("parse = %v, %v", m, err)
	}
	if m, err := parseCatOverrides(""); err != nil || len(m) != 0 {
		t.Fatalf("empty spec = %v, %v", m, err)
	}
	for _, bad := range []string{"1", "x=5", "1=0", "0=10", "1=a"} {
		if _, err := parseCatOverrides(bad); err == nil {
			t.Errorf("spec %q must fail", bad)
		}
	}
}
