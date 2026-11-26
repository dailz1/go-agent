package codemode

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dailz1/go-agent/tool"
)

func TestNullReturnEncodes(t *testing.T) {
	apply := &applyTagTool{}
	_, _, wrapper := setupPair(t, apply)
	res := runScript(t, wrapper, `return null;`)
	if res.IsError() {
		t.Fatalf("null must encode as JSON null, got error: %s", res.Content)
	}
	if strings.TrimSpace(res.Content) != "null" {
		t.Fatalf("want null, got %q", res.Content)
	}
}

func TestGetterThrowIsSoft(t *testing.T) {
	apply := &applyTagTool{}
	_, _, wrapper := setupPair(t, apply)
	res := runScript(t, wrapper, `return {get x(){ throw new Error("getter boom"); }};`)
	if !res.IsError() {
		t.Fatalf("throwing getter must be a soft failure")
	}
}

func TestSymbolAndBigIntRejected(t *testing.T) {
	apply := &applyTagTool{}
	_, _, wrapper := setupPair(t, apply)
	res := runScript(t, wrapper, `return Symbol("x");`)
	if !res.IsError() || !strings.Contains(res.Content, "Symbol") {
		t.Fatalf("Symbol must fail result_encoding, got: %s", res.Content)
	}
	res2 := runScript(t, wrapper, `return 1n;`)
	if !res2.IsError() || !strings.Contains(res2.Content, "BigInt") {
		t.Fatalf("BigInt must fail result_encoding, got: %s", res2.Content)
	}
}

func TestSparseArrayFaithful(t *testing.T) {
	apply := &applyTagTool{}
	_, _, wrapper := setupPair(t, apply)
	res := runScript(t, wrapper, `return [,,1];`)
	if res.IsError() {
		t.Fatalf("sparse array must encode, got: %s", res.Content)
	}
	if strings.TrimSpace(res.Content) != "[null,null,1]" {
		t.Fatalf("holes must become null in place, got %q", res.Content)
	}
}

type dataTool struct {
	block    chan struct{}
	data     []byte
	hardLate bool
}

func (t *dataTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	return tool.ToolInfo{Name: "d", Parameters: ps}
}

func (t *dataTool) Execute(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	select {
	case <-t.block:
	case <-ctx.Done():
		<-t.block
	}
	if t.hardLate {
		return nil, context.DeadlineExceeded
	}
	return tool.NewTextResult(string(t.data)), nil
}

func TestDrainWindowHardErrorStaysSoft(t *testing.T) {
	// R2 regression: the timeout selects the terminal cause first; a hard
	// Go error arriving during the drain window is diagnostics only — the
	// outer result stays the timeout soft error and never propagates.
	dt := &dataTool{block: make(chan struct{}), hardLate: true}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(dt); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model,
		Config{WallClock: 150 * time.Millisecond, VMExitTimeout: 300 * time.Millisecond, HostDrainTimeout: 300 * time.Millisecond},
		WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args, _ := json.Marshal(map[string]string{"code": `const r = await tools["d"]({}); return r.content;`, "catalog_version": wrapper.gen.version})
	res, herr := wrapper.Execute(ctx, args)
	if herr != nil {
		t.Fatalf("late hard error must not propagate after the cause was selected")
	}
	if !res.IsError() || !strings.Contains(res.Content, "timeout") {
		t.Fatalf("expected timeout soft error, got: %s", res.Content)
	}
	close(dt.block) // tool now returns its Go error inside the drain window
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && m.Snapshot().LiveRuns > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if snap := m.Snapshot(); snap.LiveRuns != 0 {
		t.Fatalf("ledger slot must release after real cleanup, got %+v", snap)
	}
}

