package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wallfacers/data-agent/internal/provider"
)

func TestProviderError_Retryable(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{provider.CodeRateLimited, true},
		{provider.CodeServerError, true},
		{provider.CodeNetworkError, true},
		{provider.CodeStreamBroken, true},
		{provider.CodeAuthFailed, false},
		{provider.CodeInvalidRequest, false},
		{provider.CodeContextLengthExceeded, false},
		{provider.CodeInsufficientQuota, false},
		{provider.CodeCanceled, false},
		{"floof", false},
	}
	for _, c := range cases {
		pe := provider.NewProviderError("test", 0, c.code, "", nil)
		if got := pe.IsRetryable(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.code, got, c.want)
		}
	}
}

func TestProviderError_RetryAfter(t *testing.T) {
	pe := provider.NewProviderError("anthropic", 429, provider.CodeRateLimited, "", nil)
	pe.SetRetryAfter(5 * time.Second)
	if pe.RetryAfter() != 5*time.Second {
		t.Errorf("RetryAfter: got %v, want 5s", pe.RetryAfter())
	}
}

func TestProviderError_ErrorString(t *testing.T) {
	pe := provider.NewProviderError("openai", 401, provider.CodeAuthFailed, "bad key", nil)
	got := pe.Error()
	if !strings.Contains(got, "openai") || !strings.Contains(got, "401") {
		t.Errorf("error string missing provider/status: %q", got)
	}
}

func TestModelPolicy_Pick(t *testing.T) {
	p := provider.ModelPolicy{
		Default: "anthropic:claude-sonnet-4-6",
		Fast:    "anthropic:claude-haiku-4-5-20251001",
		BySessionType: map[string]string{
			"researcher": "openai:gpt-4o",
		},
	}
	cases := []struct {
		name, agentType, override, wantP, wantM string
	}{
		{"default", "", "", "anthropic", "claude-sonnet-4-6"},
		{"by_type", "researcher", "", "openai", "gpt-4o"},
		{"override_wins", "researcher", "anthropic:custom", "anthropic", "custom"},
		{"unknown_type", "ghost", "", "anthropic", "claude-sonnet-4-6"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pname, m := p.Pick(c.agentType, c.override)
			if pname != c.wantP || m != c.wantM {
				t.Errorf("got (%q,%q), want (%q,%q)", pname, m, c.wantP, c.wantM)
			}
		})
	}
}

// Scenario from spec: 同家压缩 — OpenAI session must compact with gpt-4o-mini.
func TestModelPolicy_PickFast_SameFamily(t *testing.T) {
	p := provider.ModelPolicy{
		Fast: "anthropic:claude-haiku-4-5-20251001",
	}
	// Anthropic session: keep the configured fast.
	pname, m := p.PickFast("anthropic")
	if pname != "anthropic" || m != "claude-haiku-4-5-20251001" {
		t.Errorf("anthropic session: got (%q,%q)", pname, m)
	}
	// OpenAI session: switch to gpt-4o-mini despite Fast being Anthropic.
	pname, m = p.PickFast("openai")
	if pname != "openai" || m != "gpt-4o-mini" {
		t.Errorf("openai session: got (%q,%q)", pname, m)
	}
}

func TestRetry_HappyPath(t *testing.T) {
	calls := 0
	err := provider.Retry(context.Background(),
		provider.RetryConfig{Attempts: 3, BackoffMs: []int{1, 1, 1}},
		func(int) error {
			calls++
			return nil
		}, nil)
	if err != nil || calls != 1 {
		t.Errorf("expected 1 call no error, got calls=%d err=%v", calls, err)
	}
}

func TestRetry_StopsOnNonRetryable(t *testing.T) {
	calls := 0
	err := provider.Retry(context.Background(),
		provider.RetryConfig{Attempts: 5, BackoffMs: []int{1, 1, 1, 1, 1}},
		func(int) error {
			calls++
			return provider.NewProviderError("openai", 401, provider.CodeAuthFailed, "", nil)
		}, nil)
	if calls != 1 {
		t.Errorf("non-retryable should abort: calls=%d", calls)
	}
	pe, ok := provider.AsProviderError(err)
	if !ok || pe.Code != provider.CodeAuthFailed {
		t.Errorf("expected AuthFailed, got %v", err)
	}
}

func TestRetry_RetriesRetryableUntilExhausted(t *testing.T) {
	calls := 0
	err := provider.Retry(context.Background(),
		provider.RetryConfig{Attempts: 3, BackoffMs: []int{1, 1, 1}},
		func(int) error {
			calls++
			return provider.NewProviderError("anthropic", 429, provider.CodeRateLimited, "", nil)
		}, nil)
	if calls != 3 {
		t.Errorf("retry until exhausted: calls=%d, want 3", calls)
	}
	if err == nil {
		t.Error("expected error after exhausting retries")
	}
}

func TestRetry_PrefersRetryAfter(t *testing.T) {
	var slept []time.Duration
	start := time.Now()
	_ = provider.Retry(context.Background(),
		provider.RetryConfig{Attempts: 2, BackoffMs: []int{500}},
		func(int) error {
			pe := provider.NewProviderError("a", 429, provider.CodeRateLimited, "", nil)
			pe.SetRetryAfter(10 * time.Millisecond) // override 500ms backoff
			return pe
		},
		func(attempt int, wait time.Duration, pe *provider.ProviderError) {
			slept = append(slept, wait)
		})
	elapsed := time.Since(start)
	if len(slept) != 1 || slept[0] != 10*time.Millisecond {
		t.Errorf("expected RetryAfter to win, got slept=%v", slept)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("used full backoff instead of RetryAfter: elapsed=%v", elapsed)
	}
}

func TestRetry_HonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := provider.Retry(ctx,
		provider.RetryConfig{Attempts: 5, BackoffMs: []int{50, 50, 50, 50, 50}},
		func(int) error {
			return provider.NewProviderError("a", 503, provider.CodeServerError, "", nil)
		}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected ctx.Canceled, got %v", err)
	}
}
