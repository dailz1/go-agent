package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

const validRoundResultPayload = `{"run_id":"run","round":0,"index":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`

func roundResultFixture(schema int, payload string) store.Record {
	return store.Record{Kind: store.KindRoundResult, Schema: schema, ID: "run-r0-0", Seq: 1, Payload: json.RawMessage(payload)}
}

func TestRoundResultRecordCodec(t *testing.T) {
	images := `{"run_id":"run","round":2,"index":1,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"pic","images":[{"url":"http://x","mime_type":"image/png","data":"qw=="}],"is_error":false}]}}`
	softError := `{"run_id":"run","round":0,"index":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"rejected","is_error":true}]}}`
	for _, tc := range []struct {
		name    string
		schema  int
		payload string
		wantErr bool
		check   func(t *testing.T, p roundResultPayload)
	}{
		{name: "round_zero_index_zero", schema: 1, payload: validRoundResultPayload,
			check: func(t *testing.T, p roundResultPayload) {
				if p.RunID != "run" || p.Round != 0 || p.Index != 0 {
					t.Fatalf("decoded = %+v", p)
				}
				blocks := toolResultBlocks(p.Result)
				if len(blocks) != 1 || blocks[0].ToolUseID != "c1" || blocks[0].IsError {
					t.Fatalf("result = %+v", blocks)
				}
			}},
		{name: "nonzero_round_base", schema: 1,
			payload: `{"run_id":"run","round":7,"index":3,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`,
			check: func(t *testing.T, p roundResultPayload) {
				if p.Round != 7 || p.Index != 3 {
					t.Fatalf("decoded = %+v", p)
				}
			}},
		{name: "text_and_images", schema: 1, payload: images,
			check: func(t *testing.T, p roundResultPayload) {
				blocks := toolResultBlocks(p.Result)
				if len(blocks) != 1 || len(blocks[0].Images) != 1 || blocks[0].Images[0].URL != "http://x" {
					t.Fatalf("result = %+v", blocks)
				}
			}},
		{name: "soft_error", schema: 1, payload: softError,
			check: func(t *testing.T, p roundResultPayload) {
				if !toolResultBlocks(p.Result)[0].IsError {
					t.Fatalf("result = %+v", p.Result)
				}
			}},
		{name: "missing_run_id", schema: 1,
			payload: `{"round":0,"index":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`,
			wantErr: true},
		{name: "missing_round", schema: 1,
			payload: `{"run_id":"run","index":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`,
			wantErr: true},
		{name: "missing_index", schema: 1,
			payload: `{"run_id":"run","round":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`,
			wantErr: true},
		{name: "missing_result", schema: 1, payload: `{"run_id":"run","round":0,"index":0}`, wantErr: true},
		{name: "null_result", schema: 1,
			payload: `{"run_id":"run","round":0,"index":0,"result":null}`, wantErr: true},
		{name: "negative_round_index", schema: 1,
			payload: `{"run_id":"run","round":-1,"index":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`,
			wantErr: true},
		{name: "wrong_schema", schema: 2, payload: validRoundResultPayload, wantErr: true},
		{name: "unknown_field", schema: 1,
			payload: `{"run_id":"run","round":0,"index":0,"extra":1,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`,
			wantErr: true},
		{name: "extra_result_block", schema: 1,
			payload: `{"run_id":"run","round":0,"index":0,"result":{"role":"tool","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"},{"type":"tool_result","tool_use_id":"c1","content":"again"}]}}`,
			wantErr: true},
		{name: "missing_type", schema: 1,
			payload: `{"run_id":"run","round":0,"index":0,"result":{"role":"tool","content":[{"tool_use_id":"c1","content":"ok"}]}}`,
			wantErr: true},
		{name: "wrong_type_value", schema: 1,
			payload: `{"run_id":"run","round":0,"index":0,"result":{"role":"tool","content":[{"type":"text","tool_use_id":"c1","content":"ok"}]}}`,
			wantErr: true},
		{name: "wrong_role_or_type", schema: 1,
			payload: `{"run_id":"run","round":0,"index":0,"result":{"role":"assistant","content":[{"type":"text","text":"no"}]}}`,
			wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := decodeRoundResult(roundResultFixture(tc.schema, tc.payload))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decode = %+v, want error", p)
				}
				if !errors.Is(err, ErrIncompatibleLog) {
					t.Fatalf("decode err = %v, want ErrIncompatibleLog", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

// resultFailStore lets the first N round_result appends through and fails
// every later one, simulating a crash at an exact per-call record point.
type resultFailStore struct {
	store.Store
	failAfter int
	seen      int
}

func (f *resultFailStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	for _, r := range records {
		if r.Kind == store.KindRoundResult {
			f.seen++
			if f.seen > f.failAfter {
				return 0, errors.New("result append failed")
			}
		}
	}
	return f.Store.Append(ctx, thread, expected, records...)
}

func TestRoundResultAppendFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		concurrency int
		tools       []string
	}{
		{name: "serial_stops_before_next_dispatch", concurrency: 1, tools: []string{"a", "b", "c"}},
		{name: "parallel_aborts_settlement", concurrency: 2, tools: []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := &resultFailStore{Store: store.NewMemory(), failAfter: 1}
			reg := tool.NewRegistry()
			var executions atomic.Int32
			for _, name := range tc.tools {
				name := name
				reg.MustRegister(&parallelTestTool{info: tool.ToolInfo{Name: name},
					execute: func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
						executions.Add(1)
						return tool.NewTextResult(name), nil
					}})
			}
			provider := NewMockStreamingProvider([][]llm.Chunk{
				parallelToolRound(parallelToolCalls(tc.tools...)), textChunks("done"),
			})
			agent := New(provider, reg, WithStore(st), WithLogger(discardLogger()))
			if tc.concurrency > 1 {
				agent = New(provider, reg, WithStore(st), WithToolConcurrency(tc.concurrency), WithLogger(discardLogger()))
			}
			seq, err := agent.RunStream(ctx, "go", WithThread("t"))
			if err != nil {
				t.Fatalf("RunStream: %v", err)
			}
			delivered := 0
			aborted := false
			for event, streamErr := range seq {
				if _, ok := event.(ToolResultEvent); ok {
					delivered++
				}
				if streamErr != nil {
					aborted = true
					break
				}
			}
			if !aborted || delivered != 1 {
				t.Fatalf("aborted=%v delivered=%d, want abort after exactly the recorded prefix", aborted, delivered)
			}
			if hasKind(t, st, "t", store.KindRoundCommitted) {
				t.Fatal("an aborted round must not commit")
			}
			res, err := agent.ResumeThread(ctx, "t")
			if err != nil {
				t.Fatalf("ResumeThread: %v", err)
			}
			blocks := historyResultBlocks(res.History)
			if len(blocks) != len(tc.tools) {
				t.Fatalf("recovered results = %d, want one per declared call", len(blocks))
			}
			if blocks[0].IsError || !strings.Contains(blocks[0].Content, tc.tools[0]) {
				t.Fatalf("first result = %#v, want the recorded real result", blocks[0])
			}
			for _, b := range blocks[1:] {
				if !b.IsError || !strings.Contains(b.Content, "outcome unknown") {
					t.Fatalf("tail result = %#v, want unknown", b)
				}
			}
			if tc.concurrency == 1 && executions.Load() != 2 {
				t.Fatalf("executions = %d, want 2 (the abort must stop later serial dispatch)", executions.Load())
			}
		})
	}
}

