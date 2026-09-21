// Package tui owns terminal input, rendering and all Charm UI dependencies.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// Run starts the Stage A status screen and returns after terminal restoration.
func (t Terminal) Run(ctx context.Context, state app.State) error {
	_, err := tea.NewProgram(
		newModel(state),
		tea.WithContext(ctx),
		tea.WithInput(t.Input),
		tea.WithOutput(t.Output),
	).Run()
	if err != nil {
		return fmt.Errorf("run terminal: %w", err)
	}
	return nil
}

type model struct {
	state    app.State
	viewport viewport.Model
	width    int
}

func newModel(state app.State) model {
	v := viewport.New(viewport.WithWidth(80), viewport.WithHeight(18))
	v.SetContent("Startup skeleton only.\n\nChat, tools and durable sessions are not connected.\nNo task will execute.")
	return model{state: state, viewport: v, width: 80}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, msg.Width)
		m.viewport.SetWidth(m.width)
		m.viewport.SetHeight(max(1, msg.Height-6))
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m model) View() tea.View {
	approval := "Approval: per call"
	if !m.state.ApprovalRequired {
		approval = "APPROVAL BYPASSED (--no-approval)"
	}
	header := lipgloss.NewStyle().Bold(true).Width(m.width).Render("go-agent | Stage A")
	footer := lipgloss.NewStyle().Width(m.width).Render(approval + "\nq / Esc / Ctrl+C: quit")
	view := tea.NewView(header + "\n\n" + m.viewport.View() + "\n" + footer)
	view.AltScreen = true
	return view
}
