package mcpbridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/dailz1/go-agent/pkg/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mapperError struct{ err error }

func (e *mapperError) Error() string { return e.err.Error() }
func (e *mapperError) Unwrap() error { return e.err }

type dataEnvelope struct {
	Content []dataEntry `json:"mcp_content"`
}

type dataEntry struct {
	Index       int    `json:"index"`
	Type        string `json:"type"`
	MIMEType    string `json:"mimeType,omitempty"`
	Data        []byte `json:"data,omitempty"`
	URI         string `json:"uri,omitempty"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Size        *int64 `json:"size,omitempty"`
	Blob        []byte `json:"blob,omitempty"`
}

func (e dataEntry) MarshalJSON() ([]byte, error) {
	if e.Type == "image" || e.Type == "audio" {
		data := e.Data
		if data == nil {
			data = []byte{}
		}
		return json.Marshal(struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
			MIME  string `json:"mimeType"`
			Data  []byte `json:"data"`
		}{e.Index, e.Type, e.MIMEType, data})
	}
	type ordinary dataEntry
	return json.Marshal(ordinary(e))
}

func mapResult(result *sdk.CallToolResult, limit int64) (*tool.ToolResult, error) {
	var fragments []string
	var entries []dataEntry
	for index, content := range result.Content {
		fragment, entry, err := mapContent(index, content)
		if err != nil {
			return nil, &mapperError{err: err}
		}
		if entry != nil {
			candidate := append(append([]dataEntry(nil), entries...), *entry)
			encoded, err := json.Marshal(dataEnvelope{Content: candidate})
			if err != nil {
				return nil, fmt.Errorf("marshal MCP result data: %w", err)
			}
			if int64(len(encoded)) > limit {
				fragment = "[mcp:" + entry.Type + " omitted: bridge data limit]" + embeddedText(content)
			} else {
				entries = candidate
			}
		}
		fragments = append(fragments, fragment)
	}
	if result.StructuredContent != nil {
		structured, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("marshal MCP structured content: %w", err)
		}
		fragments = append(fragments, string(structured))
	}
	out := &tool.ToolResult{Content: strings.Join(fragments, "\n")}
	if len(entries) != 0 {
		data, err := json.Marshal(dataEnvelope{Content: entries})
		if err != nil {
			return nil, fmt.Errorf("marshal MCP result data: %w", err)
		}
		out.Data = data
	}
	return out, nil
}

func mapContent(index int, content sdk.Content) (string, *dataEntry, error) {
	switch c := content.(type) {
	case *sdk.TextContent:
		return c.Text, nil, nil
	case *sdk.ImageContent:
		data := c.Data
		if data == nil {
			data = []byte{}
		}
		entry := &dataEntry{Index: index, Type: "image", MIMEType: c.MIMEType, Data: data}
		return binaryPlaceholder("image", c.MIMEType, len(data)), entry, nil
	case *sdk.AudioContent:
		data := c.Data
		if data == nil {
			data = []byte{}
		}
		entry := &dataEntry{Index: index, Type: "audio", MIMEType: c.MIMEType, Data: data}
		return binaryPlaceholder("audio", c.MIMEType, len(data)), entry, nil
	case *sdk.ResourceLink:
		entry := &dataEntry{Index: index, Type: "resource_link", URI: c.URI, Name: c.Name, Title: c.Title, Description: c.Description, MIMEType: c.MIMEType, Size: c.Size}
		return resourceLinkPlaceholder(c), entry, nil
	case *sdk.EmbeddedResource:
		if c.Resource == nil {
			return "", nil, fmt.Errorf("MCP embedded resource is nil")
		}
		r := c.Resource
		entry := &dataEntry{Index: index, Type: "resource", URI: r.URI, MIMEType: r.MIMEType, Blob: r.Blob}
		return resourcePlaceholder(r), entry, nil
	default:
		return "", nil, fmt.Errorf("unsupported decoded MCP content type %T", content)
	}
}

func binaryPlaceholder(kind, mime string, bytes int) string {
	return "[mcp:" + kind + ` mimeType="` + escapePlaceholder(mime) + `" bytes=` + strconv.Itoa(bytes) + "]"
}

func resourceLinkPlaceholder(c *sdk.ResourceLink) string {
	return `[mcp:resource_link name="` + escapePlaceholder(c.Name) + `" uri="` + escapePlaceholder(c.URI) + `" title="` + escapePlaceholder(c.Title) + `" mimeType="` + escapePlaceholder(c.MIMEType) + `"]`
}

func resourcePlaceholder(r *sdk.ResourceContents) string {
	out := `[mcp:resource uri="` + escapePlaceholder(r.URI) + `" mimeType="` + escapePlaceholder(r.MIMEType) + `" bytes=` + strconv.Itoa(len(r.Blob)) + "]"
	if r.Text != "" {
		out += "\n" + r.Text
	}
	return out
}

func embeddedText(content sdk.Content) string {
	if c, ok := content.(*sdk.EmbeddedResource); ok && c.Resource != nil && c.Resource.Text != "" {
		return "\n" + c.Resource.Text
	}
	return ""
}

func escapePlaceholder(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		case ']':
			b.WriteString(`\]`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
