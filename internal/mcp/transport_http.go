package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// HTTPTransport implements Transport over MCP Streamable HTTP (2025-11-25).
// Requests are sent via HTTP POST; server-initiated notifications arrive on a
// long-lived GET SSE stream. The transport captures and reuses the
// Mcp-Session-Id header automatically.
type HTTPTransport struct {
	url        string
	authHeader string
	client     *http.Client
	logger     *slog.Logger

	mu        sync.Mutex
	closed    bool
	sessionID string

	notifCh chan *Request

	sseCtx    context.Context
	sseCancel context.CancelFunc
	sseDone   chan struct{}
}

// HTTPConfig holds the parameters for an HTTP transport.
type HTTPConfig struct {
	URL        string
	AuthHeader string
	Logger     *slog.Logger
}

// NewHTTPTransport creates an HTTP transport and immediately starts the SSE
// notification listener.
func NewHTTPTransport(cfg HTTPConfig) *HTTPTransport {
	sseCtx, sseCancel := context.WithCancel(context.Background())
	t := &HTTPTransport{
		url:        cfg.URL,
		authHeader: cfg.AuthHeader,
		client:     &http.Client{Timeout: 30 * time.Second},
		logger:     cfg.Logger,
		notifCh:    make(chan *Request, 64),
		sseCtx:     sseCtx,
		sseCancel:  sseCancel,
		sseDone:    make(chan struct{}),
	}
	go t.sseLoop()
	return t
}

// Call implements Transport.
func (t *HTTPTransport) Call(ctx context.Context, req *Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("http: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()
	if sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	if t.authHeader != "" {
		httpReq.Header.Set("Authorization", t.authHeader)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: post %s: %w", t.url, err)
	}
	defer resp.Body.Close()

	// Capture session ID if the server sends one.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("http: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http: %s returned %d: %s", t.url, resp.StatusCode, string(respBody))
	}

	// For notifications (nil id), no response body is expected.
	if req.ID == nil {
		return nil, nil
	}

	var rpcResp Response
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("http: unmarshal response: %w", err)
	}
	return &rpcResp, nil
}

// Notify implements Transport.
func (t *HTTPTransport) Notify(ctx context.Context, req *Request) error {
	_, err := t.Call(ctx, req)
	return err
}

// Notifications implements Transport.
func (t *HTTPTransport) Notifications() <-chan *Request { return t.notifCh }

// SessionID returns the Mcp-Session-Id captured from the last response.
func (t *HTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// Close implements Transport. It cancels the SSE loop and waits for it to exit.
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	t.sseCancel()
	<-t.sseDone
	return nil
}

// ---------- SSE notification listener ----------

func (t *HTTPTransport) sseLoop() {
	defer close(t.sseDone)

	backoff := []time.Duration{0, 1 * time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second}
	failures := 0

	for {
		select {
		case <-t.sseCtx.Done():
			return
		default:
		}

		t.connectSSE(backoff[failures])

		failures++
		if failures >= len(backoff) {
			failures = len(backoff) - 1
		}
	}
}

func (t *HTTPTransport) connectSSE(wait time.Duration) {
	// Wait before connecting (0 on first attempt).
	select {
	case <-time.After(wait):
	case <-t.sseCtx.Done():
		return
	}

	req, err := http.NewRequestWithContext(t.sseCtx, http.MethodGet, t.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	t.mu.Lock()
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	t.mu.Unlock()
	if t.authHeader != "" {
		req.Header.Set("Authorization", t.authHeader)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		t.log("sse connect error", "err", err)
		return
	}
	defer resp.Body.Close()

	// Capture session ID from SSE response too.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	t.readSSE(resp.Body)
}

func (t *HTTPTransport) readSSE(r io.Reader) {
	err := provider.ParseSSE(r, func(ev provider.SSEEvent) error {
		if len(ev.Data) == 0 {
			return nil
		}
		var msg Request
		if err := json.Unmarshal(ev.Data, &msg); err != nil {
			t.log("sse unmarshal error", "err", err)
			return nil
		}
		if msg.Method == "" {
			return nil
		}
		select {
		case t.notifCh <- &msg:
		case <-t.sseCtx.Done():
			return t.sseCtx.Err()
		default:
			t.log("notification dropped, channel full", "method", msg.Method)
		}
		return nil
	})
	if err != nil && err != context.Canceled {
		t.log("sse read error", "err", err)
	}
}

func (t *HTTPTransport) log(msg string, args ...interface{}) {
	if t.logger != nil {
		t.logger.Info(msg, args...)
	}
}
