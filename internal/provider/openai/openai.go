// Package openai implements provider.Provider against the OpenAI Chat
// Completions API. The translation between our internal Message format and
// OpenAI's role:"tool" / tool_calls shape is the tricky bit; see translate.go.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// DefaultBaseURL is used when Options.BaseURL is empty.
const DefaultBaseURL = "https://api.openai.com/v1"

// Options configures one OpenAI adapter instance. BaseURL may point at an
// OpenAI-compatible third party (DeepSeek, Qwen, Ollama, ...) — per spec we
// don't promise to support those, but technical compatibility is fine.
type Options struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type Provider struct {
	opts Options
}

var _ provider.Provider = (*Provider)(nil)

func New(opts Options) *Provider {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	return &Provider{opts: opts}
}

func (p *Provider) Name() string { return "openai" }

func (p *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.ProviderEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, provider.NewProviderError(p.Name(), 0, provider.CodeCanceled, "context canceled before request", err)
	}
	body, err := encodeRequest(req)
	if err != nil {
		return nil, provider.NewProviderError(p.Name(), 0, provider.CodeInvalidRequest, "encode request", err)
	}
	url := strings.TrimRight(p.opts.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewProviderError(p.Name(), 0, provider.CodeInvalidRequest, "build http request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+p.opts.APIKey)

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

func (p *Provider) streamLoop(ctx context.Context, resp *http.Response, ch chan<- provider.ProviderEvent) {
	defer close(ch)
	defer resp.Body.Close() //nolint:errcheck

	st := &openaiStreamState{toolCalls: map[int]*toolCallBuf{}}

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
		// OpenAI sends [DONE] as the terminator; the final chunk before it
		// carries finish_reason.
		if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
			for _, e := range st.flushTerminal() {
				if !emit(e) {
					return ctx.Err()
				}
			}
			return io.EOF
		}
		out, terminal, err := st.handle(ev.Data)
		if err != nil {
			st.sawError = true
			pe := provider.NewProviderError(p.Name(), 0, provider.CodeStreamBroken, "decode openai chunk", err)
			emit(provider.ProviderEvent{Type: provider.EventError, Error: pe})
			return io.EOF
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

	// Flush remaining tool calls and emit a stop event when the stream
	// ended cleanly (graceful close or [DONE]-terminated). On mid-stream
	// errors, skip flushing — half-accumulated tool_call fragments would
	// create orphan pending entries upstream.
	if parseErr == nil || errors.Is(parseErr, io.EOF) {
		for _, e := range st.flushTerminal() {
			if !emit(e) {
				break
			}
		}
	}

	if parseErr != nil && !errors.Is(parseErr, io.EOF) {
		var pe *provider.ProviderError
		if errors.Is(parseErr, context.Canceled) {
			pe = provider.NewProviderError(p.Name(), 0, provider.CodeCanceled, "stream canceled", parseErr)
		} else {
			pe = provider.NewProviderError(p.Name(), 0, provider.CodeStreamBroken, "sse read error", parseErr)
		}
		emit(provider.ProviderEvent{Type: provider.EventError, Error: pe})
	}
}

// ---- error parsing ----

func parseErrorResponse(provName string, resp *http.Response) *provider.ProviderError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var env openaiErrEnvelope
	_ = json.Unmarshal(body, &env)
	code, msg := classifyOpenAIError(resp.StatusCode, env.Error.Type, env.Error.Code, env.Error.Message)
	pe := provider.NewProviderError(provName, resp.StatusCode, code, msg, nil)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && n > 0 {
			pe.SetRetryAfter(time.Duration(n) * time.Second)
		} else if t, err := http.ParseTime(ra); err == nil {
			if d := time.Until(t); d > 0 {
				pe.SetRetryAfter(d)
			}
		}
	}
	return pe
}

