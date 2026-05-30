// Package anthropic implements provider.Provider against the Anthropic
// Messages API (https://api.anthropic.com/v1/messages). The HTTP request is
// hand-rolled — no SDK — and the SSE stream is mapped from Anthropic's event
// types onto the internal ProviderEvent kinds per the provider-abstraction
// spec.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
	"github.com/wallfacers/workhorse-agent/internal/version"
)

// DefaultBaseURL is used when Options.BaseURL is empty.
const DefaultBaseURL = "https://api.anthropic.com"

// APIVersion is the Anthropic-Version header value we send. Update only when
// we've tested against a newer Messages API revision.
const APIVersion = "2023-06-01"

// Options configures one Anthropic adapter instance.
type Options struct {
	APIKey  string
	BaseURL string
	// HTTPClient is optional; tests inject a stub. nil falls back to
	// http.DefaultClient with no timeout (SSE connections are long-lived).
	HTTPClient *http.Client
	// MaxTokens caps every request when Request.MaxTokens is zero. Anthropic
	// requires the field, so we default to a generous 4096 if unset.
	DefaultMaxTokens int
}

// Provider is the concrete Anthropic adapter.
type Provider struct {
	opts Options
}

var _ provider.Provider = (*Provider)(nil)

// New constructs a Provider. Caller is responsible for keeping APIKey out of
// logs; the adapter itself never prints it.
func New(opts Options) *Provider {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{} // no timeout: SSE is long-lived
	}
	if opts.DefaultMaxTokens == 0 {
		opts.DefaultMaxTokens = 4096
	}
	return &Provider{opts: opts}
}

func (p *Provider) Name() string { return "anthropic" }

// Stream submits req as a streaming Messages call and returns a channel that
// emits one ProviderEvent per logical change. The channel closes after the
// terminal `stop` or `error` event.
//
// Stream follows the strict semantics from the provider spec: a non-nil
// returned error means the HTTP request never went out, the channel is nil.
// Once Stream returns (channel, nil), all subsequent failures are reported
// as `error` ProviderEvents on the channel.
func (p *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.ProviderEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, provider.NewProviderError(p.Name(), 0, provider.CodeCanceled, "context canceled before request", err)
	}
	body, err := encodeRequest(req, p.opts.DefaultMaxTokens)
	if err != nil {
		return nil, provider.NewProviderError(p.Name(), 0, provider.CodeInvalidRequest, "encode request", err)
	}
	url := strings.TrimRight(p.opts.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewProviderError(p.Name(), 0, provider.CodeInvalidRequest, "build http request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Anthropic-Version", APIVersion)
	httpReq.Header.Set("User-Agent", version.UserAgent())
	httpReq.Header.Set("x-api-key", p.opts.APIKey)
	if req.ThinkingEnabled {
		httpReq.Header.Set("anthropic-beta", BetaHeaderInterleavedThinking)
	}

	resp, err := p.opts.HTTPClient.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, provider.NewProviderError(p.Name(), 0, provider.CodeCanceled, "request canceled", err)
		}
		return nil, provider.NewProviderError(p.Name(), 0, provider.CodeNetworkError, "transport error", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close() //nolint:errcheck
		return nil, parseErrorResponse(p.Name(), resp)
	}

	ch := make(chan provider.ProviderEvent, 16)
	go p.streamLoop(ctx, resp, ch)
	return ch, nil
}

// streamLoop consumes SSE events, maps them, and closes ch when done. Closing
// ch is this function's exclusive responsibility.
func (p *Provider) streamLoop(ctx context.Context, resp *http.Response, ch chan<- provider.ProviderEvent) {
	defer close(ch)
	defer resp.Body.Close() //nolint:errcheck

	st := &anthropicStreamState{}

	emit := func(ev provider.ProviderEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	parseErr := provider.ParseSSE(resp.Body, func(ev provider.SSEEvent) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		out, terminal, err := st.handle(ev)
		if err != nil {
			pe := provider.NewProviderError(p.Name(), 0, provider.CodeStreamBroken, "decode anthropic event", err)
			emit(provider.ProviderEvent{Type: provider.EventError, Error: pe})
			return io.EOF // stop parsing
		}
		for _, e := range out {
			if !emit(e) {
				return ctx.Err()
			}
		}
		if terminal {
			return io.EOF
		}
		return nil
	})

	if parseErr != nil && !errors.Is(parseErr, io.EOF) {
		// Network drop / parser hard-fail mid-stream.
		var pe *provider.ProviderError
		if errors.Is(parseErr, context.Canceled) {
			pe = provider.NewProviderError(p.Name(), 0, provider.CodeCanceled, "stream canceled", parseErr)
		} else {
			pe = provider.NewProviderError(p.Name(), 0, provider.CodeStreamBroken, "sse read error", parseErr)
		}
		emit(provider.ProviderEvent{Type: provider.EventError, Error: pe})
	}
}

