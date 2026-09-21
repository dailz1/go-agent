package app

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/harness/internal/config"
	"github.com/dailz1/go-agent/harness/internal/session"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// stubReply is one scripted provider exchange for the recording stub.
type stubReply struct {
	chunks []llm.Chunk
	err    error
	block  chan struct{} // non-nil: the stream parks here until released or ctx ends
}

func finalText(text string) stubReply {
	return stubReply{chunks: []llm.Chunk{
		llm.TextDeltaChunk{Text: text},
		llm.DoneChunk{FinishReason: "stop"},
	}}
}

func toolCall(id, name, args string) stubReply {
	return stubReply{chunks: []llm.Chunk{
		llm.ToolCallStartChunk{Index: 0, ID: id, Name: name},
		llm.ToolCallArgsChunk{Index: 0, ID: id, Delta: args},
		llm.DoneChunk{FinishReason: "tool_calls"},
	}}
}

func blockingReply() stubReply { return stubReply{block: make(chan struct{})} }

// stubCall records one provider request.
type stubCall struct {
	Messages []llm.Message
	Tools    []tool.ToolInfo
	Options  llm.Options
}

// stubProvider is the thin ctx-honoring provider for cancel paths: it records
// requests, parks streams on gates, and never invents responses. Deterministic
// scripted runs additionally use agenttest.ScriptedProvider.
type stubProvider struct {
	mu      sync.Mutex
	replies []stubReply
	calls   []stubCall
	entered chan struct{}
}

func newStubProvider(replies ...stubReply) *stubProvider {
	return &stubProvider{replies: replies, entered: make(chan struct{}, 64)}
}

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}

func (p *stubProvider) ChatStream(ctx context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.mu.Lock()
	p.entered <- struct{}{}
	if len(p.replies) == 0 {
		p.mu.Unlock()
		return nil, fmt.Errorf("stub: no scripted reply (call %d)", len(p.calls)+1)
	}
	reply := p.replies[0]
	p.replies = p.replies[1:]
	p.calls = append(p.calls, stubCall{
		Messages: append([]llm.Message(nil), messages...),
		Tools:    append([]tool.ToolInfo(nil), tools...),
		Options:  llm.ApplyOptions(opts),
	})
	p.mu.Unlock()

	return func(yield func(llm.Chunk, error) bool) {
		if reply.block != nil {
			select {
			case <-reply.block:
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
		for _, chunk := range reply.chunks {
			if !yield(chunk, nil) {
				return
			}
		}
		if reply.err != nil {
			yield(nil, reply.err)
		}
	}, nil
}

func (p *stubProvider) addReply(reply stubReply) {
	p.mu.Lock()
	p.replies = append(p.replies, reply)
	p.mu.Unlock()
}

func (p *stubProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *stubProvider) last() stubCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[len(p.calls)-1]
}

func (p *stubProvider) callAt(i int) stubCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.calls) {
		return stubCall{}
	}
	return p.calls[i]
}

// fixture is one controller process over private temp directories.
type fixture struct {
	t          *testing.T
	prov       *stubProvider
	startup    *Startup
	mgr        *session.Manager
	ctrl       *Controller
	views      chan ViewModel
	rootCtx    context.Context
	rootCancel context.CancelFunc
	dataDir    string
}

type fixtureOptions struct {
	noApproval bool
	provider   llm.Provider // defaults to the stub
	runTimeout string
	contextArg []string
}

func newFixtureWithOptions(t *testing.T, opts fixtureOptions, replies ...stubReply) *fixture {
	t.Helper()
	prov := newStubProvider(replies...)
	if opts.provider != nil {
		prov = nil
	}
	args := []string{"--workspace", t.TempDir(), "--model", "test",
		"--data-dir", t.TempDir()}
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
	mgr, err := session.Open(startup.Config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctrl := NewController(startup, mgr)
	views := make(chan ViewModel, 1024)
	ctrl.Observe(func(e UIEvent) {
		select {
		case views <- e.View:
		default:
		}
	})
	rootCtx, rootCancel := context.WithCancel(context.Background())
	f := &fixture{
		t: t, prov: prov, startup: startup, mgr: mgr, ctrl: ctrl,
		views: views, rootCtx: rootCtx, rootCancel: rootCancel,
		dataDir: startup.Config.DataDir,
	}
	t.Cleanup(func() {
		rootCancel()
		_ = ctrl.Close(context.Background())
		startup.Close()
	})
	return f
}

func newFixture(t *testing.T, replies ...stubReply) *fixture {
	return newFixtureWithOptions(t, fixtureOptions{}, replies...)
}

// newSession submits nothing: only the durable identity exists.
func (f *fixture) newSession() session.Meta {
	f.t.Helper()
	meta, err := f.ctrl.NewSession(f.rootCtx)
	if err != nil {
		f.t.Fatal(err)
	}
	return meta
}

// waitView blocks until a view matching pred flows through the observer.
func (f *fixture) waitView(pred func(ViewModel) bool, what string) ViewModel {
	f.t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case v := <-f.views:
			if pred(v) {
				return v
			}
		case <-deadline.C:
			f.t.Fatalf("timed out waiting for view: %s", what)
		}
	}
}

