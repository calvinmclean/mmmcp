package catalog_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/catalog"
	"github.com/obot-platform/mmmcp/component"
	"github.com/obot-platform/mmmcp/config"
	"github.com/obot-platform/mmmcp/toolsearch"
)

type searchDiscoverer struct{}
type collisionDiscoverer struct{}

func (searchDiscoverer) Discover(_ context.Context, server config.Server) (*component.Features, error) {
	if server.ID == "two" {
		return &component.Features{
			Tools: []*mcp.Tool{{
				Name:        "lookup",
				Description: "Search archived invoices",
				InputSchema: map[string]any{"type": "object"},
			}},
		}, nil
	}

	return &component.Features{
		Tools: []*mcp.Tool{
			{
				Name:        "lookup",
				Description: "Search current invoices",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"invoiceNumber": map[string]any{
							"type":        "string",
							"description": "billing reference",
						},
					},
				},
			},
			{
				Name:        "hidden",
				InputSchema: map[string]any{"type": "object"},
			},
		},
	}, nil
}

func TestSearchUsesOnlyCompiledToolsAndStableReferences(t *testing.T) {
	cfg := &config.Config{
		ToolSearch: true,
		Servers: []config.Server{
			{
				ID:   "one",
				Name: "current",
				URL:  "https://example.invalid",
				Tools: []config.ToolOverride{{
					Name:    "lookup",
					Enabled: true,
				}},
			},
			{
				ID:   "two",
				Name: "archive",
				URL:  "https://example.invalid",
			},
		},
	}
	compiled, err := catalog.Compile(t.Context(), cfg, searchDiscoverer{})
	if err != nil {
		t.Fatal(err)
	}

	results, err := compiled.Search(t.Context(), "invoiceNumber billing reference", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results.Tools) == 0 || results.Tools[0].Reference != (toolsearch.Reference{
		ComponentID: "one",
		Name:        "lookup",
	}) {
		t.Fatalf("search results: %+v", results)
	}
	for _, tool := range results.Tools {
		if tool.Reference.Name == "hidden" {
			t.Fatalf("excluded tool appeared: %+v", results)
		}
		if tool.Tool.InputSchema == nil || tool.Revision == "" {
			t.Fatalf("incomplete result: %+v", tool)
		}
	}

	other, err := compiled.Search(t.Context(), "archived invoices", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Tools) == 0 || other.Tools[0].Reference.ComponentID != "two" {
		t.Fatalf("duplicate-name routing: %+v", other)
	}
	_, _, revision, ok := compiled.RouteReference(other.Tools[0].Reference)
	if !ok || revision != other.Tools[0].Revision {
		t.Fatalf("reference did not route to discovered tool")
	}
}

func TestSearchReservedToolCollision(t *testing.T) {
	cfg := &config.Config{
		ToolSearch: true,
		Servers: []config.Server{{
			Name: "fixture",
			URL:  "https://example.invalid",
		}},
	}
	_, err := catalog.Compile(t.Context(), cfg, collisionDiscoverer{})
	if err == nil {
		t.Fatal("reserved name should reject search catalog")
	}

	cfg.ToolSearch = false
	compiled, err := catalog.Compile(t.Context(), cfg, collisionDiscoverer{})
	if err != nil {
		t.Fatalf("off mode should allow existing component name: %v", err)
	}

	call, ok, err := compiled.ResolveToolCall(t.Context(), toolsearch.SearchToolName, nil)
	if err != nil || !ok || call.Route == nil || call.Route.Tool.Name != toolsearch.SearchToolName {
		t.Fatalf("off mode should route component tool with search name: %+v, %v, %v", call, ok, err)
	}
}

