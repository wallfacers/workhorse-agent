package permission_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/permission"
)

// SetDefaultDecision changes the fallback decision applied when no rule matches.
func TestSetDefaultDecision_TakesEffect(t *testing.T) {
	s := newStore(t)
	promptCalls := 0
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			promptCalls++
			return permission.Deny, true
		},
		nil, 0, "")

	// Empty default → unmatched call prompts.
	if d, err := m.Check(context.Background(), "sess", "Bash", "echo hi"); err != nil || d != permission.Deny {
		t.Fatalf("before: got (%v,%v), want (deny,nil)", d, err)
	}
	if promptCalls != 1 {
		t.Fatalf("expected prompt once before setting default, got %d", promptCalls)
	}

	m.SetDefaultDecision(permission.AllowPermanent)

	// Now the default applies without prompting.
	if d, err := m.Check(context.Background(), "sess", "Bash", "echo hi"); err != nil || d != permission.AllowPermanent {
		t.Fatalf("after: got (%v,%v), want (allow_permanent,nil)", d, err)
	}
	if promptCalls != 1 {
		t.Fatalf("expected no further prompt after setting default, got %d", promptCalls)
	}
}

// SetTimeout changes the per-prompt deadline observed by a blocking prompt.
func TestSetTimeout_AppliesToPrompt(t *testing.T) {
	s := newStore(t)
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			<-ctx.Done()
			return permission.Deny, false
		},
		nil, time.Hour, "")

	m.SetTimeout(20 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_, err := m.Check(context.Background(), "sess", "Bash", "sleep")
		if err != permission.ErrTimeout {
			t.Errorf("got err %v, want ErrTimeout", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Check did not honor the shortened timeout")
	}
}

// Concurrent setters + Check must be race-free (run with -race).
func TestSetters_NoDataRace(t *testing.T) {
	s := newStore(t)
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			return permission.Deny, true
		},
		nil, 0, "")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if i%2 == 0 {
					m.SetDefaultDecision(permission.AllowPermanent)
					m.SetTimeout(time.Duration(j) * time.Millisecond)
				} else {
					_, _ = m.Check(context.Background(), "sess", "Bash", "echo")
				}
			}
		}(i)
	}
	wg.Wait()
}
