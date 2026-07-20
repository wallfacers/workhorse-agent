package dispatch_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/tools/dispatch"
)

func TestFormatActivity(t *testing.T) {
	cases := []struct {
		name string
		tool string
		in   string
		want string
	}{
		{"read by path", "Read", `{"path":"/a/b.go"}`, "Read /a/b.go"},
		{"read by file_path", "Read", `{"file_path":"/x/y.go"}`, "Read /x/y.go"},
		{"read empty", "Read", `{}`, "Read"},
		{"grep quoted", "Grep", `{"pattern":"foo"}`, `Grep "foo"`},
		{"grep empty", "Grep", `{}`, "Grep"},
		{"bash command", "Bash", `{"command":"ls -la"}`, "ls -la"},
		{"session_search", "session_search", `{"query":"auth flow"}`, `Search "auth flow"`},
		{"memory search", "MemorySearch", `{"query":"prefs"}`, `Search "prefs"`},
		{"unknown tool name", "WeirdTool", `{"x":1}`, "WeirdTool"},
		{"bash multiline folded", "Bash", `{"command":"echo a\n  echo b"}`, "echo a echo b"},
		{"grep multiline pattern folded", "Grep", `{"pattern":"foo\nbar"}`, `Grep "foo bar"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dispatch.FormatActivity(tc.tool, json.RawMessage(tc.in))
			if got != tc.want {
				t.Fatalf("FormatActivity(%q, %s) = %q, want %q", tc.tool, tc.in, got, tc.want)
			}
			if strings.Contains(got, "\n") {
				t.Fatalf("activity must be single-line, got %q", got)
			}
		})
	}
}

func TestFormatActivity_TruncatesByRuneNotByte(t *testing.T) {
	// 100 ASCII chars → capped at 80 runes (79 + ellipsis).
	long := strings.Repeat("a", 100)
	got := dispatch.FormatActivity("Read", json.RawMessage(`{"path":"`+long+`"}`))
	if rc := len([]rune(got)); rc != 80 {
		t.Fatalf("ascii truncate rune count: got %d want 80 (%q)", rc, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("ascii truncate must end with ellipsis: %q", got)
	}

	// CJK: each char is 3 bytes. 100 runes → capped at 80 runes, not 80 bytes,
	// and the boundary must not split a multibyte sequence.
	cjk := strings.Repeat("中", 100)
	got = dispatch.FormatActivity("Read", json.RawMessage(`{"path":"`+cjk+`"}`))
	if rc := len([]rune(got)); rc != 80 {
		t.Fatalf("cjk truncate rune count: got %d want 80 (%q)", rc, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("cjk truncate must end with ellipsis: %q", got)
	}
	// valid UTF-8 (no mid-character split): the bytes round-trip.
	if !utf8Valid(got) {
		t.Fatalf("cjk truncate produced invalid UTF-8: %q", got)
	}
}

func utf8Valid(s string) bool {
	return json.Valid([]byte(`"` + s + `"`))
}