func TestTimeoutWithLateDataOverrunKeepsCause(t *testing.T) {
	// F2 regression: a late completion that trips the cumulative budget
	// after the timeout already stopped the run must not re-close stopCh.
	block := make(chan struct{})
	dt := &dataTool{block: block, data: []byte("ok")}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(dt); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model,
		Config{WallClock: 100 * time.Millisecond, VMExitTimeout: 200 * time.Millisecond, HostDrainTimeout: 200 * time.Millisecond, HostDataBytes: 1},
		WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args, _ := json.Marshal(map[string]string{"code": `const r = await tools["d"]({}); return r.content;`, "catalog_version": wrapper.gen.version})
	res, herr := wrapper.Execute(ctx, args)
	if herr != nil {
		t.Fatalf("hard error: %v", err)
	}
	if !res.IsError() || !strings.Contains(res.Content, "timeout") {
		t.Fatalf("expected timeout cause, got: %s", res.Content)
	}
	close(block)
	time.Sleep(100 * time.Millisecond) // late completion exercises stopLocked idempotency
}

func TestHardErrorDuringDrainKeepsLedgerAndPropagates(t *testing.T) {
	// F3/F5 regression: a hard error while another tool is still blocked
	// must propagate as a Go error AND keep the ledger slot until the
	// blocked tool really exits.
	release := make(chan struct{})
	var hardCalls int
	a := &hardTool{onExecute: func() { hardCalls++ }}
	b := &blockingTool{release: release}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := source.Register(b); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args, _ := json.Marshal(map[string]string{
		"code":            `await Promise.all([tools["hard"]({}), tools["block"]({})]); return "ok";`,
		"catalog_version": wrapper.gen.version,
	})
	_, herr := wrapper.Execute(ctx, args)
	if herr == nil {
		t.Fatalf("first hard error must propagate as a Go error")
	}
	if snap := m.Snapshot(); snap.LiveRuns != 1 {
		t.Fatalf("ledger slot must stay while the blocked tool runs, got %+v", snap)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.Snapshot().LiveRuns == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap := m.Snapshot(); snap.LiveRuns != 0 {
		t.Fatalf("ledger slot must release after real cleanup, got %+v", snap)
	}
	if hardCalls != 1 {
		t.Fatalf("hard tool executed %d times", hardCalls)
	}
}

type hardTool struct {
	onExecute func()
}

func (t *hardTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	return tool.ToolInfo{Name: "hard", Parameters: ps}
}

func (t *hardTool) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	t.onExecute()
	return nil, context.DeadlineExceeded
}

type blockingTool struct {
	release chan struct{}
}

func (t *blockingTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	return tool.ToolInfo{Name: "block", Parameters: ps}
}

func (t *blockingTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	<-t.release
	return tool.NewTextResult("block-ok"), nil
}

