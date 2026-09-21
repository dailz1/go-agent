package tui

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dailz1/go-agent/harness/internal/app"
)

// The bridge is the worker/UI isolation boundary of harness/DESIGN.md §3.
// These tests pin its queue contracts: view events coalesce by dropping the
// oldest, approval requests are lossless, intents run serialized on their
// own goroutine, and stop cancels and joins everything.

func newBridgeForTest() (*bridge, *recordingHost) {
	host := &recordingHost{
		requests: make(chan app.ApprovalRequest, 16),
		idle:     make(chan struct{}),
	}
	return newBridge(host), host
}

// The full path — observe registration, pump, dispatch — forwards view
// events into the program send function in order.
func TestBridgeForwardsViewEvents(t *testing.T) {
	b, host := newBridgeForTest()
	got := make(chan app.UIEvent, 4)
	b.start(context.Background(), func(msg tea.Msg) {
		if ev, ok := msg.(uiEventMsg); ok {
			got <- app.UIEvent(ev)
		}
	})
	defer b.stop()

	for i := 0; i < 3; i++ {
		host.observe(app.UIEvent{View: app.ViewModel{Text: fmt.Sprint(i)}})
	}
	for want := 0; want < 3; want++ {
		select {
		case ev := <-got:
			if ev.View.Text != fmt.Sprint(want) {
				t.Fatalf("event = %q, want %q", ev.View.Text, fmt.Sprint(want))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d never forwarded", want)
		}
	}
}

// View events supersede each other: enqueue drops the OLDEST pending
// snapshot on overflow so the newest state always gets through in order.
// Tested at the enqueue seam (white-box) — with the pump running, a fast
// consumer would drain the queue and the drop would never trigger.
func TestBridgeEventsCoalesceDropOldest(t *testing.T) {
	b, _ := newBridgeForTest()
	total := eventQueueSize + 8
	for i := 0; i < total; i++ {
		b.enqueue(app.UIEvent{View: app.ViewModel{Text: fmt.Sprint(i)}})
	}
	for want := total - eventQueueSize; want < total; want++ {
		select {
		case ev := <-b.events:
			if ev.View.Text != fmt.Sprint(want) {
				t.Fatalf("event = %q, want %q", ev.View.Text, fmt.Sprint(want))
			}
		default:
			t.Fatalf("event %d missing from the coalesced queue", want)
		}
	}
	select {
	case ev := <-b.events:
		t.Fatalf("unexpected extra event %q", ev.View.Text)
	default:
	}
}

// Approval requests must never be dropped or coalesced: a lost request
// would hang its run until cancellation.
func TestBridgeApprovalsAreLossless(t *testing.T) {
	b, host := newBridgeForTest()
	got := make(chan app.ApprovalRequest, 16)
	b.start(context.Background(), func(msg tea.Msg) {
		if req, ok := msg.(approvalMsg); ok {
			got <- req.req
		}
	})
	defer b.stop()

	const n = 8
	for i := 0; i < n; i++ {
		host.requests <- app.ApprovalRequest{ID: uint64(i + 1)}
	}
	seen := map[uint64]bool{}
	for want := 0; want < n; want++ {
		select {
		case req := <-got:
			seen[req.ID] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("approval %d never arrived (seen %v)", want+1, seen)
		}
	}
	if len(seen) != n {
		t.Fatalf("distinct approvals = %d, want %d", len(seen), n)
	}
}

// Intents run one at a time on the bridge goroutine: while one blocks, the
// queued one cannot start.
func TestBridgeIntentsSerialized(t *testing.T) {
	b, _ := newBridgeForTest()
	var mu sync.Mutex
	order := []int{}
	firstStarted := make(chan struct{})
	secondDone := make(chan struct{})
	gate := make(chan struct{})
	b.start(context.Background(), func(tea.Msg) {})
	defer b.stop()

	b.exec(func(context.Context, func(tea.Msg)) {
		close(firstStarted)
		<-gate
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	})
	b.exec(func(context.Context, func(tea.Msg)) {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		close(secondDone)
	})

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first intent never started")
	}
	// While the first intent is parked on the gate, the second must not
	// have run: its completion channel is still open.
	select {
	case <-secondDone:
		t.Fatal("second intent ran while the first was blocked")
	default:
	}
	close(gate)
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second intent never finished after release")
	}
	mu.Lock()
	defer mu.Unlock()
	if order[0] != 1 || order[1] != 2 {
		t.Fatalf("intent order = %v, want [1 2]", order)
	}
}

// stop cancels the bridge context and joins the goroutines: an in-flight
// intent observing its context returns, and dispatch after stop is a no-op
// instead of a send into a dead program.
func TestBridgeStopCancelsAndJoins(t *testing.T) {
	b, _ := newBridgeForTest()
	started := make(chan struct{})
	stopped := make(chan struct{})
	b.start(context.Background(), func(tea.Msg) {
		t.Error("dispatch after stop must be dropped")
	})

	b.exec(func(ctx context.Context, send func(tea.Msg)) {
		close(started)
		<-ctx.Done()
		send(tea.Msg(noticeMsg{})) // must be dropped: program is gone
		close(stopped)
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("intent never started")
	}

	done := make(chan struct{})
	go func() { b.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not join the in-flight intent")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("stop returned before the intent observed cancellation")
	}
}
