package memory

import "fmt"

// ErrMemoryTooLarge is returned when a write would exceed the per-entry content
// character limit. It is the structured rejection surfaced by
// Budgets.CheckEntryContent (and thus by memory_write / memory_merge).
type ErrMemoryTooLarge struct {
	Limit  int
	Actual int
}

func (e ErrMemoryTooLarge) Error() string {
	return fmt.Sprintf("memory: content exceeds char limit (limit=%d, actual=%d)", e.Limit, e.Actual)
}
