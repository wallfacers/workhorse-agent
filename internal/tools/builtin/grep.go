package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/config"
	"github.com/wallfacers/workhorse-agent/internal/tools"
	"github.com/wallfacers/workhorse-agent/internal/tools/pathguard"
)

type GrepInput struct {
	Pattern   string `json:"pattern"`
	Path      string `json:"path,omitempty"`       // file or dir, default workdir
	Include   string `json:"include,omitempty"`    // optional glob filter on filenames
	MaxHits   int    `json:"max_hits,omitempty"`   // 0 = default 500
	IgnoreVCS *bool  `json:"ignore_vcs,omitempty"` // nil = follow config; explicit takes precedence
	Hidden    bool   `json:"hidden,omitempty"`     // true = walk into . hidden files/dirs (excluding hard VCS)
}

// Grep walks the workdir (or a sub-path) and emits "path:line:content" lines
// for every regex match. Pure Go, parallel walker pool, gitignore-aware.
type Grep struct {
	Timeout time.Duration
	Cfg     config.ToolsGrep
}

func (Grep) Name() string           { return "Grep" }
func (Grep) IsReadOnly() bool       { return true }
func (Grep) CanRunInParallel() bool { return true }

func (g Grep) DefaultTimeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return 60 * time.Second
}

func (Grep) Description() string {
	return "Recursively search workdir for a Go regex; emits path:line:content for each match. Honors .gitignore and built-in default excludes (node_modules, dist, etc.) by default."
}

const grepSchema = `{
  "type": "object",
  "properties": {
    "pattern":    {"type": "string",  "description": "RE2 regex"},
    "path":       {"type": "string",  "description": "subpath inside workdir (default: workdir)"},
    "include":    {"type": "string",  "description": "glob filter on filename, e.g. '*.go'"},
    "max_hits":   {"type": "integer", "description": "default 500"},
    "ignore_vcs": {"type": "boolean", "description": "respect .gitignore (default: follow config; usually true). Set false to grep into ignored paths."},
    "hidden":     {"type": "boolean", "description": "walk into dot-files/dirs (default false). .git/.hg/.svn are still always skipped."}
  },
  "required": ["pattern"]
}`

func (Grep) InputSchema() json.RawMessage { return []byte(grepSchema) }

// grepHit is one line match emitted by a worker.
type grepHit struct {
	relPath string // relative to workdir, normalized to forward slashes
	lineNo  int
	line    string
}

func (g Grep) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
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

	// Effective settings: input > config > defaults. The single walker goroutine
	// caps useful parallelism around 8; bigger pools just add scheduling jitter
	// (see docs/bench-grep-scaling.md).
	workers := g.Cfg.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
		if workers > 8 {
			workers = 8
		}
	}

	respectGitignore := g.Cfg.RespectGitignore
	if in.IgnoreVCS != nil {
		respectGitignore = *in.IgnoreVCS
	}

	defaultExcludes := g.Cfg.DefaultExcludes
	if len(defaultExcludes) == 0 {
		defaultExcludes = builtinDefaultExcludes
	}

	var repoRoot string
	if respectGitignore {
		repoRoot = findRepoRoot(root)
	}

	opts := walkOptions{
		workdir:         env.Workdir,
		root:            root,
		repoRoot:        repoRoot,
		re:              re,
		defaultExcludes: defaultExcludes,
		hidden:          in.Hidden,
		include:         in.Include,
		maxHits:         maxHits,
	}

	var hits []grepHit
	var accessErrors int
	if workers == 1 {
		hits, accessErrors, err = runSerial(ctx, opts)
	} else {
		hits, accessErrors, err = runParallel(ctx, opts, workers)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return errorResult("grep canceled"), nil
		}
		return errorResult("grep: " + err.Error()), nil
	}

	return formatResult(hits, accessErrors, maxHits), nil
}

type walkOptions struct {
	workdir         string // session workdir; output paths are relative to this
	root            string // walk root (workdir or a sub-path)
	repoRoot        string // "" if respect_gitignore=false or workdir is outside any git repo
	re              *regexp.Regexp
	defaultExcludes []string
	hidden          bool
	include         string
	maxHits         int
}

