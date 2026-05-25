// Package sqlite is the SQLite-backed implementation of internal/store.Store,
// built on modernc.org/sqlite (pure-Go, no CGO). The on-disk file lives under
// config.store.path (default ~/.workhorse-agent/state.db); :memory: is used by
// tests.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite" // driver registers itself as "sqlite"

	"github.com/wallfacers/workhorse-agent/internal/store"
)

// Options controls how Open connects to the database.
type Options struct {
	// DSN is forwarded to sql.Open. Use ":memory:" for tests; for files,
	// use the absolute path the caller already expanded.
	DSN string
	// BusyTimeoutMs maps to PRAGMA busy_timeout. 0 means "let SQLite block
	// forever on contention", which is rarely what you want — defaults to
	// 5000 (matching config.store.busy_timeout_ms default).
	BusyTimeoutMs int
}

// Store is the concrete SQLite implementation. Construct with Open and close
// with Close. All store.Store methods are safe for concurrent use.
type Store struct {
	db        *sql.DB
	closeOnce sync.Once
}

// compile-time check that *Store satisfies the interface.
var _ store.Store = (*Store)(nil)

// Open connects to a SQLite database at opts.DSN, applies pragmas, and runs
// migrations. The returned *Store is ready for use.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.DSN == "" {
		return nil, errors.New("sqlite: DSN must not be empty")
	}
	db, err := sql.Open("sqlite", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", opts.DSN, err)
	}

	busy := opts.BusyTimeoutMs
	if busy <= 0 {
		busy = 5000
	}

	// modernc.org/sqlite is a single-writer database; SetMaxOpenConns(1)
	// serialises ALL access (reads and writes) through a single Go connection
	// so we never trip "database is locked" with concurrent goroutines.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		fmt.Sprintf("PRAGMA busy_timeout = %d", busy),
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite: %s: %w", pragma, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying handle. Safe to call multiple times.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.db.Close()
	})
	return err
}
