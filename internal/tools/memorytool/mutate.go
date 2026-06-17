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

// ---- memory_delete ----------------------------------------------------------

// Delete implements the memory_delete tool: remove one entry by name.
type Delete struct {
	Store *memory.EntryStore
}

func (Delete) Name() string { return "memory_delete" }
func (Delete) Description() string {
	return "Delete exactly one memory entry by name. Pinned entries are deletable but only by an explicit call. Takes effect from the next session."
}
func (Delete) IsReadOnly() bool              { return false }
func (Delete) CanRunInParallel() bool        { return false }
func (Delete) DefaultTimeout() time.Duration { return 10 * time.Second }

func (Delete) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Name of the entry to delete."}
		},
		"required": ["name"]
	}`)
}

func (d *Delete) Run(ctx context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in nameInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ErrorResultJSON("invalid JSON: " + err.Error()), nil
	}
	if in.Name == "" {
		return tools.ErrorResultJSON("name is required"), nil
	}

	if err := d.Store.Delete(ctx, in.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return codedError("not_found", fmt.Sprintf("memory entry %q not found", in.Name), nil), nil
		}
		return tools.ErrorResultJSON(err.Error()), nil
	}

	out, _ := json.Marshal(map[string]any{
		"deleted":                true,
		"next_session_effective": true,
	})
	return &tools.Result{Output: string(out)}, nil
}

// ---- memory_merge -----------------------------------------------------------

// Merge implements the memory_merge tool: atomically write the merged `into`
// entry and delete the named source entries in a single transaction.
type Merge struct {
	Store   *memory.EntryStore
	Budgets memory.Budgets
}

func (Merge) Name() string { return "memory_merge" }
func (Merge) Description() string {
	return "Atomically consolidate several memory entries into one. Provide names (the source entries to merge, at least one) and into (the merged entry: name required, plus optional trigger, content, pinned, durability, category). The into entry is written and the sources deleted in a single transaction. Takes effect from the next session."
}
func (Merge) IsReadOnly() bool              { return false }
func (Merge) CanRunInParallel() bool        { return false }
func (Merge) DefaultTimeout() time.Duration { return 10 * time.Second }

func (Merge) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"names": {
				"type": "array",
				"items": {"type": "string"},
				"minItems": 1,
				"description": "Names of the source entries to merge and delete."
			},
			"into": {
				"type": "object",
				"properties": {
					"name":       {"type": "string", "description": "Name of the merged entry."},
					"trigger":    {"type": "string"},
					"content":    {"type": "string"},
					"pinned":     {"type": "boolean"},
					"durability": {"type": "string", "enum": ["evergreen", "volatile"]},
					"category":   {"type": "string"}
				},
				"required": ["name"]
			}
		},
		"required": ["names", "into"]
	}`)
}

type mergeInto struct {
	Name       string `json:"name"`
	Trigger    string `json:"trigger"`
	Content    string `json:"content"`
	Pinned     bool   `json:"pinned"`
	Durability string `json:"durability"`
	Category   string `json:"category"`
}

type mergeInput struct {
	Names []string  `json:"names"`
	Into  mergeInto `json:"into"`
}

func (m *Merge) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in mergeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ErrorResultJSON("invalid JSON: " + err.Error()), nil
	}
	if len(in.Names) == 0 {
		return tools.ErrorResultJSON("names must contain at least one source entry"), nil
	}
	if in.Into.Name == "" {
		return tools.ErrorResultJSON("into.name is required"), nil
	}

	durability := in.Into.Durability
	if durability == "" {
		durability = "volatile"
	}
	if durability != "evergreen" && durability != "volatile" {
		return tools.ErrorResultJSON(fmt.Sprintf("invalid durability %q (want evergreen or volatile)", in.Into.Durability)), nil
	}

	if err := m.Budgets.CheckTrigger(in.Into.Trigger); err != nil {
		var ti memory.ErrTriggerInvalid
		if errors.As(err, &ti) {
			return codedError("trigger_invalid", ti.Error(), map[string]any{"limit": ti.Limit, "actual": ti.Actual}), nil
		}
		return tools.ErrorResultJSON(err.Error()), nil
	}
	if err := m.Budgets.CheckEntryContent(in.Into.Content); err != nil {
		var tooLarge memory.ErrMemoryTooLarge
		if errors.As(err, &tooLarge) {
			return codedError("memory_too_large", tooLarge.Error(), map[string]any{"limit": tooLarge.Limit, "actual": tooLarge.Actual}), nil
		}
		return tools.ErrorResultJSON(err.Error()), nil
	}

	src := ""
	if env != nil {
		src = env.SessionID
	}
	into := &memory.Entry{
		Name:            in.Into.Name,
		Trigger:         in.Into.Trigger,
		Content:         in.Into.Content,
		Pinned:          in.Into.Pinned,
		Durability:      durability,
		Category:        in.Into.Category,
		CharCount:       charCount(in.Into.Content),
		SourceSessionID: src,
	}
	if err := m.Store.Merge(ctx, in.Names, into); err != nil {
		return tools.ErrorResultJSON(err.Error()), nil
	}

	out, _ := json.Marshal(map[string]any{
		"merged":                 true,
		"into":                   into.Name,
		"next_session_effective": true,
	})
	return &tools.Result{Output: string(out)}, nil
}
