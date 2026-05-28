// Package approval owns the in-memory lifecycle of pending adapter-generation
// approvals: registration with a TTL, user decisions (approve / reject /
// edit), and the timer-driven expiry that removes stale entries.
//
// State is process-local. A server restart cancels every pending approval —
// the user re-triggers agent_setup. This is documented in
// add-llm-adapter-generator design.md G7.
//
// The manager calls out to three optional collaborators when a decision
// arrives: a Publisher (writes the draft to the live dir + .genmeta), a
// RegistryInjector (makes the new adapter visible in the originating
// session's snapshot), and a DedupClearer (signals the implicit-trigger
// dedup map that this name is now retry-able). All three are nil-safe so
// tests can mock them independently — see manager_test.go.
package approval

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/idgen"
)

// Decision is the user's verdict on a pending approval.
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
	DecisionEdit    Decision = "edit"
)

// SmokeOutcome describes a smoke-test result in the approval payload. Kept
// minimal so it doesn't bind us to the smoke package's struct (which lives
// outside this dependency footprint).
type SmokeOutcome struct {
	Passed     bool
	Stdout     string
	Stderr     string
	ExitCode   int
	DurationMS int64
	Reason     string // "ok", "expected_substring_missing", "output_format_mismatch", "timeout", etc.
}

// Provenance carries the metadata that distinguishes an llm_generated adapter
// from a hand-written one. Stored alongside the draft and surfaced in the
// approval payload. The audit-trail fields (Binary, Prompt, HelpOutput,
// VersionOutput, ManOutput) flow into the .genmeta sibling so an operator can
// reproduce the generation context after the fact.
type Provenance struct {
	GeneratedBy   string // model id, e.g. "anthropic:claude-opus-4-7"
	GeneratedAt   time.Time
	ToolVersion   string // raw "<bin> --version" output (may be empty per G3)
	Binary        string // resolved absolute path of the analyzed binary
	Prompt        string // rendered AdapterGeneration prompt
	HelpOutput    string // captured `<bin> --help`
	VersionOutput string // captured `<bin> --version`
	ManOutput     string // captured `man <bin>` (may be empty)
}

// PendingApproval is the in-memory record. All fields are read-only after
// Register — Decide may mutate Smoke/DraftYAML via the edit path, but only
// holds the lock briefly.
type PendingApproval struct {
	ID               string
	SessionID        string
	AgentName        string
	DraftPath        string
	DraftYAML        string
	PriorYAML        string // populated when regenerate=true and an existing adapter exists
	Diff             string // unified diff vs PriorYAML; empty if no prior
	Smoke            SmokeOutcome
	AdapterSmokePass bool // pre-publish smoke result; propagated onto the injected Adapter so IsHealthy() is true post-approval
	Provenance       Provenance
	ExpiresAt        time.Time
}

// EventEmitter abstracts the SSE/event layer so the manager doesn't import
// internal/api. The runner factory wires a real emitter at startup.
type EventEmitter interface {
	EmitApprovalEvent(sessionID, eventType string, payload any)
}

// Publisher writes the approved draft to the live dir + .genmeta. Returns the
// final published path on success. When the rename succeeded but the sibling
// .genmeta write failed, the implementation MUST return a nil error and log
// internally — Decide treats publish errors as fatal and would otherwise
// leak the pending approval into the timer-driven expire path despite the
// adapter being live (see add-llm-adapter-generator G9).
type Publisher interface {
	Publish(draftPath string, prov Provenance) (string, error)
}

// RegistryInjector adds a newly published adapter to a live session's snapshot
// so the originating session can immediately retry without waiting for a new
// session to pick up the change. The publishedPath argument carries the live
// dir + adapter filename so the injector can re-read the just-published YAML
// (with the SmokePassed flag set from the pre-approval smoke result).
type RegistryInjector interface {
	Inject(sessionID, adapterName, publishedPath string, smokePassed bool)
}

// EditValidator gates a DecisionEdit before the manager writes the edited
// YAML to disk. The hook is invoked under the manager mutex; implementations
// must avoid any callback that re-enters the manager. Returning a non-nil
// error rejects the edit and leaves the prior draft untouched.
type EditValidator interface {
	ValidateEdit(agentName string, editedYAML []byte) error
}

// DedupClearer signals the implicit-trigger dedup map about state changes.
// Clear → entry deleted (next retry succeeds), called on approve.
// MarkUnavailable → entry transitions to "unavailable" (subsequent retries
// return the rejected/expired message), called on reject and expire.
type DedupClearer interface {
	ClearImplicitTriggerDedup(sessionID, agentName string)
	MarkAdapterSetupUnavailable(sessionID, agentName, reason string)
}

// MarkApprover records an adapter as already-approved in the originating
// session so the per-session first-invocation prompt doesn't fire — see
// the modified external-agents spec, "First-invocation approval" requirement.
type MarkApprover interface {
	MarkApproved(sessionID, adapterName string)
}

