package tools_test

import (
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// tool-system spec scenario: glob 条目放行整个 MCP server.
func TestFiltered_GlobEntryMatchesServerPrefix(t *testing.T) {
	r := tools.NewRegistry()
	for _, n := range []string{"Read", "Bash", "dataweave__query_tasks", "dataweave__node_exec"} {
		if err := r.Register(fakeTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	got := names(r.Filtered([]string{"Read", "dataweave__*"}))
	want := map[string]bool{"Read": true, "dataweave__query_tasks": true, "dataweave__node_exec": true}
	if len(got) != len(want) {
		t.Fatalf("Filtered = %v", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("unexpected tool %s in %v", n, got)
		}
	}

	// A tool registered later is admitted by the same glob with no config change.
	if err := r.Register(fakeTool{name: "dataweave__query_lineage"}); err != nil {
		t.Fatal(err)
	}
	if got := names(r.Filtered([]string{"dataweave__*"})); len(got) != 3 {
		t.Fatalf("later-registered tool not admitted: %v", got)
	}
}

// tool-system spec: 无元字符条目保持精确匹配语义.
func TestFiltered_LiteralEntriesStayExact(t *testing.T) {
	r := tools.NewRegistry()
	for _, n := range []string{"Read", "ReadX", "Bash"} {
		if err := r.Register(fakeTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	got := names(r.Filtered([]string{"Read"}))
	if len(got) != 1 || got[0] != "Read" {
		t.Fatalf("literal entry must match exactly one tool: %v", got)
	}
	if got := r.Filtered(nil); len(got) != 3 {
		t.Fatalf("nil filter must return all tools, got %d", len(got))
	}
}
