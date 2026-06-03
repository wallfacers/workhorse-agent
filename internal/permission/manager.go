package permission

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store"
)

// Decision mirrors the five values in the permission-control spec.
type Decision = store.PermissionDecision

// Re-export the decision and scope constants so callers don't have to import
// internal/store directly.
const (
	AllowOnce      = store.DecisionAllowOnce
	AllowSession   = store.DecisionAllowSession
	AllowPermanent = store.DecisionAllowPermanent
	Deny           = store.DecisionDeny
	DenyPermanent  = store.DecisionDenyPermanent
)

// ErrTimeout is what Check returns when no decision arrived within
// agent.permission_request_timeout_seconds.
var ErrTimeout = errors.New("permission: request timed out")

// Request is the structured payload the prompt callback receives. The
// Manager doesn't care how the prompt is presented (UI vs CLI vs Web) —
// callers supply a PromptFunc.
type Request struct {
	SessionID string
	Tool      string
	Resource  string // file path / command string / etc.
	Dangerous bool   // true when DangerousCommandGuard hit
	Reason    string // populated when Dangerous; the matched label
}

// PromptFunc is the user-facing prompt. It must respect ctx and return
// (decision, true) when the user answered or (Deny, false) on timeout.
// The Manager applies the supplied requestTimeout when the user passes nil
// for ctx.
type PromptFunc func(ctx context.Context, req Request) (Decision, bool)

// Manager evaluates whether a tool call may proceed for a session. It
// consults (in order): persistent rules from the store, session-scoped
// in-memory rules, the configured default decision, then falls back to a
// prompt — unless DangerousGuard flagged the call, in which case the prompt
// is *forced* regardless of any existing allow_* rules.
type Manager struct {
	store     store.Store
	prompt    PromptFunc
	dangerous func(tool, resource string) (bool, string)

	mu sync.Mutex
	// timeout and defaultDecision are mutable at runtime via SetTimeout /
	// SetDefaultDecision (config hot-reload) so they live under mu alongside the
	// session rules; every read goes through a locked getter.
	timeout         time.Duration
	defaultDecision Decision
	// rules cached per session: ScopeSession rules + ScopeOnce rules pending
	// first match. Permanent rules are read live from the store on every
	// Check so a manual UPDATE on the DB takes effect immediately.
	sessionRules map[string][]rule
}

// SetDefaultDecision replaces the fallback decision applied when no rule
// matches. Used by config hot-reload so a changed tools.default_permission
// takes effect on the next Check without restarting the process.
func (m *Manager) SetDefaultDecision(d Decision) {
	m.mu.Lock()
	m.defaultDecision = d
	m.mu.Unlock()
}

// SetTimeout replaces the per-prompt deadline. Used by config hot-reload when
// agent.permission_request_timeout_seconds changes.
func (m *Manager) SetTimeout(d time.Duration) {
	m.mu.Lock()
	m.timeout = d
	m.mu.Unlock()
}

func (m *Manager) getDefaultDecision() Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defaultDecision
}

func (m *Manager) getTimeout() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.timeout
}

type rule struct {
	pattern  string
	tool     string
	decision Decision
	scope    store.PermissionScope
}

// New builds a Manager. prompt is invoked when no rule applies; dangerous
// is consulted before any cached allow rule (when it returns true the prompt
// is forced and the matched label flows into Request.Reason).
//
// timeout is the per-prompt deadline; 0 means inherit the ctx the caller
// passes in.
func New(s store.Store, prompt PromptFunc, dangerous func(tool, resource string) (bool, string), timeout time.Duration, defaultDecision Decision) *Manager {
	return &Manager{
		store:           s,
		prompt:          prompt,
		timeout:         timeout,
		dangerous:       dangerous,
		defaultDecision: defaultDecision,
		sessionRules:    map[string][]rule{},
	}
}

// Check returns a final Decision. AllowOnce / AllowSession / AllowPermanent
// mean the call proceeds; Deny / DenyPermanent mean it does not. Internally
// the Manager may issue a prompt and persist a decision (when the user picked
// _permanent or _session) before returning.
func (m *Manager) Check(ctx context.Context, sessionID, tool, resource string) (Decision, error) {
	// 1. DangerousCommandGuard overrides every cached allow. Per spec, even
	// a prior allow_permanent must yield to the live prompt.
	var (
		dangerous bool
		reason    string
	)
	if m.dangerous != nil {
		dangerous, reason = m.dangerous(tool, resource)
	}

	if !dangerous {
		// 2. Check session-scoped rules first; they're cheaper and reflect
		// the user's most recent intent.
		if d, ok := m.matchSessionRule(sessionID, tool, resource); ok {
			return d, nil
		}
		// 3. Check permanent rules from the store.
		if d, ok, err := m.matchPermanentRule(ctx, sessionID, tool, resource); err != nil {
			return Deny, err
		} else if ok {
			return d, nil
		}
	}

	// 4. No cached rule — fall back to configured default if set (unless dangerous).
	if def := m.getDefaultDecision(); !dangerous && def != "" {
		return def, nil
	}

	// 5. No cached rule and no default configured (or dangerous override). Prompt the user.
	return m.promptAndPersist(ctx, sessionID, tool, resource, dangerous, reason)
}

