package curation

import (
	"math"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

var defaultWeights = Weights{Hit: 1.0, Recency: 1.0, Age: 0.5, Volatility: 0.5}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return v.UTC()
}

func ptr(t time.Time) *time.Time { return &t }

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNorm(t *testing.T) {
	approx(t, norm(0), 0)
	approx(t, norm(-5), 0)
	approx(t, norm(1), 0.5)
	approx(t, norm(9), 0.9)
}

func TestRecency(t *testing.T) {
	now := ts(t, "2026-06-17T00:00:00Z")
	// nil → 0.
	approx(t, recency(nil, now), 0)
	// zero time → 0.
	zero := time.Time{}
	approx(t, recency(&zero, now), 0)
	// used now → 1/(1+0) = 1.
	approx(t, recency(ptr(now), now), 1)
	// used 9 days ago → 1/(1+9) = 0.1.
	approx(t, recency(ptr(now.AddDate(0, 0, -9)), now), 0.1)
	// future last-used (clock skew) clamps to 0 days → 1.
	approx(t, recency(ptr(now.AddDate(0, 0, 5)), now), 1)
}

func TestAgePenalty(t *testing.T) {
	now := ts(t, "2026-06-17T00:00:00Z")
	// evergreen at 365 days → exactly 1.0 (saturated).
	approx(t, agePenalty(now.AddDate(0, 0, -365), durabilityEvergreen, now), 1.0)
	// evergreen beyond horizon stays clamped at 1.0.
	approx(t, agePenalty(now.AddDate(0, 0, -1000), durabilityEvergreen, now), 1.0)
	// volatile at 90 days → 1.0.
	approx(t, agePenalty(now.AddDate(0, 0, -90), durabilityVolatile, now), 1.0)
	// volatile at 45 days → 0.5.
	approx(t, agePenalty(now.AddDate(0, 0, -45), durabilityVolatile, now), 0.5)
	// future created_at clamps to 0.
	approx(t, agePenalty(now.AddDate(0, 0, 10), durabilityVolatile, now), 0)
	// volatile decays ~4× steeper than evergreen at the same age.
	const ageDays = 45
	ev := agePenalty(now.AddDate(0, 0, -ageDays), durabilityEvergreen, now)
	vo := agePenalty(now.AddDate(0, 0, -ageDays), durabilityVolatile, now)
	ratio := vo / ev
	if ratio < 3.0 || ratio > 5.0 {
		t.Fatalf("volatile/evergreen age-penalty ratio = %v, want 3-5×", ratio)
	}
}

func TestVolatilityPenalty(t *testing.T) {
	approx(t, volatilityPenalty(durabilityEvergreen), 0)
	approx(t, volatilityPenalty(durabilityVolatile), 0.3)
	approx(t, volatilityPenalty(""), 0) // unknown defaults to evergreen-like (no penalty)
}

func TestScoreComposition(t *testing.T) {
	now := ts(t, "2026-06-17T00:00:00Z")
	e := &memory.Entry{
		HitCount:   1,                          // norm = 0.5
		LastUsedAt: ptr(now.AddDate(0, 0, -9)), // recency = 0.1
		CreatedAt:  now.AddDate(0, 0, -45),     // volatile age = 0.5
		Durability: durabilityVolatile,         // volatility penalty = 0.3
	}
	// 1.0*0.5 + 1.0*0.1 − 0.5*0.5 − 0.5*0.3 = 0.5 + 0.1 − 0.25 − 0.15 = 0.2
	approx(t, Score(e, defaultWeights, now), 0.2)
}

func TestScoreDeterministic(t *testing.T) {
	now := ts(t, "2026-06-17T00:00:00Z")
	e := &memory.Entry{HitCount: 3, LastUsedAt: ptr(now.AddDate(0, 0, -2)), CreatedAt: now.AddDate(0, 0, -10), Durability: durabilityEvergreen}
	first := Score(e, defaultWeights, now)
	for i := 0; i < 100; i++ {
		if Score(e, defaultWeights, now) != first {
			t.Fatalf("score not deterministic on iteration %d", i)
		}
	}
}

func TestRankCandidatesExcludesPinnedAndSortsAscending(t *testing.T) {
	now := ts(t, "2026-06-17T00:00:00Z")
	entries := []*memory.Entry{
		{Name: "pinned-user", Pinned: true, HitCount: 0, CreatedAt: now.AddDate(0, 0, -300), Durability: durabilityEvergreen},
		{Name: "hot", HitCount: 50, LastUsedAt: ptr(now), CreatedAt: now.AddDate(0, 0, -1), Durability: durabilityEvergreen},
		{Name: "cold-volatile", HitCount: 0, CreatedAt: now.AddDate(0, 0, -89), Durability: durabilityVolatile},
		{Name: "warm", HitCount: 5, LastUsedAt: ptr(now.AddDate(0, 0, -10)), CreatedAt: now.AddDate(0, 0, -30), Durability: durabilityVolatile},
	}
	got := RankCandidates(entries, defaultWeights, now)
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates (pinned excluded), got %d", len(got))
	}
	for _, c := range got {
		if c.Entry.Pinned {
			t.Fatalf("pinned entry %q must not be a candidate", c.Entry.Name)
		}
	}
	// Ascending by score: most evictable (lowest) first.
	for i := 1; i < len(got); i++ {
		if got[i-1].Score > got[i].Score {
			t.Fatalf("not ascending at %d: %v > %v", i, got[i-1].Score, got[i].Score)
		}
	}
	// The cold volatile entry should be the most evictable; the hot one the least.
	if got[0].Entry.Name != "cold-volatile" {
		t.Fatalf("expected cold-volatile most evictable, got %q", got[0].Entry.Name)
	}
	if got[len(got)-1].Entry.Name != "hot" {
		t.Fatalf("expected hot least evictable, got %q", got[len(got)-1].Entry.Name)
	}
}

func TestRankCandidatesTiebreakByName(t *testing.T) {
	now := ts(t, "2026-06-17T00:00:00Z")
	// Two identical-metric entries → tie broken by name ascending.
	mk := func(name string) *memory.Entry {
		return &memory.Entry{Name: name, HitCount: 1, LastUsedAt: ptr(now), CreatedAt: now.AddDate(0, 0, -1), Durability: durabilityEvergreen}
	}
	got := RankCandidates([]*memory.Entry{mk("zebra"), mk("alpha")}, defaultWeights, now)
	if got[0].Entry.Name != "alpha" || got[1].Entry.Name != "zebra" {
		t.Fatalf("tie not broken by name: %q, %q", got[0].Entry.Name, got[1].Entry.Name)
	}
}
