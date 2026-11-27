package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	goja "github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// gojaPinnedVersion is the exact pseudo-version this satellite is built and
// reviewed against. Upgrades require the full regression set:
// source-map disabling, the single-owner delivery loop, cancellation and
// late-result behavior, projection budgeting and the dependency closure.
const gojaPinnedVersion = "v0.0.0-20261110204830-f493a2b600d6"

// defaultMaxCallStackSize is the explicit default for the goja VM.
const defaultMaxCallStackSize = 1024

// GojaExecutor is the default Executor backend: one fresh goja.Runtime per
// run, single-owner goroutine, no host APIs beyond the tools object, the
// output helper and a strings-only console.
type GojaExecutor struct {
	stackSize int
}

// GojaOption configures the goja backend.
type GojaOption func(*GojaExecutor)

// WithMaxCallStackSize overrides the VM call-stack depth bound
// (default 1024). It bounds recursion depth; it is not a step or heap meter.
func WithMaxCallStackSize(n int) GojaOption {
	return func(g *GojaExecutor) {
		if n > 0 {
			g.stackSize = n
		}
	}
}

// NewGojaExecutor builds the default backend.
func NewGojaExecutor(opts ...GojaOption) *GojaExecutor {
	g := &GojaExecutor{stackSize: defaultMaxCallStackSize}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	return g
}

func (g *GojaExecutor) Profile() ExecutorProfile {
	return ExecutorProfile{
		Backend:          "goja",
		BackendVersion:   gojaPinnedVersion,
		Language:         "JavaScript (ECMAScript subset; no Node APIs)",
		EntrySyntax:      "async function body; top-level await and return",
		Loops:            true,
		Functions:        true,
		AsyncCalls:       true,
		ErrorRecovery:    "try/catch",
		GuestStepLimit:   false,
		GuestMemoryBytes: false,
		GuestForceStop:   false,
	}
}

const gojaInstructions = `Business tools live on the frozen "tools" object: await tools["exact-registered-name"]({objectArgs}). Each call resolves to {value, content, data?, images?, status?}: data mirrors the tool's structured result when present, otherwise value is the content text. Recoverable failures reject with an object carrying {kind, message, result?} — catch them to branch. Call output(value, imageRef...) to attach explicitly selected images to the return; a plain return sends value only (strings become text, other JSON-compatible values are JSON-encoded; undefined, functions and cyclic objects fail the result). console.log accepts strings only. There are no file, network or timer APIs.`

func (g *GojaExecutor) RenderAPI(tools []ToolDescriptor) (APIDocument, error) {
	docs, err := renderAPIDocument(tools)
	if err != nil {
		return APIDocument{}, err
	}
	docs.Instructions = gojaInstructions
	if len(docs.Notes) > 0 {
		docs.Instructions += "\nDeclaration notes: " + strings.Join(docs.Notes, "; ") + "."
	}
	return docs, nil
}

// The output marker is a host-created Symbol (see installOutput): ToValue
// deep-converts Go structs in this goja build, so a struct pointer cannot
// serve as an opaque runtime marker.

// gojaInterrupt is the sentinel value carried by Runtime.Interrupt.
type gojaInterrupt struct{}

// resolverEntry holds the owner-side resolving functions of one admitted
// call. Entries live only on the VM owner goroutine.
type resolverEntry struct {
	resolve func(interface{}) error
	reject  func(interface{}) error
}

type gojaVM struct {
	rt         *goja.Runtime
	req        RunRequest
	thenFn     goja.Callable
	parseFn    goja.Callable
	parseThis  goja.Value
	outputMark *goja.Symbol
	// Output artifacts snapshot their validated image tokens here at
	// output() time; later guest mutation cannot change the projection.
	// Owner-goroutine only.
	outputTokens map[string][]ImageRef
	outputSeq    int

	resolvers map[uint64]resolverEntry

	settled         bool
	settledRejected bool
	settledValue    goja.Value
	aborted         bool
}

