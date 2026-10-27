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
	info, err := validateForRegistration(t)
	if err != nil {
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

// validateForRegistration runs the shared Register-time checks outside the
// registry lock: user-supplied Info() is never called while holding it.
func validateForRegistration(t Tool) (ToolInfo, error) {
	if t == nil {
		return ToolInfo{}, fmt.Errorf("tool is nil")
	}
	info := t.Info()
	if info.Name == "" {
		return ToolInfo{}, fmt.Errorf("tool has empty name")
	}
	if err := validateToolInfo(info); err != nil {
		return ToolInfo{}, err
	}
	return info, nil
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

// Unregister removes the tool registered under name. It is idempotent and
// reports whether a removal actually happened. It never waits for in-flight
// Execute calls and never rewrites history: callers resolve removed names
// at their next Get or List.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[name]; !exists {
		return false
	}
	delete(r.tools, name)
	return true
}

// Replace swaps the implementation registered under t's name. Validation is
// identical to Register. A name that is not currently registered is an error
// (no upsert); callers wanting upsert semantics compose Replace-or-Register
// themselves. The swap happens inside one lock acquisition, so concurrent
// observers never see the name missing.
func (r *Registry) Replace(t Tool) error {
	info, err := validateForRegistration(t)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[info.Name]; !exists {
		return fmt.Errorf("tool %q not registered", info.Name)
	}
	r.tools[info.Name] = t
	return nil
}

// Apply atomically removes and adds a batch of tools. Every added tool is
// validated first — same schema checks as Register, no duplicates within
// added, and no duplicates against existing names that this call does not
// remove. Any failure leaves the registry at zero change. Names in removed
// that are not registered are silently ignored: the call targets an end
// state, not an exact delta. The whole batch lands inside one lock
// acquisition.
func (r *Registry) Apply(removed []string, added []Tool) error {
	type entry struct {
		info ToolInfo
		t    Tool
	}
	entries := make([]entry, 0, len(added))
	addedNames := make(map[string]struct{}, len(added))
	for _, t := range added {
		info, err := validateForRegistration(t)
		if err != nil {
			return err
		}
		if _, dup := addedNames[info.Name]; dup {
			return fmt.Errorf("tool %q duplicated in added batch", info.Name)
		}
		addedNames[info.Name] = struct{}{}
		entries = append(entries, entry{info: info, t: t})
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	removing := make(map[string]struct{}, len(removed))
	for _, name := range removed {
		removing[name] = struct{}{}
	}
	for name := range addedNames {
		if _, drop := removing[name]; drop {
			continue
		}
		if _, exists := r.tools[name]; exists {
			return fmt.Errorf("tool %q already registered", name)
		}
	}
	for _, name := range removed {
		delete(r.tools, name)
	}
	for _, e := range entries {
		r.tools[e.info.Name] = e.t
	}
	return nil
}
