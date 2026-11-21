package modelmeta

import (
	"fmt"
	"reflect"
	"time"

	"github.com/dailz1/go-agent/llm"
)

// Guard delegates model calls and checks age at the start of each window query.
// Use the result in agent.New or agent.WithProviders. The underlying provider
// must carry windows exported from this same catalog with the same age policy.
// Only llm.Provider and llm.ContextWindowProvider are preserved.
func (c *Catalog) Guard(provider llm.Provider, maxAge time.Duration) (llm.Provider, error) {
	if c == nil || c.info.VerifiedAt.IsZero() || len(c.windows) == 0 || maxAge < 0 {
		return nil, fmt.Errorf("guard catalog or age: %w", ErrInvalidArgument)
	}
	if provider == nil || nilValue(provider) {
		return nil, fmt.Errorf("guard provider: %w", ErrInvalidArgument)
	}
	windows, ok := provider.(llm.ContextWindowProvider)
	if !ok {
		return nil, fmt.Errorf("guard requires window capability: %w", ErrInvalidArgument)
	}
	if maxAge == 0 {
		maxAge = DefaultMaxAge
	}
	return guardedProvider{
		Provider: provider, windows: windows,
		verifiedAt: c.info.VerifiedAt, maxAge: maxAge,
	}, nil
}

func nilValue(provider llm.Provider) bool {
	v := reflect.ValueOf(provider)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

type guardedProvider struct {
	llm.Provider
	windows    llm.ContextWindowProvider
	verifiedAt time.Time
	maxAge     time.Duration
}

func (p guardedProvider) ContextWindow(model string) (int, bool) {
	at := timeNow()
	if at.Before(p.verifiedAt) || at.Sub(p.verifiedAt) >= p.maxAge {
		return 0, false
	}
	return p.windows.ContextWindow(model)
}
