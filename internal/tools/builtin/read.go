// Package builtin houses the four read/write/edit/grep builtin tools. Bash
// has its own package (internal/tools/bash) because its process-group +
// danger-guard + env-filter machinery is large enough to warrant its own
// surface.
package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wallfacers/data-agent/internal/tools"
	"github.com/wallfacers/data-agent/internal/tools/pathguard"
)

// ReadInput is the JSON input for the Read tool.
type ReadInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"` // 1-indexed start line
	Limit  int    `json:"limit,omitempty"`  // max lines to return (0 = unlimited)
}

// Read returns the contents of a file inside the session workdir. The output
// is line-numbered so the model can refer to specific lines.
type Read struct {
	// MaxBytes caps the returned output (separate from the agent-wide
	// truncate cap; orchestrator still post-truncates).
	MaxBytes int
	Timeout  time.Duration
}

func (Read) Name() string             { return "Read" }
func (Read) IsReadOnly() bool         { return true }
func (Read) CanRunInParallel() bool   { return true }
func (r Read) DefaultTimeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 30 * time.Second
}
func (Read) Description() string {
	return "Read the contents of a file in the session workdir. Output is line-numbered."
}

const readSchema = `{
  "type": "object",
  "properties": {
    "path":   {"type": "string", "description": "file path, relative to workdir or absolute"},
    "offset": {"type": "integer", "description": "1-indexed start line"},
    "limit":  {"type": "integer", "description": "max lines to return (0 = unlimited)"}
  },
  "required": ["path"]
}`

func (Read) InputSchema() json.RawMessage { return []byte(readSchema) }

func (r Read) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in ReadInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("invalid input: " + err.Error()), nil
	}
	if in.Path == "" {
		return errorResult("path is required"), nil
	}
	resolved, err := pathguard.Resolve(env.Workdir, in.Path)
	if err != nil {
		return errorResult("path: " + err.Error()), nil
	}
	f, err := pathguard.OpenRead(resolved)
	if err != nil {
		return errorResult("open: " + err.Error()), nil
	}
	defer f.Close() //nolint:errcheck

	max := r.MaxBytes
	if max <= 0 {
		max = 1 << 20
	}
	sc := bufio.NewScanner(io.LimitReader(f, int64(max)+1)) // +1 so we can detect overflow
	sc.Buffer(make([]byte, 0, 64*1024), max+1)
	var buf []byte
	line := 0
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return errorResult("read canceled"), nil
		default:
		}
		line++
		if in.Offset > 0 && line < in.Offset {
			continue
		}
		if in.Limit > 0 && line >= in.Offset+in.Limit {
			break
		}
		buf = fmtAppend(buf, line, sc.Bytes())
		if len(buf) >= max {
			buf = append(buf, []byte("\n[truncated]")...)
			break
		}
	}
	if err := sc.Err(); err != nil {
		if !errors.Is(err, io.EOF) {
			return errorResult("scan: " + err.Error()), nil
		}
	}
	return &tools.Result{Output: string(buf)}, nil
}

func fmtAppend(dst []byte, line int, content []byte) []byte {
	dst = append(dst, []byte(fmt.Sprintf("%6d\t", line))...)
	dst = append(dst, content...)
	dst = append(dst, '\n')
	return dst
}

func errorResult(msg string) *tools.Result {
	return &tools.Result{Output: msg, IsError: true}
}
