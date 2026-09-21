package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
)

func TestSettleCodecTornCancellationLine(t *testing.T) {
	r := settleCodecCancel(t)
	r.Seq = 2
	line := append(mustJSON(t, r), '\n')
	for _, cut := range []int{0, 1, len(line) / 2, len(line) - 2, len(line) - 1, len(line)} {
		t.Run(fmt.Sprintf("bytes_%d", cut), func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.NewJSONL(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Append(t.Context(), "t", 0, settleCodecPrefix(t)...); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "t.jsonl")
			prefix, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(prefix, line[:cut]...), 0o600); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.NewJSONL(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			a := New(nil, nil, WithStore(reopened))
			v, err := a.replayThread(t.Context(), "t")
			if err != nil {
				t.Fatal(err)
			}
			if cut == len(line) {
				if !v.cancelled || v.runActive || v.open != nil || v.head != 3 || len(v.history) != 4 {
					t.Fatalf("complete cancellation = %+v", v)
				}
			} else {
				if v.cancelled || !v.runActive || v.open == nil || v.head != 2 || len(v.history) != 1 {
					t.Fatalf("torn cancellation partially applied = %+v", v)
				}
				info, err := os.Stat(path)
				if err != nil || info.Size() != int64(len(prefix)) {
					t.Fatalf("torn suffix not repaired: %v, %v", info, err)
				}
			}
		})
	}
}

func TestSettleCodecDetachedSnapshot(t *testing.T) {
	for _, backend := range []string{"memory", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			var st store.Store = store.NewMemory()
			if backend == "jsonl" {
				dir := t.TempDir()
				disk, err := store.NewJSONL(dir)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := disk.Append(t.Context(), "t", 0, append(settleCodecPrefix(t), settleCodecCancel(t))...); err != nil {
					t.Fatal(err)
				}
				if err := disk.Close(); err != nil {
					t.Fatal(err)
				}
				disk, err = store.NewJSONL(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { disk.Close() })
				st = disk
			} else if _, err := st.Append(t.Context(), "t", 0, append(settleCodecPrefix(t), settleCodecCancel(t))...); err != nil {
				t.Fatal(err)
			}
			a := New(nil, nil, WithStore(st))
			original, err := st.History(t.Context(), "t", 0)
			if err != nil {
				t.Fatal(err)
			}
			first, err := a.ResumeThread(t.Context(), "t")
			if err != nil || first == nil || !first.Cancelled {
				t.Fatalf("snapshot = %+v, %v", first, err)
			}
			want := deepCopyMessages(first.History)
			first.History[0] = llm.UserMessage("mutated")
			first.History[2].Content[0].(llm.ReasoningItemBlock).Summary[0] = "mutated"
			first.History[2].Content[1].(llm.ToolUseBlock).Input[0] = '!'
			second, err := a.ResumeThread(t.Context(), "t")
			if err != nil || second == nil || !reflect.DeepEqual(second.History, want) {
				t.Fatalf("snapshot aliases previous result: %+v, %v", second, err)
			}
			after, err := st.History(t.Context(), "t", 0)
			if err != nil || !reflect.DeepEqual(after, original) {
				t.Fatalf("snapshot read mutated store: %+v, %v", after, err)
			}
			if _, err := json.Marshal(second.History); err != nil {
				t.Fatalf("snapshot no longer serializable: %v", err)
			}
		})
	}
}
