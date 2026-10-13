package mmmcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp"
	"github.com/obot-platform/mmmcp/config"
	"github.com/obot-platform/mmmcp/testserver"
)

func TestBooleanOutputSchemas(t *testing.T) {
	for _, schema := range []bool{true, false} {
		t.Run(strconv.FormatBool(schema), func(t *testing.T) {
			var calls atomic.Int64
			server := mcp.NewServer(&mcp.Implementation{Name: "boolean", Version: "test"}, &mcp.ServerOptions{
				Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
			})
			server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					switch method {
					case "tools/list":
						return &mcp.ListToolsResult{Tools: []*mcp.Tool{
							{Name: "object", InputSchema: map[string]any{"type": "object"}, OutputSchema: schema},
							{Name: "array", InputSchema: map[string]any{"type": "object"}, OutputSchema: schema},
						}}, nil
					case "tools/call":
						calls.Add(1)
						var value any = map[string]any{"ok": true}
						if req.(*mcp.CallToolRequest).Params.Name == "array" {
							value = []string{"item"}
						}
						return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: value}, nil
					default:
						return next(ctx, method, req)
					}
				}
			})
			upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
			t.Cleanup(upstream.Close)
			other := testserver.New(t, testserver.Options{Tools: []testserver.Tool{{
				Definition: &mcp.Tool{Name: "valid", InputSchema: map[string]any{"type": "object"}},
				Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "valid"}}}, nil
				},
			}}})
			composite, err := mmmcp.New(t.Context(), &config.Config{Servers: []config.Server{
				{Name: "boolean", Prefix: "boolean", URL: upstream.URL},
				{Name: "other", Prefix: "other", URL: other.URL},
			}}, mmmcp.Options{DSN: "sqlite://:memory:"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = composite.Close() })
			frontend := httptest.NewServer(composite.HTTPHandler())
			t.Cleanup(frontend.Close)
			for _, version := range []string{"2025-11-25", "2026-07-28"} {
				t.Run(version, func(t *testing.T) {
					session := connectFrontend(t, frontend, &mcp.Implementation{Name: "boolean-client", Version: "test"}, version)
					t.Cleanup(func() { _ = session.Close() })
					t.Run("list", func(t *testing.T) {
						listed, err := session.ListTools(t.Context(), nil)
						if err != nil {
							t.Fatalf("boolean schema prevented aggregate listing: %v", err)
						}
						if len(listed.Tools) != 3 {
							t.Fatalf("retained %d tools, want 3", len(listed.Tools))
						}
						for _, tool := range listed.Tools {
							if tool.Name != "other__valid" && tool.OutputSchema != schema {
								t.Fatalf("boolean schema was changed: %#v", tool.OutputSchema)
							}
						}
						result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "other__valid"})
						if err != nil || result.IsError {
							t.Fatalf("boolean schema affected another component's tool: %v", err)
						}
					})
					for _, name := range []string{"object", "array"} {
						t.Run(name, func(t *testing.T) {
							before := calls.Load()
							result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "boolean__" + name})
							if calls.Load() != before+1 {
								t.Fatal("downstream invocation count changed")
							}
							if err != nil {
								t.Fatalf("downstream completed but frontend reported failure: %v", err)
							}
							var want any = map[string]any{"ok": true}
							if name == "array" {
								want = []any{"item"}
								if version == "2025-11-25" {
									want = map[string]any{"result": want}
								}
							}
							if result.IsError || !reflect.DeepEqual(result.StructuredContent, want) {
								t.Fatalf("structured result = %#v, want %#v", result.StructuredContent, want)
							}
						})
					}
				})
			}
		})
	}
}
