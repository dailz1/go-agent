package llm

import (
	"context"
	"fmt"
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
		{"network error", fmt.Errorf("http request: connection refused"), true},
		{"DNS failure", fmt.Errorf("dns resolution failed"), true},
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
