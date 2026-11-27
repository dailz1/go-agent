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
	"github.com/dailz1/go-agent/tool"
)

func TestReview2RejectedCompactionPreservesDurableHistory(t *testing.T) {
	st := store.NewMemory()
	history := []llm.Message{
		llm.UserMessage(strings.Repeat("old", 150)), llm.AssistantMessage("old answer"),
		llm.UserMessage(strings.Repeat("protected", 60)), llm.AssistantMessage("latest answer"),
	}
	log := []store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: history[0].Content[0].(*llm.TextBlock).Text}),
		measurementRecord(t, 0, history[1], nil, 2),
	}
	closed, err := runClosedFor("r", 0, llm.Usage{}, llm.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	log = append(log, closed,
		mustRecord(t, store.KindRunStarted, "s-start", runStartedPayload{RunID: "s", Input: history[2].Content[0].(*llm.TextBlock).Text}))
	last := measurementRecord(t, 0, history[3], &llm.Usage{InputTokens: 4000}, 4)
	last.ID = "s-d0"
	var declared roundDeclaredPayload
	if err := json.Unmarshal(last.Payload, &declared); err != nil {
		t.Fatal(err)
	}
	declared.RunID = "s"
	last.Payload = mustJSON(t, declared)
	log = append(log, last)
	if _, err := st.Append(t.Context(), "thread", 0, log...); err != nil {
		t.Fatal(err)
	}
	a := New(NewMockStreamingProvider(nil), tool.NewRegistry(), WithStore(st), WithLogger(discardLogger()),
		WithCompactor(CompactFunc(func(_ context.Context, h []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			// A successful custom strategy can return a valid partial reduction.
			return CompactionResult{History: append(h[:1:1], h[2:]...), Changed: true}, nil
		})))
	p, history, err := a.openSession(t.Context(), st, "thread", "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()
	c := &contextRuntime{durable: true}
	p.context = c
	c.restoreAnchor(p.anchor, history)
	if c.anchor == nil || c.estimate(history) != 4000 {
		t.Fatalf("fixture did not restore measured history: anchor=%+v estimate=%d", p.anchor, c.estimate(history))
	}
	before := logRecords(t, st, "thread")
	anchor := p.anchor
	var failure error
	events := 0
	got, ok := a.compactHistoryRound(t.Context(), history, a.provider, a.llmOpts,
		func(event AgentEvent, err error) bool {
			if _, isCompaction := event.(CompactionEvent); isCompaction {
				events++
			}
			failure = err
			return true
		}, p, 2000, c)
	if ok || !errors.Is(failure, ErrCompactionBudgetExceeded) {
		t.Fatalf("protected group must miss Q: ok=%v err=%v", ok, failure)
	}
	if events != 0 || !reflect.DeepEqual(got, history) || p.anchor != anchor || c.anchor == nil {
		t.Errorf("rejected candidate adopted: events=%d anchor=%v runtime=%v", events, p.anchor, c.anchor != nil)
	}
	if after := logRecords(t, st, "thread"); !reflect.DeepEqual(before, after) {
		t.Error("rejected compaction changed the durable log")
	}
	v, err := a.replayThread(t.Context(), "thread")
	if err != nil || !reflect.DeepEqual(history, v.history) || !reflect.DeepEqual(anchor, v.anchor) {
		t.Fatalf("rejected candidate survived replay: history=%d anchor=%+v err=%v", len(v.history), v.anchor, err)
	}
}

func TestReview2UnanchoredCompactionSizesHistoryOnce(t *testing.T) {
	t.Run("estimate runs once when compaction does not trigger", func(t *testing.T) {
		encodes := 0
		block := &reviewMutableBlock{TextBlock: &llm.TextBlock{Type: "text", Text: strings.Repeat("x", 1000)}, encodes: &encodes}
		history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}
		a := New(NewMockProvider(), tool.NewRegistry(), WithLogger(discardLogger()))
		if _, ok := a.compactHistoryRound(t.Context(), history, a.provider, nil,
			func(AgentEvent, error) bool { return true }, nil, 100000, &contextRuntime{}); !ok {
			t.Fatal("non-triggering window must keep the run alive")
		}
		if encodes != 1 {
			t.Fatalf("unanchored history encoded %d times, want one estimate", encodes)
		}
	})
	t.Run("foreign compactor result is rejected", func(t *testing.T) {
		invoked := false
		compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			invoked = true
			poisoned := deepCopyMessages(history)
			poisoned[0].Content = []llm.ContentBlock{wrappedTextBlock{&llm.TextBlock{Type: "text", Text: "foreign"}}}
			return CompactionResult{History: poisoned, Changed: true}, nil
		})
		history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			&llm.TextBlock{Type: "text", Text: strings.Repeat("x", 1000)},
		}}}
		a := New(NewMockProvider(), tool.NewRegistry(), WithLogger(discardLogger()), WithCompactor(compactor))
		got, ok := a.compactHistoryRound(t.Context(), history, a.provider, nil,
			func(AgentEvent, error) bool { return true }, nil, 100, &contextRuntime{})
		if !ok {
			t.Fatal("foreign compactor result must retain warn-and-continue")
		}
		if !invoked {
			t.Fatal("compactor not invoked; the result-rejection path was not exercised")
		}
		if !reflect.DeepEqual(got, history) {
			t.Error("rejected candidate replaced the history")
		}
	})
}

