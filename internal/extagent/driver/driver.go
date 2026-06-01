package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent"
	"github.com/wallfacers/workhorse-agent/internal/extagent/jsonpath"
	"github.com/wallfacers/workhorse-agent/internal/tools/bash"
)

// Result holds the outcome of a sub-process invocation.
type Result struct {
	Stdout           string
	Stderr           string
	ExitCode         int
	Cancelled        bool
	TimedOut         bool
	Truncated        bool
	TruncatedAtBytes int
	RawDumpPath      string
}

// Opts controls the invocation.
type Opts struct {
	SessionID       string
	CallID          string
	TimeoutSec      int
	ResumeSessionID string
	Workdir         string
	// OutputCapBytes is the maximum output to collect (reuses global config).
	OutputCapBytes int
	// KillOnOutputCap controls whether to kill the child on output cap hit.
	KillOnOutputCap bool
}

// Driver runs external agent binaries as managed child processes.
type Driver struct {
	Logger *slog.Logger
}

// Run executes the adapter's binary with the given prompt and options.
func (d *Driver) Run(ctx context.Context, adapter *extagent.Adapter, promptText string, opts Opts) (Result, error) {
	start := time.Now()
	logger := d.logger()

	if opts.OutputCapBytes <= 0 {
		opts.OutputCapBytes = 1 << 20 // 1 MiB default
	}

	// Compute effective timeout. santhosh-tekuri/jsonschema does not apply
	// schema "default" values into the Go struct, so adapters that omit the
	// control block land here with DefaultTimeoutSec=0 / MaxTimeoutSec=0.
	// Treat <=0 as "unset" and substitute the documented defaults; never
	// clamp a positive timeout to zero (that produces context.WithTimeout(_,0)
	// which fires instantly).
	defaultTimeout := time.Duration(adapter.Control.DefaultTimeoutSec) * time.Second
	if defaultTimeout <= 0 {
		defaultTimeout = 600 * time.Second
	}
	effectiveTimeout := defaultTimeout
	if opts.TimeoutSec > 0 {
		effectiveTimeout = time.Duration(opts.TimeoutSec) * time.Second
	}
	maxTimeout := time.Duration(adapter.Control.MaxTimeoutSec) * time.Second
	if maxTimeout <= 0 {
		maxTimeout = 3600 * time.Second
	}
	if effectiveTimeout > maxTimeout {
		effectiveTimeout = maxTimeout
	}

	// Wrap with timeout context.
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, effectiveTimeout)
	defer timeoutCancel()

	// Build argv.
	cr, err := d.buildCmd(timeoutCtx, adapter, promptText, opts)
	if err != nil {
		return Result{}, err
	}
	cmd := cr.cmd
	if cr.cleanup != nil {
		defer cr.cleanup()
	}

	// Set process group (platform-specific).
	setProcessGroup(cmd)

	// Build env.
	cmd.Env, err = d.buildEnv(adapter)
	if err != nil {
		if cr.stdinPipe != nil {
			cr.stdinPipe.Close()
		}
		return Result{}, err
	}
	// Adapter Cwd overrides session workdir when set.
	if adapter.Invocation.Cwd != "" {
		cmd.Dir = adapter.Invocation.Cwd
	} else if cmd.Dir == "" && opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}

	// Pipes.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		if cr.stdinPipe != nil {
			cr.stdinPipe.Close()
		}
		return Result{}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		if cr.stdinPipe != nil {
			cr.stdinPipe.Close()
		}
		return Result{}, fmt.Errorf("stderr pipe: %w", err)
	}

	// Start stdin writer right before cmd.Start so there's no window for
	// a goroutine leak — if Start fails, the goroutine exits when the pipe
	// is closed by GC. Earlier failures (buildEnv, pipes) close explicitly.
	if cr.stdinPipe != nil {
		go func() {
			io.WriteString(cr.stdinPipe, promptText)
			cr.stdinPipe.Close()
		}()
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start: %w", err)
	}

	// Collect output concurrently.
	type output struct {
		data        []byte
		truncated   bool
		truncatedAt int
	}
	stdoutCh := make(chan output, 1)
	stderrCh := make(chan output, 1)

	// Recover in each goroutine: CLAUDE.md mandates the agent loop has a
	// top-level recover, but these collector goroutines live outside that
	// scope. A panic here would kill the whole server. Always send a value
	// on the channel so the main goroutine doesn't deadlock.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("driver: stdout collector panic", "panic", r)
				stdoutCh <- output{}
			}
		}()
		data, trunc, truncAt := collectOutput(stdoutPipe, opts.OutputCapBytes)
		stdoutCh <- output{data, trunc, truncAt}
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("driver: stderr collector panic", "panic", r)
				stderrCh <- output{}
			}
		}()
		data, trunc, truncAt := collectOutput(stderrPipe, opts.OutputCapBytes)
		stderrCh <- output{data, trunc, truncAt}
	}()

	stdoutResult := <-stdoutCh
	stderrResult := <-stderrCh

	// Wait for process exit.
	waitErr := cmd.Wait()

	result := Result{
		ExitCode:         exitCode(waitErr),
		Truncated:        stdoutResult.truncated || stderrResult.truncated,
		TruncatedAtBytes: max(stdoutResult.truncatedAt, stderrResult.truncatedAt),
	}

	// Determine cancel/timeout state.
	if timeoutCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.Cancelled = true
	} else if ctx.Err() != nil {
		result.Cancelled = true
	}

	// If output was truncated with kill-on-cap, or the process was
	// cancelled/timed out, tear down the process group. Without this,
	// exec.CommandContext only kills the direct child; grandchild
	// processes spawned under Setpgid=true become orphans.
	needTeardown := (result.Truncated && opts.KillOnOutputCap) || result.Cancelled
	if needTeardown {
		d.teardown(cmd, adapter)
	}

	// Parse output.
	rawStdout := string(stdoutResult.data)
	rawStderr := string(stderrResult.data)
	result.Stdout = d.parseOutput(adapter, rawStdout, logger)
	result.Stderr = rawStderr

	// Apply stderr handling.
	result.Stdout = d.applyStderr(adapter, result.Stdout, result.Stderr)

	// Add session ID footer if applicable. Take the LAST non-empty match
	// rather than the first: streaming-json adapters (claude-code) emit the
	// session id in both the init event and the final event, and in
	// nested/multi-turn flows the canonical id to resume against is the
	// last one written.
	if adapter.Session.SupportsResume && adapter.Output.Parser != nil && adapter.Output.Parser.SessionIDPath != "" {
		p, err := jsonpath.Compile(adapter.Output.Parser.SessionIDPath)
		if err == nil {
			var lastSID string
			for _, line := range splitLines(rawStdout) {
				var obj any
				if json.Unmarshal([]byte(line), &obj) == nil {
					if sid := p.Extract(obj, logger); sid != "" {
						lastSID = sid
					}
				}
			}
			if lastSID != "" {
				result.Stdout += fmt.Sprintf("\n[SESSION_ID: %s]", lastSID)
			}
		}
	}

	// Add truncation marker.
	if result.Truncated {
		result.Stdout += fmt.Sprintf("\n[... truncated %d bytes]", result.TruncatedAtBytes)
	}

	// Raw dump on error/truncation.
	if result.Truncated || result.ExitCode != 0 {
		dumpPath := filepath.Join(os.TempDir(), fmt.Sprintf("workhorse-extagent-%s-%s.log", opts.SessionID, opts.CallID))
		dumpData := rawStdout + "\n--- STDERR ---\n" + rawStderr
		if err := os.WriteFile(dumpPath, []byte(dumpData), 0o600); err == nil {
			result.RawDumpPath = dumpPath
			result.Stdout += fmt.Sprintf("\n[raw output dump: %s]", dumpPath)
		}
	}

	// Add prefix markers.
	if result.TimedOut {
		result.Stdout = "[TIMEOUT] " + result.Stdout
	} else if result.Cancelled {
		result.Stdout = "[CANCELLED] " + result.Stdout
	}

	// Structured log line.
	duration := time.Since(start)
	logger.Info("external_agent.invoke",
		"adapter", adapter.Name,
		"session_id", opts.SessionID,
		"call_id", opts.CallID,
		"duration_ms", duration.Milliseconds(),
		"exit_code", result.ExitCode,
		"cancelled", result.Cancelled,
		"timed_out", result.TimedOut,
		"truncated_bytes", result.TruncatedAtBytes,
		"prompt_chars", len(promptText),
	)

	return result, nil
}

