package permission_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/permission"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---- glob ----

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, target string
		want            bool
	}{
		{"a", "a", true},
		{"a", "b", false},
		{"*.go", "main.go", true},
		{"*.go", "main.py", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/lib/main.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "src/main.go", true},
		{"**/*.go", "src/lib/main.go", true},
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/lib/inner/x.go", true},
		{"src/**/*.go", "tests/x.go", false},
		{"a/b/c", "a/b/c", true},
		{"a/?/c", "a/b/c", true},
		{"a/?/c", "a/bb/c", false},
		{"**", "anything/at/all", true},
		{"**", "", true},
	}
	for _, c := range cases {
		got := permission.MatchGlob(c.pattern, c.target)
		if got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pattern, c.target, got, c.want)
		}
	}
}

// ---- Decision behaviour ----

func TestCheck_PermanentAllowFromStore(t *testing.T) {
	s := newStore(t)
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "p1", Tool: "Read", Pattern: "**/*.md",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}))
	called := false
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			called = true
			return permission.Deny, true
		}, nil, time.Second, "")
	d, err := m.Check(context.Background(), "sess", "Read", "docs/intro.md")
	if err != nil || d != permission.AllowPermanent {
		t.Errorf("got %v %v", d, err)
	}
	if called {
		t.Error("prompt should not be invoked when a permanent allow exists")
	}
}

func TestCheck_PromptIssuedWhenNoRule(t *testing.T) {
	s := newStore(t)
	called := 0
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			called++
			return permission.AllowSession, true
		}, nil, time.Second, "")
	d, err := m.Check(context.Background(), "sess", "Bash", "ls")
	if err != nil || d != permission.AllowSession {
		t.Errorf("got %v %v", d, err)
	}
	// AllowSession should now skip the prompt on the same resource.
	d, _ = m.Check(context.Background(), "sess", "Bash", "ls")
	if d != permission.AllowSession {
		t.Errorf("session-cached rule lost: %v", d)
	}
	if called != 1 {
		t.Errorf("prompt called %d times, want 1", called)
	}
}

func TestCheck_AllowOnceConsumed(t *testing.T) {
	s := newStore(t)
	called := 0
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			called++
			return permission.AllowOnce, true
		}, nil, time.Second, "")
	for i := 0; i < 3; i++ {
		_, _ = m.Check(context.Background(), "sess", "Bash", "ls")
	}
	if called != 3 {
		t.Errorf("AllowOnce should re-prompt every call, called=%d", called)
	}
}

// Spec scenario: 命中即强制询问，绕过 allow 规则.
func TestCheck_DangerousOverridesAllow(t *testing.T) {
	s := newStore(t)
	// pre-seed an allow_permanent for rm -rf
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "p1", Tool: "Bash", Pattern: "rm -rf *",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}))
	promptCalled := false
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			promptCalled = true
			if !req.Dangerous {
				t.Error("dangerous flag should be true")
			}
			if req.Reason == "" {
				t.Error("reason should be populated")
			}
			return permission.Deny, true
		},
		func(tool, resource string) (bool, string) { return true, "destructive_rm" },
		time.Second, "")
	d, _ := m.Check(context.Background(), "sess", "Bash", "rm -rf /")
	if !promptCalled {
		t.Error("dangerous must force prompt even with allow_permanent")
	}
	if d != permission.Deny {
		t.Errorf("expected deny, got %v", d)
	}
}

func TestCheck_TimeoutDenies(t *testing.T) {
	s := newStore(t)
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			<-ctx.Done() // wait until timeout fires
			return permission.Deny, false
		}, nil, 20*time.Millisecond, "")
	d, err := m.Check(context.Background(), "sess", "Bash", "echo")
	if d != permission.Deny || !errors.Is(err, permission.ErrTimeout) {
		t.Errorf("got %v %v", d, err)
	}
}

