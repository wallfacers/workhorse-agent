package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/store"
)

const (
	// MaxConcurrent is the fixed cap on simultaneously running delegations.
	MaxConcurrent = 4

	maxDescriptionRunes = 200
	maxPromptBytes      = 32 * 1024
	maxTitleRunes       = 48
	maxSummaryRunes     = 180

	// childTimeout bounds a single background delegation. A read-only research
	// task should finish well inside this; overrunning is recorded as a failure.
	childTimeout = 10 * time.Minute
)

// ReadOnlyAllowedTools is the fixed tool surface for delegation child sessions.
// It contains only read-only tools, so a background delegation can never mutate
// files or execute commands (FR-002). The delegate tool is absent, which is the
// first line of defense against nesting (FR-003).
var ReadOnlyAllowedTools = []string{
	"Read", "Grep", "session_search",
	"LoadMemory", "memory_read", "MemorySearch",
	"ToolSearch",
}

// ChildMetaKey marks a session as a delegation child. The Manager refuses to
// start a delegation whose parent carries this marker — the second line of
// defense against nesting (FR-003).
const ChildMetaKey = "delegation_child"

// Manager owns the background delegation lifecycle: it spawns read-only child
// sessions, persists results, and produces one-shot completion notices.
type Manager struct {
	Store   store.Store
	SessMgr *session.Manager
	Log     *slog.Logger
}

// NewManager constructs a Manager. Log defaults to slog.Default() when nil.
func NewManager(st store.Store, mgr *session.Manager, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{Store: st, SessMgr: mgr, Log: log}
}

