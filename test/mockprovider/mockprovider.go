// Package mockprovider implements provider.Provider against a scripted set of
// ProviderEvents. Tests for agent loop, session manager, and HTTP layer rely
// on this so they don't burn real LLM tokens.
//
// Usage:
//
//	mp := mockprovider.New("openai")
//	mp.QueueResponse([]provider.ProviderEvent{
//	    {Type: provider.EventTextDelta, TextDelta: "hello"},
//	    {Type: provider.EventStop,      StopReason: "end_turn"},
//	})
//	cfg.Provider = mp
//	// ... drive the loop, then mp.Requests() to inspect what it received.
package mockprovider

import (
	"context"
	"sync"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// Provider is a programmable provider.Provider. It records every Request it
// receives and emits the next queued response, in order.
type Provider struct {
	name string

	mu       sync.Mutex
	queue    [][]provider.ProviderEvent
	upfront  error // returned by Stream as the upfront error (request-never-sent)
	requests []provider.Request
	noScript func() []provider.ProviderEvent // fallback when queue is empty
}

// New constructs a Provider with the given name (anthropic/openai/...). Tests
// pick a name to match their session config.
func New(name string) *Provider {
	return &Provider{name: name}
}

func (m *Provider) Name() string { return m.name }

// QueueResponse adds one response to the FIFO. Each call to Stream pops the
// next response.
func (m *Provider) QueueResponse(events []provider.ProviderEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue = append(m.queue, events)
}

// QueueError sets an upfront error that the *next* Stream call will return as
// the (nil-channel) error. After the next call the error is cleared.
func (m *Provider) QueueError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upfront = err
}

// SetFallback registers a function that produces a default response when the
// queue is empty. Useful for "respond with stop forever" patterns.
func (m *Provider) SetFallback(fn func() []provider.ProviderEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noScript = fn
}

// Requests returns a copy of every Request received so far, in order.
func (m *Provider) Requests() []provider.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]provider.Request, len(m.requests))
	copy(out, m.requests)
	return out
}

// Stream pops the next queued response and replays it on a channel. If
// QueueError was set, the upfront error is returned and the queue is left
// alone.
func (m *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.ProviderEvent, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	if m.upfront != nil {
		err := m.upfront
		m.upfront = nil
		m.mu.Unlock()
		return nil, err
	}
	var events []provider.ProviderEvent
	if len(m.queue) > 0 {
		events = m.queue[0]
		m.queue = m.queue[1:]
	} else if m.noScript != nil {
		events = m.noScript()
	} else {
		events = []provider.ProviderEvent{{Type: provider.EventStop, StopReason: "end_turn"}}
	}
	m.mu.Unlock()

	ch := make(chan provider.ProviderEvent, len(events)+1)
	go func() {
		defer close(ch)
		for _, e := range events {
			select {
			case ch <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
