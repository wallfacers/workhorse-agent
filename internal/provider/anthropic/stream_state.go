package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// anthropicStreamState accumulates SSE state across events and produces the
// internal ProviderEvent slice for each incoming event. The Anthropic event →
// internal event mapping is:
//
//	message_start            → swallow (capture input_tokens)
//	content_block_start      → swallow + init buffer slot per index
//	                         → reasoning_start (thinking/redacted_thinking)
//	content_block_delta text → text_delta (forwarded as-is)
//	content_block_delta json → swallow (accumulate into tool_use input)
//	content_block_delta think→ reasoning_delta (accumulate + forward text)
//	content_block_delta sig  → swallow (accumulate signature, no event)
//	content_block_stop       → tool_use (for tool_use blocks)
//	                         → reasoning_end (for thinking blocks with signature)
//	message_delta            → usage (and cache stop_reason)
//	message_stop             → stop (using cached stop_reason), terminal
//	ping                     → swallow (heartbeat)
//	error                    → error, terminal
type anthropicStreamState struct {
	blocks       map[int]*blockBuf
	stopReason   string
	inputTokens  int
	outputTokens int
}

type blockBuf struct {
	typ          string // "text" | "tool_use" | "thinking" | "redacted_thinking"
	id           string
	name         string
	inputJSON    []byte // for tool_use: accumulated partial_json fragments
	thinkingText []byte // for thinking: accumulated thinking text
	signature    []byte // for thinking: accumulated signature
	redactedData string // for redacted_thinking: opaque data from content_block_start
	hasSignature bool   // true once at least one signature_delta arrived
}

func (s *anthropicStreamState) ensure() {
	if s.blocks == nil {
		s.blocks = make(map[int]*blockBuf)
	}
}

