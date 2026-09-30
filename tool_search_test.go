package mmmcp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp"
	"github.com/obot-platform/mmmcp/config"
	"github.com/obot-platform/mmmcp/testserver"
	"github.com/obot-platform/mmmcp/toolsearch"
)

func TestToolSearchAndGenericInvocation(t *testing.T) {
	fixture := testserver.New(t, testserver.Options{
		Tools: []testserver.Tool{{
			Definition: &mcp.Tool{
				Name:        "lookup",
				Description: "Find billing invoices",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"number": map[string]any{"type": "string"},
					},
				},
			},
			Handler: func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: string(req.Params.Arguments)},
					},
				}, nil
			},
		}},
	})

	for _, enabled := range []bool{false, true} {
		name := "direct"
		if enabled {
			name = "search"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{
				ToolSearch: enabled,
				Servers: []config.Server{{
					ID:   "component-1",
					Name: "billing",
					URL:  fixture.URL,
				}},
			}
			composite, err := mmmcp.New(t.Context(), cfg, mmmcp.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer composite.Close()

			frontend := httptest.NewServer(composite.HTTPHandler())
			defer frontend.Close()

			client := mcp.NewClient(&mcp.Implementation{
				Name:    "search-test",
				Version: "1",
			}, nil)
			session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
				Endpoint:             frontend.URL,
				HTTPClient:           frontend.Client(),
				DisableStandaloneSSE: true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()

			listed, err := session.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if enabled {
				want = 2
			}
			if len(listed.Tools) != want {
				t.Fatalf("listed %d tools, want %d: %+v", len(listed.Tools), want, listed.Tools)
			}

			direct, directErr := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "lookup",
				Arguments: map[string]any{"number": "123"},
			})
			if enabled {
				if directErr == nil && !direct.IsError {
					t.Fatal("guessed direct call succeeded in search mode")
				}
			} else if directErr != nil || direct.IsError {
				t.Fatalf("direct call failed: %v %+v", directErr, direct)
			}

			if !enabled {
				if _, err := session.CallTool(t.Context(), &mcp.CallToolParams{
					Name:      toolsearch.SearchToolName,
					Arguments: map[string]any{"query": "invoice"},
				}); err == nil {
					t.Fatal("search tool was callable in off mode")
				}
				return
			}

			found, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      toolsearch.SearchToolName,
				Arguments: map[string]any{"query": "invoice"},
			})
			if err != nil || found.IsError {
				t.Fatalf("search failed: %v %+v", err, found)
			}
			var results toolsearch.Results
			if err := json.Unmarshal([]byte(found.Content[0].(*mcp.TextContent).Text), &results); err != nil {
				t.Fatal(err)
			}
			if len(results.Tools) != 1 || results.Tools[0].Reference != (toolsearch.Reference{
				ComponentID: "component-1",
				Name:        "lookup",
			}) || results.Tools[0].Tool.InputSchema == nil {
				t.Fatalf("search results: %+v", results)
			}

			invoke := map[string]any{
				"tool":      results.Tools[0].Reference,
				"revision":  results.Tools[0].Revision,
				"arguments": map[string]any{"number": "456"},
			}
			called, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      toolsearch.CallToolName,
				Arguments: invoke,
			})
			if err != nil || called.IsError || called.Content[0].(*mcp.TextContent).Text != `{"number":"456"}` {
				t.Fatalf("generic call failed: %v %+v", err, called)
			}

			invoke["revision"] = "old"
			stale, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      toolsearch.CallToolName,
				Arguments: invoke,
			})
			if err != nil || !stale.IsError {
				t.Fatalf("stale revision should fail: %v %+v", err, stale)
			}

			cfg.Servers[0].DiscoveryRevision = "upgraded"
			invoke["revision"] = results.Tools[0].Revision
			upgraded, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      toolsearch.CallToolName,
				Arguments: invoke,
			})
			if err != nil || !upgraded.IsError || upgraded.Content[0].(*mcp.TextContent).Text != "STALE_TOOL_REFERENCE: tool changed; search again" {
				t.Fatalf("snapshot upgrade should require rediscovery: %v %+v", err, upgraded)
			}

			cfg.Servers[0].DisableTools = true
			invoke["revision"] = results.Tools[0].Revision
			revoked, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      toolsearch.CallToolName,
				Arguments: invoke,
			})
			if err != nil || !revoked.IsError || revoked.Content[0].(*mcp.TextContent).Text != "TOOL_UNAVAILABLE: tool is unavailable" {
				t.Fatalf("revoked tool should be unavailable: %v %+v", err, revoked)
			}

			afterRevoke, err := session.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      toolsearch.SearchToolName,
				Arguments: map[string]any{"query": "invoice"},
			})
			if err != nil || afterRevoke.IsError {
				t.Fatalf("search after revocation failed: %v %+v", err, afterRevoke)
			}
			if err := json.Unmarshal([]byte(afterRevoke.Content[0].(*mcp.TextContent).Text), &results); err != nil || len(results.Tools) != 0 {
				t.Fatalf("revoked tool remained searchable: %v %+v", err, results)
			}
		})
	}
}
