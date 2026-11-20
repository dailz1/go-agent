package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

func TestUsageAnchorEligibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		usage *llm.Usage
		input int64
		out   int64
		ok    bool
	}{
		{"valid_details_are_not_added", &llm.Usage{InputTokens: 1000, OutputTokens: 100, CachedTokens: 600, ReasoningTokens: 80}, 1000, 100, true},
		{"nil", nil, 0, 0, false},
		{"all_zero", &llm.Usage{}, 0, 0, false},
		{"output_only", &llm.Usage{OutputTokens: 100}, 0, 0, false},
		{"zero_output", &llm.Usage{InputTokens: 1}, 1, 0, true},
		{"negative_input", &llm.Usage{InputTokens: -1}, 0, 0, false},
		{"negative_output", &llm.Usage{InputTokens: 1, OutputTokens: -1}, 0, 0, false},
		{"negative_cache", &llm.Usage{InputTokens: 1, CachedTokens: -1}, 0, 0, false},
		{"negative_reasoning", &llm.Usage{InputTokens: 1, ReasoningTokens: -1}, 0, 0, false},
		{"cache_exceeds_input", &llm.Usage{InputTokens: 1, CachedTokens: 2}, 0, 0, false},
		{"overflow", &llm.Usage{InputTokens: math.MaxInt64, OutputTokens: 1}, 0, 0, false},
		{"large_overflow", &llm.Usage{InputTokens: math.MaxInt64/2 + 1, OutputTokens: math.MaxInt64/2 + 1}, 0, 0, false},
		{"maximum_total", &llm.Usage{InputTokens: math.MaxInt64 - 1, OutputTokens: 1}, math.MaxInt64 - 1, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, out, ok := eligibleUsage(tt.usage)
			if input != tt.input || out != tt.out || ok != tt.ok {
				t.Fatalf("eligibleUsage = (%d, %d, %v), want (%d, %d, %v)", input, out, ok, tt.input, tt.out, tt.ok)
			}
		})
	}
}

func TestContextFingerprintV1(t *testing.T) {
	t.Parallel()
	opts := llm.Options{Model: "model", Stop: []string{"stop"}}
	tools := []tool.ToolInfo{{Name: "read"}, {Name: "write"}}
	first := contextFingerprintV1(opts, tools)
	tests := []struct {
		name  string
		opts  llm.Options
		tools []tool.ToolInfo
		equal bool
		nil   bool
	}{
		{"deterministic", opts, tools, true, false},
		{"tool_order", opts, []tool.ToolInfo{tools[1], tools[0]}, false, false},
		{"options_change", llm.Options{Model: "other", Stop: opts.Stop}, tools, false, false},
		{"marshal_failure", llm.Options{Temperature: anchorFloat(math.NaN())}, tools, false, true},
		{"invalid_schema", opts, []tool.ToolInfo{{Parameters: tool.ParameterSchema{
			Keywords: []tool.SchemaKeyword{{Name: "custom", Value: json.RawMessage(`{`)}},
		}}}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := contextFingerprintV1(tt.opts, tt.tools)
			if tt.nil {
				if got != nil {
					t.Fatalf("marshal failure produced digest %x", got)
				}
				return
			}
			if len(got) != sha256.Size || bytes.Equal(first, got) != tt.equal {
				t.Fatalf("digest %x; equal to original = %v, want %v", got, bytes.Equal(first, got), tt.equal)
			}
		})
	}
	t.Run("versioned_encoding", func(t *testing.T) {
		optionsJSON, err := json.Marshal(opts)
		if err != nil {
			t.Fatal(err)
		}
		toolsJSON, err := json.Marshal(tools)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("go-agent-context-v1\x00" + `{"Options":` + string(optionsJSON) + `,"Tools":` + string(toolsJSON) + `}`)
		want := sha256.Sum256(payload)
		if !bytes.Equal(first, want[:]) {
			t.Fatalf("digest = %x, want %x", first, want)
		}
	})
}

func anchorFloat(n float64) *float64 { return &n }