type cmdResult struct {
	cmd       *exec.Cmd
	cleanup   func()
	stdinPipe io.WriteCloser
}

func (d *Driver) buildCmd(ctx context.Context, adapter *extagent.Adapter, promptText string, opts Opts) (*cmdResult, error) {
	var args []string
	var cleanup func()

	switch adapter.Invocation.PromptVia {
	case "arg":
		args = append([]string{adapter.ResolvedBinary}, adapter.Invocation.PromptArg, promptText)
		args = append(args, adapter.Invocation.ExtraArgs...)
	case "stdin":
		args = append([]string{adapter.ResolvedBinary}, adapter.Invocation.ExtraArgs...)
	case "file":
		f, err := os.CreateTemp(os.TempDir(), "workhorse-extagent-prompt-*.txt")
		if err != nil {
			return nil, fmt.Errorf("temp file: %w", err)
		}
		if err := os.WriteFile(f.Name(), []byte(promptText), 0o600); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, err
		}
		f.Close()
		filePath := f.Name()
		cleanup = func() { os.Remove(filePath) }
		args = append([]string{adapter.ResolvedBinary}, adapter.Invocation.PromptArg, filePath)
		args = append(args, adapter.Invocation.ExtraArgs...)
	default:
		return nil, fmt.Errorf("unknown prompt_via: %q", adapter.Invocation.PromptVia)
	}

	// Add resume args.
	if opts.ResumeSessionID != "" && adapter.Session.SupportsResume {
		if adapter.Session.ResumeFlag != "" {
			args = append(args, adapter.Session.ResumeFlag)
		}
		if adapter.Session.SessionIDArg != "" {
			args = append(args, adapter.Session.SessionIDArg, opts.ResumeSessionID)
		}
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)

	var stdinPipe io.WriteCloser
	if adapter.Invocation.PromptVia == "stdin" {
		var err error
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
	}

	return &cmdResult{cmd: cmd, cleanup: cleanup, stdinPipe: stdinPipe}, nil
}

