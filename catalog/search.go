package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/toolsearch"
)

// These aliases retain the catalog API while the search implementation lives in toolsearch.
const (
	SearchToolName = toolsearch.SearchToolName
	CallToolName   = toolsearch.CallToolName
)

type SearchHit = toolsearch.Hit
type SearchResults = toolsearch.Results

func (c *Catalog) searchDocuments() []toolsearch.Document {
	documents := make([]toolsearch.Document, 0, len(c.tools))
	for _, tool := range c.tools {
		route := c.toolRoutes[tool.Name]
		ref := route.Reference()
		documents = append(documents, toolsearch.Document{
			ExposedName:  tool.Name,
			OriginalName: route.Tool.Name,
			Component:    route.Component.Name,
			Reference:    ref,
			Revision:     toolsearch.Revision(ref, route.Component.DiscoveryRevision, tool),
			Tool:         tool,
		})
	}
	return documents
}

func (c *Catalog) BuildSearchIndex(ctx context.Context) error {
	if c.searchIndex == nil {
		return nil
	}
	return c.searchIndex.Build(ctx)
}

func (c *Catalog) StartSearchIndex(ctx context.Context) {
	if c != nil && c.searchIndex != nil {
		c.searchIndex.Start(ctx)
	}
}

func (c *Catalog) StopSearchIndex() {
	if c != nil && c.searchIndex != nil {
		c.searchIndex.Stop()
	}
}

func (c *Catalog) Search(ctx context.Context, text string, limit int) (SearchResults, error) {
	if c.searchIndex == nil {
		return SearchResults{}, fmt.Errorf("tool search is disabled")
	}
	return c.searchIndex.Search(ctx, text, limit)
}

func (c *Catalog) SearchTool(ctx context.Context, arguments any) (*mcp.CallToolResult, error) {
	if c.searchIndex == nil {
		return nil, fmt.Errorf("tool search is disabled")
	}
	return c.searchIndex.Call(ctx, arguments)
}

func (c *Catalog) RouteReference(ref ToolReference) (ToolRoute, *mcp.Tool, string, bool) {
	for _, tool := range c.tools {
		route := c.toolRoutes[tool.Name]
		if route.Reference() == ref {
			return route, tool, toolsearch.Revision(ref, route.Component.DiscoveryRevision, tool), true
		}
	}
	return ToolRoute{}, nil, "", false
}

func (c *Catalog) PageToolsMode(mode toolsearch.Mode, cursor string, pageSize int) ([]*mcp.Tool, string, error) {
	tools := append([]*mcp.Tool(nil), c.tools...)
	if mode == toolsearch.ModeSearch {
		tools = nil
	}
	if mode.Enabled() {
		tools = append(tools, toolsearch.Definitions()...)
	}
	slices.SortFunc(tools, func(a, b *mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	identities := toolNames(tools)
	start, end, next, err := c.page(FamilyTools, identities, cursor, pageSize)
	if err != nil {
		return nil, "", err
	}
	return append([]*mcp.Tool{}, tools[start:end]...), next, nil
}
