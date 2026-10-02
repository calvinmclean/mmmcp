package catalog_test

import (
	"context"
	"encoding/json"
	"strings"
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
	if server.Name == "archive" {
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
				Name: "current",
				URL:  "https://example.invalid",
				Tools: []config.ToolOverride{{
					Name:    "lookup",
					Enabled: true,
				}},
			},
			{
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
		Name: "current__lookup",
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
	if len(other.Tools) == 0 || other.Tools[0].Reference.Name != "archive__lookup" {
		t.Fatalf("duplicate-name routing: %+v", other)
	}
	_, revision, ok := compiled.RouteReference(other.Tools[0].Reference)
	if !ok || revision != other.Tools[0].Revision {
		t.Fatalf("reference did not route to discovered tool")
	}
}

func TestSearchRevisionChangesWhenComponentNameChanges(t *testing.T) {
	features := &component.Features{
		Tools: []*mcp.Tool{
			{
				Name: "lookup",
				InputSchema: map[string]any{
					"type": "object",
				},
			},
		},
	}
	discoverer := featureDiscoverer{features: map[string]*component.Features{
		"first":  features,
		"second": features,
	}}
	compile := func(name string) *catalog.Catalog {
		t.Helper()
		compiled, err := catalog.Compile(t.Context(), &config.Config{
			ToolSearch: true,
			Servers: []config.Server{
				{
					Name:   name,
					Prefix: "stable",
					URL:    "https://example.invalid",
				},
			},
		}, discoverer)
		if err != nil {
			t.Fatal(err)
		}
		return compiled
	}

	first := compile("first")
	second := compile("second")
	firstResults, err := first.Search(t.Context(), "lookup", 1)
	if err != nil || len(firstResults.Tools) != 1 {
		t.Fatalf("first search: %+v, %v", firstResults, err)
	}
	secondResults, err := second.Search(t.Context(), "lookup", 1)
	if err != nil || len(secondResults.Tools) != 1 {
		t.Fatalf("second search: %+v, %v", secondResults, err)
	}
	old := firstResults.Tools[0]
	current := secondResults.Tools[0]
	if old.Reference != current.Reference {
		t.Fatalf("exposed tool reference changed: %v, %v", old.Reference, current.Reference)
	}
	if old.Revision == current.Revision {
		t.Fatal("component change did not invalidate tool revision")
	}

	args, err := json.Marshal(toolsearch.CallArguments{
		Tool:      old.Reference,
		Revision:  old.Revision,
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	call, ok, err := second.ResolveToolCall(t.Context(), toolsearch.CallToolName, args)
	if err != nil || !ok || call.Result == nil || !call.Result.IsError {
		t.Fatalf("old reference was accepted: %+v, %v, %v", call, ok, err)
	}
	content, ok := call.Result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(content.Text, "STALE_TOOL_REFERENCE") {
		t.Fatalf("old reference returned wrong result: %+v", call.Result)
	}
}

func TestSearchRevisionChangesWhenRouteChanges(t *testing.T) {
	base := config.Server{
		Name:    "current",
		URL:     "https://first.invalid",
		Headers: map[string]string{"Authorization": "Bearer first"},
	}
	compile := func(server config.Server) *catalog.Catalog {
		t.Helper()
		compiled, err := catalog.Compile(t.Context(), &config.Config{
			ToolSearch: true,
			Servers:    []config.Server{server},
		}, searchDiscoverer{})
		if err != nil {
			t.Fatal(err)
		}
		return compiled
	}
	search := func(compiled *catalog.Catalog) toolsearch.Hit {
		t.Helper()
		results, err := compiled.Search(t.Context(), "lookup", 1)
		if err != nil || len(results.Tools) != 1 {
			t.Fatalf("search: %+v, %v", results, err)
		}
		return results.Tools[0]
	}

	old := search(compile(base))
	for _, tc := range []struct {
		name   string
		server config.Server
	}{
		{
			name: "URL",
			server: config.Server{
				Name:    base.Name,
				URL:     "https://second.invalid",
				Headers: base.Headers,
			},
		},
		{
			name: "credentials",
			server: config.Server{
				Name:    base.Name,
				URL:     base.URL,
				Headers: map[string]string{"Authorization": "Bearer second"},
			},
		},
		{
			name: "command",
			server: config.Server{
				Name:    base.Name,
				Command: "different-server",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currentCatalog := compile(tc.server)
			current := search(currentCatalog)
			if current.Reference != old.Reference {
				t.Fatalf("reference changed: %v != %v", current.Reference, old.Reference)
			}
			if current.Revision == old.Revision {
				t.Fatal("route change did not invalidate tool revision")
			}

			args, err := json.Marshal(toolsearch.CallArguments{Tool: old.Reference, Revision: old.Revision})
			if err != nil {
				t.Fatal(err)
			}
			call, ok, err := currentCatalog.ResolveToolCall(t.Context(), toolsearch.CallToolName, args)
			if err != nil || !ok || call.Result == nil || !call.Result.IsError {
				t.Fatalf("old route reference was accepted: %+v, %v, %v", call, ok, err)
			}
			content := call.Result.Content[0].(*mcp.TextContent)
			if !strings.Contains(content.Text, "STALE_TOOL_REFERENCE") {
				t.Fatalf("old route reference returned wrong result: %q", content.Text)
			}
		})
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
		if hit.Tool.Name != "billing__modernname" || hit.Tool.Description != "configuredphrase" || hit.Tool.Annotations != annotations || hit.Tool.InputSchema == nil || hit.Reference.Name != "billing__modernname" {
			t.Fatalf("search %q returned wrong effective definition: %+v", query, hit)
		}
	}

	for _, query := range []string{"legacytoken", "originalphrase", "excludedtoken", "excludedphrase"} {
		results, err := compiled.Search(t.Context(), query, 5)
		if err != nil || len(results.Tools) != 0 {
			t.Fatalf("search %q unexpectedly found tools: %+v, %v", query, results, err)
		}
	}

	if _, _, ok := compiled.RouteReference(toolsearch.Reference{
		Name: "excludedtoken",
	}); ok {
		t.Fatal("excluded tool retained an internal route")
	}
	if call, ok, err := compiled.ResolveToolCall(t.Context(), "billing__modernname", nil); err != nil || ok || call.Route != nil {
		t.Fatalf("search-only mode accepted direct call: %+v, %v, %v", call, ok, err)
	}

	args, err := json.Marshal(map[string]any{
		"tool": toolsearch.Reference{
			Name: "billing__modernname",
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
