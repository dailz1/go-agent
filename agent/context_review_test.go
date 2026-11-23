package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestReviewUnnamedMeasurementReadable(t *testing.T) {
	m := anchorMeasurement(2)
	m.provider = ""
	identity := measurementIdentityFor(m, 2)
	if err := validateMeasurementIdentity(mustJSON(t, identity), identity); err != nil {
		t.Fatalf("legal unnamed provider rejected: %v", err)
	}
	c := &contextRuntime{}
	c.restoreAnchor(&contextAnchorProjection{measurementIdentity: identity, InputTokens: 1},
		[]llm.Message{llm.UserMessage("q"), llm.AssistantMessage("a")})
	if c.anchor != nil {
		t.Fatal("unnamed provider restored across invocations")
	}
}

func TestReviewSupersededAllowsV5Declaration(t *testing.T) {
	log := recoverySupersededFixture(t, false)
	p, err := decodeContextRecovery(log[len(log)-1])
	if err != nil {
		t.Fatal(err)
	}
	// The proof spans an earlier completed reply, before the recovery episode.
	decl := measurementRecord(t, 0, llm.AssistantMessage("earlier reply"),
		&llm.Usage{InputTokens: 100}, 2)
	log = append(log[:1], append([]store.Record{decl}, log[1:]...)...)
	*p.SelectedConfigSeq++
	*p.ModelChangeSeq++
	log = sequenceRecoveryLog(log)
	if err := validateSuperseded(log, log[len(log)-1], p); err != nil {
		t.Fatalf("schema 5 reply blocks proved model switch: %v", err)
	}
}

func TestReviewDurableFallbackUsesLiveHistory(t *testing.T) {
	history := []llm.Message{llm.UserMessage("q"), {Role: llm.RoleAssistant}}
	c := &contextRuntime{durable: true}
	if got, want := uint64(c.estimate(history)), uint64(estimateRunes(history)); got != want {
		t.Fatalf("fallback %d, live-history oracle %d", got, want)
	}
	generation := uint64(0)
	a := newUsageAnchor(anchorMeasurement(1), history[:1], &generation)
	a.durable = true
	generation++
	if got, want := uint64(estimateContextTokens(history, a)), uint64(estimateRunes(history)); got != want {
		t.Fatalf("invalid anchor fallback %d, live-history oracle %d", got, want)
	}
}

func TestReviewEstimateKeepsExactTokenSum(t *testing.T) {
	for _, tokens := range []int64{3_000_000_000, math.MaxInt64} {
		history := []llm.Message{llm.UserMessage("q")}
		generation := uint64(0)
		m := anchorMeasurement(1)
		m.inputTokens, m.outputTokens = tokens, 0
		a := newUsageAnchor(m, history, &generation)
		base := estimateRunes(history)
		history = append(history, llm.AssistantMessage("suffix"))
		want := uint64(tokens) + uint64(estimateRunes(history)-base)
		if got := uint64(estimateContextTokens(history, a)); got != want {
			t.Fatalf("tokens %d: estimate %d, want exact %d", tokens, got, want)
		}
	}
}

func TestReviewFinishRoundCopyIsolatesReasoningSummary(t *testing.T) {
	original := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		&llm.ReasoningItemBlock{Summary: []string{"original"}},
	}}
	copied := copyMessage(original)
	copied.Content[0].(*llm.ReasoningItemBlock).Summary[0] = "mutated"
	if original.Content[0].(*llm.ReasoningItemBlock).Summary[0] != "original" {
		t.Fatal("FinishRound copy shares reasoning summary with live history")
	}
}

func TestReviewUnanchoredZeroTargetWarnsAndContinues(t *testing.T) {
	called := false
	a := New(NewMockProvider(), nil, WithCompactionThresholdPercent(60), WithCompactor(CompactFunc(
		func(context.Context, []llm.Message, CompactionBudget, llm.Provider, []llm.Option) (CompactionResult, error) {
			called = true
			return CompactionResult{}, errMeasurementCrash
		})))
	_, ok := a.compactHistoryRound(t.Context(), []llm.Message{llm.UserMessage("q")},
		a.provider, nil, func(AgentEvent, error) bool { return true }, nil, 1, &contextRuntime{})
	if !called || !ok {
		t.Fatalf("legacy compactor called %v, continued %v", called, ok)
	}
}