func TestReview2CompactorOptionsCannotChangeMainRequest(t *testing.T) {
	p := NewMockStreamingProvider([][]llm.Chunk{contextChunks("answer", &llm.Usage{InputTokens: 100})})
	a := New(p, tool.NewRegistry(), WithContextWindowTokens(1000), WithLLMOptions(llm.WithModel("main")),
		WithLogger(discardLogger()), WithCompactor(CompactFunc(
			func(_ context.Context, h []llm.Message, _ CompactionBudget, _ llm.Provider, opts []llm.Option) (CompactionResult, error) {
				opts[0] = llm.WithModel("summarizer")
				return CompactionResult{History: []llm.Message{llm.UserMessage("small")}, Changed: true}, nil
			})))
	if _, err := a.Run(t.Context(), strings.Repeat("q", 900)); err != nil {
		t.Fatal(err)
	}
	if model := llm.ApplyOptions(p.LastOptions).Model; model != "main" {
		t.Fatalf("compactor changed main request model to %q", model)
	}
}

func TestReview2FinishRoundResultsIsolateImages(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("image", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return &tool.ToolResult{Images: []tool.Image{{URL: "original", Data: []byte("image")}}}, nil
	}))
	p := NewMockStreamingProvider([][]llm.Chunk{toolCallChunks("one", "image"), textChunks("done")})
	hooked := false
	a := New(p, reg, WithStore(st), WithCompactor(nil), WithLogger(discardLogger()),
		WithFinishRound(func(_ context.Context, fc FinishRoundContext) (FinishRoundDecision, error) {
			if fc.Round != 0 {
				return FinishRoundDecision{}, nil
			}
			hooked = true
			image := &fc.Results[0].Content[0].(*llm.ToolResultBlock).Images[0]
			image.URL = "mutated"
			image.Data[0] = 'X'
			return FinishRoundDecision{}, nil
		}))
	result, err := a.Run(t.Context(), "q", WithThread("thread"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := a.replayThread(t.Context(), "thread")
	if err != nil {
		t.Fatal(err)
	}
	if !hooked || !reflect.DeepEqual(result.History, v.history) {
		t.Fatal("hook result mutation changed live history relative to its durable record")
	}
}

func TestReview2SteeringHeadFitsExactAppendDelta(t *testing.T) {
	for _, anchored := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "anchored"}[anchored], func(t *testing.T) {
			st := store.NewMemory()
			log := []store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
				measurementRecord(t, 0, llm.AssistantMessage("measured"), &llm.Usage{InputTokens: 100}, 2),
			}
			for i, text := range []string{"head", "tail"} {
				r := mustRecord(t, store.KindInputQueued, "queue"+jsonNumber(i), inputQueuedPayload{Input: text})
				r.Schema = store.SchemaV2
				log = append(log, r)
			}
			if _, err := st.Append(t.Context(), "thread", 0, log...); err != nil {
				t.Fatal(err)
			}
			a := New(NewMockProvider(), tool.NewRegistry(), WithStore(st))
			p, history, err := a.openSession(t.Context(), st, "thread", "", true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer p.release()
			c := &contextRuntime{durable: true}
			if anchored {
				c.restoreAnchor(p.anchor, history)
			}
			p.context = c
			delta := estimateRunes(append(append([]llm.Message(nil), history...), llm.UserMessage("head"))) - estimateRunes(history)
			capacity := c.estimate(history) + uint64(delta)
			got, err := p.drainInputs(t.Context(), func() int { return int(capacity - c.estimate(history)) })
			want := 0
			if anchored {
				want = 1
			}
			if err != nil || len(got) != want || len(p.queue) != 2-want {
				t.Fatalf("exact append allowance: injected=%d queued=%d want=%d err=%v", len(got), len(p.queue), want, err)
			}
			if anchored && (messageString(got[0]) != "head" || p.queue[0].Input != "tail") {
				t.Fatal("drain violated FIFO")
			}
		})
	}
}