// handle returns the events to forward, whether this event terminates the
// stream (caller should close), and any decode error. The returned slice may
// be empty (swallowed events).
func (s *anthropicStreamState) handle(ev provider.SSEEvent) ([]provider.ProviderEvent, bool, error) {
	s.ensure()
	switch ev.Type {
	case "ping":
		return nil, false, nil
	case "message_start":
		var p sseMessageStart
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return nil, false, fmt.Errorf("message_start: %w", err)
		}
		s.inputTokens = p.Message.Usage.InputTokens
		return nil, false, nil
	case "content_block_start":
		var p sseContentBlockStart
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return nil, false, fmt.Errorf("content_block_start: %w", err)
		}
		buf := &blockBuf{
			typ:  p.ContentBlock.Type,
			id:   p.ContentBlock.ID,
			name: p.ContentBlock.Name,
		}
		s.blocks[p.Index] = buf

		switch p.ContentBlock.Type {
		case "text":
			if p.ContentBlock.Text != "" {
				return []provider.ProviderEvent{
					{Type: provider.EventTextDelta, TextDelta: p.ContentBlock.Text},
				}, false, nil
			}
			return nil, false, nil
		case "thinking":
			rType := "thinking"
			return []provider.ProviderEvent{{
				Type:          provider.EventReasoningStart,
				BlockIndex:    p.Index,
				ReasoningType: rType,
			}}, false, nil
		case "redacted_thinking":
			buf.redactedData = p.ContentBlock.Data
			return []provider.ProviderEvent{{
				Type:          provider.EventReasoningStart,
				BlockIndex:    p.Index,
				ReasoningType: "redacted",
			}}, false, nil
		default:
			return nil, false, nil
		}
	case "content_block_delta":
		var p sseContentBlockDelta
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return nil, false, fmt.Errorf("content_block_delta: %w", err)
		}
		buf := s.blocks[p.Index]
		switch p.Delta.Type {
		case "text_delta":
			return []provider.ProviderEvent{
				{Type: provider.EventTextDelta, TextDelta: p.Delta.Text},
			}, false, nil
		case "input_json_delta":
			if buf == nil {
				return nil, false, nil
			}
			buf.inputJSON = append(buf.inputJSON, p.Delta.PartialJSON...)
			return nil, false, nil
		case "thinking_delta":
			if buf != nil {
				buf.thinkingText = append(buf.thinkingText, p.Delta.Thinking...)
			}
			return []provider.ProviderEvent{{
				Type:           provider.EventReasoningDelta,
				ReasoningDelta: p.Delta.Thinking,
				BlockIndex:     p.Index,
			}}, false, nil
		case "signature_delta":
			if buf != nil {
				buf.signature = append(buf.signature, p.Delta.Signature...)
				buf.hasSignature = true
			}
			return nil, false, nil
		default:
			return nil, false, nil
		}
	case "content_block_stop":
		var p sseContentBlockStop
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return nil, false, fmt.Errorf("content_block_stop: %w", err)
		}
		buf := s.blocks[p.Index]
		delete(s.blocks, p.Index)

		if buf == nil {
			return nil, false, nil
		}
		switch buf.typ {
		case "tool_use":
			input := buf.inputJSON
			if len(input) == 0 {
				input = []byte("{}")
			}
			var probe any
			if err := json.Unmarshal(input, &probe); err != nil {
				return nil, false, fmt.Errorf("tool_use input json: %w", err)
			}
			block := &provider.ContentBlock{
				Type:      provider.BlockToolUse,
				ToolUseID: buf.id,
				ToolName:  buf.name,
				Input:     append([]byte(nil), input...),
			}
			return []provider.ProviderEvent{{Type: provider.EventToolUse, ToolUse: block}}, false, nil

		case "thinking":
			// Incomplete thinking block (no signature received before
			// content_block_stop) is treated as a stream error.
			if !buf.hasSignature {
				return []provider.ProviderEvent{{
					Type: provider.EventError,
					Error: provider.NewProviderError("anthropic", 0,
						provider.CodeStreamBroken,
						"thinking block ended without signature", nil),
				}}, true, nil
			}
			block := &provider.ContentBlock{
				Type:      provider.BlockThinking,
				Thinking:  string(buf.thinkingText),
				Signature: string(buf.signature),
			}
			return []provider.ProviderEvent{{
				Type:           provider.EventReasoningEnd,
				BlockIndex:     p.Index,
				ReasoningBlock: block,
			}}, false, nil

		case "redacted_thinking":
			// Empty opaque data would serialize (via omitempty) to a bare
			// {"type":"redacted_thinking"} that Anthropic rejects on the next
			// round-trip. Treat it as a broken stream, mirroring the
			// missing-signature guard for plain thinking blocks above.
			if buf.redactedData == "" {
				return []provider.ProviderEvent{{
					Type: provider.EventError,
					Error: provider.NewProviderError("anthropic", 0,
						provider.CodeStreamBroken,
						"redacted_thinking block ended without data", nil),
				}}, true, nil
			}
			block := &provider.ContentBlock{
				Type:         provider.BlockRedactedThinking,
				RedactedData: buf.redactedData,
			}
			return []provider.ProviderEvent{{
				Type:           provider.EventReasoningEnd,
				BlockIndex:     p.Index,
				ReasoningBlock: block,
			}}, false, nil

		default:
			return nil, false, nil
		}
	case "message_delta":
		var p sseMessageDelta
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return nil, false, fmt.Errorf("message_delta: %w", err)
		}
		if p.Delta.StopReason != "" {
			s.stopReason = p.Delta.StopReason
		}
		s.outputTokens = p.Usage.OutputTokens
		return []provider.ProviderEvent{{
			Type: provider.EventUsage,
			Usage: &provider.Usage{
				InputTokens:  s.inputTokens,
				OutputTokens: s.outputTokens,
			},
		}}, false, nil
	case "message_stop":
		return []provider.ProviderEvent{{
			Type:       provider.EventStop,
			StopReason: s.stopReason,
		}}, true, nil
	case "error":
		var p sseError
		_ = json.Unmarshal(ev.Data, &p)
		code, msg := classifyAnthropicError(0, p.Error.Type, p.Error.Message)
		return []provider.ProviderEvent{{
			Type:  provider.EventError,
			Error: provider.NewProviderError("anthropic", 0, code, msg, nil),
		}}, true, nil
	default:
		return nil, false, nil
	}
}
