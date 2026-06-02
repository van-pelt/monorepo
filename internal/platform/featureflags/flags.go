// Package featureflags is the abstraction for runtime feature toggles.
// Today only InMemoryProvider exists — flags are loaded once from config
// at startup and never change. When a third-party SDK arrives
// (LaunchDarkly, Unleash, Statsig), it becomes a second Provider
// implementation; callers stay agnostic.
//
// Callers receive a Provider and look up flags by name with a default
// fallback. The default is the source-of-truth for "what does this flag
// mean when nothing else is configured" — code reads as if the flag is
// always present.
//
// Example:
//
//	type Module struct {
//	    flags featureflags.Provider
//	    // ...
//	}
//
//	func (m *Module) DoStuff(ctx context.Context) error {
//	    if m.flags.Bool(ctx, "enable_new_payment_flow", false) {
//	        return m.newFlow(ctx)
//	    }
//	    return m.legacyFlow(ctx)
//	}
package featureflags

import (
	"context"
	"maps"
	"sync"
)

// Provider returns the current value of a flag. ctx is passed so future
// implementations (LaunchDarkly, etc.) can resolve per-user or per-tenant
// from request claims; in-memory provider ignores it.
//
// Each typed method returns the default when the flag is absent or stored
// with a different type — flag misconfiguration shouldn't break callers.
type Provider interface {
	Bool(ctx context.Context, name string, defaultValue bool) bool
	String(ctx context.Context, name string, defaultValue string) string
	Int(ctx context.Context, name string, defaultValue int) int
}

// InMemoryProvider is the static, in-process Provider. Flags are loaded
// once at construction (typically from config) and mutable only via Set
// — convenient for tests, intentionally simplistic for production
// (real deployments should swap in a remote-config Provider).
type InMemoryProvider struct {
	mu    sync.RWMutex
	flags map[string]any
}

// NewInMemoryProvider snapshots initial; the caller's map can be mutated
// later without affecting the provider.
func NewInMemoryProvider(initial map[string]any) *InMemoryProvider {
	snapshot := make(map[string]any, len(initial))
	maps.Copy(snapshot, initial)
	return &InMemoryProvider{flags: snapshot}
}

// Set overrides a flag at runtime. Safe to call concurrently with reads.
func (p *InMemoryProvider) Set(name string, value any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flags[name] = value
}

// Count returns the number of configured flags. Useful for a startup log
// line that confirms the provider was wired.
func (p *InMemoryProvider) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.flags)
}

func (p *InMemoryProvider) Bool(_ context.Context, name string, defaultValue bool) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if v, ok := p.flags[name].(bool); ok {
		return v
	}
	return defaultValue
}

func (p *InMemoryProvider) String(_ context.Context, name string, defaultValue string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if v, ok := p.flags[name].(string); ok {
		return v
	}
	return defaultValue
}

func (p *InMemoryProvider) Int(_ context.Context, name string, defaultValue int) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	switch v := p.flags[name].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		// Viper decodes JSON/YAML numbers as float64 by default.
		return int(v)
	}
	return defaultValue
}
