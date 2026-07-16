package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/memory/pipeline"
	"github.com/wallfacers/workhorse-agent/internal/provider"
)

func TestSessionIngestor_ExtractsFromHistory(t *testing.T) {
	ctx := context.Background()
	es, _ := newStore(t)
	p := pipeline.New(pipeline.Config{
		Entries: es,
		Budgets: memory.DefaultBudgets(),
		Call:    staticCaller(`{"facts":[{"fact":"The user lives in Oslo.","entities":["Oslo"],"category":"user","durability":"evergreen"}]}`),
	})
	ing := pipeline.NewSessionIngestor(p)

	history := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "I live in Oslo"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "Got it, Oslo."}}},
	}
	ing.IngestSession("sess1", history)
	ing.Close() // waits for the detached goroutine

	entries, _ := es.List(ctx)
	if len(entries) != 1 || entries[0].Content != "The user lives in Oslo." {
		t.Fatalf("expected 1 extracted entry, got %+v", entries)
	}
}

func TestSessionIngestor_NilInert(t *testing.T) {
	var ing *pipeline.SessionIngestor
	// Must not panic.
	ing.IngestSession("s", []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockText, Text: "hi"}}}})
	ing.Close()
}

func TestSessionIngestor_SkipsToolOnlyHistory(t *testing.T) {
	es, _ := newStore(t)
	called := false
	p := pipeline.New(pipeline.Config{
		Entries: es,
		Budgets: memory.DefaultBudgets(),
		Call: func(_ context.Context, _, _ string) (string, error) {
			called = true
			return `{"facts":[]}`, nil
		},
	})
	ing := pipeline.NewSessionIngestor(p)
	// History with no user/assistant text blocks.
	ing.IngestSession("s", []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.BlockToolResult}}},
	})
	ing.Close()
	if called {
		t.Fatal("expected no extraction call for text-free history")
	}
	_ = time.Now
}
