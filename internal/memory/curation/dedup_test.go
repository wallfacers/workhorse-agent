package curation

import (
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

func TestNormalizeTextCollapsesWhitespaceAndLowercases(t *testing.T) {
	e := &memory.Entry{Name: "My-Note", Trigger: "  When   X\thappens ", Content: "Do\n\nThe Thing"}
	got := normalizeText(e)
	want := "my-note when x happens do the thing"
	if got != want {
		t.Fatalf("normalizeText = %q, want %q", got, want)
	}
}

func TestCharTrigrams(t *testing.T) {
	got := charTrigrams("abcd")
	for _, want := range []string{"abc", "bcd"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("missing trigram %q in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 trigrams, got %d (%v)", len(got), got)
	}
	// Short strings (<3 runes) collapse to a single whole-string token.
	short := charTrigrams("ab")
	if len(short) != 1 {
		t.Fatalf("expected 1 token for short string, got %d", len(short))
	}
	if _, ok := short["ab"]; !ok {
		t.Fatalf("short string token missing")
	}
	// CJK counted by code point, not byte.
	cjk := charTrigrams("数据中台")
	for _, want := range []string{"数据中", "据中台"} {
		if _, ok := cjk[want]; !ok {
			t.Fatalf("missing CJK trigram %q", want)
		}
	}
}

func TestJaccard(t *testing.T) {
	a := charTrigrams("abcd") // {abc, bcd}
	b := charTrigrams("abcd")
	approx(t, jaccard(a, b), 1.0)

	// Disjoint.
	approx(t, jaccard(charTrigrams("aaaa"), charTrigrams("zzzz")), 0)

	// Two empty sets → 0.
	approx(t, jaccard(map[string]struct{}{}, map[string]struct{}{}), 0)
}

func TestClusterGroupsNearDuplicates(t *testing.T) {
	entries := []*memory.Entry{
		{Name: "a", Content: "the quick brown fox jumps over the lazy dog"},
		{Name: "b", Content: "the quick brown fox jumps over the lazy dog!"}, // ~identical
		{Name: "c", Content: "completely unrelated content about databases"},
	}
	clusters := Cluster(entries, DefaultJaccardThreshold, nil)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d: %v", len(clusters), clusters)
	}
	got := clusters[0]
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("expected cluster {a,b}, got %v", names(got))
	}
}

func TestClusterTransitiveUnion(t *testing.T) {
	// a~b and b~c but a not directly ~c; union-find still groups all three.
	base := "shared phrase one two three four five six seven"
	entries := []*memory.Entry{
		{Name: "a", Content: base + " alpha alpha alpha"},
		{Name: "b", Content: base + " alpha beta"},
		{Name: "c", Content: base + " beta beta beta"},
	}
	clusters := Cluster(entries, 0.5, nil)
	if len(clusters) != 1 || len(clusters[0]) != 3 {
		t.Fatalf("expected one cluster of 3 (transitive), got %v", clusters)
	}
}

func TestClusterExcludesPinned(t *testing.T) {
	entries := []*memory.Entry{
		{Name: "pinned", Pinned: true, Content: "the quick brown fox jumps over the lazy dog"},
		{Name: "dup", Content: "the quick brown fox jumps over the lazy dog"},
	}
	clusters := Cluster(entries, DefaultJaccardThreshold, nil)
	if len(clusters) != 0 {
		t.Fatalf("pinned entry must not be clustered, got %v", clusters)
	}
}

func TestClusterSingletonsOmitted(t *testing.T) {
	entries := []*memory.Entry{
		{Name: "a", Content: "alpha content unique"},
		{Name: "b", Content: "totally different beta"},
	}
	if clusters := Cluster(entries, DefaultJaccardThreshold, nil); len(clusters) != 0 {
		t.Fatalf("expected no clusters for dissimilar entries, got %v", clusters)
	}
}

func TestClusterHonoursCandidatePairs(t *testing.T) {
	entries := []*memory.Entry{
		{Name: "a", Content: "the quick brown fox jumps over the lazy dog"},
		{Name: "b", Content: "the quick brown fox jumps over the lazy dog"},
		{Name: "c", Content: "the quick brown fox jumps over the lazy dog"},
	}
	// Only pre-filter (a,b); c is identical but never offered as a candidate.
	clusters := Cluster(entries, DefaultJaccardThreshold, [][2]int{{0, 1}})
	if len(clusters) != 1 || len(clusters[0]) != 2 {
		t.Fatalf("expected only {a,b} from candidate pairs, got %v", clusters)
	}
	if clusters[0][0].Name != "a" || clusters[0][1].Name != "b" {
		t.Fatalf("expected {a,b}, got %v", names(clusters[0]))
	}
	// Out-of-range / pinned-endpoint pairs are skipped without panic.
	safe := Cluster(entries, DefaultJaccardThreshold, [][2]int{{0, 99}, {-1, 1}, {2, 2}})
	if len(safe) != 0 {
		t.Fatalf("expected no clusters from invalid pairs, got %v", safe)
	}
}

func TestClusterDeterministicOrdering(t *testing.T) {
	entries := []*memory.Entry{
		{Name: "zebra", Content: "shared duplicate content here now"},
		{Name: "apple", Content: "shared duplicate content here now"},
		{Name: "mango", Content: "another duplicate pair of text values"},
		{Name: "lemon", Content: "another duplicate pair of text values"},
	}
	for i := 0; i < 20; i++ {
		clusters := Cluster(entries, DefaultJaccardThreshold, nil)
		if len(clusters) != 2 {
			t.Fatalf("expected 2 clusters, got %d", len(clusters))
		}
		// Clusters ordered by first member name: apple-cluster before lemon-cluster.
		if clusters[0][0].Name != "apple" || clusters[1][0].Name != "lemon" {
			t.Fatalf("nondeterministic cluster order: %v / %v", names(clusters[0]), names(clusters[1]))
		}
	}
}

func names(es []*memory.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name
	}
	return out
}
