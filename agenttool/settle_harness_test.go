package agenttool_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/dailz1/go-agent/agent"
)

type settleOwnedTarget struct {
	agent *agent.Agent
	token agent.SettlementToken
}

// This test harness owns the delegation inventory; agenttool does not discover it.
type settleHarness struct {
	parent           settleOwnedTarget
	children         []settleOwnedTarget
	expectedChildren int
}

var errSettleIdentityMissing = errors.New("child cleanup unconfirmed: missing identity")

func (h settleHarness) cleanup(ctx context.Context) (string, error) {
	if len(h.children) != h.expectedChildren {
		return "", errSettleIdentityMissing
	}
	for _, child := range h.children {
		if err := child.agent.SettleThread(ctx, child.token); err != nil && !errors.Is(err, agent.ErrNothingToSettle) {
			return child.token.ThreadID, fmt.Errorf("child %s pending: %w", child.token.ThreadID, err)
		}
	}
	return "", h.parent.agent.SettleThread(ctx, h.parent.token)
}

func (h settleHarness) redirect(ctx context.Context, input string) (string, error) {
	pending, err := h.cleanup(ctx)
	if err != nil {
		return pending, err
	}
	_, err = h.parent.agent.RunThread(ctx, h.parent.token.ThreadID, input)
	return "", err
}
