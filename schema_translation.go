package mmmcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// projectTools keeps the immutable catalog in its natural form. Only definitions
// requiring a legacy envelope are copied at the frontend boundary.
func projectTools(request mcp.Request, tools []*mcp.Tool) ([]*mcp.Tool, error) {
	if frontendUsesCurrentProtocol(request) {
		return tools, nil
	}
	var projected []*mcp.Tool
	for i, tool := range tools {
		if tool == nil {
			continue
		}
		schema, err := outputSchemaObject(tool.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("decode output schema for %q: %w", tool.Name, err)
		}
		if !needsLegacySchemaWrap(schema) {
			continue
		}
		if projected == nil {
			projected = slices.Clone(tools)
		}
		clone := *tool
		clone.OutputSchema = wrapOutputSchemaForLegacy(schema)
		projected[i] = &clone
	}
	if projected == nil {
		return tools, nil
	}
	return projected, nil
}

func outputSchemaObject(schema any) (map[string]any, error) {
	if schema == nil {
		return nil, nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	return object, nil
}

func needsLegacySchemaWrap(schema map[string]any) bool {
	return schema != nil && schema["type"] != "object"
}

// This follows the TypeScript SDK's rev2025-11-25/legacyWrap.ts algorithm.
// Same-document pointers move with the schema; URI $id boundaries and literal
// instance data do not. Named anchors remain location independent.
func wrapOutputSchemaForLegacy(natural map[string]any) map[string]any {
	convertRecursiveRefs := declares2019Dialect(natural["$schema"]) && natural["$recursiveAnchor"] != true
	wrapped := map[string]any{
		"type":       "object",
		"properties": map[string]any{"result": rewriteSchemaRefs(natural, false, convertRecursiveRefs)},
		"required":   []string{"result"},
	}
	if dialect, ok := natural["$schema"].(string); ok {
		wrapped["$schema"] = dialect
	}
	return wrapped
}

func declares2019Dialect(value any) bool {
	dialect, ok := value.(string)
	if !ok {
		return false
	}
	dialect = strings.TrimSuffix(dialect, "#")
	return dialect == "https://json-schema.org/draft/2019-09/schema" || dialect == "http://json-schema.org/draft/2019-09/schema"
}

func establishesSchemaBase(node map[string]any) bool {
	id, present := node["$id"]
	fragment, ok := id.(string)
	return present && (!ok || !strings.HasPrefix(fragment, "#"))
}

func rewriteSchemaRefs(node any, nameMap, convertRecursiveRefs bool) any {
	switch value := node.(type) {
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = rewriteSchemaRefs(item, false, convertRecursiveRefs)
		}
		return out
	case map[string]any:
		if !nameMap && establishesSchemaBase(value) {
			return value
		}
		out := make(map[string]any, len(value))
		convertedRecursion := false
		for key, item := range value {
			if nameMap {
				out[key] = rewriteSchemaRefs(item, false, convertRecursiveRefs)
				continue
			}
			switch key {
			case "$ref", "$dynamicRef":
				ref, ok := item.(string)
				if ok && ref == "#" {
					out[key] = "#/properties/result"
				} else if ok && strings.HasPrefix(ref, "#/") {
					out[key] = "#/properties/result" + ref[1:]
				} else {
					out[key] = item
				}
			case "$recursiveRef":
				if item == "#" && convertRecursiveRefs {
					convertedRecursion = true
				} else {
					out[key] = item
				}
			case "const", "enum", "default", "examples":
				out[key] = item
			case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
				out[key] = rewriteSchemaRefs(item, true, convertRecursiveRefs)
			default:
				out[key] = rewriteSchemaRefs(item, false, convertRecursiveRefs)
			}
		}
		// Match the SDK's handling of statically resolvable 2019-09 recursion.
		// Root-anchored dynamic recursion is copied verbatim, with the same
		// documented relocation limitation as the SDK.
		if convertedRecursion {
			if _, exists := out["$ref"]; exists {
				allOf, _ := out["allOf"].([]any)
				out["allOf"] = append(allOf, map[string]any{"$ref": "#/properties/result"})
			} else {
				out["$ref"] = "#/properties/result"
			}
		}
		return out
	default:
		return node
	}
}

func projectCallToolResult(request mcp.Request, result *mcp.CallToolResult, outputSchema any) (*mcp.CallToolResult, error) {
	if result == nil || result.StructuredContent == nil {
		return result, nil
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return nil, fmt.Errorf("marshal structured tool result: %w", err)
	}
	nonObject := bytes.TrimSpace(data)[0] != '{'
	clone := *result
	if nonObject && !slices.ContainsFunc(result.Content, func(content mcp.Content) bool {
		_, text := content.(*mcp.TextContent)
		return text
	}) {
		clone.Content = append(slices.Clone(result.Content), &mcp.TextContent{Text: string(data)})
	}
	if !frontendUsesCurrentProtocol(request) {
		schema, err := outputSchemaObject(outputSchema)
		if err != nil {
			return nil, fmt.Errorf("decode advertised output schema: %w", err)
		}
		if nonObject || needsLegacySchemaWrap(schema) {
			clone.StructuredContent = map[string]any{"result": result.StructuredContent}
		}
	}
	return &clone, nil
}
