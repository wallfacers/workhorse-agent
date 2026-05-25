package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
)

// ---- time helpers ----

// SQLite stores integers, so we serialise time.Time as Unix microseconds. The
// helpers below keep the conversion consistent and let us treat the zero time
// as "absent" by mapping it to 0.
func toMicros(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}

func fromMicros(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.UnixMicro(n).UTC()
}

// nullableMicros returns sql.NullInt64 from a *time.Time, mapping nil to NULL.
func nullableMicros(t *time.Time) sql.NullInt64 {
	if t == nil || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: toMicros(*t), Valid: true}
}

func fromNullableMicros(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMicros(n.Int64)
	return &t
}

// ---- Session CRUD ----

func (s *Store) CreateSession(ctx context.Context, sess *store.Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, parent_id, state, workdir, env_json,
			agent_type, model, ephemeral, created_at, updated_at, deleted_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.ParentID, string(sess.State), sess.Workdir, sess.EnvJSON,
		sess.AgentType, sess.Model, boolToInt(sess.Ephemeral),
		toMicros(sess.CreatedAt), toMicros(sess.UpdatedAt), nullableMicros(sess.DeletedAt))
	if err != nil {
		return fmt.Errorf("sqlite: CreateSession: %w", err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, id string) (*store.Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, parent_id, state, workdir, env_json, agent_type, model,
			ephemeral, created_at, updated_at, deleted_at
		 FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

func (s *Store) ListSessions(ctx context.Context, includeDeleted bool) ([]*store.Session, error) {
	q := `SELECT id, parent_id, state, workdir, env_json, agent_type, model,
			ephemeral, created_at, updated_at, deleted_at
		  FROM sessions`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListSessions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) UpdateSession(ctx context.Context, sess *store.Session) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET parent_id=?, state=?, workdir=?, env_json=?,
			agent_type=?, model=?, ephemeral=?, updated_at=?, deleted_at=?
		 WHERE id=?`,
		sess.ParentID, string(sess.State), sess.Workdir, sess.EnvJSON,
		sess.AgentType, sess.Model, boolToInt(sess.Ephemeral),
		toMicros(sess.UpdatedAt), nullableMicros(sess.DeletedAt), sess.ID)
	if err != nil {
		return fmt.Errorf("sqlite: UpdateSession: %w", err)
	}
	return ensureRowsAffected(res, store.ErrNotFound)
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	now := toMicros(time.Now().UTC())
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET deleted_at = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	if err != nil {
		return fmt.Errorf("sqlite: DeleteSession: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either no row, or already deleted. We treat "already deleted" as
		// idempotent success; only a truly missing row is an error.
		_, err := s.GetSession(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrNotFound
		}
	}
	return nil
}

func (s *Store) CountActiveSessions(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE deleted_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: CountActiveSessions: %w", err)
	}
	return n, nil
}

// scanner is the smallest interface that both *sql.Row and *sql.Rows satisfy
// for our purposes — lets us share the column ordering between Get and List.
type scanner interface {
	Scan(dest ...any) error
}

func scanSession(sc scanner) (*store.Session, error) {
	var sess store.Session
	var state string
	var ephemeral int
	var createdAt, updatedAt int64
	var deletedAt sql.NullInt64
	if err := sc.Scan(&sess.ID, &sess.ParentID, &state, &sess.Workdir,
		&sess.EnvJSON, &sess.AgentType, &sess.Model, &ephemeral,
		&createdAt, &updatedAt, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: scan session: %w", err)
	}
	sess.State = store.SessionState(state)
	sess.Ephemeral = ephemeral != 0
	sess.CreatedAt = fromMicros(createdAt)
	sess.UpdatedAt = fromMicros(updatedAt)
	sess.DeletedAt = fromNullableMicros(deletedAt)
	return &sess, nil
}

// ---- Message CRUD ----

func (s *Store) AppendMessage(ctx context.Context, m *store.Message) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(id, session_id, role, content_json, token_count, created_at)
		 VALUES (?,?,?,?,?,?)`,
		m.ID, m.SessionID, m.Role, m.ContentJSON, m.TokenCount, toMicros(m.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: AppendMessage: %w", err)
	}
	return nil
}

func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]*store.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, role, content_json, token_count, created_at
		 FROM messages WHERE session_id = ? ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListMessages: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.Message
	for rows.Next() {
		var m store.Message
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.ContentJSON,
			&m.TokenCount, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan message: %w", err)
		}
		m.CreatedAt = fromMicros(createdAt)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ---- Event append + incremental query ----

