package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	SearchToolName = "obot_search_tools"
	CallToolName   = "obot_call_tool"
)

type SearchHit struct {
	Reference ToolReference `json:"tool"`
	Revision  string        `json:"revision"`
	Component string        `json:"component"`
	Tool      *mcp.Tool     `json:"definition"`
}

type SearchResults struct {
	Tools   []SearchHit `json:"tools"`
	HasMore bool        `json:"hasMore"`
}

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
var separators = strings.NewReplacer("_", " ", "-", " ", "/", " ", ".", " ")

func searchText(value string) string {
	return strings.ToLower(value) + " " + strings.ToLower(separators.Replace(camelBoundary.ReplaceAllString(value, "$1 $2")))
}

func schemaTerms(value any) string {
	var schema any
	data, err := json.Marshal(value)
	if err != nil || json.Unmarshal(data, &schema) != nil {
		return ""
	}
	var parts []string
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 12 || len(parts) > 256 {
			return
		}
		switch value := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(value))
			for key := range value {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				child := value[key]
				switch key {
				case "properties", "$defs", "definitions":
					if fields, ok := child.(map[string]any); ok {
						fieldNames := make([]string, 0, len(fields))
						for name := range fields {
							fieldNames = append(fieldNames, name)
						}
						slices.Sort(fieldNames)
						for _, name := range fieldNames {
							parts = append(parts, searchText(name))
							walk(fields[name], depth+1)
						}
					}
				case "title", "description":
					if text, ok := child.(string); ok {
						parts = append(parts, text)
					}
				case "items", "anyOf", "oneOf", "allOf":
					walk(child, depth+1)
				}
			}
		case []any:
			for _, child := range value {
				walk(child, depth+1)
			}
		}
	}
	walk(schema, 0)
	return strings.Join(parts, " ")
}

func (c *Catalog) BuildSearchIndex() error {
	mapping := bleve.NewIndexMapping()
	mapping.ScoringModel = "bm25"
	mapping.DefaultAnalyzer = "en"
	mapping.DefaultMapping.Dynamic = false
	for _, field := range []string{"name", "component", "description", "parameters"} {
		mapping.DefaultMapping.AddFieldMappingsAt(field, bleve.NewTextFieldMapping())
	}
	index, err := bleve.NewUsing("", mapping, bleve.Config.DefaultIndexType, bleve.Config.DefaultMemKVStore, nil)
	if err != nil {
		return fmt.Errorf("create tool search index: %w", err)
	}
	batch := index.NewBatch()
	for _, tool := range c.tools {
		route := c.toolRoutes[tool.Name]
		if err := batch.Index(tool.Name, map[string]string{
			"name":        searchText(tool.Name + " " + route.Tool.Name + " " + tool.Title),
			"component":   searchText(route.Component.Name),
			"description": tool.Description,
			"parameters":  schemaTerms(tool.InputSchema),
		}); err != nil {
			_ = index.Close()
			return fmt.Errorf("index tool %q: %w", tool.Name, err)
		}
	}
	if err := index.Batch(batch); err != nil {
		_ = index.Close()
		return fmt.Errorf("build tool search index: %w", err)
	}
	c.searchIndex = index
	runtime.SetFinalizer(c, func(catalog *Catalog) { _ = catalog.searchIndex.Close() })
	return nil
}

