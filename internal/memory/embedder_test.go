package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

// fakeClient returns a deterministic vector derived from input length; it records
// how many texts it was asked to embed.
type fakeClient struct {
	model string
	mu    sync.Mutex
	calls int
	fail  bool
}

func (f *fakeClient) Model() string { return f.model }

func (f *fakeClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls += len(texts)
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return nil, errors.New("embed boom")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{float32(len(t)), 1, 0}
	}
	return out, nil
}

func newStores(t *testing.T) (*memory.EntryStore, *memory.VectorStore) {
	t.Helper()
	es, db := newEntryStore(t)
	return es, memory.NewVectorStore(db)
}

func TestEmbedder_WriteBehindPersistsVector(t *testing.T) {
	ctx := context.Background()
	es, vs := newStores(t)
	if err := es.Upsert(ctx, &memory.Entry{Name: "a", Content: "hello", CharCount: 5}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	fc := &fakeClient{model: "m1"}
	emb := memory.NewEmbedder(es, vs, fc, 8)
	emb.Enqueue("a")
	emb.Close() // drains and waits

	vecs, err := vs.LoadAllForModel(ctx, "m1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := vecs["a"]; !ok {
		t.Fatalf("expected vector for a, got %v", vecs)
	}
}

func TestEmbedder_NilWhenClientNil(t *testing.T) {
	es, vs := newStores(t)
	if emb := memory.NewEmbedder(es, vs, nil, 8); emb != nil {
		t.Fatal("expected nil embedder for nil client")
	}
	// nil embedder methods must not panic
	var nilEmb *memory.Embedder
	nilEmb.Enqueue("x")
	_ = nilEmb.Backfill(context.Background())
	nilEmb.Close()
}

func TestEmbedder_BackfillEnqueuesMissing(t *testing.T) {
	ctx := context.Background()
	es, vs := newStores(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := es.Upsert(ctx, &memory.Entry{Name: n, Content: n, CharCount: 1}); err != nil {
			t.Fatalf("upsert %s: %v", n, err)
		}
	}
	fc := &fakeClient{model: "m1"}
	emb := memory.NewEmbedder(es, vs, fc, 16)
	if err := emb.Backfill(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	emb.Close()

	vecs, _ := vs.LoadAllForModel(ctx, "m1")
	if len(vecs) != 3 {
		t.Fatalf("expected 3 vectors after backfill, got %d", len(vecs))
	}
}

func TestEmbedder_ModelChangeReembeds(t *testing.T) {
	ctx := context.Background()
	es, vs := newStores(t)
	if err := es.Upsert(ctx, &memory.Entry{Name: "a", Content: "x", CharCount: 1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Embed with model m1.
	emb1 := memory.NewEmbedder(es, vs, &fakeClient{model: "m1"}, 8)
	_ = emb1.Backfill(ctx)
	emb1.Close()

	// New model m2: NamesMissingModel should report "a" as missing for m2.
	missing, err := vs.NamesMissingModel(ctx, "m2")
	if err != nil {
		t.Fatalf("missing: %v", err)
	}
	if len(missing) != 1 || missing[0] != "a" {
		t.Fatalf("expected [a] missing for m2, got %v", missing)
	}
	emb2 := memory.NewEmbedder(es, vs, &fakeClient{model: "m2"}, 8)
	_ = emb2.Backfill(ctx)
	emb2.Close()
	if v, _ := vs.LoadAllForModel(ctx, "m2"); len(v) != 1 {
		t.Fatalf("expected re-embed under m2, got %d", len(v))
	}
}

func TestEmbedder_FailureIsNonFatal(t *testing.T) {
	ctx := context.Background()
	es, vs := newStores(t)
	if err := es.Upsert(ctx, &memory.Entry{Name: "a", Content: "x", CharCount: 1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	emb := memory.NewEmbedder(es, vs, &fakeClient{model: "m1", fail: true}, 8)
	emb.Enqueue("a")
	emb.Close() // must not panic despite embed error
	if v, _ := vs.LoadAllForModel(ctx, "m1"); len(v) != 0 {
		t.Fatalf("expected no vector on failure, got %d", len(v))
	}
}