func (s *Store) AppendEvent(ctx context.Context, e *store.Event) (int64, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events(session_id, type, payload_json, created_at)
		 VALUES (?,?,?,?)`,
		e.SessionID, e.Type, e.PayloadJSON, toMicros(e.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("sqlite: AppendEvent: %w", err)
	}
	idx, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sqlite: AppendEvent last id: %w", err)
	}
	e.Idx = idx
	return idx, nil
}

func (s *Store) EventsAfter(ctx context.Context, sessionID string, lastIdx, snapshot int64) ([]*store.Event, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if snapshot > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT idx, session_id, type, payload_json, created_at
			 FROM events
			 WHERE session_id = ? AND idx > ? AND idx <= ?
			 ORDER BY idx`, sessionID, lastIdx, snapshot)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT idx, session_id, type, payload_json, created_at
			 FROM events
			 WHERE session_id = ? AND idx > ?
			 ORDER BY idx`, sessionID, lastIdx)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: EventsAfter: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.Event
	for rows.Next() {
		var e store.Event
		var createdAt int64
		if err := rows.Scan(&e.Idx, &e.SessionID, &e.Type, &e.PayloadJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}
		e.CreatedAt = fromMicros(createdAt)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *Store) MaxEventIdx(ctx context.Context, sessionID string) (int64, error) {
	var max sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(idx) FROM events WHERE session_id = ?`, sessionID).Scan(&max); err != nil {
		return 0, fmt.Errorf("sqlite: MaxEventIdx: %w", err)
	}
	if !max.Valid {
		return 0, nil
	}
	return max.Int64, nil
}

// ---- ToolCall CRUD ----

func (s *Store) AppendToolCall(ctx context.Context, t *store.ToolCall) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_calls(id, session_id, message_id, tool, input_json,
			output_json, is_error, started_at, finished_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		t.ID, t.SessionID, t.MessageID, t.Tool, t.InputJSON, t.OutputJSON,
		boolToInt(t.IsError), toMicros(t.StartedAt), nullableMicros(t.FinishedAt))
	if err != nil {
		return fmt.Errorf("sqlite: AppendToolCall: %w", err)
	}
	return nil
}

func (s *Store) UpdateToolCall(ctx context.Context, t *store.ToolCall) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tool_calls SET output_json=?, is_error=?, finished_at=?
		 WHERE id=?`,
		t.OutputJSON, boolToInt(t.IsError), nullableMicros(t.FinishedAt), t.ID)
	if err != nil {
		return fmt.Errorf("sqlite: UpdateToolCall: %w", err)
	}
	return ensureRowsAffected(res, store.ErrNotFound)
}

func (s *Store) ListToolCalls(ctx context.Context, sessionID string) ([]*store.ToolCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, message_id, tool, input_json, output_json,
			is_error, started_at, finished_at
		 FROM tool_calls WHERE session_id = ? ORDER BY started_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListToolCalls: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.ToolCall
	for rows.Next() {
		var t store.ToolCall
		var isError int
		var startedAt int64
		var finishedAt sql.NullInt64
		if err := rows.Scan(&t.ID, &t.SessionID, &t.MessageID, &t.Tool,
			&t.InputJSON, &t.OutputJSON, &isError, &startedAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan tool call: %w", err)
		}
		t.IsError = isError != 0
		t.StartedAt = fromMicros(startedAt)
		t.FinishedAt = fromNullableMicros(finishedAt)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// ---- Permission CRUD ----

func (s *Store) SavePermission(ctx context.Context, p *store.Permission) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO permissions(id, session_id, tool, pattern, decision, scope, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		p.ID, p.SessionID, p.Tool, p.Pattern, string(p.Decision), string(p.Scope),
		toMicros(p.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: SavePermission: %w", err)
	}
	return nil
}

func (s *Store) ListPermissions(ctx context.Context, sessionID string) ([]*store.Permission, error) {
	// Permanent rules use session_id="" so they apply globally; we OR them
	// in with rules scoped to this specific session.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, tool, pattern, decision, scope, created_at
		 FROM permissions
		 WHERE session_id = '' OR session_id = ?
		 ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListPermissions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.Permission
	for rows.Next() {
		var p store.Permission
		var decision, scope string
		var createdAt int64
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Tool, &p.Pattern,
			&decision, &scope, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan permission: %w", err)
		}
		p.Decision = store.PermissionDecision(decision)
		p.Scope = store.PermissionScope(scope)
		p.CreatedAt = fromMicros(createdAt)
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *Store) DeletePermission(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM permissions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: DeletePermission: %w", err)
	}
	return ensureRowsAffected(res, store.ErrNotFound)
}

// ---- small helpers ----

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ensureRowsAffected(res sql.Result, onZero error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: rows affected: %w", err)
	}
	if n == 0 {
		return onZero
	}
	return nil
}