// Run is the VM-owner entry: it creates the runtime, disables
// source maps on every parse entry, wires the tools namespace, runs the
// wrapped async body, installs the host continuation through the original
// Promise.prototype.then, and pumps reserved deliveries into their one
// resolver entry each. It returns only once the interpreter has stopped and
// can no longer submit calls or process callbacks. Any panic escaping the
// script value walk (null.ToObject, throwing getters, host conversion) is
// recovered here into a soft failure — it must never kill the process.
func (g *GojaExecutor) Run(ctx context.Context, req RunRequest) (outcome RunOutcome, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = RunOutcome{Failure: &Failure{
				Kind:    FailureScript,
				Message: truncateMessage(fmt.Sprint(recovered)),
			}}
			err = nil
		}
	}()
	return g.run(ctx, req)
}

func (g *GojaExecutor) run(_ context.Context, req RunRequest) (RunOutcome, error) {
	if len(req.Source) > req.Limits.SourceBytes {
		return RunOutcome{Failure: &Failure{Kind: FailureScript, Message: fmt.Sprintf("source is %d bytes, over the %d-byte limit", len(req.Source), req.Limits.SourceBytes)}}, nil
	}
	rt := goja.New()
	rt.SetMaxCallStackSize(g.stackSize)
	// Every parse entry (RunString/RunScript/eval/function constructors)
	// inherits these options; source-map resolution reads files and is
	// disabled before any code runs.
	rt.SetParserOptions(parser.WithDisableSourceMaps)
	// Stop hook before any code: a non-blocking interrupt request; the stop
	// path already closed admission and the delivery gate.
	req.Control.RegisterInterrupt(func() { rt.Interrupt(&gojaInterrupt{}) })

	vm := &gojaVM{rt: rt, req: req, resolvers: make(map[uint64]resolverEntry), outputTokens: make(map[string][]ImageRef)}
	global := rt.GlobalObject()

	if !vm.captureOriginals(global) {
		return RunOutcome{Failure: &Failure{Kind: FailureInternal, Message: "runtime lacks Promise/JSON builtins"}}, nil
	}
	vm.hardenPromise(global)
	vm.installConsole(global)
	vm.installOutput(global)
	vm.installTools(global)

	if len(req.Tools) == 0 {
		// still register an (empty) tools object so scripts fail loudly on
		// access rather than on a missing global
	}

	wrapped := "(async () => {\n" + req.Source + "\n})()"
	promiseVal, runErr := rt.RunScript("codemode-script", wrapped)
	if runErr != nil {
		var interrupted *goja.InterruptedError
		if errors.As(runErr, &interrupted) {
			return RunOutcome{}, nil
		}
		return RunOutcome{Failure: &Failure{Kind: FailureScript, Message: truncateMessage(runErr.Error())}}, nil
	}

	// Host continuation through the original then: the script can rewrite
	// Promise.prototype or the instance, but the settled notification and
	// ScriptSettled always go through what we captured before any code ran.
	fulfill := func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) > 0 {
			vm.settle(false, call.Arguments[0])
		}
		return goja.Undefined()
	}
	rejectFn := func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) > 0 {
			vm.settle(true, call.Arguments[0])
		}
		return goja.Undefined()
	}
	if _, err := vm.thenFn(promiseVal, rt.ToValue(fulfill), rt.ToValue(rejectFn)); err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			return RunOutcome{}, nil
		}
		return RunOutcome{Failure: &Failure{Kind: FailureScript, Message: truncateMessage(err.Error())}}, nil
	}

	// A script that completed or failed synchronously has already settled
	// its promise; the continuation reaction only runs on the next top-level
	// entry. Drain once before blocking on deliveries (the completion
	// criterion is the continuation, not the queue).
	vm.drainJobs()

	for !vm.settled && !vm.aborted {
		reply, ok := req.Host.Reserve()
		if !ok {
			break
		}
		vm.deliver(reply)
	}
	return vm.buildOutcome(), nil
}

