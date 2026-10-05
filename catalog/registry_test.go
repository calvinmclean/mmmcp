package catalog_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/catalog"
	"github.com/obot-platform/mmmcp/component"
	"github.com/obot-platform/mmmcp/config"
)

type countingDiscoverer struct {
	mu      sync.Mutex
	count   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
}

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestFingerprintIsStableCompleteAndSecretSafe(t *testing.T) {
	cfg := &config.Config{Servers: []config.Server{{
		Name: "fixture", URL: "https://example.invalid", Headers: map[string]string{"Authorization": "Bearer secret"},
	}}}
	first, err := catalog.Fingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	equivalent := &config.Config{Servers: []config.Server{{
		Name: "fixture", URL: "https://example.invalid", Headers: map[string]string{"Authorization": "Bearer secret"}, Args: []string{}, Env: map[string]string{},
	}}}
	second, err := catalog.Fingerprint(equivalent)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("equivalent fingerprints differ: %q != %q", first, second)
	}
	if strings.Contains(first, "secret") || strings.Contains(first, "example.invalid") {
		t.Fatalf("fingerprint exposes config values: %q", first)
	}
	changed := *cfg
	changed.Listen = "127.0.0.1:9000"
	third, err := catalog.Fingerprint(&changed)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("configuration change did not change fingerprint")
	}
}

func TestRegistryDiscoveryRevisionTriggersRediscovery(t *testing.T) {
	discoverer := &countingDiscoverer{started: make(chan struct{}), release: make(chan struct{})}
	close(discoverer.release)
	registry := catalog.NewRegistry(discoverer)
	defer registry.Close()
	for i, field := range []string{"", "    discoveryRevision: revision-1\n"} {
		cfg, err := config.Load([]byte("servers:\n  - name: fixture\n    url: https://example.invalid\n"+field), config.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			_, _, err := registry.Get(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
		}
		if got := discoverer.Count(); got != i+1 {
			t.Fatalf("discoveries = %d, want %d", got, i+1)
		}
	}
}

func TestRegistryDeduplicatesConcurrentCompilation(t *testing.T) {
	discoverer := &countingDiscoverer{started: make(chan struct{}), release: make(chan struct{})}
	registry := catalog.NewRegistry(discoverer)
	cfg := &config.Config{Servers: []config.Server{{Name: "fixture", URL: "https://example.invalid"}}}

	const callers = 8
	results := make(chan *catalog.Catalog, callers)
	errors := make(chan error, callers)
	for range callers {
		go func() {
			compiled, _, err := registry.Get(t.Context(), cfg)
			results <- compiled
			errors <- err
		}()
	}
	<-discoverer.started
	close(discoverer.release)

	var first *catalog.Catalog
	for range callers {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		compiled := <-results
		if first == nil {
			first = compiled
		} else if compiled != first {
			t.Fatal("registry returned distinct metadata snapshots")
		}
	}
	if got := discoverer.Count(); got != 1 {
		t.Fatalf("discoveries = %d, want 1", got)
	}
}

func TestRegistryWaitersReturnInitialCompilationError(t *testing.T) {
	failure := errors.New("discovery failed")
	discoverer := &countingDiscoverer{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     failure,
	}
	defer func() {
		select {
		case <-discoverer.release:
		default:
			close(discoverer.release)
		}
	}()
	registry := catalog.NewRegistry(discoverer)
	defer registry.Close()
	cfg := &config.Config{ToolSearch: true, Servers: []config.Server{{
		Name: "fixture",
		URL:  "https://example.invalid",
	}}}

	firstDone := make(chan error, 1)
	go func() {
		_, _, err := registry.Get(t.Context(), cfg)
		firstDone <- err
	}()
	<-discoverer.started

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiterCtx := &observedDoneContext{Context: ctx, observed: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() {
		_, _, err := registry.Get(waiterCtx, cfg)
		waiterDone <- err
	}()
	select {
	case <-waiterCtx.observed:
	case <-time.After(time.Second):
		t.Fatal("second caller did not wait for initial compilation")
	}
	close(discoverer.release)

	for _, done := range []<-chan error{firstDone, waiterDone} {
		select {
		case err := <-done:
			if !errors.Is(err, failure) {
				t.Fatalf("initial compilation returned %v, want %v", err, failure)
			}
		case <-time.After(time.Second):
			t.Fatal("caller remained blocked after initial compilation failed")
		}
	}
	if got := discoverer.Count(); got != 1 {
		t.Fatalf("discoveries = %d, want 1", got)
	}
}

func TestRegistryCloseDuringInitialCompilation(t *testing.T) {
	const attempts = 32
	for range attempts {
		discoverer := &countingDiscoverer{started: make(chan struct{}), release: make(chan struct{})}
		registry := catalog.NewRegistry(discoverer)
		cfg := &config.Config{
			ToolSearch: true,
			Servers:    []config.Server{{Name: "fixture", URL: "https://example.invalid"}},
		}

		getDone := make(chan error, 1)
		go func() {
			_, _, err := registry.Get(t.Context(), cfg)
			getDone <- err
		}()
		<-discoverer.started

		closeDone := make(chan struct{})
		go func() {
			registry.Close()
			close(closeDone)
		}()
		close(discoverer.release)

		select {
		case err := <-getDone:
			if err != nil {
				t.Fatalf("initial compilation after Close: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("initial compilation did not finish")
		}
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("Close did not finish")
		}
	}
}

func (d *countingDiscoverer) Discover(context.Context, config.Server) (*component.Features, error) {
	d.mu.Lock()
	d.count++
	d.mu.Unlock()
	d.once.Do(func() { close(d.started) })
	<-d.release
	if d.err != nil {
		return nil, d.err
	}
	return &component.Features{Tools: []*mcp.Tool{{Name: "tool", InputSchema: map[string]any{"type": "object"}}}}, nil
}

func (d *countingDiscoverer) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}
