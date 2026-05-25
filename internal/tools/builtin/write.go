package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

type WriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Write atomically replaces a file. The implementation writes to a sibling
// temp file (O_EXCL via pathguard so a planted symlink doesn't redirect us),
// then renames into place. Crash-safe on POSIX filesystems.
type Write struct {
	Timeout time.Duration
}

func (Write) Name() string           { return "Write" }
func (Write) IsReadOnly() bool       { return false }
func (Write) CanRunInParallel() bool { return false }
func (w Write) DefaultTimeout() time.Duration {
	if w.Timeout > 0 {
		return w.Timeout
	}
	return 30 * time.Second
}
func (Write) Description() string {
	return "Atomically replace a file inside the session workdir."
}

const writeSchema = `{
  "type": "object",
  "properties": {
    "path":    {"type": "string"},
    "content": {"type": "string"}
  },
  "required": ["path", "content"]
}`

func (Write) InputSchema() json.RawMessage { return []byte(writeSchema) }

func (w Write) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in WriteInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("invalid input: " + err.Error()), nil
	}
	if in.Path == "" {
		return errorResult("path is required"), nil
	}
	resolved, err := pathguard.ResolveForWrite(env.Workdir, in.Path)
	if err != nil {
		return errorResult("path: " + err.Error()), nil
	}

	dir := filepath.Dir(resolved)
	base := filepath.Base(resolved)
	tmp, err := os.CreateTemp(dir, "."+base+".write.*")
	if err != nil {
		return errorResult("create temp: " + err.Error()), nil
	}
	tmpPath := tmp.Name()
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.WriteString(in.Content); err != nil {
		_ = tmp.Close()
		return errorResult("write: " + err.Error()), nil
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return errorResult("fsync: " + err.Error()), nil
	}
	if err := tmp.Close(); err != nil {
		return errorResult("close: " + err.Error()), nil
	}
	if err := os.Rename(tmpPath, resolved); err != nil {
		return errorResult("rename: " + err.Error()), nil
	}
	cleanupOnError = false

	_ = ctx
	return &tools.Result{
		Output: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), resolved),
	}, nil
}
