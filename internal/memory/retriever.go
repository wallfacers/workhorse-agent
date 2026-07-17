package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/embedding"
	"github.com/wallfacers/workhorse-agent/internal/tools/sessionsearch"
)

// rrfK is the Reciprocal Rank Fusion constant. 60 is the value from the original
// RRF paper and the de-facto default for hybrid search; using it verbatim keeps
// the fusion tuning-free (design D4).
const rrfK = 60.0

// candidateMultiplier bounds how many BM25 candidates we pull relative to the
// requested k, so fusion sees enough of the keyword tail without scanning the
// whole table.
const candidateMultiplier = 10

// minCandidatePool floors the BM25 candidate pull for small k.
const minCandidatePool = 100

// rerankPool caps how many fused candidates are handed to the cross-encoder.
const rerankPool = 50

// expansionSeeds is how many top fused hits seed the 1-hop entity expansion.
const expansionSeeds = 10

// expansionLimit caps how many entity-neighbor entries join the rerank pool.
const expansionLimit = 25

// Retriever implements three-signal hybrid retrieval with RRF fusion
// (memory-hybrid-retrieval-locomo). The three signals are:
//
//  1. semantic — cosine similarity of the embedded query to stored vectors
//     (skipped when the embedding client is nil);
//  2. keyword  — FTS5 BM25 MATCH ranking with the shared CJK-trigram synthesis
//     and LIKE fallback (identical to MemorySearch's legacy path);
//  3. entity   — exact-match count of normalized query tokens against the
//     memory_entities index.
//
// Absent signals simply drop out of the fused sum, so the retriever degrades
// gracefully: no client → keyword+entity; no entities → keyword only, which is
// behaviorally identical to the pre-feature FTS path.
type Retriever struct {
	entries  *EntryStore
	vectors  *VectorStore
	client   embedding.Client   // may be nil
	reranker embedding.Reranker // may be nil
}

// NewRetriever builds a Retriever. A nil client disables the semantic signal.
func NewRetriever(entries *EntryStore, vectors *VectorStore, client embedding.Client) *Retriever {
	return &Retriever{entries: entries, vectors: vectors, client: client}
}

// WithReranker enables the cross-encoder rerank stage (and, with it, 1-hop
// entity expansion of the candidate pool). A nil reranker is a no-op, keeping
// the pure-RRF path byte-identical.
func (r *Retriever) WithReranker(rr embedding.Reranker) *Retriever {
	if r != nil {
		r.reranker = rr
	}
	return r
}

// Result is one fused retrieval hit. Content carries the full entry body; the
// tool layer derives a snippet. EventDate/CreatedAt drive time-aware rendering.
type Result struct {
	Name      string
	Trigger   string
	Content   string
	EventDate *time.Time
	CreatedAt time.Time
	Score     float64
}

