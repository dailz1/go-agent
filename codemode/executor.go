// Package codemode exposes a code-mode satellite: the model writes one
// script (the run_code tool) instead of calling business tools one by one.
// Scripts run in a replaceable in-process Executor (default: the pinned goja
// interpreter) and reach business tools only through a controlled Host
// bridge, so intermediate results stay inside the script and only the
// script's final value (plus explicitly selected images) enters model
// history.
//
// The package is a satellite: it never modifies the kernel closure
// (agent/llm/store/tool) and its only third-party dependency is goja. The
// design contract lives in docs/CODEMODE-DESIGN.md; section references in
// comments (D0, D12, 裁决 B, ...) point at that document.
package codemode

import (
	"context"
	"encoding/json"
	"time"
)

// FailureKind is a stable, machine-readable failure category. Kinds are part
// of the satellite contract: backends and hosts must not invent new kinds
// for conditions this list already names.
type FailureKind string

const (
	// FailureScript covers compile errors and uncaught script exceptions.
	// It is terminal for the run but reported as a soft ToolResult error.
	FailureScript FailureKind = "script"
	// FailureToolError marks a nested tool result with Status=error. It is
	// recoverable: the failure carries the bounded ToolValue so the script
	// can inspect structured data (D12).
	FailureToolError FailureKind = "tool_error"
	// FailureNotFound means the requested tool name is not in the
	// execution's allow-set.
	FailureNotFound FailureKind = "not_found"
	// FailureDenied means the nested call was rejected by approval (or no
	// approval callback is configured) — fail-closed, never implicit
	// approval (D10).
	FailureDenied FailureKind = "denied"
	// FailureCatalogStale means the live tool's marshaled parameter schema
	// differs from the generation's declaration fingerprint for the same
	// name (裁决 B). The call is not executed; the host should Refresh.
	FailureCatalogStale FailureKind = "catalog_stale"
	// FailureArgsInvalid means the call arguments were not a single
	// non-null JSON object or could not be encoded within ArgsBytes.
	FailureArgsInvalid FailureKind = "args_invalid"
	// FailureBridgeLimit means a single tool result or the cumulative
	// bridged-in payload exceeded HostResultBytes / HostDataBytes. Data is
	// never truncated into a pseudo-success JSON (D5).
	FailureBridgeLimit FailureKind = "bridge_result_limit"
	// FailureBridgeInvalid means the tool result carried a Data field that
	// is not valid JSON (D11.1).
	FailureBridgeInvalid FailureKind = "bridge_result_invalid"
	// FailureCallLimit is terminal: more than MaxCalls Submit attempts in
	// one execution (D6).
	FailureCallLimit FailureKind = "call_limit"
	// FailureUnawaited is terminal: the top-level script settled while
	// admitted calls were never awaited (D13 step 7).
	FailureUnawaited FailureKind = "unawaited_calls"
	// FailureTimeout is supervisor-selected: the WallClock deadline fired.
	// It is reported as a soft error with cleanup state, not a Go error.
	FailureTimeout FailureKind = "timeout"
	// FailureCancelled is supervisor-selected: the parent context was
	// cancelled.
	FailureCancelled FailureKind = "cancelled"
	// FailureEncoding means the script's terminal value is not
	// JSON-compatible (undefined, function, symbol, bigint, non-finite
	// number, cyclic object). The value is never silently coerced (D11.2).
	FailureEncoding FailureKind = "result_encoding"
	// FailureInternal covers executor or bridge infrastructure faults that
	// are not script-visible tool failures.
	FailureInternal FailureKind = "internal"
)

// Recoverable reports whether a script may catch the failure and continue.
// Terminal and supervisor-selected causes close admission and cannot be
// caught into a successful return (D12 error table).
func (k FailureKind) Recoverable() bool {
	switch k {
	case FailureToolError, FailureNotFound, FailureDenied, FailureCatalogStale,
		FailureArgsInvalid, FailureBridgeLimit, FailureBridgeInvalid:
		return true
	}
	return false
}