// runParallel orchestrates one walker goroutine producing file paths and N
// worker goroutines consuming them. A derived context is cancelled once
// maxHits is exceeded so the walker and pending workers wind down promptly.
func runParallel(parentCtx context.Context, o walkOptions, workers int) ([]grepHit, int, error) {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	filesCh := make(chan string, 256)
	var totalHits atomic.Int64
	var accessErrors atomic.Int64

	// Pre-seed gitignore stack with .gitignore files from repoRoot down to
	// root's parent. The walker pushes root's own .gitignore on entry.
	stack := seedStack(o.repoRoot, o.root)

	var walkErr error
	var walkWG sync.WaitGroup
	walkWG.Add(1)
	go func() {
		defer walkWG.Done()
		defer close(filesCh)
		walkErr = walkDir(ctx, o, stack, o.root, initialDomain(o.repoRoot, o.root), filesCh, &accessErrors)
	}()

	type localBatch struct {
		hits []grepHit
	}
	results := make(chan localBatch, workers)

	var workersWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			var local []grepHit
			for p := range filesCh {
				if ctx.Err() != nil {
					// Drain channel without scanning so the walker (which blocks
					// on send) can finish promptly.
					continue
				}
				hits, denied := scanFile(ctx, p, o.workdir, o.re)
				if denied {
					accessErrors.Add(1)
					continue
				}
				if len(hits) == 0 {
					continue
				}
				local = append(local, hits...)
				if totalHits.Add(int64(len(hits))) >= int64(o.maxHits) {
					cancel()
				}
			}
			results <- localBatch{hits: local}
		}()
	}

	go func() {
		workersWG.Wait()
		close(results)
	}()

	var all []grepHit
	for b := range results {
		all = append(all, b.hits...)
	}
	walkWG.Wait()

	// walker may legitimately return ctx.Canceled from our early-stop cancel;
	// surface that case as nil so the caller treats it as a normal complete.
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return nil, int(accessErrors.Load()), walkErr
	}
	// But propagate the *parent* ctx cancel as an error so callers know.
	if parentCtx.Err() != nil {
		return nil, int(accessErrors.Load()), parentCtx.Err()
	}
	return all, int(accessErrors.Load()), nil
}

// runSerial runs walker + scan inline. Used when workers==1, for race-clean
// tests, and for environments where forking goroutines is undesirable.
func runSerial(ctx context.Context, o walkOptions) ([]grepHit, int, error) {
	stack := seedStack(o.repoRoot, o.root)
	var accessErrors atomic.Int64

	type emitFunc func(p string) (stop bool, err error)
	var all []grepHit
	var totalHits int

	var emit emitFunc = func(p string) (bool, error) {
		hits, denied := scanFile(ctx, p, o.workdir, o.re)
		if denied {
			accessErrors.Add(1)
			return false, nil
		}
		all = append(all, hits...)
		totalHits += len(hits)
		return totalHits >= o.maxHits, nil
	}

	// Re-use walkDir with a synthetic channel for path delivery; consumer
	// in this goroutine reads and emits. For serial mode we skip channels
	// entirely and walk directly.
	err := walkDirSerial(ctx, o, stack, o.root, initialDomain(o.repoRoot, o.root), &accessErrors, emit)
	if err != nil && !errors.Is(err, errEarlyStop) && !errors.Is(err, context.Canceled) {
		return nil, int(accessErrors.Load()), err
	}
	if ctx.Err() != nil {
		return nil, int(accessErrors.Load()), ctx.Err()
	}
	return all, int(accessErrors.Load()), nil
}

var errEarlyStop = errors.New("grep: maxHits reached")

// initialDomain computes the path components from repoRoot to root, used as
// the gitignore "domain" prefix when the walker enters root.
func initialDomain(repoRoot, root string) []string {
	if repoRoot == "" || repoRoot == root {
		return nil
	}
	rel, err := filepath.Rel(repoRoot, root)
	if err != nil || rel == "." || rel == "" {
		return nil
	}
	return strings.Split(filepath.ToSlash(rel), "/")
}

// seedStack loads .git/info/exclude and every .gitignore from repoRoot down
// to root's parent. The walker itself pushes root's own .gitignore on entry.
// repoRoot == "" returns an empty stack.
func seedStack(repoRoot, root string) *gitignoreStack {
	s := &gitignoreStack{}
	if repoRoot == "" {
		return s
	}
	// .git/info/exclude is treated as if it lived at the repo root with no domain.
	if content, err := os.ReadFile(filepath.Join(repoRoot, ".git", "info", "exclude")); err == nil {
		s.push(nil, content)
	}
	if repoRoot == root {
		return s
	}
	rel, err := filepath.Rel(repoRoot, root)
	if err != nil || rel == "." || rel == "" {
		return s
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")

	// Push repoRoot's own .gitignore (domain = nil).
	if content, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore")); err == nil {
		s.push(nil, content)
	}
	// Walk down the chain, pushing each intermediate .gitignore.
	cur := repoRoot
	var domain []string
	for i, p := range parts {
		cur = filepath.Join(cur, p)
		domain = append(domain, p)
		if i == len(parts)-1 {
			// Last segment is the root itself — walker pushes its .gitignore.
			break
		}
		if content, err := os.ReadFile(filepath.Join(cur, ".gitignore")); err == nil {
			s.push(append([]string{}, domain...), content)
		}
	}
	return s
}