func (m *Manager) matchSessionRule(sessionID, tool, resource string) (Decision, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rules := m.sessionRules[sessionID]
	for i, r := range rules {
		if !matchToolResource(r, tool, resource) {
			continue
		}
		if r.scope == store.ScopeOnce {
			// Pop after consumption.
			m.sessionRules[sessionID] = append(rules[:i], rules[i+1:]...)
		}
		return r.decision, true
	}
	return "", false
}

func (m *Manager) matchPermanentRule(ctx context.Context, sessionID, tool, resource string) (Decision, bool, error) {
	rules, err := m.store.ListPermissions(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("permission: list: %w", err)
	}
	// Deny takes precedence over allow regardless of creation order: a
	// matching deny_permanent must win even if a broader allow_permanent
	// (e.g. an older or less specific preset) was created earlier. Otherwise
	// retightening a permission via a deny rule would be silently ineffective.
	var (
		allow      Decision
		foundAllow bool
	)
	for _, p := range rules {
		if p.Scope != store.ScopePermanent {
			continue
		}
		r := rule{pattern: p.Pattern, tool: p.Tool, decision: p.Decision, scope: p.Scope}
		if !matchToolResource(r, tool, resource) {
			continue
		}
		if p.Decision == DenyPermanent {
			return p.Decision, true, nil
		}
		if !foundAllow {
			allow = p.Decision
			foundAllow = true
		}
	}
	if foundAllow {
		return allow, true, nil
	}
	return "", false, nil
}

func (m *Manager) promptAndPersist(ctx context.Context, sessionID, tool, resource string, dangerous bool, reason string) (Decision, error) {
	if m.prompt == nil {
		// No prompt callback registered — default to deny so the agent loop
		// never assumes silent allow.
		return Deny, nil
	}
	pctx := ctx
	if to := m.getTimeout(); to > 0 {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}
	decision, answered := m.prompt(pctx, Request{
		SessionID: sessionID,
		Tool:      tool,
		Resource:  resource,
		Dangerous: dangerous,
		Reason:    reason,
	})
	if !answered {
		return Deny, ErrTimeout
	}

	// Persist according to scope. AllowOnce means "this call only" — no
	// cached rule of any kind; the next call to the same resource prompts
	// again. AllowSession sticks in memory for the session lifetime.
	switch decision {
	case AllowOnce:
		// nothing to store; let the call proceed.
	case AllowSession:
		m.addSession(sessionID, rule{tool: tool, pattern: resource, decision: decision, scope: store.ScopeSession})
	case AllowPermanent:
		if err := m.savePermanent(pctx, tool, resource, AllowPermanent); err != nil {
			m.addSession(sessionID, rule{tool: tool, pattern: resource, decision: AllowPermanent, scope: store.ScopeSession})
			return AllowPermanent, err
		}
	case DenyPermanent:
		if err := m.savePermanent(pctx, tool, resource, DenyPermanent); err != nil {
			m.addSession(sessionID, rule{tool: tool, pattern: resource, decision: DenyPermanent, scope: store.ScopeSession})
			return DenyPermanent, err
		}
	}
	return decision, nil
}

func (m *Manager) addSession(sessionID string, r rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionRules[sessionID] = append(m.sessionRules[sessionID], r)
}

// GrantSession pre-populates a session-scoped AllowSession rule so the next
// Check for (tool, resource) bypasses the prompt. Used by the adapter-
// generation approval flow: when the user approves a freshly generated
// adapter through agent_setup, the originating session should not see the
// first-invocation permission prompt for that same adapter.
func (m *Manager) GrantSession(sessionID, tool, resource string) {
	if sessionID == "" || tool == "" {
		return
	}
	m.addSession(sessionID, rule{
		tool:     tool,
		pattern:  resource,
		decision: AllowSession,
		scope:    store.ScopeSession,
	})
}

func (m *Manager) savePermanent(ctx context.Context, tool, pattern string, decision Decision) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("permission: rand: %w", err)
	}
	return m.store.SavePermission(ctx, &store.Permission{
		ID:        fmt.Sprintf("perm-%x", b),
		SessionID: "", // permanent = global
		Tool:      tool,
		Pattern:   pattern,
		Decision:  decision,
		Scope:     store.ScopePermanent,
		CreatedAt: time.Now().UTC(),
	})
}

// matchToolResource enforces "tool exact-match + pattern glob".
func matchToolResource(r rule, tool, resource string) bool {
	if r.tool != "" && r.tool != tool {
		return false
	}
	if r.pattern == "" {
		// No pattern means "any resource for this tool".
		return true
	}
	return MatchGlob(r.pattern, resource)
}
