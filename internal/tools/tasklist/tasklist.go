// Package tasklist implements the session-scoped task list (todo) tool. The
// LLM submits the whole list on every call (TodoWrite semantics): the store
// holds the latest snapshot in memory for the lifetime of the session and
// broadcasts each change over SSE so the user sees overall progress. State is
// not persisted to a tasks table — it lives and dies with the session
// (add-todo-tool design D1a/D2a).
package tasklist

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// Status is the three-state lifecycle of a task.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

func validStatus(s string) bool {
	switch s {
	case StatusPending, StatusInProgress, StatusCompleted:
		return true
	default:
		return false
	}
}

// Task is one item in the list. ID is the 1-based position within the submitted
// list; because every TodoWrite replaces the whole list, the position is the
// stable identifier within a snapshot.
type Task struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	ActiveForm  string `json:"active_form,omitempty"`
}

// EmitFunc is the SSE broadcast hook injected at construction. The call site
// wires it to session.Emit so the package stays free of session imports.
type EmitFunc func(ctx context.Context, tasks []Task)

// Store holds one session's task list. Safe for concurrent use; TodoWrite is
// registered non-parallel but the snapshot may be read from other goroutines.
type Store struct {
	mu    sync.Mutex
	tasks []Task
	emit  EmitFunc
}

// NewStore returns an empty store. emit may be nil (tests / no SSE).
func NewStore(emit EmitFunc) *Store {
	return &Store{emit: emit}
}

// Replace overwrites the list with tasks and broadcasts the new snapshot.
func (s *Store) Replace(ctx context.Context, tasks []Task) {
	s.mu.Lock()
	s.tasks = tasks
	snap := append([]Task(nil), tasks...)
	emit := s.emit
	s.mu.Unlock()
	if emit != nil {
		emit(ctx, snap)
	}
}

// Snapshot returns a copy of the current list.
func (s *Store) Snapshot() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Task(nil), s.tasks...)
}

// TodoWrite is the stateless tool surface. Per-session state is read from
// env.TaskList, which the runner factory sets to this session's *Store.
type TodoWrite struct{}

func (TodoWrite) Name() string { return "TodoWrite" }

func (TodoWrite) Description() string {
	return "Maintain a structured task list for the current session so the user can see overall progress. " +
		"Pass the COMPLETE list every call — it replaces the previous list (omitting a task drops it). " +
		"Use it for non-trivial work of three or more steps. Before starting a step set its status to " +
		"'in_progress'; the moment it is done set it to 'completed' — update in real time, don't batch. " +
		"Keep exactly one task in_progress at a time. Skip the list for single, trivial actions and just do them. " +
		"Each task: 'subject' (short imperative title), 'status' (pending|in_progress|completed, default pending), " +
		"optional 'description' and 'active_form' (present-tense label shown while in progress)."
}

func (TodoWrite) IsReadOnly() bool              { return false }
func (TodoWrite) CanRunInParallel() bool        { return false }
func (TodoWrite) DefaultTimeout() time.Duration { return 5 * time.Second }

func (TodoWrite) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"tasks": {
				"type": "array",
				"description": "The complete task list. Replaces the previous list in full.",
				"items": {
					"type": "object",
					"properties": {
						"subject": {"type": "string", "description": "Short imperative title, e.g. \"Add retry to client\"."},
						"status": {"type": "string", "enum": ["pending", "in_progress", "completed"], "description": "Defaults to pending when omitted."},
						"description": {"type": "string", "description": "Optional detail of what the task entails."},
						"active_form": {"type": "string", "description": "Optional present-tense label shown while the task is in progress, e.g. \"Adding retry\"."}
					},
					"required": ["subject"]
				}
			}
		},
		"required": ["tasks"]
	}`)
}

type writeInput struct {
	Tasks []struct {
		Subject     string `json:"subject"`
		Status      string `json:"status"`
		Description string `json:"description"`
		ActiveForm  string `json:"active_form"`
	} `json:"tasks"`
}

func (TodoWrite) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	store, ok := env.TaskList.(*Store)
	if !ok || store == nil {
		return tools.ErrorResultJSON("task_list_unavailable: no task list store bound to this session"), nil
	}

	var in writeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ErrorResultJSON("invalid JSON: " + err.Error()), nil
	}

	// Validate the whole submission before committing — on any rejection the
	// existing list is left untouched (spec scenario "非法状态被拒绝").
	out := make([]Task, 0, len(in.Tasks))
	for i, t := range in.Tasks {
		if t.Subject == "" {
			return tools.ErrorResultJSON(fmt.Sprintf("invalid_task: task %d has an empty subject", i+1)), nil
		}
		status := t.Status
		if status == "" {
			status = StatusPending
		}
		if !validStatus(status) {
			return tools.ErrorResultJSON(fmt.Sprintf("invalid_status: task %d has status %q; must be one of pending, in_progress, completed", i+1, t.Status)), nil
		}
		out = append(out, Task{
			ID:          i + 1,
			Subject:     t.Subject,
			Status:      status,
			Description: t.Description,
			ActiveForm:  t.ActiveForm,
		})
	}

	store.Replace(ctx, out)

	payload, _ := json.Marshal(map[string]any{
		"tasks": out,
		"count": len(out),
	})
	return &tools.Result{Output: string(payload)}, nil
}
