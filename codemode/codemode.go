package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dailz1/go-agent/tool"
)

// ApprovalFunc matches agent.ApprovalFunc so one policy function can be
// passed to both agent.WithApprovalFn and codemode.WithApproval.
type ApprovalFunc func(tool.ToolInfo, json.RawMessage) bool

// CallObserver receives metadata-only lifecycle events (裁决 B). Events
// never carry code, arguments, Data, Content or image payloads. Delivery is
// best-effort: the queue is bounded and overflow is counted, never blocking
// execution paths. It is a UI signal, not a reliable audit log.
type CallObserver func(CallEvent)

// CallState enumerates the observable lifecycle states.
type CallState string

const (
	CallQueued               CallState = "queued"
	CallApproving            CallState = "approving"
	CallStarted              CallState = "started"
	CallFinished             CallState = "finished"
	CallDenied               CallState = "denied"
	CallNotFound             CallState = "not_found"
	CallCatalogStale         CallState = "catalog_stale"
	CallCancelledBeforeStart CallState = "cancelled_before_start"
	CallStopping             CallState = "stopping"
	CallResponseReturned     CallState = "response_returned"
	CallCleanupComplete      CallState = "cleanup_complete"
	CallLateFinished         CallState = "late_finished"
	CallLateDeliveryFinished CallState = "late_delivery_finished"
)

// CallEvent is one observer notification. CallID is 0 for execution-level
// events (stopping, response_returned, cleanup_complete). Duration is set
// only on late events; Note carries bounded diagnostic text.
type CallEvent struct {
	Seq        uint64
	ExecID     string
	Generation string
	CallID     uint64
	Tool       string
	State      CallState
	Duration   time.Duration
	Note       string
}

// Defaults from the design decision table (D6). They are conservative
// initial values, not measured performance conclusions.
const (
	DefaultWallClock          = 60 * time.Second
	DefaultVMExitTimeout      = 1 * time.Second
	DefaultHostDrainTimeout   = 5 * time.Second
	DefaultMaxLiveRuns        = 2
	DefaultMaxCalls           = 64
	DefaultMaxHostConcurrency = 4
	DefaultSourceBytes        = 64 << 10
	DefaultArgsBytes          = 64 << 10
	DefaultHostResultBytes    = 1 << 20
	DefaultHostDataBytes      = 8 << 20
	DefaultMaxImages          = 8
	DefaultStdoutBytes        = 64 << 10
	DefaultOutputBytes        = 1 << 20
	DefaultProjectionDepth    = 32
	DefaultProjectionElements = 100000
	DefaultDescriptionBudget  = 4096 // runes, skills' 2048 precedent widened by design (D8)
)

// DefaultToolName is the outer tool name unless WithToolName overrides it.
const DefaultToolName = "run_code"

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Config carries every budget and bound (D6). Zero means default; negative
// values are rejected by Connect. GuestSteps/GuestMemoryBytes are capability
// gates: setting them on a backend whose Profile does not declare the
// corresponding domain is a hard error, never a silent no-op (D5).
type Config struct {
	WallClock          time.Duration
	VMExitTimeout      time.Duration
	HostDrainTimeout   time.Duration
	MaxLiveRuns        int
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
	DescriptionBudget  int
	GuestSteps         uint64
	GuestMemoryBytes   uint64
}

// resolvedConfig is a validated Config with defaults applied.
type resolvedConfig struct {
	wallClock          time.Duration
	vmExitTimeout      time.Duration
	hostDrainTimeout   time.Duration
	maxLiveRuns        int
	maxCalls           int
	maxHostConcurrency int
	sourceBytes        int
	argsBytes          int
	hostResultBytes    int
	hostDataBytes      int
	maxImages          int
	stdoutBytes        int
	outputBytes        int
	projectionDepth    int
	projectionElements int
	descriptionBudget  int
	guestSteps         uint64
	guestMemoryBytes   uint64
}

type option struct {
	approval    ApprovalFunc
	approvalReq bool
	filter      map[string]bool
	name        string
	observer    CallObserver
	executor    Executor
}

// Option configures Connect. Options are validated where noted; failures
// surface as Connect errors before anything is registered.
type Option func(*option)

// WithApproval installs the nested-call approval policy. The same function
// shape is accepted by agent.WithApprovalFn; hosts sharing one terminal or
// UI must serialize policy invocations themselves (D10).
func WithApproval(fn ApprovalFunc) Option {
	return func(o *option) { o.approval = fn }
}

