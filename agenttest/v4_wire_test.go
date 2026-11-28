package agenttest

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestReplayerV4SentinelGrammar(t *testing.T) {
	for _, kind := range []string{"stream_incomplete", "unexpected_eof"} {
		t.Run(kind, func(t *testing.T) {
			valid := `{"kind":"` + kind + `","message":"original text"}`
			codex := func(cause string) string {
				return `{"kind":"codex","category":"protocol","code":"","retry_at":null,"cause":` + cause + `}`
			}
			for _, payload := range []string{valid, codex(valid), strings.Replace(valid, "original text", "", 1)} {
				if _, err := NewReplayer(errorRecording("4", payload)); err != nil {
					t.Fatalf("valid v4 error %s: %v", payload, err)
				}
			}
			for _, version := range []string{"1", "2", "3"} {
				t.Run("v"+version+" rejects new identity", func(t *testing.T) {
					for _, payload := range []string{valid, codex(valid)} {
						if _, err := NewReplayer(errorRecording(version, payload)); !errors.Is(err, ErrIncompatibleRecording) {
							t.Fatalf("v%s accepted %s: %v", version, payload, err)
						}
					}
				})
			}
			for _, tc := range []struct{ name, payload string }{
				{"missing message", `{"kind":"` + kind + `"}`},
				{"missing kind", `{"message":"original text"}`},
				{"null message", `{"kind":"` + kind + `","message":null}`},
				{"number message", `{"kind":"` + kind + `","message":42}`},
				{"boolean message", `{"kind":"` + kind + `","message":false}`},
				{"array message", `{"kind":"` + kind + `","message":[]}`},
				{"object message", `{"kind":"` + kind + `","message":{}}`},
				{"extra key", strings.Replace(valid, `"message":`, `"extra":false,"message":`, 1)},
				{"duplicate message", strings.Replace(valid, `"message":`, `"message":"other","message":`, 1)},
				{"duplicate kind", strings.Replace(valid, `"kind":`, `"kind":"generic","kind":`, 1)},
				{"unknown kind", `{"kind":"new_unknown_kind","message":"original text"}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					for _, payload := range []string{tc.payload, codex(tc.payload)} {
						if _, err := NewReplayer(errorRecording("4", payload)); !errors.Is(err, ErrIncompatibleRecording) {
							t.Fatalf("invalid v4 error accepted %s: %v", payload, err)
						}
					}
				})
			}
		})
	}
}

func TestReplayerV4InheritsV3ErrorGrammar(t *testing.T) {
	api := `{"kind":"api","status_code":429,"retry_after":0,"body":"","non_retryable":false}`
	codex := `{"kind":"codex","category":"protocol","code":"","retry_at":null,"cause":` + api + `}`
	auth := `{"kind":"codex_auth","stage":"refresh","code":"","temporary":false,"login_required":true,"message":"","cause":null}`
	for _, payload := range []string{api, codex, auth} {
		if _, err := NewReplayer(errorRecording("4", payload)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, payload string }{
		{"missing API flag", strings.Replace(api, `,"non_retryable":false`, "", 1)},
		{"null API flag", strings.Replace(api, `"non_retryable":false`, `"non_retryable":null`, 1)},
		{"null body", strings.Replace(api, `"body":""`, `"body":null`, 1)},
		{"duplicate API key", strings.Replace(api, `"body":""`, `"body":"","body":""`, 1)},
		{"wrong API flag type", strings.Replace(api, `"non_retryable":false`, `"non_retryable":"false"`, 1)},
		{"missing Codex cause", strings.Replace(codex, `,"cause":`+api, "", 1)},
		{"null Codex category", strings.Replace(codex, `"category":"protocol"`, `"category":null`, 1)},
		{"missing auth cause", strings.Replace(auth, `,"cause":null`, "", 1)},
		{"null auth message", strings.Replace(auth, `"message":""`, `"message":null`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReplayer(errorRecording("4", tc.payload)); !errors.Is(err, ErrIncompatibleRecording) {
				t.Fatalf("invalid inherited grammar accepted: %v", err)
			}
		})
	}
}

func TestReplayerGenericTextDoesNotAcquireSentinelIdentity(t *testing.T) {
	for _, version := range []string{"1", "2", "3", "4"} {
		for _, sentinel := range []error{llm.ErrStreamIncomplete, io.ErrUnexpectedEOF} {
			t.Run("v"+version+"/"+sentinel.Error(), func(t *testing.T) {
				payload := fmt.Sprintf(`{"kind":"generic","message":%q}`, sentinel.Error())
				replay, err := NewReplayer(errorRecording(version, payload))
				if err != nil {
					t.Fatal(err)
				}
				_, _, got := replay.Chat(t.Context(), []llm.Message{}, nil)
				if got == nil || got.Error() != sentinel.Error() || errors.Is(got, sentinel) || llm.IsNetworkError(got) {
					t.Fatalf("generic text changed identity: %v", got)
				}
				if err := replay.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
