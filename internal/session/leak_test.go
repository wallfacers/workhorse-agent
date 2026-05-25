package session

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestManager_NoGoroutineLeak_After100Cycles is task 15.5 from the MVP plan:
// after creating and deleting 100 sessions back-to-back the goroutine count
// must return to baseline (with a small tolerance for runtime housekeeping
// goroutines that may spin up during the run).
func TestManager_NoGoroutineLeak_After100Cycles(t *testing.T) {
	// Quiet the runtime before sampling — finalisers, GC sweeps, and the
	// race detector's background workers all add noise to NumGoroutine.
	runtime.GC()
	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	m := NewManager(ManagerOptions{
		MaxConcurrent: 0, // unbounded
		RunnerFactory: func(*Session) Runner {
			return &fakeRunner{drain: 5 * time.Millisecond}
		},
	})

	for i := 0; i < 100; i++ {
		s, err := m.CreateSession(context.Background(), Options{Ephemeral: true})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if err := m.DeleteSession(context.Background(), s.ID, 200*time.Millisecond); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}

	// Let any straggler goroutines finish their cancel/drain. The runner's
	// 5 ms drain bound means everything should be done within a few hundred
	// ms even on the race detector.
	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		got = runtime.NumGoroutine()
		if got <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: baseline=%d, after 100 create/delete cycles=%d", baseline, got)
}
