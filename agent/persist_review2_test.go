package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

// Simulate an ambiguous write whose identical retry is confirmed only after
// an independent control writer has appended another record.
type review2LaterHeadStore struct {
	store.Store
	kind   string
	change configChangedPayload
}

func (s *review2LaterHeadStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if records[0].Kind != s.kind {
		return s.Store.Append(ctx, thread, expected, records...)
	}
	s.kind = ""
	head, err := s.Store.Append(ctx, thread, expected, records...)
	if err != nil {
		return 0, err
	}
	control, err := encodeRecord(store.KindConfigChanged, "later-control", s.change)
	if err != nil {
		return 0, err
	}
	control.Schema = store.SchemaV2
	if _, err := s.Store.Append(ctx, thread, head, control); err != nil {
		return 0, err
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func TestReview2ResponseConfirmationConfigOrder(t *testing.T) {
	for _, kind := range []string{"tool", "terminal_text", "done"} {
		for _, change := range []string{"same_model", "different_model", "thinking"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				cp := configChangedPayload{Model: strptr("x")}
				if change == "different_model" {
					cp.Model = strptr("y")
				} else if change == "thinking" {
					cp = configChangedPayload{Thinking: strptr("enabled")}
				}
				recordKind := store.KindRoundDeclared
				if kind == "done" {
					recordKind = store.KindAgentEvent
				}
				st := &review2LaterHeadStore{Store: store.NewMemory(), kind: recordKind, change: cp}
				a := New(NewMockProvider(), nil, WithStore(st), WithLLMOptions(llm.WithModel("x")))
				p, history, err := a.openSession(t.Context(), st, "thread", "q", false, false)
				if err != nil {
					t.Fatal(err)
				}
				defer p.release()
				response := llm.AssistantMessage("measured")
				if kind == "tool" {
					response = toolCallMessage("one")
				}
				history = append(history, response)
				c := &contextRuntime{durable: true}
				p.context = c
				m := requestMeasurement(a.provider, a.llmOpts, nil)
				m.captureConfigSeq = -1
				p.stageMeasurement(m, &llm.Usage{InputTokens: 2000}, history)
				switch kind {
				case "tool":
					err = p.declareRound(t.Context(), 0, response)
					if err == nil {
						results := unknownResults(response)
						err = p.commitRound(t.Context(), 0, results)
						history = append(history, results...)
					}
				case "terminal_text":
					err = p.declareTerminalRound(t.Context(), 0, response)
				case "done":
					err = p.finishRun(t.Context(), response, 0, false, llm.Usage{InputTokens: 2000}, llm.Usage{InputTokens: 2000})
				}
				if err != nil {
					t.Fatal(err)
				}
				v, err := a.replayThread(t.Context(), "thread")
				if err != nil {
					t.Fatal(err)
				}
				valid := change == "same_model"
				if (p.anchor != nil) != valid || (c.anchor != nil) != valid || !reflect.DeepEqual(p.anchor, v.anchor) {
					t.Errorf("live/replay usage differs: live=%+v runtime=%v replay=%+v", p.anchor, c.anchor != nil, v.anchor)
				}
				if got, want := c.estimate(history), uint64(measurementEstimate(v)); got != want {
					t.Errorf("live/replay budget = %d/%d", got, want)
				}
				if p.cfgSeq != 2 || p.cfgRead != p.head {
					t.Errorf("later control was not folded: seq=%d read=%d head=%d", p.cfgSeq, p.cfgRead, p.head)
				}
			})
		}
	}
}

func TestReview2OpeningAppendFoldsControlTail(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "new_run", true: "resume_commit"}[resume], func(t *testing.T) {
			base := store.NewMemory()
			msg := llm.AssistantMessage("measured")
			if resume {
				msg = toolCallMessage("one")
			}
			log := []store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
				measurementRecord(t, 0, msg, &llm.Usage{InputTokens: 2000}, 2),
			}
			if !resume {
				rec, err := runClosedFor("r", 0, llm.Usage{}, llm.Usage{})
				if err != nil {
					t.Fatal(err)
				}
				log = append(log, rec)
			}
			if _, err := base.Append(t.Context(), "thread", 0, log...); err != nil {
				t.Fatal(err)
			}
			kind := store.KindRunStarted
			if resume {
				kind = store.KindRoundCommitted
			}
			st := &review2LaterHeadStore{Store: base, kind: kind, change: configChangedPayload{Thinking: strptr("enabled")}}
			a := New(NewMockProvider(), nil, WithStore(st))
			p, history, err := a.openSession(t.Context(), st, "thread", "next", resume, false)
			if err != nil {
				t.Fatal(err)
			}
			defer p.release()
			if err := p.refreshConfig(t.Context()); err != nil {
				t.Fatal(err)
			}
			v, err := a.replayThread(t.Context(), "thread")
			if err != nil {
				t.Fatal(err)
			}
			if p.cfg.Thinking == nil || p.anchor != nil || p.cfgSeq != v.cfgSeq {
				t.Fatalf("opening append skipped later control: live cfg=%+v anchor=%+v replay cfg=%+v", p.cfg, p.anchor, v.cfg)
			}
			c := &contextRuntime{durable: true}
			c.restoreAnchor(p.anchor, history)
			if got, want := c.estimate(history), uint64(measurementEstimate(v)); got != want {
				t.Fatalf("first live/replay budget = %d/%d", got, want)
			}
		})
	}
}

