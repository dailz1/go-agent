package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/dailz1/go-agent/harness/internal/app"
	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/harness/internal/snapshot"
)

// Messages the bridge and intents deliver into the program. They wrap app
// package types only; no terminal type crosses into app.
type (
	uiEventMsg  app.UIEvent
	approvalMsg struct{ req app.ApprovalRequest }
	noticeMsg   struct{ err error }
	draftMsg    string
	sessionsMsg struct {
		sessions []session.Meta
		err      error
	}
	changesMsg struct {
		records []snapshot.Record
		err     error
	}
	restoreDoneMsg struct {
		record snapshot.Record
		err    error
	}
	grantAppliedMsg struct{ err error }
	stopResultMsg   struct{ err error }
)

var errBridgeBusy = errors.New("terminal bridge is busy; retry the action")

// overlayKind selects the modal pane. Only one pane is active; the approval
// pane preempts the others because a run is waiting on its answer.
type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayApproval
	overlayGrant
	overlaySessions
	overlayChanges
	overlayRestore
	overlayDecision
	overlayHelp
)

type overlay struct {
	kind       overlayKind
	approval   *app.ApprovalRequest
	scroll     viewport.Model
	sessions   []session.Meta
	sel        int
	changes    []snapshot.Record
	grantPaths []string
	grantSel   int
	pathInput  textinput.Model
	blocked    bool // decision pane in settlement-blocked mode
}

// model is the terminal chat surface. It renders controller view state and
// sends intents; it never calls a provider, waits on disk, or reads stdin
// outside the Bubble Tea loop.
type model struct {
	state app.State
	host  app.Host
	exec  func(fn func(context.Context, func(tea.Msg)))

	width, height int

	ev            app.UIEvent
	hasEv         bool
	decisionShown bool
	expanded      bool
	grant         app.GrantInfo
	hasGrant      bool

	input  textarea.Model
	scroll viewport.Model

	overlay     overlay
	notice      string
	quitPending bool
}

func newModel(state app.State, host app.Host, exec func(fn func(context.Context, func(tea.Msg)))) model {
	input := textarea.New()
	input.ShowLineNumbers = false
	input.Prompt = ""
	input.Placeholder = "describe a task…  (enter send · ctrl+j newline)"
	km := input.KeyMap
	km.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "newline"))
	input.KeyMap = km
	input.SetWidth(78)
	input.SetHeight(2)
	input.CharLimit = 0
	_ = input.Focus()

	scroll := viewport.New(viewport.WithWidth(80), viewport.WithHeight(16))
	scroll.SetContent(startupContent(state))

	m := model{
		state: state, host: host, exec: exec,
		width: 80, height: 24,
		input: input, scroll: scroll,
	}
	return m
}

func startupContent(state app.State) string {
	lines := []string{"go-agent — interactive coding harness", ""}
	lines = append(lines, state.Startup...)
	return expandTabs(sanitize(strings.Join(lines, "\n")))
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case uiEventMsg:
		return m.applyEvent(app.UIEvent(msg))
	case approvalMsg:
		m.overlay = newApprovalOverlay(msg.req, m.width, m.bodyHeight())
		return m, nil
	case sessionsMsg:
		if msg.err != nil {
			m.notice = msg.err.Error()
			return m, nil
		}
		m.overlay = newSessionsOverlay(msg.sessions, m.width, m.bodyHeight())
		return m, nil
	case changesMsg:
		if msg.err != nil {
			m.notice = msg.err.Error()
			return m, nil
		}
		m.overlay = newChangesOverlay(msg.records, m.width, m.bodyHeight())
		return m, nil
	case restoreDoneMsg:
		if msg.err != nil {
			m.notice = "restore failed: " + msg.err.Error()
		} else {
			m.notice = "restored " + msg.record.Change.Path + " — conversation not rolled back"
		}
		return m, nil
	case grantAppliedMsg:
		if msg.err != nil {
			m.notice = "grant failed: " + msg.err.Error()
		} else {
			m.grant, m.hasGrant = m.host.GrantInfo(), true
			m.notice = "session grant updated"
		}
		return m, nil
	case stopResultMsg:
		if msg.err != nil {
			m.notice = "stop failed: " + msg.err.Error()
			m.quitPending = false // never exit silently over a failed settle
		}
		return m, nil
	case noticeMsg:
		m.notice = msg.err.Error()
		return m, nil
	case draftMsg:
		m.input.InsertString(string(msg))
		m.syncInputSize()
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		if m.overlay.kind == overlayNone {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(tea.Msg(msg))
			m.syncInputSize()
			return m, cmd
		}
		return m, nil
	}
	return m, nil
}

