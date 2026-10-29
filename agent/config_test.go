package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func configTestOpts(opts []llm.Option) llm.Options {
	var o llm.Options
	for _, f := range opts {
		f(&o)
	}
	return o
}

func strptr(s string) *string { return &s }

func toolCallChunks(id, name string) []llm.Chunk {
	return []llm.Chunk{
		llm.ToolCallStartChunk{Index: 0, ID: id, Name: name},
		llm.ToolCallArgsChunk{Index: 0, Delta: "{}"},
		llm.DoneChunk{FinishReason: "tool_calls"},
	}
}

func textChunks(text string) []llm.Chunk {
	return []llm.Chunk{llm.TextDeltaChunk{Text: text}, llm.DoneChunk{FinishReason: "stop"}}
}

func TestSetThreadConfigAppliesBeforeRun(t *testing.T) {
	st := store.NewMemory()
	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("done")})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLLMOptions(llm.WithModel("model-a")),
		WithLogger(discardLogger()),
	)
	if err := SetThreadConfig(context.Background(), st, "t", ConfigChange{Model: strptr("model-b")}); err != nil {
		t.Fatalf("SetThreadConfig: %v", err)
	}
	if _, err := agent.Run(context.Background(), "hello", WithThread("t")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := configTestOpts(provider.LastOptions).Model; got != "model-b" {
		t.Fatalf("model = %q, want model-b", got)
	}
}

func TestSetThreadConfigMidRunAbsorbedAtNextRound(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		if err := SetThreadConfig(ctx, st, "t", ConfigChange{Model: strptr("model-b")}); err != nil {
			return nil, err
		}
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{
		toolCallChunks("c1", "calc"),
		textChunks("done"),
	})
	agent := New(provider, reg,
		WithStore(st),
		WithLLMOptions(llm.WithModel("model-a")),
		WithLogger(discardLogger()),
	)
	if _, err := agent.Run(context.Background(), "compute", WithThread("t")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := configTestOpts(provider.LastOptions).Model; got != "model-b" {
		t.Fatalf("round-2 model = %q, want model-b (absorbed after mid-run change)", got)
	}
}

func TestSetThreadConfigValidation(t *testing.T) {
	if err := SetThreadConfig(context.Background(), nil, "t", ConfigChange{Model: strptr("m")}); err == nil || !strings.Contains(err.Error(), "no store") {
		t.Fatalf("nil store err = %v", err)
	}
	st := store.NewMemory()
	if err := SetThreadConfig(context.Background(), st, "t", ConfigChange{}); err == nil || err != ErrEmptyConfig {
		t.Fatalf("empty change err = %v, want ErrEmptyConfig", err)
	}
}

func TestConfigReplayAfterRestart(t *testing.T) {
	st := store.NewMemory()
	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("done"), textChunks("done2")})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLLMOptions(llm.WithModel("model-a")),
		WithLogger(discardLogger()),
	)
	if _, err := agent.Run(context.Background(), "first", WithThread("t")); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := configTestOpts(provider.LastOptions).Model; got != "model-a" {
		t.Fatalf("first run model = %q, want model-a", got)
	}
	if err := SetThreadConfig(context.Background(), st, "t", ConfigChange{Thinking: strptr("enabled")}); err != nil {
		t.Fatalf("SetThreadConfig: %v", err)
	}
	if _, err := agent.Run(context.Background(), "second", WithThread("t")); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got := configTestOpts(provider.LastOptions)
	if got.Model != "model-a" || got.Thinking != "enabled" {
		t.Fatalf("second run opts = %+v, want model-a + thinking enabled", got)
	}
}