// controlInjectStore writes an external control record at the expected head
// right before the first round_result append lands, forcing the absorb and
// byte-identical retry protocol.
type controlInjectStore struct {
	store.Store
	kind  string
	fired bool
}

func (c *controlInjectStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	for _, r := range records {
		if r.Kind == store.KindRoundResult && !c.fired {
			c.fired = true
			var payload any
			if c.kind == store.KindConfigChanged {
				payload = configChangedPayload{}
			} else {
				payload = inputQueuedPayload{Input: "external"}
			}
			rec, err := encodeRecord(c.kind, recordID("ext", "-1"), payload)
			if err != nil {
				return 0, err
			}
			rec.Schema = store.SchemaV2
			if _, err := c.Store.Append(ctx, thread, expected, rec); err != nil {
				return 0, err
			}
			break
		}
	}
	return c.Store.Append(ctx, thread, expected, records...)
}

func TestRoundResultControlRebase(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
	}{
		{name: "config_changed", kind: store.KindConfigChanged},
		{name: "input_queued", kind: store.KindInputQueued},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory()
			inj := &controlInjectStore{Store: st, kind: tc.kind}
			reg := tool.NewRegistry()
			reg.MustRegister(mutatorTool("calc", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
				return tool.NewTextResult("42"), nil
			}))
			provider := NewMockStreamingProvider([][]llm.Chunk{toolCallChunks("c1", "calc"), textChunks("done")})
			agent := New(provider, reg, WithStore(inj), WithLogger(discardLogger()))
			res, err := agent.Run(context.Background(), "go", WithThread("t"))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := countKind(t, st, "t", store.KindRoundResult); got != 1 {
				t.Fatalf("round_result records = %d, want exactly one (byte-identical retry, no duplicate)", got)
			}
			if got := countKind(t, st, "t", tc.kind); got != 1 {
				t.Fatalf("%s records = %d, want 1", tc.kind, got)
			}
			if len(res.History) == 0 {
				t.Fatal("empty history")
			}
		})
	}
}

