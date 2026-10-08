package catalog_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/catalog"
	"github.com/obot-platform/mmmcp/component"
	"github.com/obot-platform/mmmcp/config"
)

type mutableDiscoverer struct {
	mu    sync.Mutex
	name  string
	err   error
	count int
}

type blockedRefreshDiscoverer struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	err     error
}

func TestSearchCatalogWaitsForRefresh(t *testing.T) {
	discoverer := &blockedRefreshDiscoverer{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry := catalog.NewRegistry(discoverer)
	defer registry.Close()
	cfg := &config.Config{
		ToolSearch: true,
		Servers:    []config.Server{{Name: "fixture", URL: "https://example.invalid"}},
	}
	_, fingerprint, err := registry.Get(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	registry.RequestRefresh(fingerprint, nil)
	type result struct {
		catalog *catalog.Catalog
		err     error
	}
	done := make(chan result, 1)
	go func() {
		compiled, _, err := registry.Get(t.Context(), cfg)
		done <- result{catalog: compiled, err: err}
	}()

	select {
	case <-discoverer.started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	select {
	case got := <-done:
		t.Fatalf("Get returned before refresh completed: %v", got.err)
	default:
	}

	close(discoverer.release)
	select {
	case got := <-done:
		if got.err != nil || got.catalog.Tools()[0].Name != "second" {
			t.Fatalf("Get returned %+v, %v", got.catalog, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Get did not resume after refresh")
	}
}

func TestSearchCatalogWaitsForExplicitRefresh(t *testing.T) {
	discoverer := &blockedRefreshDiscoverer{
		started: make(chan struct{}),
		release: make(chan struct{}),
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
	cfg := &config.Config{
		ToolSearch: true,
		Servers:    []config.Server{{Name: "fixture", URL: "https://example.invalid"}},
	}
	_, fingerprint, err := registry.Get(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	refreshDone := make(chan error, 1)
	go func() {
		_, _, err := registry.Refresh(t.Context(), cfg)
		refreshDone <- err
	}()
	select {
	case <-discoverer.started:
	case <-time.After(time.Second):
		t.Fatal("explicit refresh did not start")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := registry.Get(ctx, cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get during explicit refresh returned %v, want deadline exceeded", err)
	}
	notified := make(chan bool, 1)
	registry.RequestRefresh(fingerprint, func(success bool) { notified <- success })
	close(discoverer.release)
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatalf("explicit refresh failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit refresh did not finish")
	}
	select {
	case success := <-notified:
		if !success {
			t.Fatal("notification refresh failed")
		}
	case <-time.After(time.Second):
		t.Fatal("notification during explicit refresh was lost")
	}
	current, _, err := registry.Get(t.Context(), cfg)
	if err != nil || current.Tools()[0].Name != "second" {
		t.Fatalf("Get after explicit refresh returned %+v, %v", current, err)
	}
	discoverer.mu.Lock()
	calls := discoverer.calls
	discoverer.mu.Unlock()
	if calls != 3 {
		t.Fatalf("discoveries = %d, want explicit refresh and notification refresh", calls)
	}
}

func TestSearchCatalogFailsClosedAfterFailedExplicitRefresh(t *testing.T) {
	failure := errors.New("discovery failed")
	discoverer := &blockedRefreshDiscoverer{
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
	cfg := &config.Config{
		ToolSearch: true,
		Servers:    []config.Server{{Name: "fixture", URL: "https://example.invalid"}},
	}
	if _, _, err := registry.Get(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}

	refreshDone := make(chan error, 1)
	go func() {
		_, _, err := registry.Refresh(t.Context(), cfg)
		refreshDone <- err
	}()
	select {
	case <-discoverer.started:
	case <-time.After(time.Second):
		t.Fatal("explicit refresh did not start")
	}
	close(discoverer.release)
	if err := <-refreshDone; !errors.Is(err, failure) {
		t.Fatalf("explicit refresh returned %v, want %v", err, failure)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := registry.Get(ctx, cfg); !errors.Is(err, catalog.ErrCatalogUnavailable) {
		t.Fatalf("failed explicit refresh exposed stale catalog: %v", err)
	}
}

func TestSearchCatalogRefreshWaitHonorsCancellation(t *testing.T) {
	discoverer := &blockedRefreshDiscoverer{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry := catalog.NewRegistry(discoverer)
	defer registry.Close()
	cfg := &config.Config{
		ToolSearch: true,
		Servers:    []config.Server{{Name: "fixture", URL: "https://example.invalid"}},
	}
	_, fingerprint, err := registry.Get(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	registry.RequestRefresh(fingerprint, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := registry.Get(ctx, cfg)
		done <- err
	}()

	select {
	case <-discoverer.started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Get returned %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Get did not stop waiting after cancellation")
	}
	close(discoverer.release)
}

func TestRegistryRefreshDebouncesAndKeepsLastKnownGood(t *testing.T) {
	discoverer := &mutableDiscoverer{name: "first"}
	registry := catalog.NewRegistry(discoverer)
	defer registry.Close()
	cfg := &config.Config{Servers: []config.Server{{Name: "fixture", URL: "https://example.invalid"}}}
	first, fingerprint, err := registry.Get(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	discoverer.set("second", nil)
	done := make(chan bool, 3)
	for range 3 {
		registry.RequestRefresh(fingerprint, func(success bool) { done <- success })
	}
	for range 3 {
		if !<-done {
			t.Fatal("successful refresh reported failure")
		}
	}
	second, _, err := registry.Get(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second.Tools()[0].Name != "second" {
		t.Fatalf("refreshed tools = %+v", second.Tools())
	}
	if got := discoverer.countValue(); got != 2 {
		t.Fatalf("discoveries = %d, want 2 after debounced refresh", got)
	}

	discoverer.set("broken", errors.New("discovery failed"))
	failed := make(chan bool, 1)
	registry.RequestRefresh(fingerprint, func(success bool) { failed <- success })
	select {
	case success := <-failed:
		if success {
			t.Fatal("failed refresh reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("refresh callback timed out")
	}
	retained, _, err := registry.Get(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if retained != second || retained.Tools()[0].Name != "second" {
		t.Fatalf("failed refresh replaced last-known-good catalog: %+v", retained.Tools())
	}
}

func TestSearchCatalogFailsClosedDuringAndAfterFailedRefresh(t *testing.T) {
	discoverer := &mutableDiscoverer{name: "first"}
	registry := catalog.NewRegistry(discoverer)
	defer registry.Close()

	cfg := &config.Config{
		ToolSearch: true,
		Servers: []config.Server{{
			Name: "fixture",
			URL:  "https://example.invalid",
		}},
	}
	_, fingerprint, err := registry.Get(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	discoverer.set("broken", errors.New("discovery failed"))
	done := make(chan bool, 1)
	registry.RequestRefresh(fingerprint, func(success bool) {
		done <- success
	})
	if _, _, err := registry.Get(t.Context(), cfg); !errors.Is(err, catalog.ErrCatalogUnavailable) {
		t.Fatalf("pending refresh returned %v", err)
	}
	if <-done {
		t.Fatal("failed refresh reported success")
	}
	if _, _, err := registry.Get(t.Context(), cfg); !errors.Is(err, catalog.ErrCatalogUnavailable) {
		t.Fatalf("failed refresh exposed stale catalog: %v", err)
	}

	discoverer.set("second", nil)
	done = make(chan bool, 1)
	registry.RequestRefresh(fingerprint, func(success bool) {
		done <- success
	})
	if !<-done {
		t.Fatal("recovery refresh failed")
	}

	current, _, err := registry.Get(t.Context(), cfg)
	if err != nil || current.Tools()[0].Name != "second" {
		t.Fatalf("recovered catalog = %+v, err = %v", current, err)
	}
}

func (d *mutableDiscoverer) Discover(context.Context, config.Server) (*component.Features, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count++
	if d.err != nil {
		return nil, d.err
	}
	return &component.Features{Tools: []*mcp.Tool{{Name: d.name, InputSchema: map[string]any{"type": "object"}}}}, nil
}

func (d *mutableDiscoverer) set(name string, err error) {
	d.mu.Lock()
	d.name, d.err = name, err
	d.mu.Unlock()
}

func (d *mutableDiscoverer) countValue() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

func (d *blockedRefreshDiscoverer) Discover(ctx context.Context, _ config.Server) (*component.Features, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()
	if call == 2 {
		close(d.started)
		select {
		case <-d.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if call > 1 && d.err != nil {
		return nil, d.err
	}
	name := "first"
	if call > 1 {
		name = "second"
	}
	return &component.Features{Tools: []*mcp.Tool{{Name: name, InputSchema: map[string]any{"type": "object"}}}}, nil
}
