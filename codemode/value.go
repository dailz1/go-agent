package codemode

import (
	"encoding/json"

	"github.com/dailz1/go-agent/tool"
)

// ImageRef is an opaque reference to one image produced by a nested tool
// call inside the current execution (D11.1). The token is a handle minted
// per execution; it grants no access to any host resource. Cross-execution
// or forged refs fail projection with a soft error.
type ImageRef struct {
	Token string `json:"ref"`
}

// ToolValue is the script-facing projection of one nested ToolResult
// (D11.1). The exact logic fields are Content, optional JSON Data, ordered
// Images and the original Status:
//
//   - Data present and valid JSON: Data and Value both carry that JSON
//     value (number, array, string, null — "null" as Data counts as
//     present).
//   - Data absent: the data field is omitted and Value is the Content
//     string. Content is never sniffed as JSON.
//   - Status=error: the full bounded payload is still carried; the reply
//     rejects the script-side promise with a Failure whose Value holds this
//     encoded ToolValue.
type ToolValue struct {
	Value   json.RawMessage   `json:"value"`
	Content string            `json:"content"`
	Data    json.RawMessage   `json:"data,omitempty"`
	Images  []ImageRef        `json:"images,omitempty"`
	Status  tool.ResultStatus `json:"status,omitempty"`
}

// dataPresent distinguishes absent Data (nil) from explicit JSON null.
func dataPresent(raw json.RawMessage) bool {
	return len(raw) > 0
}

// newToolValue projects one bounded ToolResult into its script-facing form.
// Image tokens must already be minted by the caller (the bridge owns the
// execution image table). The result is plain JSON-safe: Content is copied,
// Data is validated JSON, Images are opaque refs.
func newToolValue(res *tool.ToolResult, refs []ImageRef) (*ToolValue, error) {
	v := &ToolValue{
		Content: res.Content,
		Images:  refs,
		Status:  res.Status,
	}
	if dataPresent(res.Data) {
		if !json.Valid(res.Data) {
			return nil, &Failure{
				Kind:    FailureBridgeInvalid,
				Message: "tool result data is not valid JSON",
			}
		}
		v.Data = append(json.RawMessage(nil), res.Data...)
		v.Value = v.Data
	} else {
		enc, err := json.Marshal(res.Content)
		if err != nil {
			return nil, &Failure{Kind: FailureInternal, Message: "encode content string"}
		}
		v.Value = enc
	}
	return v, nil
}