func TestEstimateContextTokens(t *testing.T) {
	t.Parallel()
	for _, seed := range []int64{1, 19, 2026} {
		t.Run(fmt.Sprintf("generated_appends_seed_%d", seed), func(t *testing.T) {
			history := []llm.Message{llm.SystemMessage("system"), llm.UserMessage("question"), llm.AssistantMessage("answer")}
			prefixRunes := estimateRunes(history)
			generation := uint64(7)
			a := newUsageAnchor(anchorMeasurement(len(history)), history, &generation)
			if a == nil {
				t.Fatal("eligible boundary rejected")
			}
			corpus := anchorMessages()
			rng := rand.New(rand.NewSource(seed))
			for step := range 100 {
				want := uint64(1100 + estimateRunes(history) - prefixRunes)
				if got := estimateContextTokens(history, a); got != want {
					t.Fatalf("step %d: estimate = %d, want %d", step, got, want)
				}
				// Repeated estimates at H == P (or an already counted H)
				// must not charge the same suffix again.
				if got := estimateContextTokens(history, a); got != want {
					t.Fatalf("repeated step %d: estimate = %d, want %d", step, got, want)
				}
				for range 1 + rng.Intn(3) {
					history = append(history, corpus[rng.Intn(len(corpus))].message)
				}
			}
		})
	}
	tests := []struct {
		name   string
		change func(*usageAnchor, *[]llm.Message, *uint64)
	}{
		{"no_anchor", func(a *usageAnchor, _ *[]llm.Message, _ *uint64) { a.fingerprint = nil }},
		{"shorter_than_prefix", func(_ *usageAnchor, h *[]llm.Message, _ *uint64) { *h = (*h)[:1] }},
		{"generation_bumped_same_length", func(_ *usageAnchor, h *[]llm.Message, g *uint64) {
			*g++
			(*h)[0] = llm.UserMessage("replacement")
		}},
		{"marshal_mode_flip", func(_ *usageAnchor, h *[]llm.Message, _ *uint64) {
			*h = append(*h, llm.AssistantToolCallMessage(llm.ToolUseBlock{Input: json.RawMessage(`{`)}))
		}},
		{"custom_marshaler_mode_flip", func(_ *usageAnchor, h *[]llm.Message, _ *uint64) {
			*h = append(*h, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{&anchorCustomBlock{fail: true}}})
		}},
		{"custom_encodable_block", func(_ *usageAnchor, h *[]llm.Message, _ *uint64) {
			*h = append(*h, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{&anchorCustomBlock{}}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := []llm.Message{llm.UserMessage("question"), llm.AssistantMessage("answer")}
			generation := uint64(1)
			a := newUsageAnchor(anchorMeasurement(len(history)), history, &generation)
			if a == nil {
				t.Fatal("eligible boundary rejected")
			}
			tt.change(a, &history, &generation)
			if got, want := estimateContextTokens(history, a), uint64(estimateRunes(history)); got != want {
				t.Fatalf("fallback = %d, want %d", got, want)
			}
		})
	}
	t.Run("nil_anchor", func(t *testing.T) {
		for _, h := range [][]llm.Message{nil, {}, {llm.UserMessage("hello")}} {
			if got, want := estimateContextTokens(h, nil), uint64(estimateRunes(h)); got != want {
				t.Fatalf("without anchor = %d, want %d", got, want)
			}
		}
	})
	t.Run("fallback_boundary_adds_fallback_suffix", func(t *testing.T) {
		history := []llm.Message{llm.AssistantToolCallMessage(llm.ToolUseBlock{Input: json.RawMessage(`{`)})}
		prefixRunes := estimateRunes(history)
		generation := uint64(0)
		a := newUsageAnchor(anchorMeasurement(1), history, &generation)
		if a == nil {
			t.Fatal("known fallback boundary rejected")
		}
		for _, entry := range anchorMessages() {
			history = append(history, entry.message)
			if got, want := estimateContextTokens(history, a), uint64(1100+estimateRunes(history)-prefixRunes); got != want {
				t.Fatalf("%s: estimate = %d, want %d", entry.name, got, want)
			}
		}
	})
	t.Run("sum_retains_suffix_above_signed_range", func(t *testing.T) {
		history := []llm.Message{llm.UserMessage("question")}
		m := anchorMeasurement(1)
		m.inputTokens, m.outputTokens = math.MaxInt64-1, 1
		generation := uint64(0)
		a := newUsageAnchor(m, history, &generation)
		base := estimateRunes(history)
		history = append(history, llm.AssistantMessage("more"))
		if got, want := estimateContextTokens(history, a), uint64(math.MaxInt64)+uint64(estimateRunes(history)-base); got != want {
			t.Fatalf("exact estimate = %d, want %d", got, want)
		}
	})
}

func TestUsageAnchorBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*contextMeasurement, *[]llm.Message)
	}{
		{"nil_fingerprint", func(m *contextMeasurement, _ *[]llm.Message) { m.fingerprint = nil }},
		{"unknown_version", func(m *contextMeasurement, _ *[]llm.Message) { m.fingerprintVersion = 2 }},
		{"invalid_usage", func(m *contextMeasurement, _ *[]llm.Message) { m.inputTokens = 0 }},
		{"boundary_mismatch", func(m *contextMeasurement, _ *[]llm.Message) { m.prefixMessages++ }},
		{"empty_prefix", func(m *contextMeasurement, h *[]llm.Message) { m.prefixMessages, *h = 0, nil }},
		{"custom_boundary", func(_ *contextMeasurement, h *[]llm.Message) {
			(*h)[0].Content = []llm.ContentBlock{&anchorCustomBlock{}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := []llm.Message{llm.UserMessage("question")}
			m := anchorMeasurement(1)
			tt.change(&m, &history)
			generation := uint64(0)
			if a := newUsageAnchor(m, history, &generation); a != nil {
				t.Fatal("invalid measurement established anchor")
			}
		})
	}
}

func anchorMeasurement(prefix int) contextMeasurement {
	return contextMeasurement{
		provider: "provider", model: "model",
		fingerprint:        contextFingerprintV1(llm.Options{Model: "model"}, nil),
		fingerprintVersion: 1, prefixMessages: prefix,
		inputTokens: 1000, outputTokens: 100,
	}
}

type anchorCustomBlock struct {
	llm.TextBlock
	fail bool
}

func (b *anchorCustomBlock) MarshalJSON() ([]byte, error) {
	if b.fail {
		return nil, fmt.Errorf("custom block encoding failed")
	}
	return []byte(`{"type":"text","text":"custom"}`), nil
}

func TestAppendRunesDelta(t *testing.T) {
	t.Parallel()
	prefix := []llm.Message{llm.UserMessage("prefix")}
	for _, entry := range anchorMessages() {
		t.Run(entry.name, func(t *testing.T) {
			appended := []llm.Message{entry.message, llm.AssistantMessage("tail")}
			joined := append(append([]llm.Message{}, prefix...), appended...)
			if got, want := appendRunesDelta(prefix, appended), estimateRunes(joined)-estimateRunes(prefix); got != want {
				t.Fatalf("delta = %d, want %d", got, want)
			}
		})
	}
	malformed := []llm.Message{llm.AssistantToolCallMessage(llm.ToolUseBlock{Input: json.RawMessage(`{`)})}
	tests := []struct {
		name     string
		prev     []llm.Message
		appended []llm.Message
	}{
		{"H_equals_P_empty_suffix", prefix, nil},
		{"empty_appended_slice", prefix, []llm.Message{}},
		{"nil_prefix", nil, prefix},
		{"empty_prefix", []llm.Message{}, prefix},
		{"fallback_prefix", malformed, prefix},
		{"fallback_append", prefix, malformed},
		{"both_fallback", malformed, malformed},
		{"empty_raw_input", []llm.Message{llm.AssistantToolCallMessage(llm.ToolUseBlock{Input: json.RawMessage{}})}, prefix},
		{"custom_prefix", []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{&anchorCustomBlock{}}}}, prefix},
		{"custom_failing_append", prefix, []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{&anchorCustomBlock{fail: true}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			joined := append(append([]llm.Message{}, tt.prev...), tt.appended...)
			if got, want := appendRunesDelta(tt.prev, tt.appended), estimateRunes(joined)-estimateRunes(tt.prev); got != want {
				t.Fatalf("delta = %d, want %d", got, want)
			}
		})
	}
}

