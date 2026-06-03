package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wallfacers/workhorse-agent/internal/api/protocol"
	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

// createSessionRequest is the POST /v1/sessions body shape.
type createSessionRequest struct {
	Workdir      string            `json:"workdir"`
	Env          map[string]string `json:"env,omitempty"`
	Provider     string            `json:"provider"`
	Model        string            `json:"model"`
	AgentType    string            `json:"agent_type,omitempty"`
	Ephemeral    bool              `json:"ephemeral,omitempty"`
	AllowedTools []string          `json:"allowed_tools,omitempty"`
	ParentID     string            `json:"parent_id,omitempty"`
}

// isoMillis is the ISO-8601 timestamp format the wire SessionMeta uses.
const isoMillis = "2006-01-02T15:04:05.000Z"

// sessionMeta is the camelCase wire projection consumed by workhorse-assistant
// (AgentSessionMeta). id/workdir/title/status are always present; title is
// emitted even when empty (the TS interface types it non-optional) and status
// is strictly "idle"|"running" (add-project-sessions D1/D2).
type sessionMeta struct {
	ID                 string `json:"id"`
	Workdir            string `json:"workdir"`
	Title              string `json:"title"`
	Status             string `json:"status"`
	CreatedAt          string `json:"createdAt,omitempty"`
	UpdatedAt          string `json:"updatedAt,omitempty"`
	MessageCount       int    `json:"messageCount"`
	LastMessagePreview string `json:"lastMessagePreview,omitempty"`
	ParentID           string `json:"parentId,omitempty"`
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	AgentType          string `json:"agentType,omitempty"`
	Ephemeral          bool   `json:"ephemeral,omitempty"`
}

// metaFromLive projects a live in-memory session. MessageCount mirrors the
// persisted transcript (one row per appended message).
func metaFromLive(s *session.Session) sessionMeta {
	return sessionMeta{
		ID:           s.ID,
		Workdir:      s.Workdir,
		Title:        s.Title(),
		Status:       s.Status(),
		CreatedAt:    s.CreatedAt.Format(isoMillis),
		UpdatedAt:    s.UpdatedAt().Format(isoMillis),
		MessageCount: len(s.History()),
		ParentID:     s.ParentID,
		Provider:     s.ProviderName,
		Model:        s.Model,
		AgentType:    s.AgentType,
		Ephemeral:    s.Ephemeral,
	}
}

// metaFromSummary projects a persisted session row. status is overlaid by the
// caller (live → running, otherwise idle).
func metaFromSummary(sum *store.SessionSummary, status string) sessionMeta {
	return sessionMeta{
		ID:                 sum.ID,
		Workdir:            sum.Workdir,
		Title:              sum.Title,
		Status:             status,
		CreatedAt:          sum.CreatedAt.Format(isoMillis),
		UpdatedAt:          sum.UpdatedAt.Format(isoMillis),
		MessageCount:       sum.MessageCount,
		LastMessagePreview: previewLine(sum.LastMessagePreview),
		ParentID:           sum.ParentID,
		Model:              sum.Model,
		AgentType:          sum.AgentType,
		Ephemeral:          sum.Ephemeral,
	}
}

// previewLine collapses a last-message preview to a single trimmed line capped
// at 120 runes for the session-list subtitle.
func previewLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

func sanitizeTitle(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if i := strings.IndexAny(t, "\r\n"); i >= 0 {
		t = t[:i]
	}
	r := []rune(t)
	if len(r) > 200 {
		t = string(r[:197]) + "..."
	}
	if t == "" {
		return "", fmt.Errorf("title must not be empty")
	}
	return t, nil
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if s.shutdownInFlight.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"code":    string(protocol.ErrServerShutdown),
			"message": "server is shutting down",
		})
		return
	}
	if !requireJSONBody(w, r) {
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			writeRequestTooLarge(w, s.cfg.MaxRequestBodyBytes)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	var req createSessionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    "bad_request",
			"message": "invalid JSON body: " + err.Error(),
		})
		return
	}

	// Enforce max_concurrent at the API layer too — the manager checks but
	// returning 429 with a clear shape is the spec's contract for this end.
	if s.cfg.MaxConcurrentSessions > 0 && s.manager.CountActive() >= s.cfg.MaxConcurrentSessions {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"code":    "too_many_sessions",
			"message": "active session count is at sessions.max_concurrent",
			"limit":   s.cfg.MaxConcurrentSessions,
		})
		return
	}

	sess, err := s.manager.CreateSession(r.Context(), session.Options{
		Workdir:      req.Workdir,
		Env:          req.Env,
		Ephemeral:    req.Ephemeral,
		Model:        req.Model,
		ProviderName: req.Provider,
		AgentType:    req.AgentType,
		ParentID:     req.ParentID,
		AllowedTools: req.AllowedTools,
	})
	if err != nil {
		if errors.Is(err, session.ErrTooManyConcurrent) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"code":    "too_many_sessions",
				"message": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code":    "internal",
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusCreated, metaFromLive(sess))
}

