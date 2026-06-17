package curation

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

const testTTL = 60 * time.Second

func leaseDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.DB()
}

func TestLeaseSingleWinner(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Unix(1_000_000, 0).UTC()

	a := NewLease(db)
	b := NewLease(db)
	if a.Holder() == b.Holder() {
		t.Fatal("two leases share a holder token")
	}

	okA, err := a.Acquire(ctx, now, testTTL)
	if err != nil || !okA {
		t.Fatalf("A acquire = %v, %v; want true, nil", okA, err)
	}
	okB, err := b.Acquire(ctx, now, testTTL)
	if err != nil {
		t.Fatalf("B acquire err: %v", err)
	}
	if okB {
		t.Fatal("B acquired an unexpired lease held by A")
	}
}

func TestLeaseRenewKeepsItOurs(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Unix(1_000_000, 0).UTC()

	a := NewLease(db)
	b := NewLease(db)
	mustAcquire(t, a, now)

	// A renews near the original expiry; B still cannot take it.
	later := now.Add(50 * time.Second)
	ok, err := a.Renew(ctx, later, testTTL)
	if err != nil || !ok {
		t.Fatalf("A renew = %v, %v; want true", ok, err)
	}
	okB, _ := b.Acquire(ctx, later.Add(5*time.Second), testTTL) // still < new expiry (later+60s)
	if okB {
		t.Fatal("B took the lease A had just renewed")
	}
}

func TestLeaseTTLTakeover(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Unix(1_000_000, 0).UTC()

	a := NewLease(db)
	b := NewLease(db)
	mustAcquire(t, a, now)

	// A crashes (no renew). After the TTL, B's acquire succeeds purely by expiry.
	afterExpiry := now.Add(61 * time.Second)
	okB, err := b.Acquire(ctx, afterExpiry, testTTL)
	if err != nil || !okB {
		t.Fatalf("B takeover = %v, %v; want true after expiry", okB, err)
	}
	// A's stale renew now fails — its lease was stolen.
	okA, err := a.Renew(ctx, afterExpiry.Add(time.Second), testTTL)
	if err != nil {
		t.Fatalf("A renew err: %v", err)
	}
	if okA {
		t.Fatal("A renewed a lease that B had taken over")
	}
}

func TestLeaseReleaseEnablesImmediateTakeover(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Unix(1_000_000, 0).UTC()

	a := NewLease(db)
	b := NewLease(db)
	mustAcquire(t, a, now)

	if err := a.Release(ctx); err != nil {
		t.Fatalf("A release: %v", err)
	}
	// Even though the TTL has not elapsed, B can take over right away.
	okB, err := b.Acquire(ctx, now.Add(time.Second), testTTL)
	if err != nil || !okB {
		t.Fatalf("B acquire after release = %v, %v; want true", okB, err)
	}
}

func TestLeaseInProcessBackstop(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Unix(1_000_000, 0).UTC()

	a := NewLease(db)
	mustAcquire(t, a, now)
	// A second Acquire on the SAME lease object must not double-hold.
	ok, err := a.Acquire(ctx, now, testTTL)
	if err != nil {
		t.Fatalf("re-acquire err: %v", err)
	}
	if ok {
		t.Fatal("same lease acquired twice without release (backstop failed)")
	}
	// After release, the same object can acquire again.
	if err := a.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, err := a.Acquire(ctx, now.Add(time.Second), testTTL); err != nil || !ok {
		t.Fatalf("re-acquire after release = %v, %v; want true", ok, err)
	}
}

func mustAcquire(t *testing.T, l *Lease, now time.Time) {
	t.Helper()
	ok, err := l.Acquire(context.Background(), now, testTTL)
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v; want true", ok, err)
	}
}
