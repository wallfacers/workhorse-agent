// Package memorytool implements the entry-shaped memory tools backed by the
// per-entry SQLite store (internal/memory.EntryStore). It exposes:
//
//   - memory_write  (single-entry upsert/append; serial)
//   - memory_read   (read one entry by name; read-only, no usage bump)
//   - LoadMemory    (read full content + record a best-effort usage hit)
//   - MemorySearch  (FTS5 MATCH + CJK LIKE fallback; read-only)
//   - memory_delete (transactional)
//   - memory_merge  (atomic write+delete in one transaction)
//
// Errors are surfaced through tools.ErrorResultJSON; structured rejections add a
// machine-readable `code` field via codedError.
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

// codedError builds a structured error Result carrying a machine-readable code
// plus optional extra fields (limit/actual/budget), keeping the {"error",...}
// envelope shape consistent with tools.ErrorResultJSON.
func codedError(code, msg string, extra map[string]any) *tools.Result {
	m := map[string]any{"error": msg, "code": code}
	for k, v := range extra {
		m[k] = v
	}
	out, _ := json.Marshal(m)
	return &tools.Result{Output: string(out), IsError: true}
}

// ---- memory_write -----------------------------------------------------------

// Write implements the memory_write tool: create or update exactly one entry.
type Write struct {
	Store   *memory.EntryStore
	Budgets memory.Budgets
	// OnWrite, when set, is invoked after a successful upsert. It is the
	// curation pressure trigger (design D5) — a non-blocking, debounced signal,
	// so it must not block the tool result. Optional (nil = no curation).
	OnWrite func()
	// AfterWrite, when set, is invoked after a successful upsert with the entry
	// name. It is the write-behind embed enqueue (memory-hybrid-retrieval-locomo)
	// — non-blocking. Optional (nil = no embedding).
	AfterWrite func(name string)
}

func (Write) Name() string { return "memory_write" }
func (Write) Description() string {
	return "Create or update exactly one memory entry. Provide a single entry object (never an array): name (required, slug key), trigger (when it is relevant), content (required), and optional pinned, durability (evergreen|volatile), category, and mode (upsert default, or append to concatenate onto existing content). Takes effect from the next session."
}
func (Write) IsReadOnly() bool              { return false }
func (Write) CanRunInParallel() bool        { return false }
func (Write) DefaultTimeout() time.Duration { return 10 * time.Second }

