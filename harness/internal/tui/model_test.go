package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dailz1/go-agent/harness/internal/app"
	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/harness/internal/snapshot"
	"github.com/dailz1/go-agent/harness/internal/workspace"
)

// ---- test double: a recording Host ---------------------------------------

type respondCall struct {
	token string
	id    uint64
	allow bool
}

// recordingHost records every Host interaction. Model tests are
// single-goroutine (intents run synchronously through the rig), so no
// locking is needed.
type recordingHost struct {
	responds  []respondCall
	news      int
	submits   []string
	opens     []string
	continues int
	abandons  int
	retries   int
	stops     int
	lists     int
	grantSets [][]string
	revoked   bool
	restores  []string

	submitErr error
	newErr    error
	listErr   error
	stopErr   error
	changes   []snapshot.Record
	restoreBy map[string]snapshot.Record
	grant     app.GrantInfo

	requests chan app.ApprovalRequest
	observe  func(app.UIEvent)
	idle     chan struct{}
}

func (h *recordingHost) Observe(fn func(app.UIEvent))                 { h.observe = fn }
func (h *recordingHost) ApprovalRequests() <-chan app.ApprovalRequest { return h.requests }
func (h *recordingHost) Idle() <-chan struct{}                        { return h.idle }

func (h *recordingHost) NewSession(context.Context) (session.Meta, error) {
	h.news++
	return session.Meta{ID: "s-new"}, h.newErr
}

func (h *recordingHost) ListSessions() ([]session.Meta, error) {
	h.lists++
	return []session.Meta{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}}, h.listErr
}

func (h *recordingHost) OpenSession(_ context.Context, id string) error {
	h.opens = append(h.opens, id)
	return nil
}

func (h *recordingHost) Submit(_ context.Context, input string) error {
	h.submits = append(h.submits, input)
	return h.submitErr
}

func (h *recordingHost) ContinueRun(context.Context) error     { h.continues++; return nil }
func (h *recordingHost) AbandonRun(context.Context) error      { h.abandons++; return nil }
func (h *recordingHost) RetrySettlement(context.Context) error { h.retries++; return nil }
func (h *recordingHost) StopRun(context.Context) error         { h.stops++; return h.stopErr }
func (h *recordingHost) Close(context.Context) error           { return nil }

func (h *recordingHost) Changes(context.Context) ([]snapshot.Record, error) {
	return h.changes, nil
}

func (h *recordingHost) Restore(_ context.Context, changeID string) (snapshot.Record, error) {
	h.restores = append(h.restores, changeID)
	rec, ok := h.restoreBy[changeID]
	if !ok {
		return snapshot.Record{}, errors.New("no such change")
	}
	return rec, nil
}

func (h *recordingHost) SetGrant(paths []string) error {
	h.grantSets = append(h.grantSets, paths)
	return nil
}

func (h *recordingHost) Respond(token string, id uint64, allow bool) bool {
	h.responds = append(h.responds, respondCall{token: token, id: id, allow: allow})
	return true
}

func (h *recordingHost) GrantInfo() app.GrantInfo { return h.grant }
func (h *recordingHost) RevokeGrant()             { h.revoked = true }

// ---- test rig --------------------------------------------------------------

// testRig collects the intents the model schedules and runs them
// synchronously on the test goroutine, feeding any messages they send back
// through Update — the same shape as the bridge, minus concurrency.
type testRig struct {
	host    *recordingHost
	intents []func(context.Context, func(tea.Msg))
	sent    []tea.Msg
}

func newTestModel(state app.State) (model, *testRig) {
	rig := &testRig{host: &recordingHost{
		requests: make(chan app.ApprovalRequest, 8),
		idle:     make(chan struct{}),
	}}
	m := newModel(state, rig.host, rig.collect)
	return m, rig
}

func (r *testRig) collect(fn func(context.Context, func(tea.Msg))) {
	r.intents = append(r.intents, fn)
}

func (r *testRig) pending() int { return len(r.intents) }

