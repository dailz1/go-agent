package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// driftingTool returns a different aspect of its Info() on every call and
// counts the calls, for freeze-semantics assertions.
type driftingTool struct {
	mu        sync.Mutex
	calls     int
	nameShift int
	schema    ParameterSchema
	desc      string
}

func (d *driftingTool) Info() ToolInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	name := fmt.Sprintf("drift_%d", d.nameShift)
	d.nameShift++
	return ToolInfo{Name: name, Description: d.desc, Parameters: deepCopyParameterSchema(d.schema)}
}

func (d *driftingTool) Execute(_ context.Context, _ json.RawMessage) (*ToolResult, error) {
	return NewTextResult("drift"), nil
}

func paramTool(name string, schema ParameterSchema) *mockTool {
	m := newMock(name)
	m.info.Parameters = schema
	return m
}

func mustSchema(t *testing.T, raw string) ParameterSchema {
	t.Helper()
	var s ParameterSchema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("Unmarshal schema %q: %v", raw, err)
	}
	return s
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

// rebuildCount reads the registry's snapshot rebuild counter for the
// snapshot-lifecycle assertions.
func rebuildCount(r *Registry) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rebuilds
}

func TestListNoUserCodeUnderLock(t *testing.T) {
	r := NewRegistry()
	var reentrant atomic.Bool
	reentrantTool := &mockTool{}
	reentrantTool.info = ToolInfo{
		Name:        "reentrant",
		Description: "calls List inside Info",
		Parameters:  NewParameterSchema(),
	}
	// Register reentrantTool with an Info that re-enters the registry on
	// every call after the first (the first happens during its own freeze).
	reentrantHook := &hookTool{
		inner: reentrantTool,
		hook: func() {
			if reentrant.CompareAndSwap(false, true) {
				return
			}
			r.List()
		},
	}
	if err := r.Register(reentrantHook); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Churn writers in the background so a queued writer is likely present
	// while List executes; the old implementation deadlocked in this shape.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			name := fmt.Sprintf("churn_%d", i)
			i++
			_ = r.Register(paramTool(name, NewParameterSchema()))
			_ = r.Unregister(name)
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			infos := r.List()
			if len(infos) == 0 {
				t.Error("List returned no tools")
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("registry deadlock: List ran user Info() under lock")
	}
	close(stop)
	wg.Wait()
}

type hookTool struct {
	inner Tool
	hook  func()
}

func (h *hookTool) Info() ToolInfo {
	h.hook()
	return h.inner.Info()
}

func (h *hookTool) Execute(ctx context.Context, args json.RawMessage) (*ToolResult, error) {
	return h.inner.Execute(ctx, args)
}

func TestListInfoCallCount(t *testing.T) {
	r := NewRegistry()
	d := &driftingTool{desc: "x", schema: NewParameterSchema()}
	if err := r.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d.mu.Lock()
	d.calls = 0
	d.mu.Unlock()

	for i := 0; i < 5; i++ {
		_ = r.List()
	}
	d.mu.Lock()
	calls := d.calls
	d.mu.Unlock()
	if calls != 0 {
		t.Fatalf("Info called %d times after registration, want 0", calls)
	}
}

func TestListFrozenAtRegistration(t *testing.T) {
	t.Run("internal state drift after registration", func(t *testing.T) {
		r := NewRegistry()
		m := paramTool("frozen", mustSchema(t, `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`))
		if err := r.Register(m); err != nil {
			t.Fatalf("Register: %v", err)
		}
		before := r.List()

		m.info.Description = "mutated description"
		m.info.Parameters = mustSchema(t, `{"type":"object","properties":{"b":{"type":"number"}}}`)

		after := r.List()
		if after[0].Description != before[0].Description {
			t.Errorf("description drifted: got %q, want %q", after[0].Description, before[0].Description)
		}
		if !reflect.DeepEqual(after, before) {
			t.Error("List reflected post-registration mutation of the tool's internal state")
		}
	})

	t.Run("drifting Info name", func(t *testing.T) {
		r := NewRegistry()
		d := &driftingTool{desc: "d", schema: NewParameterSchema()}
		// The name at freeze time is the first value Info() returns.
		err := r.Register(d)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		d.mu.Lock()
		first := fmt.Sprintf("drift_%d", d.nameShift-1)
		d.mu.Unlock()

		infos := r.List()
		if len(infos) != 1 || infos[0].Name != first {
			t.Fatalf("List returned %+v, want frozen name %q", infos, first)
		}
		if _, ok := r.Get(first); !ok {
			t.Errorf("Get(%q) missed: registry key and List display name diverged", first)
		}
	})
}