func (vm *gojaVM) captureOriginals(global *goja.Object) bool {
	promise := global.Get("Promise")
	if promise == nil || goja.IsUndefined(promise) {
		return false
	}
	thenV := promise.ToObject(vm.rt).Get("prototype").ToObject(vm.rt).Get("then")
	thenFn, ok := goja.AssertFunction(thenV)
	if !ok {
		return false
	}
	jsonV := global.Get("JSON")
	if jsonV == nil || goja.IsUndefined(jsonV) {
		return false
	}
	parseV := jsonV.ToObject(vm.rt).Get("parse")
	parseFn, ok := goja.AssertFunction(parseV)
	if !ok {
		return false
	}
	vm.thenFn = thenFn
	vm.parseFn = parseFn
	vm.parseThis = jsonV.ToObject(vm.rt)
	return true
}

// hardenPromise freezes the Promise[Symbol.species] accessor before any
// guest code runs: then() reads the species to build its derived promise, so
// a guest rewrite could otherwise intercept or break the host continuation
// registration even though the original then was captured.
func (vm *gojaVM) hardenPromise(global *goja.Object) {
	rt := vm.rt
	promiseObj := global.Get("Promise").ToObject(rt)
	// Re-write the current accessor/value with default attributes: species is
	// initially writable+configurable, and SetSymbol stores a plain value
	// property with both flags off. The guest can no longer rewrite species,
	// which then() reads to build its derived promise. The
	// iterator symbol gets the same treatment.
	species := promiseObj.GetSymbol(goja.SymSpecies)
	_ = promiseObj.SetSymbol(goja.SymSpecies, species)
	iterator := promiseObj.GetSymbol(goja.SymIterator)
	_ = promiseObj.SetSymbol(goja.SymIterator, iterator)
}

// installConsole wires console.log/info/warn/error/debug: strings only,
// runes streamed into the StdoutBytes budget via Control.PrintString.
func (vm *gojaVM) installConsole(global *goja.Object) {
	rt := vm.rt
	logNative := func(call goja.FunctionCall) goja.Value {
		for _, arg := range call.Arguments {
			if !goja.IsString(arg) {
				panic(rt.NewTypeError("console accepts strings only"))
			}
			writeConsoleString(vm.req.Control, arg)
		}
		vm.req.Control.PrintString("\n")
		return goja.Undefined()
	}
	consoleObj := rt.NewObject()
	for _, name := range []string{"log", "info", "warn", "error", "debug"} {
		consoleObj.Set(name, logNative)
	}
	global.Set("console", consoleObj)
}

// writeConsoleString streams one guest string into the stdout budget via
// the goja RuneReader, so a large guest string never materializes as a
// single host allocation after the budget is already gone.
func writeConsoleString(control Control, arg goja.Value) {
	if sv, ok := arg.(goja.String); ok {
		reader := sv.Reader()
		chunk := make([]rune, 0, 512)
		flush := func() {
			if len(chunk) > 0 {
				control.PrintString(string(chunk))
				chunk = chunk[:0]
			}
		}
		for {
			r, _, err := reader.ReadRune()
			if err != nil {
				break
			}
			chunk = append(chunk, r)
			if len(chunk) == cap(chunk) {
				flush()
			}
		}
		flush()
		return
	}
	control.PrintString(arg.String())
}

// installOutput wires the output(value, imageRef...) helper. The returned
// object carries a host-created Symbol property no plain business object
// can have by accident, so same-named value/images fields on ordinary
// objects are never misdetected. The validated image tokens are
// snapshotted into a Go-side table immediately: later guest mutation of the
// returned object cannot change what gets projected.
func (vm *gojaVM) installOutput(global *goja.Object) {
	rt := vm.rt
	symbolCtor := global.Get("Symbol")
	symbolFn, ok := goja.AssertFunction(symbolCtor)
	if !ok {
		return
	}
	symV, err := symbolFn(symbolCtor, rt.ToValue("codemode.output"))
	if err != nil {
		return
	}
	sym, ok := symV.(*goja.Symbol)
	if !ok {
		return
	}
	vm.outputMark = sym
	outputNative := func(call goja.FunctionCall) goja.Value {
		obj := rt.NewObject()
		_ = obj.SetSymbol(vm.outputMark, true)
		obj.Set("value", call.Argument(0))
		var refs []ImageRef
		for _, arg := range call.Arguments[min(1, len(call.Arguments)):] {
			if goja.IsUndefined(arg) || goja.IsNull(arg) {
				continue
			}
			refObj := arg.ToObject(rt)
			refV := refObj.Get("ref")
			if refV == nil || !goja.IsString(refV) {
				panic(rt.NewTypeError("output images must be {ref: token} objects"))
			}
			refs = append(refs, ImageRef{Token: truncateMessage(refV.String())})
		}
		id := fmt.Sprintf("out-%d", vm.outputSeq)
		vm.outputSeq++
		vm.outputTokens[id] = refs
		obj.Set("output_id", id)
		return obj
	}
	global.Set("output", outputNative)
}

