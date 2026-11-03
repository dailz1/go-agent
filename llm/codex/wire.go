package codex

import (
	"encoding/json"
	"strings"
)

type createRequest struct {
	Model        string      `json:"model"`
	Instructions string      `json:"instructions"`
	Input        []inputItem `json:"input"`
	Tools        []toolDef   `json:"tools,omitempty"`
	Store        bool        `json:"store"`
	Stream       bool        `json:"stream"`
	Include      []string    `json:"include"`
}

type inputItem struct {
	Type             string         `json:"type"`
	Role             string         `json:"role,omitempty"`
	Content          []contentPart  `json:"content,omitempty"`
	CallID           string         `json:"call_id,omitempty"`
	Name             string         `json:"name,omitempty"`
	Arguments        *string        `json:"arguments,omitempty"`
	Output           *outputContent `json:"output,omitempty"`
	ID               string         `json:"id,omitempty"`
	EncryptedContent string         `json:"encrypted_content,omitempty"`
	Summary          []summaryPart  `json:"summary,omitempty"`
}

type contentPart struct {
	Type     string  `json:"type"`
	Text     *string `json:"text,omitempty"`
	Refusal  *string `json:"refusal,omitempty"`
	ImageURL string  `json:"image_url,omitempty"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// outputContent is the function_call_output payload: a JSON string (text
// only) or an ordered array of typed parts (text + images). Pointer nil
// omits the field; empty text without images still marshals as "" so the
// pre-union `"output":""` byte shape is preserved.
type outputContent struct {
	text  string
	parts []contentPart
}

func (c outputContent) MarshalJSON() ([]byte, error) {
	if len(c.parts) == 0 {
		return json.Marshal(c.text)
	}
	return json.Marshal(c.parts)
}

func (c *outputContent) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "\"") {
		return json.Unmarshal(data, &c.text)
	}
	var parts []contentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	c.parts = parts
	return nil
}

type toolDef struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type outputItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Role             string        `json:"role"`
	Status           string        `json:"status"`
	CallID           string        `json:"call_id"`
	Name             string        `json:"name"`
	Arguments        *string       `json:"arguments"`
	EncryptedContent string        `json:"encrypted_content"`
	Summary          []summaryPart `json:"summary"`
	Content          []contentPart `json:"content"`
}

type responseEnvelope struct {
	Status string         `json:"status"`
	Output []outputItem   `json:"output"`
	Usage  *usageEnvelope `json:"usage"`
	Error  *serverError   `json:"error"`
}

type usageEnvelope struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	Details      struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type serverError struct {
	Code     string          `json:"code"`
	Type     string          `json:"type"`
	ResetAt  json.RawMessage `json:"reset_at"`
	ResetsAt json.RawMessage `json:"resets_at"`
}

type streamEvent struct {
	Type         string            `json:"type"`
	OutputIndex  *int              `json:"output_index"`
	ContentIndex *int              `json:"content_index"`
	SummaryIndex *int              `json:"summary_index"`
	ItemID       string            `json:"item_id"`
	Delta        *string           `json:"delta"`
	Text         *string           `json:"text"`
	Refusal      *string           `json:"refusal"`
	Arguments    *string           `json:"arguments"`
	Item         *outputItem       `json:"item"`
	Part         *contentPart      `json:"part"`
	Response     *responseEnvelope `json:"response"`
	Error        *serverError      `json:"error"`
	Code         string            `json:"code"`
}
