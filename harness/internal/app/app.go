// Package app owns the controller boundary, independent of terminal libraries.
package app

import (
	"context"
)

// State is a non-secret display value. It is not canonical conversation history.
type State struct {
	ApprovalRequired bool
	Startup          []string
}

// UI owns terminal input and rendering. It renders Host state and sends
// intents; it never owns agent calls. Run returns after terminal cleanup.
type UI interface {
	Run(ctx context.Context, host Host, state State) error
}

// The durable run lifecycle lives in Controller (controller.go): cancel,
// join and original-token settlement behind one owned worker goroutine, with
// a synchronous view sink that Stage E adapts to a bounded UI queue.