// Failure describes one failure delivered to a script or attached to a run
// outcome. Message is bounded human-readable text; it never embeds raw tool
// results (those travel in Value).
type Failure struct {
	Kind    FailureKind `json:"kind"`
	Message string      `json:"message"`
	// Value optionally carries the bounded ToolValue of a failed tool
	// result so recoverable failures keep their structured data. It is nil
	// for terminal and infrastructure failures.
	Value json.RawMessage `json:"value,omitempty"`
}

func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	return string(f.Kind) + ": " + f.Message
}

// Executor is the pluggable language backend behind run_code (裁决 A/D4).
// The satellite ships and maintains exactly one implementation (goja);
// alternative engines must honor the common Host/lifecycle contract and
// declare their true capabilities in Profile — the interface does not
// force loops, JS catch, or hard wall-clock guest termination on anyone.
//
// Run is the VM-owner entry: it may return only once the interpreter has
// stopped and can no longer submit calls or process callbacks (D4). It is
// invoked on a goroutine owned by the bridge, so the public Execute can
// respond under C's bounded wait even while Run is still draining.
type Executor interface {
	Profile() ExecutorProfile
	RenderAPI(tools []ToolDescriptor) (APIDocument, error)
	Run(ctx context.Context, req RunRequest) (RunOutcome, error)
}

// ToolDescriptor is one business tool as seen by an executor backend.
// InputSchema is the canonical marshaled Parameters schema (public List +
// marshal; D8) — never a private registry cache.
type ToolDescriptor struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// APIDocument is the backend-rendered API surface embedded into the outer
// run_code Description (within DescriptionBudget). Diagnostics list every
// degraded or unrepresentable schema construct so the host can audit what
// the model actually sees; Notes carry the short per-tool degradation lines
// that also accompany the declarations in the model Description (D8/F18).
type APIDocument struct {
	Instructions string
	Declarations string
	Diagnostics  []string
	Notes        []string
}

// ExecutorProfile declares what a backend really supports (裁决 A). goja's
// capabilities never become other backends' promises; expression-only
// engines are allowed as long as they honor the call-ordering, cancellation
// and explicit-side-effect contracts. Resource capability flags are
// declarations, not marketing: a Config limit on an undeclared domain makes
// Connect fail — it is never silently dropped (D5).
type ExecutorProfile struct {
	Backend        string
	BackendVersion string
	Language       string
	// EntrySyntax describes the script entry contract, e.g. "async function
	// body; top-level await and return".
	EntrySyntax   string
	Loops         bool
	Functions     bool
	AsyncCalls    bool
	ErrorRecovery string // e.g. "try/catch", "value-level"

	// Guest resource domains (D5): GuestStepLimit means the backend meters
	// its own abstract steps; GuestMemoryBytes means it enforces a guest
	// memory bound (state which domain in Backend docs — VM heap vs process
	// group; never host memory); GuestForceStop means the backend can stop
	// all guest execution, not only request it. goja declares all false.
	GuestStepLimit   bool
	GuestMemoryBytes bool
	GuestForceStop   bool
}

// Host is the script-facing tool bridge installed into one run. Implementors
// (the bridge in this package) guarantee:
//
//   - Submit never blocks on the actual tool; it validates and admits the
//     call or rejects it immediately with a *Failure. Terminal failures
//     (call limit, stop) close admission and are recorded by the
//     supervisor; they are never delivered as catchable replies.
//   - A completed call becomes a plain-data completion on the bridge side;
//     queueing it grants no delivery rights (D13 step 4).
//   - Reserve applies the delivery gate under the same short lock as the
//     stop transition: after the gate closes, queued completions are
//     dropped without reservation and never reach the script. A reserved
//     reply must be fed into its one resolver entry even if the gate closes
//     afterwards.
type Host interface {
	// Submit admits one nested tool call. args must already be a validated,
	// bounded JSON object. On admission it returns the CallID; on immediate
	// rejection (limit, closed admission, bad name/args) it returns a
	// *Failure and no CallID.
	Submit(name string, args json.RawMessage) (uint64, *Failure)
	// Reserve blocks the VM owner until one admitted call has completed and
	// its single delivery is reserved, or the execution has stopped
	// (ok=false). Call it only from the VM owner goroutine.
	Reserve() (reply CallReply, ok bool)
	// FinishDelivery reports that the one resolver entry for callID has
	// returned or aborted. Finishing after stop is metadata only.
	FinishDelivery(callID uint64)
}