// Search returns the top-k entries for query, fusing whatever signals are
// available. k <= 0 defaults to 8. It never errors on a single signal's failure:
// a degraded signal is skipped, not fatal.
func (r *Retriever) Search(ctx context.Context, query string, k int) ([]Result, error) {
	if r == nil {
		return nil, nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if k <= 0 {
		k = 8
	}
	pool := k * candidateMultiplier
	if pool < minCandidatePool {
		pool = minCandidatePool
	}

	// Signal 1: keyword (BM25 / LIKE). This also bounds the candidate universe.
	bm25 := r.keywordRanks(ctx, query, pool)
	// Signal 2: semantic (optional).
	vec := r.vectorRanks(ctx, query, pool)
	// Signal 3: entity.
	ent := r.entityRanks(ctx, query)

	fused := fuseRRF(bm25, vec, ent)
	if len(fused) == 0 {
		return nil, nil
	}
	if r.reranker != nil {
		fused = r.rerank(ctx, query, fused, k)
	}
	if len(fused) > k {
		fused = fused[:k]
	}

	out := make([]Result, 0, len(fused))
	for _, s := range fused {
		e, err := r.entries.GetByName(ctx, s.Key)
		if err != nil {
			continue // entry removed between ranking and load; skip
		}
		out = append(out, Result{
			Name:      e.Name,
			Trigger:   e.Trigger,
			Content:   e.Content,
			EventDate: e.EventDate,
			CreatedAt: e.CreatedAt,
			Score:     s.Score,
		})
	}
	return out, nil
}

// rerank widens the fused list with 1-hop entity neighbors, scores every
// candidate's content against the query with the cross-encoder, and returns
// the re-ordered list. Any failure degrades to the fused input (fail-safe,
// same philosophy as the per-signal degradation above).
func (r *Retriever) rerank(ctx context.Context, query string, fused []embedding.Scored, k int) []embedding.Scored {
	pool := rerankPool
	if k > pool {
		pool = k
	}
	if len(fused) > pool {
		fused = fused[:pool]
	}
	candidates := make([]string, 0, len(fused)+expansionLimit)
	seen := make(map[string]struct{}, len(fused)+expansionLimit)
	for _, s := range fused {
		candidates = append(candidates, s.Key)
		seen[s.Key] = struct{}{}
	}
	for _, name := range r.entityNeighbors(ctx, fused) {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		candidates = append(candidates, name)
	}

	docs := make([]string, 0, len(candidates))
	names := make([]string, 0, len(candidates))
	for _, name := range candidates {
		e, err := r.entries.GetByName(ctx, name)
		if err != nil {
			continue
		}
		docs = append(docs, e.Content)
		names = append(names, name)
	}
	if len(docs) == 0 {
		return fused
	}
	ranked, err := r.reranker.Rerank(ctx, query, docs, k)
	if err != nil || len(ranked) == 0 {
		return fused
	}
	out := make([]embedding.Scored, 0, len(ranked))
	for _, rd := range ranked {
		if rd.Index < 0 || rd.Index >= len(names) {
			return fused
		}
		out = append(out, embedding.Scored{Key: names[rd.Index], Score: rd.Score})
	}
	return out
}

// entityNeighbors returns entry names sharing at least one entity with the top
// fused seeds, ordered by shared-entity count descending (name ascending on
// ties), capped at expansionLimit. Failures return nil — expansion is a bonus,
// never a dependency.
func (r *Retriever) entityNeighbors(ctx context.Context, fused []embedding.Scored) []string {
	seeds := make([]string, 0, expansionSeeds)
	for i := 0; i < len(fused) && i < expansionSeeds; i++ {
		seeds = append(seeds, fused[i].Key)
	}
	tokens, err := r.entries.EntitiesOf(ctx, seeds)
	if err != nil || len(tokens) == 0 {
		return nil
	}
	counts, err := r.entries.EntityMatchCounts(ctx, tokens)
	if err != nil || len(counts) == 0 {
		return nil
	}
	type nc struct {
		name  string
		count int
	}
	ordered := make([]nc, 0, len(counts))
	for name, c := range counts {
		ordered = append(ordered, nc{name, c})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].name < ordered[j].name
	})
	if len(ordered) > expansionLimit {
		ordered = ordered[:expansionLimit]
	}
	names := make([]string, len(ordered))
	for i, o := range ordered {
		names[i] = o.name
	}
	return names
}

// keywordRanks returns a name→rank map (1-based) from the FTS5 BM25 ordering,
// falling back to the LIKE path exactly as MemorySearch does.
func (r *Retriever) keywordRanks(ctx context.Context, query string, limit int) map[string]int {
	var names []string
	if matchExpr, ok := sessionsearch.BuildPlan(query); ok {
		names = r.ftsNames(ctx, matchExpr, limit)
	} else {
		names = r.likeNames(ctx, query, limit)
	}
	return ranksFromOrder(names)
}

