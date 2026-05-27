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
	Stdout         string
	Stderr         string
	ExitCode       int
	Cancelled      bool
	TimedOut       bool
	Truncated      bool
	TruncatedAtBytes int
	RawDumpPath    string
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

	// Compute effective timeout.
	effectiveTimeout := time.Duration(adapter.Control.DefaultTimeoutSec) * time.Second
	if opts.TimeoutSec > 0 {
		effectiveTimeout = time.Duration(opts.TimeoutSec) * time.Second
	}
	maxTimeout := time.Duration(adapter.Control.MaxTimeoutSec) * time.Second
	if effectiveTimeout > maxTimeout {
		effectiveTimeout = maxTimeout
	}

	// Wrap with timeout context.
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, effectiveTimeout)
	defer timeoutCancel()

	// Build argv.
	cmd, cleanup, err := d.buildCmd(timeoutCtx, adapter, promptText, opts)
	if err != nil {
		return Result{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Set process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Build env.
	cmd.Env, err = d.buildEnv(adapter)
	if err != nil {
		return Result{}, err
	}
	if cmd.Dir == "" && opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}

	// Pipes.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start: %w", err)
	}

	// Collect output concurrently.
	type output struct {
		data []byte
		truncated bool
		truncatedAt int
	}
	stdoutCh := make(chan output, 1)
	stderrCh := make(chan output, 1)

	go func() {
		data, trunc, truncAt := collectOutput(stdoutPipe, opts.OutputCapBytes)
		stdoutCh <- output{data, trunc, truncAt}
	}()
	go func() {
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

	// If truncated and kill-on-cap, run teardown.
	if result.Truncated && opts.KillOnOutputCap {
		d.teardown(cmd, adapter)
	}

	// Parse output.
	rawStdout := string(stdoutResult.data)
	rawStderr := string(stderrResult.data)
	result.Stdout = d.parseOutput(adapter, rawStdout, logger)
	result.Stderr = rawStderr

	// Apply stderr handling.
	result.Stdout = d.applyStderr(adapter, result.Stdout, result.Stderr)

	// Add session ID footer if applicable.
	if adapter.Session.SupportsResume && adapter.Output.Parser != nil && adapter.Output.Parser.SessionIDPath != "" {
		p, err := jsonpath.Compile(adapter.Output.Parser.SessionIDPath)
		if err == nil {
			var lines []string
			for _, line := range splitLines(rawStdout) {
				var obj any
				if json.Unmarshal([]byte(line), &obj) == nil {
					if sid := p.Extract(obj, logger); sid != "" {
						lines = append(lines, sid)
					}
				}
			}
			if len(lines) > 0 {
				result.Stdout += fmt.Sprintf("\n[SESSION_ID: %s]", lines[0])
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

func (d *Driver) buildCmd(ctx context.Context, adapter *extagent.Adapter, promptText string, opts Opts) (*exec.Cmd, func(), error) {
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
			return nil, nil, fmt.Errorf("temp file: %w", err)
		}
		if err := os.WriteFile(f.Name(), []byte(promptText), 0o600); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, nil, err
		}
		f.Close()
		filePath := f.Name()
		cleanup = func() { os.Remove(filePath) }
		args = append([]string{adapter.ResolvedBinary}, adapter.Invocation.PromptArg, filePath)
		args = append(args, adapter.Invocation.ExtraArgs...)
	default:
		return nil, nil, fmt.Errorf("unknown prompt_via: %q", adapter.Invocation.PromptVia)
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

	if adapter.Invocation.PromptVia == "stdin" {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, cleanup, err
		}
		go func() {
			io.WriteString(stdin, promptText)
			stdin.Close()
		}()
	}

	return cmd, cleanup, nil
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
			p, err := jsonpath.Compile(path.AssistantText)
			if err != nil {
				logger.Debug("driver: invalid jsonpath", "path", path.AssistantText, "err", err)
				continue
			}
			if s := p.Extract(obj, logger); s != "" {
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
		if path != nil && path.AssistantText != "" {
			p, err := jsonpath.Compile(path.AssistantText)
			if err != nil {
				continue
			}
			if s := p.Extract(obj, logger); s != "" {
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
	if adapter.Control.CancelSignal == "SIGTERM" {
		sig = syscall.SIGTERM
	}
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, sig)

	grace := time.Duration(adapter.Control.CancelGraceSec) * time.Second
	if grace <= 0 {
		grace = 5 * time.Second
	}
	time.Sleep(grace)
	_ = syscall.Kill(pgid, syscall.SIGKILL)
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
	truncatedAt = cap
	limited := io.LimitReader(r, int64(cap))
	n, _ := io.ReadFull(limited, buf[:cap])
	buf = buf[:n]
	data = buf

	// Check if there's more.
	_, err := io.Copy(io.Discard, r)
	_ = err
	// If the LimitReader hit EOF vs limit, we can tell from the read count.
	if n >= cap {
		// Drain completed — there was more data.
		truncated = true
	} else {
		truncatedAt = 0
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
