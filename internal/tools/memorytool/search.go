package memorytool

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/sessionsearch"
)

// snippetRunes bounds the content snippet length (code points).
const snippetRunes = 160

// MemorySearch implements the MemorySearch tool. When a Retriever is wired it
// runs three-signal hybrid retrieval (semantic + BM25 + entity, RRF-fused) and
// annotates each hit with event/recorded dates for time-aware disambiguation
// (memory-hybrid-retrieval-locomo). Without a Retriever it degrades to the
// legacy FTS5 MATCH + LIKE fallback over memory_entries_fts. It never bumps
// usage (scoring is reserved for LoadMemory).
type MemorySearch struct {
	DB        *sql.DB
	Retriever *memory.Retriever
}

func (MemorySearch) Name() string { return "MemorySearch" }
func (MemorySearch) Description() string {
	return "Search memory entries by query (supports ASCII words and CJK characters). Returns matching {name, trigger, snippet} rows, each annotated with [event: <date>] and [recorded: <date>] when known. Use it to find entries not surfaced in the prompt manifest, then LoadMemory the names you want."
}
func (MemorySearch) IsReadOnly() bool              { return true }
func (MemorySearch) CanRunInParallel() bool        { return true }
func (MemorySearch) DefaultTimeout() time.Duration { return 30 * time.Second }

func (MemorySearch) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "The search query. Supports ASCII words and CJK characters."},
			"top_k": {"type": "integer", "description": "Maximum number of results (default 8, max 50)."},
			"limit": {"type": "integer", "description": "Deprecated alias for top_k."}
		},
		"required": ["query"]
	}`)
}

type searchInput struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
	Limit int    `json:"limit"`
}

type match struct {
	Name    string `json:"name"`
	Trigger string `json:"trigger"`
	Snippet string `json:"snippet"`
}

func (m *MemorySearch) Run(ctx context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in searchInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ErrorResultJSON("invalid JSON: " + err.Error()), nil
	}
	if in.Query == "" {
		return tools.ErrorResultJSON("query must not be empty"), nil
	}

	// top_k is the primary knob; limit is the deprecated alias.
	k := in.TopK
	if k <= 0 {
		k = in.Limit
	}
	capped := false
	if k <= 0 {
		k = 8
	}
	if k > 50 {
		k = 50
		capped = true
	}

	if m.Retriever != nil {
		return m.searchHybrid(ctx, in.Query, k, capped)
	}
	return m.searchLegacy(ctx, in.Query, k)
}

func (m *MemorySearch) searchHybrid(ctx context.Context, query string, k int, capped bool) (*tools.Result, error) {
	results, err := m.Retriever.Search(ctx, query, k)
	if err != nil {
		return tools.ErrorResultJSON("search: " + err.Error()), nil
	}
	matches := make([]match, 0, len(results))
	for _, r := range results {
		matches = append(matches, match{
			Name:    r.Name,
			Trigger: r.Trigger,
			Snippet: renderSnippet(r.EventDate, r.CreatedAt, r.Content),
		})
	}
	return marshalMatches(matches, false, capped)
}

// renderSnippet prefixes time-aware markers and truncates the content body.
func renderSnippet(eventDate *time.Time, createdAt time.Time, content string) string {
	var b strings.Builder
	if eventDate != nil && !eventDate.IsZero() {
		fmt.Fprintf(&b, "[event: %s] ", eventDate.Format("2006-01-02"))
	}
	if !createdAt.IsZero() {
		fmt.Fprintf(&b, "[recorded: %s] ", createdAt.Format("2006-01-02"))
	}
	b.WriteString(truncateRunes(content, snippetRunes))
	return b.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// ---- legacy DB-only FTS path (Retriever unset) ----

func (m *MemorySearch) searchLegacy(ctx context.Context, query string, limit int) (*tools.Result, error) {
	matchExpr, ok := sessionsearch.BuildPlan(query)
	if ok {
		return m.searchFTS(ctx, matchExpr, limit)
	}
	return m.searchLike(ctx, query, limit)
}

func (m *MemorySearch) searchFTS(ctx context.Context, matchExpr string, limit int) (*tools.Result, error) {
	query := `
		SELECT e.name, e.trigger,
			snippet(memory_entries_fts, 2, '', '', '...', 8) AS snippet
		FROM memory_entries_fts
		JOIN memory_entries e ON e.rowid = memory_entries_fts.rowid
		WHERE memory_entries_fts MATCH ?
		ORDER BY memory_entries_fts.rank ASC
		LIMIT ?`
	rows, err := m.DB.QueryContext(ctx, query, matchExpr, limit+1)
	if err != nil {
		return tools.ErrorResultJSON("fts query: " + err.Error()), nil
	}
	defer rows.Close() //nolint:errcheck
	return collectMatches(rows, limit)
}

func (m *MemorySearch) searchLike(ctx context.Context, query string, limit int) (*tools.Result, error) {
	fragments := sessionsearch.LikeFragments(query)
	if len(fragments) == 0 {
		return marshalMatches(nil, false, false)
	}

	clauses := make([]string, len(fragments))
	args := make([]any, 0, len(fragments)+1)
	for i, f := range fragments {
		clauses[i] = "(e.name LIKE ? OR e.trigger LIKE ? OR e.content LIKE ?)"
		like := "%" + f + "%"
		args = append(args, like, like, like)
	}
	// #nosec G201 -- clauses are compile-time-constant LIKE fragments; every user
	// value is bound through a ? placeholder in args, so there is no injection
	// surface despite the Sprintf.
	query2 := fmt.Sprintf(`
		SELECT e.name, e.trigger, '' AS snippet
		FROM memory_entries e
		WHERE %s
		ORDER BY e.updated_at DESC
		LIMIT ?`, strings.Join(clauses, " AND "))
	args = append(args, limit+1)

	rows, err := m.DB.QueryContext(ctx, query2, args...)
	if err != nil {
		return tools.ErrorResultJSON("like query: " + err.Error()), nil
	}
	defer rows.Close() //nolint:errcheck
	return collectMatches(rows, limit)
}

func collectMatches(rows *sql.Rows, limit int) (*tools.Result, error) {
	var out []match
	for rows.Next() {
		var mm match
		if err := rows.Scan(&mm.Name, &mm.Trigger, &mm.Snippet); err != nil {
			slog.Warn("MemorySearch: scan row", "err", err)
			continue
		}
		out = append(out, mm)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("MemorySearch: iterate rows", "err", err)
	}

	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
	}
	return marshalMatches(out, truncated, false)
}

func marshalMatches(matches []match, truncated, capped bool) (*tools.Result, error) {
	if matches == nil {
		matches = []match{}
	}
	out, _ := json.Marshal(map[string]any{
		"matches":      matches,
		"truncated":    truncated,
		"top_k_capped": capped,
	})
	return &tools.Result{Output: string(out)}, nil
}
