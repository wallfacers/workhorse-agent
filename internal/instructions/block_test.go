package instructions

import (
	"strings"
	"testing"
)

func TestBlock_nil(t *testing.T) {
	if got := Block(nil); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestBlock_empty(t *testing.T) {
	if got := Block(&Snapshot{}); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestBlock_singleFile(t *testing.T) {
	got := Block(&Snapshot{
		Files: []File{{Path: "/project/AGENTS.md", Content: "use Go"}},
	})
	want := `<instructions>
Instructions from: /project/AGENTS.md
use Go
</instructions>`
	if got != want {
		t.Fatalf("expected:\n%s\ngot:\n%s", want, got)
	}
}

func TestBlock_multipleFiles(t *testing.T) {
	got := Block(&Snapshot{
		Files: []File{
			{Path: "/project/src/AGENTS.md", Content: "subdir rules"},
			{Path: "/project/AGENTS.md", Content: "root rules"},
		},
	})
	if !strings.Contains(got, "---\n") {
		t.Fatal("expected --- separator between files")
	}
	if !strings.HasPrefix(got, "<instructions>\n") {
		t.Fatal("expected <instructions> opening tag")
	}
	if !strings.HasSuffix(got, "</instructions>") {
		t.Fatal("expected </instructions> closing tag")
	}
	if strings.Count(got, "Instructions from:") != 2 {
		t.Fatal("expected 2 header lines")
	}
}
