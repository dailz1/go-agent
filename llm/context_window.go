package llm

import (
	"errors"
	"reflect"
)

// ContextWindowProvider is an optional Provider capability: per-model
// context-window metadata in tokens, supplied by the host at construction.
// The agent kernel queries the resolved provider once per round and folds
// the answer into the effective context window
// (docs/DESIGN.md, P1「模型上下文窗口声明与有效窗口」).
//
// Providers without this capability simply carry no declared windows; the
// kernel falls back to the constructor-level window. The model argument is
// passed through exactly as the kernel resolved it — an empty string means
// no model was configured, and the implementation decides what that means
// (the repo adapters resolve it to their constructor default). Matching
// policy beyond that (aliases, dated-snapshot families) is the
// implementation's business; the kernel never normalizes model names.
type ContextWindowProvider interface {
	ContextWindow(model string) (int, bool)
}

// DefaultModelReporter is an optional Provider capability: reporting the
// default model name the provider would use when no model is configured,
// for diagnostics only (issue #23). An empty result means the name is
// unavailable. The agent uses it solely to attribute the
// undeclared-window warning; never for window queries, request
// construction, or request identity. Implementations must be safe for
// concurrent calls and perform no I/O.
type DefaultModelReporter interface {
	DefaultModelName() string
}

// WithContextWindows attaches per-model context-window metadata (in tokens)
// to any Provider, returning a Provider that also implements
// [ContextWindowProvider]. It exists so providers that do not implement the
// optional interface themselves — including third-party implementations and
// decorators — can still carry declarations. The map is copied; entries with
// non-positive values are treated as undeclared and dropped. Registering an
// empty-string model declares the window for requests that carry no model.
// A nil provider is rejected; nil or fully filtered windows return the
// original provider unchanged, preserving any capabilities it already has.
// A returned wrapper implements only [Provider] and
// [ContextWindowProvider]; it does not forward other optional capabilities
// such as [DefaultModelReporter].
func WithContextWindows(p Provider, windows map[string]int) (Provider, error) {
	if p == nil || isNilProviderValue(p) {
		return nil, errors.New("llm: WithContextWindows requires a non-nil provider")
	}
	filtered := make(map[string]int, len(windows))
	for model, tokens := range windows {
		if tokens > 0 {
			filtered[model] = tokens
		}
	}
	if len(filtered) == 0 {
		return p, nil
	}
	return contextWindowWrapper{Provider: p, windows: filtered}, nil
}

type contextWindowWrapper struct {
	Provider
	windows map[string]int
}

// isNilProviderValue reports typed-nil provider pointers (for example a nil
// *openai.Provider stored in the interface): wrapping one would pass
// registration and panic on first use.
func isNilProviderValue(p Provider) bool {
	v := reflect.ValueOf(p)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func (w contextWindowWrapper) ContextWindow(model string) (int, bool) {
	tokens, ok := w.windows[model]
	return tokens, ok
}
