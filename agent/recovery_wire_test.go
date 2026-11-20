package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/llm/openai"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestRecoveryStatelessHistoryNeedsNoDurableDiscriminator(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"context_length_exceeded"}}`)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer srv.Close()
	p := openai.NewProvider("key", "model", openai.WithBaseURL(srv.URL))
	original := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: strings.Repeat("x", 2000)}}}}
	short := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "summary"}}}}
	a := New(p, tool.NewRegistry(), WithCompactor(recoveryCompactor(short)), WithLogger(discardLogger()))
	seq, err := a.RunStreamWithHistory(t.Context(), original, "continue")
	if err != nil {
		t.Fatal(err)
	}
	var done *DoneEvent
	for event, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		if e, ok := event.(DoneEvent); ok {
			done = &e
		}
	}
	if requests != 2 || done == nil || !reflect.DeepEqual(done.History[0], short[0]) {
		t.Fatalf("stateless recovery did not retain usable Go blocks: requests %d, done %+v", requests, done)
	}
}

func TestProactiveCompactionInvalidDurableCandidateKeepsHistory(t *testing.T) {
	input := strings.Repeat("x", 10000)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Messages) != 1 || body.Messages[0].Content != input {
			t.Error("provider did not receive original history")
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer srv.Close()
	p := openai.NewProvider("key", "model", openai.WithBaseURL(srv.URL))
	a := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
		WithCompactor(CompactFunc(func(context.Context, []llm.Message, CompactionBudget, llm.Provider, []llm.Option) (CompactionResult, error) {
			return CompactionResult{Changed: true, History: []llm.Message{
				{Role: llm.RoleUser, Content: []llm.ContentBlock{(*llm.TextBlock)(nil)}},
			}}, nil
		})))
	result, err := a.Run(t.Context(), input, WithThread("recovery"))
	if err != nil || requests != 1 || messageString(result.History[0]) != input {
		t.Fatalf("invalid candidate interrupted run: requests %d, result %+v, err %v", requests, result, err)
	}
	v := recoveryView(t, a)
	if !reflect.DeepEqual(v.history, result.History) {
		t.Fatal("invalid compaction changed persisted history")
	}
}

func TestRecoveryPointerCompactionAdoptsFoldedHistory(t *testing.T) {
	for _, proactive := range []bool{false, true} {
		t.Run(fmt.Sprintf("proactive=%t", proactive), func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if !proactive && requests == 1 {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"code":"context_length_exceeded"}}`)
					return
				}
				var body struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body.Messages) != 1 || body.Messages[0].Content != "summary" {
					t.Errorf("provider did not receive committed summary: %+v", body.Messages)
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
			}))
			defer srv.Close()
			p := openai.NewProvider("key", "model", openai.WithBaseURL(srv.URL))
			adopted := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{&llm.TextBlock{Type: "text", Text: "summary"}}}}
			a := New(p, tool.NewRegistry(), WithStore(store.NewMemory()), WithLogger(discardLogger()),
				WithCompactor(recoveryCompactor(adopted)))
			size := 2000
			if proactive {
				size = 10000
			}
			seq, err := a.RunStream(t.Context(), strings.Repeat("x", size), WithThread("recovery"))
			if err != nil {
				t.Fatal(err)
			}
			var done DoneEvent
			sawCompaction := false
			for event, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				switch e := event.(type) {
				case CompactionEvent:
					sawCompaction = true
					if _, ok := e.History[0].Content[0].(llm.TextBlock); !ok {
						t.Fatalf("compaction event kept pre-encoding pointer: %T", e.History[0].Content[0])
					}
				case DoneEvent:
					done = e
				}
			}
			wantRequests := 2
			if proactive {
				wantRequests = 1
			}
			if !sawCompaction || requests != wantRequests || messageString(done.Message) != "done" {
				t.Fatalf("immediate retry failed: requests %d, done %+v", requests, done)
			}
			v := recoveryView(t, a)
			if !reflect.DeepEqual(v.history, done.History) {
				t.Fatal("live history differs from committed history")
			}
		})
	}
}
