package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
)

// ToolAnnotations carries the optional MCP tool annotations from tools/list.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint,omitempty"`
	DestructiveHint bool `json:"destructiveHint,omitempty"`
}

// Adapter wraps a ServerTool as a tools.Tool. It implements the Tool interface
// with conservative defaults: unless the MCP server declares readOnlyHint=true,
// the tool is treated as read-write and non-parallel.
type Adapter struct {
	st          ServerTool
	name        string
	description string
	schema      json.RawMessage
	readOnly    bool
	parallel    bool
}

// NewAdapter creates an Adapter for the given ServerTool.
func NewAdapter(st ServerTool) *Adapter {
	name := st.Server + "__" + st.Def.Name
	desc := st.Def.Description

	// Parse optional annotations from MCP tool definition.
	readOnly := false
	if hasReadOnlyHint(st.Def) {
		readOnly = true
	}

	return &Adapter{
		st:          st,
		name:        name,
		description: desc,
		schema:      st.Def.InputSchema,
		readOnly:    readOnly,
		parallel:    readOnly, // read-only tools can run in parallel
	}
}

// hasReadOnlyHint checks the raw tool definition for an annotations.readOnlyHint
// field set to true. We use a lightweight partial parse to avoid adding an
// annotations field to ToolDef that most servers don't populate.
func hasReadOnlyHint(t ToolDef) bool {
	var raw struct {
		Annotations *struct {
			ReadOnlyHint bool `json:"readOnlyHint"`
		} `json:"annotations"`
	}
	// Re-marshal ToolDef to get the full JSON with annotations.
	b, err := json.Marshal(t)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return false
	}
	if raw.Annotations != nil && raw.Annotations.ReadOnlyHint {
		return true
	}
	return false
}

func (a *Adapter) Name() string                  { return a.name }
func (a *Adapter) Description() string           { return a.description }
func (a *Adapter) InputSchema() json.RawMessage  { return a.schema }
func (a *Adapter) IsReadOnly() bool              { return a.readOnly }
func (a *Adapter) CanRunInParallel() bool        { return a.parallel }
func (a *Adapter) DefaultTimeout() time.Duration { return 0 } // inherit config default

// Run calls the MCP tool via the Host and translates the result into a tools.Result.
func (a *Adapter) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (*tools.Result, error) {
	result, err := a.st.CallTool(ctx, input)
	if err != nil {
		return &tools.Result{
			Output:  fmt.Sprintf("MCP tool %s error: %s", a.name, err.Error()),
			IsError: true,
		}, nil
	}

	// Build output from content items.
	var out strings.Builder
	for i, item := range result.Content {
		if i > 0 {
			out.WriteByte('\n')
		}
		switch item.Type {
		case "text":
			out.WriteString(item.Text)
		case "resource":
			out.WriteString(item.Data)
		default:
			out.WriteString(item.Text)
		}
	}

	return &tools.Result{
		Output:  out.String(),
		IsError: result.IsError,
	}, nil
}