// WithApprovalRequired controls only the outer run_code approval (blob
// approval of the whole code, D10). It defaults to true; nested tool
// approvals always apply regardless of this option.
func WithApprovalRequired(required bool) Option {
	return func(o *option) { o.approvalReq = required }
}

// WithToolFilter restricts the exposed source-tool subset. Names outside the
// source registry are ignored; calling it with no names exposes source.List()
// in full (an empty filter must not silently hide every tool).
func WithToolFilter(names ...string) Option {
	return func(o *option) {
		if len(names) == 0 {
			o.filter = nil
			return
		}
		o.filter = make(map[string]bool, len(names))
		for _, n := range names {
			o.filter[n] = true
		}
	}
}

// WithToolName renames the outer tool. Must match ^[A-Za-z0-9_-]{1,64}$.
func WithToolName(name string) Option {
	return func(o *option) { o.name = name }
}

// WithCallObserver installs the metadata observer (see CallObserver).
func WithCallObserver(fn CallObserver) Option {
	return func(o *option) { o.observer = fn }
}

// WithExecutor replaces the default goja backend. The executor must honor
// the Executor contract; the satellite maintains only the default
// implementation (D16).
func WithExecutor(exec Executor) Option {
	return func(o *option) { o.executor = exec }
}

func resolveOptions(opts []Option) (option, error) {
	o := option{approvalReq: true, name: DefaultToolName}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if !toolNamePattern.MatchString(o.name) {
		return option{}, fmt.Errorf("codemode tool name %q must match ^[A-Za-z0-9_-]{1,64}$", o.name)
	}
	return o, nil
}

func resolveConfig(cfg Config) (resolvedConfig, error) {
	pos := func(v, def int, name string) (int, error) {
		if v < 0 {
			return 0, fmt.Errorf("codemode %s must not be negative", name)
		}
		if v == 0 {
			return def, nil
		}
		return v, nil
	}
	dur := func(v, def time.Duration, name string) (time.Duration, error) {
		if v < 0 {
			return 0, fmt.Errorf("codemode %s must not be negative", name)
		}
		if v == 0 {
			return def, nil
		}
		return v, nil
	}
	var r resolvedConfig
	var err error
	if r.wallClock, err = dur(cfg.WallClock, DefaultWallClock, "wall clock"); err != nil {
		return r, err
	}
	if r.vmExitTimeout, err = dur(cfg.VMExitTimeout, DefaultVMExitTimeout, "vm exit timeout"); err != nil {
		return r, err
	}
	if r.hostDrainTimeout, err = dur(cfg.HostDrainTimeout, DefaultHostDrainTimeout, "host drain timeout"); err != nil {
		return r, err
	}
	if r.maxLiveRuns, err = pos(cfg.MaxLiveRuns, DefaultMaxLiveRuns, "max live runs"); err != nil {
		return r, err
	}
	if r.maxCalls, err = pos(cfg.MaxCalls, DefaultMaxCalls, "max calls"); err != nil {
		return r, err
	}
	if r.maxHostConcurrency, err = pos(cfg.MaxHostConcurrency, DefaultMaxHostConcurrency, "max host concurrency"); err != nil {
		return r, err
	}
	if r.sourceBytes, err = pos(cfg.SourceBytes, DefaultSourceBytes, "source bytes"); err != nil {
		return r, err
	}
	if r.argsBytes, err = pos(cfg.ArgsBytes, DefaultArgsBytes, "args bytes"); err != nil {
		return r, err
	}
	if r.hostResultBytes, err = pos(cfg.HostResultBytes, DefaultHostResultBytes, "host result bytes"); err != nil {
		return r, err
	}
	if r.hostDataBytes, err = pos(cfg.HostDataBytes, DefaultHostDataBytes, "host data bytes"); err != nil {
		return r, err
	}
	if r.maxImages, err = pos(cfg.MaxImages, DefaultMaxImages, "max images"); err != nil {
		return r, err
	}
	if r.stdoutBytes, err = pos(cfg.StdoutBytes, DefaultStdoutBytes, "stdout bytes"); err != nil {
		return r, err
	}
	if r.outputBytes, err = pos(cfg.OutputBytes, DefaultOutputBytes, "output bytes"); err != nil {
		return r, err
	}
	if r.outputBytes <= outputMarkerReserve {
		return r, fmt.Errorf("codemode output bytes must exceed the %d-byte marker reserve", outputMarkerReserve)
	}
	if r.projectionDepth, err = pos(cfg.ProjectionDepth, DefaultProjectionDepth, "projection depth"); err != nil {
		return r, err
	}
	if r.projectionElements, err = pos(cfg.ProjectionElements, DefaultProjectionElements, "projection elements"); err != nil {
		return r, err
	}
	if r.descriptionBudget, err = pos(cfg.DescriptionBudget, DefaultDescriptionBudget, "description budget"); err != nil {
		return r, err
	}
	r.guestSteps = cfg.GuestSteps
	r.guestMemoryBytes = cfg.GuestMemoryBytes
	return r, nil
}