func TestReviewUnnamedProviderRoundTrip(t *testing.T) {
	for _, usage := range []*llm.Usage{nil, {InputTokens: 100}} {
		for _, backend := range []string{"memory", "jsonl"} {
			t.Run(backend+"/"+map[bool]string{true: "usage", false: "nil"}[usage != nil], func(t *testing.T) {
				st, reopen := measurementBackend(t, backend)
				p := recoveryNamedProvider{Provider: NewMockStreamingProvider([][]llm.Chunk{
					contextChunks("continued", usage), contextChunks("done", usage),
				})}
				a := New(p, tool.NewRegistry(), WithStore(st), WithCompactor(nil),
					WithLLMOptions(llm.WithModel("x")), WithMaxIter(2), WithFinishRound(contextContinue))
				if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
					t.Fatal(err)
				}
				v, err := a.replayThread(t.Context(), "thread")
				if err != nil {
					t.Fatalf("declaration/Done replay: %v", err)
				}
				if _, err := st.Append(t.Context(), "thread", v.head, measurementCheckpoint(t, v)); err != nil {
					t.Fatal(err)
				}
				a = New(p, nil, WithStore(reopen()))
				v, err = a.replayThread(t.Context(), "thread")
				if err != nil {
					t.Fatalf("checkpoint replay: %v", err)
				}
				c := &contextRuntime{durable: true}
				c.restoreAnchor(v.anchor, seededHistory(v.system, v.history))
				if c.anchor != nil {
					t.Fatal("unnamed provider anchor restored across invocation")
				}
			})
		}
	}
}

func TestReviewSupersededAfterMeasuredToolRound(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(map[bool]string{true: "checkpoint", false: "full_log"}[checkpoint], func(t *testing.T) {
			st := store.NewMemory()
			p := &scriptedProvider{steps: []scriptedStep{
				{stream: true, chunks: measurementToolChunks(&llm.Usage{InputTokens: 100})},
				{stream: true, err: overflowTestError()},
			}}
			a := New(p, measurementRegistry(2), WithStore(st), WithCompactor(nil),
				WithLLMOptions(llm.WithModel("A")), WithLogger(discardLogger()))
			if _, err := a.Run(t.Context(), "q", WithThread("thread")); err == nil {
				t.Fatal("expected blocked overflow")
			}
			v, err := a.replayThread(t.Context(), "thread")
			if err != nil || v.anchor == nil || v.recovery == nil {
				t.Fatalf("measured completed tool round missing: anchor=%v recovery=%v err=%v", v.anchor, v.recovery, err)
			}
			if checkpoint {
				if _, err := st.Append(t.Context(), "thread", v.head, measurementCheckpoint(t, v)); err != nil {
					t.Fatal(err)
				}
			}
			if err := SetThreadConfig(t.Context(), st, "thread", ConfigChange{Model: strptr("B")}); err != nil {
				t.Fatal(err)
			}
			next := &scriptedProvider{steps: []scriptedStep{{stream: true, chunks: textChunks("repaired")}}}
			a = New(next, measurementRegistry(2), WithStore(st), WithCompactor(nil),
				WithLLMOptions(llm.WithModel("A")), WithLogger(discardLogger()))
			if _, err := a.ResumeThread(t.Context(), "thread"); err != nil {
				t.Fatalf("explicit model repair failed after schema 5 tool reply: %v", err)
			}
			if _, err := a.replayThread(t.Context(), "thread"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewUnanchoredOverlayKeepsCanonicalCap(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{true: "changed", false: "unchanged"}[changed], func(t *testing.T) {
			history := []llm.Message{llm.UserMessage(strings.Repeat("x", 650))}
			a := New(NewMockProvider(), nil, WithCompactionThresholdPercent(60),
				WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) { return "overlay", nil }),
				WithCompactor(CompactFunc(func(_ context.Context, h []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
					if changed {
						h[0] = llm.UserMessage(strings.Repeat("s", 600))
					}
					return CompactionResult{History: h, Changed: changed}, nil
				})))
			var failure error
			_, ok := a.compactHistoryRound(t.Context(), history, a.provider, nil,
				func(_ AgentEvent, err error) bool { failure = err; return true }, nil, 1000, &contextRuntime{})
			if !ok || failure != nil {
				t.Fatalf("history between target and canonical cap rejected: %v", failure)
			}
		})
	}
}

