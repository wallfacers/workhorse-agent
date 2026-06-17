package curation

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/prompt"
)

// Config holds the curation worker's tunables. EntryCountHigh, MinInterval and
// LeaseTTL are hot-reloadable (design D6 hot-reload subset); the rest are fixed
// at construction (restart-only).
type Config struct {
	EntryCountHigh       int            // pressure water line (hot-reloadable)
	MinInterval          time.Duration  // floor between passes (hot-reloadable)
	LeaseTTL             time.Duration  // leader-lease duration (hot-reloadable)
	ManifestBudgetChars  int            // manifest-size water line (restart-only)
	MaxCandidatesPerPass int            // cap on entries sent to the judge (restart-only)
	ContentSnippetChars  int            // content code points shown to the judge (restart-only)
	Weights              Weights        // scorer weights (restart-only)
	Budgets              memory.Budgets // per-entry limits enforced on merge output (restart-only)
}

// Worker is the background curation maintenance loop (design D5/D6). It owns a
// leader lease and, when it is the elected curator and pressure water lines are
// crossed, runs one bounded pass: score → cluster → LLM judge → apply
// delete/merge. Every failure is fail-safe (WARN + no-op); the store is only
// ever changed by a validated judge decision.
type Worker struct {
	store  *memory.EntryStore
	lease  *Lease
	call   ModelCaller
	logger *slog.Logger
	nowFn  func() time.Time

	// restart-only
	manifestBudget int
	maxCandidates  int
	contentSnippet int
	weights        Weights
	budgets        memory.Budgets

	// hot-reloadable + lastPass, guarded by mu
	mu             sync.Mutex
	entryCountHigh int
	minInterval    time.Duration
	leaseTTL       time.Duration
	lastPass       time.Time

	// trigger is a buffered(1) debounced pressure signal: a pending wake
	// suppresses duplicates so a write burst enqueues at most one pass.
	trigger chan struct{}
}

// NewWorker builds a curation worker over the shared DB. call may be nil, in
// which case the worker is inert (no provider configured) — Notify/Start are
// safe no-ops, so curation simply does not run.
func NewWorker(store *memory.EntryStore, db *sql.DB, call ModelCaller, cfg Config, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		store:          store,
		lease:          NewLease(db),
		call:           call,
		logger:         logger,
		nowFn:          func() time.Time { return time.Now().UTC() },
		manifestBudget: cfg.ManifestBudgetChars,
		maxCandidates:  cfg.MaxCandidatesPerPass,
		contentSnippet: cfg.ContentSnippetChars,
		weights:        cfg.Weights,
		budgets:        cfg.Budgets,
		entryCountHigh: cfg.EntryCountHigh,
		minInterval:    cfg.MinInterval,
		leaseTTL:       cfg.LeaseTTL,
		trigger:        make(chan struct{}, 1),
	}
}

// SetHotConfig updates the hot-reloadable knobs (design D6). All other config is
// restart-only and is ignored here.
func (w *Worker) SetHotConfig(entryCountHigh int, minInterval, leaseTTL time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entryCountHigh = entryCountHigh
	w.minInterval = minInterval
	w.leaseTTL = leaseTTL
}

// Notify is the pressure trigger (design D5): called off the request hot path
// after a successful write. It is a non-blocking, debounced signal — a pending
// wake absorbs the send. Safe on a nil/inert worker.
func (w *Worker) Notify() {
	if w == nil || w.call == nil {
		return
	}
	select {
	case w.trigger <- struct{}{}:
	default: // a pass is already pending
	}
}

// Start launches the background loop until ctx is cancelled. It is a no-op on an
// inert worker (no judge model configured).
func (w *Worker) Start(ctx context.Context) {
	if w.call == nil {
		w.logger.Debug("curation worker inert (no judge model configured)")
		return
	}
	go w.run(ctx)
}

func (w *Worker) run(ctx context.Context) {
	for {
		timer := time.NewTimer(w.snapshotMinInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.trigger:
			timer.Stop()
		case <-timer.C:
		}
		w.RunPass(ctx)
	}
}