// handleListSessions serves GET /v1/sessions. With ?workdir=<path> it returns
// the project-scoped, persistence-backed listing (the in-app switcher); without
// it, the full persisted set across ALL projects (the cross-project
// session-management view). Both survive restart and overlay live status.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if workdir := r.URL.Query().Get("workdir"); workdir != "" {
		s.listSessionsByWorkdir(w, r, workdir)
		return
	}
	s.listAllSessions(w, r)
}

// listAllSessions backs GET /v1/sessions with no workdir: every non-deleted
// persisted session across all projects, live status overlaid. Falls back to the
// live-only set when no persistent store is configured.
func (s *Server) listAllSessions(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		all := s.manager.ListSessions()
		out := make([]sessionMeta, 0, len(all))
		for _, sess := range all {
			out = append(out, metaFromLive(sess))
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
		return
	}
	rows, err := s.store.ListAllSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": "internal", "message": "list sessions failed",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.summariesToMeta(rows)})
}

func (s *Server) listSessionsByWorkdir(w http.ResponseWriter, r *http.Request, workdir string) {
	if s.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": []sessionMeta{}})
		return
	}
	rows, err := s.store.ListSessionsByWorkdir(r.Context(), workdir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": "internal", "message": "list sessions failed",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.summariesToMeta(rows)})
}