// applyEvent folds a controller state snapshot: cached copy of the event,
// overlay lifecycle rules, document refresh, and the deferred-quit path
// that only fires once the stop settled cleanly.
func (m model) applyEvent(ev app.UIEvent) (tea.Model, tea.Cmd) {
	m.ev = ev
	m.hasEv = true
	m.grant, m.hasGrant = m.host.GrantInfo(), true

	// An approval prompt belongs to a live run; when the run ends the
	// request is already invalidated engine-side, so the pane closes.
	if m.overlay.kind == overlayApproval && ev.State != app.RunRunning {
		m.overlay.kind = overlayNone
	}
	if ev.State == app.RunIdle && ev.Open {
		switch ev.Phase {
		case app.PhaseIncomplete:
			// The old task died recoverably: the user must choose continue
			// or abandon before any new input is accepted.
			if !m.decisionShown && m.overlay.kind == overlayNone {
				m.overlay = newDecisionOverlay(false, m.width, m.bodyHeight())
				m.decisionShown = true
			}
		case app.PhaseBlocked:
			if m.overlay.kind == overlayNone {
				m.overlay = newDecisionOverlay(true, m.width, m.bodyHeight())
			}
		default:
			m.decisionShown = false
		}
	} else {
		m.decisionShown = false
	}

	m.refreshDoc()

	if m.quitPending && ev.State == app.RunIdle {
		if ev.SettleErr != nil || ev.Phase == app.PhaseBlocked {
			// Settlement failed: exiting now would bury the pending settle;
			// stay up so the blocked state is visible and retryable.
			m.quitPending = false
			m.notice = "stopped, but settlement is unfinished — /retry or force quit"
			return m, nil
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if m.overlay.kind != overlayNone {
		return m.overlayKey(k, msg)
	}
	switch k {
	case "enter":
		m.submitInput()
		return m, nil
	case "esc":
		if m.quitPending && m.running() {
			return m, tea.Quit // second Esc during stop is a force quit
		}
		if m.running() {
			m.stopRun(false)
			return m, nil
		}
		return m, tea.Quit
	case "ctrl+c":
		if m.quitPending && m.running() {
			return m, tea.Quit
		}
		if m.running() {
			m.stopRun(true)
			return m, nil
		}
		return m, tea.Quit
	case "ctrl+r":
		m.expanded = !m.expanded
		m.refreshDoc()
		return m, nil
	case "ctrl+g":
		m.openGrant()
		return m, nil
	case "?":
		if strings.TrimSpace(m.input.Value()) == "" {
			m.overlay = newHelpOverlay(m.width, m.bodyHeight())
			return m, nil
		}
	case "pgup":
		m.scroll.PageUp()
		return m, nil
	case "pgdown":
		m.scroll.PageDown()
		return m, nil
	case "ctrl+u":
		m.scroll.HalfPageUp()
		return m, nil
	case "ctrl+d":
		m.scroll.HalfPageDown()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.syncInputSize()
	return m, cmd
}

// overlayKey routes modal input. Every pane swallows its keys; nothing
// leaks into the chat input while a pane is open.
func (m model) overlayKey(k string, msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.overlay.kind {
	case overlayApproval:
		req := *m.overlay.approval
		switch k {
		case "y", "Y", "enter":
			m.host.Respond(req.Token, req.ID, true)
			m.overlay.kind = overlayNone
		case "n", "N", "esc":
			// Denying is the fail-safe answer: the run sees a soft refusal,
			// never a hanging request.
			m.host.Respond(req.Token, req.ID, false)
			m.overlay.kind = overlayNone
		case "pgup":
			m.overlay.scroll.PageUp()
		case "pgdown":
			m.overlay.scroll.PageDown()
		case "up":
			m.overlay.scroll.ScrollUp(1)
		case "down":
			m.overlay.scroll.ScrollDown(1)
		}
		return m, nil
	case overlayGrant:
		switch k {
		case "esc":
			m.overlay.kind = overlayNone
			return m, nil
		case "enter":
			path := strings.TrimSpace(m.overlay.pathInput.Value())
			if path != "" && !slices.Contains(m.overlay.grantPaths, path) {
				m.overlay.grantPaths = append(m.overlay.grantPaths, path)
			}
			m.overlay.pathInput.Reset()
			return m, nil
		case "up":
			if m.overlay.grantSel > 0 {
				m.overlay.grantSel--
			}
			return m, nil
		case "down":
			if m.overlay.grantSel < len(m.overlay.grantPaths)-1 {
				m.overlay.grantSel++
			}
			return m, nil
		case "ctrl+x":
			if m.overlay.grantSel < len(m.overlay.grantPaths) {
				m.overlay.grantPaths = slices.Delete(
					m.overlay.grantPaths, m.overlay.grantSel, m.overlay.grantSel+1)
				if m.overlay.grantSel >= len(m.overlay.grantPaths) && m.overlay.grantSel > 0 {
					m.overlay.grantSel--
				}
			}
			return m, nil
		case "ctrl+a":
			paths := append([]string(nil), m.overlay.grantPaths...)
			host := m.host
			m.exec(func(ctx context.Context, send func(tea.Msg)) {
				err := host.SetGrant(paths)
				send(tea.Msg(grantAppliedMsg{err: err}))
			})
			m.overlay.kind = overlayNone
			return m, nil
		case "ctrl+r":
			m.host.RevokeGrant()
			m.overlay.kind = overlayNone
			return m, nil
		}
		var cmd tea.Cmd
		m.overlay.pathInput, cmd = m.overlay.pathInput.Update(msg)
		return m, cmd
	case overlaySessions:
		switch k {
		case "esc":
			m.overlay.kind = overlayNone
		case "j", "down":
			if m.overlay.sel < len(m.overlay.sessions)-1 {
				m.overlay.sel++
			}
		case "k", "up":
			if m.overlay.sel > 0 {
				m.overlay.sel--
			}
		case "enter":
			if len(m.overlay.sessions) == 0 {
				return m, nil
			}
			id := m.overlay.sessions[m.overlay.sel].ID
			host := m.host
			m.exec(func(ctx context.Context, send func(tea.Msg)) {
				if err := host.OpenSession(ctx, id); err != nil {
					send(tea.Msg(noticeMsg{err: err}))
				}
			})
			m.overlay.kind = overlayNone
		case "n":
			host := m.host
			m.exec(func(ctx context.Context, send func(tea.Msg)) {
				if _, err := host.NewSession(ctx); err != nil {
					send(tea.Msg(noticeMsg{err: err}))
				}
			})
			m.overlay.kind = overlayNone
		}
		return m, nil
	case overlayChanges:
		switch k {
		case "esc":
			m.overlay.kind = overlayNone
		case "j", "down":
			if m.overlay.sel < len(m.overlay.changes)-1 {
				m.overlay.sel++
			}
		case "k", "up":
			if m.overlay.sel > 0 {
				m.overlay.sel--
			}
		case "enter":
			if len(m.overlay.changes) == 0 {
				return m, nil
			}
			m.overlay = newRestoreOverlay(m.overlay.changes, m.overlay.sel, m.width, m.bodyHeight())
			return m, nil
		}
		return m, nil
	case overlayRestore:
		switch k {
		case "esc": // back to the change list, records kept
			m.overlay = newChangesOverlay(m.overlay.changes, m.width, m.bodyHeight())
			m.overlay.sel = min(m.overlay.sel, max(0, len(m.overlay.changes)-1))
		case "enter":
			rec := m.overlay.changes[m.overlay.sel]
			id := rec.ID
			host := m.host
			m.exec(func(ctx context.Context, send func(tea.Msg)) {
				record, err := host.Restore(ctx, id)
				send(tea.Msg(restoreDoneMsg{record: record, err: err}))
				if err == nil {
					if records, lerr := host.Changes(ctx); lerr == nil {
						send(tea.Msg(changesMsg{records: records}))
					}
				}
			})
			m.overlay.kind = overlayNone
		}
		return m, nil
	case overlayDecision:
		host := m.host
		close := func() { m.overlay.kind = overlayNone }
		if m.overlay.blocked {
			switch k {
			case "r", "enter":
				m.exec(func(ctx context.Context, send func(tea.Msg)) {
					if err := host.RetrySettlement(ctx); err != nil {
						send(tea.Msg(noticeMsg{err: err}))
					}
				})
				close()
			case "esc":
				close()
			}
			return m, nil
		}
		switch k {
		case "c", "enter":
			m.exec(func(ctx context.Context, send func(tea.Msg)) {
				if err := host.ContinueRun(ctx); err != nil {
					send(tea.Msg(noticeMsg{err: err}))
				}
			})
			close()
		case "a":
			m.exec(func(ctx context.Context, send func(tea.Msg)) {
				if err := host.AbandonRun(ctx); err != nil {
					send(tea.Msg(noticeMsg{err: err}))
				}
			})
			close()
		case "esc":
			close()
		}
		return m, nil
	case overlayHelp:
		if k == "esc" || k == "enter" || k == "?" {
			m.overlay.kind = overlayNone
		}
		return m, nil
	}
	return m, nil
}

// submitInput commits the draft. Busy runs keep the draft and explain;
// control commands stay reachable while a task runs (/stop must always
// work) and the controller rejects state-changing ones with an honest
// error; plain input creates the session on first use and submits,
// restoring the draft if the commit fails.
func (m *model) submitInput() {
	draft := m.input.Value()
	if strings.TrimSpace(draft) == "" {
		return
	}
	if strings.HasPrefix(draft, "/") {
		m.input.Reset()
		m.syncInputSize()
		m.runCommand(draft)
		return
	}
	if m.running() {
		m.notice = "a task is running — Esc stops it; your draft is kept"
		return
	}
	m.input.Reset()
	m.syncInputSize()
	host := m.host
	hasSession := m.hasEv && m.ev.Open
	m.exec(func(ctx context.Context, send func(tea.Msg)) {
		if !hasSession {
			if _, err := host.NewSession(ctx); err != nil {
				send(tea.Msg(noticeMsg{err: err}))
				send(tea.Msg(draftMsg(draft)))
				return
			}
		}
		if err := host.Submit(ctx, draft); err != nil {
			send(tea.Msg(noticeMsg{err: err}))
			send(tea.Msg(draftMsg(draft)))
		}
	})
}

func (m *model) runCommand(line string) {
	fields := strings.Fields(line)
	name := fields[0]
	host := m.host
	switch name {
	case "/new":
		m.exec(func(ctx context.Context, send func(tea.Msg)) {
			if _, err := host.NewSession(ctx); err != nil {
				send(tea.Msg(noticeMsg{err: err}))
			}
		})
	case "/sessions", "/list":
		m.exec(func(ctx context.Context, send func(tea.Msg)) {
			sessions, err := host.ListSessions()
			send(tea.Msg(sessionsMsg{sessions: sessions, err: err}))
		})
	case "/resume":
		if len(fields) < 2 {
			m.notice = "usage: /resume <session-id> (see /sessions)"
			return
		}
		id := fields[1]
		m.exec(func(ctx context.Context, send func(tea.Msg)) {
			if err := host.OpenSession(ctx, id); err != nil {
				send(tea.Msg(noticeMsg{err: err}))
			}
		})
	case "/stop":
		if !m.running() {
			m.notice = "no task is running"
			return
		}
		m.stopRun(false)
	case "/continue":
		m.exec(func(ctx context.Context, send func(tea.Msg)) {
			if err := host.ContinueRun(ctx); err != nil {
				send(tea.Msg(noticeMsg{err: err}))
			}
		})
	case "/abandon":
		m.exec(func(ctx context.Context, send func(tea.Msg)) {
			if err := host.AbandonRun(ctx); err != nil {
				send(tea.Msg(noticeMsg{err: err}))
			}
		})
	case "/retry":
		m.exec(func(ctx context.Context, send func(tea.Msg)) {
			if err := host.RetrySettlement(ctx); err != nil {
				send(tea.Msg(noticeMsg{err: err}))
			}
		})
	case "/changes":
		m.exec(func(ctx context.Context, send func(tea.Msg)) {
			records, err := host.Changes(ctx)
			send(tea.Msg(changesMsg{records: records, err: err}))
		})
	case "/restore":
		if len(fields) < 2 {
			m.notice = "usage: /restore <change-id> (see /changes)"
			return
		}
		id := fields[1]
		m.exec(func(ctx context.Context, send func(tea.Msg)) {
			record, err := host.Restore(ctx, id)
			send(tea.Msg(restoreDoneMsg{record: record, err: err}))
			if err == nil {
				if records, lerr := host.Changes(ctx); lerr == nil {
					send(tea.Msg(changesMsg{records: records}))
				}
			}
		})
	case "/grant":
		m.openGrant()
	case "/help":
		m.overlay = newHelpOverlay(m.width, m.bodyHeight())
	default:
		m.notice = fmt.Sprintf("unknown command %q — /help lists commands", name)
	}
}

// stopRun sends the user stop path. quitAfter marks a deferred exit that
// fires only when a later event confirms the run ended cleanly.
func (m *model) stopRun(quitAfter bool) {
	if quitAfter {
		m.quitPending = true
	}
	host := m.host
	m.exec(func(ctx context.Context, send func(tea.Msg)) {
		err := host.StopRun(ctx)
		send(tea.Msg(stopResultMsg{err: err}))
	})
}

func (m *model) openGrant() {
	info := m.host.GrantInfo()
	m.grant, m.hasGrant = info, true
	paths := append([]string(nil), info.Paths...)
	path := textinput.New()
	path.Placeholder = "workspace-relative file path — enter adds it"
	path.Prompt = "+ "
	path.SetWidth(max(24, m.width-8))
	_ = path.Focus()
	m.overlay = overlay{kind: overlayGrant, grantPaths: paths, pathInput: path}
}

func (m *model) refreshDoc() {
	if !m.hasEv {
		return // keep the startup banner until the first controller event
	}
	follow := m.scroll.AtBottom()
	m.scroll.SetContent(renderDoc(m.ev.View, renderOpts{Expanded: m.expanded}).flatten())
	if follow {
		m.scroll.GotoBottom()
	}
}

func (m *model) resize(w, h int) {
	m.width, m.height = max(1, w), max(1, h)
	m.scroll.SetWidth(m.width)
	m.scroll.SetHeight(max(1, m.bodyHeight()))
	m.input.SetWidth(max(12, m.width-2))
	m.syncInputSize()
	if m.overlay.kind != overlayNone {
		m.overlay.scroll.SetWidth(max(10, m.width-4))
		m.overlay.scroll.SetHeight(max(3, m.bodyHeight()-2))
	}
}

// bodyHeight is the scroll/overlay area: total minus header, status,
// bordered input and footer.
func (m model) bodyHeight() int {
	return max(1, m.height-4-(m.input.Height()+2))
}

func (m *model) syncInputSize() {
	want := min(max(m.input.LineCount()+1, 2), 8)
	if m.input.Height() != want {
		m.input.SetHeight(want)
		m.scroll.SetHeight(max(1, m.bodyHeight()))
	}
}

func (m model) running() bool {
	return m.hasEv && m.ev.State == app.RunRunning
}

func (m model) View() tea.View {
	body := m.scroll.View()
	if m.overlay.kind != overlayNone {
		body = m.overlayBox(m.overlay)
	}
	view := tea.NewView(strings.Join([]string{
		m.headerView(), body, m.statusView(), m.inputView(), m.footerView(),
	}, "\n"))
	view.AltScreen = true
	return view
}

func (m model) headerView() string {
	title := "go-agent"
	if m.hasEv && m.ev.Open {
		title += fmt.Sprintf(" · %s · %s", shortID(m.ev.Session.ID), m.ev.Phase)
	}
	if !m.state.ApprovalRequired {
		title += "  [APPROVAL BYPASSED —no-approval]"
	}
	return lipgloss.NewStyle().Bold(true).Width(m.width).Render(title)
}

func (m model) statusView() string {
	parts := []string{}
	switch {
	case !m.hasEv:
		parts = append(parts, "starting")
	case m.ev.State == app.RunRunning:
		parts = append(parts, "running")
	case m.ev.State == app.RunStopping:
		parts = append(parts, "stopping (cancel → join → settle)")
	default:
		parts = append(parts, "idle")
	}
	if m.hasEv && !m.ev.Open {
		parts = append(parts, "no session — your first input creates one")
	}
	if m.hasGrant && m.grant.Active {
		state := "expired"
		if m.grant.Usable {
			state = fmt.Sprintf("%d edits left, until %s", m.grant.Remaining, m.grant.Expires.Format("15:04"))
		}
		parts = append(parts, fmt.Sprintf("grant: %d file(s), %s", len(m.grant.Paths), state))
	}
	if m.hasEv && m.ev.SettleErr != nil {
		parts = append(parts, "settlement pending: /retry")
	}
	line := strings.Join(parts, " · ")
	if m.notice != "" {
		line += "\n  " + truncateRunes(m.notice, max(20, m.width-4))
	}
	return lipgloss.NewStyle().Width(m.width).Render(line)
}

func (m model) inputView() string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Width(max(10, m.width-2)).
		Render(m.input.View())
}

func (m model) footerView() string {
	return lipgloss.NewStyle().Faint(true).Width(m.width).Render(
		"enter send · ctrl+j newline · esc stop/quit · ctrl+g grant · ctrl+r reasoning · " +
			"/sessions /changes /help · pgup/pgdn scroll")
}

func (m model) overlayBox(o overlay) string {
	content := overlayContent(o)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Width(max(10, m.width-2)).
		Render(content)
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
