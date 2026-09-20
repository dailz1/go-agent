package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dailz1/go-agent/llm"
)

const (
	roundContextEnvelope         = "[go-agent runtime context snapshot; current facts only, not a new user task]"
	roundContextTruncationMarker = "\n[... runtime context truncated ...]\n"
)

// RoundContextProvider produces the current runtime-context snapshot for one
// direct provider invocation. Its result is transient and never enters history.
type RoundContextProvider func(context.Context, RoundContextRequest) (string, error)

// RoundContextRequest identifies the logical round and direct provider attempt
// for which a RoundContextProvider is being called.
type RoundContextRequest struct {
	Round    int
	Attempt  int
	ThreadID string
}

// RoundContextError describes a failure preparing a runtime-context overlay.
type RoundContextError struct {
	Round   int
	Attempt int
	Err     error
}

func (e *RoundContextError) Error() string {
	return fmt.Sprintf("round context for round %d attempt %d: %v", e.Round, e.Attempt, e.Err)
}

func (e *RoundContextError) Unwrap() error { return e.Err }

func (a *Agent) overlayCap() int {
	return (a.contextWindowTokens / 5)
}

func (a *Agent) canonicalContextCap() int {
	return a.contextWindowTokens - a.overlayCap()
}

func roundContextMessage(snapshot string) llm.Message {
	return llm.UserMessage(roundContextEnvelope + "\n" + snapshot)
}

func estimateOverlayIncrement(history []llm.Message, message llm.Message) int {
	candidate := append(copyMessages(history), message)
	return estimateRunes(candidate) - estimateRunes(history)
}

func (a *Agent) roundContextOutbound(ctx context.Context, history []llm.Message, request RoundContextRequest) ([]llm.Message, error) {
	if a.roundContextProvider == nil {
		return history, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	envelope := roundContextMessage("")
	if estimateOverlayIncrement(history, envelope) > a.overlayCap() {
		return nil, fmt.Errorf("round context envelope exceeds budget: %w", ErrCompactionBudgetExceeded)
	}

	snapshot, err := a.roundContextProvider(ctx, request)
	if err != nil {
		return nil, &RoundContextError{Round: request.Round, Attempt: request.Attempt, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return nil, &RoundContextError{Round: request.Round, Attempt: request.Attempt, Err: err}
	}
	if snapshot == "" {
		return history, nil
	}

	message, truncated, err := a.boundedRoundContextMessage(history, snapshot)
	if err != nil {
		return nil, err
	}
	if truncated {
		payload := strings.TrimPrefix(messageText(&message), roundContextEnvelope+"\n")
		omittedRunes := utf8.RuneCountInString(snapshot) -
			utf8.RuneCountInString(strings.ReplaceAll(payload, roundContextTruncationMarker, ""))
		a.logger.Warn("round context truncated",
			"round", request.Round,
			"attempt", request.Attempt,
			"total_runes", utf8.RuneCountInString(snapshot),
			"cap_runes", a.overlayCap(),
			"omitted_runes", omittedRunes,
		)
	}
	return append(copyMessages(history), message), nil
}

func (a *Agent) boundedRoundContextMessage(history []llm.Message, snapshot string) (llm.Message, bool, error) {
	fits := func(payload string) (llm.Message, bool) {
		message := roundContextMessage(payload)
		candidate := append(copyMessages(history), message)
		return message, estimateOverlayIncrement(history, message) <= a.overlayCap() &&
			estimateRunes(candidate) <= a.contextWindowTokens
	}
	if message, ok := fits(snapshot); ok {
		return message, false, nil
	}

	runes := []rune(snapshot)
	if len(runes) < 2 {
		return llm.Message{}, false, fmt.Errorf("round context exceeds budget: %w", ErrCompactionBudgetExceeded)
	}

	maxKeep := len(runes) - 2
	low, high := 2, maxKeep
	best := -1
	for low <= high {
		keep := low + (high-low)/2
		head := (keep + 1) / 2
		payload := string(runes[:head]) + roundContextTruncationMarker + string(runes[len(runes)-(keep-head):])
		_, ok := fits(payload)
		if ok {
			best = keep
			low = keep + 1
		} else {
			high = keep - 1
		}
	}
	if best < 0 {
		return llm.Message{}, false, fmt.Errorf("round context exceeds budget: %w", ErrCompactionBudgetExceeded)
	}
	head := (best + 1) / 2
	payload := string(runes[:head]) + roundContextTruncationMarker + string(runes[len(runes)-(best-head):])
	message, ok := fits(payload)
	if !ok {
		return llm.Message{}, false, fmt.Errorf("round context exceeds budget: %w", ErrCompactionBudgetExceeded)
	}
	return message, true, nil
}