// installTools builds the no-prototype frozen tools namespace: every name is
// the exact registered name, JSON-string escaped in declarations; global
// Promise/JSON/console are never shadowed.
func (vm *gojaVM) installTools(global *goja.Object) {
	rt := vm.rt
	objCtor := global.Get("Object").ToObject(rt)
	createFn, _ := goja.AssertFunction(objCtor.Get("create"))
	freezeFn, _ := goja.AssertFunction(objCtor.Get("freeze"))

	toolsVal, _ := createFn(objCtor, goja.Null())
	toolsObj := toolsVal.ToObject(rt)

	for _, desc := range vm.req.Tools {
		name := desc.Name
		bindNative := func(call goja.FunctionCall) goja.Value {
			return vm.submitBinding(name, call)
		}
		toolsObj.Set(name, rt.ToValue(bindNative))
	}
	_, _ = freezeFn(objCtor, toolsObj)
	global.Set("tools", toolsObj)
}

// submitBinding validates the single object argument, bounded-encodes it and
// hands the call to Host.Submit. It never runs Get/Info/approval/Execute and
// returns immediately with a Promise.
func (vm *gojaVM) submitBinding(name string, call goja.FunctionCall) goja.Value {
	rt := vm.rt
	argsVal := call.Argument(0)
	if goja.IsUndefined(argsVal) || goja.IsNull(argsVal) {
		return vm.rejectedPromise(&Failure{Kind: FailureArgsInvalid, Message: fmt.Sprintf("tool %q expects one non-null JSON object argument", name)})
	}
	for _, extra := range call.Arguments[1:] {
		if !goja.IsUndefined(extra) {
			return vm.rejectedPromise(&Failure{Kind: FailureArgsInvalid, Message: fmt.Sprintf("tool %q expects exactly one argument", name)})
		}
	}
	// Bounded incremental encoding: the args are walked once inside the
	// ArgsBytes budget — no full Export/Marshal before the check.
	// A goja exception from a throwing getter is recovered here so the call
	// rejects instead of throwing into the script.
	if et := argsVal.ExportType(); et == nil || et.Kind() != reflect.Map {
		return vm.rejectedPromise(&Failure{Kind: FailureArgsInvalid, Message: fmt.Sprintf("tool %q expects one JSON object argument", name)})
	}
	raw, encErr, over := func() (raw json.RawMessage, encErr *Failure, over bool) {
		defer func() {
			if recover() != nil {
				encErr = &Failure{Kind: FailureArgsInvalid, Message: "arguments could not be encoded"}
			}
		}()
		enc := newBoundedEncoder(rt, vm.req.Limits.ProjectionDepth, vm.req.Limits.ProjectionElements, vm.req.Limits.ArgsBytes)
		r, ferr := enc.encode(argsVal)
		return r, ferr, enc.over
	}()
	if encErr != nil {
		return vm.rejectedPromise(&Failure{Kind: FailureArgsInvalid, Message: truncateMessage(encErr.Message)})
	}
	if over || len(raw) == 0 || raw[0] != '{' {
		return vm.rejectedPromise(&Failure{Kind: FailureArgsInvalid, Message: fmt.Sprintf("tool %q arguments are not one JSON object within %d bytes", name, vm.req.Limits.ArgsBytes)})
	}
	callID, failure := vm.req.Host.Submit(name, raw)
	if failure != nil {
		return vm.rejectedPromise(failure)
	}
	promise, resolve, reject := rt.NewPromise()
	vm.resolvers[callID] = resolverEntry{resolve: resolve, reject: reject}
	return rt.ToValue(promise)
}

