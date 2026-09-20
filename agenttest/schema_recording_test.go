package agenttest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/tool"
)

const extendedRecordingSchema = `{"type":"object","description":"","properties":{"choice":{"anyOf":[{"type":"string","description":"choice"},false]},"config":{"type":"object","properties":{"enabled":{"type":"boolean","description":"enabled"}},"additionalProperties":false},"labels":{"type":["array","null"],"items":{"type":"string","description":"label"}},"metadata":{"type":"object","additionalProperties":{"type":"string","description":"metadata value"}},"reference":{"$ref":""},"unconstrained":true},"additionalProperties":{"type":"string","description":"extra"},"anyOf":[{"$ref":"#/$defs/value"},false],"$ref":"#/$defs/root","x-vendor":{"enabled":true}}`

const canonicalExtendedRecordingSchema = `{"type":"object","properties":{"choice":{"anyOf":[{"type":"string","description":"choice"},false]},"config":{"type":"object","properties":{"enabled":{"type":"boolean","description":"enabled"}},"additionalProperties":false},"labels":{"type":["null","array"],"items":{"type":"string","description":"label"}},"metadata":{"type":"object","additionalProperties":{"type":"string","description":"metadata value"}},"reference":{"$ref":""},"unconstrained":true},"description":"","additionalProperties":{"type":"string","description":"extra"},"anyOf":[{"$ref":"#/$defs/value"},false],"$ref":"#/$defs/root","x-vendor":{"enabled":true}}`

func TestReplayerV2DecodesExtendedSchemaWithCodec(t *testing.T) {
	if _, err := NewReplayer(recordingWithSchema("1", extendedRecordingSchema)); !errors.Is(err, ErrIncompatibleRecording) {
		t.Fatalf("v1 accepted v2 schema grammar: %v", err)
	}

	decoded, err := decodeRecording(recordingWithSchema("2", extendedRecordingSchema))
	if err != nil {
		t.Fatalf("decode v2 recording: %v", err)
	}
	got, err := json.Marshal(decoded.Exchanges[0].Request.Tools[0].Parameters)
	if err != nil {
		t.Fatalf("marshal decoded schema: %v", err)
	}
	if string(got) != canonicalExtendedRecordingSchema {
		t.Fatalf("schema = %s\nwant %s", got, canonicalExtendedRecordingSchema)
	}

	replay, err := NewReplayer(recordingWithSchema("2", extendedRecordingSchema))
	if err != nil {
		t.Fatalf("NewReplayer v2: %v", err)
	}
	if _, _, err := replay.Chat(t.Context(), decoded.Exchanges[0].Request.Messages, decoded.Exchanges[0].Request.Tools); err != nil {
		t.Fatalf("replay v2 request: %v", err)
	}
	if err := replay.Verify(); err != nil {
		t.Fatalf("verify replay: %v", err)
	}
}

func TestReplayerV2RejectsDuplicateSchemaKeys(t *testing.T) {
	parameters := `{"type":"object","properties":{"value":{"type":"string"},"value":{"type":"number"}}}`
	if _, err := NewReplayer(recordingWithSchema("2", parameters)); !errors.Is(err, ErrIncompatibleRecording) {
		t.Fatalf("v2 accepted duplicate schema key: %v", err)
	}
}

func TestRecorderWritesV2ExtendedSchema(t *testing.T) {
	var schema tool.ParameterSchema
	if err := schema.UnmarshalJSON([]byte(extendedRecordingSchema)); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	request := Request{Messages: []llm.Message{llm.UserMessage("schema")}, Tools: []tool.ToolInfo{{Name: "schema", Description: "schema", Parameters: schema}}}
	recorder := NewRecorder(NewScriptedProvider(Exchange{Method: MethodChat, Request: request}))
	if _, _, err := recorder.Chat(t.Context(), request.Messages, request.Tools); err != nil {
		t.Fatalf("record chat: %v", err)
	}
	recording, err := recorder.Bytes()
	if err != nil {
		t.Fatalf("recording bytes: %v", err)
	}
	if !strings.Contains(string(recording), `"version":2`) {
		t.Fatalf("recording version = %s", recording)
	}
	replay, err := NewReplayer(recording)
	if err != nil {
		t.Fatalf("NewReplayer: %v", err)
	}
	if _, _, err := replay.Chat(t.Context(), request.Messages, request.Tools); err != nil {
		t.Fatalf("replay chat: %v", err)
	}
	if err := replay.Verify(); err != nil {
		t.Fatalf("verify replay: %v", err)
	}
}

func recordingWithSchema(version, parameters string) []byte {
	return []byte(`{"version":` + version + `,"exchanges":[{"method":"chat","request":{"messages":[],"tools":[{"name":"schema","description":"schema","parameters":` + parameters + `}],"options":{"Model":"","MaxTokens":0,"Temperature":null,"Stop":null}},"chat":{"response":null,"usage":null,"error":null}}]}`)
}
