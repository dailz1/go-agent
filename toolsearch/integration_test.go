package toolsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/codemode"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

// recordingProvider replays per-call chunk scripts and records what the
// kernel advertised on every call, so tests can pin the advertised tool
// snapshot per round without matching whole message histories.
type recordingProvider struct {
	chunks [][]llm.Chunk
	calls  []recordedCall
	next   int
}

type recordedCall struct {
	tools    []tool.ToolInfo
	messages []llm.Message
}

func (p *recordingProvider) Name() string { return "toolsearch_recording" }

func (p *recordingProvider) Chat(context.Context, []llm.Message, []tool.ToolInfo, ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, errors.New("chat path not scripted")
}

func (p *recordingProvider) ChatStream(_ context.Context, messages []llm.Message, tools []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	i := p.next
	p.next++
	p.calls = append(p.calls, recordedCall{tools: tools, messages: messages})
	if i >= len(p.chunks) {
		return nil, errors.New("no scripted round remaining")
	}
	chunks := p.chunks[i]
	return func(yield func(llm.Chunk, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

func searchCallChunks(id, query string) []llm.Chunk {
	args, _ := json.Marshal(map[string]string{"query": query})
	return []llm.Chunk{
		&llm.ToolCallStartChunk{Index: 0, ID: id, Name: DefaultToolName},
		&llm.ToolCallArgsChunk{Index: 0, ID: id, Delta: string(args)},
		&llm.DoneChunk{},
	}
}

func textChunks(text string) []llm.Chunk {
	return []llm.Chunk{&llm.TextDeltaChunk{Text: text}, &llm.DoneChunk{}}
}

func TestFirstRoundOnlySearchNextRoundAdvertisesLoaded(t *testing.T) {
	source := tool.NewRegistry()
	source.MustRegister(newStub("pdf_fill", "Fill PDF forms"))
	exposed := tool.NewRegistry()
	s, err := Connect(context.Background(), source, exposed)
	if err != nil {
		t.Fatal(err)
	}

	provider := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "pdf form"),
		textChunks("loaded and used"),
	}}
	a := agent.New(provider, exposed, agent.WithDynamicTools())
	out, err := a.Run(context.Background(), "fill the form")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.ToolCalls != 1 {
		t.Fatalf("ToolCalls = %d, want 1", out.ToolCalls)
	}
	if len(provider.calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.calls))
	}
	first := provider.calls[0].tools
	if len(first) != 1 || first[0].Name != s.Name() {
		t.Fatalf("first round advertised %v, want only the search tool", names(first))
	}
	second := provider.calls[1].tools
	if len(second) != 2 || second[0].Name != "pdf_fill" || second[1].Name != s.Name() {
		t.Fatalf("second round advertised %v, want loaded wrapper plus search", names(second))
	}
	if second[0].Description != "Fill PDF forms" {
		t.Fatalf("loaded schema description = %q", second[0].Description)
	}
}

func TestTruncatedSearchResultKeepsLoadedSet(t *testing.T) {
	source := tool.NewRegistry()
	longDescription := strings.Repeat("form filler paragraph ", 30)
	source.MustRegister(newStub("long_desc", longDescription))
	exposed := tool.NewRegistry()
	s, err := Connect(context.Background(), source, exposed, WithLimit(5))
	if err != nil {
		t.Fatal(err)
	}

	provider := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "form filler"),
		textChunks("acknowledged"),
	}}
	a := agent.New(provider, exposed,
		agent.WithDynamicTools(),
		agent.WithContextWindowTokens(1000))
	out, err := a.Run(context.Background(), "find the filler")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	truncated := false
	for _, msg := range out.History {
		for _, block := range msg.Content {
			if tb, ok := block.(*llm.ToolResultBlock); ok && strings.Contains(tb.Content, "tool result truncated") {
				truncated = true
			}
		}
	}
	if !truncated {
		t.Fatal("search result was not truncated by the kernel; test budget too large")
	}
	if len(provider.calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.calls))
	}
	second := provider.calls[1].tools
	if len(second) != 2 || second[0].Name != "long_desc" || second[1].Name != s.Name() {
		t.Fatalf("second round advertised %v, want loaded wrapper despite truncation", names(second))
	}
	if _, ok := exposed.Get("long_desc"); !ok {
		t.Fatal("loaded wrapper vanished from exposed")
	}
	st, _ := exposed.Get(s.Name())
	result := callSearch(t, st, `{"query":"form filler","limit":1}`)
	reout := decodeOutput(t, result)
	if result.IsError() || len(reout.Matches) != 1 || reout.Matches[0].NewlyLoaded {
		t.Fatalf("smaller-limit re-search = %+v IsError=%v, want idempotent reload", reout.Matches, result.IsError())
	}
	if len(exposed.List()) != 2 {
		t.Fatalf("re-search changed the exposed set: %v", exposed.List())
	}
}

