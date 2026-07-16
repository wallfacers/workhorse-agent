package memory

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/embedding"
)

// DefaultEmbedBuffer bounds the write-behind embedder's queue.
const DefaultEmbedBuffer = 256

// Embedder is the write-behind path that keeps memory_embeddings populated
// without ever blocking an entry write (design D3 + D8 usage-logger pattern). A
// single background goroutine drains a queue of entry names, fetches each
// entry's content, embeds it, and upserts the vector. Enqueue is non-blocking;
// a full queue drops the request (the startup/backfill sweep will catch it).
//
// A nil *Embedder is inert: Enqueue/Backfill/Close are all no-ops, which is the
// state when embedding is unconfigured.
type Embedder struct {
	entries *EntryStore
	vectors *VectorStore
	client  embedding.Client
	ch      chan string

	wg        sync.WaitGroup
	mu        sync.RWMutex
	closed    bool
	closeOnce sync.Once
}

// NewEmbedder starts the drain goroutine. When client is nil it returns nil (an
// inert embedder), so callers can unconditionally use the result.
func NewEmbedder(entries *EntryStore, vectors *VectorStore, client embedding.Client, buf int) *Embedder {
	if client == nil || entries == nil || vectors == nil {
		return nil
	}
	if buf <= 0 {
		buf = DefaultEmbedBuffer
	}
	e := &Embedder{
		entries: entries,
		vectors: vectors,
		client:  client,
		ch:      make(chan string, buf),
	}
	e.wg.Add(1)
	go e.drain()
	return e
}

func (e *Embedder) drain() {
	defer e.wg.Done()
	for name := range e.ch {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := e.embedOne(ctx, name); err != nil {
			slog.Warn("memory: write-behind embed failed", "name", name, "err", err)
		}
		cancel()
	}
}

// embedOne fetches, embeds, and stores the vector for one entry. A deleted entry
// (ErrNotFound) is a silent skip.
func (e *Embedder) embedOne(ctx context.Context, name string) error {
	entry, err := e.entries.GetByName(ctx, name)
	if err != nil {
		return nil //nolint:nilerr // entry gone before we embedded it: nothing to do
	}
	vecs, err := e.client.Embed(ctx, []string{embedText(entry)})
	if err != nil {
		return err
	}
	if len(vecs) != 1 {
		return nil
	}
	return e.vectors.Put(ctx, name, e.client.Model(), vecs[0], time.Now())
}

// embedText is the text handed to the embedding model for an entry. Trigger +
// content captures both the recall cue and the fact body.
func embedText(e *Entry) string {
	if e.Trigger == "" {
		return e.Content
	}
	return e.Trigger + "\n" + e.Content
}

// Enqueue schedules an entry for (re-)embedding. Non-blocking; a full queue
// drops the request. A nil embedder no-ops.
func (e *Embedder) Enqueue(name string) {
	if e == nil || name == "" {
		return
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return
	}
	select {
	case e.ch <- name:
	default:
		slog.Debug("memory: embed enqueue dropped", "name", name)
	}
}

// Backfill enqueues every entry whose vector is missing or stale for the current
// model. Intended to run once at startup and opportunistically after writes.
// Bounded by the queue: names beyond the queue capacity are dropped and picked
// up on the next Backfill. A nil embedder no-ops.
func (e *Embedder) Backfill(ctx context.Context) error {
	if e == nil {
		return nil
	}
	names, err := e.vectors.NamesMissingModel(ctx, e.client.Model())
	if err != nil {
		return err
	}
	for _, name := range names {
		e.Enqueue(name)
	}
	if len(names) > 0 {
		slog.Info("memory: embedding backfill enqueued", "count", len(names), "model", e.client.Model())
	}
	return nil
}

// Close stops the drain goroutine after the queue is drained. Safe to call
// multiple times and on a nil embedder.
func (e *Embedder) Close() {
	if e == nil {
		return
	}
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		close(e.ch)
		e.mu.Unlock()
	})
	e.wg.Wait()
}
