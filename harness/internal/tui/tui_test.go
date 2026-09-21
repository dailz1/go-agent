package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dailz1/go-agent/harness/internal/app"
)

// The terminal surface must start headless and exit on the idle quit keys.
// "q" is no longer a quit binding: it is an ordinary input character.
func TestStartsAndExits(t *testing.T) {
	keys := []struct {
		name string
		raw  string
	}{
		{"esc", "\x1b"},
		{"ctrl+c", "\x03"},
	}
	for _, key := range keys {
		t.Run(key.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			m, _ := newTestModel(app.State{ApprovalRequired: true})
			program := tea.NewProgram(
				m,
				tea.WithContext(ctx),
				tea.WithInput(strings.NewReader(key.raw)),
				tea.WithOutput(io.Discard),
				tea.WithEnvironment([]string{"TERM=dumb"}),
				tea.WithoutRenderer(),
			)
			if _, err := program.Run(); err != nil {
				t.Fatalf("start and quit: %v", err)
			}
		})
	}
}
