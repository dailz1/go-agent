package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// hotswapProvider is a named test double serving both Chat (summarization
// and the non-stream fallback) and ChatStream (conversation rounds),
// recording every call and its options. One instance can play the default
// provider, an injected alternate, and the summarizer at once.
type hotswapProvider struct {
	name string

	mu              sync.Mutex
	streamScripts   [][]llm.Chunk
	chatScripts     []llm.Message
	failFirstStream error // returned by the first ChatStream call (retryable)

	streamCalls int
	streamIdx   int
	chatCalls   int
	streamOpts  [][]llm.Option
	chatOpts    [][]llm.Option
	chatMsgLog  [][]llm.Message
}

func (p *hotswapProvider) Name() string { return p.name }

func (p *hotswapProvider) Chat(_ context.Context, messages []llm.Message, _ []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatCalls++
	p.chatOpts = append(p.chatOpts, opts)
	p.chatMsgLog = append(p.chatMsgLog, append([]llm.Message{}, messages...))
	if p.chatCalls > len(p.chatScripts) {
		return nil, nil, fmt.Errorf("hotswap provider %q: no chat scripts left", p.name)
	}
	msg := p.chatScripts[p.chatCalls-1]
	return &msg, nil, nil
}

func (p *hotswapProvider) ChatStream(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streamCalls++
	p.streamOpts = append(p.streamOpts, opts)
	if p.failFirstStream != nil && p.streamCalls == 1 {
		return nil, p.failFirstStream
	}
	if p.streamIdx >= len(p.streamScripts) {
		return nil, fmt.Errorf("hotswap provider %q: no stream scripts left", p.name)
	}
	chunks := p.streamScripts[p.streamIdx]
	p.streamIdx++
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func (p *hotswapProvider) streamModel(call int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return llm.ApplyOptions(p.streamOpts[call]).Model
}

func (p *hotswapProvider) chatModel(call int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return llm.ApplyOptions(p.chatOpts[call]).Model
}

// hotswapRegistry builds a registry with one tool whose execution lands a
// config change on the thread — the mid-run switch entry point.
func hotswapRegistry(st store.Store, ch ConfigChange) *tool.Registry {
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		if err := SetThreadConfig(ctx, st, "t", ch); err != nil {
			return nil, err
		}
		return tool.NewTextResult("42"), nil
	}))
	return reg
}

func TestSetThreadConfigProviderValidation(t *testing.T) {
	st := store.NewMemory()
	tests := []struct {
		name    string
		change  ConfigChange
		wantErr error
	}{
		{name: "provider without model is rejected", change: ConfigChange{Provider: strptr("glm")}, wantErr: ErrInvalidConfigChange},
		{name: "provider with empty model string is rejected", change: ConfigChange{Provider: strptr("glm"), Model: strptr("")}, wantErr: ErrInvalidConfigChange},
		{name: "empty provider name is rejected", change: ConfigChange{Provider: strptr(""), Model: strptr("glm-4.7")}, wantErr: ErrInvalidConfigChange},
		{name: "all fields nil is still empty", change: ConfigChange{}, wantErr: ErrEmptyConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SetThreadConfig(context.Background(), st, "t", tt.change)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SetThreadConfig err = %v, want %v", err, tt.wantErr)
			}
			state, lerr := st.Latest(context.Background(), "t")
			if lerr != nil {
				t.Fatalf("Latest: %v", lerr)
			}
			if state.Head != 0 {
				t.Errorf("rejected change landed records: head = %d, want 0", state.Head)
			}
		})
	}

	if err := SetThreadConfig(context.Background(), st, "t", ConfigChange{Provider: strptr("glm"), Model: strptr("glm-4.7")}); err != nil {
		t.Fatalf("valid provider+model switch rejected: %v", err)
	}
	state, _ := st.Latest(context.Background(), "t")
	if state.Head != 1 {
		t.Fatalf("valid switch head = %d, want 1", state.Head)
	}
}

