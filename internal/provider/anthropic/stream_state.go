package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// anthropicStreamState accumulates SSE state across events and produces the
// internal ProviderEvent slice for each incoming event. The 8→5 mapping is:
//
//	message_start            → swallow (capture input_tokens)
//	content_block_start      → swallow + init buffer slot per index
//	content_block_delta text → text_delta (forwarded as-is)
//	content_block_delta json → swallow (accumulate into tool_use input)
//	content_block_delta think→ swallow (MVP discards thinking)
//	content_block_stop       → tool_use (only for tool_use blocks; text closes silently)
//	message_delta            → usage (and cache stop_reason)
//	message_stop             → stop (using cached stop_reason), terminal
//	ping                     → swallow (heartbeat)
//	error                    → error, terminal
//
// We keep the buffer slice indexed by block index because Anthropic emits a
// single content_block_delta event per block, and blocks can interleave when a
// model produces text and a tool call in one assistant message.
type anthropicStreamState struct {
	blocks       map[int]*blockBuf
	stopReason   string
	inputTokens  int
	outputTokens int
}

type blockBuf struct {
	typ       string // "text" | "tool_use" | "thinking"
	id        string
	name      string
	inputJSON []byte // for tool_use: accumulated partial_json fragments
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
		s.blocks[p.Index] = &blockBuf{
			typ:  p.ContentBlock.Type,
			id:   p.ContentBlock.ID,
			name: p.ContentBlock.Name,
		}
		if p.ContentBlock.Type == "text" && p.ContentBlock.Text != "" {
			return []provider.ProviderEvent{
				{Type: provider.EventTextDelta, TextDelta: p.ContentBlock.Text},
			}, false, nil
		}
		return nil, false, nil
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
				// Shouldn't happen — Anthropic always emits content_block_start
				// first — but degrade gracefully.
				return nil, false, nil
			}
			buf.inputJSON = append(buf.inputJSON, p.Delta.PartialJSON...)
			return nil, false, nil
		case "thinking_delta":
			// MVP intentionally drops thinking; spec scenario "thinking 块被丢弃".
			return nil, false, nil
		default:
			// Unknown delta types are swallowed so a future Anthropic addition
			// doesn't break us; if it carries text we'll just miss it until
			// the spec catches up.
			return nil, false, nil
		}
	case "content_block_stop":
		var p sseContentBlockStop
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return nil, false, fmt.Errorf("content_block_stop: %w", err)
		}
		buf := s.blocks[p.Index]
		delete(s.blocks, p.Index)
		if buf == nil || buf.typ != "tool_use" {
			return nil, false, nil
		}
		// Parse accumulated JSON. Empty buffer means the model produced a
		// tool_use with no arguments; emit empty object.
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
		// Unknown event type → swallow. Future Anthropic additions won't
		// surface to the agent loop until we map them explicitly.
		return nil, false, nil
	}
}
