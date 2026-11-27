package codemode

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// maxRefDepth bounds local $ref resolution: deeper ref chains or ref
// cycles degrade that subtree to any with a diagnostic.
const maxRefDepth = 20

// maxTotalRenders bounds the total node renderings per tool: an acyclic but
// dense $ref graph could otherwise expand exponentially.
const maxTotalRenders = 4000

// declarationRenderer renders one tool's input schema (the canonical marshaled
// tool.ParameterSchema JSON) into a TypeScript-ish declaration used inside the
// run_code Description. It documents the API for the model; it is not a
// validator and never claims TS-level runtime guarantees. Every degraded or
// unrepresentable construct yields a diagnostic carrying the tool name and
// JSON Pointer; the original schema bytes stay untouched.
type declarationRenderer struct {
	tool        string
	root        any
	diags       []string
	notes       []string
	refs        []string // active local-ref stack for cycle detection
	depth       int
	rendered    int
	budgetFired bool
}

// schemaError is a hard generation failure: the tool's schema root provably
// disallows a JSON object, so it cannot be exposed as a single-object
// argument. Connect/Refresh fails and names the tool.
type schemaError struct{ msg string }

func (e *schemaError) Error() string { return e.msg }

// renderToolDeclaration renders tools["name"](args: T): Promise<ToolValue>.
func renderToolDeclaration(desc ToolDescriptor) (string, []string, []string, error) {
	if len(desc.InputSchema) == 0 {
		r := &declarationRenderer{tool: desc.Name}
		r.note("schema absent; accepting any object", "#")
		return fmt.Sprintf("tools[%s](args: any): Promise<ToolValue>", quoteJSName(desc.Name)),
			r.notes, r.diags, nil
	}
	var root any
	dec := json.NewDecoder(strings.NewReader(string(desc.InputSchema)))
	dec.UseNumber() // preserve big JSON numbers exactly
	if err := dec.Decode(&root); err != nil {
		return "", nil, nil, &schemaError{msg: fmt.Sprintf(
			"tool %q: decode marshaled parameter schema: %v", desc.Name, err)}
	}
	r := &declarationRenderer{tool: desc.Name, root: root}
	if err := r.checkObjectRoot("#", root, 0); err != nil {
		return "", nil, nil, err
	}
	t := r.render("#", root)
	return fmt.Sprintf("tools[%s](args: %s): Promise<ToolValue>", quoteJSName(desc.Name), t),
		r.notes, r.diags, nil
}

// renderAPIDocument renders declarations for every descriptor in input
// order, one hard failure naming the offending tool if any root excludes
// objects. Per-tool degradation notes are collected so the model Description
// can carry them next to the declarations.
func renderAPIDocument(tools []ToolDescriptor) (APIDocument, error) {
	var b strings.Builder
	var diags, notes []string
	for _, desc := range tools {
		decl, ns, ds, err := renderToolDeclaration(desc)
		if err != nil {
			return APIDocument{}, err
		}
		diags = append(diags, ds...)
		notes = append(notes, ns...)
		b.WriteString(decl)
		b.WriteString(";\n")
	}
	return APIDocument{Declarations: b.String(), Diagnostics: diags, Notes: notes}, nil
}

// checkObjectRoot hard-fails when the root provably cannot accept a JSON
// object: a primitive/array type without an object branch, or an
// enum/const without object values. Unknown shapes pass and render any.
func (r *declarationRenderer) checkObjectRoot(ptr string, node any, depth int) error {
	if depth > maxRefDepth {
		return nil
	}
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if ref, ok := m["$ref"].(string); ok && len(ref) > 0 && ref[0] == '#' {
		target, err := resolveLocalRef(r.root, ref)
		if err != nil {
			return nil // resolution failure degrades to any; not a provable exclusion
		}
		return r.checkObjectRoot(ptr, target, depth+1)
	}
	types, hasTypes := schemaTypes(m)
	if hasTypes {
		for _, t := range types {
			if t == "object" {
				return nil
			}
		}
		// JSON Schema conjunctive semantics: a sibling type applies on top of
		// any composition keywords, so a type that excludes objects excludes
		// them through anyOf/oneOf/allOf as well.
		return &schemaError{msg: fmt.Sprintf(
			"tool %q: schema root at %s declares type(s) %v which exclude JSON objects; such tools cannot be exposed as single-object code calls (filter them via WithToolFilter)",
			r.tool, ptr, types)}
	}
	if vals, ok := m["enum"].([]any); ok {
		for _, v := range vals {
			if _, isObj := v.(map[string]any); isObj {
				return nil
			}
		}
		return &schemaError{msg: fmt.Sprintf(
			"tool %q: schema root at %s is an enum without object values", r.tool, ptr)}
	}
	if _, ok := m["const"]; ok {
		if _, isObj := m["const"].(map[string]any); isObj {
			return nil
		}
		return &schemaError{msg: fmt.Sprintf(
			"tool %q: schema root at %s is a const that is not an object", r.tool, ptr)}
	}
	return nil
}

