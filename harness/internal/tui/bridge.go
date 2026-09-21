package tui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/dailz1/go-agent/harness/internal/app"
)

// Queue budgets. View events are full superseding snapshots, so overflow
// drops the oldest pending one; intents are user actions and report
// backpressure instead of silently vanishing; approval requests are never
// queued here at all — they ride their own lossless channel.
const (
	eventQueueSize  = 32
	intentQueueSize = 8
)

// bridge adapts the controller Host to the terminal program. It enforces
// the worker/UI isolation of harness/DESIGN.md §3: the Bubble Tea update
// loop only touches memory, every host call runs on a bridge goroutine, and
// host results re-enter the program as ordinary messages.
type bridge struct {
	host    app.Host
	send    func(tea.Msg)
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan app.UIEvent
	intents chan func(context.Context, func(tea.Msg))
	wg      sync.WaitGroup
}

func newBridge(host app.Host) *bridge {
	return &bridge{
		host:    host,
		events:  make(chan app.UIEvent, eventQueueSize),
		intents: make(chan func(context.Context, func(tea.Msg)), intentQueueSize),
	}
}

// start attaches the sink and launches the three bridge goroutines: the
// event pump (coalescing), the approval forwarder (lossless) and the intent
// runner (serialized). ctx should be the program lifetime context.
func (b *bridge) start(ctx context.Context, send func(tea.Msg)) {
	b.send = send
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.host.Observe(func(ev app.UIEvent) { b.enqueue(ev) })
	b.wg.Add(3)
	go b.pumpEvents()
	go b.forwardApprovals()
	go b.runIntents()
}

// stop cancels the bridge and joins its goroutines. A blocking intent in
// flight (e.g. stop-and-settle) is awaited, so terminal exit never abandons
// a settlement halfway.
func (b *bridge) stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
}

// exec schedules an intent. Called from the UI update loop; it never
// blocks. The intent receives the bridge context and a send function that
// is safe to use until the program exits.
func (b *bridge) exec(fn func(context.Context, func(tea.Msg))) {
	select {
	case b.intents <- fn:
	default:
		// Backpressure: the runner is wedged on a long blocking call; report
		// instead of dropping the action silently.
		b.dispatch(tea.Msg(noticeMsg{err: errBridgeBusy}))
	}
}

func (b *bridge) dispatch(msg tea.Msg) {
	if b.ctx == nil || b.ctx.Err() != nil {
		return // program is gone; nothing observes the message anymore
	}
	b.send(msg)
}

// enqueue publishes a view event. Each event supersedes the last, so on
// overflow the OLDEST pending snapshot is dropped to make room — the newest
// state always gets through.
func (b *bridge) enqueue(ev app.UIEvent) {
	select {
	case b.events <- ev:
		return
	default:
	}
	select {
	case <-b.events: // drop the stale snapshot
	default:
	}
	select {
	case b.events <- ev:
	default:
	}
}

func (b *bridge) pumpEvents() {
	defer b.wg.Done()
	for {
		select {
		case ev := <-b.events:
			b.dispatch(uiEventMsg(ev))
		case <-b.ctx.Done():
			return
		}
	}
}

// forwardApprovals moves approval requests into the program. Requests are
// never dropped or coalesced: a lost request would hang its run until
// cancellation instead of being answered.
func (b *bridge) forwardApprovals() {
	defer b.wg.Done()
	requests := b.host.ApprovalRequests()
	for {
		select {
		case req := <-requests:
			b.dispatch(approvalMsg{req: req})
		case <-b.ctx.Done():
			return
		}
	}
}

func (b *bridge) runIntents() {
	defer b.wg.Done()
	for {
		select {
		case fn := <-b.intents:
			fn(b.ctx, b.dispatch)
		case <-b.ctx.Done():
			return
		}
	}
}
