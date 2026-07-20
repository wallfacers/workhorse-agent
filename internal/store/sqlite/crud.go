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
			agent_type, model, provider, title, instructions, metadata_json,
			ephemeral, created_at, updated_at, deleted_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.ParentID, string(sess.State), sess.Workdir, sess.EnvJSON,
		sess.AgentType, sess.Model, sess.Provider, sess.Title,
		sess.Instructions, sess.MetadataJSON, boolToInt(sess.Ephemeral),
		toMicros(sess.CreatedAt), toMicros(sess.UpdatedAt), nullableMicros(sess.DeletedAt))
	if err != nil {
		return fmt.Errorf("sqlite: CreateSession: %w", err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, id string) (*store.Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, parent_id, state, workdir, env_json, agent_type, model, provider, title,
			instructions, metadata_json, ephemeral, created_at, updated_at, deleted_at
		 FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

func (s *Store) ListSessions(ctx context.Context, includeDeleted bool) ([]*store.Session, error) {
	q := `SELECT id, parent_id, state, workdir, env_json, agent_type, model, provider, title,
			instructions, metadata_json, ephemeral, created_at, updated_at, deleted_at
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

// ListSessionsByWorkdir returns the project-scoped listing. MessageCount and
// LastMessagePreview are computed via correlated subqueries; the preview reuses
// the extract_text() function (the same one the FTS trigger uses) on the latest
// message's content_json.
func (s *Store) ListSessionsByWorkdir(ctx context.Context, workdir string) ([]*store.SessionSummary, error) {
	rows, err := s.db.QueryContext(ctx, sessionSummarySelect+
		` WHERE s.workdir = ? AND s.deleted_at IS NULL
		 ORDER BY s.updated_at DESC, s.id`, workdir)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListSessionsByWorkdir: %w", err)
	}
	return scanSessionSummaries(rows)
}

// ListAllSessions is ListSessionsByWorkdir without the workdir filter: every
// non-deleted session across all projects, newest-updated first, capped at 100.
func (s *Store) ListAllSessions(ctx context.Context) ([]*store.SessionSummary, error) {
	rows, err := s.db.QueryContext(ctx, sessionSummarySelect+
		` WHERE s.deleted_at IS NULL
		 ORDER BY s.updated_at DESC, s.id LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListAllSessions: %w", err)
	}
	return scanSessionSummaries(rows)
}

// sessionSummarySelect is the shared projection for SessionSummary rows:
// the session columns plus a correlated message-count and last-message preview
// (the preview reuses extract_text(), the same function the FTS trigger uses).
const sessionSummarySelect = `SELECT s.id, s.parent_id, s.state, s.workdir, s.env_json, s.agent_type,
		s.model, s.provider, s.title, s.instructions, s.metadata_json,
		s.ephemeral, s.created_at, s.updated_at, s.deleted_at,
		(SELECT count(*) FROM messages m WHERE m.session_id = s.id) AS msg_count,
		coalesce((SELECT extract_text(m.content_json) FROM messages m
			WHERE m.session_id = s.id ORDER BY m.created_at DESC, m.id DESC LIMIT 1), '') AS last_preview
	 FROM sessions s`

// scanSessionSummaries drains rows from a sessionSummarySelect query and closes
// them. The column order must match sessionSummarySelect.
func scanSessionSummaries(rows *sql.Rows) ([]*store.SessionSummary, error) {
	defer rows.Close() //nolint:errcheck

	var out []*store.SessionSummary
	for rows.Next() {
		var sum store.SessionSummary
		var state string
		var ephemeral int
		var createdAt, updatedAt int64
		var deletedAt sql.NullInt64
		if err := rows.Scan(&sum.ID, &sum.ParentID, &state, &sum.Workdir,
			&sum.EnvJSON, &sum.AgentType, &sum.Model, &sum.Provider, &sum.Title,
			&sum.Instructions, &sum.MetadataJSON, &ephemeral,
			&createdAt, &updatedAt, &deletedAt,
			&sum.MessageCount, &sum.LastMessagePreview); err != nil {
			return nil, fmt.Errorf("sqlite: scan session summary: %w", err)
		}
		sum.State = store.SessionState(state)
		sum.Ephemeral = ephemeral != 0
		sum.CreatedAt = fromMicros(createdAt)
		sum.UpdatedAt = fromMicros(updatedAt)
		sum.DeletedAt = fromNullableMicros(deletedAt)
		out = append(out, &sum)
	}
	return out, rows.Err()
}

// ListProjects aggregates non-deleted sessions by workdir.
func (s *Store) ListProjects(ctx context.Context) ([]*store.Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workdir, count(*) AS cnt, max(updated_at) AS upd
		 FROM sessions WHERE deleted_at IS NULL
		 GROUP BY workdir ORDER BY upd DESC, workdir`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListProjects: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.Project
	for rows.Next() {
		var p store.Project
		var upd int64
		if err := rows.Scan(&p.Path, &p.SessionCount, &upd); err != nil {
			return nil, fmt.Errorf("sqlite: scan project: %w", err)
		}
		p.UpdatedAt = fromMicros(upd)
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *Store) UpdateSession(ctx context.Context, sess *store.Session) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET parent_id=?, state=?, workdir=?, env_json=?,
			agent_type=?, model=?, provider=?, title=?, instructions=?, metadata_json=?,
			ephemeral=?, updated_at=?, deleted_at=?
		 WHERE id=?`,
		sess.ParentID, string(sess.State), sess.Workdir, sess.EnvJSON,
		sess.AgentType, sess.Model, sess.Provider, sess.Title,
		sess.Instructions, sess.MetadataJSON, boolToInt(sess.Ephemeral),
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
		_, getErr := s.GetSession(ctx, id)
		if errors.Is(getErr, store.ErrNotFound) {
			return store.ErrNotFound
		}
		if getErr != nil {
			return fmt.Errorf("sqlite: DeleteSession: probe: %w", getErr)
		}
	}
	return nil
}

