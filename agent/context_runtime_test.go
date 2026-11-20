package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type contextBudgetRecord struct {
	Message      string `json:"msg"`
	Estimate     int    `json:"estimate_tokens"`
	AnchorTokens int64  `json:"usage_anchor_tokens"`
	Tail         int    `json:"tail_estimate_tokens"`
	Valid        bool   `json:"anchor_valid"`
	Reason       string `json:"invalidation_reason"`
	Budget       int    `json:"budget_runes"`
}

func contextBudgetRecords(t *testing.T, buf *bytes.Buffer) []contextBudgetRecord {
	t.Helper()
	var records []contextBudgetRecord
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var record contextBudgetRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Message == "agent context budget" {
			records = append(records, record)
		}
	}
	return records
}

func contextTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func contextContinue(context.Context, FinishRoundContext) (FinishRoundDecision, error) {
	return FinishRoundDecision{Action: FinishRoundContinue}, nil
}

func contextChunks(text string, usage *llm.Usage) []llm.Chunk {
	return []llm.Chunk{llm.TextDeltaChunk{Text: text}, llm.DoneChunk{Usage: usage}}
}

func TestContextRuntimeCompactionBudgets(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		runes     int
		usage     *llm.Usage
		budget    int
		wantEvent bool
	}{
		{"false_positive_2200_tokens", 12000, &llm.Usage{InputTokens: 2000, OutputTokens: 200}, 0, false},
		{"measured_overflow_2400_runes", 4000, &llm.Usage{InputTokens: 9900, OutputTokens: 100}, 2400, true},
		{"nil_usage_legacy", 12000, nil, 6000, true},
		{"zero_usage_legacy", 12000, &llm.Usage{}, 6000, true},
		{"output_only_legacy", 12000, &llm.Usage{OutputTokens: 1}, 6000, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			overhead := estimateRunes([]llm.Message{llm.UserMessage("q"), llm.AssistantMessage("")})
			p := NewMockStreamingProvider([][]llm.Chunk{
				contextChunks(strings.Repeat("x", tt.runes-overhead), tt.usage), textChunks("done"),
			})
			var seen []int
			compactor := CompactFunc(func(_ context.Context, history []llm.Message, budget CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				seen = append(seen, budget.MaxRunes)
				history[1] = llm.AssistantMessage("kept")
				return CompactionResult{History: history, Changed: true}, nil
			})
			var buf bytes.Buffer
			a := New(p, tool.NewRegistry(), WithCompactor(compactor), WithContextWindowTokens(7500),
				WithMaxIter(2), WithFinishRound(contextContinue), WithLogger(contextTestLogger(&buf)))
			seq, err := a.RunStream(t.Context(), "q")
			events, errs := collectEvents(t, seq, err)
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			if tt.budget == 0 {
				if len(seen) != 0 || len(compactionEvents(events)) != 0 {
					t.Fatalf("unnecessary compaction: budgets %v", seen)
				}
			} else if len(seen) != 1 || seen[0] != tt.budget {
				t.Fatalf("budgets = %v, want [%d]", seen, tt.budget)
			}
			compacted := compactionEvents(events)
			if tt.wantEvent && (len(compacted) != 1 || compacted[0].BeforeRunes != tt.runes ||
				compacted[0].AfterRunes != estimateRunes([]llm.Message{llm.UserMessage("q"), llm.AssistantMessage("kept")})) {
				t.Fatalf("rune evidence = %+v", compacted)
			}
			records := contextBudgetRecords(t, &buf)
			if len(records) != 2 || records[1].Budget != tt.budget {
				t.Fatalf("budget decisions = %+v", records)
			}
		})
	}
}

