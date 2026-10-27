package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

type advertisedToolsProvider struct {
	chunks    [][]llm.Chunk
	failFirst int
	failErr   error
	index     int
	calls     [][]tool.ToolInfo
}

func (p *advertisedToolsProvider) Name() string { return "advertised-tools" }

func (p *advertisedToolsProvider) ChatStream(_ context.Context, _ []llm.Message, tools []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	p.calls = append(p.calls, append([]tool.ToolInfo(nil), tools...))
	if p.failFirst > 0 {
		p.failFirst--
		return nil, p.failErr
	}
	if p.index >= len(p.chunks) {
		return nil, fmt.Errorf("advertisedToolsProvider: no scripted response left (call %d)", len(p.calls))
	}
	chunks := p.chunks[p.index]
	p.index++
	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func (p *advertisedToolsProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}

func textRound(text string) []llm.Chunk {
	return []llm.Chunk{llm.TextDeltaChunk{Text: text}, llm.DoneChunk{FinishReason: "stop"}}
}

func assertAdvertised(t *testing.T, provider *advertisedToolsProvider, round int, want ...string) {
	t.Helper()
	if round >= len(provider.calls) {
		t.Fatalf("provider call %d never happened (calls=%d)", round, len(provider.calls))
	}
	got := provider.calls[round]
	if len(got) != len(want) {
		t.Fatalf("round %d advertised %d tools %v, want %v", round, len(got), infoNames(got), want)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("round %d advertised %v, want %v", round, infoNames(got), want)
		}
	}
}

func infoNames(infos []tool.ToolInfo) []string {
	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name
	}
	return names
}

func mutatorTool(name string, execute func(context.Context, json.RawMessage) (*tool.ToolResult, error)) *parallelTestTool {
	return &parallelTestTool{info: tool.ToolInfo{Name: name}, execute: execute}
}

func TestAdvertisedSnapshotIsRunLevelByDefault(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("a", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		reg.Unregister("b")
		if err := reg.Register(mutatorTool("c", nil)); err != nil {
			return nil, err
		}
		return tool.NewTextResult("mutated"), nil
	}))
	reg.MustRegister(mutatorTool("b", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("b"), nil
	}))
	provider := &advertisedToolsProvider{chunks: [][]llm.Chunk{
		parallelToolRound(parallelToolCalls("a")),
		textRound("done"),
	}}
	result, err := New(provider, reg, WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", result.ToolCalls)
	}
	assertAdvertised(t, provider, 0, "a", "b")
	assertAdvertised(t, provider, 1, "a", "b")
}

func TestDynamicToolsRefreshAdvertisedSnapshotEachIteration(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("a", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		reg.Unregister("b")
		if err := reg.Register(mutatorTool("c", nil)); err != nil {
			return nil, err
		}
		return tool.NewTextResult("mutated"), nil
	}))
	reg.MustRegister(mutatorTool("b", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("b"), nil
	}))
	provider := &advertisedToolsProvider{chunks: [][]llm.Chunk{
		parallelToolRound(parallelToolCalls("a")),
		parallelToolRound(parallelToolCalls("b")),
		textRound("done"),
	}}
	result, err := New(provider, reg, WithDynamicTools(), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertAdvertised(t, provider, 0, "a", "b")
	assertAdvertised(t, provider, 1, "a", "c")
	assertAdvertised(t, provider, 2, "a", "c")

	blocks := historyResultBlocks(result.History)
	if len(blocks) != 2 || !blocks[1].IsError {
		t.Fatalf("result blocks = %+v, want second block to be an error result", blocks)
	}
	if blocks[1].Content != `tool "b" not found` {
		t.Fatalf("removed-tool result = %q, want %q", blocks[1].Content, `tool "b" not found`)
	}
}

func TestDynamicToolsReplaceSwapsImplementationNextRound(t *testing.T) {
	reg := tool.NewRegistry()
	replacement := &parallelTestTool{info: tool.ToolInfo{Name: "swap", Description: "new desc"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("new"), nil
	}}
	reg.MustRegister(mutatorTool("swap", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		if err := reg.Replace(replacement); err != nil {
			return nil, err
		}
		return tool.NewTextResult("old"), nil
	}))
	provider := &advertisedToolsProvider{chunks: [][]llm.Chunk{
		parallelToolRound(parallelToolCalls("swap")),
		parallelToolRound(parallelToolCalls("swap")),
		textRound("done"),
	}}
	result, err := New(provider, reg, WithDynamicTools(), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	blocks := historyResultBlocks(result.History)
	if len(blocks) != 2 || blocks[0].Content != "old" || blocks[1].Content != "new" {
		t.Fatalf("replace results = %+v, want old then new", blocks)
	}
	if got := provider.calls[1][0].Description; got != "new desc" {
		t.Fatalf("round 2 advertised description = %q, want %q", got, "new desc")
	}
	if got := provider.calls[0][0].Description; got != "" {
		t.Fatalf("round 1 advertised description = %q, want the original", got)
	}
}

func TestDynamicToolsParallelRoundKeepsFrozenHandles(t *testing.T) {
	reg := tool.NewRegistry()
	newImpl := &parallelTestTool{info: tool.ToolInfo{Name: "swap"}, execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("new"), nil
	}}
	reg.MustRegister(mutatorTool("swap", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		if err := reg.Replace(newImpl); err != nil {
			return nil, err
		}
		return tool.NewTextResult("old"), nil
	}))
	provider := &advertisedToolsProvider{chunks: [][]llm.Chunk{
		parallelToolRound(parallelToolCalls("swap", "swap")),
		parallelToolRound(parallelToolCalls("swap")),
		textRound("done"),
	}}
	result, err := New(provider, reg, WithDynamicTools(), WithToolConcurrency(2), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	blocks := historyResultBlocks(result.History)
	if len(blocks) != 3 || blocks[0].Content != "old" || blocks[1].Content != "old" || blocks[2].Content != "new" {
		t.Fatalf("parallel replace results = %+v, want old, old, new", blocks)
	}
}

