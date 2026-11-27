package toolsearch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

type stubTool struct {
	info tool.ToolInfo
	run  func(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error)
}

func (t *stubTool) Info() tool.ToolInfo { return t.info }

func (t *stubTool) Execute(ctx context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	if t.run != nil {
		return t.run(ctx, args)
	}
	return tool.NewTextResult("ran " + t.info.Name), nil
}

func newStub(name, description string) *stubTool {
	return &stubTool{info: tool.ToolInfo{
		Name:        name,
		Description: description,
		Parameters:  tool.NewParameterSchema(),
	}}
}

func connectTest(t *testing.T, opts ...Option) (*Search, *tool.Registry, tool.Tool) {
	t.Helper()
	source := tool.NewRegistry()
	exposed := tool.NewRegistry()
	s, err := Connect(context.Background(), source, exposed, opts...)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, ok := exposed.Get(s.Name())
	if !ok {
		t.Fatalf("search tool %q not registered", s.Name())
	}
	return s, exposed, st
}

func callSearch(t *testing.T, st tool.Tool, args string) *tool.ToolResult {
	t.Helper()
	result, err := st.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("search Execute returned Go error: %v", err)
	}
	return result
}

func decodeOutput(t *testing.T, result *tool.ToolResult) searchOutput {
	t.Helper()
	var out searchOutput
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("search result is not the expected JSON: %v\n%s", err, result.Content)
	}
	return out
}

