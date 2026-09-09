package llm

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/dailz1/go-agent/pkg/tool"
)

var errTestFactory = errors.New("factory error")

type mockProvider struct {
	name string
}

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) Chat(_ context.Context, _ []Message, _ []tool.ToolInfo, _ ...Option) (*Message, *Usage, error) {
	return nil, nil, nil
}
func (m *mockProvider) ChatStream(_ context.Context, _ []Message, _ []tool.ToolInfo, _ ...Option) (iter.Seq2[Chunk, error], error) {
	return nil, nil
}

func TestRegisterAndCreate(t *testing.T) {
	resetRegistry()
	RegisterProvider("test", func(cfg ProviderConfig) (Provider, error) {
		return &mockProvider{name: "test"}, nil
	})
	p, err := CreateProvider("test", ProviderConfig{APIKey: "key", Model: "model"})
	if err != nil {
		t.Fatalf("CreateProvider failed: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.Name() != "test" {
		t.Errorf("Name() = %q, want %q", p.Name(), "test")
	}
}

func TestUnknownProvider(t *testing.T) {
	resetRegistry()
	_, err := CreateProvider("unknown", ProviderConfig{})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "unknown provider")
	}
}

func TestRegisteredProviders(t *testing.T) {
	resetRegistry()
	RegisterProvider("openai", func(cfg ProviderConfig) (Provider, error) { return nil, nil })
	RegisterProvider("glm", func(cfg ProviderConfig) (Provider, error) { return nil, nil })
	providers := RegisteredProviders()
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
	if providers[0] != "glm" || providers[1] != "openai" {
		t.Errorf("expected [glm, openai], got %v", providers)
	}
}

func TestRegisteredProviders_Empty(t *testing.T) {
	resetRegistry()
	providers := RegisteredProviders()
	if len(providers) != 0 {
		t.Errorf("expected empty list, got %v", providers)
	}
}

func TestRegisterProvider_DuplicatePanics(t *testing.T) {
	resetRegistry()
	RegisterProvider("test", func(cfg ProviderConfig) (Provider, error) { return nil, nil })
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value = %v (%T), want string", r, r)
		}
		if !strings.Contains(msg, "already registered") {
			t.Errorf("panic = %q, want it to contain %q", msg, "already registered")
		}
	}()
	RegisterProvider("test", func(cfg ProviderConfig) (Provider, error) { return nil, nil })
}

func TestCreateProvider_FactoryError(t *testing.T) {
	resetRegistry()
	RegisterProvider("broken", func(cfg ProviderConfig) (Provider, error) {
		return nil, errTestFactory
	})
	p, err := CreateProvider("broken", ProviderConfig{})
	if p != nil {
		t.Error("expected nil provider")
	}
	if err != errTestFactory {
		t.Errorf("error = %v, want %v", err, errTestFactory)
	}
}

func TestRegisteredProviders_Sorted(t *testing.T) {
	resetRegistry()
	RegisterProvider("delta", func(cfg ProviderConfig) (Provider, error) { return nil, nil })
	RegisterProvider("alpha", func(cfg ProviderConfig) (Provider, error) { return nil, nil })
	RegisterProvider("charlie", func(cfg ProviderConfig) (Provider, error) { return nil, nil })
	providers := RegisteredProviders()
	if providers[0] != "alpha" || providers[1] != "charlie" || providers[2] != "delta" {
		t.Errorf("expected [alpha, charlie, delta], got %v", providers)
	}
}

func TestRegistryCleanState(t *testing.T) {
	resetRegistry()

	if got := len(RegisteredProviders()); got != 0 {
		t.Fatalf("after reset, len(RegisteredProviders()) = %d, want 0", got)
	}

	RegisterProvider("one", func(ProviderConfig) (Provider, error) { return nil, nil })
	if got := len(RegisteredProviders()); got != 1 {
		t.Fatalf("after one registration, len(RegisteredProviders()) = %d, want 1", got)
	}

	resetRegistry()

	if got := len(RegisteredProviders()); got != 0 {
		t.Fatalf("after second reset, len(RegisteredProviders()) = %d, want 0", got)
	}
}
