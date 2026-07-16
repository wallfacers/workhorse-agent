package memory_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func newEntryStore(t *testing.T) (*memory.EntryStore, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return memory.NewEntryStore(s.DB()), s.DB()
}

func ftsCount(t *testing.T, db *sql.DB, match string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memory_entries_fts WHERE memory_entries_fts MATCH ?`, match).Scan(&n); err != nil {
		t.Fatalf("fts count match %q: %v", match, err)
	}
	return n
}

func TestUpsertInsertThenConflictUpdate(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()

	e := &memory.Entry{Name: "alpha", Trigger: "t1", Content: "hello world", Category: "user", CharCount: 11}
	if err := es.Upsert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if e.ID == "" {
		t.Fatal("expected ID to be assigned")
	}
	if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
		t.Fatal("expected created_at/updated_at to be set")
	}

	got, err := es.GetByName(ctx, "alpha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	origCreated := got.CreatedAt
	if got.LastUsedAt != nil {
		t.Fatalf("expected nil LastUsedAt on fresh entry, got %v", got.LastUsedAt)
	}
	if got.Durability != "volatile" {
		t.Fatalf("expected default durability volatile, got %q", got.Durability)
	}

	// Ensure updated_at advances on conflict update.
	time.Sleep(2 * time.Millisecond)
	upd := &memory.Entry{Name: "alpha", Trigger: "t2", Content: "goodbye world", Category: "project", CharCount: 13}
	if err := es.Upsert(ctx, upd); err != nil {
		t.Fatalf("conflict upsert: %v", err)
	}

	if c, _ := es.Count(ctx); c != 1 {
		t.Fatalf("expected 1 row after conflict upsert, got %d", c)
	}
	got2, err := es.GetByName(ctx, "alpha")
	if err != nil {
		t.Fatalf("get after upsert: %v", err)
	}
	if got2.Trigger != "t2" || got2.Content != "goodbye world" || got2.Category != "project" {
		t.Fatalf("conflict update did not replace fields: %+v", got2)
	}
	if !got2.CreatedAt.Equal(origCreated) {
		t.Fatalf("created_at should be preserved: was %v now %v", origCreated, got2.CreatedAt)
	}
	if !got2.UpdatedAt.After(origCreated) {
		t.Fatalf("updated_at should advance past created_at: created %v updated %v", origCreated, got2.UpdatedAt)
	}
}

func TestGetByNameNotFound(t *testing.T) {
	es, _ := newEntryStore(t)
	_, err := es.GetByName(context.Background(), "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListOrdering(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	for _, n := range []string{"charlie", "alpha", "bravo"} {
		if err := es.Upsert(ctx, &memory.Entry{Name: n, Content: n}); err != nil {
			t.Fatalf("upsert %q: %v", n, err)
		}
	}
	list, err := es.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	if len(list) != len(want) {
		t.Fatalf("expected %d entries, got %d", len(want), len(list))
	}
	for i, e := range list {
		if e.Name != want[i] {
			t.Fatalf("list[%d] = %q, want %q", i, e.Name, want[i])
		}
	}
}

func TestDelete(t *testing.T) {
	es, db := newEntryStore(t)
	ctx := context.Background()

	if err := es.Upsert(ctx, &memory.Entry{Name: "gone", Content: "ephemeral content here"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := es.Delete(ctx, "gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := es.GetByName(ctx, "gone"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if n := ftsCount(t, db, "ephemeral"); n != 0 {
		t.Fatalf("expected FTS row removed after delete, got %d", n)
	}
	// Deleting a missing entry returns ErrNotFound.
	if err := es.Delete(ctx, "never"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound deleting missing, got %v", err)
	}
}

func TestMerge(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()

	if err := es.Upsert(ctx, &memory.Entry{Name: "a", Content: "from a"}); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if err := es.Upsert(ctx, &memory.Entry{Name: "b", Content: "from b"}); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	into := &memory.Entry{Name: "into", Trigger: "merged", Content: "from a + from b"}
	if err := es.Merge(ctx, []string{"a", "b"}, into); err != nil {
		t.Fatalf("merge: %v", err)
	}

	if _, err := es.GetByName(ctx, "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected a gone, got %v", err)
	}
	if _, err := es.GetByName(ctx, "b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected b gone, got %v", err)
	}
	got, err := es.GetByName(ctx, "into")
	if err != nil {
		t.Fatalf("expected into present: %v", err)
	}
	if got.Content != "from a + from b" {
		t.Fatalf("unexpected merged content: %q", got.Content)
	}
}

func TestMergeIntoNameInSources(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()

	if err := es.Upsert(ctx, &memory.Entry{Name: "a", Content: "old a"}); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if err := es.Upsert(ctx, &memory.Entry{Name: "b", Content: "old b"}); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	// into.Name == "a" which is also in names: must survive with new content.
	into := &memory.Entry{Name: "a", Content: "merged into a"}
	if err := es.Merge(ctx, []string{"a", "b"}, into); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, err := es.GetByName(ctx, "a")
	if err != nil {
		t.Fatalf("expected a to survive merge: %v", err)
	}
	if got.Content != "merged into a" {
		t.Fatalf("expected merged content, got %q", got.Content)
	}
	if _, err := es.GetByName(ctx, "b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected b gone, got %v", err)
	}
}

func TestBumpUsage(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()

	if err := es.Upsert(ctx, &memory.Entry{Name: "hot", Content: "popular"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now().UTC()
	if err := es.BumpUsage(ctx, "hot", now); err != nil {
		t.Fatalf("bump: %v", err)
	}
	got, err := es.GetByName(ctx, "hot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.HitCount != 1 {
		t.Fatalf("expected hit_count 1, got %d", got.HitCount)
	}
	if got.LastUsedAt == nil {
		t.Fatal("expected last_used_at set after bump")
	}
	// Bumping a missing name is best-effort: no error.
	if err := es.BumpUsage(ctx, "nonexistent", now); err != nil {
		t.Fatalf("expected no error bumping missing name, got %v", err)
	}
}

func TestFTSSyncOnUpsert(t *testing.T) {
	es, db := newEntryStore(t)
	ctx := context.Background()

	if err := es.Upsert(ctx, &memory.Entry{
		Name:    "doc",
		Trigger: "when searching",
		Content: "the quick brown fox jumps",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if n := ftsCount(t, db, "brown"); n != 1 {
		t.Fatalf("expected FTS match for 'brown' after upsert, got %d", n)
	}

	// Conflict update should keep FTS in sync via the AFTER UPDATE trigger.
	if err := es.Upsert(ctx, &memory.Entry{
		Name:    "doc",
		Trigger: "when searching",
		Content: "a totally different sentence",
	}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if n := ftsCount(t, db, "brown"); n != 0 {
		t.Fatalf("expected no FTS match for stale 'brown', got %d", n)
	}
	if n := ftsCount(t, db, "different"); n != 1 {
		t.Fatalf("expected FTS match for 'different', got %d", n)
	}
}

func TestCountAndCountNonPinned(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()

	if err := es.Upsert(ctx, &memory.Entry{Name: "p1", Content: "x", Pinned: true}); err != nil {
		t.Fatalf("upsert p1: %v", err)
	}
	if err := es.Upsert(ctx, &memory.Entry{Name: "n1", Content: "y"}); err != nil {
		t.Fatalf("upsert n1: %v", err)
	}
	if err := es.Upsert(ctx, &memory.Entry{Name: "n2", Content: "z"}); err != nil {
		t.Fatalf("upsert n2: %v", err)
	}

	if c, err := es.Count(ctx); err != nil || c != 3 {
		t.Fatalf("Count = %d, err %v; want 3", c, err)
	}
	if c, err := es.CountNonPinned(ctx); err != nil || c != 2 {
		t.Fatalf("CountNonPinned = %d, err %v; want 2", c, err)
	}
}

func TestEventDateAndFactSourceRoundTrip(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	ev := time.UnixMicro(1_600_000_000_000_000).UTC()
	e := &memory.Entry{Name: "moved", Content: "moved from sweden", CharCount: 17, EventDate: &ev, FactSource: "extraction"}
	if err := es.Upsert(ctx, e); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := es.GetByName(ctx, "moved")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FactSource != "extraction" {
		t.Fatalf("fact_source: got %q", got.FactSource)
	}
	if got.EventDate == nil || !got.EventDate.Equal(ev) {
		t.Fatalf("event_date: got %v want %v", got.EventDate, ev)
	}
}

func TestDeleteCascadesDerived(t *testing.T) {
	es, db := newEntryStore(t)
	ctx := context.Background()
	if err := es.Upsert(ctx, &memory.Entry{Name: "alpha", Content: "hi", CharCount: 2}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := es.PutEntities(ctx, "alpha", []string{"Sweden", "Quicksort"}); err != nil {
		t.Fatalf("put entities: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO memory_embeddings(entry_name, model, dims, vec, updated_at) VALUES ('alpha','m',1,x'00',0)`); err != nil {
		t.Fatalf("insert embedding: %v", err)
	}
	if err := es.Delete(ctx, "alpha"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, tbl := range []string{"memory_embeddings", "memory_entities"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tbl+` WHERE entry_name='alpha'`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("%s not cascaded: %d rows remain", tbl, n)
		}
	}
}