// Spec scenario: permanent allow persists across sessions.
func TestCheck_PermanentAllowAcrossSessions(t *testing.T) {
	s := newStore(t)
	prompts := 0
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			prompts++
			return permission.AllowPermanent, true
		}, nil, time.Second, "")
	// First session triggers prompt + persists.
	_, _ = m.Check(context.Background(), "sessA", "Read", "docs/x.md")
	if prompts != 1 {
		t.Fatalf("first call should prompt once, got %d", prompts)
	}
	// Second session shouldn't prompt — the permanent allow now applies.
	_, _ = m.Check(context.Background(), "sessB", "Read", "docs/x.md")
	if prompts != 1 {
		t.Errorf("permanent allow should skip prompt on second session, prompts=%d", prompts)
	}
}

func TestCheck_DefaultDecisionFallsBack(t *testing.T) {
	s := newStore(t)
	promptCalled := false
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			promptCalled = true
			return permission.Deny, true
		}, nil, time.Second, permission.AllowPermanent)
	d, err := m.Check(context.Background(), "sess", "Read", "file.txt")
	if err != nil || d != permission.AllowPermanent {
		t.Errorf("got %v %v", d, err)
	}
	if promptCalled {
		t.Error("prompt should not fire when defaultDecision is set")
	}
}

func TestCheck_DefaultDecisionEmptyStillPrompts(t *testing.T) {
	s := newStore(t)
	promptCalled := false
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			promptCalled = true
			return permission.AllowSession, true
		}, nil, time.Second, "")
	d, _ := m.Check(context.Background(), "sess", "Bash", "echo")
	if d != permission.AllowSession {
		t.Errorf("got %v", d)
	}
	if !promptCalled {
		t.Error("empty defaultDecision should still prompt")
	}
}

func TestCheck_DangerousIgnoresDefaultDecision(t *testing.T) {
	s := newStore(t)
	promptCalled := false
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			promptCalled = true
			return permission.Deny, true
		},
		func(tool, resource string) (bool, string) { return true, "destructive_rm" },
		time.Second, permission.AllowPermanent)
	d, _ := m.Check(context.Background(), "sess", "Bash", "rm -rf /")
	if !promptCalled {
		t.Error("dangerous must force prompt even with defaultDecision")
	}
	if d != permission.Deny {
		t.Errorf("expected deny, got %v", d)
	}
}

func TestCheck_DenyPermanentOverridesDefaultDecision(t *testing.T) {
	s := newStore(t)
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "p1", Tool: "Read", Pattern: "**",
		Decision: store.DecisionDenyPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}))
	promptCalled := false
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			promptCalled = true
			return permission.AllowSession, true
		}, nil, time.Second, permission.AllowPermanent)
	d, err := m.Check(context.Background(), "sess", "Read", "file.txt")
	if err != nil || d != permission.DenyPermanent {
		t.Errorf("deny_permanent should override defaultDecision: got %v %v", d, err)
	}
	if promptCalled {
		t.Error("prompt should not fire when permanent deny matches")
	}
}

// A matching deny_permanent must win over a broader allow_permanent even when
// the allow was created first (e.g. a pre-existing preset). Otherwise
// retightening a permission with a deny rule would be silently ineffective.
func TestCheck_PermanentDenyOverridesEarlierAllow(t *testing.T) {
	s := newStore(t)
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "allow-broad", Tool: "Bash", Pattern: "*",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}))
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "deny-specific", Tool: "Bash", Pattern: "rm *",
		Decision: store.DecisionDenyPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now().Add(time.Second),
	}))
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			t.Error("prompt should not fire when a permanent rule matches")
			return permission.Deny, true
		}, nil, time.Second, "")
	d, err := m.Check(context.Background(), "sess", "Bash", "rm file")
	if err != nil || d != permission.DenyPermanent {
		t.Errorf("deny_permanent should win over earlier allow_permanent: got %v %v", d, err)
	}
	// A resource only the allow covers still resolves to allow.
	d, err = m.Check(context.Background(), "sess", "Bash", "ls")
	if err != nil || d != permission.AllowPermanent {
		t.Errorf("non-denied resource should allow: got %v %v", d, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
