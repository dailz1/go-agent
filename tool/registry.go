package tool

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// registryEntry is the immutable per-tool record. All three fields are
// written once during the out-of-lock freeze pipeline and never mutated
// afterwards (invariant I4).
type registryEntry struct {
	tool       Tool     // live execution handle; Get returns it unchanged
	info       ToolInfo // frozen canonical: deep copy taken at registration
	paramsJSON []byte   // json.Marshal of the frozen Parameters
}

// Registry holds a collection of named tools and provides lookup by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]registryEntry
	// snapshot is the aggregate layer: tools in ascending name order.
	// nil means dirty; a non-nil empty slice is a clean empty registry
	// (invariant I10). Built by whole-array replacement, never in-place
	// edits (invariant I5).
	snapshot []ToolInfo
}

// snapshotRebuilds counts snapshot rebuilds; test seam, package-level like
// the other seams in this repo. Atomic: rebuilds of distinct registries run
// concurrently under -race.
var snapshotRebuilds atomic.Int64

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]registryEntry),
	}
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

// freezeForRegistration runs the full out-of-lock pipeline: validate (the
// only user Info() call in the codebase), deep-copy freeze, serialize. It
// must be called before any lock acquisition; none of the three steps
// depends on registry state.
func freezeForRegistration(t Tool) (registryEntry, error) {
	info, err := validateForRegistration(t)
	if err != nil {
		return registryEntry{}, err
	}
	frozen := deepCopyToolInfo(info)
	params, err := json.Marshal(frozen.Parameters)
	if err != nil {
		return registryEntry{}, fmt.Errorf("tool %q: serialize parameters: %w", frozen.Name, err)
	}
	return registryEntry{tool: t, info: frozen, paramsJSON: params}, nil
}

func (r *Registry) Register(t Tool) error {
	entry, err := freezeForRegistration(t)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[entry.info.Name]; exists {
		return fmt.Errorf("tool %q already registered", entry.info.Name)
	}
	r.tools[entry.info.Name] = entry
	r.snapshot = nil
	return nil
}

// Get returns the live execution handle registered under name. The cache
// does not apply: execution needs the Tool itself, not its description.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.tools[name]
	return entry.tool, ok
}

// List returns the frozen ToolInfo of every registered tool in ascending
// name order. It never calls user code: the infos were frozen at
// registration time and each call receives a deep copy, so mutating the
// result cannot affect the registry or other List callers. An empty
// registry yields an empty non-nil slice.
func (r *Registry) List() []ToolInfo {
	r.mu.RLock()
	snap := r.snapshot
	r.mu.RUnlock()

	if snap == nil {
		r.mu.Lock()
		if r.snapshot == nil {
			r.rebuildSnapshotLocked()
		}
		snap = r.snapshot
		r.mu.Unlock()
	}

	out := make([]ToolInfo, len(snap))
	for i := range snap {
		out[i] = deepCopyToolInfo(snap[i])
	}
	return out
}

// rebuildSnapshotLocked collects the current entries into the aggregate
// snapshot. Sort and assembly only — no codec, no user code. Callers must
// hold the write lock.
func (r *Registry) rebuildSnapshotLocked() {
	snapshotRebuilds.Add(1)
	snap := make([]ToolInfo, 0, len(r.tools))
	for _, entry := range r.tools {
		snap = append(snap, entry.info)
	}
	sort.Slice(snap, func(i, j int) bool { return snap[i].Name < snap[j].Name })
	r.snapshot = snap
}

func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Unregister removes the tool registered under name. It is idempotent and
// reports whether a removal actually happened. It never waits for in-flight
// Execute calls and never rewrites history: callers resolve removed names
// at their next Get or List. The snapshot is dropped only when the removal
// actually happened, symmetric with the zero-change failure paths.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[name]; !exists {
		return false
	}
	delete(r.tools, name)
	r.snapshot = nil
	return true
}

// Replace swaps the implementation registered under t's name. Validation is
// identical to Register. A name that is not currently registered is an error
// (no upsert); callers wanting upsert semantics compose Replace-or-Register
// themselves. The swap happens inside one lock acquisition, so concurrent
// observers never see the name missing. The old entry is replaced by a
// brand-new one; entries are never edited in place.
func (r *Registry) Replace(t Tool) error {
	entry, err := freezeForRegistration(t)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[entry.info.Name]; !exists {
		return fmt.Errorf("tool %q not registered", entry.info.Name)
	}
	r.tools[entry.info.Name] = entry
	r.snapshot = nil
	return nil
}