func (f *fixture) waitIdle(what string) {
	f.t.Helper()
	select {
	case <-f.ctrl.Idle():
	case <-time.After(5 * time.Second):
		f.t.Fatalf("timed out waiting for idle: %s", what)
	}
}

func waitPhase(t *testing.T, c *Controller, want SessionPhase) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for c.Phase() != want {
		select {
		case <-deadline.C:
			t.Fatalf("phase = %s, want %s", c.Phase(), want)
		case <-time.After(time.Millisecond):
		}
	}
}

// waitForFile is a bounded barrier on filesystem state (real subprocesses).
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for file %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// flakyStore injects settlement persistence failures without changing read
// semantics.
type flakyStore struct {
	store.Store
	failAppend bool
}

func (s *flakyStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if s.failAppend {
		return 0, fmt.Errorf("flaky: append disabled")
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func TestControllerSubmitCompletesAndCaches(t *testing.T) {
	f := newFixture(t, finalText("all done"))
	meta := f.newSession()

	if err := f.ctrl.Submit(f.rootCtx, "fix the parser test"); err != nil {
		t.Fatal(err)
	}
	view := f.waitView(func(v ViewModel) bool { return v.Complete }, "completed view")
	f.waitIdle("completion")

	if view.Text != "all done" || view.Interrupted {
		t.Fatalf("view = text:%q interrupted:%v", view.Text, view.Interrupted)
	}
	if got := f.ctrl.Phase(); got != PhaseComplete {
		t.Fatalf("phase = %s", got)
	}
	if f.prov.count() != 1 {
		t.Fatalf("provider calls = %d", f.prov.count())
	}
	sess, err := f.ctrl.Session()
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title != "fix the parser test" || sess.ID != meta.ID {
		t.Fatalf("meta = %+v", sess)
	}
	if view.ThreadID != meta.ID {
		t.Fatalf("thread id drifted: %q vs %q", view.ThreadID, meta.ID)
	}

	// The display cache was durably saved with the authoritative history.
	cache, ok, err := f.mgr.LoadCache(meta.ID)
	if err != nil || !ok {
		t.Fatalf("cache: ok=%v err=%v", ok, err)
	}
	if cache.Head == 0 || len(cache.History) == 0 {
		t.Fatalf("cache = %+v", cache)
	}
}

func TestControllerStopSettleRedirectSameThread(t *testing.T) {
	f := newFixture(t, blockingReply())
	meta := f.newSession()

	if err := f.ctrl.Submit(f.rootCtx, "task one"); err != nil {
		t.Fatal(err)
	}
	<-f.prov.entered // the one old-task model call is parked mid-stream

	if err := f.ctrl.Stop(f.rootCtx); err != nil {
		t.Fatal(err)
	}
	if got := f.ctrl.Phase(); got != PhaseSettled {
		t.Fatalf("phase after stop = %s", got)
	}
	// Zero old-model calls: between settle and the new input nothing ran.
	if got := f.prov.count(); got != 1 {
		t.Fatalf("provider calls after settle = %d, want 1", got)
	}
	// The interrupted view kept its observations and gained the refreshed
	// authoritative history from the read-only cancelled snapshot.
	view := f.ctrl.View()
	if view.Complete {
		t.Fatal("cancelled run must not render as completed")
	}
	if len(view.History) == 0 || view.Head == 0 {
		t.Fatalf("cache refresh after settle missing: %+v", view)
	}

	// Same-thread redirect: only the new input reaches the model.
	f.prov.addReply(finalText("redirected answer"))
	if err := f.ctrl.Submit(f.rootCtx, "new direction"); err != nil {
		t.Fatal(err)
	}
	f.waitView(func(v ViewModel) bool { return v.Complete }, "redirect completed")
	f.waitIdle("redirect")

	if got := f.prov.count(); got != 2 {
		t.Fatalf("provider calls after redirect = %d", got)
	}
	last := f.prov.last()
	if len(last.Messages) < 3 {
		t.Fatalf("redirect history = %d messages", len(last.Messages))
	}
	assertUserMessage(t, last.Messages[len(last.Messages)-1], "new direction")
	assertUserMessage(t, last.Messages[len(last.Messages)-2], "task one")
	if last.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("first message role = %s", last.Messages[0].Role)
	}
	if last.Options.Model != "test" {
		t.Fatalf("model option = %q", last.Options.Model)
	}
	sess, _ := f.ctrl.Session()
	if sess.ID != meta.ID {
		t.Fatalf("thread id changed across redirect: %q vs %q", sess.ID, meta.ID)
	}
}

func assertUserMessage(t *testing.T, msg llm.Message, text string) {
	t.Helper()
	if msg.Role != llm.RoleUser || len(msg.Content) != 1 {
		t.Fatalf("message = role:%s blocks:%d, want user with one block", msg.Role, len(msg.Content))
	}
	if tb, ok := msg.Content[0].(llm.TextBlock); !ok || tb.Text != text {
		t.Fatalf("user text = %#v, want %q", msg.Content[0], text)
	}
}

// TestControllerStopDuringCompletionRace proves a stop landing after the
// run durably completed keeps the completion instead of rewriting it as a
// cancellation.
func TestControllerStopDuringCompletionRace(t *testing.T) {
	f := newFixture(t, finalText("finished"))
	f.newSession()

	gate := make(chan struct{})
	atGate := make(chan struct{})
	var once sync.Once
	f.ctrl.Observe(func(e UIEvent) {
		if e.View.Complete {
			once.Do(func() {
				atGate <- struct{}{}
				<-gate // park the worker exactly at the Done-view publish
			})
		}
		select {
		case f.views <- e.View:
		default:
		}
	})

	if err := f.ctrl.Submit(f.rootCtx, "task"); err != nil {
		t.Fatal(err)
	}
	<-atGate // Done is durable; the worker is parked before its tail

	stopErr := make(chan error, 1)
	go func() { stopErr <- f.ctrl.Stop(f.rootCtx) }()
	// Release the worker only after Stop owns the terminal transition;
	// otherwise the worker's natural tail could win the lock and the stop
	// would legitimately report no active run.
	deadline := time.Now().Add(5 * time.Second)
	for f.ctrl.State() != RunStopping && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(gate) // release the worker; Stop joins, then settles
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not converge")
	}
	f.waitIdle("race stop")
	if got := f.ctrl.Phase(); got != PhaseComplete {
		t.Fatalf("phase after race = %s, want complete kept", got)
	}
	if f.prov.count() != 1 {
		t.Fatalf("provider calls = %d", f.prov.count())
	}
	sess, _ := f.ctrl.Session()
	if sess.LastKnown != session.StatusComplete {
		t.Fatalf("meta status = %s", sess.LastKnown)
	}
}

