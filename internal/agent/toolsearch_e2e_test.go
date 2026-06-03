package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/toolsearch"
)

// TestToolSearch_DiscoveryRoundTrip exercises the real ToolSearch tool, the
// real Session discovered set, and buildToolSchemas end to end: a deferrable
// tool is hidden until ToolSearch surfaces it, then stays visible — including
// across a simulated compaction.
func TestToolSearch_DiscoveryRoundTrip(t *testing.T) {
	fake := deferStub{tsStub{name: "slack__send", desc: "send a slack message", schema: `{"type":"object","properties":{"text":{"type":"string"}}}`}}
	l := newBuildLoop("tst", 0, fake, toolsearch.Tool{})

	// Turn 1: the deferrable tool is hidden; only its name is announced.
	schemas, deferred := schemaNames(l)
	if has(schemas, "slack__send") {
		t.Fatalf("turn 1: slack__send must be deferred, got schemas=%v", schemas)
	}
	if !has(deferred, "slack__send") {
		t.Fatalf("turn 1: slack__send must be announced, got %v", deferred)
	}

	// Model calls ToolSearch with the catalog buildToolSchemas exposed.
	env := &tools.Env{ToolCatalog: l.ToolEnv.ToolCatalog}
	raw, _ := json.Marshal(map[string]any{"query": "select:slack__send"})
	res, err := toolsearch.Tool{}.Run(context.Background(), env, raw)
	if err != nil {
		t.Fatalf("ToolSearch: %v", err)
	}
	if res.Modifier == nil {
		t.Fatal("ToolSearch should return a discovery modifier")
	}
	if err := res.Modifier.Apply(l.Session); err != nil {
		t.Fatalf("apply modifier: %v", err)
	}

	// Turn 2: the tool is now loaded with full schema and no longer announced.
	schemas, deferred = schemaNames(l)
	if !has(schemas, "slack__send") {
		t.Fatalf("turn 2: discovered tool must be loaded, got %v", schemas)
	}
	if has(deferred, "slack__send") {
		t.Errorf("turn 2: discovered tool must not be re-announced, got %v", deferred)
	}

	// Simulate compaction: history is replaced but the discovered set lives on
	// the session and must survive.
	l.Session.ReplaceHistory(context.Background(), []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "summary"}}},
	})
	schemas, _ = schemaNames(l)
	if !has(schemas, "slack__send") {
		t.Errorf("post-compaction: discovered tool must remain loaded, got %v", schemas)
	}
}
