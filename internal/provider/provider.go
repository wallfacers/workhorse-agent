// Package provider defines the LLM-vendor boundary. A single Provider
// interface streams events; concrete adapters (anthropic, openai) translate to
// and from each vendor's HTTP + SSE shape. The internal Message format is the
// canonical history representation and is owned by this package.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Provider is the contract every LLM adapter implements. Stream's return
// values follow strict semantics — see the provider-abstraction spec for the
// full rules. Briefly: a non-nil error means the request never went out;
// otherwise the channel carries the stream and closes after a stop/error
// event.
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request) (<-chan ProviderEvent, error)
}

// Request is the provider-agnostic input for a single inference call. Adapters
// translate Messages/Tools to their vendor's shape and serialise on the way
// out.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolSchema
	MaxTokens   int
	Temperature float64

	ThinkingEnabled      bool
	ThinkingBudgetTokens int
}

// ToolSchema is one JSON-schema tool advertised to the model. The InputSchema
// must be a valid JSON object literal; adapters embed it directly in their
// vendor format.
type ToolSchema struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Role enumerates the canonical message author values. We mirror Anthropic's
// vocabulary because it's the simpler model; the OpenAI adapter maps to/from
// its own roles when serialising.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Message is one conversation turn. The Content slice may contain a mix of
// text / tool_use / tool_result blocks; the order is significant.
type Message struct {
	Role    Role
	Content []ContentBlock
}

// BlockType discriminates ContentBlock contents.
type BlockType string

const (
	BlockText             BlockType = "text"
	BlockToolUse          BlockType = "tool_use"
	BlockToolResult       BlockType = "tool_result"
	BlockThinking         BlockType = "thinking"
	BlockRedactedThinking BlockType = "redacted_thinking"
)

// ContentBlock is a discriminated union. Only the fields appropriate for Type
// are populated; the rest are zero-valued.
type ContentBlock struct {
	Type BlockType

	// for Type == BlockText
	Text string

	// for Type == BlockToolUse
	ToolUseID string
	ToolName  string
	Input     json.RawMessage

	// for Type == BlockToolResult
	// ToolUseID is reused to pair the result with the prior tool_use.
	Output  string
	IsError bool

	// for Type == BlockThinking
	Thinking  string
	Signature string

	// for Type == BlockRedactedThinking
	RedactedData string
}

// EventType labels the five internal ProviderEvent kinds. Adapters fold each
// vendor's wider event taxonomy into this set (see Anthropic 8→5 mapping in
// the adapter spec).
type EventType string

const (
	EventTextDelta      EventType = "text_delta"
	EventToolUse        EventType = "tool_use"
	EventUsage          EventType = "usage"
	EventStop           EventType = "stop"
	EventError          EventType = "error"
	EventReasoningStart EventType = "reasoning_start"
	EventReasoningDelta EventType = "reasoning_delta"
	EventReasoningEnd   EventType = "reasoning_end"
)

// Usage carries token-accounting from the provider for the just-finished call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ProviderEvent is one item on the Stream channel. Type selects which sibling
// field is meaningful; the rest are zero.
type ProviderEvent struct {
	Type       EventType
	TextDelta  string         // EventTextDelta
	ToolUse    *ContentBlock  // EventToolUse (fully-assembled block)
	Usage      *Usage         // EventUsage
	StopReason string         // EventStop ("end_turn" | "tool_use" | "stop_sequence" | ...)
	Error      *ProviderError // EventError

	ReasoningDelta string        // EventReasoningDelta
	ReasoningBlock *ContentBlock // EventReasoningStart/End: full thinking block (text+signature/redacted_data)
	BlockIndex     int           // reasoning event block index
	ReasoningType  string        // "thinking" | "redacted" (for reasoning_start)
}

// Error codes used by ProviderError. The set is closed; any new code requires
// a spec update so the agent loop's retry classifier stays in sync.
const (
	CodeRateLimited           = "rate_limited"
	CodeAuthFailed            = "auth_failed"
	CodeContextLengthExceeded = "context_length_exceeded"
	CodeInsufficientQuota     = "insufficient_quota"
	CodeInvalidRequest        = "invalid_request"
	CodeServerError           = "server_error"
	CodeNetworkError          = "network_error"
	CodeStreamBroken          = "stream_broken"
	CodeCanceled              = "canceled"
)

// ProviderError unifies vendor-side failures into one shape the agent loop
// can classify. The retry decision lives on this type (not in the loop) so
// adapters retain full information about *why* a call failed.
type ProviderError struct {
	Provider   string
	StatusCode int    // 0 = non-HTTP (network, ctx canceled, parse)
	Code       string // one of the constants above
	Message    string
	Cause      error
	retryAfter time.Duration
}

// NewProviderError is a small convenience for adapters; it normalises Code and
// ensures Message is never empty.
func NewProviderError(provider string, status int, code, msg string, cause error) *ProviderError {
	if msg == "" {
		msg = code
	}
	return &ProviderError{
		Provider:   provider,
		StatusCode: status,
		Code:       code,
		Message:    msg,
		Cause:      cause,
	}
}

func (e *ProviderError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "provider/%s: %s", e.Provider, e.Code)
	if e.StatusCode != 0 {
		fmt.Fprintf(&sb, " (HTTP %d)", e.StatusCode)
	}
	if e.Message != "" && e.Message != e.Code {
		fmt.Fprintf(&sb, ": %s", e.Message)
	}
	if e.Cause != nil {
		fmt.Fprintf(&sb, " <- %s", e.Cause.Error())
	}
	return sb.String()
}

func (e *ProviderError) Unwrap() error { return e.Cause }

// SetRetryAfter records a parsed Retry-After header so callers can prefer
// the server's hint over the configured backoff schedule.
func (e *ProviderError) SetRetryAfter(d time.Duration) { e.retryAfter = d }

// RetryAfter returns the server-provided hint, or zero if none was supplied.
func (e *ProviderError) RetryAfter() time.Duration { return e.retryAfter }

// IsRetryable mirrors the table in the provider-abstraction spec. Codes not
// listed here are treated as non-retryable by default to avoid hammering an
// origin that returned something we don't recognise.
func (e *ProviderError) IsRetryable() bool {
	switch e.Code {
	case CodeRateLimited, CodeServerError, CodeNetworkError, CodeStreamBroken:
		return true
	default:
		return false
	}
}

// AsProviderError unwraps any error chain looking for *ProviderError. Adapters
// and callers use this to classify without panicking on plain error values.
func AsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