// generation is one immutable tool catalog version (D9). A wrapper is bound
// to exactly one generation for its whole life; execution reads only the
// generation it was started with, never the CodeMode's current pointer.
type generation struct {
	version     string
	tools       []ToolDescriptor
	allowSet    map[string]bool
	paramsFP    map[string]string // name → marshaled Parameters (fingerprint bytes)
	docs        APIDocument
	description string
	profile     ExecutorProfile
}

// CodeMode owns the run_code lifecycle over one pair of registries.
type CodeMode struct {
	source   *tool.Registry
	exposed  *tool.Registry
	executor Executor
	cfg      resolvedConfig
	name     string

	approval              ApprovalFunc
	outerApprovalRequired bool
	filter                map[string]bool

	lifecycleMu sync.Mutex
	owned       *runCodeTool
	closed      atomic.Bool

	observer CallObserver

	// live executions ledger: admitted runs count against MaxLiveRuns and
	// keep their slots until real exit, including after the response
	// returned (C).
	liveMu sync.Mutex
	live   map[*execution]struct{}
	lifeWG sync.WaitGroup

	// approvalSlot serializes nested approvals for this CodeMode (D10).
	approvalSlot chan struct{}

	// obsMu guards obsQueue sends against the close in Close: emit and the
	// close are mutually excluded, so no send can hit a closed channel (F4).
	obsMu    sync.Mutex
	obsClose bool
	obsQueue chan CallEvent
	obsWG    sync.WaitGroup
	seq      atomic.Uint64
	dropped  atomic.Uint64
	genSeq   atomic.Uint64

	closeOnce sync.Once
}

// Connect builds the first generation from source, registers run_code into
// exposed, and returns the lifecycle owner. source==exposed is rejected (D0):
// the tool-face reduction only exists when the model registry is separate.
// Nothing is registered on failure.
func Connect(_ context.Context, source, exposed *tool.Registry, cfg Config, opts ...Option) (*CodeMode, error) {
	if source == nil || exposed == nil {
		return nil, fmt.Errorf("codemode source and exposed registries must not be nil")
	}
	if source == exposed {
		return nil, fmt.Errorf("codemode source and exposed must be different registries (D0)")
	}
	opt, err := resolveOptions(opts)
	if err != nil {
		return nil, err
	}
	rc, err := resolveConfig(cfg)
	if err != nil {
		return nil, err
	}
	exec := opt.executor
	if exec == nil {
		exec = NewGojaExecutor()
	}
	m := &CodeMode{
		source:                source,
		exposed:               exposed,
		executor:              exec,
		cfg:                   rc,
		name:                  opt.name,
		approval:              opt.approval,
		outerApprovalRequired: opt.approvalReq,
		filter:                opt.filter,
		observer:              opt.observer,
		live:                  make(map[*execution]struct{}),
		approvalSlot:          make(chan struct{}, 1),
		obsQueue:              make(chan CallEvent, 1024),
	}
	// capability gates (D5): requested guest limits on a backend that does
	// not declare the domain fail construction.
	profile := exec.Profile()
	if rc.guestSteps > 0 && !profile.GuestStepLimit {
		return nil, fmt.Errorf("codemode guest steps configured but backend %q does not declare GuestStepLimit", profile.Backend)
	}
	if rc.guestMemoryBytes > 0 && !profile.GuestMemoryBytes {
		return nil, fmt.Errorf("codemode guest memory configured but backend %q does not declare GuestMemoryBytes", profile.Backend)
	}
	gen, err := m.buildGeneration()
	if err != nil {
		return nil, err
	}
	wrapper := &runCodeTool{mode: m, gen: gen}
	if err := exposed.Register(wrapper); err != nil {
		return nil, fmt.Errorf("register %s: %w", m.name, err)
	}
	m.owned = wrapper
	m.obsWG.Add(1)
	go m.observeLoop()
	return m, nil
}

