package codemode

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/agenttest"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// rwTool starts as a free read tool; tests Replace it with an
// approval-requiring version mid-run via swap.
type rwTool struct {
	mu       sync.Mutex
	approval bool
	schema   int // bumped to change the parameter fingerprint
	seen     []json.RawMessage
}

func (t *rwTool) Info() tool.ToolInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	ps := tool.NewParameterSchema()
	ps.Properties["n"] = tool.Property{Type: "number"}
	if t.schema == 1 {
		ps.Properties["extra"] = tool.Property{Type: "string"}
	}
	return tool.ToolInfo{Name: "rw", Parameters: ps, RequiresApproval: t.approval}
}

func (t *rwTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	t.mu.Lock()
	t.seen = append(t.seen, append(json.RawMessage(nil), args...))
	t.mu.Unlock()
	return tool.NewTextResult("rw-ok"), nil
}

func (t *rwTool) swap(approval bool, schema int) {
	t.mu.Lock()
	t.approval = approval
	t.schema = schema
	t.mu.Unlock()
}

func TestReplaceTimeAuthorization(t *testing.T) {
	rw := &rwTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(rw); err != nil {
		t.Fatalf("register: %v", err)
	}
	var approvals []string
	m, err := Connect(context.Background(), source, model, Config{}, WithApproval(func(info tool.ToolInfo, args json.RawMessage) bool {
		approvals = append(approvals, info.Name+"|"+string(args))
		return true
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)

	firstStarted := make(chan struct{}, 1)
	firstRelease := make(chan struct{})
	var firstOnce sync.Once
	hook := func() {
		firstOnce.Do(func() {
			firstStarted <- struct{}{}
			<-firstRelease
		})
	}
	inner := &hookTool{inner: rw, onExecute: hook}
	if err := source.Replace(inner); err != nil {
		t.Fatalf("replace with hook: %v", err)
	}

	go func() {
		<-firstStarted
		rw.swap(true, 0) // same schema, now approval-requiring
		close(firstRelease)
	}()

	res := runScript(t, wrapper, `await tools["rw"]({n:1}); await tools["rw"]({n:2}); return "done"`)
	if res.IsError() {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if len(approvals) != 1 || !strings.HasPrefix(approvals[0], "rw|") {
		t.Fatalf("second call must prompt approval with live info, got %v", approvals)
	}
	rw.mu.Lock()
	seen := len(rw.seen)
	rw.mu.Unlock()
	if seen != 2 {
		t.Fatalf("both calls must execute, got %d", seen)
	}
}

type hookTool struct {
	inner     tool.Tool
	onExecute func()
}

func (h *hookTool) Info() tool.ToolInfo { return h.inner.Info() }

func (h *hookTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	h.onExecute()
	return h.inner.Execute(ctx, args)
}

func TestApprovalCallbackCannotMutateArgs(t *testing.T) {
	rw := &rwTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(rw); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{}, WithApproval(func(info tool.ToolInfo, args json.RawMessage) bool {
		// Mutate the received slice in place: only the bridge's own copy
		// protects the executed input.
		if len(args) > 4 {
			args[2] = 'X'
		}
		return true
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	res := runScript(t, wrapper, `await tools["rw"]({n:7}); return "ok"`)
	if res.IsError() {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.seen) != 1 || !strings.Contains(string(rw.seen[0]), `"n":7`) || strings.Contains(string(rw.seen[0]), "X") {
		t.Fatalf("executed args must be the originals, got %s", rw.seen)
	}
}

func TestSchemaChangeStale(t *testing.T) {
	rw := &rwTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(rw); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	res := runScript(t, wrapper, `try { await tools["rw"]({n:1}); return "ran"; } catch (e) { return "stale:" + e.kind; }`)
	if !strings.Contains(res.Content, "ran") {
		t.Fatalf("first call must run: %s", res.Content)
	}
	rw.swap(false, 1) // schema changed
	res2 := runScript(t, wrapper, `try { await tools["rw"]({n:2}); return "ran"; } catch (e) { return "stale:" + e.kind; }`)
	if !strings.Contains(res2.Content, "stale:catalog_stale") {
		t.Fatalf("schema change must yield catalog_stale, got: %s", res2.Content)
	}
}

type barrierTool struct {
	mu       sync.Mutex
	arrived  int
	release  chan struct{}
	once     sync.Once
	executed int
}

func (b *barrierTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	ps.Properties["i"] = tool.Property{Type: "number"}
	return tool.ToolInfo{Name: "bar", Parameters: ps}
}

func (b *barrierTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	b.mu.Lock()
	b.arrived++
	arrived := b.arrived
	b.mu.Unlock()
	if arrived == 2 {
		b.once.Do(func() { close(b.release) })
	}
	select {
	case <-b.release:
	case <-time.After(5 * time.Second):
		return tool.NewErrorResult("barrier timeout"), nil
	}
	b.mu.Lock()
	b.executed++
	b.mu.Unlock()
	return tool.NewTextResult("bar-ok"), nil
}

func TestPromiseAllRunsToolsConcurrently(t *testing.T) {
	bar := &barrierTool{release: make(chan struct{})}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(bar); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	res := runScript(t, wrapper, `const rs = await Promise.all([tools["bar"]({i:1}), tools["bar"]({i:2})]); return rs.length;`)
	if res.IsError() {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "2") {
		t.Fatalf("both promises must resolve: %s", res.Content)
	}
	bar.mu.Lock()
	defer bar.mu.Unlock()
	if bar.executed != 2 {
		t.Fatalf("both tools must execute, got %d", bar.executed)
	}
}

type slowTool struct {
	mu        sync.Mutex
	release   chan struct{}
	completed int
}

func (s *slowTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	return tool.ToolInfo{Name: "slow", Parameters: ps}
}

func (s *slowTool) Execute(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
		// deliberately ignores ctx for a while; completes late anyway
		<-s.release
	}
	s.mu.Lock()
	s.completed++
	s.mu.Unlock()
	return tool.NewTextResult("slow-ok"), nil
}

func TestLateCompletionAfterTimeout(t *testing.T) {
	slow := &slowTool{release: make(chan struct{})}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(slow); err != nil {
		t.Fatalf("register: %v", err)
	}
	var lateFinished int
	var mu sync.Mutex
	m, err := Connect(context.Background(), source, model,
		Config{WallClock: 150 * time.Millisecond, VMExitTimeout: 300 * time.Millisecond, HostDrainTimeout: 300 * time.Millisecond},
		WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }),
		WithCallObserver(func(ev CallEvent) {
			if ev.State == CallLateFinished {
				mu.Lock()
				lateFinished++
				mu.Unlock()
			}
		}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args, _ := json.Marshal(map[string]string{"code": `const r = await tools["slow"]({}); return r.content;`, "catalog_version": wrapper.gen.version})
	res, err := wrapper.Execute(ctx, args)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if !res.IsError() || !strings.Contains(res.Content, "timeout") {
		t.Fatalf("expected timeout soft error, got: %s", res.Content)
	}
	time.Sleep(100 * time.Millisecond)
	close(slow.release) // the tool completes late
	time.Sleep(200 * time.Millisecond)
	slow.mu.Lock()
	completed := slow.completed
	slow.mu.Unlock()
	if completed != 1 {
		t.Fatalf("late tool completion must still happen, completed=%d", completed)
	}
	mu.Lock()
	defer mu.Unlock()
	if lateFinished == 0 {
		t.Fatalf("late completion must be observable as metadata")
	}
}

type imageTool struct{ count int }

func (t *imageTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	return tool.ToolInfo{Name: "img", Parameters: ps}
}

func (t *imageTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	t.count++
	return &tool.ToolResult{
		Content: "rendered",
		Images: []tool.Image{
			{MIMEType: "image/png", Data: []byte("PNGDATA1")},
			{MIMEType: "image/png", Data: []byte("PNGDATA2")},
		},
	}, nil
}

func TestImageSelectionAndCaps(t *testing.T) {
	img := &imageTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(img); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)

	res := runScript(t, wrapper, `const r = await tools["img"]({}); return output("pic", r.images[0]);`)
	if res.IsError() {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if len(res.Images) != 1 || string(res.Images[0].Data) != "PNGDATA1" {
		t.Fatalf("exactly the selected image must be projected, got %+v", res.Images)
	}

	res2 := runScript(t, wrapper, `try { await tools["img"]({}); return "unhandled"; } catch (e) { return "caught:" + e.kind; }`)
	_ = res2

	// cross-execution refs fail projection
	res3 := runScript(t, wrapper, `const r = await tools["img"]({}); return output("pic", {ref: "bogus-token"});`)
	if !res3.IsError() {
		t.Fatalf("unknown image ref must be a soft error")
	}
	_ = m
}

func TestMaxImagesCap(t *testing.T) {
	img := &imageTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(img); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{MaxImages: 1}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	res := runScript(t, wrapper, `try { const r = await tools["img"]({}); return "got:" + String(r.value); } catch (e) { return "caught:" + e.kind; }`)
	if !strings.Contains(res.Content, "caught:bridge_result_limit") {
		t.Fatalf("over-cap images must fail the result conversion, got: %s", res.Content)
	}
}

func TestBlobApprovalThroughAgentLoop(t *testing.T) {
	apply := &applyTagTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(&listIssuesTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := source.Register(apply); err != nil {
		t.Fatalf("register: %v", err)
	}
	var outerCalls, nestedCalls int
	var mu sync.Mutex
	m, err := Connect(context.Background(), source, model, Config{},
		WithApproval(func(info tool.ToolInfo, _ json.RawMessage) bool {
			mu.Lock()
			nestedCalls++
			mu.Unlock()
			return info.Name == "applyTag"
		}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)

	code := `const r1 = await tools["listIssues"]({projectId: "P1"});
const open = r1.value.items.filter(i => i.status === "open").length;
if (open > 0) { await tools["applyTag"]({projectId: "P1", tag: "needs-triage"}); }
return [{projectId: "P1", open}];`
	callArgs, _ := json.Marshal(map[string]string{"code": code, "catalog_version": wrapper.gen.version})

	step1 := agenttest.Exchange{
		Method: agenttest.MethodChatStream,
		Request: agenttest.Request{
			Messages: []llm.Message{llm.SystemMessage("test"), llm.UserMessage("check P1 issues")},
			Tools:    []tool.ToolInfo{wrapper.Info()},
			Options:  llm.ApplyOptions(nil),
		},
		StreamChunks: []llm.Chunk{
			&llm.ToolCallStartChunk{Index: 0, ID: "call-1", Name: m.name},
			&llm.ToolCallArgsChunk{Index: 0, ID: "call-1", Delta: string(callArgs)},
			&llm.DoneChunk{},
		},
	}
	step2 := agenttest.Exchange{
		Method: agenttest.MethodChatStream,
		Request: agenttest.Request{
			Messages: []llm.Message{
				llm.SystemMessage("test"),
				llm.UserMessage("check P1 issues"),
				llm.AssistantToolCallMessage(&llm.ToolUseBlock{Type: "tool_use", ID: "call-1", Name: m.name, Input: callArgs}),
				llm.ToolResultMessage("call-1", &tool.ToolResult{Content: `[{"projectId":"P1","open":1}]`}),
			},
			Tools:   []tool.ToolInfo{wrapper.Info()},
			Options: llm.ApplyOptions(nil),
		},
		StreamChunks: []llm.Chunk{
			&llm.TextDeltaChunk{Text: "done"},
			&llm.DoneChunk{},
		},
	}
	provider := agenttest.NewScriptedProvider(step1, step2)

	a := agent.New(provider, model,
		agent.WithSystemPrompt("test"),
		agent.WithMaxIter(4),
		agent.WithApprovalFn(func(info tool.ToolInfo, _ json.RawMessage) bool {
			mu.Lock()
			outerCalls++
			mu.Unlock()
			return true
		}),
	)
	runRes, err := a.Run(context.Background(), "check P1 issues")
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}
	if err := provider.Verify(); err != nil {
		t.Fatalf("script not fully consumed: %v", err)
	}
	mu.Lock()
	outer, nested := outerCalls, nestedCalls
	mu.Unlock()
	if outer != 1 {
		t.Fatalf("outer run_code approval must happen exactly once, got %d", outer)
	}
	if nested != 1 {
		t.Fatalf("nested applyTag approval must happen exactly once, got %d", nested)
	}
	historyJSON, _ := json.Marshal(runRes.History)
	history := string(historyJSON)
	if !strings.Contains(history, `\"open\":1`) {
		t.Fatalf("history must contain the run_code summary result")
	}
	if strings.Contains(history, "found issues") {
		t.Fatalf("intermediate tool content must never enter history")
	}
	if apply.calls != 1 {
		t.Fatalf("applyTag must execute once, got %d", apply.calls)
	}
}

func TestCloseRespectsUserReplacement(t *testing.T) {
	apply := &applyTagTool{}
	m, model, _ := setupPair(t, apply)
	replacement := &genericTool{name: m.name, params: tool.NewParameterSchema()}
	if err := model.Replace(replacement); err != nil {
		t.Fatalf("user replace: %v", err)
	}
	m.Close()
	current, ok := model.Get(m.name)
	if !ok || current != tool.Tool(replacement) {
		t.Fatalf("Close must not unregister a user replacement")
	}
}

func TestObserverSequence(t *testing.T) {
	apply := &applyTagTool{}
	var events []CallEvent
	var mu sync.Mutex
	_, _, wrapper := setupPair(t, apply, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }), WithCallObserver(func(ev CallEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	res := runScript(t, wrapper, `await tools["applyTag"]({projectId: "P1", tag: "t"}); return "ok"`)
	if res.IsError() {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	var states []CallState
	lastSeq := uint64(0)
	for _, ev := range events {
		if ev.Seq <= lastSeq && len(states) > 0 {
			t.Fatalf("event sequence must be monotonic")
		}
		lastSeq = ev.Seq
		if ev.Tool == "applyTag" {
			states = append(states, ev.State)
		}
	}
	want := []CallState{CallQueued, CallApproving, CallStarted, CallFinished}
	if len(states) != len(want) {
		t.Fatalf("applyTag states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("applyTag states = %v, want %v", states, want)
		}
	}
}