func TestReview2CheckpointFoldsControlTail(t *testing.T) {
	st := &review2LaterHeadStore{Store: store.NewMemory(), kind: store.KindCheckpoint, change: configChangedPayload{Thinking: strptr("enabled")}}
	a := New(NewMockProvider(), nil, WithStore(st))
	p, history, err := a.openSession(t.Context(), st, "thread", "q", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()
	if err := p.checkpoint(t.Context(), history); err != nil {
		t.Fatal(err)
	}
	if err := p.refreshConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := a.replayThread(t.Context(), "thread")
	if err != nil {
		t.Fatal(err)
	}
	if p.cfgSeq != v.cfgSeq || p.cfg.Thinking == nil {
		t.Fatalf("checkpoint skipped later control: live seq=%d replay=%d", p.cfgSeq, v.cfgSeq)
	}
}

func TestReview2DurableInputSuffixMatchesReplay(t *testing.T) {
	st := store.NewMemory()
	log := []store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
		measurementRecord(t, 0, llm.AssistantMessage("measured"), &llm.Usage{InputTokens: 2000}, 2),
	}
	closed, err := runClosedFor("r", 0, llm.Usage{}, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(t.Context(), "thread", 0, append(log, closed)...); err != nil {
		t.Fatal(err)
	}
	a := New(NewMockProvider(), nil, WithStore(st))
	p, history, err := a.openSession(t.Context(), st, "thread", "invalid\xff\xfeinput", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()
	v, err := a.replayThread(t.Context(), "thread")
	if err != nil {
		t.Fatal(err)
	}
	c := &contextRuntime{durable: true}
	c.restoreAnchor(p.anchor, history)
	if got, want := c.estimate(history), uint64(measurementEstimate(v)); got != want {
		t.Fatalf("invalid UTF-8 durable suffix differs: live=%d replay=%d", got, want)
	}
	if !reflect.DeepEqual(history, v.history) {
		t.Fatal("new durable input retained a different live representation")
	}
}

func TestReview2DoneReportedUsageRequiresObject(t *testing.T) {
	m := &doneMeasurement{measurementIdentity: measurementIdentityFor(anchorMeasurement(2), 2), UsageReported: true, CaptureConfigSeq: -1}
	inner := mustJSON(t, doneEventPayload{Message: llm.AssistantMessage("answer"), ContextMeasurement: m})
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		bad  bool
	}{{"missing", nil, true}, {"null", json.RawMessage("null"), true}, {"reported_zero", json.RawMessage("{}"), false}} {
		t.Run(tc.name, func(t *testing.T) {
			payload := mutateRecoveryJSON(t, store.Record{Payload: inner}, "usage", tc.raw)
			r := mustRecord(t, store.KindAgentEvent, "r-done", eventEnvelope{Type: "done", Version: eventEnvelopeV1, Payload: payload.Payload})
			r.Schema = store.SchemaV5
			if _, err := decodeAgentEvent(r); errors.Is(err, ErrIncompatibleLog) != tc.bad {
				t.Fatalf("reported usage %s: err=%v", tc.name, err)
			}
		})
	}
}

func TestReview2CheckpointPresenceSkipsHistoryCopies(t *testing.T) {
	payload := json.RawMessage(`{"history":"` + strings.Repeat("x", 128*1024) + `","context_anchor":null}`)
	allocs := testing.AllocsPerRun(10, func() {
		if !validCheckpointAnchor(payload, nil) {
			t.Fatal("explicit null anchor rejected")
		}
	})
	if allocs > 6 {
		t.Fatalf("checkpoint presence allocated unrelated values: %g allocations", allocs)
	}
}
