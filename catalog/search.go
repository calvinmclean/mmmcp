package catalog

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/toolsearch"
)

func (c *Catalog) searchDocuments() []toolsearch.Document {
	documents := make([]toolsearch.Document, 0, len(c.tools))
	for _, tool := range c.tools {
		route := c.toolRoutes[tool.Name]
		ref := route.Reference()
		documents = append(documents, toolsearch.Document{
			ExposedName: tool.Name,
			Component:   route.Component.Name,
			Reference:   ref,
			Revision:    toolsearch.Revision(ref, route.Component.Name, route.Component.DiscoveryRevision, tool),
			Tool:        tool,
		})
	}
	return documents
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

func (c *Catalog) Search(ctx context.Context, text string, limit int) (toolsearch.Results, error) {
	return c.SearchPage(ctx, text, limit, 0)
}

// SearchPage returns ranked tool matches after offset.
func (c *Catalog) SearchPage(ctx context.Context, text string, limit, offset int) (toolsearch.Results, error) {
	if c.searchIndex == nil {
		return toolsearch.Results{}, fmt.Errorf("tool search is disabled")
	}
	return c.searchIndex.SearchPage(ctx, text, limit, offset)
}

func (c *Catalog) SearchTool(ctx context.Context, arguments any) (*mcp.CallToolResult, error) {
	if c.searchIndex == nil {
		return nil, fmt.Errorf("tool search is disabled")
	}
	return c.searchIndex.Call(ctx, arguments)
}

func (c *Catalog) RouteReference(ref toolsearch.Reference) (ToolRoute, *mcp.Tool, string, bool) {
	for _, tool := range c.tools {
		route := c.toolRoutes[tool.Name]
		if route.Reference() == ref {
			return route, tool, toolsearch.Revision(ref, route.Component.Name, route.Component.DiscoveryRevision, tool), true
		}
	}
	return ToolRoute{}, nil, "", false
}
