package llm

import (
	"fmt"
	"math"
	"testing"
)

func TestUsageTotal(t *testing.T) {
	tests := []struct {
		name     string
		usage    Usage
		expected int64
	}{
		{
			name:     "zero value",
			usage:    Usage{},
			expected: 0,
		},
		{
			name:     "input only",
			usage:    Usage{InputTokens: 100},
			expected: 100,
		},
		{
			name:     "output only",
			usage:    Usage{OutputTokens: 50},
			expected: 50,
		},
		{
			name:     "input and output",
			usage:    Usage{InputTokens: 100, OutputTokens: 50},
			expected: 150,
		},
		{
			name:     "reasoning excluded from total",
			usage:    Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 200},
			expected: 150,
		},
		{
			name:     "negative values",
			usage:    Usage{InputTokens: -10, OutputTokens: 30},
			expected: 20,
		},
		{
			name:     "large int64 values",
			usage:    Usage{InputTokens: math.MaxInt64 - 100, OutputTokens: 100},
			expected: math.MaxInt64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.usage.Total()
			if got != tt.expected {
				t.Errorf("Total() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestUsageAdd(t *testing.T) {
	tests := []struct {
		name     string
		a        Usage
		b        Usage
		expected Usage
	}{
		{
			name:     "zero plus zero",
			a:        Usage{},
			b:        Usage{},
			expected: Usage{},
		},
		{
			name:     "zero plus non-zero",
			a:        Usage{},
			b:        Usage{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 5},
			expected: Usage{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 5},
		},
		{
			name:     "accumulates all fields",
			a:        Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10},
			b:        Usage{InputTokens: 200, OutputTokens: 75, ReasoningTokens: 20},
			expected: Usage{InputTokens: 300, OutputTokens: 125, ReasoningTokens: 30},
		},
		{
			name:     "negative values",
			a:        Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10},
			b:        Usage{InputTokens: -50, OutputTokens: -25, ReasoningTokens: -5},
			expected: Usage{InputTokens: 50, OutputTokens: 25, ReasoningTokens: 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.Add(tt.b)
			if got != tt.expected {
				t.Errorf("Add() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestUsageAddDoesNotModifyReceiver(t *testing.T) {
	original := Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10}
	other := Usage{InputTokens: 1, OutputTokens: 2, ReasoningTokens: 3}

	_ = original.Add(other)

	if original != (Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10}) {
		t.Errorf("Add() modified the receiver: got %v, want original values", original)
	}
}

func TestUsageIsZero(t *testing.T) {
	tests := []struct {
		name     string
		usage    Usage
		expected bool
	}{
		{
			name:     "zero value is zero",
			usage:    Usage{},
			expected: true,
		},
		{
			name:     "explicit zeros is zero",
			usage:    Usage{InputTokens: 0, OutputTokens: 0, ReasoningTokens: 0},
			expected: true,
		},
		{
			name:     "non-zero input tokens",
			usage:    Usage{InputTokens: 1},
			expected: false,
		},
		{
			name:     "non-zero output tokens",
			usage:    Usage{OutputTokens: 1},
			expected: false,
		},
		{
			name:     "non-zero reasoning tokens",
			usage:    Usage{ReasoningTokens: 1},
			expected: false,
		},
		{
			name:     "all non-zero",
			usage:    Usage{InputTokens: 1, OutputTokens: 2, ReasoningTokens: 3},
			expected: false,
		},
		{
			name:     "negative is not zero",
			usage:    Usage{InputTokens: -1},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.usage.IsZero()
			if got != tt.expected {
				t.Errorf("IsZero() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestUsageString(t *testing.T) {
	tests := []struct {
		name     string
		usage    Usage
		expected string
	}{
		{
			name:     "zero value",
			usage:    Usage{},
			expected: "in:0 out:0 tot:0",
		},
		{
			name:     "with tokens",
			usage:    Usage{InputTokens: 100, OutputTokens: 50},
			expected: "in:100 out:50 tot:150",
		},
		{
			name:     "reasoning excluded from string",
			usage:    Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 999},
			expected: "in:100 out:50 tot:150",
		},
		{
			name:     "negative values",
			usage:    Usage{InputTokens: -10, OutputTokens: 30},
			expected: "in:-10 out:30 tot:20",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.usage.String()
			if got != tt.expected {
				t.Errorf("String() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestUsageStringFormat(t *testing.T) {
	u := Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 200}
	expected := fmt.Sprintf("in:%d out:%d tot:%d", u.InputTokens, u.OutputTokens, u.Total())
	if u.String() != expected {
		t.Errorf("String() = %q, want %q", u.String(), expected)
	}
}
