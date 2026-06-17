// Package curation implements the deterministic, zero-token half of the memory
// curation engine (design D5): the eviction scorer and the near-duplicate
// clustering. The runtime half (leader lease, pressure trigger, background
// worker, LLM judge) builds on these pure functions and lives alongside them.
//
// The scorer is a pure function of an entry's metadata and a reference "now":
// higher score = more worth keeping, lower = more evictable. The same scorer
// drives both manifest survival ordering (memory.Loader.ScoreFn) and curation
// eviction ranking, so a memory that survives the manifest is exactly one the
// curator would keep — one ranking, no divergence.
package curation

import (
	"sort"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

// Durability values (mirrors the durability column in memory_entries, D1).
const (
	durabilityEvergreen = "evergreen"
	durabilityVolatile  = "volatile"
)

// Age-penalty horizons in days (design D5): the number of days at which the age
// penalty saturates to 1.0. Volatile decays ~4× steeper than evergreen.
const (
	evergreenAgeDays = 365.0
	volatileAgeDays  = 90.0
)

// volatilePenalty is the flat baseline penalty applied to volatile entries,
// distinct from the age term (design D5: evergreen 0.0, volatile 0.3).
const volatilePenalty = 0.3

// Weights are the four configurable scorer weights (design D5 default
// hit=1.0, recency=1.0, age=0.5, volatility=0.5). Kept local to this package so
// it does not import config; the caller converts from config.MemoryWeights.
type Weights struct {
	Hit        float64
	Recency    float64
	Age        float64
	Volatility float64
}

// Score returns the keep-score of a non-pinned entry given the weights and a
// reference time. Higher = keep, lower = evict (design D5):
//
//	score = w_hit·norm(hit) + w_recency·recency(last_used)
//	        − w_age·age_penalty(created, durability) − w_volatility·volatility_penalty(durability)
//
// It does not special-case pinned entries — callers exclude pinned before
// scoring (pinned entries are never evicted). Pass the same now for every entry
// in a pass so the ranking is internally consistent.
func Score(e *memory.Entry, w Weights, now time.Time) float64 {
	return w.Hit*norm(e.HitCount) +
		w.Recency*recency(e.LastUsedAt, now) -
		w.Age*agePenalty(e.CreatedAt, e.Durability, now) -
		w.Volatility*volatilityPenalty(e.Durability)
}

// norm maps a hit count into a saturating [0,1): hit/(hit+1). One hit → 0.5,
// nine hits → 0.9; diminishing returns so a runaway counter cannot dominate.
func norm(hitCount int) float64 {
	if hitCount <= 0 {
		return 0
	}
	h := float64(hitCount)
	return h / (h + 1)
}

// recency maps last-used to (0,1], decaying with age of last use:
// 1/(1+days_since_last_use). A nil last_used_at (never loaded) → 0. A timestamp
// in the future (clock skew) is clamped to "0 days since" → 1.
func recency(lastUsed *time.Time, now time.Time) float64 {
	if lastUsed == nil || lastUsed.IsZero() {
		return 0
	}
	days := daysBetween(*lastUsed, now)
	return 1.0 / (1.0 + days)
}

// agePenalty grows linearly with the entry's age, saturating at 1.0 once the
// entry reaches its durability horizon D (365d evergreen, 90d volatile):
// min(days_since_created / D, 1.0). Future created_at clamps to 0.
func agePenalty(created time.Time, durability string, now time.Time) float64 {
	d := evergreenAgeDays
	if durability == durabilityVolatile {
		d = volatileAgeDays
	}
	days := daysBetween(created, now)
	p := days / d
	if p > 1.0 {
		return 1.0
	}
	return p
}

// volatilityPenalty is the flat per-durability baseline (evergreen 0, volatile 0.3).
func volatilityPenalty(durability string) float64 {
	if durability == durabilityVolatile {
		return volatilePenalty
	}
	return 0
}

// daysBetween returns the non-negative number of days from t to now (fractional).
// A t after now (clock skew / future timestamp) yields 0.
func daysBetween(t, now time.Time) float64 {
	d := now.Sub(t)
	if d <= 0 {
		return 0
	}
	return d.Hours() / 24.0
}

// Candidate pairs a non-pinned entry with its computed keep-score.
type Candidate struct {
	Entry *memory.Entry
	Score float64
}

// RankCandidates scores every non-pinned entry and returns them ordered by
// score ascending (lowest/most-evictable first), with name ascending as a
// deterministic tiebreaker. Pinned entries are excluded entirely (never
// evicted). The returned slice is the eviction candidate list the LLM judge
// consumes head-first under its per-pass cap.
func RankCandidates(entries []*memory.Entry, w Weights, now time.Time) []Candidate {
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		if e.Pinned {
			continue
		}
		out = append(out, Candidate{Entry: e, Score: Score(e, w, now)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score // lowest score (most evictable) first
		}
		return out[i].Entry.Name < out[j].Entry.Name
	})
	return out
}