// RunPass evaluates the water lines and, if a pass is warranted and the leader
// lease is won, runs exactly one bounded curation pass. Exposed so tests can
// drive a pass deterministically without the timer loop. Errors are absorbed
// (fail-safe) — RunPass never panics or propagates.
func (w *Worker) RunPass(ctx context.Context) {
	now := w.nowFn()
	if !w.shouldRun(ctx, now) {
		return
	}

	ttl := w.snapshotLeaseTTL()
	held, err := w.lease.Acquire(ctx, now, ttl)
	if err != nil {
		w.logger.Warn("curation: lease acquire failed", "error", err)
		return
	}
	if !held {
		w.logger.Debug("curation: another process holds the lease; skipping pass")
		return
	}
	// Release uses a background context so a cancelled parent still frees the
	// lease + in-process backstop. Ordered to run AFTER cancel() (LIFO).
	defer func() { _ = w.lease.Release(context.Background()) }()

	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.heartbeat(passCtx, cancel, ttl)

	if err := w.curate(passCtx, now); err != nil {
		w.logger.Warn("curation: pass failed (fail-safe no-op)", "error", err)
	}
	w.setLastPass(now)
}

// heartbeat renews the lease every ttl/3 while the pass runs. A failed renewal
// (lease stolen after expiry) cancels the pass via cancel().
func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelFunc, ttl time.Duration) {
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := w.lease.Renew(context.Background(), w.nowFn(), w.snapshotLeaseTTL())
			if err != nil || !ok {
				w.logger.Warn("curation: lease renewal lost; aborting pass", "error", err)
				cancel()
				return
			}
		}
	}
}

// shouldRun applies the three water lines from the curation spec ("Pressure-
// triggered curation"): a pass runs when the non-pinned count exceeds
// entry_count_high, OR the estimated manifest size exceeds manifest_budget_chars,
// OR the time since the last completed pass exceeds min_interval_minutes
// (time-based fallback so stale volatile entries are still reviewed in a small
// store). An empty non-pinned set short-circuits to false (nothing to review).
// Bursts are bounded by the debounced enqueue + max_candidates_per_pass, not by
// a hard time floor.
func (w *Worker) shouldRun(ctx context.Context, now time.Time) bool {
	w.mu.Lock()
	last := w.lastPass
	minInterval := w.minInterval
	high := w.entryCountHigh
	w.mu.Unlock()

	count, err := w.store.CountNonPinned(ctx)
	if err != nil {
		w.logger.Warn("curation: count failed", "error", err)
		return false
	}
	if count == 0 {
		return false // nothing to curate
	}
	if count > high {
		return true // count water line
	}
	// Time-based fallback: never run, or long enough since the last pass.
	if last.IsZero() || now.Sub(last) >= minInterval {
		return true
	}
	// Manifest-size water line: long triggers can blow the manifest budget even
	// when the count is under the high-water mark.
	if w.manifestBudget > 0 {
		est, err := w.store.ManifestSizeEstimate(ctx)
		if err != nil {
			w.logger.Warn("curation: manifest size estimate failed", "error", err)
		} else if est > w.manifestBudget {
			return true
		}
	}
	return false
}

// curate runs the deterministic selection then the LLM judge and applies the
// verdict. Returns an error to RunPass, which logs it fail-safe.
func (w *Worker) curate(ctx context.Context, now time.Time) error {
	entries, err := w.store.List(ctx)
	if err != nil {
		return err
	}
	candidates := RankCandidates(entries, w.weights, now) // non-pinned, ascending
	if len(candidates) == 0 {
		return nil
	}
	if w.maxCandidates > 0 && len(candidates) > w.maxCandidates {
		deferred := len(candidates) - w.maxCandidates
		candidates = candidates[:w.maxCandidates]
		w.logger.Debug("curation: candidates capped", "sent", w.maxCandidates, "deferred", deferred)
	}

	// Cluster only the capped candidate set: cost is O(cap²) (cap ≤ ~20), so the
	// store-wide FTS pre-filter (a no-op at this scale) is unnecessary — the
	// Cluster API still accepts one for a future store-wide dedup pass.
	capEntries := make([]*memory.Entry, len(candidates))
	for i, c := range candidates {
		capEntries[i] = c.Entry
	}
	clusters := Cluster(capEntries, DefaultJaccardThreshold, nil)

	decision, err := Judge(ctx, w.call, w.buildJudgeCandidates(candidates, now), buildJudgeClusters(clusters))
	if err != nil {
		return err
	}
	return w.apply(ctx, decision, entries)
}

// buildJudgeCandidates converts scored candidates into the prompt's plain DTOs,
// truncating content to ContentSnippetChars code points.
func (w *Worker) buildJudgeCandidates(candidates []Candidate, now time.Time) []prompt.CurationJudgeCandidate {
	out := make([]prompt.CurationJudgeCandidate, len(candidates))
	for i, c := range candidates {
		e := c.Entry
		out[i] = prompt.CurationJudgeCandidate{
			Name:       e.Name,
			Trigger:    e.Trigger,
			Content:    snippet(e.Content, w.contentSnippet),
			Durability: e.Durability,
			Category:   e.Category,
			HitCount:   e.HitCount,
			AgeDays:    int(daysBetween(e.CreatedAt, now)),
			Score:      c.Score,
		}
	}
	return out
}