func classifyOpenAIError(status int, errType, code, msg string) (string, string) {
	if msg == "" {
		msg = http.StatusText(status)
	}
	// OpenAI's `code` is more specific than `type`; check it first.
	switch code {
	case "rate_limit_exceeded":
		return provider.CodeRateLimited, msg
	case "invalid_api_key":
		return provider.CodeAuthFailed, msg
	case "context_length_exceeded":
		return provider.CodeContextLengthExceeded, msg
	case "insufficient_quota":
		return provider.CodeInsufficientQuota, msg
	}
	switch errType {
	case "invalid_request_error":
		// context length sometimes goes here too
		if strings.Contains(strings.ToLower(msg), "context") &&
			strings.Contains(strings.ToLower(msg), "length") {
			return provider.CodeContextLengthExceeded, msg
		}
		return provider.CodeInvalidRequest, msg
	case "authentication_error":
		return provider.CodeAuthFailed, msg
	case "rate_limit_error":
		return provider.CodeRateLimited, msg
	case "server_error", "api_error":
		return provider.CodeServerError, msg
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return provider.CodeAuthFailed, msg
	case status == http.StatusTooManyRequests:
		return provider.CodeRateLimited, msg
	case status == http.StatusBadRequest:
		return provider.CodeInvalidRequest, msg
	case status >= 500:
		return provider.CodeServerError, msg
	}
	return provider.CodeInvalidRequest, msg
}

// ---- request encoding ----

func encodeRequest(r provider.Request) ([]byte, error) {
	out := openaiReq{
		Model:       r.Model,
		Stream:      true,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
	}
	if r.System != "" {
		out.Messages = append(out.Messages, openaiMsg{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		out.Messages = append(out.Messages, toOpenAIMessages(m)...)
	}
	for _, t := range r.Tools {
		tool := openaiTool{Type: "function"}
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = t.InputSchema
		out.Tools = append(out.Tools, tool)
	}
	return json.Marshal(&out)
}

// toOpenAIMessages translates a single internal Message into one OpenAI
// assistant/user message plus any number of role:"tool" messages.
//
// Spec rules:
//   - assistant text comes before tool_calls (OpenAI rejects interleaving).
//   - each tool_result becomes its own role:"tool" message with matching
//     tool_call_id.
//   - user messages with text plain become role:"user" with content.
func toOpenAIMessages(m provider.Message) []openaiMsg {
	switch m.Role {
	case provider.RoleAssistant:
		var textParts []string
		var toolCalls []openaiToolCall
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockText:
				textParts = append(textParts, b.Text)
			case provider.BlockToolUse:
				tc := openaiToolCall{ID: b.ToolUseID, Type: "function"}
				tc.Function.Name = b.ToolName
				if len(b.Input) > 0 {
					tc.Function.Arguments = string(b.Input)
				} else {
					tc.Function.Arguments = "{}"
				}
				toolCalls = append(toolCalls, tc)
			}
		}
		msg := openaiMsg{Role: "assistant", Content: strings.Join(textParts, ""), ToolCalls: toolCalls}
		return []openaiMsg{msg}
	case provider.RoleUser:
		// User messages may include tool_results (paired with prior tool_use).
		// Each tool_result becomes a standalone {role:"tool"} message; the
		// remaining text content goes into a normal user message.
		var textParts []string
		var toolMsgs []openaiMsg
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockText:
				textParts = append(textParts, b.Text)
			case provider.BlockToolResult:
				toolMsgs = append(toolMsgs, openaiMsg{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    b.Output,
				})
			}
		}
		out := append([]openaiMsg{}, toolMsgs...)
		if len(textParts) > 0 || len(toolMsgs) == 0 {
			out = append(out, openaiMsg{Role: "user", Content: strings.Join(textParts, "")})
		}
		return out
	case provider.RoleSystem:
		var sb strings.Builder
		for _, b := range m.Content {
			if b.Type == provider.BlockText {
				sb.WriteString(b.Text)
			}
		}
		return []openaiMsg{{Role: "system", Content: sb.String()}}
	}
	return nil
}