func handBuiltThread(t *testing.T, checkpoint store.Record) store.Store {
	t.Helper()
	st := store.NewMemory()
	rec, err := encodeRecord(store.KindRunStarted, "r1-start",
		runStartedPayload{RunID: "r1", Input: "go", SystemPrompt: ""})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(context.Background(), "t", 0, rec); err != nil {
		t.Fatal(err)
	}
	cfgRec, err := encodeRecord(store.KindConfigChanged, "cfg-test", configChangedPayload{Model: strptr("model-b")})
	if err != nil {
		t.Fatal(err)
	}
	cfgRec.Schema = store.SchemaV2
	if _, err := st.Append(context.Background(), "t", 1, cfgRec); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(context.Background(), "t", 2, checkpoint); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestConfigBehindV2CheckpointFallsBackToFullLog(t *testing.T) {
	v2Payload, err := json.Marshal(agentCheckpoint{
		CodecVersion: store.SchemaV2,
		History:      []llm.Message{llm.UserMessage("go")},
		System:       "",
		RunActive:    true,
		RunID:        "r1",
		LastRound:    -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := handBuiltThread(t, store.Record{
		Kind: store.KindCheckpoint, Schema: store.SchemaV2, ID: "cp-2", Payload: v2Payload,
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("resumed")})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLogger(discardLogger()),
	)
	if _, err := agent.ResumeThread(context.Background(), "t"); err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if got := configTestOpts(provider.LastOptions).Model; got != "model-b" {
		t.Fatalf("model = %q, want model-b (v2 checkpoint must not hide config_changed)", got)
	}
}

func TestConfigProjectedIntoV3Checkpoint(t *testing.T) {
	v3Payload, err := json.Marshal(agentCheckpoint{
		CodecVersion: store.SchemaV3,
		History:      []llm.Message{llm.UserMessage("go")},
		System:       "",
		RunActive:    true,
		RunID:        "r1",
		LastRound:    -1,
		Config:       &configState{Model: strptr("model-b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := handBuiltThread(t, store.Record{
		Kind: store.KindCheckpoint, Schema: store.SchemaV3, ID: "cp-2", Payload: v3Payload,
	})

	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("resumed")})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLogger(discardLogger()),
	)
	if _, err := agent.ResumeThread(context.Background(), "t"); err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if got := configTestOpts(provider.LastOptions).Model; got != "model-b" {
		t.Fatalf("model = %q, want model-b (projected in v3 checkpoint)", got)
	}
}

type injectBeforeStore struct {
	store.Store
	injected bool
}

func (s *injectBeforeStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	if !s.injected && len(records) > 0 && records[0].Kind == store.KindRunStarted {
		s.injected = true
		rec, err := encodeRecord(store.KindConfigChanged, "cfg-inject", configChangedPayload{Model: strptr("model-b")})
		if err != nil {
			return 0, err
		}
		rec.Schema = store.SchemaV2
		if _, err := s.Store.Append(ctx, thread, expected, rec); err != nil {
			return 0, err
		}
		// Do not advance expected: the runner's append must conflict so the
		// D6(a) absorb path folds the injected config into the session.
	}
	return s.Store.Append(ctx, thread, expected, records...)
}

type failingAppendStore struct {
	store.Store
	err error
}

func (s *failingAppendStore) Append(ctx context.Context, thread string, expected int64, records ...store.Record) (int64, error) {
	return 0, s.err
}

func TestSetThreadConfigABAEndsOnA(t *testing.T) {
	st := store.NewMemory()
	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("done")})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLLMOptions(llm.WithModel("model-default")),
		WithLogger(discardLogger()),
	)
	ctx := context.Background()
	for _, m := range []string{"model-a", "model-b", "model-a"} {
		if err := SetThreadConfig(ctx, st, "t", ConfigChange{Model: strptr(m)}); err != nil {
			t.Fatalf("SetThreadConfig(%s): %v", m, err)
		}
	}
	if _, err := agent.Run(ctx, "hello", WithThread("t")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := configTestOpts(provider.LastOptions).Model; got != "model-a" {
		t.Fatalf("model = %q, want model-a (A->B->A must end on A)", got)
	}
}

