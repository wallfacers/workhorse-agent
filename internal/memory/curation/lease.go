package curation

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/idgen"
)

// leaseRowID is the fixed primary key of the singleton lease row (D6).
const leaseRowID = 1

// Lease implements the single-curator leader lease (design D6) over the
// memory_curation_lease table. At most one process holds the lease at a time;
// a crashed holder is taken over purely by expiry (no deadlock — waiters hold
// nothing). An in-process mutex backstop additionally guarantees two goroutines
// in the same process never curate concurrently.
//
// The TTL is supplied per call (Acquire/Renew) rather than stored, so a
// hot-reloaded lease_ttl_seconds (design D6 hot-reload subset) takes effect on
// the next operation without any locking inside the lease. Time is stored as
// unix microseconds, consistent with the rest of the store.
type Lease struct {
	db     *sql.DB
	holder string

	// procMu is held for the entire lifetime of a held lease (acquire→release),
	// so a second Acquire in this process returns false immediately rather than
	// racing the DB CAS.
	procMu sync.Mutex
}

// NewLease builds a lease with a process-unique holder token "hostname:pid:ULID"
// (D6: hostname/pid for debuggability, ULID to distinguish restarts of the same
// pid).
func NewLease(db *sql.DB) *Lease {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	holder := fmt.Sprintf("%s:%d:%s", host, os.Getpid(), idgen.NewULID())
	return &Lease{db: db, holder: holder}
}

// Holder returns this lease's identity token (diagnostics/tests).
func (l *Lease) Holder() string { return l.holder }

// Acquire attempts to become the curator at time now for a lease lasting ttl.
// It first grabs the in-process backstop (a concurrent Acquire in the same
// process gets false immediately), then runs the DB CAS: take the row iff it is
// expired OR already ours. The lease is held only when rowsAffected == 1. On CAS
// miss or error the backstop is released so a later Acquire can retry.
func (l *Lease) Acquire(ctx context.Context, now time.Time, ttl time.Duration) (bool, error) {
	if !l.procMu.TryLock() {
		return false, nil // another goroutine in this process holds the lease
	}
	ok, err := l.acquireCAS(ctx, now, ttl)
	if err != nil || !ok {
		l.procMu.Unlock()
		return false, err
	}
	return true, nil
}

// Renew extends a held lease by ttl, guarded by holder = self. A false return
// means the lease was lost (it expired and another process stole it); the caller
// MUST abort its in-flight pass. Renew does not touch the in-process backstop —
// Release still owns unlocking it.
func (l *Lease) Renew(ctx context.Context, now time.Time, ttl time.Duration) (bool, error) {
	res, err := l.db.ExecContext(ctx,
		`UPDATE memory_curation_lease
		   SET holder = ?, expires_at = ?, heartbeat_at = ?
		 WHERE id = ? AND holder = ?`,
		l.holder, leaseMicros(now.Add(ttl)), leaseMicros(now), leaseRowID, l.holder)
	if err != nil {
		return false, fmt.Errorf("curation: renew lease: %w", err)
	}
	return rowsAffectedOne(res)
}

// Release relinquishes the lease (sets it expired) so another process can take
// over immediately rather than waiting a full TTL, and unlocks the in-process
// backstop. It is safe to call even if the lease was already lost: the holder=self
// guard makes the UPDATE a no-op in that case, and the backstop is always freed.
func (l *Lease) Release(ctx context.Context) error {
	defer l.procMu.Unlock()
	_, err := l.db.ExecContext(ctx,
		`UPDATE memory_curation_lease
		   SET holder = '', expires_at = 0, heartbeat_at = 0
		 WHERE id = ? AND holder = ?`,
		leaseRowID, l.holder)
	if err != nil {
		return fmt.Errorf("curation: release lease: %w", err)
	}
	return nil
}

// acquireCAS runs the take-or-renew UPDATE: win iff the row is expired or ours.
func (l *Lease) acquireCAS(ctx context.Context, now time.Time, ttl time.Duration) (bool, error) {
	res, err := l.db.ExecContext(ctx,
		`UPDATE memory_curation_lease
		   SET holder = ?, expires_at = ?, heartbeat_at = ?
		 WHERE id = ? AND (expires_at < ? OR holder = ?)`,
		l.holder, leaseMicros(now.Add(ttl)), leaseMicros(now),
		leaseRowID, leaseMicros(now), l.holder)
	if err != nil {
		return false, fmt.Errorf("curation: acquire lease: %w", err)
	}
	return rowsAffectedOne(res)
}

func rowsAffectedOne(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("curation: lease rows affected: %w", err)
	}
	return n == 1, nil
}

func leaseMicros(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}
