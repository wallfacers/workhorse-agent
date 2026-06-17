package memory

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Budgets holds the four code-point limits that govern memory size (design D3).
// All counts are Unicode code points, never bytes.
type Budgets struct {
	PinnedChars       int // P: total content of all pinned entries (default 1500)
	ManifestChars     int // M: the INDEX (manifest) region (default 2000)
	EntryContentChars int // C: a single entry's content (default 1200)
	TriggerChars      int // a single entry's trigger, single line (default 120)
}

// DefaultBudgets returns the spec defaults (design D3 / Character-limit enforcement).
func DefaultBudgets() Budgets {
	return Budgets{
		PinnedChars:       1500,
		ManifestChars:     2000,
		EntryContentChars: 1200,
		TriggerChars:      120,
	}
}

// ErrTriggerInvalid is returned when a trigger contains a newline or exceeds the
// trigger code-point limit. Reason carries a human-readable cause.
type ErrTriggerInvalid struct {
	Reason string
	Limit  int
	Actual int
}

func (e ErrTriggerInvalid) Error() string {
	return fmt.Sprintf("memory: trigger invalid (%s; limit=%d, actual=%d)", e.Reason, e.Limit, e.Actual)
}

// ErrPinnedBudgetExceeded is returned when creating or pinning an entry would
// push the total pinned content over the pinned budget. Defined here for Phase 3
// (memory_write) to surface a `pinned_budget_exceeded` rejection.
type ErrPinnedBudgetExceeded struct {
	Budget int
	Actual int
}

func (e ErrPinnedBudgetExceeded) Error() string {
	return fmt.Sprintf("memory: pinned content exceeds budget (budget=%d, actual=%d)", e.Budget, e.Actual)
}

// CheckEntryContent returns ErrMemoryTooLarge when content exceeds the per-entry
// content limit (code points), reusing the existing error type.
func (b Budgets) CheckEntryContent(content string) error {
	n := utf8.RuneCountInString(content)
	if n > b.EntryContentChars {
		return ErrMemoryTooLarge{Limit: b.EntryContentChars, Actual: n}
	}
	return nil
}

// CheckTrigger rejects a trigger containing a newline or exceeding the trigger
// code-point limit. A trigger MUST be a single short line.
func (b Budgets) CheckTrigger(trigger string) error {
	if strings.ContainsAny(trigger, "\r\n") {
		return ErrTriggerInvalid{Reason: "contains newline", Limit: b.TriggerChars, Actual: utf8.RuneCountInString(trigger)}
	}
	n := utf8.RuneCountInString(trigger)
	if n > b.TriggerChars {
		return ErrTriggerInvalid{Reason: "exceeds length", Limit: b.TriggerChars, Actual: n}
	}
	return nil
}
