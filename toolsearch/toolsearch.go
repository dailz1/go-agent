// Package toolsearch implements deferred tool loading: low-frequency tools
// live in a private source registry and the model initially sees a single
// search tool. A search ranks the source catalog with BM25 and publishes the
// top matches into the model-visible exposed registry, where they advertise
// full schemas from the next request onward. docs/TOOLSEARCH-DESIGN.md is
// the frozen contract.
//
// Wiring follows the codemode two-registry convention: the host passes both
// registries to Connect and gives the exposed registry to agent.New together
// with agent.WithDynamicTools, so loads become visible each iteration. The
// package imports only stdlib and github.com/dailz1/go-agent/tool.
package toolsearch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sync"

	"github.com/dailz1/go-agent/tool"
)

// DefaultToolName is the advertised name of the search tool.
const DefaultToolName = "tool_search"

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// muWaitHook, when set, runs just before the load lock is acquired. It is a
// white-box test seam in the same style as the agent package's package-var
// seams; production leaves it nil.
var muWaitHook func()

const (
	defaultLimit        = 5
	defaultMaxLoaded    = 256
	defaultCatalogLimit = 65536
	maxLimitCap         = 100
	maxQueryRunes       = 1024
	maxDescriptionRunes = 160
)

type config struct {
	toolName     string
	limit        int
	maxLoaded    int
	catalogLimit int
	logger       *slog.Logger
}

// Search owns the deferred-pool wiring: source holds the tools the host
// deferred, exposed is the model-visible registry this instance may extend
// with forwarding wrappers. mu serializes loads; wrapper execution for
// business tools runs outside it.
type Search struct {
	mu      sync.Mutex
	source  *tool.Registry
	exposed *tool.Registry
	cfg     config
	owned   map[string]*forwardTool
}

type Option func(*config)

// WithToolName renames the search tool; the default is DefaultToolName. The
// name must match [A-Za-z0-9_-]{1,64}; invalid values are rejected by
// Connect.
func WithToolName(name string) Option {
	return func(c *config) { c.toolName = name }
}

// WithLimit sets both the default and the maximum number of tools one search
// may load. n must be in [1,100]; the model may only lower it per call.
func WithLimit(n int) Option {
	return func(c *config) { c.limit = n }
}

// WithMaxLoaded caps how many distinct names the instance may have loaded at
// once; loads past the cap fail without unloading existing tools.
func WithMaxLoaded(n int) Option {
	return func(c *config) { c.maxLoaded = n }
}

// WithCatalogLimit caps the retrieved catalog (source names plus
// descriptions plus two separator runes each) in runes. Exceeding it makes
// searches fail loudly instead of silently dropping tools.
func WithCatalogLimit(runes int) Option {
	return func(c *config) { c.catalogLimit = runes }
}

// WithLogger sets the logger for load events; nil means discard. Loads log
// one metadata line (names only, never queries or business results).
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) {
		if logger == nil {
			logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		c.logger = logger
	}
}

// Connect validates the configuration and registers the search tool into
// exposed. On any error the exposed registry is left unchanged. source and
// exposed must be non-nil and different registries; source tools whose names
// already exist in exposed are rejected, and the search tool name must not
// be taken in source either. The ctx is only observed for cancellation
// during initialization; cancelling it later does not unregister anything.
func Connect(ctx context.Context, source, exposed *tool.Registry,
	opts ...Option) (*Search, error) {
	if source == nil || exposed == nil {
		return nil, fmt.Errorf("toolsearch source and exposed registries must not be nil")
	}
	if source == exposed {
		return nil, fmt.Errorf("toolsearch source and exposed must be different registries")
	}
	cfg := config{
		toolName:     DefaultToolName,
		limit:        defaultLimit,
		maxLoaded:    defaultMaxLoaded,
		catalogLimit: defaultCatalogLimit,
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !toolNamePattern.MatchString(cfg.toolName) {
		return nil, fmt.Errorf("toolsearch tool name %q must match [A-Za-z0-9_-]{1,64}", cfg.toolName)
	}
	if cfg.limit < 1 || cfg.limit > maxLimitCap {
		return nil, fmt.Errorf("toolsearch limit %d must be in [1,%d]", cfg.limit, maxLimitCap)
	}
	if cfg.maxLoaded < 1 {
		return nil, fmt.Errorf("toolsearch max loaded %d must be positive", cfg.maxLoaded)
	}
	if cfg.catalogLimit < 1 {
		return nil, fmt.Errorf("toolsearch catalog limit %d must be positive", cfg.catalogLimit)
	}
	if _, ok := source.Get(cfg.toolName); ok {
		return nil, fmt.Errorf("toolsearch tool name %q is already used by a source tool", cfg.toolName)
	}
	infos := source.List()
	inExposed := make(map[string]struct{}, len(exposed.List()))
	for _, info := range exposed.List() {
		inExposed[info.Name] = struct{}{}
	}
	for _, info := range infos {
		if _, clash := inExposed[info.Name]; clash {
			return nil, fmt.Errorf("toolsearch source tool %q is already registered in exposed", info.Name)
		}
	}
	if runes := catalogRunes(infos, int64(cfg.catalogLimit)); runes > int64(cfg.catalogLimit) {
		return nil, fmt.Errorf("toolsearch catalog uses %d runes over the %d limit; shrink the deferred pool or raise WithCatalogLimit", runes, cfg.catalogLimit)
	}

	s := &Search{
		source:  source,
		exposed: exposed,
		cfg:     cfg,
		owned:   make(map[string]*forwardTool),
	}
	if err := exposed.Register(&searchTool{s: s}); err != nil {
		return nil, fmt.Errorf("register toolsearch tool %q: %w", cfg.toolName, err)
	}
	return s, nil
}

// Name returns the advertised name of the search tool.
func (s *Search) Name() string {
	return s.cfg.toolName
}

// catalogRunes measures the retrieved catalog: every source tool's name,
// its description, and two separator runes per entry. Counting stops early
// once the configured budget is exceeded; int64 counting cannot overflow at
// these magnitudes on any build.
func catalogRunes(infos []tool.ToolInfo, budget int64) int64 {
	var total int64
	for _, info := range infos {
		total += int64(len([]rune(info.Name)) + len([]rune(info.Description)) + 2)
		if total > budget {
			return total
		}
	}
	return total
}
