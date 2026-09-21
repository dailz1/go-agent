package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/store"
)

func TestSettleMaxIterPrefixes(t *testing.T) {
	for _, failOn := range []int{3, 4, 0} {
		name := map[int]string{3: "E37_declaration", 4: "E37_committed", 0: "E37_done"}[failOn]
		t.Run(name, func(t *testing.T) {
			st := &failAppendStore{Store: store.NewMemory(), failOnCall: failOn}
			ag := terminalStoreAgent(t, st)
			var target SettlementToken
			WithRunExitFn(func(value SettlementToken) { target = value })(ag)
			_, runErr := ag.RunThread(context.Background(), "t", "old")
			if (runErr != nil) != (failOn != 0) {
				t.Fatalf("run error = %v", runErr)
			}
			if err := ag.SettleThread(context.Background(), target); failOn == 0 {
				if !errors.Is(err, ErrNothingToSettle) {
					t.Fatal(err)
				}
				return
			} else if err != nil {
				t.Fatal(err)
			}
			res, err := ag.ResumeThread(context.Background(), "t")
			if err != nil || !res.Cancelled {
				t.Fatalf("snapshot = %#v %v", res, err)
			}
			for _, message := range res.History {
				for _, result := range toolResultBlocks(message) {
					if !result.IsError || strings.Contains(result.Content, "unknown") != (failOn == 3) {
						t.Fatalf("wrong certainty after partial precommit: %#v", result)
					}
				}
			}
		})
	}
}