func TestTokenizeRules(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Fill PDF forms", []string{"fill", "pdf", "forms"}},
		{"pdf_form,handler", []string{"pdf_form,handler"}},
		{"中文搜索", []string{"中文", "文搜", "搜索"}},
		{"pdf表单", []string{"pdf", "表单"}},
		{"中", nil},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{"mixed中文text", []string{"mixed", "中文", "text"}},
	}
	for _, tc := range cases {
		if got := tokenize(tc.in); !equalStrings(got, tc.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestRankToolsOrderAndPositivity(t *testing.T) {
	infos := []tool.ToolInfo{
		{Name: "bbb", Description: "pdf form handler", Parameters: tool.NewParameterSchema()},
		{Name: "aaa", Description: "pdf form handler", Parameters: tool.NewParameterSchema()},
		{Name: "unrelated", Description: "weather lookup", Parameters: tool.NewParameterSchema()},
	}
	ranked := rankTools(infos, tokenize("pdf form"))
	if len(ranked) != 2 {
		t.Fatalf("ranked = %d entries, want 2 (positive matches only)", len(ranked))
	}
	if ranked[0].name != "aaa" || ranked[1].name != "bbb" {
		t.Fatalf("tie order = [%s %s], want aaa before bbb", ranked[0].name, ranked[1].name)
	}
	if ranked[0].score != ranked[1].score {
		t.Fatalf("identical documents must score identically: %v vs %v", ranked[0].score, ranked[1].score)
	}
	if rankTools(nil, tokenize("pdf")) != nil {
		t.Fatal("empty catalog must return no matches")
	}
	if rankTools(infos, nil) != nil {
		t.Fatal("empty query must return no matches")
	}
}

func TestConnectValidationRejectsAndLeavesExposedUnchanged(t *testing.T) {
	bigDescription := strings.Repeat("word ", 40)
	cases := []struct {
		label string
		opts  []Option
		setup func(t *testing.T) (*tool.Registry, *tool.Registry)
	}{
		{"limit zero", []Option{WithLimit(0)}, func(t *testing.T) (*tool.Registry, *tool.Registry) {
			return tool.NewRegistry(), tool.NewRegistry()
		}},
		{"limit over cap", []Option{WithLimit(101)}, func(t *testing.T) (*tool.Registry, *tool.Registry) {
			return tool.NewRegistry(), tool.NewRegistry()
		}},
		{"max loaded zero", []Option{WithMaxLoaded(0)}, func(t *testing.T) (*tool.Registry, *tool.Registry) {
			return tool.NewRegistry(), tool.NewRegistry()
		}},
		{"catalog limit negative", []Option{WithCatalogLimit(-1)}, func(t *testing.T) (*tool.Registry, *tool.Registry) {
			return tool.NewRegistry(), tool.NewRegistry()
		}},
		{"invalid tool name", []Option{WithToolName("bad name!")}, func(t *testing.T) (*tool.Registry, *tool.Registry) {
			return tool.NewRegistry(), tool.NewRegistry()
		}},
		{"catalog over budget at connect", nil, func(t *testing.T) (*tool.Registry, *tool.Registry) {
			source := tool.NewRegistry()
			source.MustRegister(newStub("big", bigDescription))
			return source, tool.NewRegistry()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			opts := tc.opts
			if tc.opts == nil {
				opts = []Option{WithCatalogLimit(10)}
			}
			source, exposed := tc.setup(t)
			if _, err := Connect(context.Background(), source, exposed, opts...); err == nil {
				t.Fatal("Connect succeeded, want configuration error")
			}
			if len(exposed.List()) != 0 {
				t.Fatalf("exposed changed on failed Connect: %v", exposed.List())
			}
		})
	}
}

func TestConnectRejectsNilSameAndNameConflicts(t *testing.T) {
	source := tool.NewRegistry()
	if _, err := Connect(context.Background(), nil, tool.NewRegistry()); err == nil {
		t.Fatal("nil source accepted")
	}
	if _, err := Connect(context.Background(), source, nil); err == nil {
		t.Fatal("nil exposed accepted")
	}
	if _, err := Connect(context.Background(), source, source); err == nil {
		t.Fatal("same registry accepted for source and exposed")
	}
	occupied := tool.NewRegistry()
	occupied.MustRegister(newStub(DefaultToolName, "host tool"))
	if _, err := Connect(context.Background(), tool.NewRegistry(), occupied); err == nil {
		t.Fatal("exposed already holding the search name accepted")
	}
	pool := tool.NewRegistry()
	pool.MustRegister(newStub(DefaultToolName, "impostor"))
	if _, err := Connect(context.Background(), pool, tool.NewRegistry()); err == nil {
		t.Fatal("source tool named like the search tool accepted")
	}
	overlapSource := tool.NewRegistry()
	overlapSource.MustRegister(newStub("clash", "clash"))
	overlapExposed := tool.NewRegistry()
	overlapExposed.MustRegister(newStub("clash", "host copy"))
	if _, err := Connect(context.Background(), overlapSource, overlapExposed); err == nil {
		t.Fatal("source name overlapping exposed accepted")
	}
}

func TestSearchEmptyPoolAndNoMatch(t *testing.T) {
	s, _, st := connectTest(t)
	result := callSearch(t, st, `{"query":"anything"}`)
	out := decodeOutput(t, result)
	if result.IsError() || len(out.Matches) != 0 {
		t.Fatalf("empty pool search = %+v IsError=%v, want empty success", out, result.IsError())
	}
	if out.Message != "No deferred tools are available." {
		t.Fatalf("message = %q", out.Message)
	}

	s.source.MustRegister(newStub("unrelated", "weather lookup"))
	result = callSearch(t, st, `{"query":"pdf"}`)
	out = decodeOutput(t, result)
	if result.IsError() || len(out.Matches) != 0 {
		t.Fatalf("no-match search = %+v IsError=%v, want empty success", out, result.IsError())
	}
	if out.Message != "No matching tools; try different task keywords." {
		t.Fatalf("message = %q", out.Message)
	}
}

func TestSearchInvalidInput(t *testing.T) {
	_, _, st := connectTest(t)
	bad := []string{
		`{"query":"   "}`,
		`{"query":""}`,
		`{"query":null}`,
		`not json`,
		`{"query":"ok","extra":1}`,
		`{"Query":"ok"}`,
		`{"query":"ok","mode":"regex","mode":"bm25"}`,
		`{"query":"ok","limit":"2"}`,
		`{"query":"ok","limit":1e999999}`,
		`{"query":"ok","limit":null}`,
		`{"query":"ok","mode":null}`,
		`{"query":"ok","mode":""}`,
		`{"query":"ok","mode":"regex"}`,
		`{"query":"ok","limit":0}`,
		`{"query":"ok","limit":6}`,
		`{"query":"ok"} trailing`,
		`{"query":"ok"} }`,
		`{"query":"ok"}]`,
		`{"query":"中"}`,
	}
	for _, args := range bad {
		result := callSearch(t, st, args)
		if !result.IsError() {
			t.Errorf("args %s succeeded, want soft error", args)
		}
	}
	long := `{"query":"` + strings.Repeat("a", 1025) + `"}`
	if !callSearch(t, st, long).IsError() {
		t.Error("over-long query succeeded")
	}
	result := callSearch(t, st, `{"query":"ok","limit":3,"mode":"bm25"}`)
	if result.IsError() {
		t.Fatalf("valid args rejected: %s", result.Content)
	}
}

func TestSearchLoadsAndDedups(t *testing.T) {
	s, exposed, st := connectTest(t)
	s.source.MustRegister(newStub("pdf_fill", "Fill PDF forms"))
	s.source.MustRegister(newStub("other", "weather lookup"))

	result := callSearch(t, st, `{"query":"pdf form"}`)
	out := decodeOutput(t, result)
	if result.IsError() || len(out.Matches) != 1 {
		t.Fatalf("matches = %+v, want one", out.Matches)
	}
	first := out.Matches[0]
	if first.Name != "pdf_fill" || !first.NewlyLoaded || first.Description != "Fill PDF forms" {
		t.Fatalf("first match = %+v", first)
	}
	if _, ok := exposed.Get("pdf_fill"); !ok {
		t.Fatal("wrapper not published to exposed")
	}

	result = callSearch(t, st, `{"query":"pdf form"}`)
	out = decodeOutput(t, result)
	if len(out.Matches) != 1 || out.Matches[0].NewlyLoaded {
		t.Fatalf("re-search match = %+v, want NewlyLoaded=false", out.Matches)
	}
	infos := exposed.List()
	if len(infos) != 2 {
		t.Fatalf("exposed has %d tools, want search + one wrapper", len(infos))
	}
}

func TestSearchLimitBoundsPerCall(t *testing.T) {
	s, _, st := connectTest(t)
	for i := 0; i < 3; i++ {
		s.source.MustRegister(newStub(string(rune('a'+i))+"_pdf", "pdf tool"))
	}
	result := callSearch(t, st, `{"query":"pdf","limit":2}`)
	out := decodeOutput(t, result)
	if len(out.Matches) != 2 {
		t.Fatalf("limit=2 returned %d matches", len(out.Matches))
	}
	if result := callSearch(t, st, `{"query":"pdf","limit":1.0}`); result.IsError() {
		t.Fatalf("JSON-number integer form 1.0 rejected: %s", result.Content)
	}
	if result := callSearch(t, st, `{"query":"pdf","limit":1.5}`); !result.IsError() {
		t.Fatal("non-integer 1.5 accepted")
	}
}

func TestSearchCatalogBudgetSoftFailsWithZeroChange(t *testing.T) {
	s, exposed, st := connectTest(t, WithCatalogLimit(50))
	s.source.MustRegister(newStub("big", strings.Repeat("word ", 30)))
	before := exposed.List()
	result := callSearch(t, st, `{"query":"word"}`)
	if !result.IsError() {
		t.Fatal("over-budget catalog search succeeded")
	}
	if !sameInfos(exposed.List(), before) {
		t.Fatal("exposed changed on budget failure")
	}
}

func TestSearchMaxLoadedCap(t *testing.T) {
	s, exposed, st := connectTest(t, WithMaxLoaded(1))
	alpha := newStub("alpha", "alpha tool")
	beta := newStub("beta", "beta tool")
	s.source.MustRegister(alpha)
	s.source.MustRegister(beta)

	if result := callSearch(t, st, `{"query":"alpha"}`); result.IsError() {
		t.Fatalf("first load failed: %s", result.Content)
	}
	result := callSearch(t, st, `{"query":"beta"}`)
	if !result.IsError() || !strings.Contains(result.Content, "MaxLoaded") {
		t.Fatalf("second load = %v %q, want MaxLoaded soft error", result.IsError(), result.Content)
	}
	if _, ok := exposed.Get("beta"); ok {
		t.Fatal("beta was published despite the cap")
	}
	if _, ok := exposed.Get("alpha"); !ok {
		t.Fatal("alpha disappeared after a failed load")
	}
}

func TestSearchOwnershipConflictZeroChange(t *testing.T) {
	s, exposed, st := connectTest(t)
	s.source.MustRegister(newStub("hosted", "hosted pdf tool"))
	exposed.MustRegister(&userStub{name: "hosted"})
	before := exposed.List()
	result := callSearch(t, st, `{"query":"hosted pdf"}`)
	if !result.IsError() || !strings.Contains(result.Content, "registered outside this instance") {
		t.Fatalf("conflict result = %v %q", result.IsError(), result.Content)
	}
	if !sameInfos(exposed.List(), before) {
		t.Fatal("exposed changed on ownership conflict")
	}
}

type userStub struct{ name string }

func (u *userStub) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: u.name, Description: "user tool", Parameters: tool.NewParameterSchema()}
}

