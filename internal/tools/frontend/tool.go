package frontend

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// ToolSpec describes one frontend tool from the client's catalog.
type ToolSpec struct {
	Name           string
	Description    string
	InputSchema    json.RawMessage
	OutputSchema   json.RawMessage
	ParallelSafety string
}

// Tool is a proxy tool whose execution is delegated to the frontend client.
type Tool struct {
	name         string
	description  string
	inputSchema  json.RawMessage
	outputSchema json.RawMessage
	parallelSafe bool
	bridge       *Bridge
}

// NewTool constructs a proxy Tool from a catalog entry and a bound Bridge.
func NewTool(spec ToolSpec, bridge *Bridge) *Tool {
	safe := spec.ParallelSafety == "safe"
	desc := spec.Description
	if len(spec.OutputSchema) > 0 {
		desc = fmt.Sprintf("%s\n\nOutput schema: %s", desc, string(spec.OutputSchema))
	}
	return &Tool{
		name:         spec.Name,
		description:  desc,
		inputSchema:  spec.InputSchema,
		outputSchema: spec.OutputSchema,
		parallelSafe: safe,
		bridge:       bridge,
	}
}

func (t *Tool) Name() string                  { return t.name }
func (t *Tool) Description() string           { return t.description }
func (t *Tool) InputSchema() json.RawMessage  { return t.inputSchema }
func (t *Tool) IsReadOnly() bool              { return false }
func (t *Tool) CanRunInParallel() bool        { return t.parallelSafe }
func (t *Tool) DefaultTimeout() time.Duration { return 0 }

func (t *Tool) Run(ctx context.Context, _ *tools.Env, input json.RawMessage) (*tools.Result, error) {
	return t.bridge.Call(ctx, t.name, input)
}