func buildJudgeClusters(clusters [][]*memory.Entry) []prompt.CurationJudgeCluster {
	out := make([]prompt.CurationJudgeCluster, 0, len(clusters))
	for _, cl := range clusters {
		names := make([]string, len(cl))
		for i, e := range cl {
			names[i] = e.Name
		}
		out = append(out, prompt.CurationJudgeCluster{Names: names})
	}
	return out
}

// apply enacts a validated judge decision: merges first (so a name consumed by a
// merge is not also separately evicted), then evictions. Every name is validated
// against the live entry set; pinned entries and unknown names are skipped with a
// WARN — the judge can never delete a pinned memory or invent a name.
func (w *Worker) apply(ctx context.Context, d *JudgeDecision, entries []*memory.Entry) error {
	byName := make(map[string]*memory.Entry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	consumed := make(map[string]bool)

	merged := 0
	for _, m := range d.Merge {
		into, names, ok := w.validateMerge(m, byName)
		if !ok {
			continue
		}
		if err := w.store.Merge(ctx, names, into); err != nil {
			w.logger.Warn("curation: merge failed", "into", into.Name, "error", err)
			continue
		}
		for _, n := range names {
			consumed[n] = true
		}
		consumed[into.Name] = false // the survivor stays
		merged++
	}

	evicted := 0
	for _, name := range d.Evict {
		if consumed[name] {
			continue // already removed by a merge
		}
		e, ok := byName[name]
		if !ok {
			w.logger.Warn("curation: judge named an unknown entry to evict; skipping", "name", name)
			continue
		}
		if e.Pinned {
			w.logger.Warn("curation: judge tried to evict a pinned entry; refusing", "name", name)
			continue
		}
		if err := w.store.Delete(ctx, name); err != nil {
			w.logger.Warn("curation: delete failed", "name", name, "error", err)
			continue
		}
		evicted++
	}

	if merged > 0 || evicted > 0 {
		w.logger.Info("curation: pass applied", "merged", merged, "evicted", evicted)
	}
	return nil
}

// validateMerge checks the judge's merge against the live store and the per-entry
// budgets, returning the entry to write plus the source names to delete. ok is
// false (with a WARN) when the merge references a pinned/unknown entry, is empty,
// or its merged content/trigger exceed the limits.
func (w *Worker) validateMerge(m MergeDecision, byName map[string]*memory.Entry) (*memory.Entry, []string, bool) {
	if m.Into.Name == "" || len(m.Names) < 2 {
		w.logger.Warn("curation: ill-formed merge decision; skipping", "into", m.Into.Name, "names", m.Names)
		return nil, nil, false
	}
	for _, n := range m.Names {
		e, ok := byName[n]
		if !ok {
			w.logger.Warn("curation: merge names an unknown entry; skipping", "name", n)
			return nil, nil, false
		}
		if e.Pinned {
			w.logger.Warn("curation: merge would consume a pinned entry; refusing", "name", n)
			return nil, nil, false
		}
	}
	if err := w.budgets.CheckEntryContent(m.Into.Content); err != nil {
		w.logger.Warn("curation: merged content over budget; skipping", "into", m.Into.Name, "error", err)
		return nil, nil, false
	}
	if err := w.budgets.CheckTrigger(m.Into.Trigger); err != nil {
		w.logger.Warn("curation: merged trigger invalid; skipping", "into", m.Into.Name, "error", err)
		return nil, nil, false
	}
	durability := m.Into.Durability
	if durability != durabilityEvergreen && durability != durabilityVolatile {
		durability = durabilityVolatile
	}
	into := &memory.Entry{
		Name:       m.Into.Name,
		Trigger:    m.Into.Trigger,
		Content:    m.Into.Content,
		Durability: durability,
		Category:   m.Into.Category,
		CharCount:  memory.CharCount(m.Into.Content),
	}
	return into, m.Names, true
}

func (w *Worker) snapshotMinInterval() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.minInterval <= 0 {
		return time.Minute
	}
	return w.minInterval
}

func (w *Worker) snapshotLeaseTTL() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.leaseTTL <= 0 {
		return 60 * time.Second
	}
	return w.leaseTTL
}

func (w *Worker) setLastPass(t time.Time) {
	w.mu.Lock()
	w.lastPass = t
	w.mu.Unlock()
}

// snippet truncates s to n code points (n<=0 means no limit), appending an
// ellipsis when truncated.
func snippet(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