func (u *userStub) Execute(context.Context, json.RawMessage) (*tool.ToolResult, error) {
	return tool.NewTextResult("user"), nil
}

func TestWrapperStaleAndRefresh(t *testing.T) {
	s, exposed, st := connectTest(t)
	original := newStub("api", "call the api")
	s.source.MustRegister(original)
	if result := callSearch(t, st, `{"query":"api"}`); result.IsError() {
		t.Fatalf("load failed: %s", result.Content)
	}
	wrapper, _ := exposed.Get("api")

	changed := newStub("api", "call the api v2")
	changed.info.RequiresApproval = true
	s.source.MustRegister(newStub("filler", "filler"))
	if err := s.source.Replace(changed); err != nil {
		t.Fatal(err)
	}

	result, err := wrapper.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("stale check produced Go error: %v", err)
	}
	if !result.IsError() || !strings.Contains(result.Content, "catalog_stale") {
		t.Fatalf("stale result = %v %q", result.IsError(), result.Content)
	}

	result = callSearch(t, st, `{"query":"api"}`)
	out := decodeOutput(t, result)
	if len(out.Matches) != 1 || out.Matches[0].NewlyLoaded {
		t.Fatalf("refresh match = %+v, want NewlyLoaded=false", out.Matches)
	}
	refreshed, _ := exposed.Get("api")
	if refreshed.Info().Description != "call the api v2" || !refreshed.Info().RequiresApproval {
		t.Fatalf("refreshed info = %+v", refreshed.Info())
	}
	result, err = refreshed.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || result.IsError() {
		t.Fatalf("refreshed wrapper execute = %v %q %v", result, result.Content, err)
	}
}

