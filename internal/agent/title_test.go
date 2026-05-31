package agent

import (
	"strings"
	"testing"
)

func TestDeriveTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trims whitespace", "  hello world  ", "hello world"},
		{"first line only", "first line\nsecond line", "first line"},
		{"carriage return", "title\r\nrest", "title"},
		{"short kept", "重构登录流程", "重构登录流程"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveTitle(tc.in); got != tc.want {
				t.Errorf("deriveTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDeriveTitle_Truncates(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := deriveTitle(long)
	if len([]rune(got)) > 80 {
		t.Errorf("title not capped at 80 runes: got %d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated title should end with ellipsis: %q", got)
	}
}
