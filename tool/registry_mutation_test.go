package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

type countingTool struct {
	info   ToolInfo
	marker string
}

func (m *countingTool) Info() ToolInfo { return m.info }
func (m *countingTool) Execute(_ context.Context, _ json.RawMessage) (*ToolResult, error) {
	return NewTextResult(m.marker), nil
}

func newCountingTool(name, marker string) *countingTool {
	return &countingTool{
		info: ToolInfo{
			Name:        name,
			Description: name + " tool",
			Parameters:  NewParameterSchema(),
		},
		marker: marker,
	}
}

func badSchemaTool(name string) Tool {
	schema := NewParameterSchema()
	schema.Required = []string{"missing_param"}
	return &mockTool{info: ToolInfo{Name: name, Parameters: schema}}
}

func listNames(r *Registry) []string {
	list := r.List()
	names := make([]string, 0, len(list))
	for _, info := range list {
		names = append(names, info.Name)
	}
	return names
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestRegistryUnregister(t *testing.T) {
	tests := []struct {
		name    string
		steps   func(r *Registry) []bool
		want    []bool
		wantEnd []string
	}{
		{
			name: "removes registered tool once",
			steps: func(r *Registry) []bool {
				first := r.Unregister("gone")
				second := r.Unregister("gone")
				return []bool{first, second}
			},
			want:    []bool{true, false},
			wantEnd: []string{"kept"},
		},
		{
			name: "missing name returns false and changes nothing",
			steps: func(r *Registry) []bool {
				return []bool{r.Unregister("never_registered")}
			},
			want:    []bool{false},
			wantEnd: []string{"gone", "kept"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(newMock("gone")); err != nil {
				t.Fatal(err)
			}
			if err := r.Register(newMock("kept")); err != nil {
				t.Fatal(err)
			}
			got := tt.steps(r)
			if !equalBools(got, tt.want) {
				t.Fatalf("Unregister results = %v, want %v", got, tt.want)
			}
			if end := listNames(r); !equalStrings(end, tt.wantEnd) {
				t.Fatalf("registry after Unregister = %v, want %v", end, tt.wantEnd)
			}
		})
	}
}

func equalBools(got, want []bool) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestRegistryReplace(t *testing.T) {
	t.Run("missing name errors without upsert", func(t *testing.T) {
		r := NewRegistry()
		err := r.Replace(newMock("absent"))
		if err == nil {
			t.Fatal("Replace on absent name returned nil error")
		}
		if _, ok := r.Get("absent"); ok {
			t.Fatal("Replace on absent name inserted a tool")
		}
	})

	t.Run("swap is visible via List and Get", func(t *testing.T) {
		r := NewRegistry()
		if err := r.Register(newCountingTool("swap", "old")); err != nil {
			t.Fatal(err)
		}
		replacement := newCountingTool("swap", "new")
		if err := r.Replace(replacement); err != nil {
			t.Fatalf("Replace: %v", err)
		}
		got, ok := r.Get("swap")
		if !ok {
			t.Fatal("Get after Replace returned ok=false")
		}
		if got != Tool(replacement) {
			t.Fatal("Get after Replace returned the old implementation")
		}
		result, err := got.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil || result.Content != "new" {
			t.Fatalf("Execute after Replace = %q, %v; want %q", result.Content, err, "new")
		}
	})

	t.Run("rejects invalid tool without change", func(t *testing.T) {
		r := NewRegistry()
		original := newCountingTool("swap", "old")
		if err := r.Register(original); err != nil {
			t.Fatal(err)
		}
		if err := r.Replace(badSchemaTool("swap")); err == nil {
			t.Fatal("Replace with invalid schema returned nil error")
		}
		got, _ := r.Get("swap")
		if got != Tool(original) {
			t.Fatal("failed Replace left a different tool registered")
		}
	})

	t.Run("rejects nil and empty name", func(t *testing.T) {
		r := NewRegistry()
		if err := r.Register(newMock("swap")); err != nil {
			t.Fatal(err)
		}
		if err := r.Replace(nil); err == nil {
			t.Fatal("Replace(nil) returned nil error")
		}
		if err := r.Replace(newMock("")); err == nil {
			t.Fatal("Replace with empty name returned nil error")
		}
	})
}

func TestRegistryApply(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*Registry) error
		removed []string
		added   []Tool
		wantErr bool
		wantEnd []string
	}{
		{
			name:    "unknown removed names are silently ignored",
			setup:   func(r *Registry) error { return r.Register(newMock("kept")) },
			removed: []string{"ghost", "phantom"},
			added:   []Tool{newMock("fresh")},
			wantEnd: []string{"fresh", "kept"},
		},
		{
			name:    "removed name is reusable by added",
			setup:   func(r *Registry) error { return r.Register(newCountingTool("swap", "old")) },
			removed: []string{"swap"},
			added:   []Tool{newCountingTool("swap", "new"), newMock("extra")},
			wantEnd: []string{"extra", "swap"},
		},
		{
			name:    "duplicate vs surviving existing name is rejected",
			setup:   func(r *Registry) error { return r.Register(newMock("survivor")) },
			removed: []string{"other"},
			added:   []Tool{newMock("survivor")},
			wantErr: true,
			wantEnd: []string{"survivor"},
		},
		{
			name:    "duplicate within added is rejected",
			setup:   func(r *Registry) error { return nil },
			added:   []Tool{newMock("dup"), newMock("dup")},
			wantErr: true,
			wantEnd: []string{},
		},
		{
			name:    "one invalid schema rejects the whole batch",
			setup:   func(r *Registry) error { return r.Register(newMock("kept")) },
			removed: []string{"kept"},
			added:   []Tool{newMock("good"), badSchemaTool("bad")},
			wantErr: true,
			wantEnd: []string{"kept"},
		},
		{
			name:    "failed batch leaves removals rolled back",
			setup:   func(r *Registry) error { return r.Register(newMock("kept")) },
			removed: []string{"kept"},
			added:   []Tool{badSchemaTool("bad")},
			wantErr: true,
			wantEnd: []string{"kept"},
		},
		{
			name:    "removal and addition land in one step",
			setup:   func(r *Registry) error { return r.Register(newMock("a")) },
			removed: []string{"a"},
			added:   []Tool{newMock("b"), newMock("c")},
			wantEnd: []string{"b", "c"},
		},
		{
			name:    "nil tool in added is rejected",
			setup:   func(r *Registry) error { return nil },
			added:   []Tool{nil},
			wantErr: true,
			wantEnd: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			if err := tt.setup(r); err != nil {
				t.Fatal(err)
			}
			err := r.Apply(tt.removed, tt.added)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Apply error = %v, wantErr %v", err, tt.wantErr)
			}
			if end := listNames(r); !equalStrings(end, tt.wantEnd) {
				t.Fatalf("registry after Apply = %v, want %v", end, tt.wantEnd)
			}
		})
	}
}

