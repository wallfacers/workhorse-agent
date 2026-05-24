package provider

import (
	"context"
	"errors"
	"time"
)

// RetryConfig is the small struct callers pass to Retry. Attempts is the
// number of attempts to make in total (NOT the number of retries after the
// first failure); BackoffMs[i] is the wait before attempt i+1, used only when
// the provider didn't emit a Retry-After header.
type RetryConfig struct {
	Attempts  int
	BackoffMs []int
}

// Retry calls fn repeatedly until it returns nil, until a non-retryable error
// is returned, until ctx is cancelled, or until Attempts have all been used.
//
// When a *ProviderError carries a RetryAfter() value, that delay wins over
// the configured backoff. Otherwise BackoffMs[attemptIndex] is consulted; if
// the slice is shorter than Attempts the last entry repeats.
//
// `Retry` accepts an optional onRetry callback so the agent loop can emit a
// provider_retry event after each backoff. Pass nil to skip.
func Retry(
	ctx context.Context,
	cfg RetryConfig,
	fn func(attempt int) error,
	onRetry func(attempt int, wait time.Duration, err *ProviderError),
) error {
	if cfg.Attempts <= 0 {
		cfg.Attempts = 1
	}
	var lastErr error
	for i := 0; i < cfg.Attempts; i++ {
		err := fn(i)
		if err == nil {
			return nil
		}
		lastErr = err

		// Classify: only *ProviderError with IsRetryable() == true gets a
		// second chance. Anything else short-circuits.
		var pe *ProviderError
		if !errors.As(err, &pe) || !pe.IsRetryable() {
			return err
		}

		// Last attempt — no point sleeping.
		if i == cfg.Attempts-1 {
			break
		}

		wait := pe.RetryAfter()
		if wait <= 0 {
			wait = backoffAt(cfg.BackoffMs, i)
		}
		if onRetry != nil {
			onRetry(i+1, wait, pe)
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return lastErr
}

// backoffAt returns the i-th backoff entry; if the slice is exhausted, the
// last entry is reused so the function never panics.
func backoffAt(backoff []int, i int) time.Duration {
	if len(backoff) == 0 {
		return 0
	}
	if i >= len(backoff) {
		i = len(backoff) - 1
	}
	return time.Duration(backoff[i]) * time.Millisecond
}
