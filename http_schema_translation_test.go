package mmmcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp"
	"github.com/obot-platform/mmmcp/config"
	"github.com/obot-platform/mmmcp/toolsearch"
)

func TestLegacyOutputSchemaProjection(t *testing.T) {
	upstream := schemaTranslationUpstream(t)
	direct := connectFrontend(t, upstream, &mcp.Implementation{Name: "direct", Version: "test"}, "2025-11-25")
	t.Cleanup(func() { _ = direct.Close() })
	listed, err := direct.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "union" && tool.OutputSchema != nil {
			t.Fatalf("direct legacy tool has modern output schema: %+v", tool)
		}
	}

	for _, first := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(first, func(t *testing.T) {
			composite, err := mmmcp.New(t.Context(), &config.Config{Servers: []config.Server{{
				Name:   "fixture",
				URL:    upstream.URL,
				Prefix: "gh",
				Tools:  []config.ToolOverride{{Name: "absent", Enabled: true}, {Name: "object", Enabled: true}, {Name: "union", OverrideName: "renamed", Enabled: true}},
			}}}, mmmcp.Options{DSN: "sqlite://:memory:", PageSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = composite.Close() })
			frontend := httptest.NewServer(composite.HTTPHandler())
			t.Cleanup(frontend.Close)
			other := "2026-07-28"
			if first == other {
				other = "2025-11-25"
			}
			for _, version := range []string{first, other, first} {
				session := connectFrontend(t, frontend, &mcp.Implementation{Name: "projection", Version: "test"}, version)
				t.Cleanup(func() { _ = session.Close() })
				var tools []*mcp.Tool
				var cursor string
				for {
					page, err := session.ListTools(t.Context(), &mcp.ListToolsParams{Cursor: cursor})
					if err != nil {
						t.Fatal(err)
					}
					tools = append(tools, page.Tools...)
					cursor = page.NextCursor
					if cursor == "" {
						break
					}
				}
				if len(tools) != 3 {
					t.Fatalf("%s retained %d tools, want 3", version, len(tools))
				}
				foundUnion := false
				for _, tool := range tools {
					if version == "2025-11-25" && tool.OutputSchema != nil {
						data, _ := json.Marshal(tool.OutputSchema)
						var schema map[string]any
						if err := json.Unmarshal(data, &schema); err != nil || schema["type"] != "object" {
							t.Fatalf("legacy %s output schema is invalid: %s", tool.Name, data)
						}
					}
					if tool.Name != "gh__renamed" {
						continue
					}
					foundUnion = true
					result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool.Name})
					if err != nil {
						t.Fatal(err)
					}
					var want any = map[string]any{"ok": true}
					if version == "2025-11-25" {
						want = map[string]any{"result": want}
					}
					if !reflect.DeepEqual(result.StructuredContent, want) {
						t.Fatalf("%s structured result = %#v, want %#v", version, result.StructuredContent, want)
					}
					data, _ := json.Marshal(tool.OutputSchema)
					if version == "2026-07-28" && string(data) != `{"oneOf":[{"type":"null"},{"type":"object"}]}` {
						t.Fatalf("modern schema was mutated: %s", data)
					}
				}
				if !foundUnion {
					t.Fatal("renamed tool was not retained")
				}
			}
		})
	}
}

func schemaTranslationUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "schema-upstream", Version: "test"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/list":
				tools := []*mcp.Tool{
					{Name: "absent", InputSchema: map[string]any{"type": "object"}},
					{Name: "object", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}},
					{Name: "union", InputSchema: map[string]any{"type": "object"}},
				}
				if req.(*mcp.ListToolsRequest).ProtocolVersion() >= "2026-07-28" {
					tools[2].OutputSchema = map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"type": "object"}}}
				}
				return &mcp.ListToolsResult{Tools: tools}, nil
			case "tools/call":
				if req.(*mcp.CallToolRequest).Params.Name == "absent" {
					return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: []any{1, 2}}, nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}, StructuredContent: map[string]any{"ok": true}}, nil
			default:
				return next(ctx, method, req)
			}
		}
	})
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestGenericInvocationUsesAdvertisedSchema(t *testing.T) {
	upstream := schemaTranslationUpstream(t)
	composite, err := mmmcp.New(t.Context(), &config.Config{
		ToolSearch: true,
		Servers:    []config.Server{{Name: "fixture", URL: upstream.URL}},
	}, mmmcp.Options{DSN: "sqlite://:memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = composite.Close() })
	frontend := httptest.NewServer(composite.HTTPHandler())
	t.Cleanup(frontend.Close)
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			session := connectFrontend(t, frontend, &mcp.Implementation{Name: "generic", Version: "test"}, version)
			t.Cleanup(func() { _ = session.Close() })
			for _, name := range []string{"union", "absent"} {
				search, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: toolsearch.SearchToolName, Arguments: map[string]any{"query": name}})
				if err != nil {
					t.Fatal(err)
				}
				var found toolsearch.Results
				data, err := json.Marshal(search.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &found); err != nil {
					t.Fatal(err)
				}
				called := false
				for _, match := range found.Tools {
					if match.Reference.Name != name {
						continue
					}
					called = true
					result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: toolsearch.CallToolName, Arguments: map[string]any{
						"name": name, "revision": match.Revision, "arguments": map[string]any{},
					}})
					if err != nil {
						t.Fatal(err)
					}
					var want any = map[string]any{"ok": true}
					if name == "absent" {
						want = []any{float64(1), float64(2)}
						if version == "2025-11-25" {
							want = map[string]any{"result": want}
						}
						if len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "[1,2]" {
							t.Fatalf("missing natural-value text fallback: %+v", result.Content)
						}
					}
					if !reflect.DeepEqual(result.StructuredContent, want) {
						t.Fatalf("%s generic %s result = %#v, want %#v", version, name, result.StructuredContent, want)
					}
				}
				if !called {
					t.Fatalf("search did not retain %s", name)
				}
			}
		})
	}
}