// parseErrorResponse maps an Anthropic non-200 to a ProviderError. The error
// body shape is `{"type":"error","error":{"type":"...","message":"..."}}`.
func parseErrorResponse(provName string, resp *http.Response) *provider.ProviderError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)

	code, msg := classifyAnthropicError(resp.StatusCode, env.Error.Type, env.Error.Message)
	pe := provider.NewProviderError(provName, resp.StatusCode, code, msg, nil)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if d := parseRetryAfter(ra); d > 0 {
			pe.SetRetryAfter(d)
		}
	}
	return pe
}

func classifyAnthropicError(status int, errType, msg string) (code, message string) {
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch errType {
	case "authentication_error":
		return provider.CodeAuthFailed, msg
	case "permission_error":
		return provider.CodeAuthFailed, msg
	case "invalid_request_error":
		// Anthropic packs "context_length_exceeded" into invalid_request_error,
		// but the message string is the only hint.
		if strings.Contains(strings.ToLower(msg), "context") &&
			strings.Contains(strings.ToLower(msg), "length") {
			return provider.CodeContextLengthExceeded, msg
		}
		return provider.CodeInvalidRequest, msg
	case "rate_limit_error":
		return provider.CodeRateLimited, msg
	case "api_error", "overloaded_error":
		return provider.CodeServerError, msg
	}
	// Fall back to HTTP status when the error type was missing or unknown.
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return provider.CodeAuthFailed, msg
	case status == http.StatusTooManyRequests:
		return provider.CodeRateLimited, msg
	case status == http.StatusBadRequest:
		return provider.CodeInvalidRequest, msg
	case status >= 500:
		return provider.CodeServerError, msg
	default:
		return provider.CodeInvalidRequest, msg
	}
}

// parseRetryAfter supports both seconds-only and HTTP-date forms.
func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// BetaHeaderInterleavedThinking is the Anthropic beta header for extended
// thinking. TODO: remove when promoted from beta.
const BetaHeaderInterleavedThinking = "interleaved-thinking-2025-05-14"

// knownThinkingModels is the set of Anthropic model IDs that support extended
// thinking. This set must be updated when Anthropic adds support to new models.
var knownThinkingModels = map[string]bool{
	"claude-sonnet-4-6":          true,
	"claude-opus-4-8":            true,
	"claude-sonnet-4-5-20250514": true,
	"claude-sonnet-4-20250514":   true,
	"claude-opus-4-20250115":     true,
}

// ErrThinkingNotSupported is returned when thinking is requested for a model
// that does not support extended thinking.
var ErrThinkingNotSupported = fmt.Errorf("extended thinking not supported by model")

