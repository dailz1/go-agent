package agent

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/llm"
	"github.com/dailz1/go-agent/store"
	"github.com/dailz1/go-agent/tool"
)

func TestRecoveryCheckpointReserializesConfigAfterConflict(t *testing.T) {
	st := store.NewMemory()
	log := recoveryLogFixture(t)[:4]
	if _, err := st.Append(t.Context(), "t", 0, log...); err != nil {
		t.Fatal(err)
	}
	a := New(NewMockProvider(), tool.NewRegistry(), WithStore(st))
	p, history, err := a.openSession(t.Context(), st, "t", "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()
	frozen := *p.recovery
	if err := SetThreadConfig(t.Context(), st, "t", ConfigChange{Model: strptr("new-model")}); err != nil {
		t.Fatal(err)
	}
	if err := p.checkpoint(t.Context(), history); err != nil {
		t.Fatal(err)
	}
	state, err := st.Latest(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	cp, err := decodeCheckpoint(*state.Checkpoint)
	if err != nil || cp == nil || cp.ConfigSeq == nil || *cp.ConfigSeq != 4 ||
		cp.Config.Model == nil || *cp.Config.Model != "new-model" || !reflect.DeepEqual(cp.Recovery, &frozen) ||
		p.cfgSeq != 4 {
		t.Fatalf("rebased checkpoint lost live config or changed frozen recovery: %+v, %v", cp, err)
	}
}

func TestRecoveryRunStartRebasePreservesConfigSeqZero(t *testing.T) {
	st := &injectBeforeStore{Store: store.NewMemory()}
	a := New(NewMockProvider(), tool.NewRegistry(), WithStore(st))
	p, _, err := a.openSession(t.Context(), st, "t", "input", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()
	if p.cfgSeq != 0 || p.cfg.Model == nil || *p.cfg.Model != "model-b" {
		t.Fatalf("seq zero lost by run-start rebase: %+v", p)
	}
}

func TestRecoveryCheckpointScanUsesLatestHead(t *testing.T) {
	for _, hideV4 := range []bool{false, true} {
		t.Run(map[bool]string{false: "adopt legacy", true: "fallback legacy"}[hideV4], func(t *testing.T) {
			st := store.NewMemory()
			log := recoveryLogFixture(t)
			if !hideV4 {
				log = log[:1]
			}
			cp := mustRecord(t, store.KindCheckpoint, "legacy", agentCheckpoint{
				CodecVersion: store.SchemaV3, History: []llm.Message{llm.UserMessage("legacy view")},
				RunActive: true, RunID: "r", LastRound: -1,
			})
			cp.Schema = store.SchemaV3
			log = append(log, cp)
			if _, err := st.Append(t.Context(), "t", 0, log...); err != nil {
				t.Fatal(err)
			}
			concurrent := &enqueueAfterLatestStore{Store: st}
			a := New(NewMockProvider(), tool.NewRegistry(), WithStore(concurrent))
			v, err := a.replayThread(t.Context(), "t")
			if err != nil || v.head != int64(len(log)) || len(v.queue) != 0 {
				t.Fatalf("scan crossed Latest head: %+v, %v", v, err)
			}
			if hideV4 != (v.recovery != nil) {
				t.Fatalf("legacy adoption recovery = %+v", v.recovery)
			}
			p := &persistence{a: a, thread: "t", head: v.head, cfgRead: v.head, cfgSeq: v.cfgSeq, queue: v.queue}
			if err := p.absorbTail(t.Context(), v.head); err != nil {
				t.Fatal(err)
			}
			if len(p.queue) != 1 || p.queue[0].ID != "q-h1" {
				t.Fatalf("concurrent queue was not absorbed exactly once: %+v", p.queue)
			}
		})
	}
}

func TestRecoverySchemaFourRejectsLegacyKinds(t *testing.T) {
	for _, kind := range []string{store.KindRunStarted, store.KindConfigChanged, store.KindAgentEvent, store.KindError} {
		t.Run(kind, func(t *testing.T) {
			r := store.Record{Kind: kind, Schema: store.SchemaV4, ID: "invalid", Payload: []byte(`{}`)}
			_, err := replayRecords(threadView{lastRound: -1, cfgSeq: -1}, []store.Record{r})
			if !errors.Is(err, ErrIncompatibleLog) {
				t.Fatalf("schema 4 %s: %v", kind, err)
			}
		})
	}
}