func TestWrapperNoFalseStaleOnKeywordOrder(t *testing.T) {
	s, exposed, st := connectTest(t)
	original := newStub("api", "stable")
	original.info.Parameters.Keywords = []tool.SchemaKeyword{
		{Name: "default", Value: json.RawMessage(`{"a":1,"b":2}`)},
	}
	s.source.MustRegister(original)
	if result := callSearch(t, st, `{"query":"api"}`); result.IsError() {
		t.Fatalf("load failed: %s", result.Content)
	}
	reordered := newStub("api", "stable")
	reordered.info.Parameters.Keywords = []tool.SchemaKeyword{
		{Name: "default", Value: json.RawMessage(`{"b":2,"a":1}`)},
	}
	reordered.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("v2"), nil
	}
	s.source.MustRegister(newStub("filler", "filler"))
	if err := s.source.Replace(reordered); err != nil {
		t.Fatal(err)
	}
	wrapper, _ := exposed.Get("api")
	result, err := wrapper.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("keyword-order replace produced Go error: %v", err)
	}
	if result.IsError() || result.Content != "v2" {
		t.Fatalf("keyword-order replace = %v %q, want forwarding without stale", result.IsError(), result.Content)
	}
}

func TestWrapperForwardsSameMetadataReplace(t *testing.T) {
	s, exposed, st := connectTest(t)
	v1 := newStub("api", "stable")
	v1.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("v1"), nil
	}
	s.source.MustRegister(v1)
	if result := callSearch(t, st, `{"query":"api"}`); result.IsError() {
		t.Fatalf("load failed: %s", result.Content)
	}
	v2 := newStub("api", "stable")
	v2.run = func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("v2"), nil
	}
	if err := s.source.Replace(v2); err != nil {
		t.Fatal(err)
	}
	wrapper, _ := exposed.Get("api")
	result, err := wrapper.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || result.Content != "v2" {
		t.Fatalf("same-metadata replace forwarded %q %v, want v2", result.Content, err)
	}
}