func (vm *gojaVM) rejectedPromise(f *Failure) goja.Value {
	promise, _, reject := vm.rt.NewPromise()
	if err := reject(vm.failureValue(f)); err != nil {
		vm.abortOrScriptFail(err)
	}
	return vm.rt.ToValue(promise)
}

// abortOrScriptFail routes a delivery error: only uncatchable
// interrupts abort the run; anything else settles as a rejected script
// failure so the outer result carries the real cause instead of an empty
// outcome.
func (vm *gojaVM) abortOrScriptFail(err error) {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		vm.markAborted(err)
		return
	}
	if !vm.settled {
		vm.settled = true
		vm.settledRejected = true
		vm.settledValue = vm.rt.NewTypeError(truncateMessage(err.Error()))
	}
}

// deliver feeds one reserved completion into its one resolver entry, then
// re-enters the runtime so the pending promise jobs run to quiescence
// (leave() drains the job queue on every top-level return). Resolver calls
// and job execution happen outside every bridge lock.
// FinishDelivery runs even if reply construction or the resolver entry
// panics — the reservation is settled exactly once, on every path.
func (vm *gojaVM) deliver(reply CallReply) {
	entry, ok := vm.resolvers[reply.CallID]
	if !ok {
		vm.req.Host.FinishDelivery(reply.CallID)
		return
	}
	delete(vm.resolvers, reply.CallID)

	func() {
		defer vm.req.Host.FinishDelivery(reply.CallID)
		defer func() {
			if recover() != nil {
				vm.markAborted(errors.New("delivery entry panicked"))
			}
		}()
		var err error
		switch {
		case reply.Failure != nil:
			err = entry.reject(vm.failureValue(reply.Failure))
		default:
			value := vm.parseJSON(reply.Value)
			if value == nil {
				err = entry.reject(vm.failureValue(&Failure{Kind: FailureBridgeInvalid, Message: "completion payload was not valid JSON"}))
			} else {
				err = entry.resolve(value)
			}
		}
		if err != nil {
			vm.abortOrScriptFail(err)
		}
	}()
	if vm.aborted {
		return
	}
	vm.drainJobs()
}

// drainJobs re-enters the runtime with a no-op script; Runtime.leave() then
// runs every currently-runnable promise job, including ones spawned by
// those jobs. The VM owner goroutine is the only caller.
func (vm *gojaVM) drainJobs() {
	_, err := vm.rt.RunString("0")
	if err == nil {
		return
	}
	vm.abortOrScriptFail(err)
}

func (vm *gojaVM) markAborted(err error) {
	vm.aborted = true
	_ = err
}

func (vm *gojaVM) settle(rejected bool, v goja.Value) {
	if vm.settled {
		return
	}
	vm.settled = true
	vm.settledRejected = rejected
	vm.settledValue = v
	vm.req.Control.ScriptSettled()
}

func (vm *gojaVM) parseJSON(raw json.RawMessage) (result goja.Value) {
	defer func() {
		if recover() != nil {
			result = nil
		}
	}()
	value, err := vm.parseFn(vm.parseThis, vm.rt.ToValue(string(raw)))
	if err != nil {
		return nil
	}
	return value
}

// failureValue builds the script-facing rejection object {kind, message,
// result?}; result carries the bounded ToolValue of recoverable
// tool failures.
func (vm *gojaVM) failureValue(f *Failure) goja.Value {
	rt := vm.rt
	obj := rt.NewObject()
	obj.Set("kind", string(f.Kind))
	obj.Set("message", f.Message)
	if len(f.Value) > 0 {
		if parsed := vm.parseJSON(f.Value); parsed != nil {
			obj.Set("result", parsed)
		}
	}
	return obj
}