func TestHostWithoutDynamicToolsKeepsStaleAdvert(t *testing.T) {
	source := tool.NewRegistry()
	source.MustRegister(newStub("pdf_fill", "Fill PDF forms"))
	exposed := tool.NewRegistry()
	s, err := Connect(context.Background(), source, exposed)
	if err != nil {
		t.Fatal(err)
	}

	provider := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "pdf form"),
		textChunks("done"),
	}}
	a := agent.New(provider, exposed)
	if _, err := a.Run(context.Background(), "fill the form"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	second := provider.calls[1].tools
	if len(second) != 1 || second[0].Name != s.Name() {
		t.Fatalf("second round advertised %v; without WithDynamicTools the run keeps the start snapshot", names(second))
	}
	if _, ok := exposed.Get("pdf_fill"); !ok {
		t.Fatal("wrapper was still published, independent of the agent option")
	}
}

func TestLoadedToolExecutesThroughKernel(t *testing.T) {
	source := tool.NewRegistry()
	ran := false
	tool1 := newStub("pdf_fill", "Fill PDF forms")
	tool1.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		ran = true
		return tool.NewTextResult("filled"), nil
	}
	source.MustRegister(tool1)
	exposed := tool.NewRegistry()
	if _, err := Connect(context.Background(), source, exposed); err != nil {
		t.Fatal(err)
	}

	fillArgs := json.RawMessage(`{"path":"a.pdf"}`)
	provider := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "pdf form"),
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-2", Name: "pdf_fill"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-2", Delta: string(fillArgs)},
			&llm.DoneChunk{},
		},
		textChunks("filled"),
	}}
	a := agent.New(provider, exposed, agent.WithDynamicTools(),
		agent.WithMaxIter(4))
	out, err := a.Run(context.Background(), "fill the form")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ran {
		t.Fatal("loaded wrapper did not forward to the source tool")
	}
	if out.ToolCalls != 2 {
		t.Fatalf("ToolCalls = %d, want search + business call", out.ToolCalls)
	}
	found := false
	for _, msg := range out.History {
		for _, block := range msg.Content {
			if tb, ok := block.(*llm.ToolResultBlock); ok && tb.Content == "filled" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("business result did not reach the history")
	}
}

func names(infos []tool.ToolInfo) []string {
	out := make([]string, len(infos))
	for i, info := range infos {
		out[i] = info.Name
	}
	return out
}