type reviewMutableBlock struct {
	*llm.TextBlock
	encodes *int
}

func (b *reviewMutableBlock) MarshalJSON() ([]byte, error) {
	if b.encodes != nil {
		*b.encodes++
	}
	return json.Marshal(b.TextBlock)
}

func TestReviewUnknownEncoderFreshCandidate(t *testing.T) {
	block := &reviewMutableBlock{TextBlock: &llm.TextBlock{Type: "text", Text: "small"}}
	history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}
	a := New(NewMockProvider(), nil, WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
		block.Text = strings.Repeat("grown", 500)
		return "snapshot", nil
	}))
	fitter := newOverlayFitter(history)
	_, err := a.roundContextOutbound(t.Context(), deepCopyMessages(history), RoundContextRequest{}, 1000,
		uint64(estimateRunes(history)), fitter)
	if !errors.Is(err, ErrCompactionBudgetExceeded) {
		t.Fatalf("mutated unknown block exceeded window without rejection: %v", err)
	}
}

type reviewDrainStore struct {
	store.Store
	inject func(context.Context, string) error
}

func (s *reviewDrainStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if records[0].Kind == store.KindInputConsumed && s.inject != nil {
		inject := s.inject
		s.inject = nil
		if err := inject(ctx, thread); err != nil {
			return 0, err
		}
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func TestReviewDrainRecomputesAfterConfigAbsorption(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{true: "append_conflict", false: "initial_absorption"}[conflict], func(t *testing.T) {
			base := store.NewMemory()
			st := &reviewDrainStore{Store: base}
			log := sequenceRecoveryLog([]store.Record{
				mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
				measurementRecord(t, 0, llm.AssistantMessage(strings.Repeat("x", 800)), &llm.Usage{InputTokens: 10}, 2),
				mustRecord(t, store.KindInputQueued, "queued", inputQueuedPayload{Input: strings.Repeat("s", 200)}),
			})
			log[2].Schema = store.SchemaV2
			if _, err := base.Append(t.Context(), "thread", 0, log...); err != nil {
				t.Fatal(err)
			}
			a := New(NewMockProvider(), nil, WithStore(st))
			p, history, err := a.openSession(t.Context(), st, "thread", "", true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer p.release()
			c := &contextRuntime{durable: true}
			c.restoreAnchor(p.anchor, history)
			p.context = c
			change := func(ctx context.Context, thread string) error {
				return SetThreadConfig(ctx, base, thread, ConfigChange{Thinking: strptr("high")})
			}
			if conflict {
				st.inject = change
			} else if err := change(t.Context(), "thread"); err != nil {
				t.Fatal(err)
			}
			injected, err := p.drainInputs(t.Context(), func() int {
				e := c.estimate(history)
				return int(1000 - min(uint64(1000), e))
			})
			if err != nil || len(injected) != 0 || len(p.queue) != 1 {
				t.Fatalf("stale budget consumed input: injected=%d queue=%d err=%v", len(injected), len(p.queue), err)
			}
			v, err := a.replayThread(t.Context(), "thread")
			if err != nil || len(v.queue) != 1 || v.anchor != nil {
				t.Fatalf("queue or invalidation differs on replay: queue=%d anchor=%v err=%v", len(v.queue), v.anchor, err)
			}
		})
	}
}

type reviewRetryHeadStore struct {
	store.Store
	config bool
	fired  bool
}

func (s *reviewRetryHeadStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if !s.fired && records[0].Kind == store.KindRoundDeclared {
		s.fired = true
		head, err := s.Store.Append(ctx, thread, expected, records...)
		if err != nil {
			return 0, err
		}
		control := mustReviewControl(s.config)
		if _, err := s.Store.Append(ctx, thread, head, control); err != nil {
			return 0, err
		}
		// Store idempotency confirms the original batch at the newer head.
		return s.Store.Append(ctx, thread, expected, records...)
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

func mustReviewControl(config bool) store.Record {
	kind := store.KindInputQueued
	var payload any = inputQueuedPayload{Input: "queued"}
	if config {
		kind, payload = store.KindConfigChanged, configChangedPayload{Thinking: strptr("high")}
	}
	rec, err := encodeRecord(kind, "later-control", payload)
	if err != nil {
		panic(err)
	}
	rec.Schema = store.SchemaV2
	return rec
}

func TestReviewIdempotentAppendHeadPreservesResponsePosition(t *testing.T) {
	for _, config := range []bool{false, true} {
		t.Run(map[bool]string{true: "config_tail", false: "queue_tail"}[config], func(t *testing.T) {
			st := &reviewRetryHeadStore{Store: store.NewMemory(), config: config}
			var buf bytes.Buffer
			a := measurementTestAgent(st, measurementRegistry(0),
				[][]llm.Chunk{contextChunks("measured", &llm.Usage{InputTokens: 100})}, &buf)
			p, history, err := a.openSession(t.Context(), st, "thread", "q", false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer p.release()
			a.finishRound = func(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
				if config && (p.anchor != nil || p.context.anchor != nil || p.cfg.Thinking == nil) {
					t.Error("later config was skipped or stale usage activated")
				}
				if !config && (p.anchor == nil || p.anchor.SourceSeq != 1 || len(p.queue) != 1) {
					t.Errorf("response sequence or later queue lost: anchor=%v queue=%v", p.anchor, p.queue)
				}
				return FinishRoundDecision{}, errMeasurementCrash
			}
			seq, err := a.runStreamInternal(t.Context(), history, p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.foldRunStream(seq); !errors.Is(err, errMeasurementCrash) {
				t.Fatal(err)
			}
			v, err := a.replayThread(t.Context(), "thread")
			if err != nil || (v.anchor != nil) != (p.anchor != nil) {
				t.Fatalf("live/replay anchor differs: live=%v replay=%v err=%v", p.anchor, v.anchor, err)
			}
		})
	}
}

func TestReviewDefaultToSameNamedAlternateDropsAnchor(t *testing.T) {
	st := store.NewMemory()
	positional := &windowedProvider{name: "same", steps: [][]llm.Chunk{
		contextChunks("measured", &llm.Usage{InputTokens: 2000}),
	}}
	alternate := &windowedProvider{name: "same", steps: [][]llm.Chunk{textChunks("done")}}
	var buf bytes.Buffer
	a := New(positional, tool.NewRegistry(), WithStore(st), WithProviders(alternate),
		WithCompactor(nil), WithMaxIter(2), WithLLMOptions(llm.WithModel("x")),
		WithLogger(contextTestLogger(&buf)), WithFinishRound(func(ctx context.Context, _ FinishRoundContext) (FinishRoundDecision, error) {
			if err := SetThreadConfig(ctx, st, "thread", ConfigChange{Provider: strptr("same"), Model: strptr("x")}); err != nil {
				return FinishRoundDecision{}, err
			}
			v, err := aReviewView(ctx, st)
			if err != nil || v.anchor != nil {
				t.Errorf("replay retained positional-provider anchor: %v, %v", v.anchor, err)
			}
			return FinishRoundDecision{Action: FinishRoundContinue}, nil
		}))
	if _, err := a.Run(t.Context(), "q", WithThread("thread")); err != nil {
		t.Fatal(err)
	}
	budgets := contextBudgetRecords(t, &buf)
	if len(positional.received) != 1 || len(alternate.received) != 1 || len(budgets) != 2 || budgets[1].Valid {
		t.Fatalf("same-name route reused positional measurement: calls %d/%d budgets %+v",
			len(positional.received), len(alternate.received), budgets)
	}
}

func aReviewView(ctx context.Context, st store.Store) (threadView, error) {
	return New(nil, nil, WithStore(st)).replayThread(ctx, "thread")
}

func TestReviewV3CheckpointV4TailUsesCanonicalRounds(t *testing.T) {
	log := []store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
		mustRecord(t, store.KindCheckpoint, "old", agentCheckpoint{
			CodecVersion: 3, History: []llm.Message{llm.UserMessage("stale summary")},
			RunActive: true, RunID: "r", LastRound: 7,
		}),
		recoveryRecord(t, store.KindHistoryReplaced, historyReplacedPayload{
			RunID: "r", Round: 0, OperationID: "anchor", Reason: "legacy_anchor",
			History: []llm.Message{llm.UserMessage("canonical summary")},
		}),
	}
	log[1].Schema = store.SchemaV3
	log = sequenceRecoveryLog(log)
	a := New(nil, nil, WithStore(&scriptStore{records: log}))
	v, err := a.replayThread(t.Context(), "thread")
	if err != nil || v.lastRound != -1 || !historyContainsText(v.history, "canonical summary") {
		t.Fatalf("stale v3 snapshot interpreted v4 tail: round=%d err=%v", v.lastRound, err)
	}
}

func TestReviewWideUsageCompactionAndOverlayBudgets(t *testing.T) {
	for _, w := range []int{math.MaxInt32, int(^uint(0) >> 1)} {
		for _, tokens := range []int64{3_000_000_000, math.MaxInt64} {
			t.Run(strconv.Itoa(w)+"/"+strconv.FormatInt(tokens, 10), func(t *testing.T) {
				history := []llm.Message{llm.UserMessage("q")}
				c := &contextRuntime{}
				m := anchorMeasurement(1)
				m.inputTokens, m.outputTokens = tokens, 0
				c.anchor = newUsageAnchor(m, history, &c.generation)
				base := estimateRunes(history)
				history = append(history, llm.AssistantMessage("suffix"))
				e := uint64(tokens) + uint64(estimateRunes(history)-base)
				runes := estimateRunes(history)
				want := new(big.Int).Mul(big.NewInt(int64(runes)), big.NewInt(int64(w)))
				want.Quo(want, new(big.Int).SetUint64(e))
				expected := min(w, runes-1, int(want.Int64()))
				seen := -1
				a := New(NewMockProvider(), nil, WithCompactionThresholdPercent(100),
					WithCompactor(CompactFunc(func(_ context.Context, h []llm.Message, b CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
						seen = b.MaxRunes
						return CompactionResult{History: h}, nil
					})))
				var failure error
				_, ok := a.compactHistoryRound(t.Context(), history, a.provider, nil,
					func(_ AgentEvent, err error) bool { failure = err; return true }, nil, w, c)
				if e > uint64(w) && expected == 0 {
					if ok || seen != -1 || !errors.Is(failure, ErrCompactionBudgetExceeded) {
						t.Fatalf("zero Q dispatched compactor or request: called=%d ok=%v err=%v", seen, ok, failure)
					}
				} else if e > uint64(w) && seen != expected {
					t.Fatalf("wide E=%d: Q=%d, oracle=%d", e, seen, expected)
				}
				if e <= uint64(w) && seen != -1 {
					t.Fatalf("nonoverflowing usage compacted: %d", seen)
				}
				if e > uint64(w) {
					_, _, err := a.boundedRoundContextMessage(newOverlayFitter(history), "snapshot", w, e)
					if !errors.Is(err, ErrCompactionBudgetExceeded) {
						t.Fatalf("wide E escaped outbound window check: %v", err)
					}
				}
			})
		}
	}
}

type reviewNamedCounter struct {
	llm.Provider
	names int
}

func (p *reviewNamedCounter) Name() string {
	p.names++
	return "counted"
}

func TestReviewDispatchReusesIdentityAndSkipsUnusedEstimate(t *testing.T) {
	encodes := 0
	block := &reviewMutableBlock{TextBlock: &llm.TextBlock{Type: "text", Text: "q"}, encodes: &encodes}
	history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}
	provider := &reviewNamedCounter{Provider: &scriptedProvider{steps: []scriptedStep{
		{stream: true, err: &llm.APIError{StatusCode: 500}},
		{stream: true, err: llm.ErrStreamingNotSupported},
		{msg: &llm.Message{Role: llm.RoleAssistant}},
	}}}
	a := New(provider, nil, WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: time.Nanosecond}))
	measurement := requestMeasurement(provider, nil, nil)
	provider.names, encodes = 0, 0
	r := &overflowRecovery{a: a, provider: provider, window: 8192, context: &contextRuntime{}, measurement: measurement}
	request := RoundContextRequest{}
	var retries []RetryInfo
	result := r.chatWithRetryAndFallback(t.Context(), history, nil, &request,
		func(AgentEvent, error) bool { return true }, &retries)
	if result.err != nil || result.stream == nil || request.Attempt != 3 {
		t.Fatalf("fallback failed: attempts=%d err=%v", request.Attempt, result.err)
	}
	if provider.names != 0 {
		t.Errorf("frozen request identity recomputed %d times", provider.names)
	}
	if encodes != 3 {
		t.Errorf("history encoded %d times, want exactly three outbound evidence measurements", encodes)
	}
}

func TestReviewDeclarationBoundaryAllocationsDoNotGrowWithHistory(t *testing.T) {
	history := make([]llm.Message, 2048)
	for i := range history {
		history[i] = llm.UserMessage("q")
	}
	v := threadView{system: "system", history: history, cfgSeq: -1}
	m := &declaredMeasurement{
		measurementIdentity: measurementIdentityFor(anchorMeasurement(len(history)+2), len(history)+2),
		CaptureConfigSeq:    -1,
	}
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := v.declarationAnchor(store.Record{}, m); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("position validation copied history: %g allocations", allocs)
	}
}