func (r *Retriever) ftsNames(ctx context.Context, matchExpr string, limit int) []string {
	rows, err := r.entries.db.QueryContext(ctx, `
		SELECT e.name
		FROM memory_entries_fts
		JOIN memory_entries e ON e.rowid = memory_entries_fts.rowid
		WHERE memory_entries_fts MATCH ?
		ORDER BY memory_entries_fts.rank ASC
		LIMIT ?`, matchExpr, limit)
	if err != nil {
		return nil
	}
	defer rows.Close() //nolint:errcheck
	return scanNames(rows)
}

func (r *Retriever) likeNames(ctx context.Context, query string, limit int) []string {
	fragments := sessionsearch.LikeFragments(query)
	if len(fragments) == 0 {
		return nil
	}
	clauses := make([]string, len(fragments))
	args := make([]any, 0, len(fragments)+1)
	for i, f := range fragments {
		clauses[i] = "(e.name LIKE ? OR e.trigger LIKE ? OR e.content LIKE ?)"
		like := "%" + f + "%"
		args = append(args, like, like, like)
	}
	// #nosec G201 -- clauses are constant LIKE fragments; user values are all
	// bound through ? placeholders (mirrors MemorySearch.searchLike).
	q := fmt.Sprintf(`
		SELECT e.name FROM memory_entries e
		WHERE %s ORDER BY e.updated_at DESC LIMIT ?`, strings.Join(clauses, " AND "))
	args = append(args, limit)
	rows, err := r.entries.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close() //nolint:errcheck
	return scanNames(rows)
}

// vectorRanks embeds the query and ranks stored vectors by cosine. Returns nil
// (no signal) when the client is unset or any step fails.
func (r *Retriever) vectorRanks(ctx context.Context, query string, limit int) map[string]int {
	if r.client == nil {
		return nil
	}
	vecs, err := r.client.Embed(ctx, []string{query})
	if err != nil || len(vecs) != 1 {
		return nil
	}
	candidates, err := r.vectors.LoadAllForModel(ctx, r.client.Model())
	if err != nil || len(candidates) == 0 {
		return nil
	}
	scored := embedding.TopKCosine(vecs[0], candidates, limit)
	names := make([]string, len(scored))
	for i, s := range scored {
		names[i] = s.Key
	}
	return ranksFromOrder(names)
}

// entityRanks orders entries by how many distinct query entity tokens they
// match, then maps to ranks.
func (r *Retriever) entityRanks(ctx context.Context, query string) map[string]int {
	counts, err := r.entries.EntityMatchCounts(ctx, EntityQueryTokens(query))
	if err != nil || len(counts) == 0 {
		return nil
	}
	type nc struct {
		name  string
		count int
	}
	ordered := make([]nc, 0, len(counts))
	for name, c := range counts {
		ordered = append(ordered, nc{name, c})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].name < ordered[j].name
	})
	names := make([]string, len(ordered))
	for i, o := range ordered {
		names[i] = o.name
	}
	return ranksFromOrder(names)
}

// ranksFromOrder converts an ordered name slice into a 1-based rank map.
func ranksFromOrder(names []string) map[string]int {
	m := make(map[string]int, len(names))
	for i, n := range names {
		if _, seen := m[n]; !seen {
			m[n] = i + 1
		}
	}
	return m
}

// fuseRRF combines rank maps with Reciprocal Rank Fusion and returns entries
// sorted by fused score descending, name ascending.
func fuseRRF(signals ...map[string]int) []embedding.Scored {
	acc := make(map[string]float64)
	for _, sig := range signals {
		for name, rank := range sig {
			acc[name] += 1.0 / (rrfK + float64(rank))
		}
	}
	out := make([]embedding.Scored, 0, len(acc))
	for name, score := range acc {
		out = append(out, embedding.Scored{Key: name, Score: score})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func scanNames(rows *sql.Rows) []string {
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			continue
		}
		names = append(names, n)
	}
	return names
}
