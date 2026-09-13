package tool

import (
	"fmt"
	"sort"
	"sync"
)

// Registry holds a collection of named tools and provides lookup by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool is nil")
	}

	info := t.Info()
	if info.Name == "" {
		return fmt.Errorf("tool has empty name")
	}

	if err := validateToolInfo(info); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[info.Name]; exists {
		return fmt.Errorf("tool %q already registered", info.Name)
	}
	r.tools[info.Name] = t
	return nil
}

func validateToolInfo(info ToolInfo) error {
	if err := validateRegistrySchema(info.Parameters); err != nil {
		return fmt.Errorf("tool %q: %w", info.Name, err)
	}
	return nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) List() []ToolInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]ToolInfo, 0, len(r.tools))
	for _, t := range r.tools {
		result = append(result, t.Info())
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}