func TestDynamicToolsMutationInRoundContextProviderAppliesNextRound(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("a", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("a"), nil
	}))
	reg.MustRegister(mutatorTool("b", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("b"), nil
	}))
	provider := &advertisedToolsProvider{
		chunks: [][]llm.Chunk{
			parallelToolRound(parallelToolCalls("a")),
			textRound("done"),
		},
		failFirst: 1,
		failErr:   &llm.APIError{StatusCode: 429},
	}
	firstInvocation := true
	agent := New(
		provider,
		reg,
		WithDynamicTools(),
		WithRoundContextProvider(func(context.Context, RoundContextRequest) (string, error) {
			if firstInvocation {
				firstInvocation = false
				reg.Unregister("b")
			}
			return "", nil
		}),
		WithRetryConfig(AgentRetryConfig{MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}),
		WithLogger(discardLogger()),
	)
	if _, err := agent.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertAdvertised(t, provider, 0, "a", "b")
	assertAdvertised(t, provider, 1, "a", "b")
	assertAdvertised(t, provider, 2, "a")
}

func TestDynamicToolsMaxIterTerminalSkipsRemovedTool(t *testing.T) {
	reg := tool.NewRegistry()
	a := mutatorTool("a", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		reg.Unregister("b")
		return tool.NewTextResult("a"), nil
	})
	b := mutatorTool("b", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("b"), nil
	})
	reg.MustRegister(a)
	reg.MustRegister(b)
	provider := &advertisedToolsProvider{chunks: [][]llm.Chunk{
		parallelToolRound(parallelToolCalls("a")),
		parallelToolRound(parallelToolCalls("a", "b")),
	}}
	result, err := New(provider, reg, WithDynamicTools(), WithMaxIter(2), WithLogger(discardLogger())).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Truncated {
		t.Fatal("result not truncated at maxIter")
	}
	if got := a.calls.Load(); got != 1 {
		t.Fatalf("a executions = %d, want 1", got)
	}
	if got := b.calls.Load(); got != 0 {
		t.Fatalf("removed tool b executions = %d, want 0", got)
	}
	blocks := historyResultBlocks(result.History)
	if len(blocks) != 3 {
		t.Fatalf("result blocks = %d, want 3", len(blocks))
	}
	for _, block := range blocks[1:] {
		if !block.IsError {
			t.Fatalf("terminal skip block = %+v, want error", block)
		}
		if block.Content != "not executed: the run reached its iteration limit" {
			t.Fatalf("terminal skip content = %q", block.Content)
		}
	}
}

func TestDynamicToolsCompactionWithRemovedToolGroup(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("live", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("live"), nil
	}))
	base := []llm.Message{
		llm.SystemMessage("policy"),
		llm.UserMessage("first"),
		llm.AssistantToolCallMessage(llm.ToolUseBlock{
			Type: "tool_use", ID: "g1", Name: "gone_tool", Input: json.RawMessage(`{}`),
		}),
		llm.ToolResultMessage("g1", tool.NewTextResult(strings.Repeat("payload", 100))),
		llm.AssistantMessage(strings.Repeat("old", 1000)),
		llm.UserMessage("latest"),
	}
	provider := &advertisedToolsProvider{chunks: [][]llm.Chunk{textRound("done")}}
	agent := New(provider, reg, WithDynamicTools(), WithContextWindowTokens(1_000), WithLogger(discardLogger()))
	seq, err := agent.RunStreamWithHistory(context.Background(), base, "current")
	events, errs := collectEvents(t, seq, err)
	if len(errs) != 0 {
		t.Fatalf("run errors = %v", errs)
	}
	if len(compactionEvents(events)) == 0 {
		t.Fatal("compaction never ran")
	}
	if len(provider.calls) != 1 || provider.calls[0][0].Name != "live" {
		t.Fatalf("provider calls = %v", provider.calls)
	}
}

func TestPersistedHistoryWithUnregisteredToolReplays(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	firstCall := llm.ToolUseBlock{Type: "tool_use", ID: "c1", Name: "calc", Input: json.RawMessage(`{}`)}
	secondCall := llm.ToolUseBlock{Type: "tool_use", ID: "c2", Name: "calc", Input: json.RawMessage(`{}`)}
	agent := New(
		NewMockProvider(
			MsgResponse(llm.AssistantToolCallMessage(firstCall)),
			MsgResponse(llm.AssistantMessage("done")),
			MsgResponse(llm.AssistantToolCallMessage(secondCall)),
			MsgResponse(llm.AssistantMessage("done2")),
		),
		reg,
		WithStore(st),
		WithLogger(discardLogger()),
	)
	if _, err := agent.RunThread(ctx, "t", "compute"); err != nil {
		t.Fatalf("first RunThread: %v", err)
	}
	reg.Unregister("calc")
	res, err := agent.RunThread(ctx, "t", "again")
	if err != nil {
		t.Fatalf("second RunThread after Unregister: %v", err)
	}
	blocks := historyResultBlocks(res.History)
	if len(blocks) != 2 {
		t.Fatalf("result blocks = %d, want 2", len(blocks))
	}
	if blocks[0].IsError {
		t.Fatalf("replayed historical block = %+v, want original success", blocks[0])
	}
	if !blocks[1].IsError || blocks[1].Content != `tool "calc" not found` {
		t.Fatalf("new call block = %+v, want soft not-found error", blocks[1])
	}
}
