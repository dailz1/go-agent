package agenttest

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
)

func TestReplayerV1V2FixtureCompatibility(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		t.Run("v"+version, func(t *testing.T) {
			data, err := os.ReadFile("testdata/recording_v" + version + ".json")
			if err != nil {
				t.Fatal(err)
			}
			replay, err := NewReplayer(data)
			if err != nil {
				t.Fatal(err)
			}
			_, _, got := replay.Chat(t.Context(), nil, nil)
			var api *llm.APIError
			if !errors.As(got, &api) || api.StatusCode != 429 || !api.Retryable() {
				t.Fatalf("legacy API classification = %v", got)
			}
			if err := replay.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReplayerV3Fixture(t *testing.T) {
	data, err := os.ReadFile("testdata/recording_v3.json")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := NewReplayer(data)
	if err != nil {
		t.Fatal(err)
	}
	_, _, got := replay.Chat(t.Context(), nil, nil)
	var api *llm.APIError
	if !errors.As(got, &api) || api.Retryable() || llm.IsRetryableError(got) {
		t.Fatalf("quota classification = %v", got)
	}
	if err := replay.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayerV3StrictErrorGrammar(t *testing.T) {
	api := `{"kind":"api","status_code":429,"retry_after":0,"body":"","non_retryable":false}`
	codex := `{"kind":"codex","category":"quota","code":"","retry_at":null,"cause":` + api + `}`
	auth := `{"kind":"codex_auth","stage":"refresh","code":"","temporary":false,"login_required":true,"message":"","cause":null}`
	for name, payload := range map[string]string{
		"api": api, "codex": codex, "auth": auth,
		"nested":   strings.Replace(codex, api, auth, 1),
		"retry_at": strings.Replace(codex, `"retry_at":null`, `"retry_at":"2026-09-22T01:02:03.123456789Z"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewReplayer(errorRecording("3", payload)); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, payload := range map[string]string{
		"missing non_retryable": strings.Replace(api, `,"non_retryable":false`, "", 1),
		"null non_retryable":    strings.Replace(api, `false`, `null`, 1),
		"string non_retryable":  strings.Replace(api, `false`, `"false"`, 1),
		"null status":           strings.Replace(api, `429`, `null`, 1),
		"unknown field":         strings.Replace(api, `"body":""`, `"body":"","extra":true`, 1),
		"unknown kind":          `{"kind":"unknown"}`,
		"invalid category":      strings.Replace(codex, `"quota"`, `"rate"`, 1),
		"null code":             strings.Replace(codex, `"code":""`, `"code":null`, 1),
		"missing cause":         strings.Replace(auth, `,"cause":null`, "", 1),
		"invalid stage":         strings.Replace(auth, `"refresh"`, `"login"`, 1),
		"null temporary":        strings.Replace(auth, `"temporary":false`, `"temporary":null`, 1),
		"string login_required": strings.Replace(auth, `"login_required":true`, `"login_required":"true"`, 1),
		"missing message":       strings.Replace(auth, `,"message":""`, "", 1),
		"invalid timestamp":     strings.Replace(codex, `"retry_at":null`, `"retry_at":"tomorrow"`, 1),
		"numeric timestamp":     strings.Replace(codex, `"retry_at":null`, `"retry_at":123`, 1),
		"missing timestamp":     strings.Replace(codex, `,"retry_at":null`, "", 1),
		"codex under codex":     strings.Replace(codex, api, codex, 1),
		"auth under auth":       strings.Replace(auth, `"cause":null`, `"cause":`+auth, 1),
		"codex under auth":      strings.Replace(auth, `"cause":null`, `"cause":`+codex, 1),
		"array cause":           strings.Replace(auth, `"cause":null`, `"cause":[]`, 1),
		"duplicate field":       strings.Replace(api, `"kind":"api"`, `"kind":"generic","kind":"api"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewReplayer(errorRecording("3", payload)); !errors.Is(err, ErrIncompatibleRecording) {
				t.Fatalf("malformed error accepted: %v", err)
			}
		})
	}
	for _, version := range []string{"1", "2"} {
		for _, payload := range []string{api, codex, auth} {
			if _, err := NewReplayer(errorRecording(version, payload)); !errors.Is(err, ErrIncompatibleRecording) {
				t.Fatalf("v%s accepted v3 error: %s: %v", version, payload, err)
			}
		}
	}
}

func errorRecording(version, payload string) []byte {
	return []byte(`{"version":` + version + `,"exchanges":[{"method":"chat","request":` + validWireRequest + `,"chat":{"response":null,"usage":null,"error":` + payload + `}}]}`)
}