func TestRoundCloseUsesSavedHistory(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	var orig tool.ToolResult
	reg.MustRegister(mutatorTool("pic", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		r := &tool.ToolResult{
			Content: "pic",
			Data:    json.RawMessage(`{"live":1}`),
			Images:  []tool.Image{{URL: "http://x", MIMEType: "image/png", Data: []byte{0xAB}}},
		}
		orig = tool.ToolResult{Content: r.Content, Data: append(json.RawMessage(nil), r.Data...),
			Images: []tool.Image{{URL: r.Images[0].URL, MIMEType: r.Images[0].MIMEType, Data: append([]byte(nil), r.Images[0].Data...)}}}
		return r, nil
	}))
	agent := New(parallelProvider(parallelToolCalls("pic")), reg, WithStore(st), WithLogger(discardLogger()))
	seq, err := agent.RunStream(ctx, "go", WithThread("t"))
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	mutated := false
	for event, streamErr := range seq {
		if streamErr != nil {
			t.Fatalf("stream: %v", streamErr)
		}
		if ev, ok := event.(ToolResultEvent); ok {
			ev.Result.Content = "hacked"
			ev.Result.Status = tool.ResultError
			ev.Result.Images[0].Data[0] ^= 0xFF
			if ev.Result.Data != nil {
				ev.Result.Data[0] ^= 0xFF
			}
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("consumer never saw the result event")
	}
	if orig.Content != "pic" || orig.Images[0].Data[0] != 0xAB || orig.Data[0] != '{' {
		t.Fatalf("original result mutated: %+v", orig)
	}
	for _, kind := range []string{store.KindRoundResult, store.KindRoundCommitted} {
		for _, r := range logRecords(t, st, "t") {
			if r.Kind != kind {
				continue
			}
			if strings.Contains(string(r.Payload), "hacked") {
				t.Fatalf("%s payload carries the consumer mutation: %s", kind, r.Payload)
			}
			if !strings.Contains(string(r.Payload), "pic") {
				t.Fatalf("%s payload lost the result: %s", kind, r.Payload)
			}
		}
	}
	v, err := agent.replayThread(ctx, "t")
	if err != nil {
		t.Fatalf("replay of the consumer-mutated run: %v", err)
	}
	blocks := historyResultBlocks(v.history)
	if len(blocks) == 0 || blocks[0].Content != "pic" || blocks[0].IsError {
		t.Fatalf("replayed history result = %+v, want the original", blocks)
	}
	if len(blocks[0].Images) != 1 || blocks[0].Images[0].Data[0] != 0xAB {
		t.Fatalf("replayed image bytes = %v, want the original", blocks[0].Images)
	}
}

func TestRoundCloseNormalizesInvalidUTF8(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("raw", func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
		return &tool.ToolResult{Content: "ok\n\xff"}, nil
	}))
	agent := New(parallelProvider(parallelToolCalls("raw")), reg, WithStore(st), WithLogger(discardLogger()))
	seq, err := agent.RunStream(ctx, "go", WithThread("t"))
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var delivered string
	for event, streamErr := range seq {
		if streamErr != nil {
			t.Fatalf("stream: %v", streamErr)
		}
		if ev, ok := event.(ToolResultEvent); ok {
			delivered = ev.Result.Content
		}
	}
	if !strings.Contains(delivered, "\uFFFD") || strings.ContainsRune(delivered, '\xff') {
		t.Fatalf("delivered content = %q, want normalized replacement rune", delivered)
	}
	for _, r := range logRecords(t, st, "t") {
		if r.Kind != store.KindRoundResult {
			continue
		}
		if strings.ContainsRune(string(r.Payload), '\xff') {
			t.Fatalf("payload carries the raw invalid byte: %s", r.Payload)
		}
	}
	v, err := agent.replayThread(ctx, "t")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	blocks := historyResultBlocks(v.history)
	if len(blocks) == 0 || blocks[0].Content != delivered {
		t.Fatalf("replayed %#v != delivered %q", blocks, delivered)
	}
}

func TestRoundCloseFailureKeepsRecordedResults(t *testing.T) {
	ctx := context.Background()
	st := &failAppendStore{Store: store.NewMemory(), failKind: store.KindRoundCommitted}
	reg := tool.NewRegistry()
	for _, name := range []string{"one", "two"} {
		name := name
		reg.MustRegister(mutatorTool(name, func(context.Context, json.RawMessage) (*tool.ToolResult, error) {
			return tool.NewTextResult(name), nil
		}))
	}
	agent := New(parallelProvider(parallelToolCalls("one", "two")), reg, WithStore(st), WithLogger(discardLogger()))
	seq, err := agent.RunStream(ctx, "go", WithThread("t"))
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	terminal := false
	for event, streamErr := range seq {
		if _, ok := event.(ToolResultEvent); !ok && streamErr == nil {
			continue
		}
		if streamErr != nil {
			terminal = true
			break
		}
	}
	if !terminal {
		t.Fatal("the failed round close must abort the run")
	}
	res, err := agent.ResumeThread(ctx, "t")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	blocks := historyResultBlocks(res.History)
	if len(blocks) != 2 || blocks[0].IsError || blocks[1].IsError {
		t.Fatalf("recovered results = %#v, want both recorded results preserved", blocks)
	}
	if !hasKind(t, st, "t", store.KindRoundCommitted) {
		t.Fatal("recovery must write the closing commit exactly once")
	}
}