// run executes all pending intents, then folds the messages they produced
// back through Update. One nesting level is enough for these flows.
func (r *testRig) run(m model) model {
	fns := r.intents
	r.intents = nil
	for _, fn := range fns {
		fn(context.Background(), func(msg tea.Msg) { r.sent = append(r.sent, msg) })
	}
	msgs := r.sent
	r.sent = nil
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		if mm, ok := next.(model); ok {
			m = mm
		}
	}
	return m
}

// ---- helpers ---------------------------------------------------------------

var (
	keyEnter = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyEsc   = tea.KeyPressMsg{Code: tea.KeyEscape}
	keyCtrlC = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	keyCtrlG = tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	keyCtrlR = tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}
	keyCtrlA = tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}
	keyDown  = tea.KeyPressMsg{Code: tea.KeyDown}
)

func runeKey(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func update[T tea.Msg](t testing.TB, m model, msg T) (model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	mm, ok := next.(model)
	if !ok {
		t.Fatalf("update returned %T, want model", next)
	}
	return mm, cmd
}

// isQuit reports whether a cmd is the quit command. Components like the
// textarea return their own cmds (cursor blink), so a non-nil cmd is not
// a quit; only QuitMsg is.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func event(state app.RunState, phase app.SessionPhase, open bool) uiEventMsg {
	ev := app.UIEvent{
		State: state, Phase: phase, Open: open,
		Session: session.Meta{ID: "th1"},
	}
	if open {
		ev.View = app.ViewModel{Source: app.SourceLive, ThreadID: "th1"}
	}
	return uiEventMsg(ev)
}

// ---- quit and input ---------------------------------------------------------

func TestQuitWhenIdle(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{keyEsc, keyCtrlC} {
		t.Run(key.String(), func(t *testing.T) {
			m, _ := newTestModel(app.State{ApprovalRequired: true})
			_, cmd := update(t, m, key)
			if !isQuit(cmd) {
				t.Fatalf("%s while idle must quit", key.String())
			}
		})
	}
}

// "q" is an ordinary character now: it lands in the input, never quits.
func TestQTypesIntoInput(t *testing.T) {
	m, _ := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, runeKey("q"))
	m, cmd := update(t, m, runeKey("q"))
	if isQuit(cmd) {
		t.Fatal("plain q must not quit")
	}
	if m.input.Value() != "qq" {
		t.Fatalf("input = %q, want qq", m.input.Value())
	}
}

// The first input creates the session before submitting; a failed submit
// restores the draft.
func TestSubmitCreatesSessionThenSubmits(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m.input.InsertString("fix the parser test")
	m, _ = update(t, m, keyEnter)
	if m.input.Value() != "" {
		t.Fatal("submitted draft must leave the input")
	}
	if rig.pending() == 0 {
		t.Fatal("enter must schedule a submit intent")
	}
	m = rig.run(m)
	if rig.host.news != 1 {
		t.Fatalf("new sessions = %d, want 1", rig.host.news)
	}
	if len(rig.host.submits) != 1 || rig.host.submits[0] != "fix the parser test" {
		t.Fatalf("submits = %v", rig.host.submits)
	}
}

func TestSubmitWithOpenSessionSkipsCreate(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunIdle, app.PhaseFresh, true))
	m.input.InsertString("next task")
	m, _ = update(t, m, keyEnter)
	m = rig.run(m)
	if rig.host.news != 0 {
		t.Fatalf("open session must not be recreated (news=%d)", rig.host.news)
	}
	if len(rig.host.submits) != 1 {
		t.Fatalf("submits = %v", rig.host.submits)
	}
}

func TestSubmitFailureRestoresDraft(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	rig.host.submitErr = errors.New("store down")
	m, _ = update(t, m, event(app.RunIdle, app.PhaseFresh, true))
	m.input.InsertString("draft text")
	m, _ = update(t, m, keyEnter)
	m = rig.run(m)
	if len(rig.host.submits) != 1 {
		t.Fatal("submit must have been attempted")
	}
	if m.input.Value() != "draft text" {
		t.Fatalf("failed submit must restore the draft, got %q", m.input.Value())
	}
	if m.notice == "" {
		t.Fatal("failed submit must surface a notice")
	}
}

