// Package delegationtool implements the delegate / delegation_read /
// delegation_list built-in tools that expose the background read-only
// delegation manager (001-agent-orchestration US1) to the LLM. The manager is
// obtained via a type assertion on tools.Env.Delegations.
package delegationtool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/delegation"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

func managerFrom(env *tools.Env) (*delegation.Manager, bool) {
	mgr, ok := env.Delegations.(*delegation.Manager)
	return mgr, ok && mgr != nil
}

func errResult(msg string) *tools.Result {
	return &tools.Result{Output: msg, IsError: true}
}

// ---- delegate ----

type DelegateTool struct{}

func (DelegateTool) Name() string { return "delegate" }

func (DelegateTool) Description() string {
	return `Delegate a read-only research task to a background sub-agent that runs in parallel without blocking this conversation. The sub-agent can only read and search — it cannot write files, run commands, or delegate further (nesting is refused).

When the task finishes, a completion notice is injected into this session before your next turn; call delegation_read with the returned id to fetch the full result.

Use this for exploratory research or context-gathering that would otherwise clutter your context. Do NOT use it for a single file read (use Read) or a quick lookup — those are faster done directly.`
}

func (DelegateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "description": {
      "type": "string",
      "description": "Short label for the delegated task (shown in lists), e.g. 'Research auth flow'"
    },
    "prompt": {
      "type": "string",
      "description": "Full detailed instructions for the background read-only sub-agent"
    }
  },
  "required": ["description", "prompt"]
}`)
}

func (DelegateTool) IsReadOnly() bool              { return false }
func (DelegateTool) CanRunInParallel() bool        { return true }
func (DelegateTool) DefaultTimeout() time.Duration { return 0 }

func (DelegateTool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	mgr, ok := managerFrom(env)
	if !ok {
		return errResult("delegation manager not configured"), nil
	}
	var in struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid delegate input: " + err.Error()), nil
	}
	id, err := mgr.Start(ctx, env.SessionID, env.Workdir, in.Description, in.Prompt)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: fmt.Sprintf(`Delegation started: %s
The sub-agent is read-only and runs in the background.
You will see a notification in this session when it completes.
Use delegation_read("%s") to retrieve the full result later.`, id, id)}, nil
}

// ---- delegation_read ----

type DelegationReadTool struct{}

func (DelegationReadTool) Name() string { return "delegation_read" }

func (DelegationReadTool) Description() string {
	return `Retrieve the full result of a background delegation by its id. A still-running delegation returns a non-blocking status line instead of waiting. Use delegation_list to discover ids.`
}

func (DelegationReadTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "Delegation ID, e.g. 'brisk-amber-fox'"}
  },
  "required": ["id"]
}`)
}

func (DelegationReadTool) IsReadOnly() bool              { return true }
func (DelegationReadTool) CanRunInParallel() bool        { return true }
func (DelegationReadTool) DefaultTimeout() time.Duration { return 0 }

func (DelegationReadTool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	mgr, ok := managerFrom(env)
	if !ok {
		return errResult("delegation manager not configured"), nil
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid delegation_read input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.ID) == "" {
		return errResult("id is required"), nil
	}
	d, err := mgr.Read(ctx, in.ID)
	if err != nil {
		return errResult(fmt.Sprintf(
			"Delegation %q not found. Use delegation_list to see available delegations.", in.ID)), nil
	}
	switch d.Status {
	case store.DelegationRunning:
		return &tools.Result{Output: fmt.Sprintf(
			"Delegation %q is still running. Continue other work; you will be notified.", in.ID)}, nil
	case store.DelegationError:
		return &tools.Result{Output: failDetail(d)}, nil
	default:
		return &tools.Result{Output: d.Result}, nil
	}
}

func failDetail(d *store.Delegation) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Delegation %q failed", d.ID))
	if d.Error != "" {
		sb.WriteString(": ")
		sb.WriteString(d.Error)
	}
	if d.Result != "" {
		sb.WriteString("\n\n")
		sb.WriteString(d.Result)
	}
	return sb.String()
}

// ---- delegation_list ----

type DelegationListTool struct{}

func (DelegationListTool) Name() string { return "delegation_list" }

func (DelegationListTool) Description() string {
	return `List the background delegations started in this session with their status and a short summary.`
}

func (DelegationListTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (DelegationListTool) IsReadOnly() bool              { return true }
func (DelegationListTool) CanRunInParallel() bool        { return true }
func (DelegationListTool) DefaultTimeout() time.Duration { return 0 }

func (DelegationListTool) Run(ctx context.Context, env *tools.Env, _ json.RawMessage) (*tools.Result, error) {
	mgr, ok := managerFrom(env)
	if !ok {
		return errResult("delegation manager not configured"), nil
	}
	list, err := mgr.List(ctx, env.SessionID)
	if err != nil {
		return errResult("list delegations failed: " + err.Error()), nil
	}
	if len(list) == 0 {
		return &tools.Result{Output: "No delegations found for this session."}, nil
	}
	var sb strings.Builder
	for _, d := range list {
		sb.WriteString(fmt.Sprintf("- %s [%s] %s\n", d.ID, d.Status, d.Description))
		switch d.Status {
		case store.DelegationRunning:
			sb.WriteString("  Running in the background.\n")
		default:
			if d.Summary != "" {
				sb.WriteString("  ")
				sb.WriteString(d.Summary)
				sb.WriteByte('\n')
			} else if d.Error != "" {
				sb.WriteString("  ")
				sb.WriteString(d.Error)
				sb.WriteByte('\n')
			}
		}
	}
	return &tools.Result{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

// Tools returns all three delegation tools for registration.
func Tools() []tools.Tool {
	return []tools.Tool{DelegateTool{}, DelegationReadTool{}, DelegationListTool{}}
}