func TestContextRuntimeMissingUsageKeepsBoundary(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"q", "中文输入"} {
		t.Run(input, func(t *testing.T) {
			tools := tool.NewRegistry()
			tools.MustRegister(&mockTool{info: tool.ToolInfo{Name: "echo"}, result: tool.NewTextResult("tool suffix")})
			p := NewMockStreamingProvider([][]llm.Chunk{
				contextChunks(strings.Repeat("prefix", 100), &llm.Usage{InputTokens: 1000, OutputTokens: 100, CachedTokens: 600, ReasoningTokens: 80}),
				toolCallChunks("one", "echo"), contextChunks("zero", &llm.Usage{}),
				contextChunks("output only", &llm.Usage{OutputTokens: 5}), textChunks("done"),
			})
			var buf bytes.Buffer
			a := New(p, tools, WithMaxIter(5), WithFinishRound(contextContinue), WithLogger(contextTestLogger(&buf)))
			if _, err := a.Run(t.Context(), input); err != nil {
				t.Fatal(err)
			}
			records := contextBudgetRecords(t, &buf)
			if len(records) != 5 {
				t.Fatalf("decisions = %d", len(records))
			}
			boundary := estimateRunes(p.AllMessages[1])
			for i := 1; i < len(records); i++ {
				tail := estimateRunes(p.AllMessages[i]) - boundary
				if !records[i].Valid || records[i].AnchorTokens != 1100 || records[i].Tail != tail || records[i].Estimate != 1100+tail {
					t.Fatalf("round %d decision = %+v; want 1100 + %d suffix", i, records[i], tail)
				}
			}
		})
	}
}

func TestContextRuntimeReplacementInvalidates(t *testing.T) {
	t.Parallel()
	for _, sameLength := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained_assistant", true: "edited_same_length"}[sameLength], func(t *testing.T) {
			p := NewMockStreamingProvider([][]llm.Chunk{
				contextChunks("measured reply", &llm.Usage{InputTokens: 9900, OutputTokens: 100}),
				textChunks("suffix"), textChunks("done"),
			})
			var replacement []llm.Message
			compactor := CompactFunc(func(_ context.Context, history []llm.Message, _ CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
				if sameLength {
					history[0] = llm.UserMessage("edited")
				} else {
					history = history[1:] // retain exactly the formerly measured assistant
				}
				replacement = deepCopyMessages(history)
				return CompactionResult{History: history, Changed: true}, nil
			})
			var buf bytes.Buffer
			a := New(p, tool.NewRegistry(), WithContextWindowTokens(7500), WithCompactor(compactor),
				WithMaxIter(3), WithFinishRound(contextContinue), WithLogger(contextTestLogger(&buf)))
			if _, err := a.Run(t.Context(), strings.Repeat("q", 3900)); err != nil {
				t.Fatal(err)
			}
			records := contextBudgetRecords(t, &buf)
			if len(records) != 3 || records[2].Valid || records[2].Estimate != estimateRunes(p.AllMessages[2]) || records[2].Reason != "history_replaced" {
				t.Fatalf("post-replacement decisions = %+v; replacement %+v", records, replacement)
			}
		})
	}
}

type contextFailingStream struct {
	MockProvider
	cause error
}

func (p *contextFailingStream) ChatStream(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return func(yield func(llm.Chunk, error) bool) {
		if yield(llm.DoneChunk{Usage: &llm.Usage{InputTokens: 1000}}, nil) {
			yield(nil, p.cause)
		}
	}, nil
}

func TestContextRuntimeFailedStreamDoesNotCapture(t *testing.T) {
	t.Parallel()
	cause := errors.New("stream broke after usage")
	p := &contextFailingStream{cause: cause}
	state := &contextRuntime{}
	a := New(p, tool.NewRegistry(), WithLogger(discardLogger()))
	r := overflowRecovery{a: a, provider: p, window: 8192, context: state}
	history := []llm.Message{llm.UserMessage("q")}
	var retries []RetryInfo
	accum, ok := r.receiveRound(t.Context(), &history, nil, RoundContextRequest{}, func(AgentEvent, error) bool { return true }, &retries)
	if ok || accum != nil || state.anchor != nil || r.measurement.fingerprint != nil {
		t.Fatal("failed attempt returned capturable measurement")
	}
}
