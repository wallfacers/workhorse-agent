//go:build real_e2e

package real_e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/memory/curation"
	"github.com/wallfacers/workhorse-agent/internal/memory/pipeline"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/provider/anthropic"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// TestExtraction_Smoke is the manual real-e2e for the ADD-only extraction
// pipeline (tasks.md 4.5). With a live extraction model it ingests a short
// conversation and asserts the load-bearing invariants: at least one durable
// fact is stored as a new entry, tagged fact_source=extraction, and the entity
// index is populated. Exact fact wording is model-dependent and only logged.
//
// Gated like the other real-e2e tests: it needs a live model. Credentials come
// from the environment only (never hardcoded/committed):
//
//	EXTRACT_API_KEY   (required; test skips when unset)
//	EXTRACT_BASE_URL  (default: https://api.deepseek.com/anthropic)
//	EXTRACT_MODEL     (default: deepseek-v4-pro)
func TestExtraction_Smoke(t *testing.T) {
	apiKey := os.Getenv("EXTRACT_API_KEY")
	if apiKey == "" {
		t.Skip("EXTRACT_API_KEY not set; extraction real-e2e needs a live model")
	}
	baseURL := os.Getenv("EXTRACT_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/anthropic"
	}
	model := os.Getenv("EXTRACT_MODEL")
	if model == "" {
		model = "deepseek-v4-pro"
	}

	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	es := memory.NewEntryStore(st.DB())

	prov := anthropic.New(anthropic.Options{APIKey: apiKey, BaseURL: baseURL, DefaultMaxTokens: 2048})
	call, err := curation.NewProviderCaller(map[string]provider.Provider{"anthropic": prov}, "anthropic:"+model, 2048)
	if err != nil {
		t.Fatalf("build extract caller: %v", err)
	}

	p := pipeline.New(pipeline.Config{
		Entries: es,
		Budgets: memory.DefaultBudgets(),
		Call:    pipeline.ModelCaller(call),
	})

	sessionDate := time.Date(2023, 5, 20, 0, 0, 0, 0, time.UTC)
	messages := []pipeline.Message{
		{Role: "user", Text: "Hi! I'm Alex. I moved to Berlin from Stockholm about four years ago, and I work as a data engineer."},
		{Role: "assistant", Text: "Nice to meet you, Alex! Berlin is a great city for data engineering."},
		{Role: "user", Text: "Yeah. I mostly use Python and DuckDB. Also, remind me to renew my passport next month."},
	}

	n, err := p.Ingest(ctx, sessionDate, "smoke-session", messages)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	t.Logf("extraction produced %d entries", n)
	if n == 0 {
		t.Fatal("expected at least one extracted fact from a fact-rich conversation")
	}

	entries, _ := es.List(ctx)
	for _, e := range entries {
		t.Logf("entry: name=%s fact_source=%s event_date=%v content=%q", e.Name, e.FactSource, e.EventDate, e.Content)
		if e.FactSource != "extraction" {
			t.Errorf("entry %q: fact_source=%q, want extraction", e.Name, e.FactSource)
		}
	}

	// The conversation names concrete entities; at least one should be indexed.
	counts, err := es.EntityMatchCounts(ctx, memory.EntityQueryTokens("Berlin Stockholm Python"))
	if err != nil {
		t.Fatalf("entity match: %v", err)
	}
	if len(counts) == 0 {
		t.Error("expected at least one indexed entity from the conversation")
	}
}