// Apply atomically removes and adds a batch of tools. Every added tool is
// validated first — same schema checks as Register, no duplicates within
// added, and no duplicates against existing names that this call does not
// remove. Any failure leaves the registry at zero change. Names in removed
// that are not registered are silently ignored: the call targets an end
// state, not an exact delta. The whole batch lands inside one lock
// acquisition and drops the snapshot exactly once.
func (r *Registry) Apply(removed []string, added []Tool) error {
	entries := make([]registryEntry, 0, len(added))
	addedNames := make(map[string]struct{}, len(added))
	for _, t := range added {
		entry, err := freezeForRegistration(t)
		if err != nil {
			return err
		}
		if _, dup := addedNames[entry.info.Name]; dup {
			return fmt.Errorf("tool %q duplicated in added batch", entry.info.Name)
		}
		addedNames[entry.info.Name] = struct{}{}
		entries = append(entries, entry)
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
		r.tools[e.info.Name] = e
	}
	r.snapshot = nil
	return nil
}

// deepCopyToolInfo returns a fully isolated copy of info: nested maps,
// slices (including byte-slice backing arrays), and pointers never alias the
// input. Pure function — no user code, no mutation of the input (invariant
// I7); unexported presence flags are carried over field by field.
func deepCopyToolInfo(info ToolInfo) ToolInfo {
	return ToolInfo{
		Name:             info.Name,
		Description:      info.Description,
		Parameters:       deepCopyParameterSchema(info.Parameters),
		RequiresApproval: info.RequiresApproval,
	}
}

func deepCopyParameterSchema(s ParameterSchema) ParameterSchema {
	out := s // scalars and unexported flags copy by value
	out.Properties = deepCopyPropertyMap(s.Properties)
	out.Required = cloneStrings(s.Required)
	out.AdditionalProperties = cloneBoolPtr(s.AdditionalProperties)
	out.AdditionalPropertiesSchema = deepCopyPropertyPtr(s.AdditionalPropertiesSchema)
	out.AnyOf = deepCopyPropertySlice(s.AnyOf)
	out.OneOf = deepCopyPropertySlice(s.OneOf)
	out.AllOf = deepCopyPropertySlice(s.AllOf)
	out.Keywords = deepCopyKeywords(s.Keywords)
	return out
}

func deepCopyProperty(p Property) Property {
	out := p
	out.Enum = cloneStrings(p.Enum)
	out.Properties = deepCopyPropertyMap(p.Properties)
	out.Required = cloneStrings(p.Required)
	out.Items = deepCopyPropertyPtr(p.Items)
	out.AdditionalProperties = cloneBoolPtr(p.AdditionalProperties)
	out.AdditionalPropertiesSchema = deepCopyPropertyPtr(p.AdditionalPropertiesSchema)
	out.AnyOf = deepCopyPropertySlice(p.AnyOf)
	out.OneOf = deepCopyPropertySlice(p.OneOf)
	out.AllOf = deepCopyPropertySlice(p.AllOf)
	out.Keywords = deepCopyKeywords(p.Keywords)
	out.BooleanSchema = cloneBoolPtr(p.BooleanSchema)
	return out
}

func deepCopyPropertyPtr(p *Property) *Property {
	if p == nil {
		return nil
	}
	copied := deepCopyProperty(*p)
	return &copied
}

func deepCopyPropertyMap(m map[string]Property) map[string]Property {
	if m == nil {
		return nil
	}
	out := make(map[string]Property, len(m))
	for name, p := range m {
		out[name] = deepCopyProperty(p)
	}
	return out
}

func deepCopyPropertySlice(s []*Property) []*Property {
	if s == nil {
		return nil
	}
	out := make([]*Property, len(s))
	for i, p := range s {
		out[i] = deepCopyPropertyPtr(p)
	}
	return out
}

func deepCopyKeywords(k []SchemaKeyword) []SchemaKeyword {
	if k == nil {
		return nil
	}
	out := make([]SchemaKeyword, len(k))
	for i, kw := range k {
		out[i] = SchemaKeyword{Name: kw.Name, Value: cloneBytes(kw.Value)}
	}
	return out
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func cloneBoolPtr(b *bool) *bool {
	if b == nil {
		return nil
	}
	out := *b
	return &out
}