func TestDefaultLimitLoadsFiveWithSchemas(t *testing.T) {
	s, exposed, _ := connectTest(t)
	for i := 0; i < 6; i++ {
		stub := newStub(fmt.Sprintf("tool_%d_pdf", i), "pdf tool")
		stub.info.Parameters.Properties["path"] = tool.Param("string", "file path")
		s.source.MustRegister(stub)
	}

	provider := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "pdf"),
		textChunks("five loaded"),
	}}
	a := agent.New(provider, exposed, agent.WithDynamicTools())
	if _, err := a.Run(context.Background(), "pdf work"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.calls[0].tools) != 1 {
		t.Fatalf("first round advertised %v", names(provider.calls[0].tools))
	}
	second := provider.calls[1].tools
	if len(second) != 6 {
		t.Fatalf("second round advertised %d tools, want five loaded plus search", len(second))
	}
	for i, info := range second {
		if i < 5 && info.Name != fmt.Sprintf("tool_%d_pdf", i) {
			t.Fatalf("loaded order = %v, want tool_0..tool_4 by name tie-break", names(second))
		}
	}
	if second[5].Name != DefaultToolName {
		t.Fatalf("search tool missing at position 5: %v", names(second))
	}
	if second[0].Parameters.Properties["path"].Type != "string" {
		t.Fatalf("loaded schema lost its parameters: %+v", second[0].Parameters.Properties)
	}
}

func TestAgentBlocksApprovalToolWithoutCallback(t *testing.T) {
	s, exposed, _ := connectTest(t)
	admin := newStub("admin_pdf", "admin pdf tool")
	admin.info.RequiresApproval = true
	ran := false
	admin.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		ran = true
		return tool.NewTextResult("admin done"), nil
	}
	s.source.MustRegister(admin)

	provider := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "admin pdf"),
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-2", Name: "admin_pdf"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-2", Delta: `{}`},
			&llm.DoneChunk{},
		},
		textChunks("cannot run the admin tool"),
	}}
	a := agent.New(provider, exposed, agent.WithDynamicTools(), agent.WithMaxIter(5))
	out, err := a.Run(context.Background(), "run the admin tool")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran {
		t.Fatal("approval-required tool executed without a callback")
	}
	if out.ToolCalls != 2 {
		t.Fatalf("ToolCalls = %d, want search + blocked business call", out.ToolCalls)
	}
	blocked := false
	for _, msg := range out.History {
		for _, block := range msg.Content {
			if tb, ok := block.(*llm.ToolResultBlock); ok && strings.Contains(tb.Content, "requires approval but no approval callback") {
				blocked = true
			}
		}
	}
	if !blocked {
		t.Fatal("approval rejection did not reach the model history")
	}
}

func TestPersistedRecoveryRestartsFromSearchOnly(t *testing.T) {
	ctx := context.Background()
	store1 := store.NewMemory()
	source := tool.NewRegistry()
	source.MustRegister(newStub("pdf_fill", "Fill PDF forms"))
	exposed := tool.NewRegistry()
	if _, err := Connect(ctx, source, exposed); err != nil {
		t.Fatal(err)
	}
	provider1 := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "pdf form"),
		textChunks("loaded and used"),
	}}
	a1 := agent.New(provider1, exposed, agent.WithStore(store1), agent.WithDynamicTools())
	if _, err := a1.Run(ctx, "fill the form", agent.WithThread("t-recovery")); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	source2 := tool.NewRegistry()
	filled := newStub("pdf_fill", "Fill PDF forms")
	filled.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("filled"), nil
	}
	source2.MustRegister(filled)
	exposed2 := tool.NewRegistry()
	if _, err := Connect(ctx, source2, exposed2); err != nil {
		t.Fatal(err)
	}
	provider2 := &recordingProvider{chunks: [][]llm.Chunk{
		// stale name from history: fresh instance has not loaded it yet
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-2", Name: "pdf_fill"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-2", Delta: `{}`},
			&llm.DoneChunk{},
		},
		searchCallChunks("call-3", "pdf form"),
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-4", Name: "pdf_fill"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-4", Delta: `{}`},
			&llm.DoneChunk{},
		},
		textChunks("recovered"),
	}}
	a2 := agent.New(provider2, exposed2, agent.WithStore(store1), agent.WithDynamicTools(), agent.WithMaxIter(8))
	// Run 1 reached a terminal state, so continuing the thread is a new Run
	// over the replayed history, not ResumeThread (which needs an incomplete run).
	out2, err := a2.Run(ctx, "continue", agent.WithThread("t-recovery"))
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(provider2.calls) == 0 || len(provider2.calls[0].tools) != 1 || provider2.calls[0].tools[0].Name != DefaultToolName {
		t.Fatalf("resumed instance advertised %v, want search tool only", names(provider2.calls[0].tools))
	}
	stale := false
	for _, msg := range out2.History {
		for _, block := range msg.Content {
			if tb, ok := block.(*llm.ToolResultBlock); ok && strings.Contains(tb.Content, `tool "pdf_fill" not found`) {
				stale = true
			}
		}
	}
	if !stale {
		t.Fatal("stale name call did not produce the kernel not-found error")
	}
	if len(provider2.calls) < 3 {
		t.Fatalf("recovery rounds = %d, want at least 3", len(provider2.calls))
	}
	reloaded := provider2.calls[2].tools
	if len(reloaded) != 2 || reloaded[0].Name != "pdf_fill" {
		t.Fatalf("post-re-search advertisement = %v, want pdf_fill restored", names(reloaded))
	}
}