// render maps one schema node to a TypeScript-ish type string.
func (r *declarationRenderer) render(ptr string, node any) string {
	r.depth++
	r.rendered++
	defer func() { r.depth-- }()
	if r.rendered > maxTotalRenders {
		if !r.budgetFired {
			r.budgetFired = true
			r.diag(ptr, "schema expansion exceeds %d nodes; remaining branches render any", maxTotalRenders)
		}
		return "any"
	}
	if r.depth > maxRefDepth {
		r.diag(ptr, "schema nesting exceeds %d levels; rendering any", maxRefDepth)
		return "any"
	}
	switch v := node.(type) {
	case bool:
		if v {
			r.note("boolean schema true (anything allowed)", ptr)
			return "any"
		}
		r.note("boolean schema false (no value allowed)", ptr)
		return "never"
	case nil:
		r.diag(ptr, "null schema node; rendering any")
		return "any"
	case map[string]any:
		return r.renderMap(ptr, v)
	case []any:
		r.diag(ptr, "schema array node has no single meaning; rendering any")
		return "any"
	default:
		r.diag(ptr, "unexpected schema node %v; rendering any", node)
		return "any"
	}
}

func (r *declarationRenderer) renderMap(ptr string, m map[string]any) string {
	r.noteValidationKeywords(ptr, m)
	if ref, ok := m["$ref"].(string); ok && len(ref) > 0 {
		// $ref siblings apply alongside the target (2020-12); the declaration
		// renders only the target, so referring-node siblings are diagnosed
		// rather than silently dropped.
		r.noteCompositionSiblings(ptr, "$ref", m)
		return r.renderRef(ptr, ref)
	}
	if _, ok := m["anyOf"]; ok {
		r.noteCompositionSiblings(ptr, "anyOf", m)
		return r.renderUnion(ptr, m["anyOf"], "anyOf")
	}
	if _, ok := m["oneOf"]; ok {
		r.noteCompositionSiblings(ptr, "oneOf", m)
		u := r.renderUnion(ptr, m["oneOf"], "oneOf")
		r.diag(ptr, "oneOf exclusivity is not enforced by declarations")
		return u
	}
	if _, ok := m["allOf"]; ok {
		r.noteCompositionSiblings(ptr, "allOf", m)
		return r.renderIntersection(ptr, m["allOf"])
	}
	if c, ok := m["const"]; ok {
		return r.literal(ptr, c)
	}
	if vals, ok := m["enum"].([]any); ok && len(vals) > 0 {
		parts := make([]string, 0, len(vals))
		for _, v := range vals {
			parts = append(parts, r.literal(ptr, v))
		}
		return joinDistinct(parts)
	}
	types, hasTypes := schemaTypes(m)
	if hasTypes {
		parts := make([]string, 0, len(types))
		for _, t := range types {
			parts = append(parts, r.primitive(ptr, t, m))
		}
		s := strings.Join(parts, " | ")
		if nullable(m) && !containsType(types, "null") {
			s += " | null"
		}
		return s
	}
	if props, ok := m["properties"].(map[string]any); ok {
		return r.renderObject(ptr, m, props)
	}
	if items, ok := m["items"]; ok {
		return r.renderArrayItems(ptr, items)
	}
	r.diag(ptr, "schema vocabulary not recognized; rendering any")
	return "any"
}

// renderArrayItems groups union item types before appending []: without
// parens, string | null[] parses as string | (null[]) and changes meaning.
func (r *declarationRenderer) renderArrayItems(ptr string, items any) string {
	t := r.render(ptr+"/items", items)
	if strings.Contains(t, " ") {
		t = "(" + t + ")"
	}
	return t + "[]"
}