// PurgeSession hard-deletes the session row. messages/events/tool_calls are
// removed via ON DELETE CASCADE; messages are deleted first so their AFTER
// DELETE trigger keeps messages_fts in sync (FK cascade deletes do not reliably
// fire row triggers).
func (s *Store) PurgeSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: PurgeSession begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: PurgeSession messages: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: PurgeSession: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: PurgeSession commit: %w", err)
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

func (s *Store) UpdateSessionTitle(ctx context.Context, id, title string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		title, toMicros(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: UpdateSessionTitle: %w", err)
	}
	return ensureRowsAffected(res, store.ErrNotFound)
}

func (s *Store) CountMessages(ctx context.Context, sessionID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: CountMessages: %w", err)
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
		&sess.EnvJSON, &sess.AgentType, &sess.Model, &sess.Provider, &sess.Title,
		&sess.Instructions, &sess.MetadataJSON, &ephemeral,
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
		`INSERT INTO messages(id, session_id, role, content_json, stop_reason, token_count, interrupted, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		m.ID, m.SessionID, m.Role, m.ContentJSON, m.StopReason, m.TokenCount, boolToInt(m.Interrupted), toMicros(m.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: AppendMessage: %w", err)
	}
	return nil
}

// ReplaceMessages atomically swaps a session's entire transcript for msgs.
// Used by the compaction rewrite path so the persisted messages stay equal to
// the in-memory context the model actually sees (add-project-sessions D9).
func (s *Store) ReplaceMessages(ctx context.Context, sessionID string, msgs []*store.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: ReplaceMessages begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("sqlite: ReplaceMessages delete: %w", err)
	}
	for _, m := range msgs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages(id, session_id, role, content_json, stop_reason, token_count, interrupted, created_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			m.ID, m.SessionID, m.Role, m.ContentJSON, m.StopReason, m.TokenCount, boolToInt(m.Interrupted), toMicros(m.CreatedAt)); err != nil {
			return fmt.Errorf("sqlite: ReplaceMessages insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: ReplaceMessages commit: %w", err)
	}
	return nil
}

func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]*store.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, role, content_json, stop_reason, token_count, interrupted, created_at
		 FROM messages WHERE session_id = ? ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListMessages: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.Message
	for rows.Next() {
		var m store.Message
		var createdAt int64
		var interrupted int64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.ContentJSON,
			&m.StopReason, &m.TokenCount, &interrupted, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan message: %w", err)
		}
		m.CreatedAt = fromMicros(createdAt)
		m.Interrupted = interrupted != 0
		out = append(out, &m)
	}
	return out, rows.Err()
}

// MarkMessageInterrupted sets the interrupted flag on a specific message by
// its ULID primary key. The UPDATE is a no-op (succeeds with 0 rows affected)
// when the message does not exist, since we always have the ID in hand from
// the Session's lastAssistantMsgID tracker.
func (s *Store) MarkMessageInterrupted(ctx context.Context, messageID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET interrupted = 1 WHERE id = ?`, messageID)
	if err != nil {
		return fmt.Errorf("sqlite: MarkMessageInterrupted: %w", err)
	}
	return nil
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
		`INSERT OR REPLACE INTO permissions(id, session_id, tool, pattern, decision, scope, created_at)
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

// ---- Delegation CRUD (001-agent-orchestration US1) ----

const delegationColumns = `id, session_id, description, prompt, workdir, status,
	title, summary, result, error, started_at, completed_at, notified_at`

func (s *Store) CreateDelegation(ctx context.Context, d *store.Delegation) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO delegations(id, session_id, description, prompt, workdir, status,
			title, summary, result, error, started_at, completed_at, notified_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.SessionID, d.Description, d.Prompt, d.Workdir, string(d.Status),
		d.Title, d.Summary, d.Result, d.Error,
		toMicros(d.StartedAt), nullableMicros(d.CompletedAt), nullableMicros(d.NotifiedAt))
	if err != nil {
		return fmt.Errorf("sqlite: CreateDelegation: %w", err)
	}
	return nil
}

func (s *Store) GetDelegation(ctx context.Context, id string) (*store.Delegation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+delegationColumns+` FROM delegations WHERE id = ?`, id)
	return scanDelegation(row)
}

func (s *Store) ListDelegations(ctx context.Context, sessionID string) ([]*store.Delegation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+delegationColumns+`
		 FROM delegations WHERE session_id = ? ORDER BY started_at DESC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListDelegations: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.Delegation
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) CountRunningDelegations(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM delegations WHERE status = 'running'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: CountRunningDelegations: %w", err)
	}
	return n, nil
}

