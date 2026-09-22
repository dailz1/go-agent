package codex_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/dailz1/go-agent/agent"
	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/codex"
	"github.com/dailz1/go-agent/llm/codex/auth"
	"github.com/dailz1/go-agent/tool"
)

type fixedSource struct{}

func (fixedSource) Token(context.Context) (auth.Token, error) {
	return auth.Token{AccessToken: "test", AccountID: "test"}, nil
}
func (f fixedSource) Refresh(ctx context.Context, _ *auth.Token) (auth.Token, error) {
	return f.Token(ctx)
}

type functionTool struct {
	name  string
	calls *atomic.Int32
	fail  bool
}

func (f functionTool) Info() tool.ToolInfo {
	return tool.ToolInfo{Name: f.name, Parameters: tool.ParameterSchema{
		Type: "object", Properties: map[string]tool.Property{"values": {Type: "array", Items: &tool.Property{Type: "integer"}}},
		Required: []string{"values"}, Keywords: []tool.SchemaKeyword{{Name: "x-test", Value: json.RawMessage(`{"keep":true}`)}},
	}}
}
func (f functionTool) Execute(_ context.Context, args json.RawMessage) (*tool.ToolResult, error) {
	f.calls.Add(1)
	if f.fail {
		return tool.NewErrorResult("unavailable"), nil
	}
	return tool.NewTextResult(string(args)), nil
}

func TestAgentTwoFunctionRoundTrip(t *testing.T) {
	fixture, err := os.ReadFile("testdata/two_functions.sse")
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var requests, calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				round := requests.Add(1)
				var request struct {
					Input []struct {
						Type, ID, Name string
						CallID         string `json:"call_id"`
						Output         string `json:"output"`
						Encrypted      string `json:"encrypted_content"`
					}
					Tools []struct {
						Type       string
						Strict     bool
						Parameters json.RawMessage
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if len(request.Tools) != 2 {
					t.Errorf("tools=%+v", request.Tools)
				}
				for _, def := range request.Tools {
					var schema map[string]any
					if err := json.Unmarshal(def.Parameters, &schema); err != nil {
						t.Error(err)
						return
					}
					if def.Type != "function" || def.Strict || schema["x-test"] == nil {
						t.Errorf("tool=%+v", def)
					}
					values := schema["properties"].(map[string]any)["values"].(map[string]any)
					if values["items"].(map[string]any)["type"] != "integer" {
						t.Errorf("schema=%s", def.Parameters)
					}
				}
				if round == 1 {
					w.Write(fixture)
					return
				}
				if round != 2 {
					t.Errorf("extra request %d", round)
				}
				outputs := map[string]string{}
				var kinds []string
				for _, item := range request.Input {
					switch item.Type {
					case "reasoning":
						kinds = append(kinds, item.Type)
						if item.ID != "reasoning_a" || item.Encrypted != "synthetic-opaque" {
							t.Errorf("reasoning=%+v", item)
						}
					case "function_call":
						kinds = append(kinds, item.Type)
					case "function_call_output":
						outputs[item.CallID] = item.Output
					}
				}
				if !reflect.DeepEqual(kinds, []string{"reasoning", "function_call", "function_call"}) ||
					!reflect.DeepEqual(outputs, map[string]string{"call_a": `{"values":[1]}`, "call_b": "unavailable"}) {
					t.Errorf("replay=%+v outputs=%v", request.Input, outputs)
				}
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"final\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"finished\"}]}]}}\n\n")
			}))
			defer server.Close()
			registry := tool.NewRegistry()
			for _, f := range []functionTool{{name: "first", calls: &calls}, {name: "second", calls: &calls, fail: true}} {
				if err := registry.Register(f); err != nil {
					t.Fatal(err)
				}
			}
			provider := codex.NewProvider("synthetic", codex.WithAuthSource(fixedSource{}), codex.WithBaseURL(server.URL))
			a := agent.New(provider, registry, agent.WithLLMOptions(llm.WithMaxTokens(4096)))
			var history []llm.Message
			if stream {
				seq, err := a.RunStream(t.Context(), "use both")
				if err != nil {
					t.Fatal(err)
				}
				for event, err := range seq {
					if err != nil {
						t.Fatal(err)
					}
					if done, ok := event.(agent.DoneEvent); ok {
						history = done.History
					}
				}
			} else {
				result, err := a.Run(t.Context(), "use both")
				if err != nil {
					t.Fatal(err)
				}
				history = result.History
			}
			if requests.Load() != 2 || calls.Load() != 2 || len(history) == 0 {
				t.Fatalf("requests=%d calls=%d history=%v", requests.Load(), calls.Load(), history)
			}
			last := history[len(history)-1]
			if !reflect.DeepEqual(last, llm.AssistantMessage("finished")) {
				t.Fatalf("final=%+v", last)
			}
		})
	}
}

func TestAgentDoesNotExecuteIncompleteCalls(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"f\",\"call_id\":\"c\",\"name\":\"first\",\"arguments\":\"{}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.incomplete\"}\n\n")
	}))
	defer server.Close()
	registry := tool.NewRegistry()
	if err := registry.Register(functionTool{name: "first", calls: &calls}); err != nil {
		t.Fatal(err)
	}
	provider := codex.NewProvider("m", codex.WithAuthSource(fixedSource{}), codex.WithBaseURL(server.URL))
	_, err := agent.New(provider, registry).Run(t.Context(), "invoke")
	if err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v tool executions=%d", err, calls.Load())
	}
}
