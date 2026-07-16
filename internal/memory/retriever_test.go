package memory_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

// seedRetrievalCorpus inserts a small labeled corpus and returns the stores.
func seedRetrievalCorpus(t *testing.T) (*memory.EntryStore, *memory.VectorStore) {
	t.Helper()
	ctx := context.Background()
	es, vs := newStores(t)
	corpus := []struct {
		name, trigger, content string
		entities               []string
	}{
		{"sweden-move", "where the user is from", "The user moved from Sweden four years ago.", []string{"Sweden"}},
		{"python-pref", "favorite language", "The user prefers Python for scripting tasks.", []string{"Python"}},
		{"coffee-habit", "morning routine", "The user drinks black coffee every morning.", []string{"coffee"}},
	}
	for _, c := range corpus {
		if err := es.Upsert(ctx, &memory.Entry{Name: c.name, Trigger: c.trigger, Content: c.content, CharCount: len([]rune(c.content))}); err != nil {
			t.Fatalf("upsert %s: %v", c.name, err)
		}
		if err := es.PutEntities(ctx, c.name, c.entities); err != nil {
			t.Fatalf("entities %s: %v", c.name, err)
		}
	}
	return es, vs
}

func TestRetriever_KeywordOnlyDegradation(t *testing.T) {
	ctx := context.Background()
	es, vs := seedRetrievalCorpus(t)
	// nil client → no semantic signal.
	r := memory.NewRetriever(es, vs, nil)
	got, err := r.Search(ctx, "Python scripting", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 || got[0].Name != "python-pref" {
		t.Fatalf("expected python-pref top, got %+v", got)
	}
}

func TestRetriever_EntitySignalContributes(t *testing.T) {
	ctx := context.Background()
	es, vs := seedRetrievalCorpus(t)
	r := memory.NewRetriever(es, vs, nil)
	// "Sweden" appears as an entity and in content; entity signal reinforces it.
	got, err := r.Search(ctx, "Sweden", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 || got[0].Name != "sweden-move" {
		t.Fatalf("expected sweden-move top, got %+v", got)
	}
}

func TestRetriever_SemanticOnlyMatch(t *testing.T) {
	ctx := context.Background()
	es, vs := seedRetrievalCorpus(t)
	// Fake client that returns a vector aligning the query with coffee-habit only.
	fc := &vectorFakeClient{
		model: "m1",
		vectors: map[string][]float32{
			"The user moved from Sweden four years ago.":   {1, 0, 0},
			"The user prefers Python for scripting tasks.": {0, 1, 0},
			"The user drinks black coffee every morning.":  {0, 0, 1},
			"caffeine intake": {0, 0, 1}, // query aligns with coffee
		},
	}
	// Embed & store vectors for the corpus.
	emb := memory.NewEmbedder(es, vs, fc, 8)
	_ = emb.Backfill(ctx)
	emb.Close()

	r := memory.NewRetriever(es, vs, fc)
	// Query shares NO keywords/entities with coffee-habit, only semantic vector.
	got, err := r.Search(ctx, "caffeine intake", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected a semantic hit, got none")
	}
	found := false
	for _, g := range got {
		if g.Name == "coffee-habit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected coffee-habit via semantic signal, got %+v", got)
	}
}

func TestRetriever_EmptyStore(t *testing.T) {
	ctx := context.Background()
	es, vs := newStores(t)
	r := memory.NewRetriever(es, vs, nil)
	got, err := r.Search(ctx, "anything", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

func TestRetriever_TimeAwareFields(t *testing.T) {
	ctx := context.Background()
	es, vs := newStores(t)
	if err := es.Upsert(ctx, &memory.Entry{Name: "dated", Content: "lived in Berlin", CharCount: 15}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	r := memory.NewRetriever(es, vs, nil)
	got, err := r.Search(ctx, "Berlin", 5)
	if err != nil || len(got) == 0 {
		t.Fatalf("search: got %v err %v", got, err)
	}
	if got[0].CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt populated on result")
	}
}

// vectorFakeClient returns a stored vector by exact input text; unknown inputs
// map to a zero vector (cosine 0).
type vectorFakeClient struct {
	model   string
	vectors map[string][]float32
}

func (f *vectorFakeClient) Model() string { return f.model }

func (f *vectorFakeClient) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := f.vectors[strings.TrimSpace(t)]; ok {
			out[i] = v
			continue
		}
		// embedText joins trigger+content; match on the content suffix.
		matched := []float32{0, 0, 0}
		for key, v := range f.vectors {
			if strings.Contains(t, key) {
				matched = v
				break
			}
		}
		out[i] = matched
	}
	return out, nil
}
