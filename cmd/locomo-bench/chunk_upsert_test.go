package main

import (
	"context"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func TestChunkUpsertAndRetrieve(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	es := memory.NewEntryStore(st.DB())
	conv := conversation{ID: 0, Sessions: []session{{
		Index: 1,
		Date:  time.Date(2023, 5, 8, 0, 0, 0, 0, time.UTC),
		Turns: []turn{{Speaker: "Caroline", Text: "I adopted a golden retriever named Max."}},
	}}}
	n, err := ingestChunks(ctx, es, conv)
	if err != nil || n != 1 {
		t.Fatalf("ingestChunks = %d, %v", n, err)
	}
	// idempotent re-run
	if n, err = ingestChunks(ctx, es, conv); err != nil || n != 1 {
		t.Fatalf("re-run ingestChunks = %d, %v", n, err)
	}
	r := memory.NewRetriever(es, memory.NewVectorStore(st.DB()), nil)
	hits, err := r.Search(ctx, "golden retriever", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("Search = %d hits, %v", len(hits), err)
	}
	if hits[0].EventDate == nil || hits[0].EventDate.Day() != 8 {
		t.Errorf("chunk EventDate not surfaced: %+v", hits[0])
	}
}
