package extagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/driver"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// InternalGated marks this tool as bypassing the standard permission check.
// The ExternalAgent tool handles its own per-adapter approval inside Run.
type InternalGated interface {
	IsInternalGated() bool
}

// PermissionGate prompts the user for adapter approval.
type PermissionGate interface {
	Prompt(ctx context.Context, sessionID, toolName, adapterName string) (bool, error)
}

// Host holds the runtime dependencies injected at construction.
type Host struct {
	Registry       *extagent.Registry
	PermissionGate PermissionGate
	Driver         *driver.Driver
	OutputCapBytes int
	KillOnOutputCap bool
}

// Tool implements tools.Tool for the ExternalAgent tool.
type Tool struct {
	Host *Host

	cachedSchema json.RawMessage
	cachedDesc   string
	cachedTimeout time.Duration
}

type externalAgentInput struct {
	AgentName       string         `json:"agent_name"`
	Prompt          string         `json:"prompt"`
	Inputs          map[string]any `json:"inputs,omitempty"`
	TimeoutSec      int            `json:"timeout_sec,omitempty"`
	ResumeSessionID string         `json:"resume_session_id,omitempty"`
}

// approved tracks per-session adapter approvals.
type approvalKey struct {
	sessionID  string
	agentName  string
}

var _ tools.Tool = (*Tool)(nil)
var _ InternalGated = (*Tool)(nil)

// New creates a new ExternalAgent tool. Returns nil if no healthy sub_agents.
func New(host *Host) *Tool {
	healthy := host.Registry.HealthySubAgents()
	if len(healthy) == 0 {
		return nil
	}
	t := &Tool{Host: host}
	t.cachedSchema = t.buildSchema(healthy)
	t.cachedDesc = t.buildDescription(healthy)
	t.cachedTimeout = t.computeTimeout(healthy)
	return t
}

func (t *Tool) Name() string { return "ExternalAgent" }

func (t *Tool) Description() string { return t.cachedDesc }

func (t *Tool) InputSchema() json.RawMessage { return t.cachedSchema }

func (t *Tool) IsReadOnly() bool { return false }

func (t *Tool) CanRunInParallel() bool { return true }

func (t *Tool) DefaultTimeout() time.Duration { return t.cachedTimeout }

func (t *Tool) IsInternalGated() bool { return true }

func (t *Tool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (*tools.Result, error) {
	var in externalAgentInput
	if err := json.Unmarshal(input, &in); err != nil {
		return &tools.Result{Output: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}

	reg := t.Host.Registry
	if env != nil {
		if r, ok := env.ExtAgentRegistry.(*extagent.Registry); ok && r != nil {
			reg = r
		}
	}

	adapter := reg.Get(in.AgentName)
	if adapter == nil {
		available := reg.HealthySubAgents()
		names := make([]string, len(available))
		for i, a := range available {
			names[i] = a.Name
		}
		return &tools.Result{
			Output:  fmt.Sprintf("unknown agent %q. Available: %s", in.AgentName, strings.Join(names, ", ")),
			IsError: true,
		}, nil
	}

	if !adapter.IsHealthy() {
		reason := "unhealthy"
		if adapter.BinaryMissing {
			reason = "binary not found"
		} else if !adapter.SmokePassed {
			reason = "smoke test failed: " + adapter.SmokeError
		}
		return &tools.Result{
			Output:  fmt.Sprintf("adapter %q is not available: %s", in.AgentName, reason),
			IsError: true,
		}, nil
	}

	// Resume gating.
	if in.ResumeSessionID != "" && !adapter.Session.SupportsResume {
		return &tools.Result{
			Output:  fmt.Sprintf("adapter %q does not support resume", in.AgentName),
			IsError: true,
		}, nil
	}

	// Permission gate for untrusted adapters.
	if !adapter.Security.Trusted && t.Host.PermissionGate != nil {
		key := approvalKey{sessionID: env.SessionID, agentName: in.AgentName}
		approved, err := t.Host.PermissionGate.Prompt(ctx, env.SessionID, "ExternalAgent", in.AgentName)
		if err != nil {
			return &tools.Result{Output: fmt.Sprintf("permission check failed: %v", err), IsError: true}, nil
		}
		if !approved {
			return &tools.Result{Output: fmt.Sprintf("permission denied for adapter %q", in.AgentName), IsError: true}, nil
		}
		_ = key
	}

	// Compute timeout.
	effectiveTimeout := adapter.Control.DefaultTimeoutSec
	if in.TimeoutSec > 0 {
		effectiveTimeout = in.TimeoutSec
	}
	if effectiveTimeout > adapter.Control.MaxTimeoutSec {
		effectiveTimeout = adapter.Control.MaxTimeoutSec
	}
	if effectiveTimeout < 1 {
		effectiveTimeout = 1
	}

	outputCap := t.Host.OutputCapBytes
	if outputCap <= 0 {
		outputCap = 1 << 20
	}

	drv := t.Host.Driver
	if drv == nil {
		drv = &driver.Driver{Logger: env.Logger}
	}

	result, err := drv.Run(ctx, adapter, in.Prompt, driver.Opts{
		SessionID:        env.SessionID,
		CallID:           fmt.Sprintf("%d", time.Now().UnixNano()),
		TimeoutSec:       effectiveTimeout,
		ResumeSessionID:  in.ResumeSessionID,
		Workdir:          env.Workdir,
		OutputCapBytes:   outputCap,
		KillOnOutputCap:  t.Host.KillOnOutputCap,
	})
	if err != nil {
		return &tools.Result{Output: fmt.Sprintf("invocation error: %v", err), IsError: true}, nil
	}

	return &tools.Result{Output: result.Stdout}, nil
}

func (t *Tool) buildSchema(adapters []*extagent.Adapter) json.RawMessage {
	names := make([]string, len(adapters))
	for i, a := range adapters {
		names[i] = a.Name
	}
	sort.Strings(names)

	schema := map[string]any{
		"type": "object",
		"required": []string{"agent_name", "prompt"},
		"properties": map[string]any{
			"agent_name": map[string]any{
				"type": "string",
				"enum": names,
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "The instruction to hand to the sub-agent",
			},
			"inputs": map[string]any{
				"type":                 "object",
				"additionalProperties": true,
				"description":          "Optional key-value inputs for the adapter",
			},
			"timeout_sec": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "Per-call timeout override in seconds",
			},
			"resume_session_id": map[string]any{
				"type":        "string",
				"description": "Resume a prior session (only for resumable adapters)",
			},
		},
	}
	data, _ := json.Marshal(schema)
	return data
}

func (t *Tool) buildDescription(adapters []*extagent.Adapter) string {
	var parts []string
	for _, a := range adapters {
		desc := a.Name + ": " + a.Description
		if a.UsageHints != "" {
			desc += ". " + a.UsageHints
		}
		parts = append(parts, desc)
	}
	return "Invoke an external sub-agent CLI. Available agents:\n" + strings.Join(parts, "\n")
}

func (t *Tool) computeTimeout(adapters []*extagent.Adapter) time.Duration {
	maxTimeout := time.Duration(0)
	for _, a := range adapters {
		if d := time.Duration(a.Control.MaxTimeoutSec) * time.Second; d > maxTimeout {
			maxTimeout = d
		}
	}
	if maxTimeout == 0 {
		return 3630 * time.Second
	}
	return maxTimeout + 30*time.Second
}
