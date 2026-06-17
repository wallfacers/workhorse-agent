package memorytool

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/sessionsearch"
)

// MemorySearch implements the MemorySearch tool: an FTS5 MATCH over
// memory_entries_fts with the session_search CJK-trigram synthesis and LIKE
// fallback. It returns {name, trigger, snippet} rows and does NOT bump usage
// (scoring is reserved for LoadMemory). It is the recall backup when the
// manifest does not surface a needed entry.
type MemorySearch struct {
	DB *sql.DB
}

func (MemorySearch) Name() string { return "MemorySearch" }
func (MemorySearch) Description() string {
	return "Search memory entries by full-text query (supports ASCII words and CJK characters). Returns matching {name, trigger, snippet} rows. Use it to find entries not surfaced in the prompt manifest, then LoadMemory the names you want."
}
func (MemorySearch) IsReadOnly() bool              { return true }
func (MemorySearch) CanRunInParallel() bool        { return true }
func (MemorySearch) DefaultTimeout() time.Duration { return 30 * time.Second }

func (MemorySearch) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "The search query. Supports ASCII words and CJK characters."},
			"limit": {"type": "integer", "description": "Maximum number of results (default 10, max 50)."}
		},
		"required": ["query"]
	}`)
}

type searchInput struct {
	Query string `json:"query"`
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

	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	matchExpr, ok := sessionsearch.BuildPlan(in.Query)
	if ok {
		return m.searchFTS(ctx, matchExpr, limit)
	}
	return m.searchLike(ctx, in.Query, limit)
}

func (m *MemorySearch) searchFTS(ctx context.Context, matchExpr string, limit int) (*tools.Result, error) {
	// snippet column index 2 = content (columns are name=0, trigger=1, content=2).
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
		return marshalMatches(nil, false)
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
	return marshalMatches(out, truncated)
}

func marshalMatches(matches []match, truncated bool) (*tools.Result, error) {
	if matches == nil {
		matches = []match{}
	}
	out, _ := json.Marshal(map[string]any{
		"matches":   matches,
		"truncated": truncated,
	})
	return &tools.Result{Output: string(out)}, nil
}
