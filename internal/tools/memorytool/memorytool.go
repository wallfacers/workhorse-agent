package memorytool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/memory"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// Read implements the memory_read tool.
type Read struct {
	ProfileDir  string
	MemoryLimit int
	UserLimit   int
}

func (Read) Name() string                  { return "memory_read" }
func (Read) Description() string           { return "Read the current content of a memory file from disk." }
func (Read) IsReadOnly() bool              { return true }
func (Read) CanRunInParallel() bool        { return true }
func (Read) DefaultTimeout() time.Duration { return 10 * time.Second }

func (Read) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"kind": {
				"type": "string",
				"enum": ["memory", "user"],
				"description": "Which memory file to read: 'memory' for agent-curated facts, 'user' for user identity and preferences."
			}
		},
		"required": ["kind"]
	}`)
}

type readInput struct {
	Kind string `json:"kind"`
}

func (r *Read) Run(_ context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in readInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("invalid JSON: " + err.Error()), nil
	}

	kind, err := memory.ValidateKind(in.Kind)
	if err != nil {
		return errorResult("invalid_kind: " + err.Error()), nil
	}

	content, err := memory.ReadFile(r.ProfileDir, in.Kind)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	limit := r.limitFor(kind)
	out, _ := json.Marshal(map[string]any{
		"content":    content,
		"char_count": memory.CharCount(content),
		"char_limit": limit,
	})
	return &tools.Result{Output: string(out)}, nil
}

func (r *Read) limitFor(kind memory.Kind) int {
	switch kind {
	case memory.KindMemory:
		return r.MemoryLimit
	case memory.KindUser:
		return r.UserLimit
	default:
		return 0
	}
}

// Write implements the memory_write tool.
type Write struct {
	ProfileDir  string
	MemoryLimit int
	UserLimit   int
}

func (Write) Name() string { return "memory_write" }
func (Write) Description() string {
	return "Write content to a memory file. Use mode 'replace' (default) or 'append'."
}
func (Write) IsReadOnly() bool              { return false }
func (Write) CanRunInParallel() bool        { return false }
func (Write) DefaultTimeout() time.Duration { return 10 * time.Second }

func (Write) InputSchema() json.RawMessage {
	return []byte(`{
		"type": "object",
		"properties": {
			"kind": {
				"type": "string",
				"enum": ["memory", "user"],
				"description": "Which memory file to write."
			},
			"content": {
				"type": "string",
				"description": "The content to write."
			},
			"mode": {
				"type": "string",
				"enum": ["replace", "append"],
				"description": "Write mode: 'replace' (default) overwrites the file; 'append' adds after existing content."
			}
		},
		"required": ["kind", "content"]
	}`)
}

type writeInput struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	Mode    string `json:"mode"`
}

func (w *Write) Run(_ context.Context, _ *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in writeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("invalid JSON: " + err.Error()), nil
	}

	kind, err := memory.ValidateKind(in.Kind)
	if err != nil {
		return errorResult("invalid_kind: " + err.Error()), nil
	}

	writer := &memory.Writer{
		ProfileDir:  w.ProfileDir,
		MemoryLimit: w.MemoryLimit,
		UserLimit:   w.UserLimit,
	}

	mode := memory.EnsureValidMode(in.Mode)
	if err := writer.Write(kind, in.Content, mode); err != nil {
		if _, ok := err.(memory.ErrMemoryTooLarge); ok {
			return errorResult(fmt.Sprintf("memory_too_large: %v", err)), nil
		}
		return errorResult(err.Error()), nil
	}

	// Re-read to get accurate char count
	content, err := memory.ReadFile(w.ProfileDir, in.Kind)
	if err != nil {
		content = in.Content
	}
	limit := writer.CharLimit(kind)

	out, _ := json.Marshal(map[string]any{
		"accepted":               true,
		"char_count":             memory.CharCount(content),
		"char_limit":             limit,
		"next_session_effective": true,
	})
	return &tools.Result{Output: string(out)}, nil
}

func errorResult(msg string) *tools.Result {
	return &tools.Result{Output: fmt.Sprintf(`{"error":%q}`, msg), IsError: true}
}
