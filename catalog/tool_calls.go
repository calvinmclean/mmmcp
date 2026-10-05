package catalog

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/toolsearch"
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

// ResolveToolCall permits direct calls only when search is off and otherwise
// resolves the two search tools. A false ok means the name is not callable.
func (c *Catalog) ResolveToolCall(ctx context.Context, name string, arguments json.RawMessage) (ResolvedToolCall, bool, error) {
	if !c.toolSearch {
		route, ok := c.RouteTool(name)
		if !ok {
			return ResolvedToolCall{}, false, nil
		}
		return ResolvedToolCall{
			Route:     &route,
			Arguments: arguments,
		}, true, nil
	}

	switch name {
	case toolsearch.SearchToolName:
		result, err := c.SearchTool(ctx, arguments)
		return ResolvedToolCall{Result: result}, true, err
	case toolsearch.CallToolName:
		args, err := toolsearch.ParseCallArguments(arguments)
		if err != nil {
			return failedToolCall(invalidArgumentsCode, err.Error()), true, nil
		}

		route, revision, ok := c.RouteReference(toolsearch.Reference{Name: args.Name})
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