// Busy runs never queue a second task: Enter keeps the draft and explains.
func TestBusyEnterKeepsDraft(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunRunning, app.PhaseFresh, true))
	m.input.InsertString("only tests")
	m, _ = update(t, m, keyEnter)
	if rig.pending() != 0 {
		t.Fatal("busy enter must not schedule a submit intent")
	}
	if m.input.Value() != "only tests" {
		t.Fatal("draft must be kept while a task runs")
	}
	if m.notice == "" {
		t.Fatal("busy enter must explain instead of queueing")
	}
}

// Multi-line paste (including CJK) lands in the input verbatim.
func TestPasteIntoInput(t *testing.T) {
	m, _ := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, tea.PasteMsg{Content: "第一行\n第二行"})
	got := m.input.Value()
	if !strings.Contains(got, "第一行") || !strings.Contains(got, "\n") || !strings.Contains(got, "第二行") {
		t.Fatalf("paste = %q, want multi-line CJK content preserved", got)
	}
}

// ---- slash command routing ---------------------------------------------------

func TestSlashCommands(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		running bool
		verify  func(t *testing.T, m model, rig *testRig)
	}{
		{
			name: "/new",
			line: "/new",
			verify: func(t *testing.T, m model, rig *testRig) {
				if rig.host.news != 1 {
					t.Fatalf("news = %d, want 1", rig.host.news)
				}
			},
		},
		{
			name: "/sessions lists and opens overlay",
			line: "/sessions",
			verify: func(t *testing.T, m model, rig *testRig) {
				if rig.host.lists != 1 {
					t.Fatalf("lists = %d, want 1", rig.host.lists)
				}
				if m.overlay.kind != overlaySessions {
					t.Fatalf("overlay = %v, want sessions", m.overlay.kind)
				}
				if len(m.overlay.sessions) != 3 {
					t.Fatalf("sessions listed = %d, want 3", len(m.overlay.sessions))
				}
			},
		},
		{
			name: "/resume takes an id",
			line: "/resume s2",
			verify: func(t *testing.T, m model, rig *testRig) {
				if len(rig.host.opens) != 1 || rig.host.opens[0] != "s2" {
					t.Fatalf("opens = %v", rig.host.opens)
				}
			},
		},
		{
			name: "/resume without id explains",
			line: "/resume",
			verify: func(t *testing.T, m model, rig *testRig) {
				if rig.pending() != 0 || len(rig.host.opens) != 0 {
					t.Fatal("/resume without id must not open anything")
				}
				if m.notice == "" {
					t.Fatal("/resume without id must explain usage")
				}
			},
		},
		{
			name:    "/stop while running stops",
			line:    "/stop",
			running: true,
			verify: func(t *testing.T, m model, rig *testRig) {
				if rig.host.stops != 1 {
					t.Fatalf("stops = %d, want 1", rig.host.stops)
				}
			},
		},
		{
			name: "/stop while idle refuses",
			line: "/stop",
			verify: func(t *testing.T, m model, rig *testRig) {
				if rig.pending() != 0 {
					t.Fatal("/stop while idle must not schedule a stop")
				}
				if m.notice == "" {
					t.Fatal("/stop while idle must explain")
				}
			},
		},
		{
			name: "/changes opens the change list",
			line: "/changes",
			verify: func(t *testing.T, m model, rig *testRig) {
				if m.overlay.kind != overlayChanges {
					t.Fatalf("overlay = %v, want changes", m.overlay.kind)
				}
			},
		},
		{
			name: "/grant opens the grant panel",
			line: "/grant",
			verify: func(t *testing.T, m model, rig *testRig) {
				if m.overlay.kind != overlayGrant {
					t.Fatalf("overlay = %v, want grant", m.overlay.kind)
				}
			},
		},
		{
			name: "/help opens help",
			line: "/help",
			verify: func(t *testing.T, m model, rig *testRig) {
				if m.overlay.kind != overlayHelp {
					t.Fatalf("overlay = %v, want help", m.overlay.kind)
				}
			},
		},
		{
			name: "unknown command explains",
			line: "/wat",
			verify: func(t *testing.T, m model, rig *testRig) {
				if rig.pending() != 0 {
					t.Fatal("unknown command must not schedule intents")
				}
				if m.notice == "" {
					t.Fatal("unknown command must explain")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, rig := newTestModel(app.State{ApprovalRequired: true})
			if tt.running {
				m, _ = update(t, m, event(app.RunRunning, app.PhaseFresh, true))
			} else {
				m, _ = update(t, m, event(app.RunIdle, app.PhaseFresh, true))
			}
			m.input.InsertString(tt.line)
			m, _ = update(t, m, keyEnter)
			m = rig.run(m)
			tt.verify(t, m, rig)
		})
	}
}