func (s *Store) CompleteDelegation(ctx context.Context, id, title, summary, result string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE delegations SET status='complete', title=?, summary=?, result=?, completed_at=?
		 WHERE id=?`,
		title, summary, result, toMicros(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: CompleteDelegation: %w", err)
	}
	return ensureRowsAffected(res, store.ErrNotFound)
}

func (s *Store) FailDelegation(ctx context.Context, id, errMsg, result string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE delegations SET status='error', error=?, result=?, completed_at=? WHERE id=?`,
		errMsg, result, toMicros(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: FailDelegation: %w", err)
	}
	return ensureRowsAffected(res, store.ErrNotFound)
}

// ClaimPendingNotifications selects finished-but-unnotified delegations for the
// session and stamps notified_at inside one transaction. The mark happens before
// the caller injects the notice into history, so a crash between claim and
// inject drops one notification instead of producing a duplicate on restart.
func (s *Store) ClaimPendingNotifications(ctx context.Context, sessionID string) ([]*store.Delegation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ClaimPendingNotifications begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	rows, err := tx.QueryContext(ctx,
		`SELECT `+delegationColumns+`
		 FROM delegations
		 WHERE session_id = ? AND status != 'running' AND notified_at IS NULL
		 ORDER BY started_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ClaimPendingNotifications select: %w", err)
	}
	var pending []*store.Delegation
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: ClaimPendingNotifications drain: %w", err)
	}

	if len(pending) == 0 {
		return nil, tx.Commit()
	}
	now := toMicros(time.Now().UTC())
	for _, d := range pending {
		if _, err := tx.ExecContext(ctx,
			`UPDATE delegations SET notified_at = ? WHERE id = ?`, now, d.ID); err != nil {
			return nil, fmt.Errorf("sqlite: ClaimPendingNotifications mark: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: ClaimPendingNotifications commit: %w", err)
	}
	return pending, nil
}

func (s *Store) ReapRunningDelegations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE delegations SET status='error', error='server restarted', completed_at=?
		 WHERE status='running'`, toMicros(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("sqlite: ReapRunningDelegations: %w", err)
	}
	return nil
}

func scanDelegation(sc scanner) (*store.Delegation, error) {
	var d store.Delegation
	var status string
	var startedAt int64
	var completedAt, notifiedAt sql.NullInt64
	if err := sc.Scan(&d.ID, &d.SessionID, &d.Description, &d.Prompt, &d.Workdir,
		&status, &d.Title, &d.Summary, &d.Result, &d.Error,
		&startedAt, &completedAt, &notifiedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: scan delegation: %w", err)
	}
	d.Status = store.DelegationStatus(status)
	d.StartedAt = fromMicros(startedAt)
	d.CompletedAt = fromNullableMicros(completedAt)
	d.NotifiedAt = fromNullableMicros(notifiedAt)
	return &d, nil
}

// ---- Schedule CRUD (001-agent-orchestration US3) ----

const scheduleColumns = `id, name, instruction, cron, run_at, workdir, enabled,
	created_at, last_run_at`

const scheduleRunColumns = `id, schedule_id, session_id, started_at, completed_at,
	status, output_tail, error`

const scheduleRunsKept = 20

func (s *Store) CreateSchedule(ctx context.Context, sch *store.Schedule) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO schedules(id, name, instruction, cron, run_at, workdir, enabled,
			created_at, last_run_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		sch.ID, sch.Name, sch.Instruction, nullableString(sch.Cron), nullableMicros(sch.RunAt),
		sch.Workdir, boolToInt(sch.Enabled), toMicros(sch.CreatedAt), nullableMicros(sch.LastRunAt))
	if err != nil {
		return fmt.Errorf("sqlite: CreateSchedule: %w", err)
	}
	return nil
}