// CallReply is the bounded completion of one admitted nested call. Value is
// the canonical ToolValue JSON (value/content/data/images/status); Failure,
// when set, rejects the script-side promise.
type CallReply struct {
	CallID  uint64
	Value   json.RawMessage // encoded ToolValue
	Failure *Failure        // non-nil on recoverable rejection
}

// RunRequest carries everything one script execution needs. All fields are
// owned by the bridge for the duration of Run; the executor must not retain
// them beyond Run returning (every cross-boundary value is copied plain
// JSON, never script objects or live Go handles).
type RunRequest struct {
	Source     string
	Generation string
	Tools      []ToolDescriptor
	Host       Host
	Limits     Limits
	Control    Control
}

// RunOutcome is what the executor reports for one run. Failure holds
// executor-level failures only (script, result_encoding, internal);
// supervisor-selected causes (timeout, cancellation, unawaited calls, host
// hard errors) are selected by the bridge and win over anything here per
// D12's first-registered-cause rule.
type RunOutcome struct {
	// Value is the JSON-compatible terminal value, already encoded within
	// OutputBytes. ValueOmitted reports that a terminal value existed but
	// could not be represented within the display budget; Value is then nil
	// and the projector emits the omission marker (D11.2).
	Value        json.RawMessage
	ValueOmitted bool
	Images       []ImageRef
	Failure      *Failure
}

// Limits carries the per-execution budgets resolved from Config (D6). The
// bridge resolves defaults before Run; a resolved value is always positive
// (GuestSteps/GuestMemoryBytes are Config-level capability checks and never
// appear here). WallClock starts at runCodeTool.Execute — the outer Agent
// approval happens before Execute and is outside this budget (D6).
type Limits struct {
	WallClock          time.Duration
	VMExitTimeout      time.Duration
	HostDrainTimeout   time.Duration
	MaxCalls           int
	MaxHostConcurrency int
	SourceBytes        int
	ArgsBytes          int
	HostResultBytes    int
	HostDataBytes      int
	MaxImages          int
	StdoutBytes        int
	OutputBytes        int
	ProjectionDepth    int
	ProjectionElements int
	// GuestSteps / GuestMemoryBytes carry the Config guest limits to the
	// backend (F16); Connect only accepts them when the Profile declares the
	// domain, so the default goja backend never sees them set.
	GuestSteps       uint64
	GuestMemoryBytes uint64
}

// Control is the executor-facing control surface for one execution. All
// methods are thread-safe; except RegisterInterrupt, the executor calls them
// only from its VM owner goroutine. Control never executes tools or
// approvals and never exposes the VM to the supervisor (D4).
type Control interface {
	// RegisterInterrupt registers a non-blocking stop hook — for goja, the
	// Runtime's interrupt function. The hook must not wait on user code. If
	// the execution has already been asked to stop, fn is invoked before
	// RegisterInterrupt returns.
	RegisterInterrupt(fn func())
	// StopRequested reports whether stop has been requested (deadline,
	// cancellation, terminal cause).
	StopRequested() bool
	// ScriptSettled marks the top-level continuation as fired (D13 step 6).
	// It closes further Submissions: with no pending unawaited calls the
	// execution enters finishing, otherwise stopping with the
	// unawaited_calls cause. ScriptSettled does not mean the VM exited.
	ScriptSettled()
	// PrintString captures one stdout chunk, encoding runes from s until
	// the StdoutBytes budget is exhausted (D5). It returns the number of
	// bytes accepted and whether the budget ran out (the caller stops
	// saving and flags truncation).
	PrintString(s string) (written int, exhausted bool)
}
