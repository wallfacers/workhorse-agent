package session

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/tools/toolsearch"
)

func TestMarkAndDiscoveredTools(t *testing.T) {
	s := New(Options{Ephemeral: true})
	if got := s.DiscoveredTools(); len(got) != 0 {
		t.Fatalf("fresh session should have no discovered, got %v", got)
	}
	s.MarkToolsDiscovered([]string{"slack__send", "github__create_pr"})
	s.MarkToolsDiscovered([]string{"slack__send"}) // idempotent
	got := map[string]bool{}
	for _, n := range s.DiscoveredTools() {
		got[n] = true
	}
	if len(got) != 2 || !got["slack__send"] || !got["github__create_pr"] {
		t.Errorf("expected 2 unique discovered, got %v", s.DiscoveredTools())
	}
}

func TestMarkToolsDiscovered_RaceFree(t *testing.T) {
	s := New(Options{Ephemeral: true})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.MarkToolsDiscovered([]string{"a", "b"}) }()
		go func() { defer wg.Done(); _ = s.DiscoveredTools() }()
	}
	wg.Wait()
	got := s.DiscoveredTools()
	if len(got) != 2 {
		t.Errorf("expected 2 discovered after concurrent marks, got %v", got)
	}
}

func TestRestoreHistory_RebuildsDiscovered(t *testing.T) {
	s := New(Options{Ephemeral: true})
	body := `<functions><function>{"description":"x","name":"slack__send","parameters":{}}</function></functions>`
	hist := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: provider.BlockToolUse, ToolUseID: "tu1", ToolName: toolsearch.Name, Input: json.RawMessage(`{"query":"select:slack__send"}`)},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{
			{Type: provider.BlockToolResult, ToolUseID: "tu1", Output: body},
		}},
	}
	s.RestoreHistory(hist)
	got := s.DiscoveredTools()
	if len(got) != 1 || got[0] != "slack__send" {
		t.Errorf("expected slack__send rebuilt from history, got %v", got)
	}
}
