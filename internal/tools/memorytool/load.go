package memorytool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// LoadMemory implements the LoadMemory tool: return the full content of an entry
// by name and record an idempotent best-effort usage hit (hit_count++,
// last_used_at=now) via the usage logger. The hit MUST NOT block or fail the
// result (design D8). Mirrors internal/skills.LoadSkill.
type LoadMemory struct {
	Store *memory.EntryStore
	Usage *memory.UsageLogger
}

// NewLoadMemory constructs a LoadMemory tool.
func NewLoadMemory(store *memory.EntryStore, usage *memory.UsageLogger) *LoadMemory {
	return &LoadMemory{Store: store, Usage: usage}
}

func (LoadMemory) Name() string { return "LoadMemory" }
func (LoadMemory) Description() string {
	return "Load a memory entry's full content by name. Records a usage hit so the entry is favored by curation. Use this (not memory_read) when you actually intend to use the entry."
}
func (LoadMemory) IsReadOnly() bool              { return true }
func (LoadMemory) CanRunInParallel() bool        { return true }
func (LoadMemory) DefaultTimeout() time.Duration { return 10 * time.Second }

func (LoadMemory) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Name of the memory entry to load."}
		},
		"required": ["name"]
	}`)
}

func (l *LoadMemory) Run(ctx context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in nameInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ErrorResultJSON("invalid JSON: " + err.Error()), nil
	}
	if in.Name == "" {
		return tools.ErrorResultJSON("name is required"), nil
	}

	e, err := l.Store.GetByName(ctx, in.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return codedError("not_found", fmt.Sprintf("memory entry %q not found", in.Name), nil), nil
		}
		return tools.ErrorResultJSON(err.Error()), nil
	}

	// Best-effort, non-blocking usage hit (design D8). Never affects the result.
	if l.Usage != nil {
		l.Usage.Bump(e.Name)
	}

	return &tools.Result{Output: e.Content}, nil
}
