package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

type EditInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// Edit applies an exact-match string replacement to a file. If OldString is
// not present, the tool fails so the model can correct itself instead of
// silently writing the wrong content. If OldString appears multiple times
// without ReplaceAll, the tool also fails — there is no heuristic guessing.
type Edit struct {
	Timeout time.Duration
}

func (Edit) Name() string           { return "Edit" }
func (Edit) IsReadOnly() bool       { return false }
func (Edit) CanRunInParallel() bool { return false }
func (e Edit) DefaultTimeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return 30 * time.Second
}
func (Edit) Description() string {
	return "Replace an exact string in a file inside the workdir. Use replace_all to handle multiple occurrences."
}

const editSchema = `{
  "type": "object",
  "properties": {
    "path":        {"type": "string"},
    "old_string":  {"type": "string"},
    "new_string":  {"type": "string"},
    "replace_all": {"type": "boolean"}
  },
  "required": ["path", "old_string", "new_string"]
}`

func (Edit) InputSchema() json.RawMessage { return []byte(editSchema) }

func (Edit) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in EditInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("invalid input: " + err.Error()), nil
	}
	if in.Path == "" {
		return errorResult("path is required"), nil
	}
	if in.OldString == "" {
		return errorResult("old_string must not be empty"), nil
	}
	resolved, err := pathguard.Resolve(env.Workdir, in.Path)
	if err != nil {
		return errorResult("path: " + err.Error()), nil
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		return errorResult("read: " + err.Error()), nil
	}
	src := string(body)
	count := strings.Count(src, in.OldString)
	if count == 0 {
		return errorResult("old_string not found in file"), nil
	}
	if count > 1 && !in.ReplaceAll {
		return errorResult(fmt.Sprintf("old_string appears %d times; set replace_all=true to replace every occurrence", count)), nil
	}

	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(src, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(src, in.OldString, in.NewString, 1)
	}

	// Atomic write via temp + rename (same idea as Write).
	dir := filepath.Dir(resolved)
	base := filepath.Base(resolved)
	tmp, err := os.CreateTemp(dir, "."+base+".edit.*")
	if err != nil {
		return errorResult("temp: " + err.Error()), nil
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(updated); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return errorResult("write: " + err.Error()), nil
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return errorResult("fsync: " + err.Error()), nil
	}
	_ = tmp.Close()
	if err := os.Rename(tmpPath, resolved); err != nil {
		_ = os.Remove(tmpPath)
		return errorResult("rename: " + err.Error()), nil
	}

	_ = ctx
	return &tools.Result{Output: fmt.Sprintf("edited %s (%d replacements)", resolved, replacementCount(in, count))}, nil
}

func replacementCount(in EditInput, total int) int {
	if in.ReplaceAll {
		return total
	}
	return 1
}