func TestRegistryApplyReplacedToolIsNewImplementation(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newCountingTool("swap", "old")); err != nil {
		t.Fatal(err)
	}
	if err := r.Apply([]string{"swap"}, []Tool{newCountingTool("swap", "new")}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, ok := r.Get("swap")
	if !ok {
		t.Fatal("Get after Apply returned ok=false")
	}
	result, err := got.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || result.Content != "new" {
		t.Fatalf("Execute after Apply = %q, %v; want %q", result.Content, err, "new")
	}
}

func TestRegistryListReturnsCopy(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newMock("a")); err != nil {
		t.Fatal(err)
	}
	list := r.List()
	list[0].Name = "mutated"
	if names := listNames(r); !equalStrings(names, []string{"a"}) {
		t.Fatalf("mutating List result changed registry to %v", names)
	}
}

func TestRegistryConcurrentMutations(t *testing.T) {
	r := NewRegistry()
	for i := range 40 {
		if err := r.Register(newMock(fmt.Sprintf("seed_%02d", i))); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(4)
		go func(i int) {
			defer wg.Done()
			_ = r.Register(newMock(fmt.Sprintf("reg_%02d", i)))
		}(i)
		go func(i int) {
			defer wg.Done()
			r.Unregister(fmt.Sprintf("seed_%02d", i))
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = r.Apply([]string{fmt.Sprintf("seed_%02d", i+10)}, []Tool{newCountingTool(fmt.Sprintf("app_%02d", i), "x")})
		}(i)
		go func() {
			defer wg.Done()
			_ = r.List()
		}()
	}
	wg.Wait()

	// Concurrent mutations have no guaranteed order (P2-4); assert only internal consistency.
	seen := make(map[string]struct{})
	for _, info := range r.List() {
		if _, dup := seen[info.Name]; dup {
			t.Fatalf("duplicate name %q in List", info.Name)
		}
		seen[info.Name] = struct{}{}
		if _, ok := r.Get(info.Name); !ok {
			t.Fatalf("List reported %q but Get missed it", info.Name)
		}
	}
}
