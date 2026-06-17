package memory

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultUsageBuffer is the default size of the usage-logger's buffered channel
// when a non-positive buffer is passed to NewUsageLogger.
const DefaultUsageBuffer = 256

// UsageLogger is the idempotent best-effort usage-count side-effect path for the
// read-only LoadMemory/MemorySearch tools (design D8). A single background
// goroutine drains a buffered channel and performs the actual
// hit_count++ / last_used_at=now UPDATE via the shared *sql.DB, so the tools
// never write on their own goroutine and never surface a usage-bump error to the
// caller. Sends are non-blocking: a full channel drops the bump (scoring is
// approximate by design) and logs at DEBUG.
type UsageLogger struct {
	store *EntryStore
	ch    chan string
	wg    sync.WaitGroup

	closeOnce sync.Once
}

// NewUsageLogger starts the background drain goroutine. buf bounds the channel;
// a non-positive buf falls back to DefaultUsageBuffer. Call Close to stop the
// goroutine and drain the channel.
func NewUsageLogger(store *EntryStore, buf int) *UsageLogger {
	if buf <= 0 {
		buf = DefaultUsageBuffer
	}
	u := &UsageLogger{
		store: store,
		ch:    make(chan string, buf),
	}
	u.wg.Add(1)
	go u.drain()
	return u
}

func (u *UsageLogger) drain() {
	defer u.wg.Done()
	for name := range u.ch {
		// Best-effort: a failed UPDATE is logged at DEBUG and never surfaced.
		// Use a fresh short-lived context so a long-running drain cannot be
		// starved by a cancelled tool ctx (the bump is fully detached).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := u.store.BumpUsage(ctx, name, time.Now()); err != nil {
			slog.Debug("memory: usage bump failed", "name", name, "err", err)
		}
		cancel()
	}
}

// Bump enqueues a usage hit for name. It is non-blocking: if the channel is full
// the bump is dropped and logged at DEBUG. Bump MUST NOT block the tool result.
func (u *UsageLogger) Bump(name string) {
	if u == nil || name == "" {
		return
	}
	select {
	case u.ch <- name:
	default:
		slog.Debug("memory: usage bump dropped", "name", name)
	}
}

// Close stops the drain goroutine after the channel is fully drained. It is safe
// to call multiple times and from a defer.
func (u *UsageLogger) Close() {
	if u == nil {
		return
	}
	u.closeOnce.Do(func() {
		close(u.ch)
	})
	u.wg.Wait()
}