func (Write) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"name":    {"type": "string", "description": "Unique slug key for the entry. Writing an existing name upserts it."},
			"trigger": {"type": "string", "description": "Single short line describing when this entry is relevant."},
			"content": {"type": "string", "description": "The full body of the entry."},
			"pinned":  {"type": "boolean", "description": "Pinned entries are loaded in full into every session prompt."},
			"durability": {"type": "string", "enum": ["evergreen", "volatile"], "description": "evergreen survives longer; volatile decays faster under curation. Defaults to volatile."},
			"category": {"type": "string", "description": "Facet such as user, feedback, project, reference."},
			"mode":     {"type": "string", "enum": ["upsert", "append"], "description": "upsert (default) replaces the entry; append concatenates content onto an existing entry."}
		},
		"required": ["name", "content"]
	}`)
}

type writeInput struct {
	Name       string `json:"name"`
	Trigger    string `json:"trigger"`
	Content    string `json:"content"`
	Pinned     bool   `json:"pinned"`
	Durability string `json:"durability"`
	Category   string `json:"category"`
	Mode       string `json:"mode"`
}

func (w *Write) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in writeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		// A JSON array (batch input) fails to decode into a single object.
		return tools.ErrorResultJSON("memory_write accepts a single entry, not an array"), nil
	}
	if in.Name == "" {
		return tools.ErrorResultJSON("name is required"), nil
	}
	if in.Content == "" {
		return tools.ErrorResultJSON("content is required"), nil
	}

	// 1. Durability: default volatile, reject unknown enum values.
	durability := in.Durability
	if durability == "" {
		durability = "volatile"
	}
	if durability != "evergreen" && durability != "volatile" {
		return tools.ErrorResultJSON(fmt.Sprintf("invalid durability %q (want evergreen or volatile)", in.Durability)), nil
	}

	// 2. Trigger validation (newline / over-length).
	if err := w.Budgets.CheckTrigger(in.Trigger); err != nil {
		var ti memory.ErrTriggerInvalid
		if errors.As(err, &ti) {
			return codedError("trigger_invalid", ti.Error(), map[string]any{"limit": ti.Limit, "actual": ti.Actual}), nil
		}
		return tools.ErrorResultJSON(err.Error()), nil
	}

	// 3. Resolve final content (append concatenates onto existing).
	finalContent := in.Content
	if in.Mode == "append" {
		existing, err := w.Store.GetByName(ctx, in.Name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return tools.ErrorResultJSON(err.Error()), nil
		}
		if err == nil && existing.Content != "" {
			finalContent = existing.Content + "\n" + in.Content
		}
	}

	// 4. Per-entry content limit.
	if err := w.Budgets.CheckEntryContent(finalContent); err != nil {
		var tooLarge memory.ErrMemoryTooLarge
		if errors.As(err, &tooLarge) {
			return codedError("memory_too_large", tooLarge.Error(), map[string]any{"limit": tooLarge.Limit, "actual": tooLarge.Actual}), nil
		}
		return tools.ErrorResultJSON(err.Error()), nil
	}

	cc := memory.CharCount(finalContent)

	// 5. Pinned budget: existing pinned total (excluding this name) + new content.
	if in.Pinned {
		base, err := w.Store.PinnedCharTotal(ctx, in.Name)
		if err != nil {
			return tools.ErrorResultJSON(err.Error()), nil
		}
		total := base + cc
		if total > w.Budgets.PinnedChars {
			e := memory.ErrPinnedBudgetExceeded{Budget: w.Budgets.PinnedChars, Actual: total}
			return codedError("pinned_budget_exceeded", e.Error(), map[string]any{"budget": e.Budget, "actual": e.Actual}), nil
		}
	}

	src := ""
	if env != nil {
		src = env.SessionID
	}
	entry := &memory.Entry{
		Name:            in.Name,
		Trigger:         in.Trigger,
		Content:         finalContent,
		Pinned:          in.Pinned,
		Durability:      durability,
		Category:        in.Category,
		CharCount:       cc,
		SourceSessionID: src,
	}
	if err := w.Store.Upsert(ctx, entry); err != nil {
		return tools.ErrorResultJSON(err.Error()), nil
	}
	if w.OnWrite != nil {
		w.OnWrite() // curation pressure trigger (non-blocking)
	}
	if w.AfterWrite != nil {
		w.AfterWrite(in.Name) // write-behind embed enqueue (non-blocking)
	}

	out, _ := json.Marshal(map[string]any{
		"accepted":               true,
		"char_count":             cc,
		"next_session_effective": true,
	})
	return &tools.Result{Output: string(out)}, nil
}

// ---- memory_read ------------------------------------------------------------

// Read implements the memory_read tool: read one entry by name from the store
// without recording a usage hit.
type Read struct {
	Store *memory.EntryStore
}

func (Read) Name() string { return "memory_read" }
func (Read) Description() string {
	return "Read the current stored content and metadata of one memory entry by name, directly from the store (not the frozen session snapshot). Does NOT count as a usage hit. Use LoadMemory instead when you intend to actually use the entry."
}
func (Read) IsReadOnly() bool              { return true }
func (Read) CanRunInParallel() bool        { return true }
func (Read) DefaultTimeout() time.Duration { return 10 * time.Second }

func (Read) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Name of the entry to read."}
		},
		"required": ["name"]
	}`)
}

type nameInput struct {
	Name string `json:"name"`
}

func (r *Read) Run(ctx context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in nameInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ErrorResultJSON("invalid JSON: " + err.Error()), nil
	}
	if in.Name == "" {
		return tools.ErrorResultJSON("name is required"), nil
	}

	e, err := r.Store.GetByName(ctx, in.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return codedError("not_found", fmt.Sprintf("memory entry %q not found", in.Name), nil), nil
		}
		return tools.ErrorResultJSON(err.Error()), nil
	}

	var lastUsed any
	if e.LastUsedAt != nil {
		lastUsed = e.LastUsedAt.UTC().Format(time.RFC3339Nano)
	}
	out, _ := json.Marshal(map[string]any{
		"name":         e.Name,
		"content":      e.Content,
		"trigger":      e.Trigger,
		"pinned":       e.Pinned,
		"durability":   e.Durability,
		"category":     e.Category,
		"hit_count":    e.HitCount,
		"last_used_at": lastUsed,
		"char_count":   e.CharCount,
	})
	return &tools.Result{Output: string(out)}, nil
}