// buildGeneration lists source, applies the filter, marshals each schema via
// the public codec, renders the API document and assembles the bounded
// description. It runs under lifecycleMu (Connect or Refresh).
func (m *CodeMode) buildGeneration() (*generation, error) {
	infos := m.source.List()
	var descs []ToolDescriptor
	allow := make(map[string]bool, len(infos))
	fp := make(map[string]string, len(infos))
	for _, info := range infos {
		if m.filter != nil && !m.filter[info.Name] {
			continue
		}
		if info.Name == m.name {
			continue // never expose a same-named source tool over the wrapper
		}
		if handle, ok := m.source.Get(info.Name); ok {
			if _, isWrapper := handle.(*runCodeTool); isWrapper {
				continue // D7: never expose any codemode wrapper to scripts
			}
		}
		schemaBytes, err := json.Marshal(info.Parameters)
		if err != nil {
			return nil, fmt.Errorf("marshal parameters of tool %q: %w", info.Name, err)
		}
		descs = append(descs, ToolDescriptor{
			Name:        info.Name,
			Description: info.Description,
			InputSchema: schemaBytes,
		})
		allow[info.Name] = true
		fp[info.Name] = string(schemaBytes)
	}
	docs, err := m.executor.RenderAPI(descs)
	if err != nil {
		return nil, fmt.Errorf("render API: %w", err)
	}
	profile := m.executor.Profile()
	version := fmt.Sprintf("gen-%d", m.genSeq.Add(1))
	description, err := m.assembleDescription(version, profile, docs, descs)
	if err != nil {
		return nil, err
	}
	sort.Slice(descs, func(i, j int) bool { return descs[i].Name < descs[j].Name })
	return &generation{
		version:     version,
		tools:       descs,
		allowSet:    allow,
		paramsFP:    fp,
		docs:        docs,
		description: description,
		profile:     profile,
	}, nil
}

// assembleDescription builds the run_code Description within
// DescriptionBudget (runes). Overflow is a hard error, never a silent
// truncation that would hide declarations from the model (D8).
func (m *CodeMode) assembleDescription(version string, p ExecutorProfile, docs APIDocument, descs []ToolDescriptor) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Executes a %s script against the business tools. %s\n", p.Language, p.EntrySyntax)
	b.WriteString("Call business tools inside the script via tools[\"name\"]({json object args}); each call returns a Promise of {value, content, data?, images?, status?}. Intermediate results stay in the script; only the script's return value (plus images you explicitly pass to output(value, images)) is shown to the conversation. Prefer output(value, imageRefs) for images; console.log accepts strings only.\n")
	b.WriteString("Recoverable call failures reject the promise with {kind, message, result?}; catch them to branch. Tool names map exactly: tools[\"exact-registered-name\"].\n")
	fmt.Fprintf(&b, "Catalog %s. Tools exposed: %d. Declarations follow.\n", version, len(descs))
	if len(docs.Diagnostics) > 0 {
		fmt.Fprintf(&b, "Declaration notes: %d schema constructs are degraded (kept conservative, originals preserved).\n", len(docs.Diagnostics))
	}
	b.WriteString(docs.Instructions)
	b.WriteString("\n")
	b.WriteString(docs.Declarations)
	out := b.String()
	if len([]rune(out)) > m.cfg.descriptionBudget {
		return "", fmt.Errorf("codemode description needs %d runes, over the %d-rune budget (WithToolFilter or a larger DescriptionBudget required)",
			len([]rune(out)), m.cfg.descriptionBudget)
	}
	return out, nil
}

// Name returns the outer tool name this instance owns.
func (m *CodeMode) Name() string { return m.name }

// ToolInfo returns the model-facing Info of the currently published wrapper.
func (m *CodeMode) ToolInfo() tool.ToolInfo {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.owned.Info()
}

// CatalogVersion returns the current generation version: the exact string a
// run_code call must carry in catalog_version (D9).
func (m *CodeMode) CatalogVersion() string {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.owned.gen.version
}

// Refresh publishes a new generation atomically: build g2 fully, verify we
// still own the wrapper name, then Replace — the single publication point
// (D9). Executions started on g1 keep g1's allow-set and declarations.
func (m *CodeMode) Refresh() error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.closed.Load() {
		return fmt.Errorf("codemode is closed")
	}
	old := m.owned
	gen, err := m.buildGeneration()
	if err != nil {
		return err
	}
	wrapper := &runCodeTool{mode: m, gen: gen}
	current, ok := m.exposed.Get(m.name)
	if !ok || current != tool.Tool(old) {
		return fmt.Errorf("codemode ownership lost for %q (replaced or removed externally)", m.name)
	}
	if err := m.exposed.Replace(wrapper); err != nil {
		return fmt.Errorf("replace %s: %w", m.name, err)
	}
	// Post-publication verification: the registry has no CAS, so a concurrent
	// external Replace after ours is possible; hosts must serialize Refresh
	// with external mutations (documented contract). The re-check shrinks the
	// detection window to immediately after publication (ocr high).
	if current, ok := m.exposed.Get(m.name); !ok || current != tool.Tool(wrapper) {
		return fmt.Errorf("codemode ownership lost for %q immediately after Replace (replaced or removed externally)", m.name)
	}
	m.owned = wrapper
	return nil
}

