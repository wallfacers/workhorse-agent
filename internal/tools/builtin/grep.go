package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wallfacers/data-agent/internal/tools"
	"github.com/wallfacers/data-agent/internal/tools/pathguard"
)

type GrepInput struct {
	Pattern  string `json:"pattern"`
	Path     string `json:"path,omitempty"`     // file or dir, default workdir
	Include  string `json:"include,omitempty"`  // optional glob filter on filenames
	MaxHits  int    `json:"max_hits,omitempty"` // 0 = default 500
}

// Grep walks the workdir (or a sub-path) and emits "path:line:content" lines
// for every regex match. Pure Go, no ripgrep dependency.
type Grep struct {
	Timeout time.Duration
}

func (Grep) Name() string                   { return "Grep" }
func (Grep) IsReadOnly() bool               { return true }
func (Grep) CanRunInParallel() bool         { return true }
func (g Grep) DefaultTimeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return 60 * time.Second
}
func (Grep) Description() string {
	return "Recursively search workdir for a Go regex; emits path:line:content for each match."
}

const grepSchema = `{
  "type": "object",
  "properties": {
    "pattern":  {"type": "string", "description": "RE2 regex"},
    "path":     {"type": "string", "description": "subpath inside workdir (default: workdir)"},
    "include":  {"type": "string", "description": "glob filter on filename, e.g. '*.go'"},
    "max_hits": {"type": "integer", "description": "default 500"}
  },
  "required": ["pattern"]
}`

func (Grep) InputSchema() json.RawMessage { return []byte(grepSchema) }

func (Grep) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in GrepInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("invalid input: " + err.Error()), nil
	}
	if in.Pattern == "" {
		return errorResult("pattern is required"), nil
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return errorResult("regex: " + err.Error()), nil
	}
	maxHits := in.MaxHits
	if maxHits <= 0 {
		maxHits = 500
	}

	root := env.Workdir
	if in.Path != "" {
		root, err = pathguard.Resolve(env.Workdir, in.Path)
		if err != nil {
			return errorResult("path: " + err.Error()), nil
		}
	}

	var sb strings.Builder
	hits := 0
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // ignore unreadable entries
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() {
			// Skip dot-dirs by default (`.git` etc).
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		// Optional include filter.
		if in.Include != "" {
			match, _ := filepath.Match(in.Include, d.Name())
			if !match {
				return nil
			}
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close() //nolint:errcheck
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			if re.Match(sc.Bytes()) {
				rel, _ := filepath.Rel(env.Workdir, p)
				if rel == "" {
					rel = p
				}
				fmt.Fprintf(&sb, "%s:%d:%s\n", rel, lineNo, sc.Text())
				hits++
				if hits >= maxHits {
					return errStopWalk
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != errStopWalk {
		// surface ctx cancel as is; other errors get reported but not fatal
		if walkErr == context.Canceled {
			return errorResult("grep canceled"), nil
		}
	}
	if sb.Len() == 0 {
		return &tools.Result{Output: "(no matches)"}, nil
	}
	return &tools.Result{Output: sb.String()}, nil
}

// errStopWalk is a sentinel used to stop WalkDir early once max_hits is hit.
var errStopWalk = fmt.Errorf("stop walk")