// Manager owns the pending approvals map and the expiry timers. Construct
// one per server; safe for concurrent use.
type Manager struct {
	mu        sync.Mutex
	pending   map[string]*PendingApproval
	timers    map[string]*time.Timer
	timeout   time.Duration
	emitter   EventEmitter
	pub       Publisher
	injector  RegistryInjector
	dedup     DedupClearer
	marker    MarkApprover
	validator EditValidator
}

// Options bundles the optional collaborators so callers can supply only what
// they have wired up.
type Options struct {
	Timeout          time.Duration
	Emitter          EventEmitter
	Publisher        Publisher
	RegistryInjector RegistryInjector
	DedupClearer     DedupClearer
	MarkApprover     MarkApprover
	EditValidator    EditValidator
}

// SetEmitter wires (or replaces) the event emitter at any time after
// construction. Useful when the emitter depends on collaborators that aren't
// available at New time — e.g. the session manager, which itself depends on
// the approval manager via the runner factory.
func (m *Manager) SetEmitter(e EventEmitter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emitter = e
}

// SetPublisher wires (or replaces) the publish hook.
func (m *Manager) SetPublisher(p Publisher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pub = p
}

// SetRegistryInjector wires the per-session registry injection hook.
func (m *Manager) SetRegistryInjector(i RegistryInjector) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.injector = i
}

// SetDedupClearer wires the implicit-trigger dedup clearer.
func (m *Manager) SetDedupClearer(c DedupClearer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dedup = c
}

// SetMarkApprover wires the per-session approved-set marker.
func (m *Manager) SetMarkApprover(a MarkApprover) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marker = a
}

// SetEditValidator wires the DecisionEdit re-validation hook.
func (m *Manager) SetEditValidator(v EditValidator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validator = v
}

// New constructs a Manager. A zero or negative Timeout falls back to five
// minutes — the spec default.
func New(opts Options) *Manager {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &Manager{
		pending:   map[string]*PendingApproval{},
		timers:    map[string]*time.Timer{},
		timeout:   timeout,
		emitter:   opts.Emitter,
		pub:       opts.Publisher,
		injector:  opts.RegistryInjector,
		dedup:     opts.DedupClearer,
		marker:    opts.MarkApprover,
		validator: opts.EditValidator,
	}
}

// Register stores a new PendingApproval, fills in its ID/ExpiresAt, schedules
// the expiry timer, emits the adapter_approval_request event, and returns
// the assigned ID. The caller must have populated SessionID, AgentName,
// DraftPath, DraftYAML, PriorYAML (or ""), Diff (or ""), Smoke, Provenance.
func (m *Manager) Register(p *PendingApproval) string {
	if p == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		p.ID = idgen.NewULID()
	}
	p.ExpiresAt = time.Now().Add(m.timeout)
	m.pending[p.ID] = p

	m.timers[p.ID] = time.AfterFunc(m.timeout, func() {
		m.expire(p.ID)
	})

	m.emitRequestLocked(p)
	return p.ID
}

// Get returns a snapshot of the pending approval, or nil if it doesn't exist.
// The returned value is a copy — callers can read it without holding the lock.
func (m *Manager) Get(id string) *PendingApproval {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[id]
	if !ok {
		return nil
	}
	cp := *p
	return &cp
}

// Decide applies the user's verdict to a pending approval.
//
// approve: invokes Publisher, then RegistryInjector, then MarkApprover,
//
//	then DedupClearer (all nil-safe), then removes the entry.
//
// reject:  deletes the draft file, signals DedupClearer (so retries return
//
//	"rejected" instead of re-triggering), removes the entry.
//
// edit:    requires editedYAML; updates DraftYAML on disk and in-memory but
//
//	leaves the entry pending. Publisher is NOT invoked — the caller
//	should call Decide again with "approve" once the user confirms.
//
// Returns ErrNotFound if the id is unknown (e.g. already expired) and other
// errors verbatim from the collaborator interfaces.
var ErrNotFound = errors.New("approval: not found")