// buildOutcome folds the settled state into the executor-level outcome:
// script rejections and incompatible terminal values become
// failures; everything else is bounded-encoded within OutputBytes.
func (vm *gojaVM) buildOutcome() RunOutcome {
	if vm.aborted {
		return RunOutcome{}
	}
	if !vm.settled {
		return RunOutcome{}
	}
	if vm.settledRejected {
		return RunOutcome{Failure: &Failure{Kind: FailureScript, Message: truncateMessage(vm.valueMessage(vm.settledValue))}}
	}
	value := vm.settledValue
	var images []ImageRef
	// Non-JSON-compatible primitives are rejected before any ToObject walk:
	// null has no object form and Symbol/BigInt cannot enter JSON.
	// goja's Symbol.String() returns just the description, so the type is
	// checked via ExportType.
	if goja.IsNull(value) {
		return RunOutcome{Value: json.RawMessage("null")}
	}
	// A bare return (undefined) is rejected before any ToObject walk so it
	// reports a clean result_encoding failure instead of a converted panic.
	if goja.IsUndefined(value) {
		return RunOutcome{Failure: &Failure{Kind: FailureEncoding, Message: "script returned undefined; return a JSON-compatible value"}}
	}
	// A goja Symbol is the one value whose ExportType is string while the
	// value itself is not a string primitive; it cannot enter JSON.
	if et := value.ExportType(); et != nil && et.Kind() == reflect.String && !goja.IsString(value) {
		return RunOutcome{Failure: &Failure{Kind: FailureEncoding, Message: "script returned a Symbol"}}
	}
	if et := value.ExportType(); et != nil && (strings.Contains(et.String(), "bigInt") || strings.Contains(et.String(), "big.Int")) {
		return RunOutcome{Failure: &Failure{Kind: FailureEncoding, Message: "script returned a BigInt"}}
	}
	if vm.outputMark != nil {
		obj := value.ToObject(vm.rt)
		mark := obj.GetSymbol(vm.outputMark)
		matched := mark != nil && !goja.IsUndefined(mark) && mark.ToBoolean()
		if matched {
			value = obj.Get("value")
			// Tokens come from the Go-side snapshot taken at output() time —
			// the id is read from the object, the list cannot be rewritten by
			// the guest afterwards.
			if idV := obj.Get("output_id"); idV != nil && goja.IsString(idV) {
				images = vm.outputTokens[idV.String()]
			}
		}
	}
	budget := vm.req.Limits.OutputBytes - outputMarkerReserve
	enc := newBoundedEncoder(vm.rt, vm.req.Limits.ProjectionDepth, vm.req.Limits.ProjectionElements, budget)
	raw, encErr := enc.encode(value)
	if encErr != nil {
		return RunOutcome{Failure: encErr}
	}
	if enc.over {
		return RunOutcome{ValueOmitted: true, Images: images}
	}
	return RunOutcome{Value: raw, Images: images}
}

func (vm *gojaVM) valueMessage(v goja.Value) string {
	if v == nil {
		return ""
	}
	return truncateMessage(v.String())
}

func truncateMessage(s string) string {
	const cap = 512
	if len(s) <= cap {
		return s
	}
	return cutAtRuneBoundary(s, cap)
}

// boundedEncoder walks a goja.Value incrementally into a fixed-capacity
// JSON writer: no full Stringify-then-truncate, no full Export of the whole
// tree. Property access may run script getters; that stays on the
// owner goroutine inside the execution deadline.
type boundedEncoder struct {
	rt          *goja.Runtime
	buf         []byte
	budget      int
	over        bool
	depth       int
	maxDepth    int
	elements    int
	maxElements int
	seen        map[*goja.Object]bool
}

func newBoundedEncoder(rt *goja.Runtime, maxDepth, maxElements, budget int) *boundedEncoder {
	return &boundedEncoder{
		rt: rt, budget: budget,
		maxDepth: maxDepth, maxElements: maxElements,
		seen: make(map[*goja.Object]bool),
	}
}

func (enc *boundedEncoder) write(s string) bool {
	if enc.over || len(enc.buf)+len(s) > enc.budget {
		enc.over = true
		return false
	}
	enc.buf = append(enc.buf, s...)
	return true
}

// encode returns the encoded bytes; enc.over reports budget exhaustion
// (ValueOmitted) and a non-nil failure reports an incompatible value
// (result_encoding).
func (enc *boundedEncoder) encode(v goja.Value) (json.RawMessage, *Failure) {
	if err := enc.value(v, 0); err != nil {
		return nil, err
	}
	if enc.over {
		return nil, nil
	}
	return json.RawMessage(enc.buf), nil
}