func TestControllerRunTimeoutLeavesIncompleteDecision(t *testing.T) {
	f := newFixtureWithOptions(t, fixtureOptions{runTimeout: "150ms"}, blockingReply())
	meta := f.newSession()

	if err := f.ctrl.Submit(f.rootCtx, "slow task"); err != nil {
		t.Fatal(err)
	}
	<-f.prov.entered
	f.waitIdle("timeout interruption")

	// A timeout is a recoverable interruption: no auto-abandon, decision open.
	if got := f.ctrl.Phase(); got != PhaseIncomplete {
		t.Fatalf("phase after timeout = %s", got)
	}
	// Submit is gated until the user decides.
	if err := f.ctrl.Submit(f.rootCtx, "another"); !errors.Is(err, ErrDecisionRequired) {
		t.Fatalf("submit during incomplete = %v", err)
	}
	// Abandon uses a fresh SettlementTarget taken now.
	if err := f.ctrl.Abandon(f.rootCtx); err != nil {
		t.Fatal(err)
	}
	if got := f.ctrl.Phase(); got != PhaseSettled {
		t.Fatalf("phase after abandon = %s", got)
	}
	if f.prov.count() != 1 {
		t.Fatalf("abandon made a model call: %d", f.prov.count())
	}
	_ = meta
}