func TestWrapperDeletedFromSource(t *testing.T) {
	s, exposed, st := connectTest(t)
	s.source.MustRegister(newStub("api", "call the api"))
	if result := callSearch(t, st, `{"query":"api"}`); result.IsError() {
		t.Fatalf("load failed: %s", result.Content)
	}
	s.source.Unregister("api")
	wrapper, _ := exposed.Get("api")
	result, err := wrapper.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("deleted-source wrapper returned Go error: %v", err)
	}
	if !result.IsError() || !strings.Contains(result.Content, "no longer available") || !strings.Contains(result.Content, s.Name()) {
		t.Fatalf("deleted-source result = %q", result.Content)
	}
}

func TestDescriptionFoldedAndTruncated(t *testing.T) {
	_, _, st := connectTest(t)
	s := st.(*searchTool).s
	s.source.MustRegister(newStub("long", strings.Repeat("word ", 60)))
	result := callSearch(t, st, `{"query":"word"}`)
	out := decodeOutput(t, result)
	if len(out.Matches) != 1 {
		t.Fatalf("matches = %d", len(out.Matches))
	}
	got := out.Matches[0].Description
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > maxDescriptionRunes {
		t.Fatalf("folded description length = %d, suffix %q", len([]rune(got)), got[len(got)-3:])
	}
	if strings.HasSuffix(got, "  ") || strings.Contains(got, "  ") {
		t.Fatal("whitespace was not collapsed")
	}
}

func TestConcurrentSearchLoadsOnce(t *testing.T) {
	s, exposed, st := connectTest(t)
	s.source.MustRegister(newStub("only", "the only pdf tool"))
	const workers = 4
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			result, err := st.Execute(context.Background(), json.RawMessage(`{"query":"only pdf"}`))
			if err != nil {
				errs <- err
				return
			}
			if result.IsError() {
				errs <- context.DeadlineExceeded
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent search failed: %v", err)
		}
	}
	infos := exposed.List()
	if len(infos) != 2 {
		t.Fatalf("exposed has %d tools, want search + one wrapper", len(infos))
	}
}

func sameInfos(got, want []tool.ToolInfo) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Name != want[i].Name {
			return false
		}
	}
	return true
}