func TestSearchUsesEffectiveOverrides(t *testing.T) {
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"account": map[string]any{"type": "string"},
		},
	}
	discoverer := featureDiscoverer{features: map[string]*component.Features{
		"billing": {
			Tools: []*mcp.Tool{
				{
					Name:        "legacytoken",
					Title:       "legacytoken",
					Description: "originalphrase",
					InputSchema: schema,
					Annotations: annotations,
				},
				{
					Name:        "excludedtoken",
					Description: "excludedphrase",
					InputSchema: map[string]any{"type": "object"},
				},
			},
		},
	}}
	cfg := &config.Config{
		ToolSearch: true,
		Servers: []config.Server{{
			Name:   "billing",
			Prefix: "billing",
			URL:    "https://example.invalid",
			Tools: []config.ToolOverride{
				{
					Name:                "legacytoken",
					OverrideName:        "modernname",
					OverrideDescription: "configuredphrase",
					Enabled:             true,
				},
				{
					Name:    "excludedtoken",
					Enabled: false,
				},
			},
		}},
	}
	compiled, err := catalog.Compile(t.Context(), cfg, discoverer)
	if err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{"modernname", "configuredphrase", "account"} {
		results, err := compiled.Search(t.Context(), query, 5)
		if err != nil || len(results.Tools) != 1 {
			t.Fatalf("search %q: %+v, %v", query, results, err)
		}
		hit := results.Tools[0]
		if hit.Tool.Name != "billing__modernname" || hit.Tool.Description != "configuredphrase" || hit.Tool.Annotations != annotations || hit.Tool.InputSchema == nil || hit.Reference.Name != "legacytoken" {
			t.Fatalf("search %q returned wrong effective definition: %+v", query, hit)
		}
	}

	for _, query := range []string{"legacytoken", "originalphrase", "excludedtoken", "excludedphrase"} {
		results, err := compiled.Search(t.Context(), query, 5)
		if err != nil || len(results.Tools) != 0 {
			t.Fatalf("search %q unexpectedly found tools: %+v, %v", query, results, err)
		}
	}

	if _, _, _, ok := compiled.RouteReference(toolsearch.Reference{
		ComponentID: "billing",
		Name:        "excludedtoken",
	}); ok {
		t.Fatal("excluded tool retained an internal route")
	}
	if call, ok, err := compiled.ResolveToolCall(t.Context(), "billing__modernname", nil); err != nil || ok || call.Route != nil {
		t.Fatalf("search-only mode accepted direct call: %+v, %v, %v", call, ok, err)
	}

	args, err := json.Marshal(map[string]any{
		"tool": toolsearch.Reference{
			ComponentID: "billing",
			Name:        "legacytoken",
		},
		"revision": "bad",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call, ok, err := compiled.ResolveToolCall(t.Context(), toolsearch.CallToolName, args); err != nil || !ok || call.Result == nil || !call.Result.IsError {
		t.Fatalf("generic call did not check revision: %+v, %v, %v", call, ok, err)
	}

	cfg.Servers[0].DisableTools = true
	disabled, err := catalog.Compile(t.Context(), cfg, discoverer)
	if err != nil {
		t.Fatal(err)
	}

	results, err := disabled.Search(t.Context(), "modernname", 5)
	if err != nil || len(results.Tools) != 0 {
		t.Fatalf("disableTools left searchable tools: %+v, %v", results, err)
	}
}

func TestVisibleToolCursorChangesWithToolSearch(t *testing.T) {
	cfg := &config.Config{
		ToolSearch: false,
		Servers: []config.Server{{
			Name: "billing",
			URL:  "https://example.invalid",
		}},
	}
	off, err := catalog.Compile(t.Context(), cfg, searchDiscoverer{})
	if err != nil {
		t.Fatal(err)
	}
	_, cursor, err := off.PageVisibleTools("", 1)
	if err != nil || cursor == "" {
		t.Fatalf("expected off-mode cursor: %q, %v", cursor, err)
	}

	cfg.ToolSearch = true
	search, err := catalog.Compile(t.Context(), cfg, searchDiscoverer{})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := search.PageVisibleTools(cursor, 1); err == nil {
		t.Fatal("cursor from direct mode was accepted in search mode")
	}
}

func (collisionDiscoverer) Discover(context.Context, config.Server) (*component.Features, error) {
	return &component.Features{
		Tools: []*mcp.Tool{{
			Name:        toolsearch.SearchToolName,
			InputSchema: map[string]any{"type": "object"},
		}},
	}, nil
}
