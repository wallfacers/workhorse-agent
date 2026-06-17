package curation

import (
	"sort"
	"strings"
	"unicode"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

// DefaultJaccardThreshold is the character-trigram Jaccard similarity at or
// above which two entries are treated as near-duplicates and unioned into the
// same cluster (design D5).
const DefaultJaccardThreshold = 0.7

// normalizeText builds the comparison text for similarity: name + trigger +
// content joined by newlines, lowercased, with all whitespace runs collapsed to
// a single space and the ends trimmed. This is the text the FTS pre-filter and
// the exact Jaccard both operate on, so they agree on what "the entry's text"
// is (design D5).
func normalizeText(e *memory.Entry) string {
	raw := e.Name + "\n" + e.Trigger + "\n" + e.Content
	var b strings.Builder
	b.Grow(len(raw))
	prevSpace := false
	for _, r := range raw {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(unicode.ToLower(r))
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

// charTrigrams returns the set of character trigrams (sliding window of 3 code
// points) over s. Strings shorter than 3 code points yield a single "trigram"
// equal to the whole string, so very short entries still compare meaningfully.
func charTrigrams(s string) map[string]struct{} {
	runes := []rune(s)
	set := make(map[string]struct{})
	if len(runes) < 3 {
		if len(runes) > 0 {
			set[string(runes)] = struct{}{}
		}
		return set
	}
	for i := 0; i+3 <= len(runes); i++ {
		set[string(runes[i:i+3])] = struct{}{}
	}
	return set
}

// jaccard returns |a∩b| / |a∪b| for two trigram sets. Two empty sets are
// defined as similarity 0 (no signal), not 1.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	// Iterate the smaller set for the intersection count.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	for t := range small {
		if _, ok := large[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Cluster groups near-duplicate entries by exact character-trigram Jaccard
// similarity ≥ threshold, unioning matching pairs (union-find). Pinned entries
// are excluded — curation never merges away a pinned memory.
//
// candidatePairs is an optional pre-filter: index pairs (into entries) that an
// upstream cheap filter (FTS5 in the runtime path, design D5) deemed worth an
// exact comparison. When candidatePairs is nil every pair is compared (O(n²),
// fine for small stores and tests); when non-nil only those pairs are compared,
// making the exact step O(pairs). Pairs referencing a pinned or out-of-range
// index are skipped.
//
// The result is the list of clusters of size ≥ 2 (singletons carry no merge
// signal and are omitted), each cluster's entries sorted by name and the
// clusters themselves ordered by their first entry's name — deterministic.
func Cluster(entries []*memory.Entry, threshold float64, candidatePairs [][2]int) [][]*memory.Entry {
	n := len(entries)
	if n < 2 {
		return nil
	}

	// Precompute trigram sets once; nil for pinned (never compared).
	grams := make([]map[string]struct{}, n)
	for i, e := range entries {
		if e.Pinned {
			continue
		}
		grams[i] = charTrigrams(normalizeText(e))
	}

	uf := newUnionFind(n)
	consider := func(i, j int) {
		if i == j || i < 0 || j < 0 || i >= n || j >= n {
			return
		}
		if grams[i] == nil || grams[j] == nil { // pinned endpoint
			return
		}
		if jaccard(grams[i], grams[j]) >= threshold {
			uf.union(i, j)
		}
	}

	if candidatePairs == nil {
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				consider(i, j)
			}
		}
	} else {
		for _, p := range candidatePairs {
			consider(p[0], p[1])
		}
	}

	// Collect members per root.
	byRoot := make(map[int][]*memory.Entry)
	for i, e := range entries {
		if grams[i] == nil { // pinned / excluded
			continue
		}
		r := uf.find(i)
		byRoot[r] = append(byRoot[r], e)
	}

	var clusters [][]*memory.Entry
	for _, members := range byRoot {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(a, b int) bool { return members[a].Name < members[b].Name })
		clusters = append(clusters, members)
	}
	sort.Slice(clusters, func(a, b int) bool { return clusters[a][0].Name < clusters[b][0].Name })
	return clusters
}

// unionFind is a minimal disjoint-set with path compression and union by size.
type unionFind struct {
	parent []int
	size   []int
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n), size: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
		uf.size[i] = 1
	}
	return uf
}

func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]] // path halving
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if u.size[ra] < u.size[rb] {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	u.size[ra] += u.size[rb]
}
