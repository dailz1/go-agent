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

var sleepFor = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Backoff computes an exponential backoff duration with full jitter.
// The returned duration is capped at maxDelay.
func Backoff(base, maxDelay time.Duration, attempt int) time.Duration {
	mult := math.Pow(2, float64(attempt))
	jitter := rand.Float64()
	d := time.Duration(float64(base) * mult * (0.5 + 0.5*jitter))
	if d > maxDelay {
		return maxDelay
	}
	return d
}

// WaitForRetry waits before the next retry attempt for err.
// If err carries an APIError with a positive RetryAfter, that duration is used
// instead of the jittered exponential backoff; otherwise Backoff(base,
// maxDelay, attempt) applies. The wait is capped by the context deadline and
// returns ctx.Err() on cancellation or deadline exhaustion.
func WaitForRetry(ctx context.Context, err error, base, maxDelay time.Duration, attempt int) error {
	return waitForRetry(ctx, err, base, maxDelay, attempt)
}

func waitForRetry(ctx context.Context, err error, base, maxDelay time.Duration, attempt int) error {
	delay := Backoff(base, maxDelay, attempt)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		delay = apiErr.RetryAfter
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx.Err()
		}
		if delay > remaining {
			delay = remaining
		}
	}
	return sleepFor(ctx, delay)
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
