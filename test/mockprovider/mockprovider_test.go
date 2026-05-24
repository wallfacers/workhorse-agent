package mockprovider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wallfacers/data-agent/internal/provider"
	"github.com/wallfacers/data-agent/test/mockprovider"
)

func TestQueueAndReplay(t *testing.T) {
	mp := mockprovider.New("openai")
	mp.QueueResponse([]provider.ProviderEvent{
		{Type: provider.EventTextDelta, TextDelta: "hi"},
		{Type: provider.EventStop, StopReason: "end_turn"},
	})

	ch, err := mp.Stream(context.Background(), provider.Request{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got []provider.ProviderEvent
	for e := range ch {
		got = append(got, e)
	}
	if len(got) != 2 || got[0].TextDelta != "hi" {
		t.Errorf("replay broken: %+v", got)
	}
	if reqs := mp.Requests(); len(reqs) != 1 || reqs[0].Model != "gpt-4o" {
		t.Errorf("requests not recorded: %+v", reqs)
	}
}

func TestQueueError_OneShot(t *testing.T) {
	mp := mockprovider.New("anthropic")
	boom := errors.New("boom")
	mp.QueueError(boom)
	mp.QueueResponse([]provider.ProviderEvent{{Type: provider.EventStop}})

	_, err := mp.Stream(context.Background(), provider.Request{})
	if !errors.Is(err, boom) {
		t.Errorf("first call should return queued error, got %v", err)
	}
	// Second call should succeed.
	ch, err := mp.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Errorf("second call should succeed, got %v", err)
	}
	for range ch {
	}
}

func TestFallbackOnEmptyQueue(t *testing.T) {
	mp := mockprovider.New("anthropic")
	mp.SetFallback(func() []provider.ProviderEvent {
		return []provider.ProviderEvent{{Type: provider.EventStop, StopReason: "end_turn"}}
	})
	ch, err := mp.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for range ch {
		count++
	}
	if count != 1 {
		t.Errorf("expected 1 fallback event, got %d", count)
	}
}
