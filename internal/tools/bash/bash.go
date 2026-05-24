package bash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wallfacers/data-agent/internal/tools"
)

// BashInput is the JSON input for the Bash tool. Commands run under `bash -c`.
type BashInput struct {
	Command string `json:"command"`
	// Timeout in seconds. 0 means inherit tools.bash.timeout_seconds.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Bash executes shell commands with full process-group teardown on cancel.
// Hard rules from the spec:
//
//   - syscall.Setpgid=true so the child is in its own group; cancel sends
//     SIGTERM to -pgid, killing grandchildren too.
//   - 1500ms after SIGTERM, send SIGKILL to anything still alive.
//   - Inherit env is filtered through envfilter to strip LD_PRELOAD etc.
//   - Stdout+stderr go through a ring buffer capped at MaxOutputBytes;
//     overruns are silently dropped on the left so we keep the most recent
//     output (most useful for the model).
type Bash struct {
	// DefaultTimeoutSeconds is consulted when input.TimeoutSeconds is zero.
	// Comes from config.tools.bash.timeout_seconds.
	DefaultTimeoutSeconds int
	// MaxOutputBytes caps the ring buffer for stdout/stderr capture.
	MaxOutputBytes int
	// BaseEnv is the starting environment for child processes. Use
	// os.Environ() as the default; the call always runs filter on top.
	BaseEnv []string
}

func (Bash) Name() string                  { return "Bash" }
func (Bash) IsReadOnly() bool              { return false }
func (Bash) CanRunInParallel() bool        { return false }
func (b Bash) DefaultTimeout() time.Duration {
	if b.DefaultTimeoutSeconds > 0 {
		return time.Duration(b.DefaultTimeoutSeconds) * time.Second
	}
	return 120 * time.Second
}
func (Bash) Description() string {
	return "Execute a shell command via bash -c. Cancellation kills the whole process group."
}

const bashSchema = `{
  "type": "object",
  "properties": {
    "command":         {"type": "string"},
    "timeout_seconds": {"type": "integer"}
  },
  "required": ["command"]
}`

func (Bash) InputSchema() json.RawMessage { return []byte(bashSchema) }

func (b Bash) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) (*tools.Result, error) {
	var in BashInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("invalid input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return errorResult("command is empty"), nil
	}

	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = b.DefaultTimeout()
	}
	timedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(timedCtx, "bash", "-c", in.Command)
	cmd.Dir = env.Workdir
	configureProcessGroup(cmd) // Setpgid on Unix; no-op on Windows.

	// Build child env: start from BaseEnv (or os.Environ if none), merge in
	// env.Env, filter the whole lot through envfilter so dangerous keys are
	// stripped no matter how they got into the bag.
	base := b.BaseEnv
	if len(base) == 0 {
		base = osEnviron() // separately mockable shim
	}
	merged := mergeEnv(base, env.Env)
	filtered, dropped := Filter(merged)
	LogDropped(env.Logger, dropped)
	cmd.Env = filtered

	max := b.MaxOutputBytes
	if max <= 0 {
		max = 1 << 20
	}
	rb := newRingBuffer(max)
	cmd.Stdout = rb
	cmd.Stderr = rb

	if err := cmd.Start(); err != nil {
		return errorResult("start: " + err.Error()), nil
	}

	// Run with the explicit teardown helper so we get the SIGTERM→1.5s→SIGKILL
	// escalation regardless of why we're aborting (ctx, timeout, or normal exit).
	waitErr := awaitWithKill(timedCtx, cmd)

	output := rb.String()
	isErr := false
	if waitErr != nil {
		isErr = true
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			output = fmt.Sprintf("%s\n[exit %d]", output, exitErr.ExitCode())
		} else if errors.Is(waitErr, context.DeadlineExceeded) {
			output = fmt.Sprintf("%s\n[timed out after %s]", output, timeout)
		} else if errors.Is(waitErr, context.Canceled) {
			output = fmt.Sprintf("%s\n[canceled]", output)
		} else {
			output = fmt.Sprintf("%s\n[error: %s]", output, waitErr)
		}
	}
	return &tools.Result{Output: output, IsError: isErr}, nil
}

// awaitWithKill waits for cmd.Wait() while honouring ctx. If ctx fires first
// we trigger the OS-specific process-group kill helper and let Wait drain.
func awaitWithKill(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		killProcessGroup(cmd)
		// Wait still has to be called to free OS resources; ignore its
		// post-kill exit error.
		<-done
		return ctx.Err()
	}
}

// ---- ring buffer ----

type ringBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func newRingBuffer(limit int) *ringBuffer {
	return &ringBuffer{limit: limit}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.limit {
		// Single write bigger than the limit: keep the tail.
		r.buf.Reset()
		r.buf.Write(p[len(p)-r.limit:])
		return len(p), nil
	}
	if r.buf.Len()+len(p) > r.limit {
		// Drop the head until p fits.
		drop := r.buf.Len() + len(p) - r.limit
		full := r.buf.Bytes()
		r.buf.Reset()
		r.buf.Write(full[drop:])
	}
	return r.buf.Write(p)
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// ---- helpers ----

func errorResult(msg string) *tools.Result {
	return &tools.Result{Output: msg, IsError: true}
}

// mergeEnv produces a flat ["KEY=VALUE", ...] slice. Entries from override
// replace any matching entries from base.
func mergeEnv(base []string, override map[string]string) []string {
	idx := make(map[string]int, len(base))
	out := make([]string, len(base))
	copy(out, base)
	for i, e := range out {
		if k, _, ok := strings.Cut(e, "="); ok {
			idx[k] = i
		}
	}
	for k, v := range override {
		if i, ok := idx[k]; ok {
			out[i] = k + "=" + v
		} else {
			out = append(out, k+"="+v)
			idx[k] = len(out) - 1
		}
	}
	return out
}

// osEnviron is a function variable so tests can stub the starting env
// without touching the real process state. Default is os.Environ.
var osEnviron = osEnvironReal

// compile-time check that ringBuffer fulfils io.Writer.
var _ io.Writer = (*ringBuffer)(nil)
