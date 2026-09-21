package agent

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/dailz1/go-agent/store"
)

func TestSettleCodecRejectsMalformedCancellation(t *testing.T) {
	for _, name := range []string{
		"wrong run", "empty run", "wrong id", "wrong round", "null round",
		"missing result", "extra result", "duplicate result", "out of order",
		"successful result", "different outcome", "extra block", "unknown block field",
		"unknown result field", "missing run key", "missing round key", "missing results key",
		"null results", "unknown field", "old schema", "no active run", "duplicate terminal",
		"unexpected round", "unexpected results",
	} {
		t.Run(name, func(t *testing.T) {
			prefix := settleCodecPrefix(t)
			r := settleCodecCancel(t)
			var p map[string]json.RawMessage
			if err := json.Unmarshal(r.Payload, &p); err != nil {
				t.Fatal(err)
			}
			var results []map[string]json.RawMessage
			if err := json.Unmarshal(p["results"], &results); err != nil {
				t.Fatal(err)
			}
			var blocks []map[string]json.RawMessage
			if err := json.Unmarshal(results[0]["content"], &blocks); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "wrong run":
				p["run_id"] = mustJSON(t, "other")
			case "empty run":
				p["run_id"] = mustJSON(t, "")
			case "wrong id":
				r.ID = "another-cancel"
			case "wrong round":
				p["open_round"] = []byte("1")
			case "null round":
				p["open_round"] = []byte("null")
			case "missing result":
				results = results[:1]
			case "extra result":
				results = append(results, results[0])
			case "duplicate result":
				results[1] = results[0]
			case "out of order":
				results[0], results[1] = results[1], results[0]
			case "successful result":
				blocks[0]["is_error"] = []byte("false")
				results[0]["content"] = mustJSON(t, blocks)
			case "different outcome":
				blocks[0]["content"] = mustJSON(t, "a different failure")
				results[0]["content"] = mustJSON(t, blocks)
			case "extra block":
				blocks = append(blocks, map[string]json.RawMessage{"type": mustJSON(t, "text"), "text": mustJSON(t, "extra")})
				results[0]["content"] = mustJSON(t, blocks)
			case "unknown block field":
				blocks[0]["future"] = []byte("true")
				results[0]["content"] = mustJSON(t, blocks)
			case "unknown result field":
				results[0]["future"] = []byte("true")
			case "missing run key":
				delete(p, "run_id")
			case "missing round key":
				delete(p, "open_round")
			case "unknown field":
				p["future"] = []byte("true")
			case "old schema":
				r.Schema = store.SchemaV1
			case "no active run":
				prefix = nil
			case "duplicate terminal":
				prefix = append(prefix, r)
			case "unexpected round", "unexpected results":
				prefix = prefix[:1]
				if name == "unexpected round" {
					results = []map[string]json.RawMessage{}
				} else {
					p["open_round"] = []byte("null")
				}
			}
			p["results"] = mustJSON(t, results)
			if name == "missing results key" {
				delete(p, "results")
			}
			if name == "null results" {
				p["results"] = []byte("null")
			}
			r.Payload = mustJSON(t, p)
			a := New(nil, nil, WithStore(settleCodecStore(append(prefix, r))))
			if _, err := a.ResumeThread(t.Context(), "t"); !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("malformed cancellation = %v, want ErrIncompatibleLog", err)
			}
		})
	}
}
