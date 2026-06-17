package memory_test

import (
	"context"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

func TestUsageLogger_BumpThenClose(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	must(t, es.Upsert(ctx, &memory.Entry{Name: "u1", Content: "x", CharCount: 1}))

	u := memory.NewUsageLogger(es, 16)
	u.Bump("u1")
	u.Bump("u1")
	u.Close() // drains the channel before returning

	e, err := es.GetByName(ctx, "u1")
	must(t, err)
	if e.HitCount != 2 {
		t.Fatalf("hit_count = %d, want 2", e.HitCount)
	}
	if e.LastUsedAt == nil {
		t.Fatal("last_used_at should be set after bump")
	}
}

func TestUsageLogger_FullChannelDropsWithoutPanic(t *testing.T) {
	es, _ := newEntryStore(t)
	// buf 1: send many bumps without anything draining mid-flight is impossible
	// (the goroutine drains), so instead assert that flooding never panics and a
	// later Close drains cleanly. The drop path is exercised under burst.
	u := memory.NewUsageLogger(es, 1)
	for i := 0; i < 10000; i++ {
		u.Bump("missing") // name does not exist; BumpUsage is a no-op, never errors
	}
	u.Close() // must not panic or hang
}

func TestUsageLogger_BumpAfterCloseIsNoPanic(t *testing.T) {
	es, _ := newEntryStore(t)
	u := memory.NewUsageLogger(es, 4)
	u.Close()
	// A Bump arriving after shutdown (out-of-order wiring, in-flight tool) must be
	// a silent no-op, never a send-on-closed-channel panic.
	u.Bump("late")
	u.Bump("late2")
}

func TestUsageLogger_ConcurrentBumpDuringClose(t *testing.T) {
	es, _ := newEntryStore(t)
	must(t, es.Upsert(context.Background(), &memory.Entry{Name: "u", Content: "x", CharCount: 1}))
	u := memory.NewUsageLogger(es, 8)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			u.Bump("u")
		}
		close(done)
	}()
	u.Close() // racing in-flight Bumps must not panic
	<-done
}

func TestUsageLogger_NilSafe(t *testing.T) {
	var u *memory.UsageLogger
	u.Bump("x") // must not panic
	u.Close()   // must not panic
}

func TestUsageLogger_DefaultBuffer(t *testing.T) {
	es, _ := newEntryStore(t)
	u := memory.NewUsageLogger(es, 0) // non-positive → DefaultUsageBuffer
	u.Bump("anything")
	u.Close()
}