// noteCompositionSiblings: sibling constraints beside a composition keyword
// (2020-12 semantics) are not applied by the declaration render; say so
// instead of implying they were folded in.
func (r *declarationRenderer) noteCompositionSiblings(ptr string, keyword string, m map[string]any) {
	present := make([]string, 0, 3)
	for _, key := range []string{"type", "properties", "required", "items", "enum", "const"} {
		if _, ok := m[key]; ok {
			present = append(present, key)
		}
	}
	if len(present) > 0 {
		r.diag(ptr, "%s composes with sibling keywords [%s]; the declaration renders only the composition", keyword, strings.Join(present, ","))
	}
}

func (r *declarationRenderer) primitive(ptr, t string, m map[string]any) string {
	switch t {
	case "string":
		return "string"
	case "number":
		return "number"
	case "integer":
		r.note("integer constraints are not enforced in declarations", ptr)
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "object":
		props, _ := m["properties"].(map[string]any)
		return r.renderObject(ptr, m, props)
	case "array":
		if items, ok := m["items"]; ok {
			return r.renderArrayItems(ptr, items)
		}
		return "any[]"
	default:
		r.diag(ptr, "unknown type %q; rendering any", t)
		return "any"
	}
}

func (r *declarationRenderer) renderObject(ptr string, m map[string]any, props map[string]any) string {
	required := map[string]bool{}
	if req, ok := m["required"].([]any); ok {
		for _, k := range req {
			if s, ok := k.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	// required keys without a properties entry are real required members of
	// unknown type: render them instead of silently omitting.
	for name := range required {
		if _, ok := props[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("{ ")
	for _, name := range names {
		child, declared := props[name]
		if !declared {
			b.WriteString(quoteJSName(name))
			b.WriteString(": unknown; ")
			continue
		}
		childType := r.render(ptr+"/properties/"+jsonPointerEscape(name), child)
		b.WriteString(quoteJSName(name))
		if !required[name] {
			b.WriteString("?")
		}
		b.WriteString(": ")
		b.WriteString(childType)
		b.WriteString("; ")
	}
	switch ap := m["additionalProperties"].(type) {
	case bool:
		if !ap {
			r.note("additional keys are rejected by the tool's own validation, not by declarations", ptr)
		}
	case map[string]any:
		b.WriteString("[k: string]: ")
		b.WriteString(r.render(ptr+"/additionalProperties", ap))
		b.WriteString("; ")
		// A TS index signature also constrains named properties, while JSON
		// Schema additionalProperties does not; say so instead of implying
		// full fidelity.
		r.diag(ptr, "index signature does not exempt named properties from the constraint in TypeScript")
	}
	b.WriteString("}")
	return b.String()
}

func (r *declarationRenderer) renderUnion(ptr string, list any, keyword string) string {
	items, ok := list.([]any)
	if !ok || len(items) == 0 {
		r.diag(ptr, "empty composition; rendering any")
		return "any"
	}
	parts := make([]string, 0, len(items))
	for i, item := range items {
		parts = append(parts, r.render(fmt.Sprintf("%s/%s/%d", ptr, keyword, i), item))
	}
	return "(" + strings.Join(parts, " | ") + ")"
}

func (r *declarationRenderer) renderIntersection(ptr string, list any) string {
	items, ok := list.([]any)
	if !ok || len(items) == 0 {
		r.diag(ptr, "empty composition; rendering any")
		return "any"
	}
	parts := make([]string, 0, len(items))
	for i, item := range items {
		part := r.render(fmt.Sprintf("%s/allOf/%d", ptr, i), item)
		// A multi-alternative part must be grouped or the & precedence
		// silently changes meaning.
		if strings.Contains(part, " | ") {
			part = "(" + part + ")"
		}
		parts = append(parts, part)
	}
	return "(" + strings.Join(parts, " & ") + ")"
}

// validationKeywords are JSON Schema validation vocabularies the TS-ish
// declaration cannot express; their presence yields a diagnostic so the
// model and the host know the constraint is documentation only.
var validationKeywords = map[string]bool{
	"pattern": true, "format": true, "minimum": true, "maximum": true,
	"exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"minLength": true, "maxLength": true, "minItems": true, "maxItems": true,
	"uniqueItems": true, "minProperties": true, "maxProperties": true,
	"minContains": true, "maxContains": true, "contains": true,
	"prefixItems": true, "propertyNames": true, "contentEncoding": true,
	"contentMediaType": true, "if": true, "then": true, "else": true,
	"dependencies": true, "unevaluatedItems": true, "unevaluatedProperties": true,
}

func (r *declarationRenderer) noteValidationKeywords(ptr string, m map[string]any) {
	present := make([]string, 0, 4)
	for kw := range validationKeywords {
		if _, ok := m[kw]; ok {
			present = append(present, kw)
		}
	}
	if len(present) > 0 {
		sort.Strings(present)
		r.diag(ptr, "validation keywords [%s] are not enforced by declarations", strings.Join(present, ","))
	}
}

func (r *declarationRenderer) renderRef(ptr, ref string) string {
	if len(ref) == 0 || ref[0] != '#' {
		r.diag(ptr, "external $ref %q is never fetched (no I/O); rendering any and keeping the original schema", ref)
		return "any"
	}
	for _, active := range r.refs {
		if active == ref {
			r.diag(ptr, "local $ref cycle at %s; rendering any", ref)
			return "any"
		}
	}
	target, err := resolveLocalRef(r.root, ref)
	if err != nil {
		r.diag(ptr, "local $ref %q cannot be resolved (%v); rendering any", ref, err)
		return "any"
	}
	r.refs = append(r.refs, ref)
	defer func() { r.refs = r.refs[:len(r.refs)-1] }()
	return r.render(ptr, target)
}

func (r *declarationRenderer) literal(ptr string, v any) string {
	switch c := v.(type) {
	case string:
		b, _ := json.Marshal(c)
		return string(b)
	case json.Number:
		return c.String() // UseNumber decoding keeps big integers exact
	case float64:
		return formatNumber(c)
	case bool:
		if c {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	case []any:
		parts := make([]string, 0, len(c))
		for _, item := range c {
			parts = append(parts, r.literal(ptr, item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		// Object literals cannot be faithfully expressed inside enum/const
		// unions; degrade with a diagnostic instead of silently.
		r.diag(ptr, "object literal in enum/const is not representable; rendering any")
		return "any"
	default:
		r.diag(ptr, "unrepresentable literal; rendering any")
		return "any"
	}
}

func (r *declarationRenderer) diag(ptr, format string, args ...any) {
	r.diags = append(r.diags, fmt.Sprintf("[tool %s] %s: %s", r.tool, ptr, fmt.Sprintf(format, args...)))
}

func (r *declarationRenderer) note(text, _ string) {
	key := "[" + r.tool + "] " + text
	r.notes = append(r.notes, key)
}

// resolveLocalRef resolves one in-document JSON Pointer ref ("#..." form).
// URI fragment percent-escapes are decoded per segment before lookup, and
// the ~0/~1 pointer escapes are unescaped after that (RFC 6901).
func resolveLocalRef(root any, ref string) (any, error) {
	if ref == "#" {
		return root, nil
	}
	if len(ref) < 2 || ref[0] != '#' || ref[1] != '/' {
		return nil, fmt.Errorf("not an in-document pointer")
	}
	cur := root
	for _, seg := range strings.Split(ref[2:], "/") {
		if decoded, derr := url.PathUnescape(seg); derr == nil {
			seg = decoded
		}
		seg = jsonPointerUnescape(seg)
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, fmt.Errorf("pointer segment %q not found", seg)
			}
			cur = next
		case []any:
			idx, aerr := strconv.Atoi(seg)
			if aerr != nil || fmt.Sprint(idx) != seg || idx < 0 || idx >= len(node) {
				// Exact match required: a prefix like "1x" must not resolve.
				return nil, fmt.Errorf("pointer index %q invalid or out of range", seg)
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("pointer descends into scalar")
		}
	}
	return cur, nil
}

func jsonPointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func jsonPointerUnescape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

func quoteJSName(name string) string {
	b, _ := json.Marshal(name)
	return string(b)
}

func formatNumber(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

func schemaTypes(m map[string]any) ([]string, bool) {
	switch t := m["type"].(type) {
	case string:
		return []string{t}, true
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}

func nullable(m map[string]any) bool {
	b, ok := m["nullable"].(bool)
	return ok && b
}

func containsType(types []string, t string) bool {
	for _, item := range types {
		if item == t {
			return true
		}
	}
	return false
}

func joinDistinct(parts []string) string {
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return strings.Join(out, " | ")
}
