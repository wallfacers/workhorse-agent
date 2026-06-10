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

// check adapts the CheckInput signature for the many positional call sites in
// these tests; Source is asserted where it matters via checkSrc.
func check(m *permission.Manager, sessionID, tool, resource string) (permission.Decision, error) {
	d, _, err := m.Check(context.Background(), permission.CheckInput{
		SessionID: sessionID, Tool: tool, Resource: resource,
	})
	return d, err
}

func checkSrc(m *permission.Manager, sessionID, tool, resource string) (permission.Decision, permission.Source, error) {
	return m.Check(context.Background(), permission.CheckInput{
		SessionID: sessionID, Tool: tool, Resource: resource,
	})
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
	d, err := check(m, "sess", "Read", "docs/intro.md")
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
	d, err := check(m, "sess", "Bash", "ls")
	if err != nil || d != permission.AllowSession {
		t.Errorf("got %v %v", d, err)
	}
	// AllowSession should now skip the prompt on the same resource.
	d, _ = check(m, "sess", "Bash", "ls")
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
		_, _ = check(m, "sess", "Bash", "ls")
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
	d, _ := check(m, "sess", "Bash", "rm -rf /")
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
	d, err := check(m, "sess", "Bash", "echo")
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
	_, _ = check(m, "sessA", "Read", "docs/x.md")
	if prompts != 1 {
		t.Fatalf("first call should prompt once, got %d", prompts)
	}
	// Second session shouldn't prompt — the permanent allow now applies.
	_, _ = check(m, "sessB", "Read", "docs/x.md")
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
	d, err := check(m, "sess", "Read", "file.txt")
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
	d, _ := check(m, "sess", "Bash", "echo")
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
	d, _ := check(m, "sess", "Bash", "rm -rf /")
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
	d, err := check(m, "sess", "Read", "file.txt")
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
	d, err := check(m, "sess", "Bash", "rm file")
	if err != nil || d != permission.DenyPermanent {
		t.Errorf("deny_permanent should win over earlier allow_permanent: got %v %v", d, err)
	}
	// A resource only the allow covers still resolves to allow.
	d, err = check(m, "sess", "Bash", "ls")
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

// ---- tool-name glob (support-dataweave-headless-integration) ----

// Spec scenario: tool glob 免打扰放行 MCP 只读工具.
func TestCheck_ToolGlobMatchesMCPNames(t *testing.T) {
	s := newStore(t)
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "glob-ro", Tool: "dataweave__query_*", Pattern: "",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}))
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			t.Errorf("prompt should not fire for %s", req.Tool)
			return permission.Deny, true
		}, nil, time.Second, "")
	for _, tool := range []string{"dataweave__query_tasks", "dataweave__query_instances"} {
		d, src, err := checkSrc(m, "sess", tool, "")
		if err != nil || d != permission.AllowPermanent {
			t.Errorf("%s: got %v %v, want allow_permanent", tool, d, err)
		}
		if src != permission.SourceRule {
			t.Errorf("%s: source = %v, want rule", tool, src)
		}
	}
	// A non-matching tool still falls through (prompt declines → deny).
	m2 := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			return permission.Deny, true
		}, nil, time.Second, "")
	if d, _ := check(m2, "sess", "dataweave__node_exec", ""); d != permission.Deny {
		t.Errorf("non-matching tool should not be allowed by glob rule: got %v", d)
	}
}

// Spec scenario: tool glob 下 deny 仍优先.
func TestCheck_ToolGlobDenyStillWins(t *testing.T) {
	s := newStore(t)
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "glob-all", Tool: "dataweave__*", Pattern: "**",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}))
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "deny-exec", Tool: "dataweave__node_exec", Pattern: "",
		Decision: store.DecisionDenyPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now().Add(time.Second),
	}))
	m := permission.New(s, nil, nil, time.Second, "")
	if d, err := check(m, "sess", "dataweave__node_exec", "anything"); err != nil || d != permission.DenyPermanent {
		t.Errorf("deny_permanent should win over glob allow: got %v %v", d, err)
	}
	if d, err := check(m, "sess", "dataweave__query_tasks", "x"); err != nil || d != permission.AllowPermanent {
		t.Errorf("glob allow should still cover other tools: got %v %v", d, err)
	}
}

