package catalog

import (
	"context"
	"fmt"

	"github.com/obot-platform/mmmcp/component"
	"github.com/obot-platform/mmmcp/config"
)

// Compile discovers every configured component and builds a complete snapshot.
func Compile(ctx context.Context, cfg *config.Config, discoverer component.Discoverer) (*Catalog, error) {
	if cfg == nil {
		return nil, fmt.Errorf("catalog: nil config")
	}
	if discoverer == nil {
		return nil, fmt.Errorf("catalog: nil discoverer")
	}
	compiled, err := compile(ctx, cfg, discoverer)
	if err != nil {
		return nil, err
	}
	if err := compiled.BuildSearchIndex(ctx); err != nil {
		return nil, err
	}
	return compiled, nil
}
