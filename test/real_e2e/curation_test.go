//go:build real_e2e

package real_e2e

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/memory/curation"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/provider/anthropic"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// TestCuration_RecallDoesNotRegress is the manual real-e2e for the curation
// engine (tasks.md 7.7a). With a live judge model it runs one full curation pass
// over a seeded store and asserts the load-bearing invariant from the
// memory-curation spec: a curation pass must NOT evict high-value memories — the
// agent's recall of them does not regress. Junk eviction and near-duplicate
// merging are the judge's discretion and only logged, not asserted, since they
// are inherently model-dependent.
//
// Gated like the other real-e2e tests: it needs a real model, so it skips unless
// DASHSCOPE_API_KEY is set (curation makes a single non-conversational judge
// call, so the conversation record/replay harness does not apply here).
func TestCuration_RecallDoesNotRegress(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		t.Skip("DASHSCOPE_API_KEY not set; curation real-e2e needs a live judge model")
	}
	baseURL := os.Getenv("DASHSCOPE_BASE_URL")
	if baseURL == "" {
		baseURL = "https://coding.dashscope.aliyuncs.com/apps/anthropic"
	}

	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	es := memory.NewEntryStore(st.DB())

	now := time.Now().UTC()
	old := now.AddDate(0, 0, -200) // well past the volatile age horizon

	// High-value keepers: frequently used, recent, evergreen. Recall of these
	// MUST survive a curation pass.
	keepers := []string{"build-command", "deploy-process", "oncall-runbook"}
	seedReal(t, es, &memory.Entry{Name: "build-command", Trigger: "before committing", Content: "Always run `make build && make test` before committing.", Durability: "evergreen", Category: "project", HitCount: 42, LastUsedAt: &now, CreatedAt: old})
	seedReal(t, es, &memory.Entry{Name: "deploy-process", Trigger: "when deploying", Content: "Deploy via `dw deploy --env prod` only after the staging smoke test passes.", Durability: "evergreen", Category: "project", HitCount: 25, LastUsedAt: &now, CreatedAt: old})
	seedReal(t, es, &memory.Entry{Name: "oncall-runbook", Trigger: "during an incident", Content: "On-call: check the dashboard, then escalate to the platform team if error rate > 5%.", Durability: "evergreen", Category: "reference", HitCount: 18, LastUsedAt: &now, CreatedAt: old})

	// A pinned entry — never a candidate, must always survive.
	seedReal(t, es, &memory.Entry{Name: "user-profile", Trigger: "user identity", Content: "The user is a senior Go engineer who prefers concise answers.", Pinned: true, Durability: "evergreen", Category: "user", CreatedAt: old})

	// Low-value junk: never used, old, trivial. Eviction candidates.
	seedReal(t, es, &memory.Entry{Name: "scratch-note", Trigger: "n/a", Content: "asdf testing 123", Durability: "volatile", Category: "", HitCount: 0, CreatedAt: old})
	seedReal(t, es, &memory.Entry{Name: "stale-todo", Trigger: "n/a", Content: "TODO: look into that thing later (from months ago).", Durability: "volatile", Category: "", HitCount: 0, CreatedAt: old})

	// A near-duplicate pair the judge may merge.
	seedReal(t, es, &memory.Entry{Name: "go-test-style-a", Trigger: "writing Go tests", Content: "Prefer table-driven tests and small focused interfaces in Go.", Durability: "volatile", Category: "project", HitCount: 1, CreatedAt: old})
	seedReal(t, es, &memory.Entry{Name: "go-test-style-b", Trigger: "writing tests in Go", Content: "Use table-driven tests and keep interfaces small when writing Go.", Durability: "volatile", Category: "project", HitCount: 1, CreatedAt: old})

	before, _ := es.Count(ctx)

	prov := anthropic.New(anthropic.Options{APIKey: apiKey, BaseURL: baseURL, DefaultMaxTokens: 2048})
	call, err := curation.NewProviderCaller(map[string]provider.Provider{"anthropic": prov}, "anthropic:"+defaultTestModel, 2048)
	if err != nil {
		t.Fatalf("build judge caller: %v", err)
	}

	w := curation.NewWorker(es, st.DB(), call, curation.Config{
		EntryCountHigh:       2, // count (8) is over the water line → pass runs
		MinInterval:          time.Minute,
		LeaseTTL:             60 * time.Second,
		ManifestBudgetChars:  2000,
		MaxCandidatesPerPass: 20,
		ContentSnippetChars:  1200,
		Weights:              curation.Weights{Hit: 1.0, Recency: 1.0, Age: 0.5, Volatility: 0.5},
		Budgets:              memory.DefaultBudgets(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	w.RunPass(ctx)

	after, _ := es.Count(ctx)
	t.Logf("curation pass: %d → %d entries (%d removed)", before, after, before-after)

	// Invariant: every high-value keeper and the pinned entry still exists.
	for _, name := range append(keepers, "user-profile") {
		if _, err := es.GetByName(ctx, name); err != nil {
			t.Errorf("recall regressed: high-value entry %q was removed by curation: %v", name, err)
		}
	}

	// Diagnostics only (judge discretion, not asserted).
	for _, name := range []string{"scratch-note", "stale-todo"} {
		if _, err := es.GetByName(ctx, name); errors.Is(err, store.ErrNotFound) {
			t.Logf("judge evicted low-value entry %q (expected-ish)", name)
		}
	}
	a := exists(ctx, es, "go-test-style-a")
	b := exists(ctx, es, "go-test-style-b")
	if !(a && b) {
		t.Logf("judge merged the near-duplicate go-test-style pair (a=%v b=%v)", a, b)
	}
}

func seedReal(t *testing.T, es *memory.EntryStore, e *memory.Entry) {
	t.Helper()
	if e.CharCount == 0 {
		e.CharCount = memory.CharCount(e.Content)
	}
	if err := es.Upsert(context.Background(), e); err != nil {
		t.Fatalf("seed %q: %v", e.Name, err)
	}
}

func exists(ctx context.Context, es *memory.EntryStore, name string) bool {
	_, err := es.GetByName(ctx, name)
	return err == nil
}