// ---- streaming state machine ----

type openaiStreamState struct {
	textSeen   bool
	finished   bool
	sawError   bool
	toolCalls  map[int]*toolCallBuf // index → accumulator
	stopReason string
}

type toolCallBuf struct {
	id        string
	name      string
	arguments []byte
}

// handle digests one chunk JSON. Returns events to emit, whether the stream
// should close, and any decode error.
func (s *openaiStreamState) handle(data []byte) ([]provider.ProviderEvent, bool, error) {
	var chunk openaiChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, false, err
	}
	if len(chunk.Choices) == 0 {
		return nil, false, nil
	}
	choice := chunk.Choices[0]
	var out []provider.ProviderEvent

	if choice.Delta.Content != "" {
		s.textSeen = true
		out = append(out, provider.ProviderEvent{
			Type:      provider.EventTextDelta,
			TextDelta: choice.Delta.Content,
		})
	}

	for _, tc := range choice.Delta.ToolCalls {
		buf, ok := s.toolCalls[tc.Index]
		if !ok {
			buf = &toolCallBuf{}
			s.toolCalls[tc.Index] = buf
		}
		if tc.ID != "" {
			buf.id = tc.ID
		}
		if tc.Function.Name != "" {
			buf.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			buf.arguments = append(buf.arguments, tc.Function.Arguments...)
		}
	}

	if choice.FinishReason != "" {
		s.stopReason = mapFinishReason(choice.FinishReason)
	}

	// Some non-streaming responses come back here with choice.Message set; we
	// also support that path for tests / fallback.
	if choice.Message != nil {
		if choice.Message.Content != "" {
			out = append(out, provider.ProviderEvent{
				Type:      provider.EventTextDelta,
				TextDelta: choice.Message.Content,
			})
		}
		for _, tc := range choice.Message.ToolCalls {
			block := &provider.ContentBlock{
				Type:      provider.BlockToolUse,
				ToolUseID: tc.ID,
				ToolName:  tc.Function.Name,
				Input:     json.RawMessage(tc.Function.Arguments),
			}
			if len(block.Input) == 0 {
				block.Input = json.RawMessage("{}")
			}
			out = append(out, provider.ProviderEvent{Type: provider.EventToolUse, ToolUse: block})
		}
	}

	if chunk.Usage != nil {
		out = append(out, provider.ProviderEvent{
			Type: provider.EventUsage,
			Usage: &provider.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			},
		})
	}

	return out, false, nil
}

// flushTerminal is called when [DONE] is seen. It drains the tool_call
// accumulators in index order and emits a final stop event.
func (s *openaiStreamState) flushTerminal() []provider.ProviderEvent {
	if s.finished || s.sawError {
		return nil
	}
	s.finished = true

	var out []provider.ProviderEvent
	idxs := make([]int, 0, len(s.toolCalls))
	for i := range s.toolCalls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		buf := s.toolCalls[i]
		input := buf.arguments
		if len(input) == 0 {
			input = []byte("{}")
		}
		var probe any
		if err := json.Unmarshal(input, &probe); err == nil {
			out = append(out, provider.ProviderEvent{
				Type: provider.EventToolUse,
				ToolUse: &provider.ContentBlock{
					Type:      provider.BlockToolUse,
					ToolUseID: buf.id,
					ToolName:  buf.name,
					Input:     append([]byte(nil), input...),
				},
			})
		} else {
			out = append(out, provider.ProviderEvent{
				Type:  provider.EventError,
				Error: provider.NewProviderError("openai", 0, provider.CodeStreamBroken, "tool_call arguments not valid JSON", err),
			})
		}
	}

	reason := s.stopReason
	if reason == "" {
		reason = "stop"
	}
	out = append(out, provider.ProviderEvent{Type: provider.EventStop, StopReason: reason})
	return out
}

func mapFinishReason(r string) string {
	switch r {
	case "tool_calls":
		return "tool_use"
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return r
	}
}