func (d *Driver) buildEnv(adapter *extagent.Adapter) ([]string, error) {
	// Start from parent env.
	parentEnv := os.Environ()
	envMap := make(map[string]string, len(parentEnv))
	for _, e := range parentEnv {
		k, v := splitEnvKV(e)
		envMap[k] = v
	}

	// Restrict to passthrough allowlist.
	passthrough := adapter.Invocation.EnvPassthrough
	if len(passthrough) > 0 {
		restricted := make(map[string]string, len(passthrough))
		for _, k := range passthrough {
			if v, ok := envMap[k]; ok {
				restricted[k] = v
			}
		}
		envMap = restricted
	}

	// Layer env_override.
	for k, v := range adapter.Invocation.EnvOverride {
		envMap[k] = v
	}

	// Inject PATH and HOME if absent.
	if _, ok := envMap["PATH"]; !ok {
		if v, ok := getEnvFromOS("PATH"); ok {
			envMap["PATH"] = v
		}
	}
	if _, ok := envMap["HOME"]; !ok {
		if v, ok := getEnvFromOS("HOME"); ok {
			envMap["HOME"] = v
		}
	}

	// Serialize to []string and apply envfilter LAST.
	var envSlice []string
	for k, v := range envMap {
		envSlice = append(envSlice, k+"="+v)
	}

	kept, dropped := bash.Filter(envSlice)
	bash.LogDropped(d.logger(), dropped)
	return kept, nil
}

func (d *Driver) parseOutput(adapter *extagent.Adapter, raw string, logger *slog.Logger) string {
	if raw == "" {
		return ""
	}

	switch adapter.Output.Format {
	case "text":
		return stripANSI(raw)
	case "jsonl":
		return d.parseJSONL(raw, adapter, logger, false)
	case "streaming-json":
		return d.parseJSONL(raw, adapter, logger, true)
	case "sse":
		return d.parseSSE(raw, adapter, logger)
	default:
		return raw
	}
}