func (c *Catalog) Search(ctx context.Context, text string, limit int) (SearchResults, error) {
	if c.searchIndex == nil {
		return SearchResults{}, fmt.Errorf("tool search is disabled")
	}
	if strings.TrimSpace(text) == "" || limit < 1 || limit > 20 {
		return SearchResults{}, fmt.Errorf("query must be nonempty and limit must be between 1 and 20")
	}
	var clauses []query.Query
	for _, field := range []struct {
		name  string
		boost float64
	}{{"name", 4}, {"component", 2}, {"description", 2}, {"parameters", 1}} {
		q := bleve.NewMatchQuery(searchText(text))
		q.SetField(field.name)
		q.SetBoost(field.boost)
		clauses = append(clauses, q)
	}
	request := bleve.NewSearchRequestOptions(bleve.NewDisjunctionQuery(clauses...), limit+1, 0, false)
	result, err := c.searchIndex.SearchInContext(ctx, request)
	if err != nil {
		return SearchResults{}, err
	}
	// An exact name can be outside the BM25 window. Probe it explicitly.
	if _, ok := c.toolRoutes[text]; ok {
		found := false
		for _, hit := range result.Hits {
			found = found || hit.ID == text
		}
		if !found {
			result.Hits = append(result.Hits, &search.DocumentMatch{ID: text})
		}
	}
	slices.SortStableFunc(result.Hits, func(a, b *search.DocumentMatch) int {
		if a.ID == text {
			return -1
		}
		if b.ID == text {
			return 1
		}
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	response := SearchResults{HasMore: len(result.Hits) > limit, Tools: make([]SearchHit, 0, min(len(result.Hits), limit))}
	for _, found := range result.Hits[:min(len(result.Hits), limit)] {
		tool := c.toolByName(found.ID)
		route := c.toolRoutes[found.ID]
		response.Tools = append(response.Tools, SearchHit{Reference: route.Reference(), Revision: toolRevision(route, tool), Component: route.Component.Name, Tool: tool})
	}
	return response, nil
}

func (c *Catalog) toolByName(name string) *mcp.Tool {
	for _, tool := range c.tools {
		if tool.Name == name {
			return tool
		}
	}
	return nil
}

func toolRevision(route ToolRoute, tool *mcp.Tool) string {
	data, _ := json.Marshal([]any{route.Reference(), route.Component.DiscoveryRevision, tool})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func (c *Catalog) RouteReference(ref ToolReference) (ToolRoute, *mcp.Tool, string, bool) {
	for _, tool := range c.tools {
		route := c.toolRoutes[tool.Name]
		if route.Reference() == ref {
			return route, tool, toolRevision(route, tool), true
		}
	}
	return ToolRoute{}, nil, "", false
}

func (c *Catalog) PageToolsMode(mode string, cursor string, pageSize int) ([]*mcp.Tool, string, error) {
	tools := append([]*mcp.Tool(nil), c.tools...)
	if mode == "search" {
		tools = nil
	}
	if mode == "search" || mode == "hybrid" {
		tools = append(tools, &mcp.Tool{
			Name:        SearchToolName,
			Description: "Find available tools by name, description, and input parameters. Results include schemas and references for obot_call_tool.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Natural-language tool search query"},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "default": 5},
			}, "required": []string{"query"}},
		}, &mcp.Tool{
			Name:        CallToolName,
			Description: "Invoke a tool returned by obot_search_tools using its reference and revision.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"tool": map[string]any{"type": "object", "properties": map[string]any{
					"componentID": map[string]any{"type": "string"},
					"name":        map[string]any{"type": "string"},
				}, "required": []string{"componentID", "name"}},
				"revision":  map[string]any{"type": "string"},
				"arguments": map[string]any{"type": "object"},
			}, "required": []string{"tool", "revision"}},
		})
	}
	slices.SortFunc(tools, func(a, b *mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	identities := toolNames(tools)
	start, end, next, err := c.page(FamilyTools, identities, cursor, pageSize)
	if err != nil {
		return nil, "", err
	}
	return append([]*mcp.Tool{}, tools[start:end]...), next, nil
}

func (c *Catalog) SearchTool(ctx context.Context, arguments any) (*mcp.CallToolResult, error) {
	var params struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	data, err := json.Marshal(arguments)
	if err != nil || json.Unmarshal(data, &params) != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "INVALID_ARGUMENTS: expected query and optional limit"}}}, nil
	}
	if params.Limit == 0 {
		params.Limit = 5
	}
	if strings.TrimSpace(params.Query) == "" || params.Limit < 1 || params.Limit > 20 {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "INVALID_ARGUMENTS: query must be nonempty and limit must be between 1 and 20"}}}, nil
	}
	results, err := c.Search(ctx, params.Query, params.Limit)
	if err != nil {
		return nil, err
	}
	output, err := json.Marshal(results)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{StructuredContent: results, Content: []mcp.Content{&mcp.TextContent{Text: string(output)}}}, nil
}