// Snapshot reports executions still under supervision and observer drops.
type Snapshot struct {
	LiveRuns           int
	HostOutstanding    int
	DeliveriesInFlight int
	VMsNotExited       int
	DroppedEvents      uint64
}

// Snapshot summarizes supervised state; it is a UI signal, not an audit log.
func (m *CodeMode) Snapshot() Snapshot {
	m.liveMu.Lock()
	live := len(m.live)
	var host, deliveries, vmNotExited int
	for e := range m.live {
		host, deliveries, vmNotExited = host+e.hostOutstandingSnapshot(), deliveries+e.deliveriesSnapshot(), vmNotExited+e.vmNotExitedSnapshot()
	}
	m.liveMu.Unlock()
	return Snapshot{
		LiveRuns:           live,
		HostOutstanding:    host,
		DeliveriesInFlight: deliveries,
		VMsNotExited:       vmNotExited,
		DroppedEvents:      m.dropped.Load(),
	}
}

// Close is a graceful drain (C): close the global admission gate, request
// every supervised execution to stop, wait for all VMs, host workers and the
// observer to exit for real, then unregister the wrapper after an identity
// check. Close has no timeout guarantee and never reports success with work
// outstanding; a blocked host tool or approval callback delays it.
func (m *CodeMode) Close() {
	m.closeOnce.Do(func() {
		m.lifecycleMu.Lock()
		m.closed.Store(true)
		wrapper := m.owned
		m.lifecycleMu.Unlock()

		m.liveMu.Lock()
		live := make([]*execution, 0, len(m.live))
		for e := range m.live {
			live = append(live, e)
		}
		m.liveMu.Unlock()
		for _, e := range live {
			e.requestStop(FailureCancelled, "codemode closed")
		}
		m.lifeWG.Wait()

		if current, ok := m.exposed.Get(m.name); ok && current == tool.Tool(wrapper) {
			m.exposed.Unregister(m.name)
		}
		// After lifeWG the supervision paths are done emitting; exclude any
		// straggler send before closing so no emit can hit a closed channel
		// (F4).
		m.obsMu.Lock()
		m.obsClose = true
		close(m.obsQueue)
		m.obsMu.Unlock()
		m.obsWG.Wait()
	})
}

// observeLoop drains the bounded observer queue on one worker; overflow is
// counted, callbacks never block execution paths, a panicking callback is
// recorded and stops this observer.
func (m *CodeMode) observeLoop() {
	defer m.obsWG.Done()
	if m.observer == nil {
		// drain the queue so emit never blocks and drops stay counted (ocr
		// medium: a nil observer must not turn every event into a panic).
		for range m.obsQueue {
			m.dropped.Add(1)
		}
		return
	}
	dead := false
	for ev := range m.obsQueue {
		if dead {
			// The callback panicked once and stopped consuming: keep draining
			// and counting so emit never blocks and drops stay accurate
			// (ocr medium).
			m.dropped.Add(1)
			continue
		}
		if m.deliverEvent(ev) {
			dead = true
			m.dropped.Add(1)
		}
	}
}

// deliverEvent invokes the observer once; a panicking callback stops this
// observer permanently and is counted as a drop, so it can never take down
// execution or supervision paths.
func (m *CodeMode) deliverEvent(ev CallEvent) (panicked bool) {
	defer func() {
		if recover() != nil {
			m.dropped.Add(1)
			panicked = true
		}
	}()
	m.observer(ev)
	return false
}

// emit queues one metadata event with a monotonic sequence number. The
// obsMu section excludes the Close-time close of the queue (F4); queue-full
// drops are counted, never blocking (裁决 B observation contract).
func (m *CodeMode) emit(ev CallEvent) {
	m.obsMu.Lock()
	if m.obsClose {
		m.obsMu.Unlock()
		m.dropped.Add(1)
		return
	}
	ev.Seq = m.seq.Add(1)
	select {
	case m.obsQueue <- ev:
		m.obsMu.Unlock()
	default:
		m.obsMu.Unlock()
		m.dropped.Add(1)
	}
}