// ---- stop, settle, redirect ---------------------------------------------------

func TestStopWhileRunningSendsStopIntentNoQuit(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunRunning, app.PhaseFresh, true))
	m, cmd := update(t, m, keyEsc)
	if isQuit(cmd) {
		t.Fatal("first esc while running must stop, not quit")
	}
	if rig.pending() != 1 {
		t.Fatalf("stop intents = %d, want 1", rig.pending())
	}
	m = rig.run(m)
	if rig.host.stops != 1 {
		t.Fatalf("stops = %d, want 1", rig.host.stops)
	}
}

// Esc → stop → settle → quit: the exit only fires after the run reached a
// clean idle state with no pending settlement.
func TestCtrlCStopsThenQuitsAfterCleanSettle(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunRunning, app.PhaseFresh, true))
	m, cmd := update(t, m, keyCtrlC)
	if isQuit(cmd) {
		t.Fatal("ctrl+c while running must stop, not quit immediately")
	}
	if !m.quitPending {
		t.Fatal("ctrl+c must arm the deferred quit")
	}
	m = rig.run(m) // StopRun succeeds; stopResultMsg folds back
	m, cmd = update(t, m, event(app.RunIdle, app.PhaseFresh, true))
	if !isQuit(cmd) {
		t.Fatal("clean settle must release the deferred quit")
	}
}

// A second interrupt during the stop window is an explicit force quit.
func TestSecondInterruptDuringStopForcesQuit(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunRunning, app.PhaseFresh, true))
	m, _ = update(t, m, keyCtrlC)
	m = rig.run(m)
	// still stopping: no idle event yet
	m, cmd := update(t, m, keyEsc)
	if !isQuit(cmd) {
		t.Fatal("esc during the stop window must force quit")
	}
}

// A failed settle must never exit silently: the pane stays up with the
// blocked decision, and /retry is offered.
func TestFailedSettleKeepsUIUp(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	rig.host.stopErr = errors.New("settle io")
	m, _ = update(t, m, event(app.RunRunning, app.PhaseFresh, true))
	m, _ = update(t, m, keyCtrlC)
	m = rig.run(m) // stopResultMsg carries the error
	if m.quitPending {
		t.Fatal("failed stop must disarm the deferred quit")
	}
	m, cmd := update(t, m, uiEventMsg(app.UIEvent{
		State: app.RunIdle, Phase: app.PhaseBlocked, Open: true,
		Session: session.Meta{ID: "th1"}, SettleErr: rig.host.stopErr,
	}))
	if isQuit(cmd) {
		t.Fatal("blocked settle must not quit")
	}
	if m.overlay.kind != overlayDecision || !m.overlay.blocked {
		t.Fatalf("overlay = %+v, want blocked decision pane", m.overlay)
	}
	m, _ = update(t, m, runeKey("r"))
	m = rig.run(m)
	if rig.host.retries != 1 {
		t.Fatalf("retries = %d, want 1", rig.host.retries)
	}
}

// ---- approval pane -------------------------------------------------------------

