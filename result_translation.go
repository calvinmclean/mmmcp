package mmmcp

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type operationResult interface {
	NeedsInput() bool
}

func normalizeCallToolResult(request mcp.Request, result *mcp.CallToolResult, outputSchema any) (*mcp.CallToolResult, error) {
	projected, err := projectCallToolResult(request, result, outputSchema)
	if err != nil || projected == nil {
		return projected, err
	}
	normalized, err := normalizeOperationResult(request, projected, func() *mcp.CallToolResult { return new(mcp.CallToolResult) }, false)
	// Unmarshalling into any collapses explicit null to nil. Retain null when
	// the caller represented it (for example with json.RawMessage). An upstream
	// null already collapsed by the SDK cannot be distinguished from absence.
	if err == nil && normalized.StructuredContent == nil && projected.StructuredContent != nil {
		normalized.StructuredContent = json.RawMessage("null")
	}
	return normalized, err
}

func normalizeGetPromptResult(request mcp.Request, result *mcp.GetPromptResult) (*mcp.GetPromptResult, error) {
	return normalizeOperationResult(request, result, func() *mcp.GetPromptResult { return new(mcp.GetPromptResult) }, false)
}

func normalizeReadResourceResult(request mcp.Request, result *mcp.ReadResourceResult) (*mcp.ReadResourceResult, error) {
	return normalizeOperationResult(request, result, func() *mcp.ReadResourceResult { return new(mcp.ReadResourceResult) }, true)
}

func normalizeOperationResult[T operationResult](request mcp.Request, result T, create func() T, cacheable bool) (T, error) {
	var zero T
	if any(result) == nil {
		return result, nil
	}
	current := frontendUsesCurrentProtocol(request)
	if !current && result.NeedsInput() {
		return zero, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: "downstream returned an input-required result that cannot be represented for a legacy frontend",
		}
	}

	data, err := json.Marshal(result)
	if err != nil {
		return zero, fmt.Errorf("marshal downstream operation result: %w", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		return zero, fmt.Errorf("decode downstream operation result: %w", err)
	}
	if current {
		resultType := "complete"
		if result.NeedsInput() {
			resultType = "input_required"
		}
		wire["resultType"], _ = json.Marshal(resultType)
		if cacheable {
			if scope, ok := wire["cacheScope"]; !ok || string(scope) == `""` || string(scope) == "null" {
				wire["cacheScope"] = json.RawMessage(`"public"`)
			}
			if _, ok := wire["ttlMs"]; !ok {
				wire["ttlMs"] = json.RawMessage("0")
			}
		}
	} else {
		delete(wire, "resultType")
	}
	stripServerInfo(wire)

	data, err = json.Marshal(wire)
	if err != nil {
		return zero, fmt.Errorf("marshal normalized operation result: %w", err)
	}
	normalized := create()
	if err := json.Unmarshal(data, normalized); err != nil {
		return zero, fmt.Errorf("decode normalized operation result: %w", err)
	}
	return normalized, nil
}

func frontendUsesCurrentProtocol(request mcp.Request) bool {
	if request == nil || request.GetParams() == nil {
		return false
	}
	version, _ := request.GetParams().GetMeta()[mcp.MetaKeyProtocolVersion].(string)
	return version >= currentProtocolVersion
}

func stripServerInfo(wire map[string]json.RawMessage) {
	raw, ok := wire["_meta"]
	if !ok {
		return
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(raw, &meta) != nil {
		return
	}
	delete(meta, mcp.MetaKeyServerInfo)
	if len(meta) == 0 {
		delete(wire, "_meta")
		return
	}
	wire["_meta"], _ = json.Marshal(meta)
}
