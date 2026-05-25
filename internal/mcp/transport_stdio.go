package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/tools/bash"
)

// StdioTransport implements Transport over a child process's stdin/stdout. Each
// JSON-RPC message is one newline-delimited line (UTF-8). Responses are matched
// to requests by id; messages without an id are server notifications.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	logger *slog.Logger

	mu       sync.Mutex
	closed   bool
	requests map[int64]chan *Response

	notifCh chan *Request

	writeCh chan *Request
	done    chan struct{}
	exited  chan struct{}
	writeWG sync.WaitGroup
}

// StdioConfig holds the parameters for a stdio transport.
type StdioConfig struct {
	Command string
	Args    []string
	Env     map[string]string
	Logger  *slog.Logger
}

// NewStdioTransport starts the child process and begins reading from its
// stdout/stderr. The returned transport is ready for Call/Notify immediately.
func NewStdioTransport(cfg StdioConfig) (*StdioTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...) //nolint:gosec // MCP servers are operator-configured via ~/.workhorse-agent/mcp.json; no user-provided strings reach here
	cmd.Env = buildEnv(cfg.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdio: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("stdio: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, fmt.Errorf("stdio: start %s: %w", cfg.Command, err)
	}

	t := &StdioTransport{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		logger:   cfg.Logger,
		requests: make(map[int64]chan *Response),
		notifCh:  make(chan *Request, 64),
		writeCh:  make(chan *Request),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}

	go func() {
		_ = t.cmd.Wait()
		close(t.exited)
	}()

	t.writeWG.Add(1)
	go t.writeLoop()
	go t.readLoop()
	go t.stderrLoop()

	return t, nil
}

// Call implements Transport.
func (t *StdioTransport) Call(ctx context.Context, req *Request) (*Response, error) {
	if req.ID == nil {
		return nil, fmt.Errorf("stdio: Call requires a non-nil id")
	}
	respCh := make(chan *Response, 1)

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("stdio: transport closed")
	}
	t.requests[*req.ID] = respCh
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.requests, *req.ID)
		t.mu.Unlock()
	}()

	select {
	case t.writeCh <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, fmt.Errorf("stdio: transport closed")
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, fmt.Errorf("stdio: transport closed")
	}
}

// Notify implements Transport.
func (t *StdioTransport) Notify(ctx context.Context, req *Request) error {
	if req.ID != nil {
		return fmt.Errorf("stdio: Notify requires a nil id")
	}
	select {
	case t.writeCh <- req:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return fmt.Errorf("stdio: transport closed")
	}
}

// Notifications implements Transport.
func (t *StdioTransport) Notifications() <-chan *Request { return t.notifCh }

// Close implements Transport. It signals writeLoop to stop, closes stdin, and
// waits for the child process to exit.
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	close(t.done)
	close(t.writeCh)
	t.stdin.Close()

	// The watchdog goroutine in NewStdioTransport is the only caller of
	// cmd.Wait — calling it twice races on internal Cmd fields. We wait on
	// `t.exited` (closed by that goroutine) instead.
	select {
	case <-t.exited:
	case <-time.After(3 * time.Second):
		_ = t.cmd.Process.Kill()
		<-t.exited
	}

	t.stdout.Close()
	t.stderr.Close()
	t.writeWG.Wait()
	return nil
}

// Process returns the underlying OS process for signal delivery.
func (t *StdioTransport) Process() *os.Process { return t.cmd.Process }

// Exited returns a channel that closes when the child process exits.
func (t *StdioTransport) Exited() <-chan struct{} { return t.exited }

// ---------- internal goroutines ----------

func (t *StdioTransport) writeLoop() {
	defer t.writeWG.Done()
	for req := range t.writeCh {
		b, err := json.Marshal(req)
		if err != nil {
			t.log("write marshal error", "err", err)
			continue
		}
		if _, err := fmt.Fprintf(t.stdin, "%s\n", b); err != nil {
			t.log("write error", "err", err)
			return
		}
	}
}

func (t *StdioTransport) readLoop() {
	sc := bufio.NewScanner(t.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg Request
		if err := json.Unmarshal(line, &msg); err != nil {
			t.log("read unmarshal error", "err", err)
			continue
		}
		if msg.ID != nil {
			var resp Response
			if err := json.Unmarshal(line, &resp); err != nil {
				t.log("response unmarshal error", "err", err)
				continue
			}
			t.mu.Lock()
			ch, ok := t.requests[resp.ID]
			t.mu.Unlock()
			if ok {
				select {
				case ch <- &resp:
				default:
				}
			}
		} else {
			select {
			case t.notifCh <- &msg:
			default:
				t.log("notification dropped, channel full", "method", msg.Method)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.log("stdout read error", "err", err)
	}
}

func (t *StdioTransport) stderrLoop() {
	sc := bufio.NewScanner(t.stderr)
	sc.Buffer(make([]byte, 0, 8*1024), 1*1024*1024)
	for sc.Scan() {
		t.log("mcp.stderr", "text", sc.Text())
	}
}

func (t *StdioTransport) log(msg string, args ...interface{}) {
	if t.logger != nil {
		t.logger.Info(msg, args...)
	}
}

// buildEnv filters the parent process environment through the bash envfilter and
// merges the MCP-configured extras on top.
func buildEnv(extra map[string]string) []string {
	kept, _ := bash.Filter(os.Environ())
	if len(extra) == 0 {
		return kept
	}
	filtered, _ := bash.FilterMap(extra)
	for k, v := range filtered {
		kept = append(kept, k+"="+v)
	}
	return kept
}