func (m *Manager) Decide(id string, decision Decision, editedYAML string) error {
	m.mu.Lock()
	p, ok := m.pending[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	switch decision {
	case DecisionApprove:
		// We hold the lock while delegating to Publisher to keep the
		// "approve is atomic in the manager's view" invariant — a second
		// Decide call on the same id would block until this completes.
		published, err := m.publishLocked(p)
		if err != nil {
			// Publish failed before the atomic rename — draft is still in
			// .drafts/, no live-side artifact exists. Remove the entry and
			// return the error; the timer would have done the same thing
			// after the timeout otherwise, but doing it here avoids the
			// spurious "expired" event.
			m.markUnavailableDedupLocked(p, "publish_failed")
			m.emitResolvedLocked(p, "rejected", "")
			m.removeLocked(id)
			m.mu.Unlock()
			return err
		}
		m.injectLocked(p, published)
		m.markApprovedLocked(p)
		m.clearDedupLocked(p)
		m.emitResolvedLocked(p, "approved", published)
		m.removeLocked(id)
		m.mu.Unlock()
		return nil
	case DecisionReject:
		_ = os.Remove(p.DraftPath)
		m.markUnavailableDedupLocked(p, "rejected")
		m.emitResolvedLocked(p, "rejected", "")
		m.removeLocked(id)
		m.mu.Unlock()
		return nil
	case DecisionEdit:
		if editedYAML == "" {
			m.mu.Unlock()
			return errors.New("approval: edit decision requires non-empty edited_yaml")
		}
		// Re-validate before persisting: schema, dangerous-arg scan, and
		// (per task 7.3) smoke. Validation is delegated so the manager
		// stays free of extagent / smoke imports — the agentsetup runner
		// factory wires the validator.
		if m.validator != nil {
			if err := m.validator.ValidateEdit(p.AgentName, []byte(editedYAML)); err != nil {
				m.mu.Unlock()
				return fmt.Errorf("approval: edit rejected: %w", err)
			}
		}
		p.DraftYAML = editedYAML
		if err := os.WriteFile(p.DraftPath, []byte(editedYAML), 0o600); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("approval: write edited draft: %w", err)
		}
		m.mu.Unlock()
		return nil
	default:
		m.mu.Unlock()
		return fmt.Errorf("approval: unknown decision %q", decision)
	}
}

// expire is the timer callback. We re-check the entry's presence to handle
// the race where Decide ran before the timer fired.
func (m *Manager) expire(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[id]
	if !ok {
		return
	}
	_ = os.Remove(p.DraftPath)
	m.markUnavailableDedupLocked(p, "expired")
	m.emitResolvedLocked(p, "expired", "")
	m.removeLocked(id)
}

// publishLocked invokes the Publisher. Must be called with m.mu held.
func (m *Manager) publishLocked(p *PendingApproval) (string, error) {
	if m.pub == nil {
		return p.DraftPath, nil // tests with no publisher: treat as a no-op success
	}
	return m.pub.Publish(p.DraftPath, p.Provenance)
}

func (m *Manager) injectLocked(p *PendingApproval, publishedPath string) {
	if m.injector == nil {
		return
	}
	m.injector.Inject(p.SessionID, p.AgentName, publishedPath, p.AdapterSmokePass)
}

func (m *Manager) markApprovedLocked(p *PendingApproval) {
	if m.marker == nil {
		return
	}
	m.marker.MarkApproved(p.SessionID, p.AgentName)
}

func (m *Manager) clearDedupLocked(p *PendingApproval) {
	if m.dedup == nil {
		return
	}
	m.dedup.ClearImplicitTriggerDedup(p.SessionID, p.AgentName)
}

// markUnavailableDedupLocked is called on reject/expire so the per-session
// dedup map transitions to "unavailable" instead of being cleared. A cleared
// entry would let the next retry re-trigger generation against a binary the
// user just said no to — wrong UX.
func (m *Manager) markUnavailableDedupLocked(p *PendingApproval, reason string) {
	if m.dedup == nil {
		return
	}
	m.dedup.MarkAdapterSetupUnavailable(p.SessionID, p.AgentName, reason)
}

func (m *Manager) emitRequestLocked(p *PendingApproval) {
	if m.emitter == nil {
		return
	}
	payload := map[string]any{
		"approval_id":        p.ID,
		"type":               "adapter_generation",
		"agent_name":         p.AgentName,
		"draft_yaml":         p.DraftYAML,
		"prior_yaml":         p.PriorYAML,
		"diff_against_prior": p.Diff,
		"smoke_result": map[string]any{
			"passed":      p.Smoke.Passed,
			"stdout":      p.Smoke.Stdout,
			"stderr":      p.Smoke.Stderr,
			"exit_code":   p.Smoke.ExitCode,
			"duration_ms": p.Smoke.DurationMS,
			"reason":      p.Smoke.Reason,
		},
		"provenance": map[string]any{
			"generated_by": p.Provenance.GeneratedBy,
			"generated_at": p.Provenance.GeneratedAt.Format(time.RFC3339),
			"tool_version": p.Provenance.ToolVersion,
		},
		"expires_at": p.ExpiresAt.Format(time.RFC3339),
	}
	m.emitter.EmitApprovalEvent(p.SessionID, "adapter_approval_request", payload)
}

func (m *Manager) emitResolvedLocked(p *PendingApproval, status, publishedPath string) {
	if m.emitter == nil {
		return
	}
	eventType := "adapter_approval_resolved"
	if status == "expired" {
		eventType = "adapter_approval_expired"
	}
	payload := map[string]any{
		"approval_id":    p.ID,
		"agent_name":     p.AgentName,
		"status":         status,
		"published_path": publishedPath,
	}
	m.emitter.EmitApprovalEvent(p.SessionID, eventType, payload)
}

func (m *Manager) removeLocked(id string) {
	delete(m.pending, id)
	if t, ok := m.timers[id]; ok {
		t.Stop()
		delete(m.timers, id)
	}
}

// Cancel stops every running timer and discards every pending approval.
// Intended for use at server shutdown so goroutines don't outlive the process.
func (m *Manager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.pending {
		m.removeLocked(id)
	}
}
