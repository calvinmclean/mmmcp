package catalog

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/toolsearch"
)

type toolCallKind uint8

const (
	toolCallDirect toolCallKind = iota + 1
	toolCallSearch
	toolCallReference
)

const (
	// Generic invocation errors are tool results so clients can recover and search again.
	invalidArgumentsCode = "INVALID_ARGUMENTS"
	toolUnavailableCode  = "TOOL_UNAVAILABLE"
	staleReferenceCode   = "STALE_TOOL_REFERENCE"
)

// ResolvedToolCall contains either a downstream route or a completed local result.
// Exactly one of Route and Result is set for a callable name.
type ResolvedToolCall struct {
	Route     *ToolRoute
	Arguments json.RawMessage
	Result    *mcp.CallToolResult
}

// configureToolCalls selects the names clients can list and call for this catalog.
// Component routes remain available internally for generic calls in search mode.
func (c *Catalog) configureToolCalls(toolSearch bool) {
	if toolSearch {
		definitions := toolsearch.Definitions()
		c.toolCalls = make(map[string]toolCallKind, len(definitions))
		for _, tool := range definitions {
			c.visibleTools = append(c.visibleTools, tool)
			switch tool.Name {
			case toolsearch.SearchToolName:
				c.toolCalls[tool.Name] = toolCallSearch
			case toolsearch.CallToolName:
				c.toolCalls[tool.Name] = toolCallReference
			}
		}
	} else {
		c.toolCalls = make(map[string]toolCallKind, len(c.tools))
		c.visibleTools = append(c.visibleTools, c.tools...)
		for _, tool := range c.tools {
			c.toolCalls[tool.Name] = toolCallDirect
		}
	}
	sort.Slice(c.visibleTools, func(i, j int) bool { return c.visibleTools[i].Name < c.visibleTools[j].Name })
}

// ResolveToolCall checks the callable view, then resolves a direct or local tool call.
// A false ok means the name is not callable in this catalog's configured mode.
func (c *Catalog) ResolveToolCall(ctx context.Context, name string, arguments json.RawMessage) (ResolvedToolCall, bool, error) {
	switch c.toolCalls[name] {
	case toolCallDirect:
		route := c.toolRoutes[name]
		return ResolvedToolCall{
			Route:     &route,
			Arguments: arguments,
		}, true, nil
	case toolCallSearch:
		result, err := c.SearchTool(ctx, arguments)
		return ResolvedToolCall{Result: result}, true, err
	case toolCallReference:
		args, err := toolsearch.ParseCallArguments(arguments)
		if err != nil {
			return failedToolCall(invalidArgumentsCode, "tool, revision, and arguments are required"), true, nil
		}

		route, _, revision, ok := c.RouteReference(args.Tool)
		if !ok {
			return failedToolCall(toolUnavailableCode, "tool is unavailable"), true, nil
		}
		if revision != args.Revision {
			return failedToolCall(staleReferenceCode, "tool changed; search again"), true, nil
		}

		return ResolvedToolCall{
			Route:     &route,
			Arguments: args.Arguments,
		}, true, nil
	default:
		return ResolvedToolCall{}, false, nil
	}
}

func failedToolCall(code, message string) ResolvedToolCall {
	return ResolvedToolCall{
		Result: &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{Text: code + ": " + message},
			},
		},
	}
}