func (enc *boundedEncoder) value(v goja.Value, depth int) *Failure {
	if enc.over {
		return nil
	}
	if v == nil || goja.IsUndefined(v) {
		return &Failure{Kind: FailureEncoding, Message: "value contains undefined"}
	}
	switch {
	case goja.IsString(v):
		enc.writeStringValue(v)
		return nil
	case goja.IsNumber(v):
		if !finiteJSONNumber(v) {
			return &Failure{Kind: FailureEncoding, Message: "value contains a non-finite number"}
		}
		enc.write(v.String())
		return nil
	case goja.IsNull(v):
		enc.write("null")
		return nil
	}
	// Boolean by type, not truthiness: objects are truthy but must encode
	// as composites.
	if et := v.ExportType(); et != nil && et.Kind() == reflect.Bool {
		if v.ToBoolean() {
			enc.write("true")
		} else {
			enc.write("false")
		}
		return nil
	}
	// Symbols are the one non-string value whose ExportType is string;
	// BigInt is detected via its Go type. Neither can enter JSON.
	if et := v.ExportType(); et != nil && et.Kind() == reflect.String && !goja.IsString(v) {
		return &Failure{Kind: FailureEncoding, Message: "value contains a Symbol"}
	}
	if et := v.ExportType(); et != nil && (strings.Contains(et.String(), "bigInt") || strings.Contains(et.String(), "big.Int")) {
		return &Failure{Kind: FailureEncoding, Message: "value contains a BigInt"}
	}
	if _, isCallable := goja.AssertFunction(v); isCallable {
		return &Failure{Kind: FailureEncoding, Message: "value contains a function"}
	}
	return enc.composite(v, depth)
}

func (enc *boundedEncoder) composite(v goja.Value, depth int) *Failure {
	if depth >= enc.maxDepth {
		return &Failure{Kind: FailureEncoding, Message: fmt.Sprintf("value nesting exceeds %d levels", enc.maxDepth)}
	}
	obj := v.ToObject(enc.rt)
	identity := obj
	if enc.seen[identity] {
		return &Failure{Kind: FailureEncoding, Message: "value contains a reference cycle"}
	}
	enc.seen[identity] = true
	defer delete(enc.seen, identity)

	isArray := v.ExportType() != nil && v.ExportType().Kind() == reflect.Slice
	if isArray {
		// Arrays iterate by index up to length: holes become null and extra
		// non-index properties are not silently reshaped into elements.
		enc.write("[")
		length := arrayLength(obj)
		for i := range length {
			enc.elements++
			if enc.elements > enc.maxElements {
				return &Failure{Kind: FailureEncoding, Message: fmt.Sprintf("value exceeds %d elements", enc.maxElements)}
			}
			if enc.over {
				return nil
			}
			if i > 0 {
				enc.write(",")
			}
			item := obj.Get(strconv.Itoa(i))
			if item == nil || goja.IsUndefined(item) {
				enc.write("null")
				continue
			}
			if err := enc.value(item, depth+1); err != nil {
				return err
			}
		}
		if enc.over {
			return nil
		}
		enc.write("]")
		return nil
	}
	enc.write("{")
	first := true
	var compErr *Failure
	if !enc.forEachOwnKey(obj, func(key string) bool {
		if compErr != nil {
			return false
		}
		enc.elements++
		if enc.elements > enc.maxElements {
			compErr = &Failure{Kind: FailureEncoding, Message: fmt.Sprintf("value exceeds %d elements", enc.maxElements)}
			return false
		}
		if enc.over {
			return false
		}
		if !first {
			enc.write(",")
		}
		first = false
		enc.writeStringJSON(key)
		enc.write(":")
		if err := enc.value(obj.Get(key), depth+1); err != nil {
			compErr = err
			return false
		}
		return !enc.over
	}) {
		// Enumeration failed mid-walk (throwing getter): the partial buffer
		// is discarded and the run settles as an encoding failure.
		return &Failure{Kind: FailureEncoding, Message: "value could not be enumerated"}
	}
	if compErr != nil {
		return compErr
	}
	if enc.over {
		return nil
	}
	enc.write("}")
	return nil
}