// summariesToMeta projects persisted summaries to wire metas, overlaying live
// status: a session currently running a turn reports "running"; everything
// persisted-but-idle reports "idle".
func (s *Server) summariesToMeta(rows []*store.SessionSummary) []sessionMeta {
	out := make([]sessionMeta, 0, len(rows))
	for _, row := range rows {
		status := "idle"
		if live, err := s.manager.GetSession(row.ID); err == nil {
			status = live.Status()
		}
		out = append(out, metaFromSummary(row, status))
	}
	return out
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.manager.GetSession(id)
	if err == nil {
		writeJSON(w, http.StatusOK, metaFromLive(sess))
		return
	}
	// Fall back to store for persisted-but-not-loaded sessions.
	if s.store != nil {
		row, serr := s.store.GetSession(r.Context(), id)
		if serr == nil && row.DeletedAt == nil {
			msgCount, _ := s.store.CountMessages(r.Context(), id)
			writeJSON(w, http.StatusOK, metaFromSummary(&store.SessionSummary{
				Session:      *row,
				MessageCount: msgCount,
			}, "idle"))
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{
		"code": "session_not_found", "message": "no such session",
	})
}

// handleRenameSession serves PATCH /v1/sessions/{id} with body {"title": ...}.
// It updates the persisted title (if the session is stored) and the live title
// (if loaded), then returns the updated SessionMeta. No hydration: renaming an
// unloaded session must not spin up a runner.
func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	if !requireJSONBody(w, r) {
		return
	}
	defer r.Body.Close()
	id := r.PathValue("id")

	var req struct {
		Title string `json:"title"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			writeRequestTooLarge(w, s.cfg.MaxRequestBodyBytes)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "bad_request", "message": err.Error()})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "bad_request", "message": "invalid JSON body"})
		return
	}
	title, titleErr := sanitizeTitle(req.Title)
	if titleErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "bad_request", "message": titleErr.Error()})
		return
	}

	live, liveErr := s.manager.GetSession(id)

	var row *store.Session
	if s.store != nil {
		if r2, err := s.store.GetSession(r.Context(), id); err == nil && r2.DeletedAt == nil {
			row = r2
		}
	}
	if liveErr != nil && row == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": "session_not_found", "message": "no such session"})
		return
	}

	if liveErr == nil {
		live.SetTitle(title)
	}
	if row != nil {
		if err := s.store.UpdateSessionTitle(r.Context(), id, title); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"code": "internal", "message": "rename failed"})
			return
		}
		row.Title = title
	}

	if liveErr == nil {
		writeJSON(w, http.StatusOK, metaFromLive(live))
		return
	}
	writeJSON(w, http.StatusOK, metaFromSummary(&store.SessionSummary{Session: *row}, "idle"))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.manager.DeleteSession(r.Context(), id, s.cfg.GracefulShutdownTimeout)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			// Not live — try hard-purging from store directly (add-project-sessions D6).
			if s.store != nil {
				if perr := s.store.PurgeSession(r.Context(), id); perr == nil {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			writeJSON(w, http.StatusNotFound, map[string]any{
				"code": "session_not_found", "message": "no such session",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": "internal", "message": err.Error(),
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCancelSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.manager.Cancel(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCompactSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.manager.GetSession(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	if state := sess.State(); state != session.StateIdle {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":    string(protocol.ErrSessionBusy),
			"message": "compact requires session in Idle",
			"state":   string(state),
		})
		// Mirror to SSE so SSE-only clients see the same outcome (spec
		// requires double-emit for POST/state conflicts).
		_ = sess.EmitNow(string(protocol.EventError),
			protocol.NewErrorPayload(protocol.ErrSessionBusy,
				"compact requires session in Idle",
				map[string]any{"state": string(state)}))
		return
	}
	if err := s.manager.RequestCompact(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    "session_not_found",
			"message": "no such session",
		})
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleHistory returns the reconstructed transcript for a session
// (add-project-sessions T2, design D4). tool_result blocks are merged into
// their matching tool_call by toolUseId — they do not appear as separate parts.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []any{}})
		return
	}
	// Verify the session exists (live or persisted).
	if _, err := s.manager.GetSession(id); err != nil {
		row, serr := s.store.GetSession(r.Context(), id)
		if serr != nil || row.DeletedAt != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"code": "session_not_found", "message": "no such session",
			})
			return
		}
	}

	msgs, err := s.store.ListMessages(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": "internal", "message": err.Error(),
		})
		return
	}
	result := buildHistory(msgs)
	writeJSON(w, http.StatusOK, map[string]any{"messages": result})
}

// handleListProjects returns distinct workdirs with at least one non-deleted
// session (add-project-sessions T2).
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"projects": []any{}})
		return
	}
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": "internal", "message": err.Error(),
		})
		return
	}
	type projectMeta struct {
		Path         string `json:"path"`
		SessionCount int    `json:"sessionCount,omitempty"`
		UpdatedAt    string `json:"updatedAt,omitempty"`
	}
	out := make([]projectMeta, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectMeta{
			Path:         p.Path,
			SessionCount: p.SessionCount,
			UpdatedAt:    p.UpdatedAt.Format(isoMillis),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

// historyPart is one content block in a HistoryMessage (add-project-sessions D4).
type historyPart struct {
	Type     string          `json:"type"`
	Content  string          `json:"content,omitempty"`
	Text     string          `json:"text,omitempty"`
	Status   string          `json:"status,omitempty"`
	Redacted bool            `json:"redacted,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
}

type historyMessage struct {
	ID    string        `json:"id"`
	Role  string        `json:"role"`
	Parts []historyPart `json:"parts"`
}

// buildHistory converts stored messages into wire HistoryMessage slices.
func buildHistory(msgs []*store.Message) []historyMessage {
	type toolCallRef struct{ msgIdx, partIdx int }
	toolCallMap := make(map[string]toolCallRef)

	// Pre-parse content blocks.
	type parsed struct {
		id     string
		role   string
		blocks []provider.ContentBlock
	}
	parsedMsgs := make([]parsed, 0, len(msgs))
	for _, m := range msgs {
		blocks, _ := session.DecodeContent(m.ContentJSON)
		parsedMsgs = append(parsedMsgs, parsed{id: m.ID, role: m.Role, blocks: blocks})
	}

	allParts := make([][]historyPart, len(parsedMsgs))
	for mi, pm := range parsedMsgs {
		parts := make([]historyPart, 0, len(pm.blocks))
		for _, b := range pm.blocks {
			switch b.Type {
			case provider.BlockText:
				parts = append(parts, historyPart{Type: "text", Content: b.Text})
			case provider.BlockThinking:
				parts = append(parts, historyPart{Type: "reasoning", Text: b.Thinking, Status: "done"})
			case provider.BlockRedactedThinking:
				parts = append(parts, historyPart{Type: "reasoning", Text: b.RedactedData, Status: "done", Redacted: true})
			case provider.BlockToolUse:
				pi := len(parts)
				parts = append(parts, historyPart{
					Type: "tool_call", ID: b.ToolUseID, Name: b.ToolName,
					Input: b.Input, Status: "pending",
				})
				toolCallMap[b.ToolUseID] = toolCallRef{msgIdx: mi, partIdx: pi}
			case provider.BlockToolResult:
				if ref, ok := toolCallMap[b.ToolUseID]; ok {
					tc := &allParts[ref.msgIdx][ref.partIdx]
					if b.IsError {
						tc.Status = "error"
					} else {
						tc.Status = "done"
					}
					if b.Output != "" {
						raw := json.RawMessage(b.Output)
						if !json.Valid(raw) {
							quoted, _ := json.Marshal(b.Output)
							raw = quoted
						}
						tc.Output = raw
					}
				}
			}
		}
		allParts[mi] = parts
	}

	result := make([]historyMessage, 0, len(parsedMsgs))
	for i, pm := range parsedMsgs {
		if len(allParts[i]) == 0 {
			continue
		}
		result = append(result, historyMessage{ID: pm.id, Role: pm.role, Parts: allParts[i]})
	}
	return result
}

// requireJSONBody enforces Content-Type: application/json on POST. Returns
// false (and writes 415) if absent. POSTs without bodies (cancel/compact)
// skip this; they call directly.
func requireJSONBody(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
			"code":    "unsupported_media_type",
			"message": "missing Content-Type",
		})
		return false
	}
	// Allow `application/json; charset=utf-8` and the like.
	if !isJSONContentType(ct) {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
			"code":    "unsupported_media_type",
			"message": "Content-Type must be application/json",
		})
		return false
	}
	return true
}

func isJSONContentType(ct string) bool {
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	for i := 0; i < len(ct); i++ {
		if ct[i] == ' ' || ct[i] == '\t' {
			continue
		}
		ct = ct[i:]
		break
	}
	// Trim trailing whitespace.
	for len(ct) > 0 && (ct[len(ct)-1] == ' ' || ct[len(ct)-1] == '\t') {
		ct = ct[:len(ct)-1]
	}
	return ct == "application/json"
}
