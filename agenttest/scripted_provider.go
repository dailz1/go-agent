// Package agenttest provides public, stdlib-only test doubles for llm providers and tools.
package agenttest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"sync"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

type Method string

const (
	MethodChat       Method = "chat"
	MethodChatStream Method = "chat_stream"
)

var (
	ErrInvalidExchange     = errors.New("invalid scripted exchange")
	ErrConcurrentScriptUse = errors.New("concurrent scripted provider use")
	ErrScriptExhausted     = errors.New("script exhausted")
	ErrScriptMismatch      = errors.New("script request mismatch")
	ErrUnverifiedScript    = errors.New("unverified script")
)

// Request is the canonical provider request used for strict script matching.
type Request struct {
	Messages []llm.Message
	Tools    []tool.ToolInfo
	Options  llm.Options
}

// Exchange defines one globally ordered provider call.
type Exchange struct {
	Method  Method
	Request Request

	ChatResponse *llm.Message
	ChatUsage    *llm.Usage
	ChatErr      error

	StreamChunks   []llm.Chunk
	StreamOuterErr error
	StreamErr      error
}

// RequestMismatchError deliberately excludes request content so prompts are not exposed in errors.
type RequestMismatchError struct {
	Step           int
	ExpectedMethod Method
	ActualMethod   Method
}

func (e *RequestMismatchError) Error() string {
	return fmt.Sprintf("script step %d expects %s, got %s", e.Step, e.ExpectedMethod, e.ActualMethod)
}
func (*RequestMismatchError) Unwrap() error { return ErrScriptMismatch }

// ScriptVerificationError identifies unconsumed or active scripted exchanges.
type ScriptVerificationError struct {
	Next      int
	Remaining int
	Active    bool
}

func (e *ScriptVerificationError) Error() string {
	return fmt.Sprintf("script has %d unconsumed exchange(s) from step %d", e.Remaining, e.Next)
}
func (*ScriptVerificationError) Unwrap() error { return ErrUnverifiedScript }

// ScriptedProvider consumes one global, strict exchange sequence safely.
type ScriptedProvider struct {
	mu        sync.Mutex
	callMu    sync.Mutex
	exchanges []Exchange
	next      int
	active    bool
}

func NewScriptedProvider(exchanges ...Exchange) *ScriptedProvider {
	copied := make([]Exchange, len(exchanges))
	for i, exchange := range exchanges {
		copied[i] = cloneExchange(exchange)
	}
	return &ScriptedProvider{exchanges: copied}
}

func (*ScriptedProvider) Name() string { return "agenttest_scripted" }

func (p *ScriptedProvider) Chat(_ context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (*llm.Message, *llm.Usage, error) {
	if !p.callMu.TryLock() {
		return nil, nil, ErrConcurrentScriptUse
	}
	defer p.callMu.Unlock()
	exchange, err := p.reserve(MethodChat, canonicalRequest(messages, tools, opts))
	if err != nil {
		return nil, nil, err
	}
	return cloneMessagePtr(exchange.ChatResponse), cloneUsage(exchange.ChatUsage), exchange.ChatErr
}

func (p *ScriptedProvider) ChatStream(_ context.Context, messages []llm.Message, tools []tool.ToolInfo, opts ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	if !p.callMu.TryLock() {
		return nil, ErrConcurrentScriptUse
	}
	keep := false
	defer func() {
		if !keep {
			p.callMu.Unlock()
		}
	}()
	exchange, err := p.reserve(MethodChatStream, canonicalRequest(messages, tools, opts))
	if err != nil {
		return nil, err
	}
	if exchange.StreamOuterErr != nil {
		p.finishOuter()
		return nil, exchange.StreamOuterErr
	}
	keep = true
	return p.stream(exchange), nil
}

func (p *ScriptedProvider) reserve(method Method, request Request) (Exchange, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active {
		return Exchange{}, ErrConcurrentScriptUse
	}
	if p.next == len(p.exchanges) {
		return Exchange{}, fmt.Errorf("%w: %s step %d", ErrScriptExhausted, method, p.next+1)
	}
	exchange := p.exchanges[p.next]
	if !validExchange(exchange) {
		return Exchange{}, ErrInvalidExchange
	}
	if exchange.Method != method || !reflect.DeepEqual(exchange.Request, request) {
		return Exchange{}, &RequestMismatchError{Step: p.next, ExpectedMethod: exchange.Method, ActualMethod: method}
	}
	if method == MethodChat {
		p.next++
	} else {
		p.active = true
	}
	return cloneExchange(exchange), nil
}

func (p *ScriptedProvider) stream(exchange Exchange) iter.Seq2[llm.Chunk, error] {
	var once sync.Once
	return func(yield func(llm.Chunk, error) bool) {
		run := false
		once.Do(func() { run = true })
		if !run {
			return
		}
		for _, chunk := range exchange.StreamChunks {
			if !yield(cloneChunk(chunk), nil) {
				p.finishStream(true)
				return
			}
		}
		if exchange.StreamErr != nil {
			yield(nil, exchange.StreamErr)
		}
		p.finishStream(true)
	}
}

func (p *ScriptedProvider) finishOuter() { p.mu.Lock(); p.active = false; p.next++; p.mu.Unlock() }

func (p *ScriptedProvider) finishStream(advance bool) {
	p.mu.Lock()
	if !p.active {
		p.mu.Unlock()
		return
	}
	p.active = false
	if advance {
		p.next++
	}
	p.mu.Unlock()
	p.callMu.Unlock()
}

func (p *ScriptedProvider) Verify() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next == len(p.exchanges) && !p.active {
		return nil
	}
	return &ScriptVerificationError{Next: p.next, Remaining: len(p.exchanges) - p.next, Active: p.active}
}

func canonicalRequest(messages []llm.Message, tools []tool.ToolInfo, opts []llm.Option) Request {
	return Request{Messages: cloneMessages(messages), Tools: cloneTools(tools), Options: cloneOptions(llm.ApplyOptions(opts))}
}

func validExchange(e Exchange) bool {
	switch e.Method {
	case MethodChat:
		return e.StreamChunks == nil && e.StreamOuterErr == nil && e.StreamErr == nil
	case MethodChatStream:
		return e.ChatResponse == nil && e.ChatUsage == nil && e.ChatErr == nil
	default:
		return false
	}
}
