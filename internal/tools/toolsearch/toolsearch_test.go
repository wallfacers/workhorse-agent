package toolsearch_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/toolsearch"
)

// stubCatalog implements tools.ToolCatalog.
type stubCatalog struct{ infos []tools.ToolInfo }

func (c stubCatalog) DeferredTools() []tools.ToolInfo { return c.infos }

func sampleCatalog() stubCatalog {
	return stubCatalog{infos: []tools.ToolInfo{
		{Name: "slack__send", Description: "Send a message to a Slack channel.", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)},
		{Name: "slack__list_channels", Description: "List Slack channels.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "github__create_pr", Description: "Create a GitHub pull request.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "notebook__edit", Description: "Edit a Jupyter notebook cell.", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
}

func run(t *testing.T, cat tools.ToolCatalog, query string, maxResults int) *tools.Result {
	t.Helper()
	in := map[string]any{"query": query}
	if maxResults > 0 {
		in["max_results"] = maxResults
	}
	raw, _ := json.Marshal(in)
	env := &tools.Env{ToolCatalog: cat}
	res, err := toolsearch.Tool{}.Run(context.Background(), env, raw)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	return res
}

// recordTarget captures MarkToolsDiscovered calls.
type recordTarget struct{ discovered []string }

func (r *recordTarget) SetAllowedTools([]string) {}
func (r *recordTarget) MarkToolsDiscovered(names []string) {
	r.discovered = append(r.discovered, names...)
}

func applyModifier(t *testing.T, res *tools.Result) []string {
	t.Helper()
	if res.Modifier == nil {
		return nil
	}
	rt := &recordTarget{}
	if err := res.Modifier.Apply(rt); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return rt.discovered
}

// ---- 3.1 select ----

func TestSelect_MultiExact(t *testing.T) {
	res := run(t, sampleCatalog(), "select:slack__send,github__create_pr", 0)
	out := res.Output
	if !strings.Contains(out, `"name":"slack__send"`) || !strings.Contains(out, `"name":"github__create_pr"`) {
		t.Fatalf("expected both tools in output, got:\n%s", out)
	}
	if strings.Contains(out, "notebook__edit") {
		t.Errorf("unexpected tool leaked into select result")
	}
	disc := applyModifier(t, res)
	if len(disc) != 2 {
		t.Errorf("expected 2 discovered, got %v", disc)
	}
}

func TestSelect_PartialMissing(t *testing.T) {
	res := run(t, sampleCatalog(), "select:slack__send,does__not_exist", 0)
	if !strings.Contains(res.Output, `"name":"slack__send"`) {
		t.Fatalf("expected slack__send, got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "does__not_exist") {
		t.Errorf("missing tool should not appear")
	}
	if disc := applyModifier(t, res); len(disc) != 1 || disc[0] != "slack__send" {
		t.Errorf("expected only slack__send discovered, got %v", disc)
	}
}

func TestSelect_NoneFound(t *testing.T) {
	res := run(t, sampleCatalog(), "select:nope", 0)
	if res.Modifier != nil {
		t.Errorf("no match should not mark anything discovered")
	}
	if !strings.Contains(res.Output, "No matching") {
		t.Errorf("expected no-match message, got %q", res.Output)
	}
}

// ---- 3.2 keyword ----

func TestKeyword_RankedByRelevance(t *testing.T) {
	res := run(t, sampleCatalog(), "slack send", 5)
	// slack__send should rank first (matches both name parts).
	first := strings.Index(res.Output, `"name":"slack__send"`)
	listIdx := strings.Index(res.Output, `"name":"slack__list_channels"`)
	if first < 0 {
		t.Fatalf("slack__send missing:\n%s", res.Output)
	}
	if listIdx >= 0 && first > listIdx {
		t.Errorf("slack__send should rank before slack__list_channels")
	}
	if strings.Contains(res.Output, "github__create_pr") {
		t.Errorf("github should not match 'slack send'")
	}
}

func TestKeyword_MaxResults(t *testing.T) {
	res := run(t, sampleCatalog(), "slack", 1)
	if n := strings.Count(res.Output, "<function>"); n != 1 {
		t.Errorf("expected max_results=1 to cap at 1, got %d", n)
	}
}

func TestKeyword_RequiredTerm(t *testing.T) {
	// "+notebook edit" requires notebook; only notebook__edit qualifies even
	// though slack__send / github also have no 'edit'.
	res := run(t, sampleCatalog(), "+notebook edit", 5)
	if !strings.Contains(res.Output, "notebook__edit") {
		t.Fatalf("expected notebook__edit, got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "slack__send") || strings.Contains(res.Output, "github__create_pr") {
		t.Errorf("required term should exclude non-notebook tools")
	}
}

func TestKeyword_NoMatchNoModifier(t *testing.T) {
	res := run(t, sampleCatalog(), "kubernetes deploy", 5)
	if res.Modifier != nil {
		t.Errorf("no-match keyword search must not mark discovered")
	}
	if !strings.Contains(res.Output, "No matching") {
		t.Errorf("expected no-match message, got %q", res.Output)
	}
}

// ---- 3.3 functions block + matches ----

func TestRender_FunctionsBlockParseable(t *testing.T) {
	res := run(t, sampleCatalog(), "select:slack__send", 0)
	out := res.Output
	if !strings.HasPrefix(out, "<functions>") || !strings.HasSuffix(out, "</functions>") {
		t.Fatalf("not a functions block:\n%s", out)
	}
	// The parameters must be the real input schema (contains the text prop).
	if !strings.Contains(out, `"parameters":{"type":"object","properties":{"text"`) {
		t.Errorf("expected embedded parameters schema, got:\n%s", out)
	}
}

func TestEmptyCatalog(t *testing.T) {
	res := run(t, stubCatalog{}, "slack", 5)
	if res.Modifier != nil || !strings.Contains(res.Output, "No matching") {
		t.Errorf("empty catalog should yield no-match, got %q", res.Output)
	}
}

func TestNilCatalog(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"query": "slack"})
	res, err := toolsearch.Tool{}.Run(context.Background(), &tools.Env{}, raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(res.Output, "No matching") {
		t.Errorf("nil catalog should yield no-match, got %q", res.Output)
	}
}

// ---- 2.4 reconstruction ----

func TestReconstructDiscovered(t *testing.T) {
	// Simulate history: an assistant ToolSearch tool_use + a user tool_result
	// carrying the rendered <functions> block.
	res := run(t, sampleCatalog(), "select:slack__send,github__create_pr", 0)
	history := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: toolsearch.Name, Input: json.RawMessage(`{"query":"select:slack__send,github__create_pr"}`)},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{
			{Type: provider.BlockToolResult, ToolUseID: "tu1", Output: res.Output},
		}},
	}
	got := toolsearch.ReconstructDiscovered(history)
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	if !set["slack__send"] || !set["github__create_pr"] {
		t.Errorf("expected both reconstructed, got %v", got)
	}
}

func TestReconstruct_IgnoresNonToolSearch(t *testing.T) {
	// A tool_result with a functions-looking body but NOT from ToolSearch must
	// be ignored (correlated by tool_use name).
	history := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockToolUse, ToolUseID: "x", ToolName: "SomethingElse"},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{
			{Type: provider.BlockToolResult, ToolUseID: "x", Output: `<functions><function>{"name":"slack__send"}</function></functions>`},
		}},
	}
	if got := toolsearch.ReconstructDiscovered(history); len(got) != 0 {
		t.Errorf("non-ToolSearch result must be ignored, got %v", got)
	}
}
