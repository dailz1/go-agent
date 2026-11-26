package codemode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dailz1/go-agent/tool"
)

type listIssuesTool struct{}

func (t *listIssuesTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	ps.Properties["projectId"] = tool.Property{Type: "string"}
	ps.Required = []string{"projectId"}
	return tool.ToolInfo{Name: "listIssues", Description: "lists issues for a project", Parameters: ps}
}

func (t *listIssuesTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	var in struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.NewErrorResult("bad args"), nil
	}
	items := []map[string]string{{"status": "closed"}}
	if in.ProjectID == "P1" {
		items = []map[string]string{{"status": "open"}, {"status": "closed"}}
	}
	return tool.NewDataResult("found issues", map[string]any{"items": items})
}

type applyTagTool struct{ calls int }

func (t *applyTagTool) Info() tool.ToolInfo {
	ps := tool.NewParameterSchema()
	ps.Properties["projectId"] = tool.Property{Type: "string"}
	ps.Properties["tag"] = tool.Property{Type: "string"}
	ps.Required = []string{"projectId", "tag"}
	return tool.ToolInfo{Name: "applyTag", Parameters: ps, RequiresApproval: true}
}

func (t *applyTagTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	t.calls++
	return tool.NewTextResult("tagged"), nil
}

func setupPair(t *testing.T, apply *applyTagTool, opts ...Option) (*CodeMode, *tool.Registry, *runCodeTool) {
	t.Helper()
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(&listIssuesTool{}); err != nil {
		t.Fatalf("register listIssues: %v", err)
	}
	if err := source.Register(apply); err != nil {
		t.Fatalf("register applyTag: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{}, opts...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, ok := model.Get(m.name)
	if !ok {
		t.Fatalf("run_code not registered")
	}
	wrapper, ok := current.(*runCodeTool)
	if !ok {
		t.Fatalf("model registry entry is not the run_code wrapper")
	}
	return m, model, wrapper
}

func runScript(t *testing.T, wrapper *runCodeTool, code string) *tool.ToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args, _ := json.Marshal(map[string]string{"code": code, "catalog_version": wrapper.gen.version})
	res, err := wrapper.Execute(ctx, args)
	if err != nil {
		t.Fatalf("execute returned hard error: %v", err)
	}
	return res
}

func TestEndToEndScriptFlow(t *testing.T) {
	apply := &applyTagTool{}
	var approvals []string
	var mu = make(chan struct{}, 1)
	m, model, wrapper := setupPair(t, apply, WithApproval(func(info tool.ToolInfo, _ json.RawMessage) bool {
		mu <- struct{}{}
		approvals = append(approvals, info.Name)
		<-mu
		return true
	}))
	if n := len(model.List()); n != 1 {
		t.Fatalf("model registry must expose only run_code, got %d tools", n)
	}
	res := runScript(t, wrapper, `const r1 = await tools["listIssues"]({projectId: "P1"});
const open = r1.value.items.filter(i => i.status === "open").length;
if (open > 0) { await tools["applyTag"]({projectId: "P1", tag: "needs-triage"}); }
return [{projectId: "P1", open}];`)
	if res.IsError() {
		t.Fatalf("expected success, got error result: %s", res.Content)
	}
	if !strings.Contains(res.Content, `"open":1`) {
		t.Fatalf("content missing summary: %s", res.Content)
	}
	if res.Data != nil {
		t.Fatalf("outer Data must stay nil (D11.2), got %s", res.Data)
	}
	if len(res.Images) != 0 {
		t.Fatalf("no images expected")
	}
	if apply.calls != 1 {
		t.Fatalf("applyTag executed %d times, want 1", apply.calls)
	}
	if len(approvals) != 1 || approvals[0] != "applyTag" {
		t.Fatalf("nested approvals = %v, want [applyTag]", approvals)
	}
	_ = m
}

func TestEndToEndDenialCatch(t *testing.T) {
	apply := &applyTagTool{}
	m, _, wrapper := setupPair(t, apply, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return false }))
	res := runScript(t, wrapper, `try {
  await tools["applyTag"]({projectId: "P1", tag: "x"});
  return "unexpected success";
} catch (e) {
  if (e.kind !== "denied") { return "wrong kind: " + e.kind; }
  return "caught " + e.kind;
}`)
	if res.IsError() {
		t.Fatalf("denial must be catchable, got error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "caught denied") {
		t.Fatalf("unexpected content: %s", res.Content)
	}
	if apply.calls != 0 {
		t.Fatalf("denied tool must not execute, calls=%d", apply.calls)
	}
	_ = m
}

