package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/obot-platform/mmmcp/component"
	"github.com/obot-platform/mmmcp/config"
)

const (
	refreshDebounce = 10 * time.Millisecond
)

var (
	// ErrCatalogUnavailable means a search-enabled catalog could not refresh.
	ErrCatalogUnavailable = fmt.Errorf("CATALOG_UNAVAILABLE: tool catalog refresh failed; retry")
)

// Registry caches immutable catalogs by complete configuration fingerprint.
type Registry struct {
	discoverer component.Discoverer
	ctx        context.Context
	cancel     context.CancelFunc

	mu      sync.Mutex
	entries map[string]*registryEntry
	closed  bool
}

type registryEntry struct {
	ready       chan struct{}
	catalog     *Catalog
	err         error
	config      *config.Config
	refreshing  bool
	pending     bool
	timer       *time.Timer
	callbacks   []func(bool)
	stale       bool
	refreshDone chan struct{}
	refreshed   time.Time
}

// NewRegistry creates a catalog registry.
func NewRegistry(discoverer component.Discoverer) *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{
		discoverer: discoverer,
		ctx:        ctx,
		cancel:     cancel,
		entries:    make(map[string]*registryEntry),
	}
}

// Fingerprint returns a stable, non-reversible digest of a complete configuration.
func Fingerprint(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("catalog: nil config")
	}
	canonical := canonicalConfig(*cfg)
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("catalog fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Get returns the cached catalog for cfg, compiling it once on first use.
func (r *Registry) Get(ctx context.Context, cfg *config.Config) (*Catalog, string, error) {
	fingerprint, err := Fingerprint(cfg)
	if err != nil {
		return nil, "", err
	}

	r.mu.Lock()
	if entry := r.entries[fingerprint]; entry != nil {
		r.mu.Unlock()
		select {
		case <-entry.ready:
			r.mu.Lock()
			catalog, entryErr := entry.catalog, entry.err
			if entryErr != nil {
				r.mu.Unlock()
				return catalog, fingerprint, entryErr
			}
			if cfg.ToolSearch && time.Since(entry.refreshed) >= time.Minute {
				entry.stale = true
			}
			if entry.stale && cfg.ToolSearch {
				if r.closed {
					r.mu.Unlock()
					return nil, fingerprint, context.Canceled
				}
				if entry.refreshDone == nil {
					entry.refreshDone = make(chan struct{})
				}
				done := entry.refreshDone
				if !entry.refreshing && entry.timer == nil && !r.closed {
					entry.timer = time.AfterFunc(refreshDebounce, func() { r.runRefresh(fingerprint) })
				}
				r.mu.Unlock()
				select {
				case <-done:
				case <-ctx.Done():
					return nil, fingerprint, ctx.Err()
				}
				r.mu.Lock()
				current := r.entries[fingerprint]
				if current == nil || current.stale {
					r.mu.Unlock()
					return nil, fingerprint, ErrCatalogUnavailable
				}
				catalog, entryErr = current.catalog, current.err
			}
			r.mu.Unlock()
			return catalog, fingerprint, entryErr
		case <-ctx.Done():
			return nil, fingerprint, ctx.Err()
		}
	}
	entry := &registryEntry{ready: make(chan struct{}), config: cfg}
	r.entries[fingerprint] = entry
	r.mu.Unlock()

	compiled, compileErr := compile(ctx, cfg, r.discoverer)
	r.mu.Lock()
	entry.catalog, entry.err = compiled, compileErr
	if compileErr != nil {
		delete(r.entries, fingerprint)
	} else {
		entry.refreshed = time.Now()
		if !r.closed {
			compiled.StartSearchIndex(r.ctx)
		}
	}
	close(entry.ready)
	r.mu.Unlock()
	return compiled, fingerprint, compileErr
}

// Refresh recompiles cfg and replaces its cached catalog only after success.
func (r *Registry) Refresh(ctx context.Context, cfg *config.Config) (*Catalog, string, error) {
	fingerprint, err := Fingerprint(cfg)
	if err != nil {
		return nil, "", err
	}
	var current *registryEntry
	// Search catalogs must be marked stale before discovery so Get waits for this
	// refresh. Wait for any initial compile or earlier refresh to finish first.
	if cfg.ToolSearch {
		for {
			r.mu.Lock()
			if r.closed {
				r.mu.Unlock()
				return nil, fingerprint, context.Canceled
			}
			current = r.entries[fingerprint]
			if current == nil {
				r.mu.Unlock()
				break
			}
			var done <-chan struct{}
			select {
			case <-current.ready:
				if current.refreshDone != nil {
					done = current.refreshDone
				} else {
					current.stale = true
					current.refreshing = true
					current.refreshDone = make(chan struct{})
				}
			default:
				done = current.ready
			}
			r.mu.Unlock()
			if done == nil {
				break
			}
			select {
			case <-done:
			case <-ctx.Done():
				return nil, fingerprint, ctx.Err()
			}
		}
	}
	compiled, err := compile(ctx, cfg, r.discoverer)
	// Complete the search refresh on the same entry that Get is waiting on.
	// Keep it stale after failure or while a notification refresh is pending.
	if cfg.ToolSearch && current != nil {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, fingerprint, context.Canceled
		}
		r.finishRefresh(fingerprint, current, compiled, err)
		r.mu.Unlock()
		return compiled, fingerprint, err
	}
	if err != nil {
		if cfg.ToolSearch {
			r.mu.Lock()
			if entry := r.entries[fingerprint]; entry != nil {
				entry.stale = true
			}
			r.mu.Unlock()
		}
		return nil, fingerprint, err
	}
	entry := &registryEntry{
		ready:     make(chan struct{}),
		catalog:   compiled,
		config:    cfg,
		refreshed: time.Now(),
	}
	close(entry.ready)
	compiled.StartSearchIndex(r.ctx)
	r.mu.Lock()
	old := r.entries[fingerprint]
	r.entries[fingerprint] = entry
	if old != nil && old.refreshDone != nil {
		close(old.refreshDone)
		old.refreshDone = nil
	}
	r.mu.Unlock()
	if old != nil {
		old.catalog.StopSearchIndex()
	}
	return compiled, fingerprint, nil
}