func shellRequest() app.ApprovalRequest {
	return app.ApprovalRequest{
		Token: "tok1", ID: 42, Tool: "shell", ThreadID: "th1",
		Command: "go test ./... -count=1", Cwd: "/work/proj", Timeout: 2 * time.Minute,
		Arguments: `{"command":"go test ./... -count=1"}`,
	}
}

func editRequest() app.ApprovalRequest {
	return app.ApprovalRequest{
		Token: "tok2", ID: 43, Tool: "edit", ThreadID: "th1",
		Change:    workspace.Change{Path: "internal/a.go", Before: "old line", After: "new line", Exists: true},
		Arguments: `{"path":"internal/a.go"}`,
	}
}

// The approval basis is complete: full command for shell, full diff for
// edit — the approver decides on real content, never a truncated surrogate.
func TestApprovalShowsFullBasis(t *testing.T) {
	m, _ := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, approvalMsg{req: shellRequest()})
	if m.overlay.kind != overlayApproval {
		t.Fatalf("overlay = %v, want approval", m.overlay.kind)
	}
	shell := approvalContent(*m.overlay.approval)
	for _, want := range []string{"go test ./... -count=1", "/work/proj"} {
		if !strings.Contains(shell, want) {
			t.Fatalf("shell approval missing %q:\n%s", want, shell)
		}
	}
	m, _ = update(t, m, approvalMsg{req: editRequest()})
	edit := approvalContent(*m.overlay.approval)
	for _, want := range []string{"internal/a.go", "- old line", "+ new line"} {
		if !strings.Contains(edit, want) {
			t.Fatalf("edit approval missing %q:\n%s", want, edit)
		}
	}
}

func TestApprovalRespondsExactlyOnce(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, approvalMsg{req: shellRequest()})

	// Unrelated keys are swallowed by the pane: no response, no typing.
	m, _ = update(t, m, runeKey("x"))
	if len(rig.host.responds) != 0 {
		t.Fatalf("responds = %v, want none yet", rig.host.responds)
	}
	if m.input.Value() != "" {
		t.Fatal("approval pane must swallow typing")
	}

	m, _ = update(t, m, runeKey("y"))
	if len(rig.host.responds) != 1 {
		t.Fatalf("responds = %v, want one", rig.host.responds)
	}
	if got := rig.host.responds[0]; got.token != "tok1" || got.id != 42 || !got.allow {
		t.Fatalf("approve call = %+v", got)
	}
	if m.overlay.kind != overlayNone {
		t.Fatal("pane must close after answering")
	}

	m, _ = update(t, m, approvalMsg{req: shellRequest()})
	m, _ = update(t, m, runeKey("n"))
	if got := rig.host.responds[1]; got.allow {
		t.Fatalf("n must deny, got %+v", got)
	}

	m, _ = update(t, m, approvalMsg{req: shellRequest()})
	m, _ = update(t, m, keyEsc)
	if got := rig.host.responds[2]; got.allow {
		t.Fatalf("esc must deny (fail-safe), got %+v", got)
	}
}

// When the run ends, its pending request is already invalidated
// engine-side; the pane closes without answering — a late keypress can
// never approve anything.
func TestApprovalClosesWhenRunEnds(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunRunning, app.PhaseFresh, true))
	m, _ = update(t, m, approvalMsg{req: shellRequest()})
	m, _ = update(t, m, event(app.RunIdle, app.PhaseFresh, true))
	if m.overlay.kind != overlayNone {
		t.Fatal("approval pane must close when the run ends")
	}
	if len(rig.host.responds) != 0 {
		t.Fatalf("UI must not answer an invalidated request: %v", rig.host.responds)
	}
}

// ---- decision pane (interrupted / blocked) --------------------------------------

