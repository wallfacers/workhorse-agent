// Package dispatch implements the Dispatch tool from the multi-agent spec.
//
// Dispatch creates a child Session inheriting the parent's workdir/env/
// provider/model (overridable), seeds it with the prompt as the first
// user_message, optionally streams the child's events back to the parent
// (subagent_event wrapping), and returns the child's final assistant text as
// the tool_result.
//
// The tool itself is stateless; all wiring (SessionManager, agent_type
// loader, depth cap) lives on the injected Host.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/coord"
	"github.com/wallfacers/workhorse-agent/internal/session"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// Host bundles the runtime dependencies Dispatch needs. Construct one per
// process and share it with every Tool instance.
type Host struct {
	Manager  *session.Manager
	Loader   *coord.Loader
	MaxDepth int
}

// DispatchInput mirrors the multi-agent spec's Dispatch signature.
type DispatchInput struct {
	Prompt       string         `json:"prompt"`
	AgentType    string         `json:"agent_type,omitempty"`
	Inputs       map[string]any `json:"inputs,omitempty"`
	Mode         string         `json:"mode,omitempty"`
	Workdir      string         `json:"workdir,omitempty"`
	AllowedTools []string       `json:"allowed_tools,omitempty"`
	DeniedTools  []string       `json:"denied_tools,omitempty"`
	Provider     string         `json:"provider,omitempty"`
	Model        string         `json:"model,omitempty"`
}

const (
	modeStreaming = "streaming"
	modeBlocking  = "blocking"
)

// Tool is the registry-facing handle. Construct once per process; Run is safe
// for concurrent invocation because each call drives its own child session.
type Tool struct {
	Host *Host
}

// Name is the registry-facing identifier matched by the LLM's tool_use.
func (Tool) Name() string { return "Dispatch" }

// Description is what the LLM sees in the tools array.
func (Tool) Description() string {
	return "Spawn a sub-agent session to handle a delegated task. " +
		"Returns the sub-agent's final assistant text."
}