func TestEntityMatchCounts(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	for _, e := range []*memory.Entry{
		{Name: "a", Content: "x", CharCount: 1},
		{Name: "b", Content: "y", CharCount: 1},
	} {
		if err := es.Upsert(ctx, e); err != nil {
			t.Fatalf("upsert %s: %v", e.Name, err)
		}
	}
	if err := es.PutEntities(ctx, "a", []string{"Sweden", "Python"}); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if err := es.PutEntities(ctx, "b", []string{"Python"}); err != nil {
		t.Fatalf("put b: %v", err)
	}
	counts, err := es.EntityMatchCounts(ctx, memory.EntityQueryTokens("Tell me about python and sweden"))
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if counts["a"] != 2 {
		t.Fatalf("entry a: got %d want 2", counts["a"])
	}
	if counts["b"] != 1 {
		t.Fatalf("entry b: got %d want 1", counts["b"])
	}
}

func TestPutEntitiesReplaces(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	if err := es.Upsert(ctx, &memory.Entry{Name: "a", Content: "x", CharCount: 1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := es.PutEntities(ctx, "a", []string{"Sweden"}); err != nil {
		t.Fatalf("put1: %v", err)
	}
	if err := es.PutEntities(ctx, "a", []string{"Norway"}); err != nil {
		t.Fatalf("put2: %v", err)
	}
	counts, err := es.EntityMatchCounts(ctx, []string{"sweden", "norway"})
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if counts["a"] != 1 {
		t.Fatalf("expected only norway to match after replace, got %d", counts["a"])
	}
}
