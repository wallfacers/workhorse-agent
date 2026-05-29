package frontend

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/wallfacers/workhorse-agent/internal/idgen"
	"github.com/wallfacers/workhorse-agent/internal/tools"
)

type emitFunc func(ctx context.Context, evType string, payload map[string]any) error

// Bridge correlates frontend_tool_use calls with their frontend_tool_result
// responses. Each Call mints a ULID, emits the event, and blocks until
// Resolve delivers a result or the context is cancelled.
type Bridge struct {
	emit    emitFunc
	pending map[string]chan *tools.Result
	mu      sync.Mutex
}

// NewBridge creates a Bridge that emits events via the provided closure.
func NewBridge(emit emitFunc) *Bridge {
	return &Bridge{
		emit:    emit,
		pending: map[string]chan *tools.Result{},
	}
}

// Call emits a frontend_tool_use event and blocks until the matching result
// arrives via Resolve or the context is cancelled.
func (b *Bridge) Call(ctx context.Context, name string, input json.RawMessage) (*tools.Result, error) {
	id := idgen.NewULID()
	ch := make(chan *tools.Result, 1)

	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()

	if err := b.emit(ctx, "frontend_tool_use", map[string]any{
		"tool_use_id": id,
		"name":        name,
		"input":       json.RawMessage(input),
	}); err != nil {
		return nil, err
	}

	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Resolve delivers a frontend_tool_result to the waiting Call. Unknown ids are
// dropped silently (e.g. the call already timed out).
func (b *Bridge) Resolve(id string, raw json.RawMessage) {
	res := parseResult(raw)

	b.mu.Lock()
	ch, ok := b.pending[id]
	b.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- res:
	default:
	}
}

func parseResult(raw json.RawMessage) *tools.Result {
	var envelope struct {
		OK    bool `json:"ok"`
		Value any  `json:"value"`
		Error *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}
	}
	if envelope.OK {
		val, _ := json.Marshal(envelope.Value)
		return &tools.Result{Output: string(val), IsError: false}
	}
	msg := "unknown frontend error"
	if envelope.Error != nil && envelope.Error.Message != "" {
		msg = envelope.Error.Message
	}
	return &tools.Result{Output: msg, IsError: true}
}
