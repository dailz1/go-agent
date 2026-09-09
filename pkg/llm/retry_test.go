package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"
)

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"429 rate limit", &APIError{StatusCode: 429}, true},
		{"500 server error", &APIError{StatusCode: 500}, true},
		{"502 bad gateway", &APIError{StatusCode: 502}, true},
		{"503 service unavailable", &APIError{StatusCode: 503}, true},
		{"529", &APIError{StatusCode: 529}, true},
		{"400 bad request", &APIError{StatusCode: 400}, false},
		{"401 unauthorized", &APIError{StatusCode: 401}, false},
		{"403 forbidden", &APIError{StatusCode: 403}, false},
		{"404 not found", &APIError{StatusCode: 404}, false},
		{"nil error", nil, false},
		{"wrapped APIError", fmt.Errorf("wrapped: %w", &APIError{StatusCode: 429}), true},
		{"generic error", fmt.Errorf("something failed"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryableError(tt.err); got != tt.want {
				t.Errorf("IsRetryableError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsNetworkError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"plain connection message", errors.New("http request: connection refused"), false},
		{"plain DNS message", errors.New("dns resolution failed"), false},
		{"timeout", &net.DNSError{Err: "request timed out", IsTimeout: true}, true},
		{"URL-wrapped network error", &url.Error{Op: "Get", URL: "https://example.com", Err: &net.DNSError{Err: "no such host"}}, true},
		{"wrapped connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"wrapped connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{"wrapped broken pipe", fmt.Errorf("write: %w", syscall.EPIPE), true},
		{"APIError 429", &APIError{StatusCode: 429}, false},
		{"APIError 500", &APIError{StatusCode: 500}, false},
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		{"ErrStreamingNotSupported", ErrStreamingNotSupported, false},
		{"nil error", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNetworkError(tt.err); got != tt.want {
				t.Errorf("IsNetworkError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsNetworkError_DeterministicError(t *testing.T) {
	if IsNetworkError(errors.New("deterministic")) {
		t.Error("IsNetworkError() = true, want false")
	}
}

func TestIsNetworkError_TypedNilAPIError(t *testing.T) {
	var apiErr *APIError
	if IsNetworkError(apiErr) {
		t.Error("IsNetworkError() = true, want false")
	}
}

func TestIsNetworkError_Timeout(t *testing.T) {
	err := &net.DNSError{Err: "request timed out", IsTimeout: true}
	if !IsNetworkError(err) {
		t.Error("IsNetworkError() = false, want true")
	}
}

func TestIsRetryableError_TypedNilAPIError(t *testing.T) {
	var apiErr *APIError
	if IsRetryableError(apiErr) {
		t.Error("IsRetryableError() = true, want false")
	}
}

func TestBackoff(t *testing.T) {
	base := 500 * time.Millisecond
	maxDelay := DefaultMaxDelay

	t.Run("monotonically increasing on average", func(t *testing.T) {
		var sum0, sum1, sum2 float64
		runs := 1000
		for i := 0; i < runs; i++ {
			sum0 += float64(Backoff(base, maxDelay, 0))
			sum1 += float64(Backoff(base, maxDelay, 1))
			sum2 += float64(Backoff(base, maxDelay, 2))
		}
		avg0 := sum0 / float64(runs)
		avg1 := sum1 / float64(runs)
		avg2 := sum2 / float64(runs)

		if avg0 >= avg1 {
			t.Errorf("average backoff(0)=%v >= backoff(1)=%v", avg0, avg1)
		}
		if avg1 >= avg2 {
			t.Errorf("average backoff(1)=%v >= backoff(2)=%v", avg1, avg2)
		}
	})

	t.Run("in range with jitter", func(t *testing.T) {
		for attempt := 0; attempt < 5; attempt++ {
			for i := 0; i < 100; i++ {
				got := Backoff(base, maxDelay, attempt)
				mult := float64(uint64(1) << attempt) // 2^attempt
				minDur := time.Duration(float64(base) * mult * 0.5)
				maxDur := time.Duration(float64(base) * mult)
				if got < minDur || got > maxDur {
					t.Errorf("Backoff(%v, %v, %d) = %v, want [%v, %v]", base, maxDelay, attempt, got, minDur, maxDur)
					break
				}
			}
		}
	})

	t.Run("capped at maxDelay", func(t *testing.T) {
		smallMax := 1 * time.Second
		for attempt := 5; attempt < 20; attempt++ {
			got := Backoff(base, smallMax, attempt)
			if got > smallMax {
				t.Errorf("Backoff(%v, %v, %d) = %v, want <= %v", base, smallMax, attempt, got, smallMax)
			}
		}
	})
}

func TestWaitForRetry(t *testing.T) {
	originalSleepFor := sleepFor
	t.Cleanup(func() { sleepFor = originalSleepFor })

	var recordedDelay time.Duration
	sleepFor = func(_ context.Context, delay time.Duration) error {
		recordedDelay = delay
		return nil
	}

	t.Run("Retry-After skips jittered backoff", func(t *testing.T) {
		recordedDelay = 0
		err := waitForRetry(
			context.Background(),
			fmt.Errorf("wrapped: %w", &APIError{StatusCode: 429, RetryAfter: 2 * time.Second}),
			defaultBaseDelay,
			DefaultMaxDelay,
			0,
		)
		if err != nil {
			t.Fatalf("waitForRetry() error = %v", err)
		}
		if recordedDelay != 2*time.Second {
			t.Errorf("delay = %v, want 2s", recordedDelay)
		}
	})

	t.Run("missing Retry-After uses jittered backoff", func(t *testing.T) {
		recordedDelay = 0
		err := waitForRetry(
			context.Background(),
			&APIError{StatusCode: 429},
			defaultBaseDelay,
			DefaultMaxDelay,
			0,
		)
		if err != nil {
			t.Fatalf("waitForRetry() error = %v", err)
		}
		if recordedDelay < 250*time.Millisecond || recordedDelay > 500*time.Millisecond {
			t.Errorf("delay = %v, want jittered backoff in [250ms, 500ms]", recordedDelay)
		}
	})

	t.Run("exported wrapper honors Retry-After over base backoff", func(t *testing.T) {
		recordedDelay = 0
		err := WaitForRetry(
			context.Background(),
			&APIError{StatusCode: 429, RetryAfter: 2 * time.Second},
			defaultBaseDelay,
			DefaultMaxDelay,
			3,
		)
		if err != nil {
			t.Fatalf("WaitForRetry() error = %v", err)
		}
		if recordedDelay != 2*time.Second {
			t.Errorf("delay = %v, want 2s (Retry-After, not attempt-3 backoff)", recordedDelay)
		}
	})

	t.Run("Retry-After is capped by context deadline", func(t *testing.T) {
		recordedDelay = 0
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		err := waitForRetry(
			ctx,
			&APIError{StatusCode: 429, RetryAfter: 2 * time.Second},
			defaultBaseDelay,
			DefaultMaxDelay,
			0,
		)
		if err != nil {
			t.Fatalf("waitForRetry() error = %v", err)
		}
		if recordedDelay <= 0 || recordedDelay > 50*time.Millisecond {
			t.Errorf("delay = %v, want context-capped delay in (0, 50ms]", recordedDelay)
		}
	})
}

func TestSanitizeRetryReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"429", &APIError{StatusCode: 429}, "rate limited (429)"},
		{"500", &APIError{StatusCode: 500}, "server error (500)"},
		{"502", &APIError{StatusCode: 502}, "server error (502)"},
		{"503", &APIError{StatusCode: 503}, "server error (503)"},
		{"400", &APIError{StatusCode: 400}, "API error (400)"},
		{"401", &APIError{StatusCode: 401}, "API error (401)"},
		{"network error", fmt.Errorf("connection refused"), "connection failed"},
		{"nil error", nil, "unknown error"},
		{"wrapped APIError 429", fmt.Errorf("wrapped: %w", &APIError{StatusCode: 429}), "rate limited (429)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeRetryReason(tt.err); got != tt.want {
				t.Errorf("SanitizeRetryReason() = %q, want %q", got, tt.want)
			}
		})
	}
}
