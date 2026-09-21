package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/dailz1/go-agent/harness/internal/app"
	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/harness/internal/snapshot"
)

func overlayViewport(width, height int) viewport.Model {
	vp := viewport.New(viewport.WithWidth(max(10, width-4)), viewport.WithHeight(max(3, height-2)))
	return vp
}

func newApprovalOverlay(req app.ApprovalRequest, width, height int) overlay {
	o := overlay{kind: overlayApproval, approval: &req, scroll: overlayViewport(width, height)}
	o.scroll.SetContent(approvalContent(req))
	o.scroll.GotoTop()
	return o
}

// approvalContent renders the full approval basis: complete path, diff or
// command, never a truncated surrogate. The overlay viewport scrolls; the
// approver always sees everything on demand.
func approvalContent(req app.ApprovalRequest) string {
	var b strings.Builder
	b.WriteString("APPROVAL REQUIRED — this exact call, nothing else\n\n")
	switch req.Tool {
	case "edit", "write":
		change := req.Change
		fmt.Fprintf(&b, "tool: %s\npath: %s\n", req.Tool, change.Path)
		if change.Exists {
			fmt.Fprintf(&b, "existing file, mode %04o — your uncommitted changes are snapshotted before the write\n", change.Mode)
		} else {
			b.WriteString("new file\n")
		}
		b.WriteString("\n--- change (− before / + after) ---\n")
		for _, line := range lineDiff(change.Before, change.After) {
			fmt.Fprintf(&b, "%c %s\n", line.Kind, line.Text)
		}
	case "shell":
		fmt.Fprintf(&b, "tool: shell\ncommand: %s\ncwd: %s\ntimeout: %s\n",
			req.Command, req.Cwd, req.Timeout)
		b.WriteString("\nshell runs with your user permissions — it is NOT sandboxed and never inherits a file grant\n")
	default:
		fmt.Fprintf(&b, "tool: %s\npath: %s\n", req.Tool, req.Path)
	}
	b.WriteString("\n--- raw arguments ---\n")
	b.WriteString(expandTabs(sanitize(req.Arguments)))
	b.WriteString("\n\ny approve · n / esc deny (soft refusal) · pgup/pgdn scroll")
	return b.String()
}

func newGrantOverlay(width, height int) overlay {
	return overlay{kind: overlayGrant, scroll: overlayViewport(width, height)}
}

func newSessionsOverlay(sessions []session.Meta, width, height int) overlay {
	o := overlay{kind: overlaySessions, sessions: sessions, scroll: overlayViewport(width, height)}
	return o
}

func newChangesOverlay(records []snapshot.Record, width, height int) overlay {
	o := overlay{kind: overlayChanges, changes: records, scroll: overlayViewport(width, height)}
	return o
}

func newRestoreOverlay(records []snapshot.Record, sel, width, height int) overlay {
	o := overlay{kind: overlayRestore, changes: records, sel: sel, scroll: overlayViewport(width, height)}
	o.scroll.SetContent(restoreContent(records, sel))
	o.scroll.GotoTop()
	return o
}

// restoreContent shows the exact restore a confirmation would perform:
// verify current bytes match the recorded after-image, then revert to the
// before-image. Conflicts refuse; there is no force-overwrite key.
func restoreContent(records []snapshot.Record, sel int) string {
	if sel < 0 || sel >= len(records) {
		return "no change selected"
	}
	rec := records[sel]
	var b strings.Builder
	fmt.Fprintf(&b, "RESTORE FILE — change %s (order %d, state %s)\n", shortID(rec.ID), rec.Order, rec.State)
	fmt.Fprintf(&b, "path: %s\n\n", rec.Change.Path)
	b.WriteString("restore reverts the tool's write (after → before). Current bytes must\n")
	b.WriteString("still match the recorded after-image; any later modification refuses.\n")
	b.WriteString("Shell-made changes are never covered by restore.\n\n")
	b.WriteString("--- restore diff (− current/after / + restored/before) ---\n")
	for _, line := range lineDiff(rec.Change.After, rec.Change.Before) {
		fmt.Fprintf(&b, "%c %s\n", line.Kind, line.Text)
	}
	b.WriteString("\nenter confirm restore · esc back to list")
	return b.String()
}

func newDecisionOverlay(blocked bool, width, height int) overlay {
	return overlay{kind: overlayDecision, blocked: blocked, scroll: overlayViewport(width, height)}
}

func newHelpOverlay(width, height int) overlay {
	o := overlay{kind: overlayHelp, scroll: overlayViewport(width, height)}
	o.scroll.SetContent(helpContent())
	o.scroll.GotoTop()
	return o
}