func TestReviewAnchorActivationDoesNotEncodeMeasuredPrefix(t *testing.T) {
	history := []llm.Message{llm.UserMessage(strings.Repeat("prefix", 10000))}
	m := anchorMeasurement(1)
	generation := uint64(0)
	allocs := testing.AllocsPerRun(20, func() {
		if newUsageAnchor(m, history, &generation) == nil {
			t.Fatal("known prefix rejected")
		}
	})
	if allocs > 1 {
		t.Fatalf("activation serialized measured prefix: %g allocations", allocs)
	}
}

func TestReviewAppendRetainsUnreadControlTail(t *testing.T) {
	st := store.NewMemory()
	log := []store.Record{
		mustRecord(t, store.KindRunStarted, "r-start", runStartedPayload{RunID: "r", Input: "q"}),
		mustReviewControl(true),
	}
	if _, err := st.Append(t.Context(), "thread", 0, log...); err != nil {
		t.Fatal(err)
	}
	// A preceding idempotent write returned head=2 while only its own
	// record through seq=0 was folded. The control at seq=1 is unread.
	p := &persistence{a: New(nil, nil, WithStore(st)), thread: "thread", head: 2, cfgRead: 1, cfgSeq: -1}
	reply := mustRecord(t, store.KindRoundDeclared, "r-d0", roundDeclaredPayload{
		RunID: "r", Round: 0, Kind: terminalTextDeclaration, Message: llm.AssistantMessage("reply"),
	})
	if err := p.append(t.Context(), reply); err != nil {
		t.Fatal(err)
	}
	if p.cfgSeq != 1 || p.cfg.Thinking == nil || *p.cfg.Thinking != "high" {
		t.Fatalf("subsequent write skipped unread control: cfgSeq=%d cfg=%+v", p.cfgSeq, p.cfg)
	}
}