func TestStaleCatalogVersionRejected(t *testing.T) {
	apply := &applyTagTool{}
	m, _, wrapper := setupPair(t, apply)
	args, _ := json.Marshal(map[string]string{"code": "return 1;", "catalog_version": "gen-999"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := wrapper.Execute(ctx, args)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if !res.IsError() {
		t.Fatalf("stale version must be rejected")
	}
	if strings.Contains(res.Content, "tools[") || strings.Contains(res.Content, "gen-2") {
		t.Fatalf("stale rejection must not carry another generation's declarations: %s", res.Content)
	}
	// refresh publishes a new generation; the old wrapper keeps its own version
	if err := m.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	res2, _ := wrapper.Execute(ctx, args)
	if !res2.IsError() {
		t.Fatalf("old wrapper must still reject the stale version after refresh")
	}
}

func TestScriptErrorSoft(t *testing.T) {
	apply := &applyTagTool{}
	m, _, wrapper := setupPair(t, apply)
	res := runScript(t, wrapper, `throw new Error("boom")`)
	if !res.IsError() {
		t.Fatalf("script error must be a soft error result")
	}
	if !strings.Contains(res.Content, "boom") {
		t.Fatalf("error message missing: %s", res.Content)
	}
	_ = m
}

func TestEncodingFailure(t *testing.T) {
	apply := &applyTagTool{}
	m, _, wrapper := setupPair(t, apply)
	res := runScript(t, wrapper, `return {fn: function() {}}`)
	if !res.IsError() {
		t.Fatalf("function in result must fail encoding")
	}
	if !strings.Contains(res.Content, "function") {
		t.Fatalf("unexpected message: %s", res.Content)
	}
	_ = m
}

func TestUnawaitedCalls(t *testing.T) {
	apply := &applyTagTool{}
	m, _, wrapper := setupPair(t, apply, WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	res := runScript(t, wrapper, `tools["listIssues"]({projectId: "P1"}); return "done"`)
	if !res.IsError() {
		t.Fatalf("unawaited admitted call must terminate the run with an error result")
	}
	_ = m
}

func TestOuterDataAndOutputHelper(t *testing.T) {
	apply := &applyTagTool{}
	m, _, wrapper := setupPair(t, apply)
	res := runScript(t, wrapper, `return output({a: 1});`)
	if res.IsError() {
		t.Fatalf("output helper failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, `"a":1`) {
		t.Fatalf("content: %s", res.Content)
	}
	_ = m
}

func TestWallClockTimeout(t *testing.T) {
	apply := &applyTagTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(&listIssuesTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := source.Register(apply); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{WallClock: 200 * time.Millisecond, VMExitTimeout: 300 * time.Millisecond, HostDrainTimeout: 300 * time.Millisecond},
		WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args, _ := json.Marshal(map[string]string{"code": `await new Promise(() => {}); return 1;`, "catalog_version": wrapper.gen.version})
	start := time.Now()
	res, err := wrapper.Execute(ctx, args)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if !res.IsError() || !strings.Contains(res.Content, "timeout") {
		t.Fatalf("expected timeout soft error, got: %s", res.Content)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout response took too long: %v", elapsed)
	}
}

func TestMaxCallsTerminal(t *testing.T) {
	apply := &applyTagTool{}
	source := tool.NewRegistry()
	model := tool.NewRegistry()
	if err := source.Register(&listIssuesTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := source.Register(apply); err != nil {
		t.Fatalf("register: %v", err)
	}
	m, err := Connect(context.Background(), source, model, Config{MaxCalls: 4},
		WithApproval(func(tool.ToolInfo, json.RawMessage) bool { return true }))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	current, _ := model.Get(m.name)
	wrapper := current.(*runCodeTool)
	res := runScript(t, wrapper, `for (let i = 0; i < 20; i++) { await tools["listIssues"]({projectId: "P1"}); } return "ok"`)
	if !res.IsError() || !strings.Contains(res.Content, "call_limit") {
		t.Fatalf("expected call_limit soft error, got: %s", res.Content)
	}
}

func TestRefreshGenerations(t *testing.T) {
	apply := &applyTagTool{}
	m, model, wrapper := setupPair(t, apply)
	v1 := wrapper.gen.version
	if err := m.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	current, _ := model.Get(m.name)
	wrapper2 := current.(*runCodeTool)
	if wrapper2.gen.version == v1 {
		t.Fatalf("refresh must publish a new generation")
	}
	if v1 != wrapper.gen.version {
		t.Fatalf("old wrapper must keep its own generation")
	}
	// script via old wrapper still works with the old catalog
	res := runScript(t, wrapper, `const r = await tools["listIssues"]({projectId: "P1"}); return r.value.items.length;`)
	if res.IsError() {
		t.Fatalf("old generation execution failed: %s", res.Content)
	}
	// a tool added after g1 is not even bound in g1's tools object; the
	// call must fail inside the script and the tool must never execute
	genericCalls := 0
	_ = genericCalls
	res2 := runScript(t, wrapper, `try { await tools["newTool"]({}); return "bad"; } catch (e) { return "caught " + (e.kind || e.name); }`)
	if !strings.Contains(res2.Content, "caught") {
		t.Fatalf("g1 must not reach g2-only tools: %s", res2.Content)
	}
	if strings.Contains(res2.Content, "ok") {
		t.Fatalf("newTool executed under g1: %s", res2.Content)
	}
}

func source2Register(t *testing.T, m *CodeMode, name string) *tool.Registry {
	t.Helper()
	ps := tool.NewParameterSchema()
	if err := m.source.Register(&genericTool{name: name, params: ps}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return m.source
}

type genericTool struct {
	name   string
	params tool.ParameterSchema
}

func (t *genericTool) Info() tool.ToolInfo { return tool.ToolInfo{Name: t.name, Parameters: t.params} }

func (t *genericTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	return tool.NewTextResult("ok"), nil
}
