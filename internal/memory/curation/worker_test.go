package curation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func seed(t *testing.T, es *memory.EntryStore, e *memory.Entry) {
	t.Helper()
	if e.CharCount == 0 {
		e.CharCount = memory.CharCount(e.Content)
	}
	if err := es.Upsert(context.Background(), e); err != nil {
		t.Fatalf("seed %q: %v", e.Name, err)
	}
}

func TestWorkerAppliesEvictAndMerge(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	es := memory.NewEntryStore(s.DB())

	seed(t, es, &memory.Entry{Name: "keep-me", Trigger: "useful", Content: "keep this", Durability: "evergreen", HitCount: 9})
	seed(t, es, &memory.Entry{Name: "evict-me", Trigger: "stale", Content: "obsolete", Durability: "volatile"})
	seed(t, es, &memory.Entry{Name: "dup-a", Trigger: "dupe", Content: "same fact one", Durability: "volatile"})
	seed(t, es, &memory.Entry{Name: "dup-b", Trigger: "dupe", Content: "same fact two", Durability: "volatile"})
	seed(t, es, &memory.Entry{Name: "pinned-user", Trigger: "id", Content: "the user", Pinned: true, Durability: "evergreen"})

	call := func(ctx context.Context, system, user string) (string, error) {
		return `{"evict":["evict-me"],"merge":[{"names":["dup-a","dup-b"],"into":{"name":"dup-a","trigger":"merged","content":"same fact one and two","durability":"volatile","category":"project"}}]}`, nil
	}
	w := NewWorker(es, s.DB(), call, Config{
		EntryCountHigh: 0, MinInterval: time.Minute, LeaseTTL: 60 * time.Second,
		MaxCandidatesPerPass: 20, ContentSnippetChars: 200, Weights: defaultWeights, Budgets: memory.DefaultBudgets(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	w.RunPass(ctx)

	assertGone(t, es, "evict-me")
	assertGone(t, es, "dup-b")
	assertExists(t, es, "keep-me")
	assertExists(t, es, "pinned-user")
	got := mustGet(t, es, "dup-a")
	if got.Content != "same fact one and two" || got.Trigger != "merged" {
		t.Fatalf("dup-a not merged: %+v", got)
	}
}

func TestWorkerRefusesToEvictPinned(t *testing.T) {
	ctx := context.Background()
	s, _ := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	t.Cleanup(func() { _ = s.Close() })
	es := memory.NewEntryStore(s.DB())
	seed(t, es, &memory.Entry{Name: "pinned-user", Content: "the user", Pinned: true, Durability: "evergreen"})
	seed(t, es, &memory.Entry{Name: "filler", Content: "x", Durability: "volatile"})

	call := func(ctx context.Context, system, user string) (string, error) {
		// Judge maliciously/erroneously targets the pinned entry and an unknown name.
		return `{"evict":["pinned-user","ghost"],"merge":[]}`, nil
	}
	w := NewWorker(es, s.DB(), call, Config{
		EntryCountHigh: 0, MinInterval: time.Minute, LeaseTTL: 60 * time.Second,
		MaxCandidatesPerPass: 20, ContentSnippetChars: 200, Weights: defaultWeights, Budgets: memory.DefaultBudgets(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	w.RunPass(ctx)

	assertExists(t, es, "pinned-user") // never evicted
}

func TestWorkerFailSafeOnBadJudgeOutput(t *testing.T) {
	ctx := context.Background()
	s, _ := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	t.Cleanup(func() { _ = s.Close() })
	es := memory.NewEntryStore(s.DB())
	seed(t, es, &memory.Entry{Name: "a", Content: "one", Durability: "volatile"})
	seed(t, es, &memory.Entry{Name: "b", Content: "two", Durability: "volatile"})

	call := func(ctx context.Context, system, user string) (string, error) {
		return "the model rambled and produced no json", nil
	}
	w := NewWorker(es, s.DB(), call, Config{
		EntryCountHigh: 0, MinInterval: time.Minute, LeaseTTL: 60 * time.Second,
		MaxCandidatesPerPass: 20, ContentSnippetChars: 200, Weights: defaultWeights, Budgets: memory.DefaultBudgets(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	w.RunPass(ctx) // must not panic, must not mutate

	assertExists(t, es, "a")
	assertExists(t, es, "b")
}

func TestWorkerCallErrorIsFailSafe(t *testing.T) {
	ctx := context.Background()
	s, _ := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	t.Cleanup(func() { _ = s.Close() })
	es := memory.NewEntryStore(s.DB())
	seed(t, es, &memory.Entry{Name: "a", Content: "one", Durability: "volatile"})

	call := func(ctx context.Context, system, user string) (string, error) {
		return "", errors.New("model overloaded")
	}
	w := NewWorker(es, s.DB(), call, Config{
		EntryCountHigh: 0, MinInterval: time.Minute, LeaseTTL: 60 * time.Second,
		MaxCandidatesPerPass: 20, ContentSnippetChars: 200, Weights: defaultWeights, Budgets: memory.DefaultBudgets(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	w.RunPass(ctx)
	assertExists(t, es, "a")
}

func TestWorkerSkipsMergeOverBudget(t *testing.T) {
	ctx := context.Background()
	s, _ := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	t.Cleanup(func() { _ = s.Close() })
	es := memory.NewEntryStore(s.DB())
	seed(t, es, &memory.Entry{Name: "dup-a", Content: "fact one", Durability: "volatile"})
	seed(t, es, &memory.Entry{Name: "dup-b", Content: "fact two", Durability: "volatile"})

	big := make([]byte, 0)
	for i := 0; i < 5000; i++ {
		big = append(big, 'x')
	}
	call := func(ctx context.Context, system, user string) (string, error) {
		return `{"evict":[],"merge":[{"names":["dup-a","dup-b"],"into":{"name":"dup-a","trigger":"t","content":"` + string(big) + `","durability":"volatile"}}]}`, nil
	}
	budgets := memory.DefaultBudgets() // EntryContentChars=1200
	w := NewWorker(es, s.DB(), call, Config{
		EntryCountHigh: 0, MinInterval: time.Minute, LeaseTTL: 60 * time.Second,
		MaxCandidatesPerPass: 20, ContentSnippetChars: 200, Weights: defaultWeights, Budgets: budgets,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	w.RunPass(ctx)

	// Over-budget merge skipped: both sources survive unchanged.
	assertExists(t, es, "dup-a")
	assertExists(t, es, "dup-b")
}

func TestWorkerWaterLines(t *testing.T) {
	ctx := context.Background()
	s, _ := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	t.Cleanup(func() { _ = s.Close() })
	es := memory.NewEntryStore(s.DB())
	seed(t, es, &memory.Entry{Name: "a", Trigger: "t", Content: "one", Durability: "volatile"})

	now := time.Unix(1_700_000_000, 0).UTC()
	mk := func(high, manifestBudget int, minInterval time.Duration) *Worker {
		w := NewWorker(es, s.DB(), failCall(t), Config{
			EntryCountHigh: high, MinInterval: minInterval, LeaseTTL: 60 * time.Second,
			ManifestBudgetChars: manifestBudget, MaxCandidatesPerPass: 20, ContentSnippetChars: 200,
			Weights: defaultWeights, Budgets: memory.DefaultBudgets(),
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		w.nowFn = func() time.Time { return now }
		return w
	}

	// Count water line: count(1) > high(0).
	wc := mk(0, 100000, time.Minute)
	wc.setLastPass(now) // recent, so time-fallback does not mask the count trigger
	if !wc.shouldRun(ctx, now) {
		t.Fatal("count > high should trigger")
	}

	// Below all water lines (count ≤ high, recent pass, manifest under budget) → no run.
	wq := mk(5, 100000, time.Minute)
	wq.setLastPass(now)
	if wq.shouldRun(ctx, now.Add(30*time.Second)) {
		t.Fatal("should not run below every water line")
	}

	// Time-based fallback: count ≤ high but min_interval elapsed since the last pass.
	if !wq.shouldRun(ctx, now.Add(2*time.Minute)) {
		t.Fatal("time-based fallback should trigger once min_interval elapsed")
	}

	// First-ever pass (last is zero) is a time-based trigger even under the count.
	wf := mk(5, 100000, time.Minute)
	if !wf.shouldRun(ctx, now) {
		t.Fatal("a never-run worker should trigger its first pass")
	}

	// Manifest-size water line: count ≤ high, recent pass, but estimated manifest
	// size (name+trigger+overhead) exceeds the tiny budget.
	wm := mk(5, 1, time.Minute)
	wm.setLastPass(now)
	if !wm.shouldRun(ctx, now.Add(30*time.Second)) {
		t.Fatal("manifest-size water line should trigger")
	}
}

func TestWorkerEmptyStoreNeverRuns(t *testing.T) {
	ctx := context.Background()
	s, _ := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	t.Cleanup(func() { _ = s.Close() })
	es := memory.NewEntryStore(s.DB())
	now := time.Unix(1_700_000_000, 0).UTC()
	w := NewWorker(es, s.DB(), failCall(t), Config{
		EntryCountHigh: 0, MinInterval: time.Minute, LeaseTTL: 60 * time.Second,
		ManifestBudgetChars: 1, MaxCandidatesPerPass: 20, ContentSnippetChars: 200,
		Weights: defaultWeights, Budgets: memory.DefaultBudgets(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.nowFn = func() time.Time { return now }
	// Even with last zero (time fallback) and tiny manifest budget, an empty
	// non-pinned set short-circuits to no-run.
	if w.shouldRun(ctx, now.Add(2*time.Minute)) {
		t.Fatal("empty store must never trigger a pass")
	}
}

func failCall(t *testing.T) ModelCaller {
	return func(ctx context.Context, system, user string) (string, error) {
		t.Fatal("model caller must not be invoked")
		return "", nil
	}
}

func assertExists(t *testing.T, es *memory.EntryStore, name string) {
	t.Helper()
	if _, err := es.GetByName(context.Background(), name); err != nil {
		t.Fatalf("expected %q to exist: %v", name, err)
	}
}

func assertGone(t *testing.T, es *memory.EntryStore, name string) {
	t.Helper()
	_, err := es.GetByName(context.Background(), name)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected %q gone, got err=%v", name, err)
	}
}

func mustGet(t *testing.T, es *memory.EntryStore, name string) *memory.Entry {
	t.Helper()
	e, err := es.GetByName(context.Background(), name)
	if err != nil {
		t.Fatalf("get %q: %v", name, err)
	}
	return e
}