func (s *Store) GetSchedule(ctx context.Context, id string) (*store.Schedule, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id)
	return scanSchedule(row)
}

func (s *Store) ListSchedules(ctx context.Context) ([]*store.Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListSchedules: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.Schedule
	for rows.Next() {
		sch, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, rows.Err()
}

// DeleteSchedule removes a schedule and its run log atomically. The run rows
// are deleted first so there is no window where a plan is gone but its runs
// linger.
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: DeleteSchedule begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx, `DELETE FROM schedule_runs WHERE schedule_id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: DeleteSchedule runs: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: DeleteSchedule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: DeleteSchedule commit: %w", err)
	}
	return nil
}

// TouchScheduleRun stamps last_run_at and, for a one-shot schedule (run_at set),
// disables it so it never fires again (FR-019).
func (s *Store) TouchScheduleRun(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET last_run_at = ?,
		   enabled = CASE WHEN run_at IS NOT NULL THEN 0 ELSE enabled END
		 WHERE id = ?`, toMicros(at), id)
	if err != nil {
		return fmt.Errorf("sqlite: TouchScheduleRun: %w", err)
	}
	return ensureRowsAffected(res, store.ErrNotFound)
}

func (s *Store) CreateScheduleRun(ctx context.Context, r *store.ScheduleRun) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite: CreateScheduleRun begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	res, err := tx.ExecContext(ctx,
		`INSERT INTO schedule_runs(schedule_id, session_id, started_at, completed_at, status, output_tail, error)
		 VALUES (?,?,?,?,?,?,?)`,
		r.ScheduleID, nullableString(r.SessionID), toMicros(r.StartedAt), nullableMicros(r.CompletedAt),
		string(r.Status), nullableString(r.OutputTail), nullableString(r.Error))
	if err != nil {
		return 0, fmt.Errorf("sqlite: CreateScheduleRun: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sqlite: CreateScheduleRun last id: %w", err)
	}
	r.ID = id
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM schedule_runs WHERE schedule_id = ? AND id NOT IN (
			SELECT id FROM schedule_runs WHERE schedule_id = ? ORDER BY started_at DESC LIMIT ?
		)`, r.ScheduleID, r.ScheduleID, scheduleRunsKept); err != nil {
		return 0, fmt.Errorf("sqlite: CreateScheduleRun prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite: CreateScheduleRun commit: %w", err)
	}
	return id, nil
}

func (s *Store) FinishScheduleRun(ctx context.Context, id int64, status store.ScheduleRunStatus, outputTail, errMsg string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedule_runs SET status = ?, output_tail = ?, error = ?, completed_at = ? WHERE id = ?`,
		string(status), outputTail, errMsg, toMicros(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: FinishScheduleRun: %w", err)
	}
	return ensureRowsAffected(res, store.ErrNotFound)
}

