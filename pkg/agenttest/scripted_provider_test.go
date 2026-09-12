package agenttest

import (
	"context"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/pkg/llm"
	"github.com/dailz1/go-agent/pkg/tool"
)

func TestScriptedProviderSequentialAndFallback(t *testing.T) {
	request := Request{Messages: []llm.Message{llm.UserMessage("hello")}}
	fallback := llm.AssistantMessage("fallback")
	provider := NewScriptedProvider(
		Exchange{Method: MethodChatStream, Request: request, StreamOuterErr: llm.ErrStreamingNotSupported},
		Exchange{Method: MethodChat, Request: request, ChatResponse: &fallback},
	)
	if _, err := provider.ChatStream(context.Background(), request.Messages, nil); !errors.Is(err, llm.ErrStreamingNotSupported) {
		t.Fatalf("ChatStream error = %v", err)
	}
	message, _, err := provider.Chat(context.Background(), request.Messages, nil)
	if err != nil || message.Content[0].(llm.TextBlock).Text != "fallback" {
		t.Fatalf("Chat = %#v, %v", message, err)
	}
	if err := provider.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestScriptedProviderReservationAndExhaustion(t *testing.T) {
	request := Request{Messages: []llm.Message{llm.UserMessage("x")}}
	provider := NewScriptedProvider(Exchange{Method: MethodChatStream, Request: request})
	sequence, err := provider.ChatStream(context.Background(), request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.Chat(context.Background(), request.Messages, nil); !errors.Is(err, ErrConcurrentScriptUse) {
		t.Fatalf("concurrent call = %v", err)
	}
	verification := new(ScriptVerificationError)
	if err := provider.Verify(); !errors.As(err, &verification) || !verification.Active || verification.Remaining != 1 {
		t.Fatalf("Verify = %#v", err)
	}
	for range sequence {
	}
	if _, _, err := provider.Chat(context.Background(), request.Messages, nil); !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("exhaustion = %v", err)
	}
}

func TestScriptedProviderStrictRequestMatching(t *testing.T) {
	temperature := 0.4
	request := Request{
		Messages: []llm.Message{llm.UserMessage("expected")},
		Tools:    []tool.ToolInfo{{Name: "a"}, {Name: "b"}},
		Options:  llm.Options{Model: "model", Temperature: &temperature},
	}
	cases := []struct {
		name     string
		messages []llm.Message
		tools    []tool.ToolInfo
		options  []llm.Option
	}{
		{"messages", []llm.Message{llm.UserMessage("other")}, request.Tools, []llm.Option{llm.WithModel("model"), llm.WithTemperature(temperature)}},
		{"tools", request.Messages, []tool.ToolInfo{{Name: "a"}, {Name: "other"}}, []llm.Option{llm.WithModel("model"), llm.WithTemperature(temperature)}},
		{"options", request.Messages, request.Tools, []llm.Option{llm.WithModel("other"), llm.WithTemperature(temperature)}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			provider := NewScriptedProvider(Exchange{Method: MethodChat, Request: request})
			_, _, err := provider.Chat(context.Background(), test.messages, test.tools, test.options...)
			mismatch := new(RequestMismatchError)
			if !errors.Is(err, ErrScriptMismatch) || !errors.As(err, &mismatch) || mismatch.Step != 0 {
				t.Fatalf("mismatch = %#v", err)
			}
		})
	}
}

func TestScriptedProviderNormalizesOptionsAndTools(t *testing.T) {
	first := func(options *llm.Options) { options.Model = "model"; options.MaxTokens = 5 }
	second := func(options *llm.Options) { options.Model = "model"; options.MaxTokens = 5 }
	provider := NewScriptedProvider(Exchange{Method: MethodChat, Request: Request{Messages: []llm.Message{llm.UserMessage("x")}, Tools: []tool.ToolInfo{{Name: "a"}, {Name: "b"}}, Options: llm.ApplyOptions([]llm.Option{first})}})
	if _, _, err := provider.Chat(context.Background(), []llm.Message{llm.UserMessage("x")}, []tool.ToolInfo{{Name: "b"}, {Name: "a"}}, second); err != nil {
		t.Fatal(err)
	}
}