func TestConfigChangedPayloadProviderFieldByteCompat(t *testing.T) {
	switched, err := json.Marshal(configChangedPayload{Model: strptr("glm-4.7"), Provider: strptr("glm")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(switched), `"provider":"glm"`) {
		t.Errorf("switch payload %s misses the provider field", switched)
	}

	// A record without a provider switch must stay byte-identical to the
	// pre-provider payload: older binaries only fail loudly on threads
	// that actually switched.
	modelOnly, err := json.Marshal(configChangedPayload{Model: strptr("model-b")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"model":"model-b","thinking":null}`; string(modelOnly) != want {
		t.Errorf("model-only payload = %s, want byte-identical %s", modelOnly, want)
	}
}

func TestDecodeConfigChangedProviderPairRule(t *testing.T) {
	tests := []struct {
		name      string
		payload   configChangedPayload
		wantError bool
	}{
		{name: "provider without model", payload: configChangedPayload{Provider: strptr("glm")}, wantError: true},
		{name: "provider with empty model", payload: configChangedPayload{Provider: strptr("glm"), Model: strptr("")}, wantError: true},
		{name: "empty provider name", payload: configChangedPayload{Provider: strptr(""), Model: strptr("glm-4.7")}, wantError: true},
		{name: "paired switch decodes", payload: configChangedPayload{Provider: strptr("glm"), Model: strptr("glm-4.7")}},
		{name: "legacy record without provider", payload: configChangedPayload{Model: strptr("m"), Thinking: strptr("enabled")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := encodeRecord(store.KindConfigChanged, "cfg-"+tt.name, tt.payload)
			if err != nil {
				t.Fatal(err)
			}
			rec.Schema = store.SchemaV2
			p, err := decodeConfigChanged(rec)
			if tt.wantError {
				if !errors.Is(err, ErrIncompatibleLog) {
					t.Fatalf("decodeConfigChanged err = %v, want ErrIncompatibleLog", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeConfigChanged: %v", err)
			}
			if tt.payload.Provider != nil && (p.Provider == nil || *p.Provider != *tt.payload.Provider) {
				t.Errorf("provider = %v, want %v", p.Provider, tt.payload.Provider)
			}
		})
	}
}

func TestConfigStateFoldsProviderAcrossRecords(t *testing.T) {
	var cfg configState
	cfg.fold(configChangedPayload{Model: strptr("model-a")})
	cfg.fold(configChangedPayload{Provider: strptr("glm"), Model: strptr("glm-4.7"), Thinking: strptr("enabled")})
	if cfg.Provider == nil || *cfg.Provider != "glm" || *cfg.Model != "glm-4.7" {
		t.Fatalf("fold after switch = %+v, want provider glm + model glm-4.7", cfg)
	}
	// A provider-less record leaves the folded provider untouched.
	cfg.fold(configChangedPayload{Thinking: strptr("disabled")})
	if *cfg.Provider != "glm" || *cfg.Model != "glm-4.7" || *cfg.Thinking != "disabled" {
		t.Fatalf("fold after provider-less record = %+v, want provider/model unchanged", cfg)
	}
	// apply keeps the base options and appends the overrides after them:
	// the thread model mechanically shadows a base WithModel, while base
	// cross-provider options ride along with no suppression machinery.
	opts := cfg.apply([]llm.Option{llm.WithModel("base-model"), llm.WithMaxTokens(512)})
	got := llm.ApplyOptions(opts)
	if got.Model != "glm-4.7" {
		t.Errorf("applied model = %q, want thread model glm-4.7 shadowing base", got.Model)
	}
	if got.MaxTokens != 512 || got.Thinking != "disabled" {
		t.Errorf("applied options = %+v, want base MaxTokens riding along + thread thinking", got)
	}
}

func TestThreadProviderSwitchNextRoundOnNewInstance(t *testing.T) {
	st := store.NewMemory()
	def := &hotswapProvider{name: "default-prov", streamScripts: [][]llm.Chunk{toolCallChunks("c1", "calc")}}
	alt := &hotswapProvider{name: "alt-prov", streamScripts: [][]llm.Chunk{textChunks("alt done")}}
	agent := New(def, hotswapRegistry(st, ConfigChange{Provider: strptr("alt-prov"), Model: strptr("alt-model")}),
		WithStore(st),
		WithProviders(alt),
		WithLLMOptions(llm.WithModel("base-model")),
		WithLogger(discardLogger()),
	)
	res, err := agent.Run(context.Background(), "compute", WithThread("t"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The in-flight first round completed on the default provider; only the
	// next round's request moved to the switched-to instance.
	if def.streamCalls != 1 {
		t.Errorf("default provider stream calls = %d, want 1 (in-flight round)", def.streamCalls)
	}
	if def.streamModel(0) != "base-model" {
		t.Errorf("round 1 model = %q, want base-model", def.streamModel(0))
	}
	if alt.streamCalls != 1 {
		t.Fatalf("alt provider stream calls = %d, want 1", alt.streamCalls)
	}
	if alt.streamModel(0) != "alt-model" {
		t.Errorf("round 2 model = %q, want alt-model (thread record shadows base)", alt.streamModel(0))
	}
	if txt := messageText(&res.Message); txt != "alt done" {
		t.Errorf("final message = %q, want alt done", txt)
	}
}

func TestThreadProviderSwitchPinsRetryAttempts(t *testing.T) {
	st := store.NewMemory()
	def := &hotswapProvider{name: "default-prov", streamScripts: [][]llm.Chunk{toolCallChunks("c1", "calc")}}
	alt := &hotswapProvider{
		name:            "alt-prov",
		failFirstStream: &net.OpError{Op: "dial", Err: syscall.ECONNRESET},
		streamScripts:   [][]llm.Chunk{textChunks("alt done")},
	}
	agent := New(def, hotswapRegistry(st, ConfigChange{Provider: strptr("alt-prov"), Model: strptr("alt-model")}),
		WithStore(st),
		WithProviders(alt),
		WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: 1, MaxDelay: 1}),
		WithLogger(discardLogger()),
	)
	res, err := agent.Run(context.Background(), "compute", WithThread("t"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Provider switching is round-granular, not attempt-granular: both
	// attempts of the switched round reuse the resolved instance.
	if def.streamCalls != 1 {
		t.Errorf("default provider stream calls = %d, want 1", def.streamCalls)
	}
	if alt.streamCalls != 2 {
		t.Errorf("alt provider stream calls = %d, want 2 (retry pinned to the resolved instance)", alt.streamCalls)
	}
	if len(res.Retries) != 1 {
		t.Errorf("retries = %d, want 1", len(res.Retries))
	}
}

func TestUnknownProviderAbortsRunAndRepairResumes(t *testing.T) {
	st := store.NewMemory()
	def := &hotswapProvider{name: "default-prov"}
	alt := &hotswapProvider{name: "alt-prov", streamScripts: [][]llm.Chunk{textChunks("alt done")}}
	agent := New(def, tool.NewRegistry(),
		WithStore(st),
		WithProviders(alt),
		WithLogger(discardLogger()),
	)
	ctx := context.Background()
	if err := SetThreadConfig(ctx, st, "t", ConfigChange{Provider: strptr("nope"), Model: strptr("m-nope")}); err != nil {
		t.Fatalf("SetThreadConfig: %v", err)
	}
	_, err := agent.Run(ctx, "go", WithThread("t"))
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("Run err = %v, want ErrUnknownProvider", err)
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), `"t"`) {
		t.Errorf("error %v does not name the unknown provider and thread", err)
	}
	if def.streamCalls != 0 {
		t.Errorf("default provider stream calls = %d, want 0 (fail closed before any request)", def.streamCalls)
	}

	// The thread is not damaged: a later record with a valid name repairs
	// the projection (last write wins) and the interrupted run resumes on
	// the repaired provider.
	if err := SetThreadConfig(ctx, st, "t", ConfigChange{Provider: strptr("alt-prov"), Model: strptr("alt-model")}); err != nil {
		t.Fatalf("repair SetThreadConfig: %v", err)
	}
	res, err := agent.ResumeThread(ctx, "t")
	if err != nil {
		t.Fatalf("ResumeThread after repair: %v", err)
	}
	if txt := messageText(&res.Message); txt != "alt done" {
		t.Errorf("resumed message = %q, want alt done", txt)
	}
	if alt.streamCalls != 1 || def.streamCalls != 0 {
		t.Errorf("stream calls default/alt = %d/%d, want 0/1", def.streamCalls, alt.streamCalls)
	}
}

func TestSummarizationFollowsSwitchedProvider(t *testing.T) {
	st := store.NewMemory()
	// Run one establishes a long first answer on the default provider; the
	// switch lands between runs; run two's round top compacts over budget,
	// and the summarization call must follow the switched-to provider with
	// the thread's effective model — not the constructor-era default.
	def := &hotswapProvider{name: "default-prov", streamScripts: [][]llm.Chunk{
		textChunks(strings.Repeat("answer ", 80)),
	}}
	alt := &hotswapProvider{
		name:        "alt-prov",
		chatScripts: []llm.Message{llm.AssistantMessage("compact summary")},
		streamScripts: [][]llm.Chunk{
			textChunks("alt done"),
			textChunks("third done"),
		},
	}
	agent := New(def, tool.NewRegistry(),
		WithStore(st),
		WithProviders(alt),
		WithCompactor(NewSummarizationCompactor()),
		WithContextWindowTokens(600),
		WithCompactionThresholdPercent(100),
		WithLLMOptions(llm.WithModel("base-model")),
		WithLogger(discardLogger()),
	)
	ctx := context.Background()
	if _, err := agent.Run(ctx, "first", WithThread("t")); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if def.chatCalls != 0 {
		t.Fatalf("unexpected summarization on default provider: %d chat calls", def.chatCalls)
	}
	if err := SetThreadConfig(ctx, st, "t", ConfigChange{Provider: strptr("alt-prov"), Model: strptr("alt-model")}); err != nil {
		t.Fatalf("SetThreadConfig: %v", err)
	}
	res, err := agent.Run(ctx, "go", WithThread("t"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if def.streamCalls != 1 || def.chatCalls != 0 {
		t.Errorf("default provider calls stream/chat = %d/%d, want 1/0 (dead provider untouched)", def.streamCalls, def.chatCalls)
	}
	if alt.chatCalls != 1 {
		t.Fatalf("summarization calls on alt = %d, want 1", alt.chatCalls)
	}
	if alt.chatModel(0) != "alt-model" {
		t.Errorf("summarization model = %q, want alt-model (thread effective options)", alt.chatModel(0))
	}
	if len(alt.chatMsgLog) != 1 || len(alt.chatMsgLog[0]) != 1 ||
		!strings.Contains(compactorText(alt.chatMsgLog[0][0]), "Summarize the following conversation:") {
		t.Errorf("summarization prompt = %#v, want the standard summarization prompt", alt.chatMsgLog)
	}
	if alt.streamCalls != 1 || alt.streamModel(0) != "alt-model" {
		t.Errorf("conversation on alt = %d calls, model %q; want 1 call on alt-model", alt.streamCalls, alt.streamModel(alt.streamCalls-1))
	}
	if !historyContainsText(res.History, "[Summary]\ncompact summary") {
		t.Errorf("history misses the summary: %#v", res.History)
	}

	// The compaction checkpoint written mid-run-2 carries the extended
	// config projection; a fresh run rebuilds from it and the very first
	// request resolves the switched-to provider without any new record.
	res, err = agent.Run(ctx, "third", WithThread("t"))
	if err != nil {
		t.Fatalf("third run (checkpoint rebuild): %v", err)
	}
	if txt := messageText(&res.Message); txt != "third done" {
		t.Errorf("third run message = %q, want third done", txt)
	}
	if def.streamCalls != 1 || def.chatCalls != 0 || alt.streamCalls != 2 {
		t.Errorf("calls default stream/chat, alt stream = %d/%d/%d, want 1/0/2", def.streamCalls, def.chatCalls, alt.streamCalls)
	}
	if alt.streamModel(1) != "alt-model" {
		t.Errorf("checkpoint-rebuilt run model = %q, want alt-model", alt.streamModel(1))
	}
}

// reasoningItemGroup builds an assistant message whose reasoning item is
// followed by its paired block inside one message — the Responses-family
// shape that compaction must never orphan.
func reasoningItemToolGroup(itemID, callID string, resultRunes int) (llm.Message, llm.Message) {
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningItemBlock{Type: "reasoning_item", ID: itemID, EncryptedContent: "enc-" + itemID},
		llm.ToolUseBlock{Type: "tool_use", ID: callID, Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
	}}
	result := llm.ToolResultMessage(callID, tool.NewTextResult(strings.Repeat("payload", resultRunes)))
	return assistant, result
}

// assertNoOrphanReasoningItems checks the rs_ pairing invariant: every
// reasoning item in an assistant message is followed by the non-reasoning
// block it belongs to, in order. Outbound Responses adapters replay the
// item immediately before its paired message/function_call item, so an
// orphaned item would be rejected by the server (openclaw #1540).
func assertNoOrphanReasoningItems(t *testing.T, history []llm.Message) {
	t.Helper()
	for _, message := range history {
		if message.Role != llm.RoleAssistant {
			continue
		}
		for i, block := range message.Content {
			switch block.(type) {
			case llm.ReasoningItemBlock, *llm.ReasoningItemBlock:
			default:
				continue
			}
			paired := false
			for _, later := range message.Content[i+1:] {
				switch later.(type) {
				case llm.ReasoningItemBlock, *llm.ReasoningItemBlock, llm.ReasoningBlock, *llm.ReasoningBlock:
					continue
				default:
					paired = true
				}
			}
			if !paired {
				t.Errorf("orphan reasoning item at content index %d in %+v", i, message.Content)
			}
		}
	}
}

func TestCompactionPreservesReasoningItemPairing(t *testing.T) {
	// Cross-provider history: tool groups produced by a Responses-family
	// provider (reasoning item + tool call), a reasoning item paired with
	// plain text, and neutral rounds in between.
	assistant1, result1 := reasoningItemToolGroup("rs_1", "call_1", 40)
	assistant2 := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.ReasoningItemBlock{Type: "reasoning_item", ID: "rs_2", EncryptedContent: "enc-rs_2"},
		llm.TextBlock{Type: "text", Text: "note"},
	}}
	assistant3, result3 := reasoningItemToolGroup("rs_3", "call_3", 200)
	history := []llm.Message{
		llm.UserMessage("first"),
		assistant1, result1,
		assistant2,
		assistant3, result3,
		llm.UserMessage("latest"),
		llm.AssistantMessage("final"),
	}

	compactors := []struct {
		name      string
		compactor Compactor
	}{
		{name: "drop oldest tool groups", compactor: NewDropOldestToolGroupsCompactor()},
		{name: "sliding window", compactor: NewSlidingWindowCompactor()},
		{name: "standard chain", compactor: NewStandardCompactor()},
	}
	for _, maxRunes := range []int{0, estimateRunes(history) - 200, estimateRunes(history) - 40} {
		for _, tt := range compactors {
			t.Run(fmt.Sprintf("%s budget %d", tt.name, maxRunes), func(t *testing.T) {
				result, err := tt.compactor.Compact(context.Background(), history, CompactionBudget{MaxRunes: maxRunes}, nil, nil)
				if err != nil && !errors.Is(err, ErrCompactionBudgetExceeded) {
					t.Fatalf("Compact: %v", err)
				}
				if _, perr := partitionHistory(result.History); perr != nil {
					t.Fatalf("compacted history is not partitionable: %v", perr)
				}
				assertNoOrphanReasoningItems(t, result.History)
			})
		}
	}
}