func TestDecisionPaneOnIncomplete(t *testing.T) {
	m, _ := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunIdle, app.PhaseIncomplete, true))
	if m.overlay.kind != overlayDecision || m.overlay.blocked {
		t.Fatalf("overlay = %+v, want unblocked decision pane", m.overlay)
	}
	// After the user dismisses it, the pane does not nag on every
	// subsequent event of the same phase; the choice stays reachable via
	// /continue and /abandon.
	m, _ = update(t, m, keyEsc)
	if m.overlay.kind != overlayNone {
		t.Fatal("esc must dismiss the decision pane")
	}
	m, _ = update(t, m, event(app.RunIdle, app.PhaseIncomplete, true))
	if m.overlay.kind != overlayNone {
		t.Fatal("decision pane must not reopen while dismissed for the same phase")
	}
}

func TestDecisionContinueRoutesIntent(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunIdle, app.PhaseIncomplete, true))
	m, _ = update(t, m, runeKey("c"))
	if rig.pending() != 1 {
		t.Fatal("continue must schedule its intent")
	}
	m = rig.run(m)
	if rig.host.continues != 1 {
		t.Fatalf("continues = %d, want 1", rig.host.continues)
	}
	if m.overlay.kind != overlayNone {
		t.Fatal("decision pane must close after the choice")
	}
}

func TestDecisionAbandonRoutesIntent(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunIdle, app.PhaseIncomplete, true))
	m, _ = update(t, m, runeKey("a"))
	m = rig.run(m)
	if rig.host.abandons != 1 {
		t.Fatalf("abandons = %d, want 1", rig.host.abandons)
	}
}

// ---- grant panel ------------------------------------------------------------------

func TestGrantPanelAddApplyRevoke(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	rig.host.grant = app.GrantInfo{
		Active: true, Usable: true, Paths: []string{"b.go"},
		Remaining: 20, Expires: time.Now().Add(30 * time.Minute),
	}
	m, _ = update(t, m, keyCtrlG)
	if m.overlay.kind != overlayGrant {
		t.Fatalf("overlay = %v, want grant", m.overlay.kind)
	}
	if len(m.overlay.grantPaths) != 1 || m.overlay.grantPaths[0] != "b.go" {
		t.Fatalf("seeded paths = %v", m.overlay.grantPaths)
	}
	// enter adds a typed path; duplicate entry is ignored
	m.overlay.pathInput.SetValue("a.go")
	m, _ = update(t, m, keyEnter)
	m.overlay.pathInput.SetValue("a.go")
	m, _ = update(t, m, keyEnter)
	if len(m.overlay.grantPaths) != 2 || m.overlay.grantPaths[1] != "a.go" {
		t.Fatalf("paths after adds = %v", m.overlay.grantPaths)
	}
	// apply sends the exact set
	m, _ = update(t, m, keyCtrlA)
	if rig.pending() != 1 {
		t.Fatal("apply must schedule the grant intent")
	}
	m = rig.run(m)
	if len(rig.host.grantSets) != 1 {
		t.Fatalf("grant sets = %v", rig.host.grantSets)
	}
	if got := rig.host.grantSets[0]; len(got) != 2 || got[0] != "b.go" || got[1] != "a.go" {
		t.Fatalf("applied set = %v", got)
	}
	if m.overlay.kind != overlayNone {
		t.Fatal("grant panel must close after apply")
	}
	if !m.hasGrant || !m.grant.Active {
		t.Fatal("model must adopt the refreshed grant info")
	}
}

func TestGrantRevoke(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, keyCtrlG)
	m, _ = update(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	if !rig.host.revoked {
		t.Fatal("ctrl+r in the grant panel must revoke")
	}
	if m.overlay.kind != overlayNone {
		t.Fatal("grant panel must close after revoke")
	}
}

// ---- sessions and changes overlays -------------------------------------------------

