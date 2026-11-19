package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"net/url"
	"syscall"
	"time"
)

const (
	defaultMaxRetries = 3
	defaultBaseDelay  = 500 * time.Millisecond
	DefaultMaxDelay   = 120 * time.Second
)

// sleepBlockErr reports why a sleep on ctx must not proceed, if the
// context is canceled or its deadline has elapsed. The deadline timer may
// not have fired yet (ctx.Err() still nil) right after the deadline: the
// direct deadline check catches that window.
func sleepBlockErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

var sleepFor = func(ctx context.Context, delay time.Duration) error {
	// Check before starting the timer: an already-canceled or exhausted
	// context must never read as a successful sleep.
	if err := sleepBlockErr(ctx); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		// The select can pick the timer even when ctx is also done (zero
		// or very short delay): cancellation wins over the timer.
		if err := sleepBlockErr(ctx); err != nil {
			return err
		}
		return nil
	}
}

// Backoff computes an exponential backoff duration with full jitter.
// The returned duration is capped at maxDelay; the sampled float is capped
// before the float-to-Duration conversion so huge attempt counts cannot
// overflow the conversion into implementation-defined (possibly negative)
// territory.
func Backoff(base, maxDelay time.Duration, attempt int) time.Duration {
	if base <= 0 {
		// Zero base stays zero regardless of attempt: 0 * +Inf is NaN,
		// which would bypass the cap comparison into an undefined
		// conversion. Only affects direct callers of the exported helper
		// (the agent normalizes zero bases).
		return 0
	}
	mult := math.Pow(2, float64(attempt))
	jitter := rand.Float64()
	sampled := float64(base) * mult * (0.5 + 0.5*jitter)
	if sampled >= float64(maxDelay) {
		return maxDelay
	}
	return time.Duration(sampled)
}

// ResolveRetryDelay resolves the delay before the next retry attempt for
// err: a positive RetryAfter on the API error takes precedence over the
// jittered exponential backoff, and the selected duration is capped by the
// context deadline. It does not sleep — callers pass the returned duration
// to SleepFor so that reported and actual waits come from one resolution
// (issue #16). The returned error is non-nil when the context is already
// canceled or its deadline is exhausted: the retry would never happen, so
// callers should terminate instead of announcing one.
func ResolveRetryDelay(ctx context.Context, err error, base, maxDelay time.Duration, attempt int) (time.Duration, error) {
	// An already-canceled or exhausted context would sleep zero and retry
	// nothing: terminate before announcing a retry.
	if cerr := ctx.Err(); cerr != nil {
		return 0, cerr
	}
	delay := Backoff(base, maxDelay, attempt)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		delay = apiErr.RetryAfter
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// The deadline timer may not have fired yet (ctx.Err() still
			// nil): report the exhaustion explicitly so the caller never
			// announces a retry that cannot happen.
			if cerr := ctx.Err(); cerr != nil {
				return 0, cerr
			}
			return 0, context.DeadlineExceeded
		}
		if delay > remaining {
			delay = remaining
		}
	}
	return delay, nil
}

// SleepFor waits for delay, returning ctx.Err() on cancellation or
// deadline exhaustion.
func SleepFor(ctx context.Context, delay time.Duration) error {
	return sleepFor(ctx, delay)
}

// WaitForRetry resolves the retry delay for err (ResolveRetryDelay) and
// sleeps it (SleepFor) — the same resolve-once policy the agent's retry
// event reporting uses, kept as a convenience wrapper.
func WaitForRetry(ctx context.Context, err error, base, maxDelay time.Duration, attempt int) error {
	delay, rerr := ResolveRetryDelay(ctx, err, base, maxDelay, attempt)
	if rerr != nil {
		return rerr
	}
	return SleepFor(ctx, delay)
}

// IsRetryableError reports whether the error is an API error that can be retried
// (429 rate limit or 5xx server error).
func IsRetryableError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	return apiErr.Retryable()
}

// IsNetworkError reports whether the error is a transient network error
// (DNS failure, connection refused, TCP timeout) that should be retried.
// It returns false for context cancellation errors and capability errors,
// which must NOT be retried.
func IsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrStreamingNotSupported) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr != nil && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr != nil {
		var unwrappedNetErr net.Error
		if errors.As(urlErr.Unwrap(), &unwrappedNetErr) && unwrappedNetErr != nil {
			return true
		}
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// SanitizeRetryReason returns a user-friendly error description for RetryEvent.Reason.
// It never exposes raw API response bodies — sensitive details go to slog only.
func SanitizeRetryReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 429:
			return "rate limited (429)"
		case apiErr.StatusCode >= 500:
			return fmt.Sprintf("server error (%d)", apiErr.StatusCode)
		default:
			return fmt.Sprintf("API error (%d)", apiErr.StatusCode)
		}
	}
	return "connection failed"
}