func TestSearchOwnershipTakeoverZeroChange(t *testing.T) {
	s, exposed, st := connectTest(t)
	s.source.MustRegister(newStub("api", "call the api"))
	s.source.MustRegister(newStub("other", "other pdf tool"))
	if result := callSearch(t, st, `{"query":"api"}`); result.IsError() {
		t.Fatalf("initial load failed: %s", result.Content)
	}

	if err := exposed.Replace(&userStub{name: "api"}); err != nil {
		t.Fatal(err)
	}
	result := callSearch(t, st, `{"query":"api pdf"}`)
	if !result.IsError() || !strings.Contains(result.Content, "taken over or removed outside this instance") {
		t.Fatalf("takeover re-search = %v %q", result.IsError(), result.Content)
	}
	if _, ok := exposed.Get("other"); ok {
		t.Fatal("batch was not zero-change: the other candidate got published")
	}
	if got, _ := exposed.Get("api"); got.Info().Description != "user tool" {
		t.Fatal("host replacement was overwritten by the search")
	}

	exposed.Unregister("api")
	result = callSearch(t, st, `{"query":"api pdf"}`)
	if !result.IsError() || !strings.Contains(result.Content, "taken over or removed outside this instance") {
		t.Fatalf("removed-name re-search = %v %q", result.IsError(), result.Content)
	}
	if _, ok := exposed.Get("other"); ok {
		t.Fatal("batch was not zero-change after removal")
	}
}

func TestWrapperPureApprovalChangeStale(t *testing.T) {
	s, exposed, st := connectTest(t)
	s.source.MustRegister(newStub("api", "stable description"))
	if result := callSearch(t, st, `{"query":"api"}`); result.IsError() {
		t.Fatalf("load failed: %s", result.Content)
	}
	changed := newStub("api", "stable description")
	changed.info.RequiresApproval = true
	s.source.MustRegister(newStub("filler", "filler"))
	if err := s.source.Replace(changed); err != nil {
		t.Fatal(err)
	}
	wrapper, _ := exposed.Get("api")
	result, err := wrapper.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("stale check produced Go error: %v", err)
	}
	if !result.IsError() || !strings.Contains(result.Content, "catalog_stale") {
		t.Fatalf("pure-approval change result = %v %q", result.IsError(), result.Content)
	}
	if result := callSearch(t, st, `{"query":"api"}`); result.IsError() {
		t.Fatalf("refresh search failed: %s", result.Content)
	}
	refreshed, _ := exposed.Get("api")
	if !refreshed.Info().RequiresApproval {
		t.Fatal("refreshed wrapper lost the approval requirement")
	}
}

func TestSearchCancellationZeroChange(t *testing.T) {
	_, exposed, st := connectTest(t)
	s0 := st.(*searchTool).s
	s0.source.MustRegister(newStub("pdf", "pdf tool"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := st.Execute(ctx, json.RawMessage(`{"query":"pdf"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search = %v %v, want context.Canceled", result, err)
	}
	if len(exposed.List()) != 1 {
		t.Fatalf("exposed changed on cancelled search: %v", exposed.List())
	}
}

func TestSearchCancelWhileWaitingForLockZeroChange(t *testing.T) {
	s, exposed, st := connectTest(t)
	s.source.MustRegister(newStub("pdf", "pdf tool"))
	// Handshake: the hook fires after Execute passed its first ctx check and
	// just before it parks on the load lock, so the cancel deterministically
	// lands inside Execute's protected region.
	arrived := make(chan struct{})
	muWaitHook = func() { arrived <- struct{}{} }
	t.Cleanup(func() { muWaitHook = nil })
	s.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := st.Execute(ctx, json.RawMessage(`{"query":"pdf"}`))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("lock-wait search = %v %v, want context.Canceled", result, err)
		}
	}()
	<-arrived
	cancel()
	s.mu.Unlock()
	<-done
	if len(exposed.List()) != 1 {
		t.Fatalf("exposed changed on cancelled search: %v", exposed.List())
	}
}

func TestSearchPostPublishCancellationKeepsLoaded(t *testing.T) {
	_, exposed, st := connectTest(t)
	s0 := st.(*searchTool).s
	s0.source.MustRegister(newStub("pdf", "pdf tool"))
	if result := callSearch(t, st, `{"query":"pdf"}`); result.IsError() {
		t.Fatalf("initial load failed: %s", result.Content)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := st.Execute(ctx, json.RawMessage(`{"query":"pdf"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-publish cancelled search = %v %v, want context.Canceled", result, err)
	}
	if len(exposed.List()) != 2 {
		t.Fatalf("post-publish cancellation changed the loaded set: %v", exposed.List())
	}
}
