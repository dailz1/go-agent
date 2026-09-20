package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

// MockProvider records calls and returns preset responses in order.
type MockProvider struct {
	responses    []mockResponse
	index        int
	LastMessages []llm.Message
	LastTools    []tool.ToolInfo
	failBefore   int   // fail this many times with failErr before using responses
	failErr      error // error to return for failures
	failCount    int   // tracks how many times we've failed
}

type mockResponse struct {
	msg   *llm.Message
	usage *llm.Usage
	err   error
}

func NewMockProvider(responses ...mockResponse) *MockProvider {
	return &MockProvider{responses: responses}
}

func MsgResponse(msg llm.Message) mockResponse {
	return mockResponse{msg: &msg}
}

func ErrResponse(err error) mockResponse {
	return mockResponse{err: err}
}

func MsgWithUsageResponse(msg llm.Message, usage *llm.Usage) mockResponse {
	return mockResponse{msg: &msg, usage: usage}
}

func (m *MockProvider) Name() string { return "mock" }

func (m *MockProvider) Chat(_ context.Context, messages []llm.Message, tools []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	m.LastMessages = messages
	m.LastTools = tools
	if m.failBefore > 0 && m.failCount < m.failBefore {
		m.failCount++
		return nil, nil, m.failErr
	}
	if m.index >= len(m.responses) {
		return nil, nil, fmt.Errorf("mock: no more responses (called %d times)", m.index+1)
	}
	resp := m.responses[m.index]
	m.index++
	return resp.msg, resp.usage, resp.err
}

func (m *MockProvider) ChatStream(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	return nil, llm.ErrStreamingNotSupported
}

// NewRetryableMockProvider creates a mock that returns failErr failCount times,
// then returns successMsg. Used for retry testing.
func NewRetryableMockProvider(failErr error, failCount int, successMsg llm.Message) *MockProvider {
	return &MockProvider{
		responses:  []mockResponse{{msg: &successMsg}},
		failBefore: failCount,
		failErr:    failErr,
	}
}

// mockTool is a test double for tool.Tool.
type mockTool struct {
	info   tool.ToolInfo
	result *tool.ToolResult
	err    error
}

func (m *mockTool) Info() tool.ToolInfo { return m.info }

func (m *mockTool) Execute(_ context.Context, _ json.RawMessage) (*tool.ToolResult, error) {
	return m.result, m.err
}

// MockStreamingProvider records calls and returns preset chunk slices for streaming tests.
type MockStreamingProvider struct {
	chunks       [][]llm.Chunk
	index        int
	streamErr    error // if set, ChatStream returns this error immediately
	failBefore   int   // fail this many ChatStream calls with failErr
	failErr      error // error to return for failures
	failCount    int   // tracks failure count
	LastMessages []llm.Message
	LastTools    []tool.ToolInfo
}

func NewMockStreamingProvider(responses [][]llm.Chunk) *MockStreamingProvider {
	return &MockStreamingProvider{chunks: responses}
}

// StreamErrResponse is a helper that returns nil to signal an error-only response.
func StreamErrResponse(err error) [][]llm.Chunk {
	return nil
}

func (m *MockStreamingProvider) Name() string { return "mock_streaming" }

func (m *MockStreamingProvider) Chat(_ context.Context, _ []llm.Message, _ []tool.ToolInfo, _ ...llm.Option) (*llm.Message, *llm.Usage, error) {
	return nil, nil, llm.ErrStreamingNotSupported
}

func (m *MockStreamingProvider) ChatStream(_ context.Context, messages []llm.Message, tools []tool.ToolInfo, _ ...llm.Option) (iter.Seq2[llm.Chunk, error], error) {
	m.LastMessages = messages
	m.LastTools = tools
	if m.failBefore > 0 && m.failCount < m.failBefore {
		m.failCount++
		return nil, m.failErr
	}
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	if m.index >= len(m.chunks) {
		return nil, fmt.Errorf("mock streaming: no more responses (called %d times)", m.index+1)
	}
	chunks := m.chunks[m.index]
	m.index++

	return func(yield func(llm.Chunk, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

// WithStreamError configures the mock to return an outer error from ChatStream.
func (m *MockStreamingProvider) WithStreamError(err error) *MockStreamingProvider {
	m.streamErr = err
	return m
}

// NewRetryableStreamingMockProvider creates a streaming mock that returns failErr
// failCount times from ChatStream, then streams successChunks. Used for retry testing.
func NewRetryableStreamingMockProvider(failErr error, failCount int, successChunks [][]llm.Chunk) *MockStreamingProvider {
	return &MockStreamingProvider{
		chunks:     successChunks,
		failBefore: failCount,
		failErr:    failErr,
	}
}