func TestCheckpointCarriesAbsorbedConfig(t *testing.T) {
	st := store.NewMemory()
	reg := tool.NewRegistry()
	reg.MustRegister(mutatorTool("calc", func(ctx context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
		return tool.NewTextResult("42"), nil
	}))
	provider := NewMockStreamingProvider([][]llm.Chunk{
		toolCallChunks("c1", "calc"),
		textChunks("done"),
	})
	agent := New(provider, reg,
		WithStore(st),
		WithLLMOptions(llm.WithModel("model-a")),
		WithLogger(discardLogger()),
		WithContextWindowTokens(400),
		WithCompactionThresholdPercent(40),
		WithCompactor(CompactFunc(func(ctx context.Context, history []llm.Message, budget CompactionBudget, _ llm.Provider, _ []llm.Option) (CompactionResult, error) {
			if err := SetThreadConfig(ctx, st, "t", ConfigChange{Model: strptr("model-b")}); err != nil {
				return CompactionResult{}, err
			}
			return CompactionResult{History: []llm.Message{llm.UserMessage("compute")}, Changed: true}, nil
		})),
	)
	if _, err := agent.Run(context.Background(), "compute something here now", WithThread("t")); err != nil {
		t.Fatalf("run: %v", err)
	}
	state, err := st.Latest(context.Background(), "t")
	if err != nil || state.Checkpoint == nil {
		t.Fatalf("checkpoint missing: %v", err)
	}
	var cp agentCheckpoint
	if err := strictDecode(state.Checkpoint.Payload, &cp); err != nil {
		t.Fatalf("checkpoint decode: %v", err)
	}
	if cp.Config == nil || cp.Config.Model == nil || *cp.Config.Model != "model-b" {
		t.Fatalf("checkpoint config = %+v, want model-b (rebased projection)", cp.Config)
	}
	provider2 := NewMockStreamingProvider([][]llm.Chunk{textChunks("done2")})
	resumed := New(provider2, tool.NewRegistry(),
		WithStore(st),
		WithLLMOptions(llm.WithModel("model-a")),
		WithLogger(discardLogger()),
	)
	if _, err := resumed.Run(context.Background(), "again", WithThread("t")); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if got := configTestOpts(provider2.LastOptions).Model; got != "model-b" {
		t.Fatalf("resumed model = %q, want model-b", got)
	}
}

func TestRunStartRebasesOverConfig(t *testing.T) {
	st := &injectBeforeStore{Store: store.NewMemory()}
	provider := NewMockStreamingProvider([][]llm.Chunk{textChunks("done")})
	agent := New(provider, tool.NewRegistry(),
		WithStore(st),
		WithLLMOptions(llm.WithModel("model-a")),
		WithLogger(discardLogger()),
	)
	if _, err := agent.Run(context.Background(), "hello", WithThread("t")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := configTestOpts(provider.LastOptions).Model; got != "model-b" {
		t.Fatalf("model = %q, want model-b (run_started rebased over injected config)", got)
	}
}

func TestSetThreadConfigPreservesStoreError(t *testing.T) {
	cause := errors.New("disk on fire")
	st := &failingAppendStore{Store: store.NewMemory(), err: cause}
	err := SetThreadConfig(context.Background(), st, "t", ConfigChange{Model: strptr("m")})
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err = %v, want wrapped store cause", err)
	}
	if errors.Is(err, ErrThreadBusy) {
		t.Fatalf("store cause must not be reported as ErrThreadBusy")
	}

	conflictOnly := &failingAppendStore{Store: store.NewMemory(), err: store.ErrRevisionConflict}
	err = SetThreadConfig(context.Background(), conflictOnly, "t", ConfigChange{Model: strptr("m")})
	if !errors.Is(err, ErrThreadBusy) {
		t.Fatalf("sustained conflicts err = %v, want ErrThreadBusy", err)
	}
}
