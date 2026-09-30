package catalog_test

import (
	"context"
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
		return &component.Features{Tools: []*mcp.Tool{{Name: "lookup", Description: "Search archived invoices", InputSchema: map[string]any{"type": "object"}}}}, nil
	}
	return &component.Features{Tools: []*mcp.Tool{
		{Name: "lookup", Description: "Search current invoices", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"invoiceNumber": map[string]any{"type": "string", "description": "billing reference"}}}},
		{Name: "hidden", InputSchema: map[string]any{"type": "object"}},
	}}, nil
}

func TestSearchUsesOnlyCompiledToolsAndStableReferences(t *testing.T) {
	cfg := &config.Config{ToolSearchMode: toolsearch.ModeSearch, Servers: []config.Server{
		{ID: "one", Name: "current", URL: "https://example.invalid", Tools: []config.ToolOverride{{Name: "lookup", Enabled: true}}},
		{ID: "two", Name: "archive", URL: "https://example.invalid"},
	}}
	compiled, err := catalog.Compile(t.Context(), cfg, searchDiscoverer{})
	if err != nil {
		t.Fatal(err)
	}
	results, err := compiled.Search(t.Context(), "invoiceNumber billing reference", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results.Tools) == 0 || results.Tools[0].Reference != (catalog.ToolReference{ComponentID: "one", Name: "lookup"}) {
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
	cfg := &config.Config{ToolSearchMode: toolsearch.ModeSearch, Servers: []config.Server{{Name: "fixture", URL: "https://example.invalid"}}}
	_, err := catalog.Compile(t.Context(), cfg, collisionDiscoverer{})
	if err == nil {
		t.Fatal("reserved name should reject search catalog")
	}
	cfg.ToolSearchMode = toolsearch.ModeOff
	if _, err := catalog.Compile(t.Context(), cfg, collisionDiscoverer{}); err != nil {
		t.Fatalf("off mode should allow existing component name: %v", err)
	}
}

func (collisionDiscoverer) Discover(context.Context, config.Server) (*component.Features, error) {
	return &component.Features{Tools: []*mcp.Tool{{Name: catalog.SearchToolName, InputSchema: map[string]any{"type": "object"}}}}, nil
}
