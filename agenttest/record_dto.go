package agenttest

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dailz1/go-agent/llm"
)

func (c dtoChunk) MarshalJSON() ([]byte, error) {
	switch c.Type {
	case "text_delta":
		return json.Marshal(struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			OutputIndex int64  `json:"output_index"`
		}{c.Type, c.Text, c.OutputIndex})
	case "reasoning_delta":
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{c.Type, c.Text})
	case "tool_call_start":
		return json.Marshal(struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			ID    string `json:"id"`
			Name  string `json:"name"`
		}{c.Type, c.Index, c.ID, c.Name})
	case "tool_call_args":
		return json.Marshal(struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			ID    string `json:"id"`
			Delta string `json:"delta"`
		}{c.Type, c.Index, c.ID, c.Delta})
	case "reasoning_item":
		return json.Marshal(struct {
			Type        string                  `json:"type"`
			OutputIndex int64                   `json:"output_index"`
			Item        *llm.ReasoningItemBlock `json:"item"`
		}{c.Type, c.OutputIndex, c.Item})
	case "done":
		return json.Marshal(struct {
			Type         string     `json:"type"`
			FinishReason string     `json:"finish_reason"`
			Usage        *llm.Usage `json:"usage"`
		}{c.Type, c.FinishReason, c.Usage})
	default:
		return nil, fmt.Errorf("unknown chunk type %q", c.Type)
	}
}

func (e dtoError) MarshalJSON() ([]byte, error) {
	switch e.Kind {
	case "api":
		return json.Marshal(struct {
			Kind         string `json:"kind"`
			StatusCode   int    `json:"status_code"`
			RetryAfter   int64  `json:"retry_after"`
			Body         string `json:"body"`
			NonRetryable bool   `json:"non_retryable"`
		}{e.Kind, e.StatusCode, int64(e.RetryAfter), e.Body, e.NonRetryable})
	case "codex":
		return json.Marshal(struct {
			Kind     string     `json:"kind"`
			Category string     `json:"category"`
			Code     string     `json:"code"`
			RetryAt  *time.Time `json:"retry_at"`
			Cause    *dtoError  `json:"cause"`
		}{e.Kind, e.Category, e.Code, e.RetryAt, e.Cause})
	case "codex_auth":
		return json.Marshal(struct {
			Kind          string    `json:"kind"`
			Stage         string    `json:"stage"`
			Code          string    `json:"code"`
			Temporary     bool      `json:"temporary"`
			LoginRequired bool      `json:"login_required"`
			Message       string    `json:"message"`
			Cause         *dtoError `json:"cause"`
		}{e.Kind, e.Stage, e.Code, e.Temporary, e.LoginRequired, e.Message, e.Cause})
	case "network_timeout", "network_temporary", "network_url", "generic":
		return json.Marshal(struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}{e.Kind, e.Message})
	case "network_errno":
		return json.Marshal(struct {
			Kind  string `json:"kind"`
			Errno string `json:"errno"`
		}{e.Kind, e.Errno})
	case "context_canceled", "deadline_exceeded", "streaming_not_supported":
		return json.Marshal(struct {
			Kind string `json:"kind"`
		}{e.Kind})
	default:
		return nil, fmt.Errorf("unknown error type %q", e.Kind)
	}
}