func TestRecordReplayRoundTripPreservesContract(t *testing.T) {
	build := func() (*tool.Registry, *tool.Registry) {
		source := tool.NewRegistry()
		source.MustRegister(newStub("pdf_fill", "Fill PDF forms"))
		exposed := tool.NewRegistry()
		if _, err := Connect(context.Background(), source, exposed); err != nil {
			panic(err)
		}
		return source, exposed
	}
	normalizeInfo := func(info tool.ToolInfo) tool.ToolInfo {
		b, err := json.Marshal(info)
		if err != nil {
			panic(err)
		}
		var out tool.ToolInfo
		if err := json.Unmarshal(b, &out); err != nil {
			panic(err)
		}
		return out
	}

	source, exposed := build()
	searchLiteral := exposed.List()[0]
	user := llm.UserMessage("fill the form")
	searchJSON := `{"matches":[{"name":"pdf_fill","description":"Fill PDF forms","newly_loaded":true}],"message":"Tools are available next round; use their advertised schemas."}`
	step1 := agenttest.Exchange{
		Method: agenttest.MethodChatStream,
		Request: agenttest.Request{
			Messages: []llm.Message{user},
			Tools:    exposed.List(),
			Options:  llm.ApplyOptions(nil),
		},
		StreamChunks: searchCallChunks("call-1", "pdf form"),
	}
	step2 := agenttest.Exchange{
		Method: agenttest.MethodChatStream,
		Request: agenttest.Request{
			Messages: []llm.Message{
				user,
				llm.AssistantToolCallMessage(&llm.ToolUseBlock{Type: "tool_use", ID: "call-1", Name: DefaultToolName, Input: json.RawMessage(`{"query":"pdf form"}`)}),
				llm.ToolResultMessage("call-1", &tool.ToolResult{Content: searchJSON}),
			},
			Tools: []tool.ToolInfo{
				normalizeInfo(source.List()[0]),
				exposed.List()[0],
			},
			Options: llm.ApplyOptions(nil),
		},
		StreamChunks: textChunks("done"),
	}
	recorder := agenttest.NewRecorder(agenttest.NewScriptedProvider(step1, step2))
	a := agent.New(recorder, exposed, agent.WithDynamicTools())
	if _, err := a.Run(context.Background(), "fill the form"); err != nil {
		t.Fatalf("recorded Run: %v", err)
	}
	recording, err := recorder.Bytes()
	if err != nil {
		t.Fatalf("recording: %v", err)
	}
	replay, err := agenttest.NewReplayer(recording)
	if err != nil {
		t.Fatalf("NewReplayer: %v", err)
	}

	// Run 2 re-wires the same contract through wire-normalized infos: the
	// recording decodes schemas into codec-normal form, so the replayed run
	// must present exactly that form for the strict request match to hold.
	source2 := tool.NewRegistry()
	filled := &stubTool{info: normalizeInfo(source.List()[0])}
	filled.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("filled"), nil
	}
	source2.MustRegister(filled)
	exposed2 := tool.NewRegistry()
	searchStub := &stubTool{info: normalizeInfo(searchLiteral)}
	searchStub.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		exposed2.MustRegister(&stubTool{info: normalizeInfo(source.List()[0])})
		return tool.NewTextResult(searchJSON), nil
	}
	exposed2.MustRegister(searchStub)
	a2 := agent.New(replay, exposed2, agent.WithDynamicTools())
	out2, err := a2.Run(context.Background(), "fill the form")
	if err != nil {
		t.Fatalf("replayed Run diverged from the recorded contract: %v", err)
	}
	if out2.ToolCalls != 1 {
		t.Fatalf("replayed ToolCalls = %d, want the single recorded search call", out2.ToolCalls)
	}
}
func TestCodemodeReverseOrderExecutesBothEntries(t *testing.T) {
	source := tool.NewRegistry()
	ran := false
	pdf := newStub("pdf_fill", "Fill PDF forms")
	pdf.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		ran = true
		return tool.NewTextResult("filled"), nil
	}
	source.MustRegister(pdf)
	exposed := tool.NewRegistry()
	mode, err := codemode.Connect(context.Background(), source, exposed, codemode.Config{},
		codemode.WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer mode.Close()
	if _, err := Connect(context.Background(), source, exposed); err != nil {
		t.Fatal(err)
	}

	codeArgs, _ := json.Marshal(map[string]string{"code": "return 1;", "catalog_version": mode.CatalogVersion()})
	codeArgsAfter, _ := json.Marshal(map[string]string{"code": "return 2;", "catalog_version": mode.CatalogVersion()})
	provider := &recordingProvider{chunks: [][]llm.Chunk{
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-1", Name: "run_code"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-1", Delta: string(codeArgs)},
			&llm.DoneChunk{},
		},
		searchCallChunks("call-2", "pdf form"),
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-3", Name: "pdf_fill"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-3", Delta: `{}`},
			&llm.DoneChunk{},
		},
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-4", Name: "run_code"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-4", Delta: string(codeArgsAfter)},
			&llm.DoneChunk{},
		},
		textChunks("both entries worked"),
	}}
	a := agent.New(provider, exposed, agent.WithDynamicTools(),
		agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return true }),
		agent.WithMaxIter(8))
	out, err := a.Run(context.Background(), "use both entries")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.ToolCalls != 4 {
		t.Fatalf("ToolCalls = %d, want two run_code + search + business", out.ToolCalls)
	}
	if !ran {
		t.Fatal("business tool did not execute")
	}
	first := names(provider.calls[0].tools)
	if len(first) != 2 {
		t.Fatalf("reverse-order first round = %v, want run_code and tool_search only", first)
	}
	second := names(provider.calls[1].tools)
	joined := strings.Join(second, ",")
	if strings.Contains(joined, "pdf_fill") {
		t.Fatalf("round receiving the search call already advertised the loaded tool: %v", second)
	}
	third := names(provider.calls[2].tools)
	thirdJoined := strings.Join(third, ",")
	if !strings.Contains(thirdJoined, "pdf_fill") || !strings.Contains(thirdJoined, "run_code") || !strings.Contains(thirdJoined, DefaultToolName) {
		t.Fatalf("third round = %v, want loaded tool and run_code preserved", third)
	}
	results := map[string]*llm.ToolResultBlock{}
	for _, msg := range out.History {
		for _, block := range msg.Content {
			if tb, ok := block.(*llm.ToolResultBlock); ok {
				results[tb.ToolUseID] = tb
			}
		}
	}
	for _, id := range []string{"call-1", "call-3", "call-4"} {
		tb, ok := results[id]
		if !ok || tb.IsError {
			t.Fatalf("tool result for %s missing or errored: %+v", id, tb)
		}
	}
	if results["call-1"].Content != "1" || results["call-4"].Content != "2" {
		t.Fatalf("script results = %q / %q, want 1 and 2", results["call-1"].Content, results["call-4"].Content)
	}
	if results["call-3"].Content != "filled" {
		t.Fatalf("business result = %q, want filled", results["call-3"].Content)
	}
}

func TestInterruptedRunLeavesUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	store1 := store.NewMemory()
	s, exposed, _ := connectTest(t)
	s.source.MustRegister(newStub("pdf_fill", "Fill PDF forms"))
	proceed := make(chan struct{})
	entered := make(chan struct{})
	blocker := newStub("slow", "slow tool")
	blocker.run = func(runCtx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		entered <- struct{}{}
		<-proceed
		return tool.NewTextResult("late"), runCtx.Err()
	}
	s.source.MustRegister(blocker)

	runCtx, cancel := context.WithCancel(ctx)
	provider1 := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "slow"),
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-2", Name: "slow"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-2", Delta: `{}`},
			&llm.DoneChunk{},
		},
	}}
	a1 := agent.New(provider1, exposed, agent.WithStore(store1), agent.WithDynamicTools(), agent.WithMaxIter(6))
	runErr := make(chan error, 1)
	go func() {
		_, err := a1.Run(runCtx, "work", agent.WithThread("t-unknown"))
		runErr <- err
	}()
	<-entered
	cancel()
	close(proceed)
	if err := <-runErr; err == nil {
		t.Fatal("cancelled run completed normally, want interruption")
	}

	provider2 := &recordingProvider{chunks: [][]llm.Chunk{
		textChunks("gave up"),
	}}
	a2 := agent.New(provider2, exposed, agent.WithStore(store1), agent.WithDynamicTools())
	out2, err := a2.ResumeThread(ctx, "t-unknown")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	unknown := false
	for _, msg := range out2.History {
		for _, block := range msg.Content {
			if tb, ok := block.(*llm.ToolResultBlock); ok && strings.Contains(tb.Content, "outcome unknown") {
				unknown = true
			}
		}
	}
	if !unknown {
		t.Fatal("interrupted tool call did not report outcome unknown after resume")
	}
}