// Spec scenario: 无元字符规则语义不变 — a literal tool value must not match
// other tool names, and `*` matches any tool.
func TestCheck_ToolLiteralAndStarSemantics(t *testing.T) {
	s := newStore(t)
	must(t, s.SavePermission(context.Background(), &store.Permission{
		ID: "lit", Tool: "Bash", Pattern: "",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}))
	m := permission.New(s, nil, nil, time.Second, "")
	if d, _ := check(m, "sess", "Bash", "ls"); d != permission.AllowPermanent {
		t.Errorf("literal tool rule should match its own tool: got %v", d)
	}
	if d, _ := check(m, "sess", "Bashful", "ls"); d == permission.AllowPermanent {
		t.Error("literal tool rule must not match a longer tool name")
	}

	s2 := newStore(t)
	must(t, s2.SavePermission(context.Background(), &store.Permission{
		ID: "star", Tool: "*", Pattern: "",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
		CreatedAt: time.Now(),
	}))
	m2 := permission.New(s2, nil, nil, time.Second, "")
	for _, tool := range []string{"Read", "dataweave__query_tasks"} {
		if d, _ := check(m2, "sess", tool, "x"); d != permission.AllowPermanent {
			t.Errorf("* rule should match %s: got %v", tool, d)
		}
	}
}

// ---- decision Source (permission_resolved audit) ----

func TestCheck_SourceClassification(t *testing.T) {
	s := newStore(t)
	// default decision → SourceDefault
	m := permission.New(s, nil, nil, time.Second, permission.AllowPermanent)
	if _, src, _ := checkSrc(m, "sess", "Read", "f"); src != permission.SourceDefault {
		t.Errorf("default path: source = %v, want default", src)
	}
	// no prompt callback, no rule, no default → SourceNone + deny
	m2 := permission.New(s, nil, nil, time.Second, "")
	if d, src, _ := checkSrc(m2, "sess", "Read", "f"); d != permission.Deny || src != permission.SourceNone {
		t.Errorf("no-prompt path: got %v/%v, want deny/none", d, src)
	}
	// answered prompt → SourcePrompt
	m3 := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			return permission.AllowOnce, true
		}, nil, time.Second, "")
	if d, src, _ := checkSrc(m3, "sess", "Read", "f"); d != permission.AllowOnce || src != permission.SourcePrompt {
		t.Errorf("prompt path: got %v/%v, want allow_once/prompt", d, src)
	}
	// unanswered prompt → SourceTimeout + ErrTimeout
	m4 := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			<-ctx.Done()
			return permission.Deny, false
		}, nil, 20*time.Millisecond, "")
	d, src, err := checkSrc(m4, "sess", "Read", "f")
	if d != permission.Deny || src != permission.SourceTimeout || !errors.Is(err, permission.ErrTimeout) {
		t.Errorf("timeout path: got %v/%v/%v, want deny/timeout/ErrTimeout", d, src, err)
	}
}

// RequestID must flow from CheckInput into the prompt Request unchanged.
func TestCheck_RequestIDReachesPrompt(t *testing.T) {
	s := newStore(t)
	var got string
	m := permission.New(s,
		func(ctx context.Context, req permission.Request) (permission.Decision, bool) {
			got = req.RequestID
			return permission.AllowOnce, true
		}, nil, time.Second, "")
	_, _, _ = m.Check(context.Background(), permission.CheckInput{
		SessionID: "sess", RequestID: "toolu_123", Tool: "Read", Resource: "f",
	})
	if got != "toolu_123" {
		t.Errorf("prompt saw RequestID %q, want toolu_123", got)
	}
}