func TestGetterSideEffectAfterSettleRejected(t *testing.T) {
	// F7 regression: terminal-value encoding runs getters, and getters must
	// not be able to admit new side effects after the script settled.
	writes := 0
	write := &counterTool{name: "w", onCall: func() { writes++ }}
	_, _, wrapper := setupPair(t, &applyTagTool{}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	_ = write
	source := wrapper.mode.source
	if err := source.Register(write); err != nil {
		t.Fatalf("register w: %v", err)
	}
	if err := wrapper.mode.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	current, _ := wrapper.mode.exposed.Get(wrapper.mode.name)
	wrapper2 := current.(*runCodeTool)
	res := runScript(t, wrapper2, `return {get x(){ tools["w"]({}); return 1; }};`)
	if res.IsError() {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if writes != 0 {
		t.Fatalf("getter must not admit side effects after settle, writes=%d", writes)
	}
}

type counterTool struct {
	name   string
	onCall func()
}

func (t *counterTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	return tool.ToolInfo{Name: t.name, Parameters: ps}
}

func (t *counterTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	t.onCall()
	return tool.NewTextResult("ok"), nil
}

func TestCumulativeImageCap(t *testing.T) {
	// F12 regression: MaxImages caps the accepted images of one execution:
	// two calls of two images each with a cap of two fail on the second.
	img := &imageTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(img); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{MaxImages: 2}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	res := runScript(t, wrapper, `try { await tools["img"]({}); const r = await tools["img"]({}); return "second:" + r.images.length; } catch (e) { return "caught:" + e.kind; }`)
	if !strings.Contains(res.Content, "caught:bridge_result_limit") {
		t.Fatalf("second call must fail on the cumulative cap, got: %s", res.Content)
	}
}

func TestRepeatedImageOmissionWithinBudget(t *testing.T) {
	// F13 regression: repeated refs of an omitted image must not multiply
	// markers past OutputBytes.
	img := &imageTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(img); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{OutputBytes: 400}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	res := runScript(t, wrapper, `const r = await tools["img"]({});
return output("x", r.images[0], r.images[0], r.images[0], r.images[0], r.images[0], r.images[0], r.images[0], r.images[0]);`)
	if len(res.Content) > 400 {
		t.Fatalf("content %d bytes must stay within OutputBytes 400", len(res.Content))
	}
	if !strings.Contains(res.Content, "image omitted") {
		t.Fatalf("omission must be visible, got: %q", res.Content)
	}
}

func TestWrapperNotAdvertisedToScripts(t *testing.T) {
	// F14 regression: a second CodeMode's wrapper registered into the source
	// registry is never exposed to scripts.
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	source2 := tool.NewRegistry()
	model2 := tool.NewRegistry()
	if err := source2.Register(&listIssuesTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	mode2, err := Connect(context.Background(), source2, model2, Config{}, WithToolName("code2"))
	if err != nil {
		t.Fatalf("connect mode2: %v", err)
	}
	t.Cleanup(mode2.Close)
	wrapper2, _ := model2.Get("code2")
	if err := source.Register(wrapper2); err != nil {
		t.Fatalf("register wrapper into source: %v", err)
	}
	if err := source.Register(&listIssuesTool{}); err != nil {
		t.Fatalf("register listIssues: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{}, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	res := runScript(t, wrapper, `try { return typeof tools["code2"]; } catch (e) { return "caught"; }`)
	if !strings.Contains(res.Content, "undefined") {
		t.Fatalf("wrapper must not be exposed to scripts: %s", res.Content)
	}
}

func TestRecursiveCallRejected(t *testing.T) {
	// D7: a ctx-forwarding alias in the source registry that re-enters
	// run_code is rejected by the nested-execution ctx marker.
	apply := &applyTagTool{}
	m, _, wrapper := setupPair(t, apply)
	alias := &aliasTool{inner: wrapper, name: "alias_code"}
	if err := m.source.Register(alias); err != nil {
		t.Fatalf("register alias: %v", err)
	}
	if err := m.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	current, _ := m.exposed.Get(m.name)
	wrapper2 := current.(*runCodeTool)
	res := runScript(t, wrapper2, `try { await tools["alias_code"]({code: "return 1;", catalog_version: "x"}); return "reentered"; } catch (e) { return "caught:" + e.message; }`)
	if !strings.Contains(res.Content, "caught:recursive") {
		t.Fatalf("alias re-entry must be rejected, got: %s", res.Content)
	}
}

type aliasTool struct {
	inner tool.Tool
	name  string
	calls int
}

func (t *aliasTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	allowExtra := true
	ps.AdditionalProperties = &allowExtra
	return tool.ToolInfo{Name: t.name, Parameters: ps}
}

func (t *aliasTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	t.calls++
	return t.inner.Execute(ctx, args)
}

func TestConcurrentCloseAndExecuteNoPanic(t *testing.T) {
	// F4 regression: Close concurrent with in-flight executions and their
	// emits must never send on the closed observer queue (run under -race).
	for i := range 5 {
		apply := &applyTagTool{}
		source := tool.NewRegistry()
		model := tool.NewRegistry()
		if err := source.Register(&listIssuesTool{}); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := source.Register(apply); err != nil {
			t.Fatalf("register: %v", err)
		}
		m, err := Connect(context.Background(), source, model, Config{},
			WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }),
			WithCallObserver(func(CallEvent) {}))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		current, _ := model.Get(m.name)
		wrapper := current.(*runCodeTool)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			args, _ := json.Marshal(map[string]string{"code": `const r = await tools["listIssues"]({projectId: "P1"}); return r.value.items.length;`, "catalog_version": wrapper.gen.version})
			_, _ = wrapper.Execute(ctx, args)
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 2 * time.Millisecond)
			m.Close()
		}()
		wg.Wait()
	}
}
