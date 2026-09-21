// Package tui owns terminal input, rendering and all Charm UI dependencies.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/dailz1/go-agent/harness/internal/app"
)

// Terminal is the sole owner of the application's terminal streams.
type Terminal struct {
	Input  io.Reader
	Output io.Writer
}

// IsTerminal requires both streams to be real terminals, not character devices
// such as /dev/null. It never opens a fallback controlling terminal.
func IsTerminal(in *os.File, out io.Writer) bool {
	output, ok := out.(*os.File)
	return ok && term.IsTerminal(in.Fd()) && term.IsTerminal(output.Fd())
}

// Run starts the interactive chat surface. The bridge enforces the
// worker/UI isolation of harness/DESIGN.md §3: the Bubble Tea update loop
// only touches memory, every host intent runs on a bridge goroutine, and
// host results re-enter the program as ordinary messages. When the program
// exits, the bridge is stopped and joined before Run returns, so terminal
// exit never abandons an in-flight intent halfway; cancelling the active
// run itself is main's deferred controller close, not a UI concern.
func (t Terminal) Run(ctx context.Context, host app.Host, state app.State) error {
	b := newBridge(host)
	program := tea.NewProgram(
		newModel(state, host, b.exec),
		tea.WithContext(ctx),
		tea.WithInput(t.Input),
		tea.WithOutput(t.Output),
	)
	b.start(ctx, program.Send)
	defer b.stop()
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run terminal: %w", err)
	}
	return nil
}