func anchorMessages() []struct {
	name    string
	message llm.Message
} {
	blocks := []struct {
		name  string
		value llm.ContentBlock
		ptr   llm.ContentBlock
	}{
		{"text", llm.TextBlock{Type: "text", Text: "中文 😀 \"\\\n\t<>&"},
			&llm.TextBlock{Type: "text", Text: "中文 😀 \"\\\n\t<>&"}},
		{"image", llm.ImageBlock{Type: "image", MIMEType: "image/png", Data: []byte{0, 1, 2, 255}},
			&llm.ImageBlock{Type: "image", MIMEType: "image/png", Data: []byte{0, 1, 2, 255}}},
		{"reasoning", llm.ReasoningBlock{Type: "reasoning", Content: "思考 😀"},
			&llm.ReasoningBlock{Type: "reasoning", Content: "思考 😀"}},
		{"reasoning_item", llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", Summary: []string{"摘要"}, EncryptedContent: "opaque+/="},
			&llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_1", Summary: []string{"摘要"}, EncryptedContent: "opaque+/="}},
		{"tool_use", llm.ToolUseBlock{Type: "tool_use", ID: "call", Name: "read", Input: json.RawMessage(" { \"path\": \"中文\", \"n\": 1 } ")},
			&llm.ToolUseBlock{Type: "tool_use", ID: "call", Name: "read", Input: json.RawMessage(" { \"path\": \"中文\", \"n\": 1 } ")}},
		{"tool_result", llm.ToolResultBlock{Type: "tool_result", ToolUseID: "call", Content: "结果\n", Images: []llm.ImageBlock{{Data: []byte{1, 2, 3}}}},
			&llm.ToolResultBlock{Type: "tool_result", ToolUseID: "call", Content: "结果\n", Images: []llm.ImageBlock{{Data: []byte{1, 2, 3}}}}},
	}
	var result []struct {
		name    string
		message llm.Message
	}
	for _, block := range blocks {
		for i, form := range []llm.ContentBlock{block.value, block.ptr} {
			result = append(result, struct {
				name    string
				message llm.Message
			}{
				name:    fmt.Sprintf("%s_%s", block.name, []string{"value", "pointer"}[i]),
				message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{form}},
			})
		}
	}
	return result
}

func TestCompactionTargetRunes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		B, R, E int64
		want    int64
	}{
		{"small_rune_history", 6000, 4000, 10000, 2400},
		{"legacy_cap", 6000, 20000, 7000, 6000},
		{"no_overflow", 6000, 4000, 5000, 6000},
		{"equal_estimate", 6000, 4000, 6000, 6000},
		{"single_rune", 1, 1, 2, 0},
		{"zero_budget", 0, 4000, 10000, 0},
		{"rounds_to_zero", 1, 2, 10000, 0},
		{"wide_product", math.MaxInt64/2 - 1, math.MaxInt64, math.MaxInt64 / 2, math.MaxInt64/2 - 1},
		{"equal_large_budget", math.MaxInt64 / 2, math.MaxInt64, math.MaxInt64 / 2, math.MaxInt64 / 2},
		{"maximum_overflow", math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64, math.MaxInt64 - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compactionTargetRunes(tt.B, tt.R, uint64(tt.E)); got != tt.want {
				t.Fatalf("Q(%d, %d, %d) = %d, want %d", tt.B, tt.R, tt.E, got, tt.want)
			}
		})
	}
	t.Run("generated_grid_big_integer_oracle", func(t *testing.T) {
		values := []int64{0, 1, 2, 3, 17, 99, 4000, 6000, 7000, 10000, 20000, math.MaxInt32, math.MaxInt64/2 - 1, math.MaxInt64 / 2, math.MaxInt64 - 1, math.MaxInt64}
		for _, B := range values {
			for _, R := range values[1:] {
				for _, E := range values[1:] {
					want := B
					if E > B {
						product := new(big.Int).Mul(big.NewInt(R), big.NewInt(B))
						product.Quo(product, big.NewInt(E))
						want = min(B, R-1, product.Int64())
					}
					got := compactionTargetRunes(B, R, uint64(E))
					if got != want || (E > B && (got >= R || got > B || got < 0)) {
						t.Fatalf("Q(%d, %d, %d) = %d, want %d", B, R, E, got, want)
					}
				}
			}
		}
	})
}
