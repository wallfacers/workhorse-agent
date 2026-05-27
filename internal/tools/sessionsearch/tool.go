package sessionsearch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// Tool implements the session_search tool.
type Tool struct {
	DB *sql.DB
}

func (Tool) Name() string { return "session_search" }
func (Tool) Description() string {
	return "Search historical messages across sessions using full-text search."
}
func (Tool) IsReadOnly() bool              { return true }
func (Tool) CanRunInParallel() bool        { return true }
func (Tool) DefaultTimeout() time.Duration { return 30 * time.Second }

func (Tool) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "The search query. Supports ASCII words and CJK characters."
			},
			"session_id": {
				"type": "string",
				"description": "The current session ID, used for scope resolution."
			},
			"scope": {
				"type": "string",
				"enum": ["session", "all"],
				"description": "Search scope: 'session' for current session only, 'all' for all non-deleted sessions. Default is the session tree (current + ancestors + descendants)."
			},
			"limit": {
				"type": "integer",
				"description": "Maximum number of results (default 10, max 50)."
			},
			"context_before": {
				"type": "integer",
				"description": "Number of messages before each hit for context (default 5, max 20)."
			},
			"context_after": {
				"type": "integer",
				"description": "Number of messages after each hit for context (default 5, max 20)."
			}
		},
		"required": ["query", "session_id"]
	}`)
}

type searchInput struct {
	Query         string `json:"query"`
	SessionID     string `json:"session_id"`
	Scope         string `json:"scope"`
	Limit         int    `json:"limit"`
	ContextBefore int    `json:"context_before"`
	ContextAfter  int    `json:"context_after"`
}

type hit struct {
	SessionID     string           `json:"session_id"`
	MessageID     string           `json:"message_id"`
	Role          string           `json:"role"`
	Snippet       string           `json:"snippet"`
	CreatedAt     int64            `json:"created_at"`
	ContextBefore []map[string]any `json:"context_before"`
	ContextAfter  []map[string]any `json:"context_after"`
}

func (t *Tool) Run(ctx context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in searchInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("invalid JSON: " + err.Error()), nil
	}
	if in.Query == "" {
		return errorResult("query must not be empty"), nil
	}
	if in.SessionID == "" {
		return errorResult("session_id must not be empty"), nil
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	ctxBefore := in.ContextBefore
	if ctxBefore <= 0 {
		ctxBefore = 5
	}
	if ctxBefore > 20 {
		ctxBefore = 20
	}

	ctxAfter := in.ContextAfter
	if ctxAfter <= 0 {
		ctxAfter = 5
	}
	if ctxAfter > 20 {
		ctxAfter = 20
	}

	sessionIDs, err := t.resolveScope(ctx, in.SessionID, in.Scope)
	if err != nil {
		return errorResult("scope resolution: " + err.Error()), nil
	}
	if len(sessionIDs) == 0 {
		return &tools.Result{Output: `{"hits":[],"truncated":false}`}, nil
	}

	matchExpr, ok := buildPlan(in.Query)
	if ok {
		return t.searchFTS(ctx, matchExpr, sessionIDs, limit, ctxBefore, ctxAfter)
	}
	return t.searchLike(ctx, in.Query, sessionIDs, limit, ctxBefore, ctxAfter)
}

func (t *Tool) searchFTS(ctx context.Context, matchExpr string, sessionIDs []string, limit, ctxBefore, ctxAfter int) (*tools.Result, error) {
	effectiveLimit := limit + 1
	placeholder, args := inList(sessionIDs)

	query := fmt.Sprintf(`
		SELECT m.id, m.session_id, m.role, m.created_at,
			snippet(messages_fts, 0, '', '', '...', 8) as snippet,
			messages_fts.rank
		FROM messages_fts
		JOIN messages m ON m.rowid = messages_fts.rowid
		WHERE messages_fts MATCH ?
			AND m.session_id IN (%s)
		ORDER BY messages_fts.rank ASC, m.created_at DESC
		LIMIT ?`, placeholder)

	args = append([]any{matchExpr}, args...)
	args = append(args, effectiveLimit)

	rows, err := t.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return errorResult("fts query: " + err.Error()), nil
	}
	defer rows.Close()

	hits, truncated := t.collectHits(ctx, rows, limit, ctxBefore, ctxAfter)
	return marshalHits(hits, truncated)
}

func (t *Tool) searchLike(ctx context.Context, query string, sessionIDs []string, limit, ctxBefore, ctxAfter int) (*tools.Result, error) {
	fragments := likeFragments(query)
	if len(fragments) == 0 {
		return &tools.Result{Output: `{"hits":[],"truncated":false}`}, nil
	}

	placeholder, sessionArgs := inList(sessionIDs)

	likeClauses := make([]string, len(fragments))
	likeArgs := make([]any, len(fragments))
	for i, f := range fragments {
		likeClauses[i] = "extract_text(m.content_json) LIKE ?"
		likeArgs[i] = "%" + f + "%"
	}

	queryStr := fmt.Sprintf(`
		SELECT m.id, m.session_id, m.role, m.created_at,
			'' as snippet,
			0 as rank
		FROM messages m
		WHERE (%s)
			AND m.session_id IN (%s)
		ORDER BY m.created_at DESC
		LIMIT ?`, strings.Join(likeClauses, " AND "), placeholder)

	args := append(likeArgs, sessionArgs...)
	args = append(args, limit+1)

	rows, err := t.DB.QueryContext(ctx, queryStr, args...)
	if err != nil {
		return errorResult("like query: " + err.Error()), nil
	}
	defer rows.Close()

	hits, truncated := t.collectHits(ctx, rows, limit, ctxBefore, ctxAfter)
	return marshalHits(hits, truncated)
}

func (t *Tool) collectHits(ctx context.Context, rows *sql.Rows, limit, ctxBefore, ctxAfter int) ([]hit, bool) {
	var hits []hit
	for rows.Next() {
		var h hit
		var rank float64
		if err := rows.Scan(&h.MessageID, &h.SessionID, &h.Role, &h.CreatedAt, &h.Snippet, &rank); err != nil {
			slog.Warn("session_search: scan row", "err", err)
			continue
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("session_search: iterate rows", "err", err)
	}

	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}

	for i := range hits {
		hits[i].ContextBefore = t.fetchContext(ctx, hits[i].SessionID, hits[i].CreatedAt, ctxBefore, "before")
		hits[i].ContextAfter = t.fetchContext(ctx, hits[i].SessionID, hits[i].CreatedAt, ctxAfter, "after")
	}

	return hits, truncated
}

func (t *Tool) fetchContext(ctx context.Context, sessionID string, createdAt int64, count int, direction string) []map[string]any {
	var query string
	if direction == "before" {
		query = `SELECT id, role, created_at FROM messages
			WHERE session_id = ? AND created_at < ?
			ORDER BY created_at DESC LIMIT ?`
	} else {
		query = `SELECT id, role, created_at FROM messages
			WHERE session_id = ? AND created_at > ?
			ORDER BY created_at ASC LIMIT ?`
	}
	args := []any{sessionID, createdAt, count}

	rows, err := t.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id, role string
		var ca int64
		if err := rows.Scan(&id, &role, &ca); err != nil {
			continue
		}
		result = append(result, map[string]any{
			"message_id": id,
			"role":       role,
			"created_at": ca,
		})
	}
	if err := rows.Err(); err != nil {
		slog.Warn("session_search: fetch context rows", "err", err)
	}

	if direction == "before" {
		for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
			result[i], result[j] = result[j], result[i]
		}
	}

	return result
}

const scopeTreeQuery = `
WITH tree AS (
	SELECT id, parent_id FROM sessions WHERE id = ? AND deleted_at IS NULL
	UNION ALL
	SELECT s.id, s.parent_id FROM sessions s
	JOIN tree t ON s.parent_id = t.id
	WHERE s.deleted_at IS NULL
),
ancestors AS (
	SELECT id, parent_id FROM sessions WHERE id = ? AND deleted_at IS NULL
	UNION ALL
	SELECT s.id, s.parent_id FROM sessions s
	JOIN ancestors a ON s.id = a.parent_id
	WHERE s.deleted_at IS NULL
)
SELECT id FROM tree
UNION
SELECT id FROM ancestors`

func (t *Tool) resolveScope(ctx context.Context, sessionID, scope string) ([]string, error) {
	switch scope {
	case "all":
		rows, err := t.DB.QueryContext(ctx,
			"SELECT id FROM sessions WHERE deleted_at IS NULL")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanIDs(rows)

	case "session":
		return []string{sessionID}, nil

	default:
		rows, err := t.DB.QueryContext(ctx, scopeTreeQuery, sessionID, sessionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanIDs(rows)
	}
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func inList(ids []string) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ","), args
}

func marshalHits(hits []hit, truncated bool) (*tools.Result, error) {
	out, _ := json.Marshal(map[string]any{
		"hits":      hits,
		"truncated": truncated,
	})
	return &tools.Result{Output: string(out)}, nil
}

func errorResult(msg string) *tools.Result {
	return &tools.Result{Output: fmt.Sprintf(`{"error":%q}`, msg), IsError: true}
}