// walkDir recursively descends dir, sending matched files to filesCh.
// Stack push/pop happens at directory boundaries via defer.
func walkDir(
	ctx context.Context,
	o walkOptions,
	stack *gitignoreStack,
	dir string,
	domain []string,
	filesCh chan<- string,
	accessErrors *atomic.Int64,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if o.repoRoot != "" {
		if content, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil {
			stack.push(append([]string{}, domain...), content)
			defer stack.pop()
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		accessErrors.Add(1)
		return nil //nolint:nilerr // access errors are counted, not fatal
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		isDir := e.IsDir()

		if !shouldDescend(name, isDir, o, stack, domain) {
			continue
		}
		if isDir {
			child := filepath.Join(dir, name)
			childDomain := append(append([]string{}, domain...), name)
			if err := walkDir(ctx, o, stack, child, childDomain, filesCh, accessErrors); err != nil {
				return err
			}
			continue
		}
		// File entry — apply include glob and queue.
		if o.include != "" {
			if ok, _ := filepath.Match(o.include, name); !ok {
				continue
			}
		}
		select {
		case filesCh <- filepath.Join(dir, name):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// walkDirSerial is the workers=1 codepath. It calls emit inline and stops
// when emit returns stop=true. Stack push/pop and filtering match walkDir.
func walkDirSerial(
	ctx context.Context,
	o walkOptions,
	stack *gitignoreStack,
	dir string,
	domain []string,
	accessErrors *atomic.Int64,
	emit func(p string) (stop bool, err error),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if o.repoRoot != "" {
		if content, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil {
			stack.push(append([]string{}, domain...), content)
			defer stack.pop()
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		accessErrors.Add(1)
		return nil //nolint:nilerr
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		isDir := e.IsDir()

		if !shouldDescend(name, isDir, o, stack, domain) {
			continue
		}
		if isDir {
			child := filepath.Join(dir, name)
			childDomain := append(append([]string{}, domain...), name)
			if err := walkDirSerial(ctx, o, stack, child, childDomain, accessErrors, emit); err != nil {
				return err
			}
			continue
		}
		if o.include != "" {
			if ok, _ := filepath.Match(o.include, name); !ok {
				continue
			}
		}
		stop, err := emit(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if stop {
			return errEarlyStop
		}
	}
	return nil
}

// shouldDescend returns false when a directory entry should be skipped due
// to the hard VCS list, hidden, default_excludes, or gitignore.
func shouldDescend(name string, isDir bool, o walkOptions, stack *gitignoreStack, domain []string) bool {
	// Hard VCS skip applies always (directory-level only — files named .git
	// are vanishingly rare and not worth special-casing).
	if isDir && isHardVCSDir(name) {
		return false
	}
	if !o.hidden && strings.HasPrefix(name, ".") {
		// Allow .gitignore itself to be grep-able when hidden=true; when
		// hidden=false dot-files are always skipped.
		return false
	}
	if matchExclude(name, o.defaultExcludes) {
		return false
	}
	if o.repoRoot != "" {
		childDomain := append(append([]string{}, domain...), name)
		if stack.IsIgnored(childDomain, isDir) {
			return false
		}
	}
	return true
}

const binarySniffSize = 8 << 10

// scanFile opens p, sniffs the first 8 KiB for a NUL byte (skipping binary
// files silently), then scans line-by-line for regex matches. Returns the
// list of hits and a denied flag set when the file could not be opened.
func scanFile(ctx context.Context, p, workdir string, re *regexp.Regexp) (hits []grepHit, denied bool) {
	f, err := os.Open(p)
	if err != nil {
		return nil, true
	}
	defer f.Close() //nolint:errcheck

	head := make([]byte, binarySniffSize)
	n, _ := io.ReadFull(f, head)
	if n > 0 && bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, false // binary, intentional silent skip
	}

	rel, err := filepath.Rel(workdir, p)
	if err != nil || rel == "" {
		rel = p
	}
	rel = filepath.ToSlash(rel)

	// Reattach the prefetched head as the front of the scanner input.
	reader := io.MultiReader(bytes.NewReader(head[:n]), f)
	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	lineNo := 0
	for sc.Scan() {
		// Check ctx periodically; scan is the hot loop.
		if lineNo&63 == 0 {
			if err := ctx.Err(); err != nil {
				return hits, false
			}
		}
		lineNo++
		if re.Match(sc.Bytes()) {
			hits = append(hits, grepHit{relPath: rel, lineNo: lineNo, line: sc.Text()})
		}
	}
	return hits, false
}

// formatResult sorts hits by (path, line), caps at maxHits, and serializes
// to the canonical "path:line:content\n" format. accessErrors > 0 yields the
// trailing "[N entries skipped due to access errors]" suffix.
func formatResult(hits []grepHit, accessErrors, maxHits int) *tools.Result {
	if len(hits) == 0 {
		msg := "(no matches)"
		if accessErrors > 0 {
			msg = fmt.Sprintf("(no matches, %d entries skipped due to access errors)", accessErrors)
		}
		return &tools.Result{Output: msg}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].relPath != hits[j].relPath {
			return hits[i].relPath < hits[j].relPath
		}
		return hits[i].lineNo < hits[j].lineNo
	})
	if len(hits) > maxHits {
		hits = hits[:maxHits]
	}
	var sb strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&sb, "%s:%d:%s\n", h.relPath, h.lineNo, h.line)
	}
	if accessErrors > 0 {
		fmt.Fprintf(&sb, "\n[%d entries skipped due to access errors]", accessErrors)
	}
	return &tools.Result{Output: sb.String()}
}
