// Package app owns the controller boundary, independent of terminal libraries.
package app

import (
	"context"

	"github.com/dailz1/go-agent/agent"
)

// State is a non-secret display value. It is not canonical conversation history.
type State struct {
	ApprovalRequired bool
}

// UI owns terminal input and rendering. Run returns after terminal cleanup.
type UI interface {
	Run(context.Context, State) error
}

// Worker owns blocking execution and returns only after its owned work exits.
// The observer is synchronous; false stops observation, not cancellation settlement.
// TODO(D): implement durable run ownership, bounded UI delivery and original-token
// settlement behind this boundary. No worker is started by the Stage A skeleton.
type Worker interface {
	Run(context.Context, string, func(agent.AgentEvent) bool) error
}