func (s *Store) ListScheduleRuns(ctx context.Context, scheduleID string, limit int) ([]*store.ScheduleRun, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scheduleRunColumns+`
		 FROM schedule_runs WHERE schedule_id = ? ORDER BY started_at DESC LIMIT ?`, scheduleID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: ListScheduleRuns: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []*store.ScheduleRun
	for rows.Next() {
		r, err := scanScheduleRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) PruneScheduleRuns(ctx context.Context, scheduleID string, keep int) error {
	if keep < 0 {
		keep = scheduleRunsKept
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM schedule_runs WHERE schedule_id = ? AND id NOT IN (
			SELECT id FROM schedule_runs WHERE schedule_id = ? ORDER BY started_at DESC LIMIT ?
		)`, scheduleID, scheduleID, keep)
	if err != nil {
		return fmt.Errorf("sqlite: PruneScheduleRuns: %w", err)
	}
	return nil
}

func scanSchedule(sc scanner) (*store.Schedule, error) {
	var sch store.Schedule
	var cron sql.NullString
	var runAt, lastRunAt sql.NullInt64
	var enabled int
	var createdAt int64
	if err := sc.Scan(&sch.ID, &sch.Name, &sch.Instruction, &cron, &runAt,
		&sch.Workdir, &enabled, &createdAt, &lastRunAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: scan schedule: %w", err)
	}
	sch.Cron = fromNullableString(cron)
	sch.RunAt = fromNullableMicros(runAt)
	sch.LastRunAt = fromNullableMicros(lastRunAt)
	sch.Enabled = enabled != 0
	sch.CreatedAt = fromMicros(createdAt)
	return &sch, nil
}

func scanScheduleRun(sc scanner) (*store.ScheduleRun, error) {
	var r store.ScheduleRun
	var status string
	var sessionID, outputTail, errMsg sql.NullString
	var startedAt int64
	var completedAt sql.NullInt64
	if err := sc.Scan(&r.ID, &r.ScheduleID, &sessionID, &startedAt, &completedAt,
		&status, &outputTail, &errMsg); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: scan schedule run: %w", err)
	}
	r.SessionID = fromNullableString(sessionID)
	r.StartedAt = fromMicros(startedAt)
	r.CompletedAt = fromNullableMicros(completedAt)
	r.Status = store.ScheduleRunStatus(status)
	r.OutputTail = fromNullableString(outputTail)
	r.Error = fromNullableString(errMsg)
	return &r, nil
}

// ---- small helpers ----

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullableString maps the empty string to SQL NULL so optional TEXT columns
// (schedule cron/session_id/output_tail/error) stay NULL when unset.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func fromNullableString(n sql.NullString) string {
	if !n.Valid {
		return ""
	}
	return n.String
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