func TestControllerOpenIsReadOnly(t *testing.T) {
	f := newFixture(t, finalText("first answer"), finalText("second answer"))
	meta := f.newSession()
	if err := f.ctrl.Submit(f.rootCtx, "question"); err != nil {
		t.Fatal(err)
	}
	f.waitView(func(v ViewModel) bool { return v.Complete }, "completion")
	f.waitIdle("completion")

	// Re-open the session: cache view, zero model calls.
	res, err := f.ctrl.Open(f.rootCtx, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != PhaseComplete {
		t.Fatalf("open phase = %s", res.Phase)
	}
	if res.View.Source != SourceCache || res.View.Cache != CacheCurrent {
		t.Fatalf("open view = source:%v cache:%v", res.View.Source, res.View.Cache)
	}
	if !res.View.Complete || len(res.View.History) == 0 {
		t.Fatalf("open view missing authoritative history: %+v", res.View)
	}
	if got := f.prov.count(); got != 1 {
		t.Fatalf("open made model calls: %d", got)
	}
	if _, err := f.ctrl.Open(f.rootCtx, "s-missing"); err == nil {
		t.Fatal("unknown session opened")
	}
}

func TestControllerContinueGating(t *testing.T) {
	f := newFixture(t, finalText("done"))
	f.newSession()
	if err := f.ctrl.Continue(f.rootCtx); err == nil {
		t.Fatal("continue accepted without an interrupted run")
	}
}

func TestControllerSubmitRejectsWhileRunning(t *testing.T) {
	f := newFixture(t, blockingReply())
	f.newSession()
	if err := f.ctrl.Submit(f.rootCtx, "one"); err != nil {
		t.Fatal(err)
	}
	<-f.prov.entered
	if err := f.ctrl.Submit(f.rootCtx, "two"); !errors.Is(err, ErrRunActive) {
		t.Fatalf("second submit = %v", err)
	}
	if err := f.ctrl.Stop(f.rootCtx); err != nil {
		t.Fatal(err)
	}
}

// TestControllerSettleFaultBlocksThenRetriesSameToken proves the settle
// failure window: the session blocks, new input is refused, and retrying
// the ORIGINAL token (never a refreshed head) unblocks it.
func TestControllerSettleFaultBlocksThenRetriesSameToken(t *testing.T) {
	f := newFixture(t, blockingReply())
	f.newSession()

	flaky := &flakyStore{Store: f.mgr.Store()}
	f.ctrl.store = flaky
	f.ctrl.controlAgent = agent.New(f.startup.Provider, tool.NewRegistry(), agent.WithStore(flaky))

	if err := f.ctrl.Submit(f.rootCtx, "task"); err != nil {
		t.Fatal(err)
	}
	<-f.prov.entered

	flaky.failAppend = true
	if err := f.ctrl.Stop(f.rootCtx); err == nil {
		t.Fatal("settle fault was swallowed")
	}
	if got := f.ctrl.Phase(); got != PhaseBlocked {
		t.Fatalf("phase after failed settle = %s", got)
	}
	if f.ctrl.SettleErr() == nil {
		t.Fatal("settle error not surfaced")
	}
	if err := f.ctrl.Submit(f.rootCtx, "next"); !errors.Is(err, ErrSettlementPending) {
		t.Fatalf("submit while blocked = %v", err)
	}

	flaky.failAppend = false
	if err := f.ctrl.RetrySettle(f.rootCtx); err != nil {
		t.Fatal(err)
	}
	if got := f.ctrl.Phase(); got != PhaseSettled {
		t.Fatalf("phase after retry = %s", got)
	}
	f.prov.addReply(finalText("after retry"))
	if err := f.ctrl.Submit(f.rootCtx, "next"); err != nil {
		t.Fatal(err)
	}
	f.waitView(func(v ViewModel) bool { return v.Complete }, "post-retry run")
	f.waitIdle("post-retry")
}

// TestControllerApprovalCancelDuringWait covers stop while a side-effect
// approval is pending: the late approval is refused and nothing is written.
func TestControllerApprovalCancelDuringWait(t *testing.T) {
	f := newFixtureWithOptions(t, fixtureOptions{noApproval: false}, toolCall("c1", "edit",
		`{"path":"a.txt","old_text":"al","new_text":"AL","text":"ALpha"}`))
	workspacePath := f.startup.Workspace.Path()
	if err := os.WriteFile(filepath.Join(workspacePath, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.newSession()

	if err := f.ctrl.Submit(f.rootCtx, "edit the file"); err != nil {
		t.Fatal(err)
	}
	// The approval request is the barrier: the tool is waiting on the user.
	var request ApprovalRequest
	select {
	case request = <-f.startup.Approvals.Requests():
	case <-time.After(5 * time.Second):
		t.Fatal("approval request never arrived")
	}
	if request.Tool != "edit" || request.Path != "a.txt" {
		t.Fatalf("approval request = %+v", request)
	}

	if err := f.ctrl.Stop(f.rootCtx); err != nil {
		t.Fatal(err)
	}
	f.waitIdle("approval cancel")
	if got := f.ctrl.Phase(); got != PhaseSettled {
		t.Fatalf("phase = %s", got)
	}
	// The late approval only closes the stale prompt; it authorizes nothing.
	if f.startup.Approvals.Respond(request.Token, request.ID, true) {
		t.Fatal("late approval was accepted after cancellation")
	}
	data, err := os.ReadFile(filepath.Join(workspacePath, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alpha" {
		t.Fatalf("file was modified despite cancel: %q", data)
	}
}