func helpContent() string {
	return strings.Join([]string{
		"KEYBINDINGS",
		"",
		"  enter            send input (creates the session on first use)",
		"  ctrl+j           insert newline (multi-line input, paste works)",
		"  esc              close pane · stop the running task · quit when idle",
		"  ctrl+c           stop the task, then quit after it settles; twice = force",
		"  ctrl+r           expand / collapse reasoning sections",
		"  ctrl+g           grant panel (exact file set, 20 edits / 30 min)",
		"  pgup / pgdown    page scrollback; ctrl+u / ctrl+d half-page",
		"  ?                this help (only when the input is empty)",
		"",
		"COMMANDS",
		"",
		"  /new                      start a fresh session",
		"  /sessions                 list persisted sessions (open = no model call)",
		"  /resume <id>              open a session read-only; interrupted tasks",
		"                            ask continue or abandon first",
		"  /stop                     stop: cancel → join → settle (original token)",
		"  /continue                 resume the interrupted task (non-streaming)",
		"  /abandon                  settle the old task, then accept new input",
		"  /retry                    retry a blocked settlement",
		"  /changes                  list restorable edit/write changes",
		"  /restore <change-id>      confirm-then-restore one change",
		"  /grant                    grant panel",
		"  /help                     this help",
		"",
		"APPROVAL PANE",
		"",
		"  y / enter approve · n / esc deny · approval covers this one call only;",
		"  shell commands always ask, grants never cover them.",
	}, "\n")
}

// overlayContent renders the pane body. Panes with their own input field
// (grant) compose live; the rest use their scroll viewport.
func overlayContent(o overlay) string {
	switch o.kind {
	case overlayGrant:
		return grantContent(o)
	case overlaySessions:
		return sessionsContent(o)
	case overlayChanges:
		return changesContent(o)
	case overlayDecision:
		return decisionContent(o)
	default:
		return o.scroll.View()
	}
}

func grantContent(o overlay) string {
	var b strings.Builder
	b.WriteString("SESSION GRANT — edit/write on an exact file set\n")
	b.WriteString("up to 20 edits or 30 minutes, whichever first · shell and sensitive\n")
	b.WriteString("files are never covered · cancel, redirect or restart revokes it\n\n")
	b.WriteString("paths:\n")
	if len(o.grantPaths) == 0 {
		b.WriteString("  (none — add paths below)\n")
	}
	for i, path := range o.grantPaths {
		marker := "  "
		if i == o.grantSel {
			marker = "> "
		}
		fmt.Fprintf(&b, "%s%s\n", marker, path)
	}
	b.WriteString("\n")
	b.WriteString(o.pathInput.View())
	b.WriteString("\n\nenter add path · ctrl+x remove selected · up/down select\n")
	b.WriteString("ctrl+a apply (requires a running task) · ctrl+r revoke · esc close")
	return b.String()
}

func sessionsContent(o overlay) string {
	var b strings.Builder
	b.WriteString("SESSIONS — opening is read-only; no model call, no tools\n\n")
	if len(o.sessions) == 0 {
		b.WriteString("no persisted sessions\n")
	}
	for i, meta := range o.sessions {
		marker := "  "
		if i == o.sel {
			marker = "> "
		}
		title := meta.Title
		if title == "" {
			title = "(created, never started)"
		}
		fmt.Fprintf(&b, "%s%s  %s  %s  %s  %s\n", marker, shortID(meta.ID),
			meta.UpdatedAt.Format("2006-01-02 15:04"), meta.LastKnown, meta.Provider, title)
	}
	b.WriteString("\nenter open · n new session · j/k move · esc close")
	return b.String()
}

func changesContent(o overlay) string {
	var b strings.Builder
	b.WriteString("CHANGES — restorable edit/write records of this session\n")
	b.WriteString("shell modifications are not restorable; newest first\n\n")
	if len(o.changes) == 0 {
		b.WriteString("no recorded changes\n")
	}
	for i := len(o.changes) - 1; i >= 0; i-- {
		rec := o.changes[i]
		marker := "  "
		if i == o.sel {
			marker = "> "
		}
		fmt.Fprintf(&b, "%s%s  %-6s  %s\n", marker, shortID(rec.ID), rec.State, rec.Change.Path)
	}
	b.WriteString("\nenter inspect + confirm restore · j/k move · esc close")
	return b.String()
}

func decisionContent(o overlay) string {
	if o.blocked {
		return strings.Join([]string{
			"SETTLEMENT PENDING",
			"",
			"The cancelled task could not be settled durably. New input stays",
			"blocked until the settlement is retried with the original token.",
			"",
			"r / enter  retry settlement",
			"esc       dismiss (input stays blocked)",
		}, "\n")
	}
	return strings.Join([]string{
		"INTERRUPTED TASK — DECIDE FIRST",
		"",
		"The old task was interrupted. It must be explicitly continued or",
		"abandoned before this session accepts new input.",
		"",
		"c / enter  continue old task (non-streaming resume, cancellable)",
		"a         abandon: settle the old task, then type the new direction",
		"esc       dismiss (input stays blocked)",
	}, "\n")
}

// unused import guards kept explicit for clarity.
var _ = tea.Msg(nil)