func TestSessionsOverlayNavigateAndOpen(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, sessionsMsg{sessions: []session.Meta{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}}})
	if m.overlay.kind != overlaySessions || m.overlay.sel != 0 {
		t.Fatalf("overlay = %+v", m.overlay)
	}
	m, _ = update(t, m, keyDown)
	m, _ = update(t, m, runeKey("j"))
	if m.overlay.sel != 2 {
		t.Fatalf("sel = %d, want 2", m.overlay.sel)
	}
	m, _ = update(t, m, runeKey("k"))
	if m.overlay.sel != 1 {
		t.Fatalf("sel = %d, want 1", m.overlay.sel)
	}
	m, _ = update(t, m, keyEnter)
	m = rig.run(m)
	if len(rig.host.opens) != 1 || rig.host.opens[0] != "s2" {
		t.Fatalf("opens = %v, want [s2]", rig.host.opens)
	}
	if m.overlay.kind != overlayNone {
		t.Fatal("sessions pane must close after open")
	}
}

func TestSessionsOverlayNewSession(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, sessionsMsg{sessions: []session.Meta{{ID: "s1"}}})
	m, _ = update(t, m, runeKey("n"))
	if rig.pending() != 1 {
		t.Fatal("n must schedule a new-session intent")
	}
	rig.run(m)
	if rig.host.news != 1 {
		t.Fatalf("news = %d, want 1", rig.host.news)
	}
}

func changeRecords() []snapshot.Record {
	return []snapshot.Record{
		{ID: "chg-1", Order: 1, State: snapshot.Applied, Change: workspace.Change{Path: "a.go", Before: "old", After: "new"}},
		{ID: "chg-2", Order: 2, State: snapshot.Applied, Change: workspace.Change{Path: "b.go", Before: "x", After: "y"}},
	}
}

func TestChangesRestoreFlow(t *testing.T) {
	m, rig := newTestModel(app.State{ApprovalRequired: true})
	rig.host.changes = changeRecords()
	rig.host.restoreBy = map[string]snapshot.Record{
		"chg-1": rig.host.changes[0],
		"chg-2": rig.host.changes[1],
	}
	m, _ = update(t, m, changesMsg{records: rig.host.changes})
	if m.overlay.kind != overlayChanges {
		t.Fatalf("overlay = %v, want changes", m.overlay.kind)
	}
	// enter inspects the selected change
	m, _ = update(t, m, keyEnter)
	if m.overlay.kind != overlayRestore {
		t.Fatalf("overlay = %v, want restore confirmation", m.overlay.kind)
	}
	// esc returns to the list without restoring
	m, _ = update(t, m, keyEsc)
	if m.overlay.kind != overlayChanges {
		t.Fatalf("overlay = %v, want back on the list", m.overlay.kind)
	}
	if len(rig.host.restores) != 0 {
		t.Fatal("esc from the confirmation must not restore")
	}
	// confirm restores the selected change and refreshes the list
	m, _ = update(t, m, keyEnter)
	m, _ = update(t, m, keyEnter)
	m = rig.run(m)
	if len(rig.host.restores) != 1 || rig.host.restores[0] != "chg-1" {
		t.Fatalf("restores = %v, want [chg-1]", rig.host.restores)
	}
	if m.notice == "" {
		t.Fatal("restore result must surface a notice")
	}
}

// ---- reasoning toggle and event fold ------------------------------------------------

func TestReasoningToggle(t *testing.T) {
	m, _ := newTestModel(app.State{ApprovalRequired: true})
	m, _ = update(t, m, event(app.RunIdle, app.PhaseFresh, true))
	m, _ = update(t, m, keyCtrlR)
	if !m.expanded {
		t.Fatal("ctrl+r must expand reasoning")
	}
	m, _ = update(t, m, keyCtrlR)
	if m.expanded {
		t.Fatal("second ctrl+r must collapse reasoning")
	}
}

// The first controller event replaces the startup banner with the live
// document; the model keeps rendering after every fold.
func TestFirstEventReplacesStartupDoc(t *testing.T) {
	m, _ := newTestModel(app.State{ApprovalRequired: true, Startup: []string{"banner line"}})
	if m.hasEv {
		t.Fatal("model must start without an event")
	}
	m, _ = update(t, m, event(app.RunIdle, app.PhaseFresh, true))
	if !m.hasEv {
		t.Fatal("event must be folded")
	}
	_ = m.View() // smoke: renders without panic
}