func (d *Driver) parseJSONL(raw string, adapter *extagent.Adapter, logger *slog.Logger, toleratePartial bool) string {
	lines := splitLines(raw)
	var extracted []string
	var jsonObjects []map[string]any

	path := adapter.Output.Parser

	// Compile the JSONPath once, not per line. For long streaming-json
	// outputs (claude-code transcripts emit thousands of events) this
	// turns O(lines × pathTokens) into O(lines).
	var compiled jsonpath.Path
	var compileErr error
	if path != nil && path.AssistantText != "" {
		compiled, compileErr = jsonpath.Compile(path.AssistantText)
		if compileErr != nil {
			logger.Debug("driver: invalid jsonpath", "path", path.AssistantText, "err", compileErr)
		}
	}

	for i, line := range lines {
		if line == "" {
			continue
		}
		var obj any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			if toleratePartial && i == len(lines)-1 {
				continue // tolerate trailing partial
			}
			logger.Debug("driver: jsonl line parse error", "line", line, "err", err)
			continue
		}
		if path != nil && path.AssistantText != "" {
			if compileErr != nil {
				continue
			}
			if s := compiled.Extract(obj, logger); s != "" {
				extracted = append(extracted, s)
			}
		} else {
			if m, ok := obj.(map[string]any); ok {
				jsonObjects = append(jsonObjects, m)
			}
		}
	}

	if len(extracted) > 0 {
		return strings.Join(extracted, "")
	}
	if len(jsonObjects) > 0 {
		data, _ := json.MarshalIndent(jsonObjects, "", "  ")
		return string(data)
	}
	return raw
}

func (d *Driver) parseSSE(raw string, adapter *extagent.Adapter, logger *slog.Logger) string {
	var extracted []string
	path := adapter.Output.Parser

	// Compile the JSONPath once, not per line.
	var compiled jsonpath.Path
	var compileErr error
	if path != nil && path.AssistantText != "" {
		compiled, compileErr = jsonpath.Compile(path.AssistantText)
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var obj any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			logger.Debug("driver: sse data parse error", "data", data, "err", err)
			continue
		}
		if path != nil && path.AssistantText != "" && compileErr == nil {
			if s := compiled.Extract(obj, logger); s != "" {
				extracted = append(extracted, s)
			}
		}
	}

	if len(extracted) > 0 {
		return strings.Join(extracted, "")
	}
	return raw
}

func (d *Driver) applyStderr(adapter *extagent.Adapter, stdout, stderr string) string {
	if stderr == "" {
		return stdout
	}
	switch adapter.Output.Stderr {
	case "separate":
		return stdout + "\n<stderr>\n" + stderr + "\n</stderr>"
	case "merge":
		// Simple merge: append stderr lines after stdout.
		return stdout + "\n" + stderr
	case "ignore":
		return stdout
	default:
		return stdout
	}
}

func (d *Driver) teardown(cmd *exec.Cmd, adapter *extagent.Adapter) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGINT
	switch adapter.Control.CancelSignal {
	case "SIGTERM":
		sig = syscall.SIGTERM
	case "SIGINT":
		// already default
	default:
		d.logger().Warn("driver: unknown cancel_signal, falling back to SIGINT",
			"signal", adapter.Control.CancelSignal, "adapter", adapter.Name)
	}
	killProcessGroup(cmd, sig)

	// Skip grace period if the process has already exited.
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return
	}

	grace := time.Duration(adapter.Control.CancelGraceSec) * time.Second
	if grace <= 0 {
		grace = 5 * time.Second
	}
	time.Sleep(grace)
	killProcessGroup(cmd, syscall.SIGKILL)
}

func (d *Driver) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// collectOutput reads from r up to cap bytes, draining excess to Discard.
func collectOutput(r io.Reader, cap int) (data []byte, truncated bool, truncatedAt int) {
	buf := make([]byte, 0, cap)
	limited := io.LimitReader(r, int64(cap))
	n, _ := io.ReadFull(limited, buf[:cap])
	buf = buf[:n]
	data = buf

	// Drain remaining output. If the drain actually read bytes, the
	// LimitReader capped us — that's genuine truncation. If n == cap but
	// the drain read nothing, the output was exactly cap bytes (no truncation).
	drained, _ := io.Copy(io.Discard, r)
	if drained > 0 {
		truncated = true
		truncatedAt = cap + int(drained)
	}
	return data, truncated, truncatedAt
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func splitEnvKV(e string) (string, string) {
	i := strings.IndexByte(e, '=')
	if i < 0 {
		return e, ""
	}
	return e[:i], e[i+1:]
}

func getEnvFromOS(key string) (string, bool) {
	for _, e := range os.Environ() {
		k, v := splitEnvKV(e)
		if k == key {
			return v, true
		}
	}
	return "", false
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// Ensure the driver struct's result buffer never exceeds cap.
var _ = bytes.TrimSpace