// forEachOwnKey streams the object's own enumerable keys through visit
// without materializing the full key list: a wide guest object cannot force
// a host-sized slice before the budget can stop the walk. visit returning
// false stops iteration early.
func (enc *boundedEncoder) forEachOwnKey(obj *goja.Object, visit func(key string) bool) bool {
	rt := enc.rt
	ownFn, _ := goja.AssertFunction(rt.GlobalObject().Get("Object").ToObject(rt).Get("prototype").ToObject(rt).Get("hasOwnProperty"))
	iterVal, err := rt.RunString(`(function (target, visit) { for (var key in target) { if (visit(key) === false) return; } })`)
	if err != nil {
		for _, key := range obj.Keys() {
			if !visit(key) {
				return true
			}
		}
		return true
	}
	iterFn, ok := goja.AssertFunction(iterVal)
	if !ok {
		for _, key := range obj.Keys() {
			if !visit(key) {
				return true
			}
		}
		return true
	}
	visitValue := rt.ToValue(func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return goja.Undefined()
		}
		key := call.Arguments[0].String()
		if ownFn != nil {
			owned, oerr := ownFn(obj, rt.ToValue(key))
			if oerr != nil || !owned.ToBoolean() {
				return goja.Undefined()
			}
		}
		if visit(key) {
			return goja.Undefined()
		}
		return rt.ToValue(false)
	})
	_, iterErr := iterFn(goja.Undefined(), obj, visitValue)
	return iterErr == nil
}

// writeStringValue streams the string through goja's RuneReader instead of
// materializing the whole Go string first: a guest-built large UTF-16
// string never lands as one host allocation.
func (enc *boundedEncoder) writeStringValue(v goja.Value) {
	if enc.over || !enc.write(`"`) {
		return
	}
	sv, ok := v.(goja.String)
	if !ok {
		enc.writeStringJSON(v.String())
		return
	}
	reader := sv.Reader()
	for {
		if enc.over {
			return
		}
		r, _, err := reader.ReadRune()
		if err != nil {
			break
		}
		enc.writeJSONRune(r)
	}
	enc.write(`"`)
}

// writeStringJSON escapes an already-materialized Go string rune by rune
// inside the remaining budget.
func (enc *boundedEncoder) writeStringJSON(s string) {
	if enc.over || !enc.write(`"`) {
		return
	}
	for _, r := range s {
		if enc.over {
			return
		}
		enc.writeJSONRune(r)
	}
	enc.write(`"`)
}

func (enc *boundedEncoder) writeJSONRune(r rune) {
	if enc.over {
		return
	}
	switch r {
	case '"':
		enc.write(`\"`)
	case '\\':
		enc.write(`\\`)
	case '\n':
		enc.write(`\n`)
	case '\r':
		enc.write(`\r`)
	case '\t':
		enc.write(`\t`)
	default:
		if r < 0x20 {
			enc.write(fmt.Sprintf(`\u%04x`, r))
		} else {
			enc.write(string(r))
		}
	}
}

// arrayLength reads the array length without exporting the whole array.
func arrayLength(obj *goja.Object) int {
	lenV := obj.Get("length")
	if lenV == nil || !goja.IsNumber(lenV) {
		return 0
	}
	switch n := lenV.Export().(type) {
	case int:
		return clampIndex(n)
	case int64:
		return clampIndex(int(n))
	case float64:
		return clampIndex(int(n))
	}
	return 0
}

func clampIndex(n int) int {
	if n < 0 {
		return 0
	}
	if n > 1<<30 {
		return 1 << 30
	}
	return n
}

func formatJSONNumber(s string) string {
	return s
}

// finiteJSONNumber reports whether the number renders as a finite JSON
// number; goja exports integral values as int/int64 and others as float64.
func finiteJSONNumber(v goja.Value) bool {
	switch n := v.Export().(type) {
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		return !math.IsNaN(float64(n)) && !math.IsInf(float64(n), 0)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	}
	return false
}
