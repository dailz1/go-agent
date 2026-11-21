package source

import (
	"fmt"
	"reflect"
	"testing"
)

func TestDecodeScopesAndWindows(t *testing.T) {
	for _, tc := range []struct {
		name string
		v2   bool
		body string
		want map[string]map[string]int
	}{
		{
			name: "v1 keeps separate providers and ignores codex",
			body: `{"a":{"id":"a","models":{"same":{"id":"same","limit":{"context":10},"cost":{"future":[]}}}},"b":{"models":{"same":{"limit":{"context":20}}}},"openai-codex":[]}`,
			want: map[string]map[string]int{"a": {"same": 10}, "b": {"same": 20}},
		},
		{
			name: "v2 only adopts codex and ignores other structures", v2: true,
			body: `{"openai":[],"other":null,"openai-codex":{"id":"openai-codex","models":{"gpt-6.1-terra":{"limit":{"context":1050000},"capabilities":{"unknown":[]}},"gpt-6.1-sol":{"limit":{"context":1050000},"canonical_id":"ignore"}}}}`,
			want: map[string]map[string]int{Codex: {"gpt-6.1-terra": 1050000, "gpt-6.1-sol": 1050000}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, skipped, err := Decode([]byte(tc.body), tc.v2)
			if err != nil || skipped != 0 || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decode = %v, %d, %v; want %v", got, skipped, err, tc.want)
			}
		})
	}
}

func TestDecodeInvalidWindowsSkipped(t *testing.T) {
	for _, value := range []string{"null", "0", "-1", "1.5", `"100"`, "2147483648", "1e3", "true", "{}", "[]"} {
		t.Run(value, func(t *testing.T) {
			body := fmt.Sprintf(`{"p":{"models":{"bad":{"limit":{"context":%s,"input":100}},"missing":{},"nullLimit":{"limit":null},"ok":{"limit":{"context":2147483647}}}}}`, value)
			got, skipped, err := Decode([]byte(body), false)
			if err != nil || skipped != 3 || !reflect.DeepEqual(got["p"], map[string]int{"ok": 2147483647}) {
				t.Fatalf("decode = %v, %d, %v", got, skipped, err)
			}
		})
	}
}

func TestDecodeRejectsInvalidSource(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		v2         bool
	}{
		{"syntax", `{`, false},
		{"root array", `[]`, false},
		{"root null", `null`, false},
		{"provider container", `{"p":[]}`, false},
		{"model container", `{"p":{"models":[]}}`, false},
		{"null models", `{"p":{"models":null}}`, false},
		{"model scalar", `{"p":{"models":{"m":3}}}`, false},
		{"limit container", `{"p":{"models":{"m":{"limit":[]}}}}`, false},
		{"provider identity", `{"p":{"id":"q","models":{}}}`, false},
		{"model identity", `{"p":{"models":{"m":{"id":"n","limit":{"context":1}}}}}`, false},
		{"id type", `{"p":{"id":3,"models":{}}}`, false},
		{"empty key", `{"":{"models":{}}}`, false},
		{"second document", `{} {}`, false},
		{"no valid windows", `{"p":{"models":{"m":{"limit":{"context":0}}}}}`, false},
		{"missing codex", `{"openai":{"models":{"m":{"limit":{"context":1}}}}}`, true},
		{"empty codex", `{"openai-codex":{"models":{}}}`, true},
		{"duplicate provider", `{"p":{},"p":{}}`, false},
		{"duplicate model", `{"p":{"models":{"m":{},"m":{}}}}`, false},
		{"duplicate window", `{"p":{"models":{"m":{"limit":{"context":1,"context":2}}}}}`, false},
		{"escaped duplicate", `{"p":{"models":{"m":{},"\u006d":{}}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Decode([]byte(tc.body), tc.v2)
			if err == nil || got != nil {
				t.Fatalf("decode = %v, %v; want nil and error", got, err)
			}
		})
	}
}