func TestListSnapshotLifecycle(t *testing.T) {
	r := NewRegistry()

	r.List()
	if r.snapshot == nil {
		t.Fatal("snapshot of empty registry is nil after List, want non-nil empty slice")
	}
	if len(r.snapshot) != 0 {
		t.Fatalf("empty registry snapshot has %d entries, want 0", len(r.snapshot))
	}
	rebuilds := rebuildCount(r)
	r.List()
	if rebuildCount(r) != rebuilds {
		t.Fatal("second List on empty registry rebuilt the snapshot")
	}

	if err := r.Register(newMock("a")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r.snapshot != nil {
		t.Fatal("snapshot not dropped after Register")
	}
	r.List()
	if r.snapshot == nil {
		t.Fatal("snapshot not rebuilt after List")
	}
	rebuilds = rebuildCount(r)
	r.List()
	if rebuildCount(r) != rebuilds {
		t.Fatal("steady-state List rebuilt the snapshot")
	}

	err := r.Apply(
		[]string{"a"},
		[]Tool{paramTool("b", NewParameterSchema()), paramTool("c", NewParameterSchema())},
	)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r.snapshot != nil {
		t.Fatal("snapshot not dropped after Apply")
	}
	r.List()
	rebuilds = rebuildCount(r)
	r.List()
	if rebuildCount(r) != rebuilds {
		t.Fatal("second List after batch Apply rebuilt the snapshot; batch must fold into one rebuild")
	}

	if r.Unregister("missing") {
		t.Fatal("Unregister of absent name reported removal")
	}
	if r.snapshot == nil {
		t.Fatal("snapshot dropped by no-op Unregister; zero-change calls must not dirty it")
	}
	if !r.Unregister("b") {
		t.Fatal("Unregister of present name reported no removal")
	}
	if r.snapshot != nil {
		t.Fatal("snapshot not dropped after actual removal")
	}
}

func TestListDeepCopyIsolation(t *testing.T) {
	r := NewRegistry()
	schema := mustSchema(t, `{
		"type":"object",
		"properties":{
			"a":{"type":"array","items":{"type":"string","enum":["x"]}},
			"b":{"anyOf":[{"type":"string"},{"type":"number"}]},
			"c":{"type":"string","description":""}
		},
		"required":["a"],
		"additionalProperties":{"type":"boolean"},
		"format":"table"
	}`)
	if err := r.Register(paramTool("iso", schema)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	baseline := r.List()

	list := r.List()
	list[0].Name = "renamed"
	list[0].Description = "mutated"
	params := &list[0].Parameters
	params.Description = "mutated"
	params.Required[0] = "hijacked"
	if len(params.Keywords) == 0 {
		t.Fatal("test schema must carry a root-level carrier keyword")
	}
	params.Keywords[0].Value = []byte(`false`)
	if pa, ok := params.Properties["a"]; ok {
		pa.Items.Enum[0] = "hijacked"
		params.Properties["a"] = pa
	}
	if pb, ok := params.Properties["b"]; ok {
		pb.AnyOf[0].Type = "hijacked"
		params.Properties["b"] = pb
	}
	if params.AdditionalPropertiesSchema != nil {
		*params.AdditionalPropertiesSchema = Property{Type: "hijacked"}
	}

	after := r.List()
	if !reflect.DeepEqual(after, baseline) {
		t.Error("mutating a returned List value corrupted the registry view")
	}
	fresh, err := json.Marshal(after[0].Parameters)
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	want, err := json.Marshal(baseline[0].Parameters)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	if !bytesEqual(fresh, want) {
		t.Errorf("registry bytes drifted after consumer mutation:\n want:  %s\n fresh: %s", want, fresh)
	}
}

func TestFreezeFidelityBytes(t *testing.T) {
	cases := map[string]string{
		"nullable":       `{"type":"object","properties":{"a":{"type":"string","nullable":true}}}`,
		"nested_items":   `{"type":"object","properties":{"a":{"type":"array","items":{"type":"number"}}}}`,
		"any_of":         `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"number"}]}}}`,
		"one_of":         `{"type":"object","properties":{"a":{"oneOf":[{"type":"string"}]}}}`,
		"all_of":         `{"type":"object","properties":{"a":{"allOf":[{"type":"string"}]}}}`,
		"ref":            `{"type":"object","properties":{"a":{"$ref":"#/x"}}}`,
		"boolean_schema": `{"type":"object","properties":{"a":true}}`,
		"ap_bool":        `{"type":"object","properties":{},"additionalProperties":false}`,
		"ap_schema":      `{"type":"object","properties":{},"additionalProperties":{"type":"string"}}`,
		"empty_desc":     `{"type":"object","properties":{"a":{"type":"string","description":""}}}`,
		"keywords":       `{"type":"object","properties":{"a":{"type":"string"}},"format":"table","minItems":1}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			schema := mustSchema(t, raw)
			original := paramTool("t", schema)
			entry, err := freezeForRegistration(original)
			if err != nil {
				t.Fatalf("freeze: %v", err)
			}
			// Deep copy must be byte-identical through the public codec...
			fresh, err := json.Marshal(deepCopyToolInfo(entry.info).Parameters)
			if err != nil {
				t.Fatalf("marshal copy: %v", err)
			}
			direct, err := json.Marshal(entry.info.Parameters)
			if err != nil {
				t.Fatalf("marshal original: %v", err)
			}
			if !bytesEqual(fresh, direct) {
				t.Errorf("deep copy changed marshal bytes:\n copy:   %s\n direct: %s", fresh, direct)
			}
			// ...and equal the cached entry bytes.
			if !bytesEqual(fresh, entry.paramsJSON) {
				t.Errorf("cached paramsJSON diverges:\n cached: %s\n fresh:  %s", entry.paramsJSON, fresh)
			}
		})
	}
}

func TestRegistryConcurrentStressCache(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newMock("seed")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				name := fmt.Sprintf("w%d_%d", id, i)
				switch i % 3 {
				case 0:
					_ = r.Register(paramTool(name, NewParameterSchema()))
				case 1:
					_ = r.Replace(paramTool(name, NewParameterSchema()))
				case 2:
					_ = r.Apply([]string{name}, []Tool{paramTool(name, NewParameterSchema())})
				}
			}
		}(w)
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				infos := r.List()
				for i := 1; i < len(infos); i++ {
					if infos[i-1].Name >= infos[i].Name {
						t.Errorf("List not sorted: %q >= %q", infos[i-1].Name, infos[i].Name)
						return
					}
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	infos := r.List()
	for i := 1; i < len(infos); i++ {
		if infos[i-1].Name >= infos[i].Name {
			t.Fatalf("final List unsorted: %q >= %q", infos[i-1].Name, infos[i].Name)
		}
	}
}