// supportsThinking reports whether model can run extended thinking. It matches
// knownThinkingModels exactly, then retries after stripping a trailing
// "-YYYYMMDD" snapshot suffix so a dated pin of a listed base model (e.g.
// "claude-sonnet-4-6-20250514") is accepted without the allowlist having to
// enumerate every dated build.
func supportsThinking(model string) bool {
	if knownThinkingModels[model] {
		return true
	}
	if i := strings.LastIndex(model, "-"); i >= 0 {
		if suffix := model[i+1:]; len(suffix) == 8 && isAllDigits(suffix) {
			return knownThinkingModels[model[:i]]
		}
	}
	return false
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// encodeRequest turns our internal Request into Anthropic's JSON shape.
func encodeRequest(r provider.Request, defaultMax int) ([]byte, error) {
	if r.ThinkingEnabled && !supportsThinking(r.Model) {
		return nil, ErrThinkingNotSupported
	}

	max := r.MaxTokens
	if max <= 0 {
		max = defaultMax
	}
	// Anthropic requires max_tokens > thinking.budget_tokens (max_tokens is the
	// total output budget that thinking draws from). Config validation catches
	// this at startup; guard here too so a per-session override can never ship
	// a request that 400s.
	if r.ThinkingEnabled && max <= r.ThinkingBudgetTokens {
		return nil, fmt.Errorf("max_tokens (%d) must be greater than thinking budget_tokens (%d)", max, r.ThinkingBudgetTokens)
	}
	body := anthropicReq{
		Model:     r.Model,
		MaxTokens: max,
		Stream:    true,
	}

	if r.System != "" {
		blocks := []systemBlock{{
			Type:         "text",
			Text:         r.System,
			CacheControl: &cacheControl{Type: "ephemeral"},
		}}
		sysJSON, err := json.Marshal(blocks)
		if err != nil {
			return nil, fmt.Errorf("marshal system blocks: %w", err)
		}
		body.System = sysJSON
	}

	if r.ThinkingEnabled {
		body.Thinking = &thinkingReq{
			Type:         "enabled",
			BudgetTokens: r.ThinkingBudgetTokens,
		}
	} else if r.Temperature > 0 {
		body.Temperature = r.Temperature
	}

	var stripBefore int
	if r.ThinkingEnabled {
		stripBefore = findLastEndTurn(r.Messages)
	}
	for i, m := range r.Messages {
		if m.Role == provider.RoleSystem {
			continue // already in body.System
		}
		if r.ThinkingEnabled && i < stripBefore {
			m = stripThinkingBlocks(m)
		}
		am, err := toAnthropicMessage(m)
		if err != nil {
			return nil, err
		}
		// Stripping thinking can empty an all-thinking assistant turn; never
		// emit a message with no content blocks — Anthropic rejects
		// content:null / [].
		if len(am.Content) == 0 {
			continue
		}
		body.Messages = append(body.Messages, am)
	}
	for _, t := range r.Tools {
		body.Tools = append(body.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return json.Marshal(&body)
}

func toAnthropicMessage(m provider.Message) (anthropicMsg, error) {
	// System messages aren't a role in Anthropic; the caller already moved
	// system text to Request.System. Anything labelled "system" here we drop
	// — it should not have made it this far.
	if m.Role == provider.RoleSystem {
		return anthropicMsg{Role: "user"}, nil // defensive; treat as user
	}
	out := anthropicMsg{Role: string(m.Role)}
	for _, b := range m.Content {
		ab, err := toAnthropicBlock(b)
		if err != nil {
			return anthropicMsg{}, err
		}
		out.Content = append(out.Content, ab)
	}
	return out, nil
}

func toAnthropicBlock(b provider.ContentBlock) (anthropicBlock, error) {
	switch b.Type {
	case provider.BlockText:
		return anthropicBlock{Type: "text", Text: b.Text}, nil
	case provider.BlockToolUse:
		return anthropicBlock{
			Type:  "tool_use",
			ID:    b.ToolUseID,
			Name:  b.ToolName,
			Input: b.Input,
		}, nil
	case provider.BlockToolResult:
		blk := anthropicBlock{
			Type:      "tool_result",
			ToolUseID: b.ToolUseID,
			Content:   b.Output,
		}
		if b.IsError {
			blk.IsError = true
		}
		return blk, nil
	case provider.BlockThinking:
		return anthropicBlock{
			Type:      "thinking",
			Thinking:  b.Thinking,
			Signature: b.Signature,
		}, nil
	case provider.BlockRedactedThinking:
		return anthropicBlock{
			Type: "redacted_thinking",
			Data: b.RedactedData,
		}, nil
	default:
		// Unknown block types are a programming error (a new BlockType wired
		// into history but not here). Fail loudly rather than coercing a
		// signature-bearing block into text and silently corrupting the turn.
		return anthropicBlock{}, fmt.Errorf("anthropic: cannot serialize unknown content block type %q", b.Type)
	}
}

// findLastEndTurn scans messages from back to front looking for the assistant
// message whose stop_reason was "end_turn". It returns the index *after* that
// message (i.e. the first message of the active tool-use chain). All thinking
// blocks in messages before that index will be stripped. Returns 0 if no
// prior end_turn is found (keep everything — first turn).
func findLastEndTurn(msgs []provider.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != provider.RoleAssistant {
			continue
		}
		if turnClosed(m) {
			return i + 1
		}
	}
	return 0
}

// turnClosed reports whether an assistant turn ended the chain (anything other
// than a tool_use continuation). It prefers the real provider stop_reason and
// falls back to the structural "no tool_use block ⇒ closed" heuristic for
// messages that carry no stop_reason (history restored from the store).
func turnClosed(m provider.Message) bool {
	if m.StopReason != "" {
		return m.StopReason != "tool_use"
	}
	for _, b := range m.Content {
		if b.Type == provider.BlockToolUse {
			return false
		}
	}
	return true
}

// stripThinkingBlocks returns a copy of m with all thinking/redacted_thinking
// blocks removed. Non-thinking blocks are preserved in order.
func stripThinkingBlocks(m provider.Message) provider.Message {
	filtered := make([]provider.ContentBlock, 0, len(m.Content))
	for _, b := range m.Content {
		if b.Type != provider.BlockThinking && b.Type != provider.BlockRedactedThinking {
			filtered = append(filtered, b)
		}
	}
	m.Content = filtered
	return m
}

// EncodeRequestForTest exposes encodeRequest for unit tests.
func EncodeRequestForTest(r provider.Request) ([]byte, error) {
	return encodeRequest(r, 4096)
}