// Start validates the request, persists a running delegation row, and launches
// a background goroutine that drives an ephemeral read-only child session to
// completion. It returns immediately with the human-readable delegation id.
func (m *Manager) Start(ctx context.Context, parentSessionID, workdir, description, prompt string) (string, error) {
	if err := validateRequest(description, prompt); err != nil {
		return "", err
	}
	parent, err := m.SessMgr.GetSession(parentSessionID)
	if err != nil {
		return "", fmt.Errorf("delegation: parent session not found: %w", err)
	}
	if _, nested := parent.Metadata[ChildMetaKey]; nested {
		return "", errors.New("Nested delegations are not allowed.")
	}
	running, err := m.Store.CountRunningDelegations(ctx)
	if err != nil {
		return "", fmt.Errorf("delegation: count running: %w", err)
	}
	if running >= MaxConcurrent {
		return "", fmt.Errorf("Too many running delegations (%d). Wait for one to finish.", MaxConcurrent)
	}

	id, err := GenerateUnique(func(candidate string) bool {
		_, lookupErr := m.Store.GetDelegation(ctx, candidate)
		return lookupErr == nil
	})
	if err != nil {
		return "", err
	}
	if err := m.Store.CreateDelegation(ctx, &store.Delegation{
		ID:          id,
		SessionID:   parentSessionID,
		Description: description,
		Prompt:      prompt,
		Workdir:     workdir,
		Status:      store.DelegationRunning,
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		return "", fmt.Errorf("delegation: create: %w", err)
	}

	go m.runChild(id, parentSessionID, workdir, prompt)
	return id, nil
}

func validateRequest(description, prompt string) error {
	if strings.TrimSpace(description) == "" {
		return errors.New("delegation: description is required")
	}
	if utf8.RuneCountInString(description) > maxDescriptionRunes {
		return fmt.Errorf("delegation: description exceeds %d chars", maxDescriptionRunes)
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("delegation: prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return fmt.Errorf("delegation: prompt exceeds %d KiB", maxPromptBytes/1024)
	}
	return nil
}

// runChild drives one ephemeral read-only child session to completion and
// records the outcome. It runs in its own goroutine; every exit path writes a
// terminal status to the store. A panic is recovered and recorded as a failure.
func (m *Manager) runChild(id, parentSessionID, workdir, prompt string) {
	defer func() {
		if r := recover(); r != nil {
			m.Log.Error("delegation: child panic", "id", id, "recover", fmt.Sprintf("%v", r))
			_ = m.Store.FailDelegation(context.Background(), id, fmt.Sprintf("internal error: %v", r), "")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), childTimeout)
	defer cancel()

	child, err := m.SessMgr.CreateSession(ctx, session.Options{
		ParentID:     parentSessionID,
		Workdir:      workdir,
		Ephemeral:    true,
		AllowedTools: ReadOnlyAllowedTools,
		Metadata:     map[string]string{ChildMetaKey: id},
	})
	if err != nil {
		m.fail(id, "create child session: "+err.Error())
		return
	}
	defer m.deleteChild(child.ID)

	c := newCollector()
	pumpDone := make(chan struct{})
	go pump(ctx, child, c, pumpDone)

	payload, _ := json.Marshal(session.UserMessagePayload{Content: prompt})
	select {
	case child.Inbox <- session.ClientMessage{Type: session.ClientUserMessage, Payload: payload}:
	case <-ctx.Done():
		<-pumpDone
		m.fail(id, "cancelled before the task started")
		return
	}

	<-pumpDone

	if ctx.Err() != nil {
		m.fail(id, "cancelled")
		return
	}
	if msg := c.ErrorMessage(); msg != "" {
		m.fail(id, msg)
		return
	}
	result := c.FinalText()
	if err := m.Store.CompleteDelegation(context.Background(), id, deriveTitle(result), deriveSummary(result), result); err != nil {
		m.Log.Warn("delegation: record completion", "id", id, "err", err)
	}
}

func (m *Manager) fail(id, reason string) {
	if err := m.Store.FailDelegation(context.Background(), id, reason, ""); err != nil {
		m.Log.Warn("delegation: record failure", "id", id, "err", err)
	}
}

func (m *Manager) deleteChild(id string) {
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.SessMgr.DeleteSession(cctx, id, 2*time.Second); err != nil {
		m.Log.Warn("delegation: cleanup child session", "id", id, "err", err)
	}
}

// Read returns the delegation record by id.
func (m *Manager) Read(ctx context.Context, id string) (*store.Delegation, error) {
	return m.Store.GetDelegation(ctx, id)
}

// List returns every delegation for a session, newest-started first.
func (m *Manager) List(ctx context.Context, sessionID string) ([]*store.Delegation, error) {
	return m.Store.ListDelegations(ctx, sessionID)
}

// ConsumeNotifications claims every finished-but-unnotified delegation for the
// session and renders each as a one-shot completion notice. The agent loop
// injects these as system messages at the start of the next turn.
func (m *Manager) ConsumeNotifications(ctx context.Context, sessionID string) []string {
	pending, err := m.Store.ClaimPendingNotifications(ctx, sessionID)
	if err != nil {
		m.Log.Warn("delegation: claim notifications", "session", sessionID, "err", err)
		return nil
	}
	out := make([]string, 0, len(pending))
	for _, d := range pending {
		out = append(out, renderNotice(d))
	}
	return out
}

func renderNotice(d *store.Delegation) string {
	var sb strings.Builder
	if d.Status == store.DelegationError {
		sb.WriteString("[Background research delegation failed] ")
	} else {
		sb.WriteString("[Background research delegation completed] ")
	}
	sb.WriteString(d.ID)
	if d.Title != "" {
		sb.WriteString(" — ")
		sb.WriteString(d.Title)
	}
	sb.WriteByte('\n')
	if d.Status == store.DelegationError {
		sb.WriteString("The task did not finish successfully")
		if d.Error != "" {
			sb.WriteString(": ")
			sb.WriteString(d.Error)
		}
		sb.WriteByte('.')
	} else if d.Summary != "" {
		sb.WriteString(d.Summary)
	} else {
		sb.WriteString("The full result is ready.")
	}
	sb.WriteString("\nCall delegation_read(\"")
	sb.WriteString(d.ID)
	sb.WriteString("\") to retrieve the full result.")
	return sb.String()
}

// deriveTitle returns the first non-empty line of the result, capped at
// maxTitleRunes code points.
func deriveTitle(result string) string {
	s := strings.TrimSpace(result)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return capRunes(s, maxTitleRunes)
}

// deriveSummary collapses all whitespace runs to single spaces and caps the
// result at maxSummaryRunes code points.
func deriveSummary(result string) string {
	folded := strings.Join(strings.Fields(result), " ")
	return capRunes(folded, maxSummaryRunes)
}

func capRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
