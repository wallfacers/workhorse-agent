package agent

import (
	"context"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// RetryConfig encodes the spec's "indefinite-but-bounded exponential backoff"
// for provider retries. Backoff is one entry per retry attempt; if the slice
// is shorter than Attempts the last value is reused.
type RetryConfig struct {
	Attempts int
	Backoff  []time.Duration
}

// DefaultRetryConfig matches the agent-loop spec: 3 attempts with 500ms / 2s
// / 8s waits between them.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		Attempts: 3,
		Backoff:  []time.Duration{500 * time.Millisecond, 2 * time.Second, 8 * time.Second},
	}
}

// onRetryFn is invoked before each retry sleep with the retry attempt number
// (1-indexed), the wait the loop is about to take, and the provider error that
// triggered it. Used by the agent loop to emit `provider_retry` events.
type onRetryFn func(attempt int, wait time.Duration, code string)

// streamWithRetry calls p.Stream up to Attempts+1 times. The first call counts
// as attempt 0; retries get backoff[attempt-1] (or the last entry if attempt
// exceeds the slice). Each call's returned channel is fully drained by the
// caller — this helper only handles the *upfront* error (request never sent)
// and IsRetryable() classification.
//
// Returns the live channel + nil on success, or the final ProviderError on
// give-up. ctx cancellation aborts retries immediately and returns
// CodeCanceled.
func streamWithRetry(
	ctx context.Context,
	p provider.Provider,
	req provider.Request,
	cfg RetryConfig,
	onRetry onRetryFn,
) (<-chan provider.ProviderEvent, error) {
	for attempt := 0; ; attempt++ {
		ch, err := p.Stream(ctx, req)
		if err == nil {
			return ch, nil
		}
		pe, ok := provider.AsProviderError(err)
		if !ok || !pe.IsRetryable() || attempt >= cfg.Attempts {
			return nil, err
		}
		wait := backoffFor(cfg, attempt, pe.RetryAfter())
		if onRetry != nil {
			onRetry(attempt+1, wait, pe.Code)
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, provider.NewProviderError(p.Name(), 0, provider.CodeCanceled,
				"retry cancelled", err)
		}
	}
}

// backoffFor returns the wait before the (attempt+1)-th call. The
// server-supplied Retry-After (if larger) wins; this matches the spec's
// "if ProviderError.RetryAfter() given value > backoff, take RetryAfter()".
func backoffFor(cfg RetryConfig, attempt int, retryAfter time.Duration) time.Duration {
	var base time.Duration
	switch {
	case len(cfg.Backoff) == 0:
		base = 500 * time.Millisecond
	case attempt < len(cfg.Backoff):
		base = cfg.Backoff[attempt]
	default:
		base = cfg.Backoff[len(cfg.Backoff)-1]
	}
	if retryAfter > base {
		return retryAfter
	}
	return base
}

// sleep respects ctx; returns ctx.Err() if interrupted.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// providerErrorCodeFor returns the api-protocol `error.code` to surface for a
// given ProviderError. The map covers the non-retryable codes that the agent
// loop reports to the client; retryable ones are only logged.
func providerErrorCodeFor(pe *provider.ProviderError) (code string, recoverable bool) {
	switch pe.Code {
	case provider.CodeAuthFailed:
		return "provider_auth_failed", false
	case provider.CodeInvalidRequest:
		return "provider_invalid_request", false
	case provider.CodeContextLengthExceeded:
		return "provider_context_length_exceeded", false
	case provider.CodeInsufficientQuota:
		return "provider_insufficient_quota", false
	case provider.CodeCanceled:
		return "provider_unrecoverable", false
	default:
		return "provider_unrecoverable", false
	}
}
