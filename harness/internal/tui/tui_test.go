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

func TestSkeletonStartsAndExits(t *testing.T) {
	for _, key := range []string{"q", "\x1b", "\x03"} {
		t.Run(key, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			program := tea.NewProgram(
				newModel(app.State{ApprovalRequired: true}),
				tea.WithContext(ctx),
				tea.WithInput(strings.NewReader(key)),
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
