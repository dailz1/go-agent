package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// mockTool is a minimal Tool implementation for testing.
type mockTool struct {
	info ToolInfo
}

func (m *mockTool) Info() ToolInfo { return m.info }
func (m *mockTool) Execute(_ context.Context, _ json.RawMessage) (*ToolResult, error) {
	return NewTextResult("mock"), nil
}

func newMock(name string) *mockTool {
	return &mockTool{
		info: ToolInfo{
			Name:        name,
			Description: name + " tool",
			Parameters:  NewParameterSchema(),
		},
	}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	m := newMock("my_tool")
	if err := r.Register(m); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	got, ok := r.Get("my_tool")
	if !ok {
		t.Fatal("Get returned ok=false, want true")
	}
	if got != m {
		t.Error("Get returned different tool instance")
	}
}

func TestRegistry_RegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newMock("dup"))

	err := r.Register(newMock("dup"))
	if err == nil {
		t.Fatal("expected error for duplicate registration, got nil")
	}
}

func TestRegistry_RegisterEmptyName(t *testing.T) {
	r := NewRegistry()
	err := r.Register(newMock(""))
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
}

func TestRegistry_GetNotFound(t *testing.T) {
	r := NewRegistry()
	got, ok := r.Get("no_such_tool")
	if ok {
		t.Error("Get returned ok=true for missing tool, want false")
	}
	if got != nil {
		t.Error("Get returned non-nil tool for missing name, want nil")
	}
}

func TestRegistry_List_SortedOrder(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newMock("z_tool"))
	_ = r.Register(newMock("a_tool"))
	_ = r.Register(newMock("m_tool"))

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("List returned %d items, want 3", len(list))
	}

	wantOrder := []string{"a_tool", "m_tool", "z_tool"}
	for i, want := range wantOrder {
		if list[i].Name != want {
			t.Errorf("List[%d].Name = %q, want %q", i, list[i].Name, want)
		}
	}
}

func TestRegistry_List_Empty(t *testing.T) {
	r := NewRegistry()
	list := r.List()

	if list == nil {
		t.Fatal("List returned nil, want non-nil slice")
	}
	if len(list) != 0 {
		t.Errorf("List returned %d items, want 0", len(list))
	}
}

func TestRegistry_MustRegister(t *testing.T) {
	// Normal registration should not panic.
	r := NewRegistry()
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Errorf("unexpected panic: %v", rec)
			}
		}()
		r.MustRegister(newMock("ok_tool"))
	}()

	// Verify it was registered.
	if _, ok := r.Get("ok_tool"); !ok {
		t.Error("ok_tool not found after MustRegister")
	}

	// Duplicate should panic.
	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Error("expected panic on duplicate MustRegister, got none")
			}
		}()
		r.MustRegister(newMock("ok_tool"))
	}()
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	const n = 100
	r := NewRegistry()
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("tool_%03d", i)
			if err := r.Register(newMock(name)); err != nil {
				t.Errorf("Register(%q) returned error: %v", name, err)
			}
		}(i)
	}
	wg.Wait()

	list := r.List()
	if len(list) != n {
		t.Fatalf("List returned %d items, want %d", len(list), n)
	}

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool_%03d", i)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := r.Get(name)
			if !ok {
				t.Fatalf("Get(%q) returned ok=false", name)
			}
			if got.Info().Name != name {
				t.Errorf("Get(%q).Name = %q, want %q", name, got.Info().Name, name)
			}
		})
	}
}

func TestRegistry_ConcurrentReadWrite(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < 50; i++ {
		r.MustRegister(newMock(fmt.Sprintf("preload_%03d", i)))
	}

	const writers = 20
	const readers = 30
	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("writer_%03d", i)
			_ = r.Register(newMock(name))
		}(i)
	}

	for i := 0; i < readers; i++ {
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("preload_%03d", i%50)
			_, _ = r.Get(name)
			_ = r.List()
		}(i)
	}

	wg.Wait()

	total := 50 + writers
	list := r.List()
	if len(list) != total {
		t.Errorf("List returned %d items, want %d", len(list), total)
	}
}

func TestRegistry_RegisterNilTool(t *testing.T) {
	r := NewRegistry()

	err := r.Register(nil)
	if err == nil {
		t.Fatal("expected error when registering nil tool, got nil")
	}
}

func TestRegistry_MultipleRegistrations_LargeCount(t *testing.T) {
	const n = 1000
	r := NewRegistry()

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool_%04d", i)
		if err := r.Register(newMock(name)); err != nil {
			t.Fatalf("Register(%q) returned error: %v", name, err)
		}
	}

	list := r.List()
	if len(list) != n {
		t.Fatalf("List returned %d items, want %d", len(list), n)
	}

	for i := 1; i < len(list); i++ {
		if list[i].Name < list[i-1].Name {
			t.Errorf("List not sorted: %q > %q at index %d", list[i-1].Name, list[i].Name, i)
		}
	}
}

func TestRegistry_Get_Concurrent(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(newMock("shared_tool"))

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			got, ok := r.Get("shared_tool")
			if !ok {
				t.Error("Get returned ok=false for shared_tool")
			}
			if got == nil {
				t.Error("Get returned nil tool")
			}
		}()
	}
	wg.Wait()
}

func TestRegister_RequiredNotInProperties(t *testing.T) {
	r := NewRegistry()
	schema := NewParameterSchema()
	schema.Required = []string{"missing_param"}

	err := r.Register(&mockTool{
		info: ToolInfo{Name: "bad_tool", Parameters: schema},
	})
	if err == nil {
		t.Fatal("expected error when required param not in properties, got nil")
	}
	if !strings.Contains(err.Error(), "required parameter") {
		t.Errorf("error = %q, want substring %q", err.Error(), "required parameter")
	}
}

func TestRegister_PropertyEmptyType(t *testing.T) {
	r := NewRegistry()
	schema := NewParameterSchema()
	schema.Properties["expr"] = Property{Description: "an expression"}

	err := r.Register(&mockTool{
		info: ToolInfo{Name: "bad_tool", Parameters: schema},
	})
	if err == nil {
		t.Fatal("expected error when property has empty type, got nil")
	}
	if !strings.Contains(err.Error(), "empty type") {
		t.Errorf("error = %q, want substring %q", err.Error(), "empty type")
	}
}
