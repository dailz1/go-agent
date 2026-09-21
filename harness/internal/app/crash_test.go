package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/harness/internal/config"
	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/harness/internal/snapshot"
	"github.com/dailz1/go-agent/harness/internal/tools"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// crash simulates process death for a fixture whose worker already drained:
// resources are released without settlement and without further durable
// writes, exactly what a dead process leaves behind.
func crash(f *fixture) {
	f.t.Helper()
	f.rootCancel()
	if f.ctrl.State() != RunIdle {
		f.t.Fatal("crash fixture must be idle first; use waitIdle")
	}
	if err := f.ctrl.Close(context.Background()); err != nil {
		f.t.Fatalf("crash close: %v", err)
	}
	if err := f.startup.Close(); err != nil {
		f.t.Fatalf("crash startup close: %v", err)
	}
}

// reopen boots a fresh "process" on the crashed fixture's directories with
// its own provider, manager and controller.
func reopen(t *testing.T, f *fixture, opts fixtureOptions, replies ...stubReply) *fixture {
	t.Helper()
	return boot(t, f.startup.Workspace.Path(), f.dataDir, opts, replies...)
}

// boot is the shared constructor for fixtures over given directories.
func boot(t *testing.T, workspace, dataDir string, opts fixtureOptions, replies ...stubReply) *fixture {
	t.Helper()
	var prov *stubProvider
	if opts.provider == nil {
		prov = newStubProvider(replies...)
	}
	args := []string{"--workspace", workspace, "--model", "test", "--data-dir", dataDir}
	if opts.runTimeout != "" {
		args = append(args, "--run-timeout", opts.runTimeout)
	}
	args = append(args, opts.contextArg...)
	cfg, err := config.Parse(args, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.NoApproval = opts.noApproval
	startup, err := Prepare(cfg, func(string) string { return "test-key" })
	if err != nil {
		t.Fatal(err)
	}
	if opts.provider != nil {
		startup.Provider = opts.provider
	} else {
		startup.Provider = prov
	}
	mgr, err := session.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctrl := NewController(startup, mgr)
	views := make(chan ViewModel, 1024)
	ctrl.Observe(func(v ViewModel) {
		select {
		case views <- v:
		default:
		}
	})
	rootCtx, rootCancel := context.WithCancel(context.Background())
	g := &fixture{
		t: t, prov: prov, startup: startup, mgr: mgr, ctrl: ctrl,
		views: views, rootCtx: rootCtx, rootCancel: rootCancel, dataDir: dataDir,
	}
	t.Cleanup(func() {
		rootCancel()
		_ = ctrl.Close(context.Background())
		startup.Close()
	})
	return g
}

// Row 1: the process died after the session identity was persisted but
// before any model call. The restart lists it, opens it as fresh, and the
// first input starts normally.
func TestCrashBeforeFirstRun(t *testing.T) {
	f := newFixture(t)
	meta := f.newSession()
	crash(f)

	g := reopen(t, f, fixtureOptions{})
	list, err := g.ctrl.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range list {
		if m.ID == meta.ID && m.LastKnown == session.StatusCreated {
			found = true
		}
	}
	if !found {
		t.Fatalf("created session missing from list: %+v", list)
	}
	res, err := g.ctrl.Open(g.rootCtx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseFresh || res.View.Cache != CacheNone {
		t.Fatalf("open after pre-run crash = phase:%s cache:%v", res.Phase, res.View.Cache)
	}
	g.prov.addReply(finalText("first answer"))
	if err := g.ctrl.Submit(g.rootCtx, "first input"); err != nil {
		t.Fatal(err)
	}
	g.waitView(func(v ViewModel) bool { return v.Complete }, "first run after restart")
	g.waitIdle("first run")
	if g.prov.count() != 1 {
		t.Fatalf("provider calls = %d", g.prov.count())
	}
}

// Row 2: the process died mid-model-reply (no durable round). The restart
// shows the interrupted decision; explicit continue resumes WITHOUT a new
// input and completes non-streaming.
func TestCrashMidStreamContinue(t *testing.T) {
	f := newFixture(t, blockingReply())
	meta := f.newSession()
	if err := f.ctrl.Submit(f.rootCtx, "task one"); err != nil {
		t.Fatal(err)
	}
	<-f.prov.entered
	f.rootCancel()
	f.waitIdle("crash drain")
	crash(f)

	g := reopen(t, f, fixtureOptions{}, finalText("resumed answer"))
	res, err := g.ctrl.Open(g.rootCtx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseIncomplete {
		t.Fatalf("phase after mid-stream crash = %s", res.Phase)
	}
	if got := g.prov.count(); got != 0 {
		t.Fatalf("open made provider calls: %d", got)
	}
	if err := g.ctrl.Continue(g.rootCtx); err != nil {
		t.Fatal(err)
	}
	g.waitView(func(v ViewModel) bool { return v.Complete }, "resume completion")
	g.waitIdle("resume")
	if got := g.prov.count(); got != 1 {
		t.Fatalf("provider calls after resume = %d", got)
	}
	last := g.prov.last()
	if len(last.Messages) != 2 {
		t.Fatalf("resume messages = %d, want [system, user]", len(last.Messages))
	}
	assertUserMessage(t, last.Messages[1], "task one") // no new input appended
	if got := g.ctrl.Phase(); got != PhaseComplete {
		t.Fatalf("phase after resume = %s", got)
	}
	cache, ok, err := g.mgr.LoadCache(meta.ID)
	if err != nil || !ok || len(cache.History) == 0 {
		t.Fatalf("cache after resume: ok=%v err=%v", ok, err)
	}
}

// Row 3: cancellation and join happened, the process died BEFORE settling.
// The restart still sees the interrupted run; abandon settles it with a
// fresh recovery decision and the same thread accepts the redirect.
func TestCrashBetweenCancelAndSettleAbandonRedirect(t *testing.T) {
	f := newFixture(t, blockingReply())
	meta := f.newSession()
	if err := f.ctrl.Submit(f.rootCtx, "task one"); err != nil {
		t.Fatal(err)
	}
	<-f.prov.entered
	f.rootCancel() // cancel + worker drains; death precedes any settle
	f.waitIdle("cancel-join without settle")
	crash(f)

	g := reopen(t, f, fixtureOptions{}, finalText("redirect answer"))
	if _, err := g.ctrl.Open(g.rootCtx, meta.ID); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, g.ctrl, PhaseIncomplete)
	if err := g.ctrl.Abandon(g.rootCtx); err != nil {
		t.Fatal(err)
	}
	if got := g.ctrl.Phase(); got != PhaseSettled {
		t.Fatalf("phase after abandon = %s", got)
	}
	if got := g.prov.count(); got != 0 {
		t.Fatalf("abandon made a model call: %d", got)
	}
	if err := g.ctrl.Submit(g.rootCtx, "task two"); err != nil {
		t.Fatal(err)
	}
	g.waitView(func(v ViewModel) bool { return v.Complete }, "redirect")
	g.waitIdle("redirect")
	if got := g.prov.count(); got != 1 {
		t.Fatalf("provider calls after redirect = %d", got)
	}
	last := g.prov.last()
	if len(last.Messages) != 3 {
		t.Fatalf("redirect messages = %d, want [system, user, user]", len(last.Messages))
	}
	assertUserMessage(t, last.Messages[1], "task one")
	assertUserMessage(t, last.Messages[2], "task two")
}

// Row 4: the settle record is durable but the process died before it could
// acknowledge or update metadata. The restart derives the settled phase
// from the log and the redirect proceeds with zero old-model calls.
func TestCrashAfterSettleRecordWritten(t *testing.T) {
	f := newFixture(t, blockingReply())
	meta := f.newSession()
	if err := f.ctrl.Submit(f.rootCtx, "task one"); err != nil {
		t.Fatal(err)
	}
	<-f.prov.entered
	if err := f.ctrl.Stop(f.rootCtx); err != nil {
		t.Fatal(err)
	}
	f.waitIdle("settle")
	crash(f)

	g := reopen(t, f, fixtureOptions{}, finalText("next task answer"))
	res, err := g.ctrl.Open(g.rootCtx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseSettled {
		t.Fatalf("phase after settle-record crash = %s", res.Phase)
	}
	if got := g.prov.count(); got != 0 {
		t.Fatalf("open after settled crash made provider calls: %d", got)
	}
	if err := g.ctrl.Submit(g.rootCtx, "task two"); err != nil {
		t.Fatal(err)
	}
	g.waitView(func(v ViewModel) bool { return v.Complete }, "post-settle redirect")
	g.waitIdle("redirect")
	if got := g.prov.count(); got != 1 {
		t.Fatalf("provider calls = %d", g.prov.count())
	}
	assertUserMessage(t, g.prov.last().Messages[2], "task two")
}

// Row 5: the run durably completed but the view cache save never happened
// (crash between Done and the cache write). The restart shows the stale or
// missing cache honestly instead of pretending completeness of the view.
func TestCrashAfterDoneBeforeCacheSave(t *testing.T) {
	f := newFixture(t, finalText("answer one"))
	meta := f.newSession()
	if err := f.ctrl.Submit(f.rootCtx, "one"); err != nil {
		t.Fatal(err)
	}
	f.waitView(func(v ViewModel) bool { return v.Complete }, "first completion")
	f.waitIdle("first")
	cachePath := filepath.Join(f.dataDir, "sessions", meta.ID, "cache.json")
	first, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	f.prov.addReply(finalText("answer two"))
	if err := f.ctrl.Submit(f.rootCtx, "two"); err != nil {
		t.Fatal(err)
	}
	f.waitView(func(v ViewModel) bool { return v.Complete }, "second completion")
	f.waitIdle("second")
	crash(f)

	// Rewind the cache to the first snapshot: durable head is past it.
	if err := os.WriteFile(cachePath, first, 0o600); err != nil {
		t.Fatal(err)
	}
	g := reopen(t, f, fixtureOptions{})
	res, err := g.ctrl.Open(g.rootCtx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseComplete {
		t.Fatalf("phase = %s", res.Phase)
	}
	if res.View.Cache != CacheStale || res.View.Complete {
		t.Fatalf("stale cache view = cache:%v complete:%v", res.View.Cache, res.View.Complete)
	}
	if len(res.View.History) == 0 {
		t.Fatal("stale view discarded the known history")
	}

	// Variant: no cache at all.
	crash(g)
	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	g2 := reopen(t, f, fixtureOptions{})
	res2, err := g2.ctrl.Open(g2.rootCtx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res2.View.Cache != CacheNone || res2.View.History != nil {
		t.Fatalf("missing cache view = %+v", res2.View)
	}
	if res2.Phase != PhaseComplete {
		t.Fatalf("phase with no cache = %s", res2.Phase)
	}
}

// Row 6: the process died while a declared tool was still executing. The
// declaration stays open; resume closes it with outcome-unknown results and
// the model is told honestly what stayed unresolved.
func TestCrashDuringToolExecutionOpenDeclaration(t *testing.T) {
	f := newFixtureWithOptions(t, fixtureOptions{noApproval: true}, toolCall("c1", "shell",
		`{"command":"echo started > marker.txt && sleep 30","cwd":"."}`))
	meta := f.newSession()
	workspacePath := f.startup.Workspace.Path()
	if err := f.ctrl.Submit(f.rootCtx, "run it"); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(workspacePath, "marker.txt"))
	f.rootCancel() // death mid-execution: declaration open, no commit
	f.waitIdle("crash during tool")
	crash(f)

	g := reopen(t, f, fixtureOptions{}, finalText("handled unknown"))
	if _, err := g.ctrl.Open(g.rootCtx, meta.ID); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, g.ctrl, PhaseIncomplete)
	if err := g.ctrl.Continue(g.rootCtx); err != nil {
		t.Fatal(err)
	}
	g.waitView(func(v ViewModel) bool { return v.Complete }, "resume after unknown")
	g.waitIdle("resume")

	last := g.prov.last()
	if len(last.Messages) != 4 {
		t.Fatalf("resume messages = %d, want [system, user, assistant, tool]", len(last.Messages))
	}
	assertUserMessage(t, last.Messages[1], "run it")
	if last.Messages[2].Role != llm.RoleAssistant {
		t.Fatalf("declared message role = %s", last.Messages[2].Role)
	}
	unknown := last.Messages[3]
	if unknown.Role != llm.RoleTool || len(unknown.Content) != 1 {
		t.Fatalf("unknown result = role:%s blocks:%d", unknown.Role, len(unknown.Content))
	}
	if tr, ok := unknown.Content[0].(llm.ToolResultBlock); !ok || tr.ToolUseID != "c1" ||
		!tr.IsError || !strings.Contains(tr.Content, "outcome unknown") {
		t.Fatalf("unknown result block = %#v", unknown.Content[0])
	}
}

// TestControllerRedirectAfterCrashZeroOldModelCalls is the strict
// zero-old-model-call proof: after a mid-stream crash, opening and
// abandoning the session make NO provider call, and the redirect consumes
// exactly the one scripted exchange for the new input. agenttest's strict
// script would fail on any extra or mismatched call, and Verify fails on
// any unconsumed one.
func TestControllerRedirectAfterCrashZeroOldModelCalls(t *testing.T) {
	f := newFixture(t, blockingReply())
	meta := f.newSession()
	if err := f.ctrl.Submit(f.rootCtx, "task one"); err != nil {
		t.Fatal(err)
	}
	<-f.prov.entered
	f.rootCancel()
	f.waitIdle("crash drain")
	crash(f)

	// Boot process 2 on the same directories with the strict double.
	workspace := f.startup.Workspace.Path()
	base := boot(t, workspace, f.dataDir, fixtureOptions{})

	// Capture the exact registry the controller will assemble, by running
	// the same BeginRun once and releasing it.
	snap, err := snapshot.Open(base.mgr.SnapshotDir(meta.ID), base.startup.Workspace, meta.ID,
		int64(base.startup.Config.SnapshotQuotaMiB)<<20)
	if err != nil {
		t.Fatal(err)
	}
	outs, err := tools.OpenOutputStore(base.mgr.OutputDir(meta.ID))
	if err != nil {
		t.Fatal(err)
	}
	scratchRun, reg, err := base.startup.BeginRun(context.Background(), meta.ID, snap, outs)
	if err != nil {
		t.Fatal(err)
	}
	capturedTools := append([]tool.ToolInfo(nil), reg.List()...)
	scratchRun.Cancel()
	scratchRun.Finish()
	snap.Close()
	outs.Close()

	system := base.startup.Rules.Snapshot().System
	cfgOpts := llm.Options{Model: base.startup.Config.Model, MaxTokens: base.startup.Config.MaxOutputTokens}
	scripted := agenttest.NewScriptedProvider(agenttest.Exchange{
		Method: agenttest.MethodChatStream,
		Request: agenttest.Request{
			Messages: []llm.Message{
				llm.SystemMessage(system),
				llm.UserMessage("task one"),
				llm.UserMessage("task two"),
			},
			Tools:   capturedTools,
			Options: cfgOpts,
		},
		StreamChunks: []llm.Chunk{
			llm.TextDeltaChunk{Text: "redirected"},
			llm.DoneChunk{FinishReason: "stop"},
		},
	})
	base.startup.Provider = scripted
	// Rebuild the controller's settlement surface over the strict provider;
	// the run agents read it at build time.
	base.ctrl = NewController(base.startup, base.mgr)
	views := make(chan ViewModel, 1024)
	base.ctrl.Observe(func(v ViewModel) {
		select {
		case views <- v:
		default:
		}
	})
	base.views = views

	res, err := base.ctrl.Open(base.rootCtx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseIncomplete {
		t.Fatalf("phase after crash = %s", res.Phase)
	}
	if err := base.ctrl.Abandon(base.rootCtx); err != nil {
		t.Fatal(err)
	}
	if got := base.ctrl.Phase(); got != PhaseSettled {
		t.Fatalf("phase after abandon = %s", got)
	}
	if err := base.ctrl.Submit(base.rootCtx, "task two"); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !base.ctrl.View().Complete {
		select {
		case <-deadline.C:
			t.Fatal("redirect never completed against the strict script")
		case <-time.After(time.Millisecond):
		}
	}
	// The proof: exactly one exchange was scripted and it was consumed —
	// open, abandon and settle made no provider call at all.
	if err := scripted.Verify(); err != nil {
		t.Fatalf("strict script violated: %v", err)
	}
	sess, err := base.ctrl.Session()
	if err != nil || sess.ID != meta.ID {
		t.Fatalf("thread identity drifted: %+v err=%v", sess, err)
	}
}