// InputSchema is the JSON Schema the LLM uses to construct calls. Kept
// minimal — the spec is the source of truth for semantics.
func (Tool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt":         {"type": "string"},
    "agent_type":     {"type": "string"},
    "inputs":         {"type": "object"},
    "mode":           {"type": "string", "enum": ["streaming", "blocking"]},
    "workdir":        {"type": "string"},
    "allowed_tools":  {"type": "array", "items": {"type": "string"}},
    "denied_tools":   {"type": "array", "items": {"type": "string"}},
    "provider":       {"type": "string"},
    "model":          {"type": "string"}
  },
  "required": ["prompt"]
}`)
}

// IsReadOnly is false — the child can take any side effect the registry
// exposes.
func (Tool) IsReadOnly() bool { return false }

// CanRunInParallel is true so a parent's single LLM turn can fan out several
// Dispatch calls in one parallel batch (spec scenario "父 agent 一轮并发派发").
func (Tool) CanRunInParallel() bool { return true }

// DefaultTimeout returns 0 so the orchestrator falls back to the
// tools.default_timeout_seconds value (children may legitimately run for a
// long time).
func (Tool) DefaultTimeout() time.Duration { return 0 }

// Run executes one Dispatch call. The returned error is reserved for
// programming bugs; user-visible failures come back as a tools.Result with
// IsError=true so the parent LLM sees them as a normal tool_result.
func (t Tool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	if t.Host == nil || t.Host.Manager == nil {
		return errResult("dispatch host not configured"), nil
	}

	var in DispatchInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid dispatch input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return errResult("dispatch prompt is required"), nil
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	switch mode {
	case "", modeStreaming:
		mode = modeStreaming
	case modeBlocking:
	default:
		return errResult(fmt.Sprintf("invalid mode %q (want streaming|blocking)", in.Mode)), nil
	}

	parentSess, err := t.Host.Manager.GetSession(env.SessionID)
	if err != nil {
		return errResult("parent session not found: " + err.Error()), nil
	}

	maxDepth := t.Host.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 5
	}
	childDepth := parentSess.Depth + 1
	if childDepth > maxDepth {
		return errResult(fmt.Sprintf("max sub-agent depth (%d) exceeded", maxDepth)), nil
	}

	var at coord.AgentType
	if in.AgentType != "" {
		if t.Host.Loader == nil {
			return errResult("agent_type not found: " + in.AgentType), nil
		}
		at, err = t.Host.Loader.Get(in.AgentType)
		if err != nil {
			if errors.Is(err, coord.ErrNotFound) {
				return errResult("agent_type not found: " + in.AgentType), nil
			}
			return errResult("agent_type load failed: " + err.Error()), nil
		}
	}

	childOpts := buildChildOptions(parentSess, at, in, childDepth)
	if err := session.AssertWorkdirWithin(parentSess.Workdir, childOpts.Workdir); err != nil {
		return errResult(err.Error()), nil
	}
	childSess, err := t.Host.Manager.CreateSession(ctx, childOpts)
	if err != nil {
		return errResult("create child session: " + err.Error()), nil
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := t.Host.Manager.DeleteSession(cctx, childSess.ID, 2*time.Second); err != nil {
			slog.Warn("dispatch: cleanup delete session", "session", childSess.ID, "err", err)
		}
	}()

	collector := newCollector()
	pumpDone := make(chan struct{})
	go pump(ctx, childSess, parentSess, mode, collector, pumpDone)

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_ = t.Host.Manager.Cancel(childSess.ID)
		case <-pumpDone:
		}
	}()

	payload, _ := json.Marshal(session.UserMessagePayload{Content: in.Prompt})
	select {
	case childSess.Inbox <- session.ClientMessage{
		Type:    session.ClientUserMessage,
		Payload: payload,
	}:
	case <-ctx.Done():
		<-pumpDone
		<-watcherDone
		return errResult("dispatch cancelled before send"), nil
	}

	<-pumpDone
	<-watcherDone

	if ctx.Err() != nil {
		return &tools.Result{
			Output:  "[CANCELLED] sub-agent " + childSess.ID + " was interrupted",
			IsError: true,
		}, nil
	}
	if errMsg := collector.ErrorMessage(); errMsg != "" {
		return errResult("sub-agent error: " + errMsg), nil
	}
	return &tools.Result{Output: collector.FinalText()}, nil
}

// buildChildOptions applies the inheritance rules from the multi-agent spec:
// Dispatch parameters override agent_type, which overrides parent.
func buildChildOptions(parent *session.Session, at coord.AgentType, in DispatchInput, depth int) session.Options {
	workdir := parent.Workdir
	if in.Workdir != "" {
		workdir = in.Workdir
	}
	envCopy := make(map[string]string, len(parent.Env))
	for k, v := range parent.Env {
		envCopy[k] = v
	}
	model := parent.Model
	if at.Model != "" {
		model = at.Model
	}
	if in.Model != "" {
		model = in.Model
	}
	providerName := parent.ProviderName
	if at.Provider != "" {
		providerName = at.Provider
	}
	if in.Provider != "" {
		providerName = in.Provider
	}
	allowed := append([]string(nil), in.AllowedTools...)
	if len(allowed) == 0 {
		allowed = append([]string(nil), at.AllowTools...)
	}
	denied := append([]string(nil), in.DeniedTools...)
	if len(denied) == 0 {
		denied = append([]string(nil), at.DenyTools...)
	}
	return session.Options{
		ParentID:         parent.ID,
		Workdir:          workdir,
		Env:              envCopy,
		Ephemeral:        parent.Ephemeral,
		Model:            model,
		ProviderName:     providerName,
		AgentType:        at.Name,
		AllowedTools:     allowed,
		DenyTools:        denied,
		Depth:            depth,
		SystemPromptBase: at.SystemPrompt,
	}
}

func errResult(msg string) *tools.Result {
	return &tools.Result{Output: msg, IsError: true}
}
