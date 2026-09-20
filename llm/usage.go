package llm

import "fmt"

// Usage tracks token consumption for a single LLM request.
type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}

// Total returns the sum of input and output tokens.
func (u Usage) Total() int64 {
	return u.InputTokens + u.OutputTokens
}

// Add returns a new Usage with each field summed from u and other.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:     u.InputTokens + other.InputTokens,
		OutputTokens:    u.OutputTokens + other.OutputTokens,
		ReasoningTokens: u.ReasoningTokens + other.ReasoningTokens,
	}
}

// IsZero reports whether all token counts are zero.
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.ReasoningTokens == 0
}

// String returns a human-readable summary in the form "in:X out:Y tot:Z".
func (u Usage) String() string {
	return fmt.Sprintf("in:%d out:%d tot:%d", u.InputTokens, u.OutputTokens, u.Total())
}