func TestCodemodeCompositionSharedExposed(t *testing.T) {
	source := tool.NewRegistry()
	source.MustRegister(newStub("pdf_fill", "Fill PDF forms"))
	exposed := tool.NewRegistry()
	if _, err := Connect(context.Background(), source, exposed); err != nil {
		t.Fatal(err)
	}
	mode, err := codemode.Connect(context.Background(), source, exposed, codemode.Config{},
		codemode.WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer mode.Close()

	provider := &recordingProvider{chunks: [][]llm.Chunk{
		searchCallChunks("call-1", "pdf form"),
		{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-2", Name: "pdf_fill"},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-2", Delta: `{}`},
			&llm.DoneChunk{},
		},
		textChunks("done"),
	}}
	a := agent.New(provider, exposed, agent.WithDynamicTools(),
		agent.WithApprovalFn(func(tool.ToolInfo, json.RawMessage) bool { return true }),
		agent.WithMaxIter(6))
	out, err := a.Run(context.Background(), "work with pdf")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.ToolCalls != 2 {
		t.Fatalf("ToolCalls = %d, want search + business", out.ToolCalls)
	}
	first := names(provider.calls[0].tools)
	if !strings.Contains(strings.Join(first, ","), "run_code") || !strings.Contains(strings.Join(first, ","), DefaultToolName) {
		t.Fatalf("first round = %v, want both entries", first)
	}
	second := names(provider.calls[1].tools)
	joined := strings.Join(second, ",")
	if !strings.Contains(joined, "pdf_fill") || !strings.Contains(joined, "run_code") {
		t.Fatalf("second round = %v, want loaded tool and run_code preserved", second)
	}
}