// RequestRefresh debounces a full rediscovery for fingerprint. A failed
// refresh leaves the previous immutable snapshot installed.
func (r *Registry) RequestRefresh(fingerprint string, callback func(bool)) {
	r.mu.Lock()
	entry := r.entries[fingerprint]
	if r.closed || entry == nil || entry.config == nil {
		r.mu.Unlock()
		return
	}
	if callback != nil {
		entry.callbacks = append(entry.callbacks, callback)
	}
	if entry.config.ToolSearch {
		entry.stale = true
		if entry.refreshDone == nil {
			entry.refreshDone = make(chan struct{})
		}
	}
	if entry.refreshing {
		entry.pending = true
		r.mu.Unlock()
		return
	}
	if entry.timer == nil {
		entry.timer = time.AfterFunc(refreshDebounce, func() { r.runRefresh(fingerprint) })
	} else {
		entry.timer.Reset(refreshDebounce)
	}
	r.mu.Unlock()
}

func (r *Registry) runRefresh(fingerprint string) {
	r.mu.Lock()
	entry := r.entries[fingerprint]
	if r.closed || entry == nil || entry.refreshing {
		r.mu.Unlock()
		return
	}
	entry.timer = nil
	entry.refreshing = true
	entry.pending = false
	cfg := entry.config
	callbacks := entry.callbacks
	entry.callbacks = nil
	r.mu.Unlock()

	compiled, err := compile(r.ctx, cfg, r.discoverer)

	r.mu.Lock()
	if r.entries[fingerprint] != entry {
		r.mu.Unlock()
		for _, callback := range callbacks {
			callback(err == nil)
		}
		return
	}
	r.finishRefresh(fingerprint, entry, compiled, err)
	r.mu.Unlock()
	for _, callback := range callbacks {
		callback(err == nil)
	}
}

// finishRefresh installs a successful snapshot and releases Get waiters once
// no further notification refresh is pending. The caller holds r.mu.
func (r *Registry) finishRefresh(fingerprint string, entry *registryEntry, compiled *Catalog, err error) {
	if err == nil {
		compiled.StartSearchIndex(r.ctx)
		old := entry.catalog
		entry.catalog = compiled
		entry.err = nil
		entry.refreshed = time.Now()
		old.StopSearchIndex()
	}
	entry.refreshing = false
	pending := entry.pending
	entry.pending = false
	if entry.config.ToolSearch {
		entry.stale = pending || err != nil
		if !pending && entry.refreshDone != nil {
			close(entry.refreshDone)
			entry.refreshDone = nil
		}
	}
	if pending && !r.closed {
		entry.timer = time.AfterFunc(refreshDebounce, func() { r.runRefresh(fingerprint) })
	}
}

// Close cancels pending catalog refreshes.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.cancel()
	for _, entry := range r.entries {
		entry.catalog.StopSearchIndex()
		if entry.refreshDone != nil {
			close(entry.refreshDone)
			entry.refreshDone = nil
		}
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
	}
	r.mu.Unlock()
}

func canonicalConfig(cfg config.Config) config.Config {
	if cfg.Servers == nil {
		cfg.Servers = []config.Server{}
	} else {
		cfg.Servers = append([]config.Server(nil), cfg.Servers...)
	}
	for i := range cfg.Servers {
		server := &cfg.Servers[i]
		if server.Headers == nil {
			server.Headers = map[string]string{}
		}
		if server.PassthroughHeaders == nil {
			server.PassthroughHeaders = []string{}
		}
		if server.Args == nil {
			server.Args = []string{}
		}
		if server.Env == nil {
			server.Env = map[string]string{}
		}
		if server.Tools == nil {
			server.Tools = []config.ToolOverride{}
		}
		if server.Prompts == nil {
			server.Prompts = []config.PromptOverride{}
		}
		if server.Resources == nil {
			server.Resources = []config.ResourceOverride{}
		}
		if server.ResourceTemplates == nil {
			server.ResourceTemplates = []config.ResourceTemplateOverride{}
		}
	}
	return cfg
}
